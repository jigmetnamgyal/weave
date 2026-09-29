package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// RunnerStore persists runners.
type RunnerStore struct {
	pool *pgxpool.Pool
}

// NewRunnerStore wires the store.
func NewRunnerStore(pool *pgxpool.Pool) *RunnerStore { return &RunnerStore{pool: pool} }

var _ application.RunnerStore = (*RunnerStore)(nil)

// Create inserts a runner, refusing a second live one for the session.
func (s *RunnerStore) Create(ctx context.Context, runner domain.Runner) (domain.Runner, error) {
	var created domain.Runner
	err := inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		row, err := q.CreateRunner(ctx, postgresdb.CreateRunnerParams{
			ID: runner.ID, SessionID: runner.SessionID, WorkspaceID: runner.WorkspaceID, Backend: runner.Backend,
		})
		if err != nil {
			if constraintViolated(err, "runners_one_live_per_session") {
				return application.ErrRunnerExists
			}
			if constraintViolated(err, "runners_session_fkey") {
				return application.ErrSessionNotFound
			}
			return fmt.Errorf("create runner: %w", err)
		}
		created = runnerToDomain(row)
		return nil
	})
	return created, err
}

// LiveForSession returns the session's live runner, if any.
func (s *RunnerStore) LiveForSession(ctx context.Context, sessionID, workspaceID uuid.UUID) (domain.Runner, error) {
	return s.one(ctx, func(q *postgresdb.Queries) (postgresdb.Runner, error) {
		return q.GetLiveRunnerForSession(ctx, postgresdb.GetLiveRunnerForSessionParams{
			SessionID: sessionID, WorkspaceID: workspaceID,
		})
	})
}

// SetHandle records the backend's handle.
func (s *RunnerStore) SetHandle(ctx context.Context, runnerID, workspaceID uuid.UUID, handle string) (domain.Runner, error) {
	return s.one(ctx, func(q *postgresdb.Queries) (postgresdb.Runner, error) {
		return q.SetRunnerHandle(ctx, postgresdb.SetRunnerHandleParams{ID: runnerID, WorkspaceID: workspaceID, BackendHandle: &handle})
	})
}

// MarkRunning records a provisioning runner ready.
func (s *RunnerStore) MarkRunning(ctx context.Context, runnerID, workspaceID uuid.UUID) (domain.Runner, error) {
	return s.one(ctx, func(q *postgresdb.Queries) (postgresdb.Runner, error) {
		return q.MarkRunnerRunning(ctx, postgresdb.MarkRunnerRunningParams{ID: runnerID, WorkspaceID: workspaceID})
	})
}

// MarkTerminating records that teardown has begun.
func (s *RunnerStore) MarkTerminating(ctx context.Context, runnerID, workspaceID uuid.UUID) (domain.Runner, error) {
	return s.one(ctx, func(q *postgresdb.Queries) (postgresdb.Runner, error) {
		return q.MarkRunnerTerminating(ctx, postgresdb.MarkRunnerTerminatingParams{ID: runnerID, WorkspaceID: workspaceID})
	})
}

// End records the runner terminated or failed.
func (s *RunnerStore) End(
	ctx context.Context,
	runnerID, workspaceID uuid.UUID,
	state domain.RunnerState,
	reason string,
) (domain.Runner, error) {
	if state != domain.RunnerTerminated && state != domain.RunnerFailed {
		return domain.Runner{}, fmt.Errorf("end runner: %s is not an end state", state)
	}
	var failure *string
	if reason != "" {
		trimmed := domain.SafeText(reason, 500)
		failure = &trimmed
	}
	return s.one(ctx, func(q *postgresdb.Queries) (postgresdb.Runner, error) {
		return q.EndRunner(ctx, postgresdb.EndRunnerParams{
			EndState: string(state), FailureReason: failure, ID: runnerID, WorkspaceID: workspaceID,
		})
	})
}

// ListToReconcile reads live runners across every workspace through the one
// bounded privileged function. Each carries its own workspace, which is what
// every write after this is scoped by.
func (s *RunnerStore) ListToReconcile(
	ctx context.Context,
	limit int,
	after application.ReconcileCursor,
) ([]application.RunnerToReconcile, error) {
	var (
		afterCreated pgtype.Timestamptz
		afterID      pgtype.UUID
	)
	if after.ID != uuid.Nil {
		afterCreated = pgtype.Timestamptz{Time: after.CreatedAt, Valid: true}
		afterID = pgtype.UUID{Bytes: after.ID, Valid: true}
	}
	rows, err := s.pool.Query(ctx, `SELECT id, session_id, workspace_id, backend, backend_handle, state, session_state, created_at, updated_at
		FROM weave_runners_to_reconcile($1, $2, $3)`, limit, afterCreated, afterID)
	if err != nil {
		return nil, fmt.Errorf("list runners to reconcile: %w", err)
	}
	defer rows.Close()

	var out []application.RunnerToReconcile
	for rows.Next() {
		var (
			runner       domain.Runner
			handle       *string
			state        string
			sessionState string
			updatedAt    time.Time
		)
		if err := rows.Scan(&runner.ID, &runner.SessionID, &runner.WorkspaceID, &runner.Backend,
			&handle, &state, &sessionState, &runner.CreatedAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan runner to reconcile: %w", err)
		}
		if handle != nil {
			runner.Handle = *handle
		}
		runner.State = domain.RunnerState(state)
		out = append(out, application.RunnerToReconcile{
			Runner: runner, SessionState: domain.SessionState(sessionState), StateSince: updatedAt,
		})
	}
	return out, rows.Err()
}

// one runs a single-row query in the tenant transaction on ctx.
func (s *RunnerStore) one(ctx context.Context, query func(*postgresdb.Queries) (postgresdb.Runner, error)) (domain.Runner, error) {
	var runner domain.Runner
	err := inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		row, err := query(q)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrRunnerNotFound
			}
			return fmt.Errorf("runner: %w", err)
		}
		runner = runnerToDomain(row)
		return nil
	})
	return runner, err
}

func runnerToDomain(row postgresdb.Runner) domain.Runner {
	runner := domain.Runner{
		ID: row.ID, SessionID: row.SessionID, WorkspaceID: row.WorkspaceID,
		Backend: row.Backend, State: domain.RunnerState(row.State),
		CreatedAt: timestamp(row.CreatedAt),
	}
	if row.BackendHandle != nil {
		runner.Handle = *row.BackendHandle
	}
	if row.FailureReason != nil {
		runner.FailureReason = *row.FailureReason
	}
	if row.ReadyAt.Valid {
		t := row.ReadyAt.Time
		runner.ReadyAt = &t
	}
	if row.TerminatedAt.Valid {
		t := row.TerminatedAt.Time
		runner.TerminatedAt = &t
	}
	return runner
}
