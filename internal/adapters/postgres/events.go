package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// EventStore persists session events for the ingestor.
type EventStore struct {
	pool *pgxpool.Pool
}

// NewEventStore wires the store.
func NewEventStore(pool *pgxpool.Pool) *EventStore { return &EventStore{pool: pool} }

var _ application.EventStore = (*EventStore)(nil)

// ResolveSession finds a session's workspace and state by id alone.
//
// Through weave_session_for_event, the one privileged read the ingestor makes:
// it has no tenant context, and under FORCE RLS a direct read would match no
// row and report the session as unknown. The function takes one id and
// returns at most one row, so it cannot enumerate. Everything after this runs
// scoped by the workspace it returns.
func (s *EventStore) ResolveSession(ctx context.Context, sessionID uuid.UUID) (application.SessionForEvent, error) {
	var (
		workspaceID uuid.UUID
		state       string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT workspace_id, state FROM weave_session_for_event($1)`, sessionID).Scan(&workspaceID, &state)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return application.SessionForEvent{}, application.ErrSessionNotFound
		}
		return application.SessionForEvent{}, fmt.Errorf("resolve session for event: %w", err)
	}
	return application.SessionForEvent{WorkspaceID: workspaceID, State: domain.SessionState(state)}, nil
}

// errDuplicateEvent unwinds the append transaction when the event is already
// stored, taking the sequence increment with it.
var errDuplicateEvent = errors.New("event already stored")

// AppendEvent takes the next sequence and stores the event, in one tenant
// transaction.
//
// **A duplicate consumes no sequence**, and not by checking first. The counter
// is incremented, then the insert runs with DO NOTHING on the deduplication
// key; a duplicate inserts nothing, and the transaction is rolled back, which
// returns the number. A check-before-insert would let two replicas both pass
// the check — the counter's row lock is what orders them, and the unique
// constraint is what refuses the second.
//
// The workspace written is the one on ctx — resolved from the session row by
// the caller — and the row-level-security insert policy refuses anything
// else, so a bug upstream that passed the event's own claim through would
// fail here rather than write into another tenant.
func (s *EventStore) AppendEvent(ctx context.Context, event domain.SessionEvent) (int64, bool, error) {
	workspaceID := TenantFrom(ctx).WorkspaceID
	if workspaceID == uuid.Nil {
		return 0, false, errors.New("append event: no workspace in context")
	}

	var sequence int64
	err := inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		next, err := q.NextSessionEventSequence(ctx, postgresdb.NextSessionEventSequenceParams{
			SessionID:   event.SessionID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			return fmt.Errorf("next event sequence: %w", err)
		}

		stored, err := q.AppendSessionEvent(ctx, postgresdb.AppendSessionEventParams{
			SessionID:     event.SessionID,
			Sequence:      next,
			WorkspaceID:   workspaceID,
			RunnerID:      event.RunnerID,
			EventID:       event.EventID,
			Type:          string(event.Type),
			SchemaVersion: event.SchemaVersion,
			CorrelationID: event.CorrelationID,
			OccurredAt:    pgtype.Timestamptz{Time: event.OccurredAt, Valid: true},
			ReceivedAt:    pgtype.Timestamptz{Time: event.ReceivedAt, Valid: true},
			Payload:       event.Payload,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return errDuplicateEvent
			}
			return fmt.Errorf("append session event: %w", err)
		}
		sequence = stored
		return nil
	})
	if errors.Is(err, errDuplicateEvent) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return sequence, true, nil
}

// Quarantine records a refused event. No tenant: many refusals never resolved
// one, and the table is INSERT-only for the application role.
func (s *EventStore) Quarantine(ctx context.Context, event application.QuarantinedEvent) error {
	var version *string
	if event.SchemaVersion != "" {
		version = &event.SchemaVersion
	}
	return postgresdb.New(s.pool).QuarantineSessionEvent(ctx, postgresdb.QuarantineSessionEventParams{
		Subject:       event.Subject,
		Reason:        string(event.Reason),
		EventID:       nullUUID(event.EventID),
		SessionID:     nullUUID(event.SessionID),
		WorkspaceID:   nullUUID(event.WorkspaceID),
		SchemaVersion: version,
		SizeBytes:     int32(min(event.SizeBytes, 1<<30)),
		DeliveryCount: int32(min(event.Delivered, 1<<30)),
		PayloadSha256: event.PayloadSHA256,
	})
}

// ListEvents returns a session's events in sequence order, in the tenant
// context on ctx.
func (s *EventStore) ListEvents(ctx context.Context, sessionID, workspaceID uuid.UUID) ([]domain.SessionEvent, error) {
	var events []domain.SessionEvent
	err := inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		rows, err := q.ListSessionEvents(ctx, postgresdb.ListSessionEventsParams{
			SessionID: sessionID, WorkspaceID: workspaceID,
		})
		if err != nil {
			return fmt.Errorf("list session events: %w", err)
		}
		events = make([]domain.SessionEvent, 0, len(rows))
		for _, row := range rows {
			events = append(events, domain.SessionEvent{
				EventID: row.EventID, SessionID: row.SessionID, WorkspaceID: row.WorkspaceID,
				RunnerID: row.RunnerID, CorrelationID: row.CorrelationID,
				Type: domain.EventType(row.Type), SchemaVersion: row.SchemaVersion,
				OccurredAt: timestamp(row.OccurredAt), ReceivedAt: timestamp(row.ReceivedAt),
				Sequence: row.Sequence, Payload: row.Payload,
			})
		}
		return nil
	})
	return events, err
}

// nullUUID renders the zero id as SQL NULL.
func nullUUID(id uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: id, Valid: id != uuid.Nil}
}
