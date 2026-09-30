package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// RegistryStore persists the registry proxy's record (Unit M5.4c, ADR-016).
type RegistryStore struct {
	pool *pgxpool.Pool
}

var _ application.RegistryStore = (*RegistryStore)(nil)

// NewRegistryStore builds the store over the application role's pool.
func NewRegistryStore(pool *pgxpool.Pool) *RegistryStore { return &RegistryStore{pool: pool} }

// RunnerForRegistryRequest resolves a runner by id alone.
//
// Through weave_runner_for_registry_request, the proxy's one privileged read:
// it has no tenant context, and under FORCE RLS a direct read would match
// nothing. One id in, at most one row out, so it cannot enumerate; the call is
// raw SQL, as ResolveSession's is, because sqlc cannot see a set-returning
// function's columns.
func (s *RegistryStore) RunnerForRegistryRequest(ctx context.Context, runnerID uuid.UUID) (application.RegistryRunner, error) {
	var (
		runner application.RegistryRunner
		state  string
	)
	err := s.pool.QueryRow(ctx,
		`SELECT session_id, workspace_id, backend, state FROM weave_runner_for_registry_request($1)`, runnerID).
		Scan(&runner.SessionID, &runner.WorkspaceID, &runner.Backend, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return application.RegistryRunner{}, domain.ErrRunnerNotFound
	}
	if err != nil {
		return application.RegistryRunner{}, fmt.Errorf("resolve runner for registry request: %w", err)
	}
	runner.State = domain.RunnerState(state)
	return runner, nil
}

// RecordRegistryRequest appends one row in the tenant context on ctx. The
// insert policy refuses a workspace other than the context's, so a caller that
// passed a request's own claim through would fail here rather than write into
// another tenant. requested_at is the database's clock.
func (s *RegistryStore) RecordRegistryRequest(ctx context.Context, request application.RegistryRequest) error {
	return inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		return q.InsertRegistryRequest(ctx, postgresdb.InsertRegistryRequestParams{
			ID: request.ID, WorkspaceID: request.WorkspaceID, SessionID: request.SessionID,
			RunnerID: request.RunnerID, Host: request.Host, Method: request.Method, Path: request.Path,
		})
	})
}

// ListRegistryRequests reads a session's record, oldest first, in the tenant
// context on ctx.
func (s *RegistryStore) ListRegistryRequests(ctx context.Context, sessionID, workspaceID uuid.UUID) ([]application.RegistryRequest, error) {
	var out []application.RegistryRequest
	err := inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		rows, err := q.ListRegistryRequests(ctx, postgresdb.ListRegistryRequestsParams{SessionID: sessionID, WorkspaceID: workspaceID})
		if err != nil {
			return err
		}
		for _, row := range rows {
			out = append(out, application.RegistryRequest{
				ID: row.ID, WorkspaceID: row.WorkspaceID, SessionID: row.SessionID, RunnerID: row.RunnerID,
				Host: row.Host, Method: row.Method, Path: row.Path, RequestedAt: row.RequestedAt.Time,
			})
		}
		return nil
	})
	return out, err
}
