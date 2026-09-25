package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// EventDelivery is one message as the transport handed it over.
type EventDelivery struct {
	Subject string
	Data    []byte
	// Delivered counts this attempt: 1 on first delivery.
	Delivered uint64
	// ExhaustAfter is how many transient failures the ingestor tolerates
	// before quarantining the event as delivery_exhausted. The ingestor's
	// ceiling, not the transport's: the transport redelivers without limit,
	// so an event whose quarantine write *also* fails is redelivered later
	// rather than dropped. Zero means never exhaust.
	ExhaustAfter int
}

// IngestOutcome is what the transport must do with the message.
type IngestOutcome int

const (
	// IngestPersisted: stored. Acknowledge.
	IngestPersisted IngestOutcome = iota + 1
	// IngestDuplicate: already stored; nothing written. Acknowledge.
	IngestDuplicate
	// IngestQuarantined: refused, and the refusal recorded. Acknowledge —
	// redelivering something refused for a reason time cannot change only
	// refuses it again.
	IngestQuarantined
	// IngestRetry: a transient failure. Do not acknowledge; redeliver later.
	IngestRetry
)

func (o IngestOutcome) String() string {
	switch o {
	case IngestPersisted:
		return "persisted"
	case IngestDuplicate:
		return "duplicate"
	case IngestQuarantined:
		return "quarantined"
	case IngestRetry:
		return "retry"
	default:
		return "unknown"
	}
}

// SessionForEvent is what the ingestor learns about a session before it
// trusts anything the event says about it.
type SessionForEvent struct {
	WorkspaceID uuid.UUID
	State       domain.SessionState
}

// QuarantinedEvent is a refused event as recorded. No payload field exists,
// so no path can put one here: context/code-standards.md requires the
// quarantine to keep failure metadata without customer content.
type QuarantinedEvent struct {
	Subject       string
	Reason        domain.EventRefusal
	EventID       uuid.UUID
	SessionID     uuid.UUID
	WorkspaceID   uuid.UUID
	SchemaVersion string
	SizeBytes     int
	Delivered     uint64
	PayloadSHA256 string
}

// Errors an EventStore returns that are decisions, not failures: each is
// quarantined, never retried.
var (
	// ErrEventSessionTerminal: the session is terminal and this event was
	// not already stored before it ended.
	ErrEventSessionTerminal = errors.New("the session is terminal")
	// ErrEventRejectedByStore: the database refused the event's content — a
	// constraint the decoder did not anticipate. Retrying cannot change it.
	ErrEventRejectedByStore = errors.New("the database rejected the event's content")
)

// EventStore is the ingestor's persistence port.
type EventStore interface {
	// ResolveSession finds a session's workspace and state by id alone,
	// without a tenant context. ErrSessionNotFound when there is none.
	ResolveSession(ctx context.Context, sessionID uuid.UUID) (SessionForEvent, error)
	// AppendEvent takes the next sequence and stores the event, in the tenant
	// context on ctx. appended is false for a duplicate, in which case no
	// sequence was consumed.
	//
	// It decides terminality itself, under a lock on the session inside the
	// append transaction: ErrEventSessionTerminal for a new event on a
	// terminal session, and a duplicate — not a refusal — for one stored
	// before the session ended. ErrSessionNotFound if the session is gone.
	AppendEvent(ctx context.Context, event domain.SessionEvent) (sequence int64, appended bool, err error)
	// Quarantine records a refusal. It needs no tenant.
	Quarantine(ctx context.Context, event QuarantinedEvent) error
}

// Ingestor decides what happens to each event a runner sends.
//
// Independent of the transport: it takes bytes and a subject and returns what
// to do with the message, so every outcome in the spec's table is testable
// without NATS, and the NATS adapter is a thin loop that acts on the answer.
type Ingestor struct {
	store         EventStore
	bind          TenantBinder
	subjectPrefix string
	now           func() time.Time
	logger        *slog.Logger
}

// NewIngestor wires the ingestor.
func NewIngestor(store EventStore, bind TenantBinder, subjectPrefix string, now func() time.Time, logger *slog.Logger) *Ingestor {
	return &Ingestor{store: store, bind: bind, subjectPrefix: subjectPrefix, now: now, logger: logger}
}

// Ingest validates, deduplicates, sequences and persists one delivery.
//
// The outcomes, decided before the mechanism (the M3.1 lesson):
//
//	persisted                         -> ack after commit
//	duplicate                         -> ack; nothing written, no sequence used
//	transient (database unreachable)  -> retry, until the ingestor's ceiling
//	refused for a reason time cannot change -> quarantine, then ack
//	transient at the ceiling          -> quarantine as delivery_exhausted
//	quarantine cannot be written      -> retry; the transport never drops it
//
// **Every identifier in the event is a claim.** The session comes from the
// subject and must equal the envelope's; the workspace comes from the session
// row and must equal the envelope's. Nothing the producer says is used to
// scope a query.
func (i *Ingestor) Ingest(ctx context.Context, delivery EventDelivery) IngestOutcome {
	subjectSession, err := domain.SessionFromSubject(i.subjectPrefix, delivery.Subject)
	if err != nil {
		return i.quarantine(ctx, delivery, err, uuid.Nil)
	}

	event, err := domain.DecodeEvent(delivery.Data)
	if err != nil {
		return i.quarantine(ctx, delivery, err, uuid.Nil)
	}
	if event.SessionID != subjectSession {
		// A producer publishing on one session's subject about another.
		return i.quarantine(ctx, delivery, &domain.EventRefusalError{
			Reason: domain.RefusalSubjectMismatch, EventID: event.EventID,
			SessionID: subjectSession, SchemaVersion: event.SchemaVersion,
			Detail: "the envelope names a different session from its subject",
		}, uuid.Nil)
	}

	session, err := i.store.ResolveSession(ctx, subjectSession)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) {
			return i.quarantine(ctx, delivery, &domain.EventRefusalError{
				Reason: domain.RefusalUnknownSession, EventID: event.EventID,
				SessionID: subjectSession, SchemaVersion: event.SchemaVersion,
			}, uuid.Nil)
		}
		return i.transient(ctx, delivery, event, uuid.Nil, fmt.Errorf("resolve session: %w", err))
	}

	if event.WorkspaceID != session.WorkspaceID {
		// The attack this field makes possible if believed: a runner naming
		// another tenant. Logged as a security event, with identifiers only.
		i.logger.WarnContext(ctx, "security: an event named a workspace other than its session's",
			slog.String("session_id", subjectSession.String()),
			slog.String("event_id", event.EventID.String()),
			slog.String("runner_id", event.RunnerID.String()),
			slog.String("session_workspace_id", session.WorkspaceID.String()),
			slog.String("claimed_workspace_id", event.WorkspaceID.String()))
		return i.quarantine(ctx, delivery, &domain.EventRefusalError{
			Reason: domain.RefusalWorkspaceMismatch, EventID: event.EventID,
			SessionID: subjectSession, SchemaVersion: event.SchemaVersion,
		}, session.WorkspaceID)
	}

	event.ReceivedAt = i.now().UTC()
	tenant := i.bind(ctx, session.WorkspaceID)
	sequence, appended, err := i.store.AppendEvent(tenant, event)
	switch {
	case errors.Is(err, ErrEventSessionTerminal):
		// Invariant 10: terminal history is immutable. Decided by the store
		// under a lock, not from the state read above, which a transition
		// can overtake. May prove too strict once a real runner's last events
		// race the terminal transition; M5.4 revisits it with a runner that
		// can show the race.
		return i.quarantine(ctx, delivery, &domain.EventRefusalError{
			Reason: domain.RefusalSessionTerminal, EventID: event.EventID,
			SessionID: event.SessionID, SchemaVersion: event.SchemaVersion,
		}, session.WorkspaceID)
	case errors.Is(err, ErrSessionNotFound):
		// Deleted between the lookup and the append.
		return i.quarantine(ctx, delivery, &domain.EventRefusalError{
			Reason: domain.RefusalUnknownSession, EventID: event.EventID,
			SessionID: event.SessionID, SchemaVersion: event.SchemaVersion,
		}, uuid.Nil)
	case errors.Is(err, ErrEventRejectedByStore):
		// A decision, not an outage: retrying would end in delivery_exhausted
		// for something that was never going to succeed.
		return i.quarantine(ctx, delivery, &domain.EventRefusalError{
			Reason: domain.RefusalInvalidPayload, EventID: event.EventID,
			SessionID: event.SessionID, SchemaVersion: event.SchemaVersion,
		}, session.WorkspaceID)
	case err != nil:
		return i.transient(ctx, delivery, event, session.WorkspaceID, fmt.Errorf("append event: %w", err))
	}
	if !appended {
		i.logger.DebugContext(ctx, "duplicate event",
			slog.String("session_id", event.SessionID.String()),
			slog.String("event_id", event.EventID.String()))
		return IngestDuplicate
	}

	i.logger.DebugContext(ctx, "event persisted",
		slog.String("session_id", event.SessionID.String()),
		slog.String("event_id", event.EventID.String()),
		slog.Int64("sequence", sequence))
	return IngestPersisted
}

// transient handles a failure waiting might fix.
//
// Retried until the ingestor's own ceiling, then quarantined as
// delivery_exhausted — not a claim that the event was bad, only that it could
// not be stored in time. The ceiling is the ingestor's rather than JetStream's
// on purpose. The first version relied on JetStream's MaxDeliver and
// quarantined on the attempt that reached it; if the database was down, the
// quarantine write failed too, the last redelivery was spent, and the event
// was gone with no record — the exact loss the ceiling existed to prevent.
// Now the transport never gives up, and an event is acknowledged only once it
// is stored or its quarantine row is.
func (i *Ingestor) transient(
	ctx context.Context,
	delivery EventDelivery,
	event domain.SessionEvent,
	workspaceID uuid.UUID,
	err error,
) IngestOutcome {
	final := delivery.ExhaustAfter > 0 && delivery.Delivered >= uint64(delivery.ExhaustAfter)
	i.logger.WarnContext(ctx, "event ingestion failed transiently",
		slog.String("session_id", event.SessionID.String()),
		slog.String("event_id", event.EventID.String()),
		slog.Uint64("delivered", delivery.Delivered),
		slog.Bool("final_attempt", final),
		// The error is ours — a database error — never payload content.
		slog.String("error", err.Error()))
	if !final {
		return IngestRetry
	}
	return i.quarantine(ctx, delivery, &domain.EventRefusalError{
		Reason: domain.RefusalDeliveryExhausted, EventID: event.EventID,
		SessionID: event.SessionID, SchemaVersion: event.SchemaVersion,
	}, workspaceID)
}

// quarantine records a refusal and tells the transport to acknowledge.
//
// If the record itself cannot be written, the message is **not** acknowledged
// — dropping a refused event with no record is exactly the silent loss the
// quarantine exists to prevent. Because the transport redelivers without
// limit, "not acknowledged" means "tried again later", never "lost".
func (i *Ingestor) quarantine(ctx context.Context, delivery EventDelivery, refusalErr error, workspaceID uuid.UUID) IngestOutcome {
	var refusal *domain.EventRefusalError
	if !errors.As(refusalErr, &refusal) {
		refusal = &domain.EventRefusalError{Reason: domain.RefusalMalformed}
	}

	subject := domain.SafeText(delivery.Subject, 256)
	digest := sha256.Sum256(delivery.Data)
	record := QuarantinedEvent{
		Subject:       subject,
		Reason:        refusal.Reason,
		EventID:       refusal.EventID,
		SessionID:     refusal.SessionID,
		WorkspaceID:   workspaceID,
		SchemaVersion: refusal.SchemaVersion,
		SizeBytes:     len(delivery.Data),
		Delivered:     max(delivery.Delivered, 1),
		PayloadSHA256: hex.EncodeToString(digest[:]),
	}

	if err := i.store.Quarantine(ctx, record); err != nil {
		i.logger.ErrorContext(ctx, "could not quarantine a refused event; leaving it unacknowledged",
			slog.String("reason", string(refusal.Reason)),
			slog.String("event_id", refusal.EventID.String()),
			slog.String("error", err.Error()))
		return IngestRetry
	}

	i.logger.InfoContext(ctx, "event quarantined",
		slog.String("reason", string(refusal.Reason)),
		slog.String("session_id", refusal.SessionID.String()),
		slog.String("event_id", refusal.EventID.String()),
		slog.Int("size_bytes", record.SizeBytes))
	return IngestQuarantined
}
