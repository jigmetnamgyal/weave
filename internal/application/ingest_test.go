package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

const prefix = "weave.session"

// ingestWorld is one session in one workspace, and a store that behaves like
// the real one: duplicates by (session, runner, event), gapless sequences, and
// a tenant that must match the event's workspace.
type ingestWorld struct {
	store     *fakeEventStore
	ingestor  *application.Ingestor
	session   uuid.UUID
	workspace uuid.UUID
	runner    uuid.UUID
}

func newIngestWorld(t *testing.T) *ingestWorld {
	t.Helper()
	w := &ingestWorld{session: uuid.New(), workspace: uuid.New(), runner: uuid.New()}
	w.store = &fakeEventStore{
		sessions: map[uuid.UUID]application.SessionForEvent{
			w.session: {WorkspaceID: w.workspace, State: domain.SessionRunning},
		},
		seen: map[string]bool{},
	}
	w.ingestor = application.NewIngestor(w.store,
		func(ctx context.Context, workspaceID uuid.UUID) context.Context {
			return context.WithValue(ctx, tenantKey{}, workspaceID)
		},
		prefix, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return w
}

type tenantKey struct{}

// envelope builds a valid message.created event for the world's session.
func (w *ingestWorld) envelope(edit func(*domain.EventEnvelope)) []byte {
	payload, _ := json.Marshal(domain.MessageCreated{MessageID: uuid.New(), Role: "assistant", Text: "the secret source line"})
	envelope := domain.EventEnvelope{
		EventID: uuid.New(), SchemaVersion: "1.0", Type: domain.EventMessageCreated,
		OccurredAt: time.Now(), Producer: w.runner, WorkspaceID: w.workspace,
		SessionID: w.session, CorrelationID: uuid.New(), Payload: payload,
	}
	if edit != nil {
		edit(&envelope)
	}
	raw, _ := json.Marshal(envelope)
	return raw
}

func (w *ingestWorld) deliver(data []byte, delivered uint64) application.IngestOutcome {
	return w.ingestor.Ingest(context.Background(), application.EventDelivery{
		Subject: domain.EventSubject(prefix, w.session), Data: data,
		Delivered: delivered, MaxDeliver: 5,
	})
}

func TestIngestPersistsInSequence(t *testing.T) {
	w := newIngestWorld(t)
	for i := 1; i <= 3; i++ {
		if got := w.deliver(w.envelope(nil), 1); got != application.IngestPersisted {
			t.Fatalf("event %d: %v, want persisted", i, got)
		}
	}
	if len(w.store.stored) != 3 || w.store.stored[2].Sequence != 3 {
		t.Errorf("stored %d events, last sequence %d; want 3 and 3", len(w.store.stored), w.store.stored[len(w.store.stored)-1].Sequence)
	}
	// The workspace written is the resolved one, carried on the tenant
	// context — never the event's own claim.
	if w.store.lastTenant != w.workspace {
		t.Errorf("appended under workspace %s, want the session's %s", w.store.lastTenant, w.workspace)
	}
}

func TestADuplicateIsAcknowledgedAndConsumesNothing(t *testing.T) {
	w := newIngestWorld(t)
	event := w.envelope(nil)
	w.deliver(event, 1)
	if got := w.deliver(event, 2); got != application.IngestDuplicate {
		t.Fatalf("redelivery = %v, want duplicate", got)
	}
	next := w.envelope(nil)
	w.deliver(next, 1)
	if last := w.store.stored[len(w.store.stored)-1].Sequence; last != 2 {
		t.Errorf("the event after a duplicate got sequence %d, want 2 — the duplicate used a number", last)
	}
}

// TestRefusalsAreQuarantinedForTheRightReason drives each refusal the spec
// lists, and checks none of them stores anything or keeps a payload.
func TestRefusalsAreQuarantinedForTheRightReason(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(*ingestWorld) (subject string, data []byte)
		want    domain.EventRefusal
	}{
		{"malformed", func(w *ingestWorld) (string, []byte) {
			return domain.EventSubject(prefix, w.session), []byte(`{"broken`)
		}, domain.RefusalMalformed},
		{"unknown major", func(w *ingestWorld) (string, []byte) {
			return domain.EventSubject(prefix, w.session), w.envelope(func(e *domain.EventEnvelope) { e.SchemaVersion = "2.0" })
		}, domain.RefusalUnknownMajor},
		{"unknown type", func(w *ingestWorld) (string, []byte) {
			return domain.EventSubject(prefix, w.session), w.envelope(func(e *domain.EventEnvelope) { e.Type = "file.teleported" })
		}, domain.RefusalUnknownType},
		{"oversize", func(w *ingestWorld) (string, []byte) {
			return domain.EventSubject(prefix, w.session), []byte(strings.Repeat("x", domain.MaxEventBytes+1))
		}, domain.RefusalOversize},
		{"subject names another session", func(w *ingestWorld) (string, []byte) {
			return domain.EventSubject(prefix, uuid.New()), w.envelope(nil)
		}, domain.RefusalSubjectMismatch},
		{"subject does not parse", func(w *ingestWorld) (string, []byte) {
			return "weave.session.nonsense.events", w.envelope(nil)
		}, domain.RefusalUnparseableSubject},
		{"workspace is not the session's", func(w *ingestWorld) (string, []byte) {
			return domain.EventSubject(prefix, w.session), w.envelope(func(e *domain.EventEnvelope) { e.WorkspaceID = uuid.New() })
		}, domain.RefusalWorkspaceMismatch},
		{"unknown session", func(w *ingestWorld) (string, []byte) {
			delete(w.store.sessions, w.session)
			return domain.EventSubject(prefix, w.session), w.envelope(nil)
		}, domain.RefusalUnknownSession},
		{"terminal session", func(w *ingestWorld) (string, []byte) {
			w.store.sessions[w.session] = application.SessionForEvent{WorkspaceID: w.workspace, State: domain.SessionFailed}
			return domain.EventSubject(prefix, w.session), w.envelope(nil)
		}, domain.RefusalSessionTerminal},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := newIngestWorld(t)
			subject, data := tc.arrange(w)
			got := w.ingestor.Ingest(context.Background(), application.EventDelivery{
				Subject: subject, Data: data, Delivered: 1, MaxDeliver: 5,
			})
			if got != application.IngestQuarantined {
				t.Fatalf("outcome = %v, want quarantined", got)
			}
			if len(w.store.stored) != 0 {
				t.Error("a refused event was stored")
			}
			if len(w.store.quarantined) != 1 {
				t.Fatalf("%d quarantine rows, want 1", len(w.store.quarantined))
			}
			record := w.store.quarantined[0]
			if record.Reason != tc.want {
				t.Errorf("reason = %q, want %q", record.Reason, tc.want)
			}
			if len(record.PayloadSHA256) != 64 || record.SizeBytes != len(data) {
				t.Errorf("record = %+v; want the size and a SHA-256, never the content", record)
			}
		})
	}
}

// TestAWorkspaceMismatchRecordsTheRealOwner: the quarantine row names the
// workspace resolved from the session, not the one the event claimed.
func TestAWorkspaceMismatchRecordsTheRealOwner(t *testing.T) {
	w := newIngestWorld(t)
	claimed := uuid.New()
	w.deliver(w.envelope(func(e *domain.EventEnvelope) { e.WorkspaceID = claimed }), 1)
	if got := w.store.quarantined[0].WorkspaceID; got != w.workspace {
		t.Errorf("quarantine workspace = %s, want the session's %s (the event claimed %s)", got, w.workspace, claimed)
	}
}

func TestATransientFailureIsRetriedNotQuarantined(t *testing.T) {
	w := newIngestWorld(t)
	w.store.appendErr = errors.New("database: connection refused")

	if got := w.deliver(w.envelope(nil), 1); got != application.IngestRetry {
		t.Fatalf("outcome = %v, want retry", got)
	}
	if len(w.store.quarantined) != 0 {
		t.Error("a transient failure was quarantined as though the event were bad")
	}
}

// TestTheLastAttemptIsQuarantinedNotDropped is the row of the spec's table
// that is easy to miss: at the delivery ceiling JetStream stops redelivering,
// and a retry answer there means the event vanishes.
func TestTheLastAttemptIsQuarantinedNotDropped(t *testing.T) {
	w := newIngestWorld(t)
	w.store.appendErr = errors.New("database: connection refused")

	if got := w.deliver(w.envelope(nil), 5); got != application.IngestQuarantined {
		t.Fatalf("outcome on the final delivery = %v, want quarantined", got)
	}
	if len(w.store.quarantined) != 1 || w.store.quarantined[0].Reason != domain.RefusalDeliveryExhausted {
		t.Errorf("quarantine = %+v, want one delivery_exhausted row", w.store.quarantined)
	}
}

// TestAnUnrecordableRefusalIsNotAcknowledged: if the quarantine cannot be
// written, acknowledging would drop the event with no record at all.
func TestAnUnrecordableRefusalIsNotAcknowledged(t *testing.T) {
	w := newIngestWorld(t)
	w.store.quarantineErr = errors.New("database: connection refused")

	if got := w.deliver([]byte(`{"broken`), 1); got != application.IngestRetry {
		t.Errorf("outcome = %v, want retry — the refusal was never recorded", got)
	}
}

// --- fake ------------------------------------------------------------------

type fakeEventStore struct {
	sessions      map[uuid.UUID]application.SessionForEvent
	seen          map[string]bool
	stored        []domain.SessionEvent
	quarantined   []application.QuarantinedEvent
	next          int64
	lastTenant    uuid.UUID
	appendErr     error
	quarantineErr error
}

func (f *fakeEventStore) ResolveSession(_ context.Context, id uuid.UUID) (application.SessionForEvent, error) {
	session, ok := f.sessions[id]
	if !ok {
		return application.SessionForEvent{}, application.ErrSessionNotFound
	}
	return session, nil
}

func (f *fakeEventStore) AppendEvent(ctx context.Context, event domain.SessionEvent) (int64, bool, error) {
	if f.appendErr != nil {
		return 0, false, f.appendErr
	}
	tenant, _ := ctx.Value(tenantKey{}).(uuid.UUID)
	f.lastTenant = tenant
	key := event.SessionID.String() + event.RunnerID.String() + event.EventID.String()
	if f.seen[key] {
		return 0, false, nil
	}
	f.seen[key] = true
	f.next++
	event.Sequence = f.next
	f.stored = append(f.stored, event)
	return f.next, true, nil
}

func (f *fakeEventStore) Quarantine(_ context.Context, event application.QuarantinedEvent) error {
	if f.quarantineErr != nil {
		return f.quarantineErr
	}
	f.quarantined = append(f.quarantined, event)
	return nil
}
