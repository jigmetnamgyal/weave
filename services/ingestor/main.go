// Command ingestor accepts session events from runners: it validates,
// deduplicates, sequences and persists them, and quarantines what it refuses.
//
// Its own process rather than a goroutine in the worker, because
// context/architecture.md names the event ingestor as a scaling role of its
// own. Ingestion scales with runner output and workflows with sessions; an
// ingestor stuck on a poison message must not stall workflows, and a worker
// whose Temporal connection is down must not stop ingestion.
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
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jigmetnamgyal/weave/internal/adapters/eventstream"
	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/services/ingestor/internal/config"
)

const shutdownTimeout = 10 * time.Second

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "weave-ingestor: %v\n", err)
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

	appPool, err := pgxpool.New(ctx, cfg.AppDatabaseURL)
	if err != nil {
		return fmt.Errorf("configure application postgres pool: %w", err)
	}
	defer appPool.Close()

	// Unlike the API, the ingestor cannot do anything without NATS, so failing
	// to connect at startup is fatal.
	conn, err := nats.Connect(cfg.NATSURL,
		nats.Name("weave-ingestor"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	)
	if err != nil {
		return fmt.Errorf("connect to nats: %w", err)
	}
	defer conn.Close()

	js, err := jetstream.New(conn)
	if err != nil {
		return fmt.Errorf("open jetstream: %w", err)
	}

	streamCfg := eventstream.DefaultConfig()
	setupCtx, cancelSetup := context.WithTimeout(ctx, 30*time.Second)
	defer cancelSetup()
	// Created or verified; a drifted stream fails startup rather than being
	// silently reconfigured.
	if err := eventstream.EnsureStream(setupCtx, js, streamCfg); err != nil {
		return err
	}

	ingestor := application.NewIngestor(
		postgres.NewEventStore(appPool),
		postgres.WithTenantWorkspace,
		streamCfg.SubjectPrefix,
		time.Now,
		logger,
	)
	consumer, err := eventstream.NewConsumer(setupCtx, js, streamCfg, ingestor, logger)
	if err != nil {
		return err
	}

	health := &http.Server{
		Handler:           healthHandler(appPool, conn, logger),
		ReadHeaderTimeout: 5 * time.Second,
	}
	// Bound before consuming, so an address in use fails startup rather than
	// leaving an ingestor with no endpoint to tell it apart from a stuck one.
	listener, err := net.Listen("tcp", cfg.HealthAddr)
	if err != nil {
		return fmt.Errorf("bind ingestor health server on %s: %w", cfg.HealthAddr, err)
	}
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

	logger.InfoContext(ctx, "ingestor started",
		slog.String("stream", streamCfg.Stream),
		slog.String("consumer", streamCfg.Consumer),
		slog.String("health", cfg.HealthAddr))

	consumeCtx, cancelConsume := context.WithCancel(ctx)
	defer cancelConsume()
	consumed := make(chan error, 1)
	go func() { consumed <- consumer.Run(consumeCtx) }()

	var (
		failure         error
		consumerStopped bool
	)
	select {
	case <-ctx.Done():
	case failure = <-healthFailed:
		logger.Error("the ingestor health server stopped; shutting down", slog.String("error", failure.Error()))
		failure = fmt.Errorf("ingestor health server: %w", failure)
	case failure = <-consumed:
		// The channel's only value is taken here, so the drain wait below
		// must not wait for another — it would sit out the whole timeout and
		// then warn about a drain that had already happened.
		consumerStopped = true
		if failure == nil {
			failure = errors.New("the consumer stopped unexpectedly")
		}
	}
	logger.Info("ingestor shutting down")
	cancelConsume()

	if !consumerStopped {
		select {
		case err := <-consumed:
			if failure == nil && err != nil {
				failure = err
			}
		case <-time.After(shutdownTimeout):
			// Unacknowledged events are redelivered after the ack wait, so a
			// hard stop loses nothing.
			logger.Warn("the consumer did not drain in time; unacknowledged events will be redelivered")
		}
	}
	return failure
}
