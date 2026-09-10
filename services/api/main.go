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

	"github.com/jigmetnamgyal/weave/services/api/internal/config"
	"github.com/jigmetnamgyal/weave/services/api/internal/health"
	"github.com/jigmetnamgyal/weave/services/api/internal/telemetry"
)

// shutdownTimeout bounds how long in-flight requests may take to drain before
// the process exits.
const shutdownTimeout = 15 * time.Second

func main() {
	if err := run(); err != nil {
		// Configuration and bind failures are startup errors: report them on
		// stderr in plain text, because the logger may not exist yet.
		fmt.Fprintf(os.Stderr, "weave-api: %v\n", err)
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
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("configure postgres pool: %w", err)
	}
	defer pool.Close()

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
		{Name: "postgres", Probe: pool.Ping},
		{Name: "redis", Probe: func(ctx context.Context) error { return redisClient.Ping(ctx).Err() }},
		{Name: "nats", Probe: natsProbe(natsConn)},
	}

	mux := http.NewServeMux()
	mux.Handle("GET /health/live", health.Live())
	mux.Handle("GET /health/ready", withTimeout(cfg.ReadinessTimeout, health.Ready(logger, checks...)))

	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           mux,
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
func natsProbe(conn *nats.Conn) health.ProbeFunc {
	return func(context.Context) error {
		if !conn.IsConnected() {
			return fmt.Errorf("nats connection status %s", conn.Status())
		}
		if _, err := conn.RTT(); err != nil {
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
