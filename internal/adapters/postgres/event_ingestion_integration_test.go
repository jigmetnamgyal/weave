package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/jigmetnamgyal/weave/internal/adapters/eventstream"
	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// ingestionHarness is a real JetStream stream and a real ingestor over the
// application role.
//
// Real rather than mocked, as M5.1 decided for Temporal: redelivery, the ack
// wait and the delivery ceiling are the mechanism under test, and a mock would
// assert our belief about them.
//
// **A stream per test**, with its own subject prefix, so a `make dev`
// ingestor running beside the tests consumes none of their events. M5.2's
// review round found the worker doing exactly that to the outbox.
type ingestionHarness struct {
	js        jetstream.JetStream
	cfg       eventstream.Config
	publisher *eventstream.Publisher
	ownerPool *pgxpool.Pool
	appPool   *pgxpool.Pool
	store     application.EventStore
}

func newIngestionHarness(t *testing.T) *ingestionHarness {
	t.Helper()
	url := os.Getenv("TEST_NATS_URL")
	if url == "" {
		t.Skip("TEST_NATS_URL is not set; run `make test-integration`")
	}
	conn, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("connect to nats: %v", err)
	}
	t.Cleanup(conn.Close)
	js, err := jetstream.New(conn)
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}

	id := strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	cfg := eventstream.Config{
		Stream:        "TEST_SESSION_EVENTS_" + id,
		SubjectPrefix: "weavetest" + id + ".session",
		Consumer:      "test-ingestor",
		AckWait:       2 * time.Second,
	}
	ctx := context.Background()
	if err := eventstream.EnsureStream(ctx, js, cfg); err != nil {
		t.Fatalf("ensure stream: %v", err)
	}
	t.Cleanup(func() { _ = js.DeleteStream(context.Background(), cfg.Stream) })

	h := &ingestionHarness{
		js: js, cfg: cfg, publisher: eventstream.NewPublisher(js, cfg),
		ownerPool: newPool(t), appPool: newAppPool(t),
	}
	h.store = postgres.NewEventStore(h.appPool)
	return h
}

// run starts an ingestor over store until the returned stop is called.
func (h *ingestionHarness) run(t *testing.T, store application.EventStore) (stop func()) {
	t.Helper()
	ingestor := application.NewIngestor(store, postgres.WithTenantWorkspace, h.cfg.SubjectPrefix,
		time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	consumer, err := eventstream.NewConsumer(context.Background(), h.js, h.cfg, ingestor,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = consumer.Run(ctx) }()
	var once atomic.Bool
	stop = func() {
		if once.CompareAndSwap(false, true) {
			cancel()
			<-done
		}
	}
	t.Cleanup(stop)
	return stop
}

// session seeds a running session to publish events for.
func (h *ingestionHarness) session(t *testing.T, name string) (sessionFixture, domain.Session) {
	t.Helper()
	fixture := seedSessionFixture(t, h.ownerPool, name)
	session, err := fixture.createSession(t, postgres.NewSessionStore(h.ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(func() {
		// Quarantine rows have no foreign key to cascade with the workspace.
		_, _ = h.ownerPool.Exec(context.Background(),
			`DELETE FROM session_event_quarantine WHERE session_id = $1 OR subject LIKE $2`,
			session.ID, h.cfg.SubjectPrefix+".%")
	})
	return fixture, session
}

func message(fixture sessionFixture, session domain.Session, runner uuid.UUID, text string) domain.EventEnvelope {
	payload, _ := json.Marshal(domain.MessageCreated{MessageID: uuid.New(), Role: "assistant", Text: text})
	return domain.EventEnvelope{
		EventID: uuid.New(), SchemaVersion: "1.0", Type: domain.EventMessageCreated,
		OccurredAt: time.Now().UTC(), Producer: runner, WorkspaceID: fixture.workspace.ID,
		SessionID: session.ID, CorrelationID: uuid.New(), Payload: payload,
	}
}

// publishRaw sends bytes on a session's subject with no Nats-Msg-Id, so the
// stream's own duplicate window cannot absorb a repeat and the database's
// deduplication is what is tested.
func (h *ingestionHarness) publishRaw(t *testing.T, sessionID uuid.UUID, data []byte) {
	t.Helper()
	if _, err := h.js.Publish(context.Background(), domain.EventSubject(h.cfg.SubjectPrefix, sessionID), data); err != nil {
		t.Fatalf("publish: %v", err)
	}
}

func (h *ingestionHarness) events(t *testing.T, fixture sessionFixture, session domain.Session) []domain.SessionEvent {
	t.Helper()
	events, err := postgres.NewEventStore(h.appPool).ListEvents(
		postgres.WithTenantWorkspace(context.Background(), fixture.workspace.ID), session.ID, fixture.workspace.ID)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	return events
}

// eventually polls until check passes or the deadline passes.
func eventually(t *testing.T, within time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// pending reports how many messages the consumer has not acknowledged.
func (h *ingestionHarness) pending(t *testing.T) (uint64, int) {
	t.Helper()
	consumer, err := h.js.Consumer(context.Background(), h.cfg.Stream, h.cfg.Consumer)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	info, err := consumer.Info(context.Background())
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	return info.NumPending, info.NumAckPending
}

// TestPublishedEventsArePersistedInOrderIntegration is the unit's first check:
// an event published appears with sequence 1, a second with 2, and publishing
// the first again changes nothing — with the ingestor on the application role
// and no ambient tenant, the condition under which a read matches nothing.
func TestPublishedEventsArePersistedInOrderIntegration(t *testing.T) {
	h := newIngestionHarness(t)
	fixture, session := h.session(t, "Ingestion Order Workspace")
	h.run(t, h.store)

	runner := uuid.New()
	first := message(fixture, session, runner, "first")
	second := message(fixture, session, runner, "second")
	for _, envelope := range []domain.EventEnvelope{first, second} {
		if err := h.publisher.Publish(context.Background(), envelope); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	eventually(t, 10*time.Second, "two events stored", func() bool { return len(h.events(t, fixture, session)) == 2 })

	// The first again, bypassing JetStream's duplicate window.
	raw, _ := json.Marshal(first)
	h.publishRaw(t, session.ID, raw)
	eventually(t, 10*time.Second, "the repeat to be acknowledged", func() bool {
		pending, ackPending := h.pending(t)
		return pending == 0 && ackPending == 0
	})

	events := h.events(t, fixture, session)
	if len(events) != 2 {
		t.Fatalf("%d events after a repeat, want 2", len(events))
	}
	if events[0].EventID != first.EventID || events[0].Sequence != 1 ||
		events[1].EventID != second.EventID || events[1].Sequence != 2 {
		t.Errorf("events = %+v, want first at 1 and second at 2", events)
	}
	if events[0].ReceivedAt.IsZero() || events[0].WorkspaceID != fixture.workspace.ID {
		t.Errorf("stored %+v; want received_at set and the session's workspace", events[0])
	}
}

// TestRefusedEventsAreQuarantinedWithoutPayloadIntegration: each refusal is
// recorded with its reason and acknowledged — not redelivered forever.
func TestRefusedEventsAreQuarantinedWithoutPayloadIntegration(t *testing.T) {
	h := newIngestionHarness(t)
	fixture, session := h.session(t, "Quarantine Workspace")
	other := seedSessionFixture(t, h.ownerPool, "Other Tenant")
	h.run(t, h.store)

	runner := uuid.New()
	secret := "SECRET-SOURCE-LINE"
	cases := map[string]func() domain.EventEnvelope{
		string(domain.RefusalWorkspaceMismatch): func() domain.EventEnvelope {
			e := message(fixture, session, runner, secret)
			e.WorkspaceID = other.workspace.ID
			return e
		},
		string(domain.RefusalUnknownType): func() domain.EventEnvelope {
			e := message(fixture, session, runner, secret)
			e.Type = "file.teleported"
			return e
		},
		string(domain.RefusalUnknownMajor): func() domain.EventEnvelope {
			e := message(fixture, session, runner, secret)
			e.SchemaVersion = "2.0"
			return e
		},
	}
	for _, build := range cases {
		if err := h.publisher.Publish(context.Background(), build()); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}
	// Another session's id on this session's subject.
	mismatched := message(fixture, session, runner, secret)
	mismatched.SessionID = uuid.New()
	raw, _ := json.Marshal(mismatched)
	h.publishRaw(t, session.ID, raw)

	eventually(t, 10*time.Second, "four quarantine rows", func() bool {
		var n int
		_ = h.ownerPool.QueryRow(context.Background(),
			`SELECT count(*) FROM session_event_quarantine WHERE subject LIKE $1`, h.cfg.SubjectPrefix+".%").Scan(&n)
		return n == 4
	})

	rows, err := h.ownerPool.Query(context.Background(),
		`SELECT reason, row_to_json(q)::text FROM session_event_quarantine q WHERE subject LIKE $1`,
		h.cfg.SubjectPrefix+".%")
	if err != nil {
		t.Fatalf("read quarantine: %v", err)
	}
	defer rows.Close()
	reasons := map[string]bool{}
	for rows.Next() {
		var reason, whole string
		if err := rows.Scan(&reason, &whole); err != nil {
			t.Fatal(err)
		}
		reasons[reason] = true
		if strings.Contains(whole, secret) {
			t.Errorf("a %s quarantine row contains the payload: %s", reason, whole)
		}
	}
	for want := range cases {
		if !reasons[want] {
			t.Errorf("no %s row in %v", want, reasons)
		}
	}
	if !reasons[string(domain.RefusalSubjectMismatch)] {
		t.Errorf("no subject_mismatch row in %v", reasons)
	}
	if n := len(h.events(t, fixture, session)); n != 0 {
		t.Errorf("%d refused events were stored", n)
	}
	eventually(t, 5*time.Second, "refusals acknowledged", func() bool {
		pending, ackPending := h.pending(t)
		return pending == 0 && ackPending == 0
	})
}

// flakyStore fails AppendEvent a set number of times, or always.
type flakyStore struct {
	application.EventStore
	failures atomic.Int32
	always   bool
	calls    atomic.Int32
}

func (f *flakyStore) AppendEvent(ctx context.Context, event domain.SessionEvent) (int64, bool, error) {
	f.calls.Add(1)
	if f.always || f.failures.Add(-1) >= 0 {
		return 0, false, errors.New("database: connection refused (injected)")
	}
	return f.EventStore.AppendEvent(ctx, event)
}

// TestATransientFailureIsRedeliveredAndStoredOnceIntegration: not acked, so
// JetStream brings it back, and it lands exactly once.
func TestATransientFailureIsRedeliveredAndStoredOnceIntegration(t *testing.T) {
	h := newIngestionHarness(t)
	fixture, session := h.session(t, "Transient Workspace")
	store := &flakyStore{EventStore: h.store}
	store.failures.Store(2)
	h.run(t, store)

	if err := h.publisher.Publish(context.Background(), message(fixture, session, uuid.New(), "retry me")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	eventually(t, 20*time.Second, "the event stored after redelivery", func() bool {
		return len(h.events(t, fixture, session)) == 1
	})
	if calls := store.calls.Load(); calls < 3 {
		t.Errorf("appended on %d deliveries, want at least 3 — two failures, then success", calls)
	}
	var quarantined int
	_ = h.ownerPool.QueryRow(context.Background(),
		`SELECT count(*) FROM session_event_quarantine WHERE session_id = $1`, session.ID).Scan(&quarantined)
	if quarantined != 0 {
		t.Error("a transient failure was quarantined as though the event were bad")
	}
}

// TestTheFinalDeliveryIsQuarantinedNotDroppedIntegration: at the ceiling
// JetStream stops redelivering. Without the ingestor checking the count, the
// event would simply vanish.
func TestTheFinalDeliveryIsQuarantinedNotDroppedIntegration(t *testing.T) {
	h := newIngestionHarness(t)
	fixture, session := h.session(t, "Exhausted Workspace")
	store := &flakyStore{EventStore: h.store, always: true}
	h.run(t, store)

	envelope := message(fixture, session, uuid.New(), "never stored")
	if err := h.publisher.Publish(context.Background(), envelope); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Backoff 1+2+4+8 seconds across five deliveries.
	eventually(t, 40*time.Second, "a delivery_exhausted row", func() bool {
		var n int
		_ = h.ownerPool.QueryRow(context.Background(),
			`SELECT count(*) FROM session_event_quarantine
			 WHERE event_id = $1 AND reason = 'delivery_exhausted' AND delivery_count = $2`,
			envelope.EventID, eventstream.ConsumerMaxDeliver).Scan(&n)
		return n == 1
	})
	if calls := store.calls.Load(); calls != eventstream.ConsumerMaxDeliver {
		t.Errorf("attempted %d times, want exactly the ceiling of %d", calls, eventstream.ConsumerMaxDeliver)
	}
}

// TestAKilledIngestorLosesNothingIntegration: an ingestor takes a message and
// dies before acknowledging. After the ack wait it is redelivered, and a
// healthy ingestor stores it once.
func TestAKilledIngestorLosesNothingIntegration(t *testing.T) {
	h := newIngestionHarness(t)
	fixture, session := h.session(t, "Killed Ingestor Workspace")

	// Create the durable consumer, then take the message through it and walk
	// away — which is what a process killed mid-ingestion looks like from the
	// server's side.
	stop := h.run(t, &flakyStore{EventStore: h.store, always: true})
	stop()
	envelope := message(fixture, session, uuid.New(), "survive a crash")
	if err := h.publisher.Publish(context.Background(), envelope); err != nil {
		t.Fatalf("publish: %v", err)
	}
	consumer, err := h.js.Consumer(context.Background(), h.cfg.Stream, h.cfg.Consumer)
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}
	batch, err := consumer.Fetch(1, jetstream.FetchMaxWait(5*time.Second))
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	taken := 0
	for range batch.Messages() {
		taken++ // received, never acknowledged
	}
	if taken != 1 {
		t.Fatalf("the dying ingestor took %d messages, want 1", taken)
	}

	h.run(t, h.store)
	eventually(t, 15*time.Second, "the abandoned event stored", func() bool {
		return len(h.events(t, fixture, session)) == 1
	})
	if events := h.events(t, fixture, session); events[0].EventID != envelope.EventID || events[0].Sequence != 1 {
		t.Errorf("stored %+v, want the abandoned event at sequence 1", events[0])
	}
}

// TestADriftedStreamIsReportedNotReconfiguredIntegration: a stream whose
// retention was changed by hand fails startup rather than being silently put
// back — changing retention on a live stream can discard events.
func TestADriftedStreamIsReportedNotReconfiguredIntegration(t *testing.T) {
	h := newIngestionHarness(t)
	stream, err := h.js.Stream(context.Background(), h.cfg.Stream)
	if err != nil {
		t.Fatal(err)
	}
	changed := stream.CachedInfo().Config
	changed.MaxAge = time.Hour
	if _, err := h.js.UpdateStream(context.Background(), changed); err != nil {
		t.Fatalf("drift the stream: %v", err)
	}
	if err := eventstream.EnsureStream(context.Background(), h.js, h.cfg); !errors.Is(err, eventstream.ErrStreamDrift) {
		t.Errorf("EnsureStream on a drifted stream = %v, want ErrStreamDrift", err)
	}
	after, _ := h.js.Stream(context.Background(), h.cfg.Stream)
	if after.CachedInfo().Config.MaxAge != time.Hour {
		t.Error("the drifted stream was silently reconfigured")
	}
}
