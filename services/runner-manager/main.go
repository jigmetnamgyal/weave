// Command runner-manager provisions, watches and destroys session runners.
//
// A Temporal worker on its own queue, with no HTTP API beyond health. The
// architecture says only the runner manager and the workflow workers may
// provision runners; keeping the backend's credentials here, and out of the
// workflow worker, means only this process can. With no inbound API there is
// nothing to authenticate and nothing to expose.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"
	"go.temporal.io/sdk/activity"
	temporalclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/jigmetnamgyal/weave/internal/adapters/eventstream"
	githubadapter "github.com/jigmetnamgyal/weave/internal/adapters/github"
	"github.com/jigmetnamgyal/weave/internal/adapters/natsauth"
	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	weaveredis "github.com/jigmetnamgyal/weave/internal/adapters/redis"
	weavetemporal "github.com/jigmetnamgyal/weave/internal/adapters/temporal"
	"github.com/jigmetnamgyal/weave/internal/application"
	runnerbackend "github.com/jigmetnamgyal/weave/services/runner-manager/internal/backend"
	"github.com/jigmetnamgyal/weave/services/runner-manager/internal/config"
)

// reconcileInterval is how often runners left behind are looked for. Short
// enough that a lost environment is noticed promptly; each pass is one bounded
// query and one backend listing.
const reconcileInterval = 30 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "weave-runner-manager: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	backend, err := runnerbackend.New(cfg)
	if err != nil {
		return err
	}

	appPool, err := pgxpool.New(ctx, cfg.AppDatabaseURL)
	if err != nil {
		return fmt.Errorf("configure application postgres pool: %w", err)
	}
	defer appPool.Close()

	redisOpts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		return fmt.Errorf("parse REDIS_URL: %w", err)
	}
	redisClient := redis.NewClient(redisOpts)
	defer func() { _ = redisClient.Close() }()

	githubClient, err := githubadapter.NewClient(cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPath,
		weaveredis.NewTokenCache(redisClient))
	if err != nil {
		return fmt.Errorf("configure github client: %w", err)
	}
	installations := application.NewInstallationService(postgres.NewInstallationStore(appPool), nil,
		githubadapter.NewPort(githubClient), postgres.WithTenantWorkspace, "")

	// The runner manager is the only process holding the account signing key.
	issuer, err := natsauth.LoadIssuer(cfg.NATSSigningKey, cfg.NATSAccount, eventstream.DefaultConfig().SubjectPrefix)
	if err != nil {
		return err
	}
	// Its own broker connection, under an identity that may read the event
	// stream's information and nothing else — what the drain needs.
	natsConn, err := nats.Connect(cfg.NATSURL, nats.Name("weave-runner-manager"),
		nats.UserCredentials(cfg.NATSCreds), nats.MaxReconnects(-1), nats.ReconnectWait(time.Second))
	if err != nil {
		return fmt.Errorf("connect to nats: %w", err)
	}
	defer natsConn.Close()
	js, err := jetstream.New(natsConn)
	if err != nil {
		return fmt.Errorf("open jetstream: %w", err)
	}
	drain := eventstream.NewDrain(js, eventstream.DefaultConfig(), eventstream.StreamMaxAge)

	runners := application.NewRunnerService(postgres.NewRunnerStore(appPool), postgres.NewSessionStore(appPool),
		backend, installations, issuer, cfg.RunnerNATSURL, postgres.NewAgentStore(appPool), drain,
		postgres.WithTenantWorkspace, cfg.GitBaseURL, logger).WithRegistryProxy(cfg.RegistryProxyURL)
	if cfg.RegistryProxyURL == "" {
		// Allowed in development only (config refuses it elsewhere), and said
		// out loud: registries are reachable and nothing is recorded.
		logger.Warn("no registry proxy configured: runners reach package registries directly and no registry request is recorded (ADR-016); set RUNNER_REGISTRY_PROXY_URL")
	}

	temporalClient, err := temporalclient.Dial(temporalclient.Options{HostPort: cfg.TemporalHostPort, Logger: logger})
	if err != nil {
		return fmt.Errorf("connect to temporal at %s: %w", cfg.TemporalHostPort, err)
	}
	defer temporalClient.Close()

	// Leases on their own ticker, never inside a reconcile pass: a pass's
	// status reads wait on running commands, and lease renewal must not grow
	// with them (review of PR #24). Once now, too — a restarted runner
	// manager's sandboxes may be close to the end of their lease. Started before
	// startup reconciliation, which reads every runner's status and can be slow.
	go func() {
		renew := func() {
			if _, err := runners.ExtendLeases(ctx); err != nil && ctx.Err() == nil {
				logger.Warn("lease renewal failed", slog.String("error", err.Error()))
			}
		}
		renew()
		ticker := time.NewTicker(application.LeaseInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renew()
			}
		}
	}()

	// Before taking work: finish what a previous instance left behind.
	if cleaned, err := runners.Reconcile(ctx); err != nil {
		logger.Warn("startup reconciliation failed; will retry", slog.String("error", err.Error()))
	} else {
		logger.Info("startup reconciliation", slog.Int("cleaned", cleaned))
	}

	queue := weavetemporal.RunnerTaskQueue(weavetemporal.TaskQueue)
	activities := weavetemporal.NewRunnerActivities(runners)
	w := worker.New(temporalClient, queue, worker.Options{})
	w.RegisterActivityWithOptions(activities.ProvisionRunner,
		activity.RegisterOptions{Name: weavetemporal.ActivityProvisionRunner})
	w.RegisterActivityWithOptions(activities.TeardownRunner,
		activity.RegisterOptions{Name: weavetemporal.ActivityTeardownRunner})
	w.RegisterActivityWithOptions(activities.AwaitRunnerExit,
		activity.RegisterOptions{Name: weavetemporal.ActivityAwaitRunnerExit})
	w.RegisterActivityWithOptions(activities.HaltRunner,
		activity.RegisterOptions{Name: weavetemporal.ActivityHaltRunner})
	w.RegisterActivityWithOptions(activities.DrainRunnerEvents,
		activity.RegisterOptions{Name: weavetemporal.ActivityDrainRunnerEvents})
	if err := w.Start(); err != nil {
		return fmt.Errorf("start runner worker: %w", err)
	}
	defer w.Stop()

	go func() {
		ticker := time.NewTicker(reconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if cleaned, err := runners.Reconcile(ctx); err != nil {
					logger.Warn("reconciliation failed", slog.String("error", err.Error()))
				} else if cleaned > 0 {
					logger.Info("reconciliation cleaned up runners", slog.Int("cleaned", cleaned))
				}
			}
		}
	}()

	health := &http.Server{Handler: healthHandler(appPool, temporalClient, backend, logger), ReadHeaderTimeout: 5 * time.Second}
	listener, err := net.Listen("tcp", cfg.HealthAddr)
	if err != nil {
		return fmt.Errorf("bind runner-manager health server on %s: %w", cfg.HealthAddr, err)
	}
	healthFailed := make(chan error, 1)
	go func() {
		if err := health.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			healthFailed <- err
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = health.Shutdown(shutdownCtx)
	}()

	logger.InfoContext(ctx, "runner manager started",
		slog.String("task_queue", queue), slog.String("backend", backend.Name()),
		slog.String("scope", cfg.RunnerScope), slog.String("health", cfg.HealthAddr))

	select {
	case <-ctx.Done():
		return nil
	case err := <-healthFailed:
		return fmt.Errorf("runner-manager health server: %w", err)
	}
}

// healthHandler reports whether the runner manager can do its job: PostgreSQL,
// Temporal, and its backend must all answer.
func healthHandler(pool *pgxpool.Pool, client temporalclient.Client, backend application.RunnerBackend, logger *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health/live", func(w http.ResponseWriter, _ *http.Request) {
		writeHealth(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /health/ready", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		status := map[string]string{"postgres": "ok", "temporal": "ok", "backend": "ok"}
		ready := true
		if err := pool.Ping(ctx); err != nil {
			status["postgres"], ready = "unavailable", false
			logger.WarnContext(ctx, "readiness: postgres is unreachable", slog.String("error", err.Error()))
		}
		if _, err := client.CheckHealth(ctx, &temporalclient.CheckHealthRequest{}); err != nil {
			status["temporal"], ready = "unavailable", false
			logger.WarnContext(ctx, "readiness: temporal is unreachable", slog.String("error", err.Error()))
		}
		if _, err := backend.List(ctx); err != nil {
			status["backend"], ready = "unavailable", false
			logger.WarnContext(ctx, "readiness: the runner backend is unreachable", slog.String("error", err.Error()))
		}
		code := http.StatusOK
		if !ready {
			code = http.StatusServiceUnavailable
		}
		writeHealth(w, code, status)
	})
	return mux
}

func writeHealth(w http.ResponseWriter, code int, body map[string]string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
