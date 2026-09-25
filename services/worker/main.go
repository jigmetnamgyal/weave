// Command worker runs the durable session workflow and the outbox publisher.
//
// It is a separate process from the API deliberately.
// `context/architecture.md` gives workflow workers their own deployment unit,
// and the alternative — a third goroutine beside the API's two sweeps — would
// have every API replica polling one table and would make Temporal a serving
// dependency of a process whose requests never touch it. That is also what
// keeps the API's readiness probe unchanged: Temporal is this process's
// dependency, not the API's.
package main

import (
	"context"
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
	"github.com/redis/go-redis/v9"
	"go.temporal.io/sdk/activity"
	temporalclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	githubadapter "github.com/jigmetnamgyal/weave/internal/adapters/github"
	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	weaveredis "github.com/jigmetnamgyal/weave/internal/adapters/redis"
	weavetemporal "github.com/jigmetnamgyal/weave/internal/adapters/temporal"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/services/worker/internal/config"
)

const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		// The logger may not exist yet at this point, so startup failures go
		// to stderr in plain text.
		fmt.Fprintf(os.Stderr, "weave-worker: %v\n", err)
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

	// The application role, as everywhere else: row-level security applies to
	// this process too. The one cross-workspace read it needs — claiming the
	// outbox — goes through a SECURITY DEFINER function rather than through a
	// privileged connection, so everything else stays scoped.
	appPool, err := pgxpool.New(ctx, cfg.AppDatabaseURL)
	if err != nil {
		return fmt.Errorf("configure application postgres pool: %w", err)
	}
	defer appPool.Close()

	// Unlike the API, this process cannot do its job without Temporal, so a
	// failure to reach it at startup is fatal rather than something readiness
	// reports. That asymmetry is the point of the publisher living here.
	temporalClient, err := temporalclient.Dial(temporalclient.Options{
		HostPort: cfg.TemporalHostPort,
		Logger:   logger,
	})
	if err != nil {
		return fmt.Errorf("connect to temporal at %s: %w", cfg.TemporalHostPort, err)
	}
	defer temporalClient.Close()

	sessionStore := postgres.NewSessionStore(appPool)
	sessions := application.NewSessionService(
		sessionStore, postgres.NewTaskStore(appPool), postgres.NewAgentStore(appPool))

	// GitHub, since M5.2: the workflow cuts the session's branch.
	//
	// Deliberately **not** part of readiness. Redis and GitHub are reached per
	// activity, under a bounded retry policy, and a session whose branch cannot
	// be cut is failed with a reason that says so. A worker that reported
	// unready whenever GitHub did would be restarted by its orchestrator for an
	// outage it cannot fix, and would stop draining the outbox for sessions
	// that never needed GitHub at that moment. The API makes the same choice.
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

	githubClient, err := githubadapter.NewClient(
		cfg.GitHubAppID, cfg.GitHubAppPrivateKeyPath, weaveredis.NewTokenCache(redisClient))
	if err != nil {
		return fmt.Errorf("configure github client: %w", err)
	}
	// No install-state store and no slug: the worker never begins an
	// installation. If that ever changes, the nil store panics on first use
	// rather than quietly binding an installation nowhere.
	installations := application.NewInstallationService(
		postgres.NewInstallationStore(appPool),
		nil,
		githubadapter.NewPort(githubClient),
		postgres.WithTenantWorkspace,
		"",
	)
	branches := application.NewSessionBranchService(sessionStore, installations)

	activities := weavetemporal.NewSessionActivities(sessions, branches)

	w := worker.New(temporalClient, weavetemporal.TaskQueue, worker.Options{})
	w.RegisterWorkflowWithOptions(weavetemporal.SessionWorkflow, workflowOptions())
	w.RegisterActivityWithOptions(activities.MarkProvisioning,
		activityOptions(weavetemporal.ActivityMarkProvisioning))
	w.RegisterActivityWithOptions(activities.FailUnprovisionable,
		activityOptions(weavetemporal.ActivityFailUnprovisionable))
	w.RegisterActivityWithOptions(activities.CreateBranch,
		activityOptions(weavetemporal.ActivityCreateBranch))
	w.RegisterActivityWithOptions(activities.FailSession,
		activityOptions(weavetemporal.ActivityFailSession))

	if err := w.Start(); err != nil {
		return fmt.Errorf("start temporal worker: %w", err)
	}
	defer w.Stop()

	publisher := application.NewPublisher(
		postgres.NewOutboxStore(appPool),
		weavetemporal.NewStarter(temporalClient),
		logger,
	)

	// Health is served even though the worker serves no product traffic: a
	// deployment system needs to tell a working worker from one that is
	// running and unable to do anything, and a startup check alone makes the
	// process look healthy forever from the moment it starts.
	health := &http.Server{
		Handler:           healthHandler(appPool, temporalClient, logger),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Bound before anything else starts, so an address already in use fails
	// startup rather than leaving a worker that runs without the endpoint it
	// was given one for. `ListenAndServe` in a goroutine cannot do that: it
	// binds after the caller has moved on, and the failure arrives as a log
	// line nobody is reading.
	listener, err := net.Listen("tcp", cfg.HealthAddr)
	if err != nil {
		return fmt.Errorf("bind worker health server on %s: %w", cfg.HealthAddr, err)
	}

	// A later failure is fatal too, for the same reason it was worth binding
	// early: a worker with no health endpoint is one a deployment system
	// cannot tell from a working one, and a stalled worker that looks healthy
	// is exactly what this exists to prevent.
	healthFailed := make(chan error, 1)
	go func() {
		if err := health.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			healthFailed <- err
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := health.Shutdown(shutdownCtx); err != nil {
			logger.Error("health server shutdown failed", slog.String("error", err.Error()))
		}
	}()

	logger.InfoContext(ctx, "worker started",
		slog.String("task_queue", weavetemporal.TaskQueue),
		slog.String("temporal", cfg.TemporalHostPort),
		slog.String("health", cfg.HealthAddr),
	)

	// The publisher owns this goroutine until shutdown; Run returns when the
	// context is cancelled.
	done := make(chan struct{})
	go func() {
		defer close(done)
		publisher.Run(ctx)
	}()

	// Either a signal, or the health server failing — which is not something
	// to carry on without.
	var healthErr error
	select {
	case <-ctx.Done():
	case healthErr = <-healthFailed:
		logger.Error("the worker health server stopped; shutting down",
			slog.String("error", healthErr.Error()))
		stop()
	}
	logger.Info("worker shutting down")

	select {
	case <-done:
	case <-time.After(shutdownTimeout):
		// A publisher mid-delivery holds a lease, which expires on its own, so
		// a hard stop here loses nothing that the recovery path does not
		// already cover.
		logger.Warn("the publisher did not stop in time; its leases will expire")
	}

	if healthErr != nil {
		return fmt.Errorf("worker health server: %w", healthErr)
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil
	}
	return ctx.Err()
}

// workflowOptions registers the workflow under a stable name.
//
// Named explicitly rather than by function name: a running workflow is resumed
// by this string, so renaming the Go function must not strand executions that
// were started under the old one.
func workflowOptions() workflow.RegisterOptions {
	return workflow.RegisterOptions{Name: weavetemporal.SessionWorkflowName}
}

// activityOptions registers an activity under a stable name, for the same
// reason.
func activityOptions(name string) activity.RegisterOptions {
	return activity.RegisterOptions{Name: name}
}
