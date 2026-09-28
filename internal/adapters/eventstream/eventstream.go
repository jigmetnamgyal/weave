// Package eventstream carries session events over NATS JetStream: the stream
// itself, the publisher runners use, and the consumer loop that hands each
// message to the application's Ingestor.
//
// Thin on purpose. Every decision about what an event means is made by
// application.Ingestor, which is testable without NATS; this package only
// acts on its answer — acknowledge, or leave for redelivery.
package eventstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Config names the stream and consumer.
//
// Configurable so a test can run against a stream no running ingestor
// consumes. M5.2's review round found a `make dev` worker claiming the
// integration tests' outbox rows; the same would happen here with one shared
// stream.
type Config struct {
	Stream        string
	SubjectPrefix string
	Consumer      string
	// ExhaustAfter is how many transient failures an event survives before
	// the ingestor quarantines it as delivery_exhausted. The ingestor's
	// ceiling; JetStream's is unlimited. Configurable so a test does not wait
	// out ten backoffs.
	ExhaustAfter int
	// AckWait is how long an ingestor has before a message is redelivered.
	// An ingestion is one short transaction; thirty seconds is far outside
	// it, and short enough that a killed ingestor's messages come back
	// promptly. Configurable so a test of that recovery does not wait for it.
	AckWait time.Duration
}

// DefaultConfig is production's stream.
func DefaultConfig() Config {
	return Config{
		Stream: "SESSION_EVENTS", SubjectPrefix: "weave.session",
		Consumer: "session-event-ingestor", AckWait: 30 * time.Second,
		// With the 30-second backoff cap, about five minutes of failures.
		ExhaustAfter: 10,
	}
}

// Stream limits. Retention is operational, per the architecture:
// session_events is the permanent history, and the stream only has to hold
// what has not been ingested yet.
const (
	// streamMaxAge is how long an unacknowledged event survives. The one way
	// the stream loses an event without telling anyone is an ingestor outage
	// longer than this — three days is far past any outage this system is
	// meant to ride out, and alerting on ingestion lag (M9) is what keeps it
	// from being reached silently.
	streamMaxAge = 72 * time.Hour
	// streamMaxBytes bounds the backlog. With DiscardNew, reaching it makes
	// publishing fail — loud, at the producer — rather than evicting the
	// oldest un-ingested events, which would be silent.
	streamMaxBytes = 1 << 30
	// streamDuplicateWindow is how long JetStream remembers a Nats-Msg-Id.
	// An optimisation only: the database's unique key is the guarantee,
	// because this window expires.
	streamDuplicateWindow = 10 * time.Minute
	// streamMaxMsgSize is above domain.MaxEventBytes, so an oversized event
	// still reaches the ingestor and is quarantined with a reason rather than
	// vanishing at the server — but not by so much that the stream carries
	// megabytes nobody will keep.
	streamMaxMsgSize = 2 * domain.MaxEventBytes

	// consumerMaxDeliver is unlimited, deliberately.
	//
	// The first version set 5 and had the ingestor quarantine on the fifth
	// attempt. A review found the hole: with the database down, the
	// quarantine write fails too, the fifth delivery is spent, JetStream
	// stops, and the event is gone with no record. So JetStream never gives
	// up, and the ceiling is the ingestor's (Config.ExhaustAfter), which only
	// counts once its quarantine row is actually written.
	consumerMaxDeliver = -1
)

// streamConfig is the stream this code expects.
//
// WorkQueue retention removes a message once acknowledged, so the limits
// below bound the *backlog*, not the history.
func streamConfig(cfg Config) jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:        cfg.Stream,
		Description: "Session events from runners, awaiting ingestion. Not the permanent history.",
		Subjects:    []string{cfg.SubjectPrefix + ".*.events"},
		Retention:   jetstream.WorkQueuePolicy,
		Storage:     jetstream.FileStorage,
		Discard:     jetstream.DiscardNew,
		MaxAge:      streamMaxAge,
		MaxBytes:    streamMaxBytes,
		MaxMsgSize:  streamMaxMsgSize,
		Duplicates:  streamDuplicateWindow,
	}
}

// ErrStreamDrift means the stream exists with settings this code did not set.
var ErrStreamDrift = errors.New("the event stream's configuration differs from what this ingestor expects")

// EnsureStream creates the stream, or verifies an existing one matches.
//
// Idempotent, and deliberately not self-healing. A stream whose settings have
// drifted — someone changed retention by hand, or an older release set
// different limits — is reported and startup fails, rather than being
// silently reconfigured: changing retention on a live stream can discard
// events, and that is a decision for a person.
func EnsureStream(ctx context.Context, js jetstream.JetStream, cfg Config) error {
	want := streamConfig(cfg)

	stream, err := js.Stream(ctx, cfg.Stream)
	if errors.Is(err, jetstream.ErrStreamNotFound) {
		if _, err := js.CreateStream(ctx, want); err != nil {
			return fmt.Errorf("create event stream %s: %w", cfg.Stream, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read event stream %s: %w", cfg.Stream, err)
	}

	got := stream.CachedInfo().Config
	var drift []string
	if !slices.Equal(got.Subjects, want.Subjects) {
		drift = append(drift, fmt.Sprintf("subjects %v, want %v", got.Subjects, want.Subjects))
	}
	if got.Retention != want.Retention {
		drift = append(drift, fmt.Sprintf("retention %v, want %v", got.Retention, want.Retention))
	}
	if got.Storage != want.Storage {
		drift = append(drift, fmt.Sprintf("storage %v, want %v", got.Storage, want.Storage))
	}
	if got.Discard != want.Discard {
		drift = append(drift, fmt.Sprintf("discard %v, want %v", got.Discard, want.Discard))
	}
	if got.MaxAge != want.MaxAge || got.MaxBytes != want.MaxBytes ||
		got.MaxMsgSize != want.MaxMsgSize || got.Duplicates != want.Duplicates {
		drift = append(drift, "limits differ")
	}
	if len(drift) > 0 {
		return fmt.Errorf("%w: %s: %v", ErrStreamDrift, cfg.Stream, drift)
	}
	return nil
}

// Publisher sends session events. M5.5's fake provider is its first real
// caller; until then it is exercised by tests.
type Publisher struct {
	js     jetstream.JetStream
	prefix string
}

// NewPublisher wires a publisher for the stream in cfg.
func NewPublisher(js jetstream.JetStream, cfg Config) *Publisher {
	return &Publisher{js: js, prefix: cfg.SubjectPrefix}
}

// Publish sends one event and waits for the stream to store it.
//
// `Nats-Msg-Id` is the event id, so JetStream drops a producer's retry within
// its duplicate window. Past the window the database's unique key does the
// same job; this only saves the ingestor the work.
func (p *Publisher) Publish(ctx context.Context, envelope domain.EventEnvelope) error {
	data, err := json.Marshal(envelope)
	if err != nil {
		return fmt.Errorf("encode event: %w", err)
	}
	if _, err := p.js.Publish(ctx, domain.EventSubject(p.prefix, envelope.SessionID), data,
		jetstream.WithMsgID(envelope.EventID.String())); err != nil {
		return fmt.Errorf("publish event %s: %w", envelope.EventID, err)
	}
	return nil
}

// Consumer feeds the stream to an Ingestor.
type Consumer struct {
	consumer jetstream.Consumer
	ingestor *application.Ingestor
	logger   *slog.Logger
	ackWait  time.Duration
	// exhaustAfter is passed to the ingestor with each delivery.
	exhaustAfter int
}

// NewConsumer creates or updates the durable consumer and wires it.
//
// Explicit acknowledgement, and only ever after the ingestor has answered:
// an event is acknowledged when it is stored, known to be a duplicate, or
// recorded in the quarantine — never before, and never on a failure.
func NewConsumer(
	ctx context.Context,
	js jetstream.JetStream,
	cfg Config,
	ingestor *application.Ingestor,
	logger *slog.Logger,
) (*Consumer, error) {
	if cfg.AckWait <= 0 {
		return nil, errors.New("the event consumer needs a positive ack wait")
	}
	consumer, err := js.CreateOrUpdateConsumer(ctx, cfg.Stream, jetstream.ConsumerConfig{
		Durable:       cfg.Consumer,
		Description:   "Validates, deduplicates, sequences and persists session events.",
		FilterSubject: cfg.SubjectPrefix + ".*.events",
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       cfg.AckWait,
		MaxDeliver:    consumerMaxDeliver,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		return nil, fmt.Errorf("create event consumer %s: %w", cfg.Consumer, err)
	}
	return &Consumer{consumer: consumer, ingestor: ingestor, logger: logger,
		ackWait: cfg.AckWait, exhaustAfter: cfg.ExhaustAfter}, nil
}

// Run consumes until ctx is cancelled.
func (c *Consumer) Run(ctx context.Context) error {
	consuming, err := c.consumer.Consume(func(msg jetstream.Msg) { c.handle(ctx, msg) })
	if err != nil {
		return fmt.Errorf("start consuming events: %w", err)
	}
	<-ctx.Done()
	// Drain rather than stop: a message mid-ingestion finishes and is
	// acknowledged. Anything not yet acknowledged is redelivered after the
	// ack wait, to this ingestor or another.
	consuming.Drain()
	<-consuming.Closed()
	return nil
}

// handle ingests one message and acts on the outcome.
func (c *Consumer) handle(ctx context.Context, msg jetstream.Msg) {
	delivered := uint64(1)
	if meta, err := msg.Metadata(); err == nil {
		delivered = meta.NumDelivered
	}

	// Not the Run context: on shutdown it is already cancelled while Drain
	// lets in-flight messages finish, and ingesting under it would fail every
	// one of them. Bounded well inside the ack wait instead.
	ingestCtx, cancelIngest := context.WithTimeout(context.WithoutCancel(ctx), c.ackWait/2)
	defer cancelIngest()

	outcome := c.ingestor.Ingest(ingestCtx, application.EventDelivery{
		Subject:      msg.Subject(),
		Data:         msg.Data(),
		Delivered:    delivered,
		ExhaustAfter: c.exhaustAfter,
	})

	if outcome == application.IngestRetry {
		// Backed off by attempt, so a database outage is not hammered.
		if err := msg.NakWithDelay(retryDelay(delivered)); err != nil {
			c.logger.WarnContext(ctx, "could not negatively acknowledge an event; it will be redelivered after the ack wait",
				slog.String("error", err.Error()))
		}
		return
	}

	// DoubleAck waits for the server to confirm. A plain Ack that is lost
	// leads to a redelivery the database then absorbs as a duplicate — safe,
	// but waiting makes the common case exact.
	ackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := msg.DoubleAck(ackCtx); err != nil {
		c.logger.WarnContext(ctx, "an event's acknowledgement was not confirmed; a redelivery will be deduplicated",
			slog.String("outcome", outcome.String()), slog.String("error", err.Error()))
	}
}

// retryDelay backs off by attempt: 1s, 2s, 4s, 8s, capped at 30s.
func retryDelay(delivered uint64) time.Duration {
	delay := time.Second << min(delivered-1, 5)
	return min(delay, 30*time.Second)
}
