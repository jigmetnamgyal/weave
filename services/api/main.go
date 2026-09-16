// Command api is the Weave control-plane HTTP service.
//
// This baseline serves only the liveness and readiness probes. Product
// modules, authentication and authorization arrive in later units; the shape
// established here — fail-fast configuration, explicit dependency wiring,
// bounded timeouts and graceful shutdown — is what they are built on.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"

	"github.com/jigmetnamgyal/weave/internal/adapters/clerk"
	githubadapter "github.com/jigmetnamgyal/weave/internal/adapters/github"
	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	weaveredis "github.com/jigmetnamgyal/weave/internal/adapters/redis"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/services/api/internal/auth"
	"github.com/jigmetnamgyal/weave/services/api/internal/config"
	"github.com/jigmetnamgyal/weave/services/api/internal/health"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
	"github.com/jigmetnamgyal/weave/services/api/internal/telemetry"
	"github.com/jigmetnamgyal/weave/services/api/internal/users"
	"github.com/jigmetnamgyal/weave/services/api/internal/workspaces"
)

// shutdownTimeout bounds how long in-flight requests may take to drain before
// the process exits.
const shutdownTimeout = 15 * time.Second

// main starts the API process and reports startup failures to standard error.
func main() {
	if err := run(); err != nil {
		// Configuration and bind failures are startup errors: report them on
		// stderr in plain text, because the logger may not exist yet.
		fmt.Fprintf(os.Stderr, "weave-api: %v\n", err)
		os.Exit(1)
	}
}

// run wires the API dependencies and serves requests until shutdown.
func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	// Cancelled on SIGINT/SIGTERM, which starts graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownTelemetry, err := telemetry.Setup(ctx, cfg.OTelExporter, cfg.OTelServiceName, cfg.AppEnv, os.Stdout)
	if err != nil {
		return fmt.Errorf("bootstrap telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := shutdownTelemetry(shutdownCtx); err != nil {
			logger.Error("telemetry shutdown failed", slog.String("error", err.Error()))
		}
	}()

	// Dependency clients are constructed eagerly but connect lazily. The
	// service must start even when a dependency is down: that is precisely
	// the state readiness exists to report.
	// Two pools, deliberately. The readiness probe connects as the owner so a
	// policy misconfiguration cannot make the service look unhealthy; every
	// request goes through appPool, which authenticates as a role that
	// row-level security applies to.
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("configure postgres pool: %w", err)
	}
	defer pool.Close()

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
	defer func() {
		if err := redisClient.Close(); err != nil {
			logger.Error("redis client close failed", slog.String("error", err.Error()))
		}
	}()

	// RetryOnFailedConnect keeps startup non-blocking when NATS is not up yet.
	natsConn, err := nats.Connect(cfg.NATSURL,
		nats.Name(cfg.OTelServiceName),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	)
	if err != nil {
		return fmt.Errorf("configure nats connection: %w", err)
	}
	defer natsConn.Close()

	checks := []health.Check{
		{Name: "postgres-owner", Probe: pool.Ping},
		// The application pool connects lazily, so without its own probe a
		// broken APP_DATABASE_URL stays invisible until the first request
		// that needs it — and readiness would report healthy throughout.
		{Name: "postgres-app", Probe: appRoleProbe(appPool)},
		{Name: "redis", Probe: func(ctx context.Context) error { return redisClient.Ping(ctx).Err() }},
		{Name: "nats", Probe: natsProbe(natsConn)},
	}

	verifier, err := clerk.New(ctx, clerk.Config{
		Issuer:   cfg.ClerkIssuer,
		Audience: cfg.ClerkJWTAudience,
	})
	if err != nil {
		return fmt.Errorf("configure identity verifier: %w", err)
	}

	// users carries no workspace_id and has no policy, so provisioning uses
	// the application pool like everything else.
	provisioner := application.NewUserProvisioner(postgres.NewUserStore(appPool))
	authMiddleware := auth.NewMiddleware(verifier, provisioner, logger)

	// Every product route lives under /v1 and the whole subtree is wrapped in
	// authentication exactly once. Public routes are enumerated individually
	// below, so adding a route to the product surface makes it protected by
	// default rather than by remembering to protect it.
	protected := http.NewServeMux()
	protected.Handle("GET /v1/me", users.Me())

	workspaceStore := postgres.NewWorkspaceStore(appPool)
	workspaceService := application.NewWorkspaceService(workspaceStore)
	invitationService := application.NewInvitationService(
		postgres.NewInvitationStore(appPool), workspaceStore, time.Now)

	githubClient, err := githubadapter.NewClient(
		cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPath, weaveredis.NewTokenCache(redisClient))
	if err != nil {
		return fmt.Errorf("configure github client: %w", err)
	}
	installationService := application.NewInstallationService(
		postgres.NewInstallationStore(appPool),
		weaveredis.NewInstallStateStore(redisClient),
		githubadapter.NewPort(githubClient),
		postgres.WithTenantWorkspace,
		cfg.GitHubAppSlug,
	)

	// Delivery records are only needed while a retry might still arrive, and
	// nothing else ever removes one — so without this the table grows for the
	// life of the deployment. Started here rather than left to an external
	// cron because a table that grows forever is the kind of thing nobody
	// notices until it is a problem.
	go pruneDeliveries(ctx, logger, installationService)

	taskStore := postgres.NewTaskStore(appPool)
	agentStore := postgres.NewAgentStore(appPool)
	taskService := application.NewTaskService(taskStore)
	agentService := application.NewAgentService(agentStore)
	// The session service reads tasks and agent versions through narrower
	// ports than the stores expose: it must be able to read one of each and
	// must not be able to write either.
	sessionService := application.NewSessionService(
		postgres.NewSessionStore(appPool), taskStore, agentStore)

	workspaceHandler := workspaces.NewHandler(workspaceService, logger)
	workspaceHandler.Register(protected)
	workspaceHandler.RegisterInvitations(protected, invitationService)
	workspaceHandler.RegisterGitHub(protected, installationService, cfg.GitHubWebhookSecret)
	workspaceHandler.RegisterTasks(protected, taskService)
	workspaceHandler.RegisterAgents(protected, agentService)
	workspaceHandler.RegisterSessions(protected, sessionService)

	mux := http.NewServeMux()
	mux.Handle("GET /health/live", health.Live())
	mux.Handle("GET /health/ready", withTimeout(cfg.ReadinessTimeout, health.Ready(logger, checks...)))
	// Mounted before the authenticated subtree so its specific pattern wins
	// over the "/v1/" prefix below. GitHub authenticates with a signature, not
	// a token.
	workspaceHandler.RegisterGitHubWebhook(mux, installationService, cfg.GitHubWebhookSecret)
	mux.Handle("/v1/", authMiddleware.Require(protected))

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           httpx.WithRequestID(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErr := make(chan error, 1)
	go func() {
		logger.Info("api listening",
			slog.String("addr", cfg.HTTPAddr),
			slog.String("env", cfg.AppEnv),
			slog.String("otel_exporter", cfg.OTelExporter),
		)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErr <- err
			return
		}
		serverErr <- nil
	}()

	select {
	case err := <-serverErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		logger.Info("shutdown signal received, draining connections")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("graceful shutdown: %w", err)
	}

	logger.Info("shutdown complete")
	return nil
}

// natsProbe reports NATS reachability with a real round trip, so a connection
// that is open but unresponsive is treated as unavailable.
//
// The round trip is FlushWithContext rather than RTT because RTT flushes with a
// hardcoded 10s timeout, which outlives cfg.ReadinessTimeout; health.Ready waits
// for every probe, so one unresponsive server would hold the whole probe open.
func natsProbe(conn *nats.Conn) health.ProbeFunc {
	return func(ctx context.Context) error {
		if !conn.IsConnected() {
			return fmt.Errorf("nats connection status %s", conn.Status())
		}
		if err := conn.FlushWithContext(ctx); err != nil {
			return fmt.Errorf("nats round trip: %w", err)
		}
		return nil
	}
}

// withTimeout bounds a handler's request context so that a probe against an
// unresponsive dependency fails fast instead of holding the request open.
func withTimeout(timeout time.Duration, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// appRoleProbe reports the application pool healthy only when the role it
// connects as is one that row-level security actually applies to.
//
// PostgreSQL ignores every policy for a superuser or a role holding
// BYPASSRLS. An application connecting as either would run with tenant
// isolation silently switched off — the policies would still be there, still
// look correct, and do nothing. That is the failure this whole unit exists to
// prevent, so it is asserted continuously rather than assumed from
// configuration.
//
// It rides on the readiness probe rather than running at startup because the
// service is required to start while a dependency is down and report the
// state through readiness. A misconfigured role therefore means the instance
// never becomes ready, and so never takes traffic.
func appRoleProbe(pool *pgxpool.Pool) func(context.Context) error {
	return func(ctx context.Context) error {
		var (
			role              string
			superuser, bypass bool
		)
		err := pool.QueryRow(ctx,
			`SELECT current_user, rolsuper, rolbypassrls
			   FROM pg_roles WHERE rolname = current_user`).
			Scan(&role, &superuser, &bypass)
		if err != nil {
			return fmt.Errorf("inspect application role: %w", err)
		}
		if superuser || bypass {
			return fmt.Errorf(
				"application role %q bypasses row-level security (superuser=%t, bypassrls=%t): "+
					"APP_DATABASE_URL must connect as an unprivileged role",
				role, superuser, bypass)
		}
		return nil
	}
}

// deliveryRetention is how long a webhook delivery record is kept.
//
// Deduplication only needs it while GitHub might still retry, which it stops
// doing well inside a day. A week is generous, and leaves the table useful for
// answering "did we receive that delivery" while an incident is being looked
// at.
const deliveryRetention = 7 * 24 * time.Hour

// pruneInterval is how often the sweep runs. Rare, because the work is a
// single indexed DELETE and nothing depends on it being prompt.
const pruneInterval = 6 * time.Hour

// pruneDeliveries removes webhook delivery records past the retention window.
//
// Failures are logged and never fatal: this is housekeeping, and a database
// hiccup during a sweep is not a reason to disturb a healthy process. The
// first sweep is delayed so it does not compete with startup.
func pruneDeliveries(ctx context.Context, logger *slog.Logger, service *application.InstallationService) {
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			removed, err := service.PruneDeliveries(ctx, deliveryRetention)
			if err != nil {
				logger.WarnContext(ctx, "pruning webhook deliveries failed",
					slog.String("error", err.Error()))
				continue
			}
			if removed > 0 {
				logger.InfoContext(ctx, "pruned webhook deliveries",
					slog.Int64("removed", removed))
			}
		}
	}
}
