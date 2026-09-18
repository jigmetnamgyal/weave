package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"github.com/jigmetnamgyal/weave/internal/application"
)

// OutboxStore claims and settles outbox rows.
type OutboxStore struct {
	pool *pgxpool.Pool
}

// NewOutboxStore returns a store backed by pool.
func NewOutboxStore(pool *pgxpool.Pool) *OutboxStore {
	return &OutboxStore{pool: pool}
}

// Claim takes a batch of due rows across every workspace.
//
// **The one privileged read in this package's publisher path, and the only
// one.** The publisher polls every tenant's queue, so it cannot carry a
// workspace context — and under FORCE row-level security an absent context
// matches no row, so a direct poll would read nothing and report success. That
// is how M5.0's retention sweep silently never ran.
//
// Everything after the claim is ordinary. The returned rows carry their own
// workspace, and each is settled in a tenant transaction scoped by that value
// — taken from the row rather than from a caller, so there is no identifier a
// caller could supply to reach another tenant's queue.
func (s *OutboxStore) Claim(
	ctx context.Context,
	batchSize int,
	lease time.Duration,
) ([]application.ClaimedOutboxEvent, error) {
	// No tenant context is set deliberately: the function is SECURITY DEFINER
	// and the poll is cross-workspace by nature.
	rows, err := postgresdb.New(s.pool).ClaimOutboxBatch(ctx, postgresdb.ClaimOutboxBatchParams{
		BatchSize: int32(batchSize),
		Lease:     intervalOf(lease),
	})
	if err != nil {
		return nil, fmt.Errorf("claim outbox batch: %w", err)
	}

	claimed := make([]application.ClaimedOutboxEvent, 0, len(rows))
	for _, row := range rows {
		event := application.ClaimedOutboxEvent{
			ID:          row.ID,
			WorkspaceID: row.WorkspaceID,
			Topic:       row.Topic,
			SubjectID:   row.SubjectID,
			Payload:     row.Payload,
			Attempts:    row.Attempts,
		}
		if row.Claimant.Valid {
			event.Claimant = uuid.UUID(row.Claimant.Bytes)
		}
		claimed = append(claimed, event)
	}
	return claimed, nil
}

// Complete marks a delivered row done.
func (s *OutboxStore) Complete(ctx context.Context, event application.ClaimedOutboxEvent) error {
	return s.settle(ctx, event, "complete", func(q *postgresdb.Queries) (int64, error) {
		return q.CompleteOutboxEvent(ctx, postgresdb.CompleteOutboxEventParams{
			ID:       event.ID,
			Claimant: nullableUUID(&event.Claimant),
		})
	})
}

// Retry releases the row to be tried again later.
func (s *OutboxStore) Retry(
	ctx context.Context,
	event application.ClaimedOutboxEvent,
	backoff time.Duration,
	reason string,
) error {
	return s.settle(ctx, event, "retry", func(q *postgresdb.Queries) (int64, error) {
		return q.RetryOutboxEvent(ctx, postgresdb.RetryOutboxEventParams{
			ID:        event.ID,
			Claimant:  nullableUUID(&event.Claimant),
			Backoff:   intervalOf(backoff),
			LastError: truncateError(reason),
		})
	})
}

// Terminate stops a row being tried again, keeping why.
func (s *OutboxStore) Terminate(ctx context.Context, event application.ClaimedOutboxEvent, reason string) error {
	return s.settle(ctx, event, "terminate", func(q *postgresdb.Queries) (int64, error) {
		return q.TerminateOutboxEvent(ctx, postgresdb.TerminateOutboxEventParams{
			ID:        event.ID,
			Claimant:  nullableUUID(&event.Claimant),
			LastError: truncateError(reason),
		})
	})
}

// settle runs one fenced update in the row's own tenant context.
//
// Scoped by the workspace carried on the claimed row, which is what keeps the
// privileged claim from widening into a privileged write: once the publisher
// knows whose row it is, it stops being privileged.
//
// Every settling update is fenced on the claimant, not just completion and
// retry. A publisher that paused past its lease must not be able to terminate
// the claim that replaced it either — the first draft of the M5.1 spec fenced
// two of the three and a reviewer found the third.
func (s *OutboxStore) settle(
	ctx context.Context,
	event application.ClaimedOutboxEvent,
	operation string,
	update func(*postgresdb.Queries) (int64, error),
) error {
	tenant := WithTenant(ctx, TenantContext{WorkspaceID: event.WorkspaceID})
	return inTenantTx(tenant, s.pool, func(q *postgresdb.Queries) error {
		rows, err := update(q)
		if err != nil {
			return fmt.Errorf("%s outbox event: %w", operation, err)
		}
		if rows == 0 {
			// The fence held: this publisher's lease expired and another
			// claimed the row. Reported rather than ignored, because a
			// publisher discovering this has been working on someone else's
			// row and should stop.
			return application.ErrOutboxClaimLost
		}
		return nil
	})
}

// truncateError bounds what is stored, matching the column's CHECK.
//
// A provider error can be long, and an outbox row is not the place a stack
// trace lives. The first 2000 characters carry the cause; the rest is for the
// logs.
func truncateError(reason string) *string {
	if reason == "" {
		return nil
	}
	const max = 2000
	runes := []rune(reason)
	if len(runes) > max {
		reason = string(runes[:max])
	}
	return &reason
}
