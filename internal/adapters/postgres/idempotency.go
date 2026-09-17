package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// IdempotencyStore persists idempotency keys.
type IdempotencyStore struct {
	pool *pgxpool.Pool
}

// NewIdempotencyStore returns a store backed by pool.
func NewIdempotencyStore(pool *pgxpool.Pool) *IdempotencyStore {
	return &IdempotencyStore{pool: pool}
}

// Claim takes the key for this attempt, in its own transaction.
//
// **Its own transaction, deliberately, and this is the crux of the unit.** A
// claim written inside the work's transaction is invisible until that
// transaction commits, so a concurrent caller blocks on the unique index for
// the whole duration of somebody else's request and then wakes to find the
// work complete. It is never told that an attempt is in flight, which is the
// one thing it can usefully act on.
//
// Committing first reintroduces the claim that outlives its holder, which is
// what the lease answers — the same answer `outbox_events` reached.
func (s *IdempotencyStore) Claim(
	ctx context.Context,
	record domain.IdempotencyRecord,
	lease time.Duration,
) (application.IdempotencyOutcome, domain.IdempotencyRecord, error) {
	recordID, err := domain.NewIdempotencyRecordID()
	if err != nil {
		return 0, domain.IdempotencyRecord{}, err
	}

	var outcome application.IdempotencyOutcome
	var existing domain.IdempotencyRecord

	err = inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		_, err := q.ClaimIdempotencyKey(ctx, postgresdb.ClaimIdempotencyKeyParams{
			ID:                 recordID,
			WorkspaceID:        record.Scope.WorkspaceID,
			UserID:             record.Scope.UserID,
			Endpoint:           record.Scope.Endpoint,
			IdempotencyKey:     record.Key,
			RequestFingerprint: record.Fingerprint,
			Lease:              intervalOf(lease),
		})
		if err == nil {
			outcome = application.IdempotencyClaimed
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("claim idempotency key: %w", err)
		}

		// No rows means the conflict clause did not match: the row is either
		// in flight or complete. Read it to find out which, and to compare
		// what was asked for the first time.
		row, err := q.GetIdempotencyKey(ctx, postgresdb.GetIdempotencyKeyParams{
			WorkspaceID:    record.Scope.WorkspaceID,
			UserID:         record.Scope.UserID,
			Endpoint:       record.Scope.Endpoint,
			IdempotencyKey: record.Key,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The row was pruned or released between the two statements.
				// Rare, and the honest answer is to let the caller retry
				// rather than guess which of the two it was.
				outcome = application.IdempotencyInFlight
				return nil
			}
			return fmt.Errorf("read idempotency key: %w", err)
		}

		existing = idempotencyToDomain(row)
		switch {
		case !bytesEqual(row.RequestFingerprint, record.Fingerprint):
			// Checked before in-flight and before complete: a key reused for a
			// different request is a caller bug whichever state the first
			// attempt is in, and answering with the first result would hand
			// back work they did not ask for.
			outcome = application.IdempotencyMismatch
		case row.CompletedAt.Valid:
			outcome = application.IdempotencyComplete
		default:
			outcome = application.IdempotencyInFlight
		}
		return nil
	})
	if err != nil {
		return 0, domain.IdempotencyRecord{}, err
	}
	return outcome, existing, nil
}

// Release gives the key back after the work failed.
//
// The fourth outcome, which the spec did not name: without it a caller whose
// request failed for an unrelated reason would be refused for the length of
// the lease, having done nothing wrong.
func (s *IdempotencyStore) Release(ctx context.Context, scope domain.IdempotencyScope, key string) error {
	return inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		if err := q.ReleaseIdempotencyKey(ctx, postgresdb.ReleaseIdempotencyKeyParams{
			WorkspaceID:    scope.WorkspaceID,
			UserID:         scope.UserID,
			Endpoint:       scope.Endpoint,
			IdempotencyKey: key,
		}); err != nil {
			return fmt.Errorf("release idempotency key: %w", err)
		}
		return nil
	})
}

// Prune removes records past the retention window.
func (s *IdempotencyStore) Prune(ctx context.Context, retention time.Duration) (int64, error) {
	var removed int64
	err := inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		count, err := q.PruneIdempotencyKeys(ctx, intervalOf(retention))
		if err != nil {
			return fmt.Errorf("prune idempotency keys: %w", err)
		}
		removed = count
		return nil
	})
	return removed, err
}

// completeIdempotency writes the stored response using the caller's queries,
// so it lands in the same transaction as the work it describes.
//
// Not exported and not a method: it exists to be called from inside another
// store's transaction, which is the only place it is correct to call it.
func completeIdempotency(
	ctx context.Context,
	q *postgresdb.Queries,
	completion application.IdempotentCompletion,
	status int,
	body []byte,
) error {
	if err := q.CompleteIdempotencyKey(ctx, postgresdb.CompleteIdempotencyKeyParams{
		WorkspaceID:     completion.Scope.WorkspaceID,
		UserID:          completion.Scope.UserID,
		Endpoint:        completion.Scope.Endpoint,
		IdempotencyKey:  completion.Key,
		ResponseStatus:  int32Ptr(int32(status)),
		ResponseBody:    body,
		OriginRequestID: stringPtr(completion.OriginRequestID),
	}); err != nil {
		return fmt.Errorf("record idempotent response: %w", err)
	}
	return nil
}

// intervalOf renders a duration as the interval the queries take.
func intervalOf(d time.Duration) pgtype.Interval {
	return pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

func int32Ptr(v int32) *int32 { return &v }

func stringPtr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func idempotencyToDomain(row postgresdb.IdempotencyKey) domain.IdempotencyRecord {
	record := domain.IdempotencyRecord{
		Scope: domain.IdempotencyScope{
			WorkspaceID: row.WorkspaceID,
			UserID:      row.UserID,
			Endpoint:    row.Endpoint,
		},
		Key:         row.IdempotencyKey,
		Fingerprint: row.RequestFingerprint,
	}
	if row.CompletedAt.Valid && row.ResponseStatus != nil {
		response := domain.IdempotentResponse{
			Status: int(*row.ResponseStatus),
			Body:   row.ResponseBody,
		}
		if row.OriginRequestID != nil {
			response.OriginRequestID = *row.OriginRequestID
		}
		record.Response = &response
	}
	return record
}
