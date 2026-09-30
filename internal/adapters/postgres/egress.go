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

// EgressStore persists configuration and reads trigger-created snapshots.
type EgressStore struct{ pool *pgxpool.Pool }

var _ application.EgressStore = (*EgressStore)(nil)

// NewEgressStore uses the application role, never an owner/bypass pool.
func NewEgressStore(pool *pgxpool.Pool) *EgressStore { return &EgressStore{pool: pool} }

// Authorize checks membership/permission at the replay decision under a lock.
func (s *EgressStore) Authorize(ctx context.Context, workspace uuid.UUID, actor application.Actor) error {
	return inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error { return authorizeEgressActor(ctx, q, workspace, actor) })
}

// List rechecks current authority rather than trusting the handler's snapshot.
func (s *EgressStore) List(ctx context.Context, workspace uuid.UUID, actor application.Actor) ([]domain.EgressHost, error) {
	out := []domain.EgressHost{}
	err := inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		if err := authorizeEgressActor(ctx, q, workspace, actor); err != nil {
			return err
		}
		rows, err := q.ListWorkspaceEgressHosts(ctx, workspace)
		if err != nil {
			return err
		}
		for _, row := range rows {
			out = append(out, egressHost(row))
		}
		return nil
	})
	return out, err
}

// Add serializes duplicate/cap decisions with membership changes and snapshots.
func (s *EgressStore) Add(ctx context.Context, host domain.EgressHost, actor application.Actor, audit application.AuditEvent, completion *application.EgressCompletion) (domain.EgressHost, error) {
	var out domain.EgressHost
	err := inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		if err := authorizeEgressActor(ctx, q, host.WorkspaceID, actor); err != nil {
			return err
		}
		exists, err := q.WorkspaceEgressHostExists(ctx, postgresdb.WorkspaceEgressHostExistsParams{WorkspaceID: host.WorkspaceID, Hostname: host.Hostname})
		if err != nil {
			return err
		}
		if exists {
			return application.ErrEgressHostExists
		}
		count, err := q.CountWorkspaceEgressHosts(ctx, host.WorkspaceID)
		if err != nil {
			return err
		}
		if count >= domain.MaxWorkspaceEgressHosts {
			return application.ErrEgressHostLimit
		}
		row, err := q.InsertWorkspaceEgressHost(ctx, postgresdb.InsertWorkspaceEgressHostParams{ID: host.ID, WorkspaceID: host.WorkspaceID, Hostname: host.Hostname, CreatedBy: host.CreatedBy})
		if err != nil {
			return err
		}
		out = egressHost(row)
		if err := appendAudit(ctx, q, audit); err != nil {
			return err
		}
		return completeEgress(ctx, q, host.WorkspaceID, actor, out, "addEgressHost", completion)
	})
	if err != nil {
		return domain.EgressHost{}, err
	}
	return out, nil
}

// Remove records the actual removed hostname with the deletion atomically.
func (s *EgressStore) Remove(ctx context.Context, workspace, id uuid.UUID, actor application.Actor, audit application.AuditEvent, completion *application.EgressCompletion) error {
	return inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		if err := authorizeEgressActor(ctx, q, workspace, actor); err != nil {
			return err
		}
		row, err := q.DeleteWorkspaceEgressHost(ctx, postgresdb.DeleteWorkspaceEgressHostParams{ID: id, WorkspaceID: workspace})
		if errors.Is(err, pgx.ErrNoRows) {
			return application.ErrEgressHostNotFound
		}
		if err != nil {
			return err
		}
		audit.Detail = map[string]any{"hostname": row.Hostname}
		if err := appendAudit(ctx, q, audit); err != nil {
			return err
		}
		return completeEgress(ctx, q, workspace, actor, egressHost(row), "removeEgressHost", completion)
	})
}

// Snapshot distinguishes missing data from an explicit zero-host snapshot.
func (s *EgressStore) Snapshot(ctx context.Context, workspace, runner uuid.UUID) (domain.RunnerEgressSnapshot, error) {
	var out domain.RunnerEgressSnapshot
	err := inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		row, err := q.GetRunnerEgressSnapshot(ctx, postgresdb.GetRunnerEgressSnapshotParams{RunnerID: runner, WorkspaceID: workspace})
		if errors.Is(err, pgx.ErrNoRows) {
			return application.ErrEgressSnapshotMissing
		}
		if err != nil {
			return err
		}
		out = domain.RunnerEgressSnapshot{RunnerID: row.RunnerID, WorkspaceID: row.WorkspaceID, SessionID: row.SessionID, Hosts: row.Hosts, CreatedAt: row.CreatedAt.Time}
		return nil
	})
	return out, err
}

func egressHost(row postgresdb.WorkspaceEgressHost) domain.EgressHost {
	return domain.EgressHost{ID: row.ID, WorkspaceID: row.WorkspaceID, Hostname: row.Hostname, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt.Time}
}

// completeEgress rejects mismatched scopes and commits response bytes with work.
func completeEgress(ctx context.Context, q *postgresdb.Queries, workspace uuid.UUID, actor application.Actor, host domain.EgressHost, endpoint string, c *application.EgressCompletion) error {
	if c == nil {
		return nil
	}
	if c.Claim.Scope.WorkspaceID != workspace || c.Claim.Scope.UserID != actor.UserID || c.Claim.Scope.Endpoint != endpoint || c.Render == nil {
		return fmt.Errorf("invalid egress completion scope")
	}
	status, body, err := c.Render(host)
	if err != nil {
		return err
	}
	if body == nil {
		body = []byte{}
	}
	return completeIdempotency(ctx, q, c.Claim, status, body)
}

// authorizeEgressActor does not permit system identities to edit admin policy.
func authorizeEgressActor(ctx context.Context, q *postgresdb.Queries, workspace uuid.UUID, actor application.Actor) error {
	if actor.System || actor.UserID == uuid.Nil || actor.Required != domain.PermissionWorkspaceManage {
		return application.ErrPermissionDenied
	}
	return authorizeActor(ctx, q, workspace, actor)
}

// NewEgressAuthorizer is the egress proxy's authorizer as production wires it:
// the bounded runner lookup, then the runner's snapshot in its own tenant, on
// the application role. One constructor for the service and its integration
// test, so the test covers the composition the proxy actually runs.
func NewEgressAuthorizer(pool *pgxpool.Pool, backendPrefix string) *application.EgressAuthorizer {
	return application.NewEgressAuthorizer(NewRegistryStore(pool), NewEgressStore(pool), WithTenantWorkspace, backendPrefix)
}
