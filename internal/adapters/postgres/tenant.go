package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
)

// TenantContext is the identity a database transaction runs under.
//
// Row-level security reads it through `app.user_id` and `app.workspace_id`,
// and the policies match no row when a setting is absent. That is deliberate:
// a query that forgets to establish context returns nothing rather than
// everything.
type TenantContext struct {
	// UserID is set for every authenticated request.
	UserID uuid.UUID
	// WorkspaceID is set only when the operation is scoped to one workspace.
	// It is deliberately absent for the two that cannot be — listing the
	// workspaces a user belongs to, and accepting an invitation.
	WorkspaceID uuid.UUID
}

type tenantKey int

const tenantContextKey tenantKey = iota

// WithTenant attaches the tenant identity to a context.
//
// Passing it through the context rather than through every store signature
// makes it impossible to call a store method having forgotten to thread it —
// and the consequence of forgetting is a query that returns nothing, not one
// that returns another tenant's rows.
func WithTenant(ctx context.Context, tenant TenantContext) context.Context {
	return context.WithValue(ctx, tenantContextKey, tenant)
}

// WithTenantWorkspace attaches a workspace with no user.
//
// For work that has no acting user at all: a webhook delivery is authenticated
// by its signature, not by a session, and there is nobody to attribute it to.
// The policies that matter here key on the workspace, so an absent user
// narrows access rather than widening it — a read that needed the membership
// arm would return nothing rather than everything.
func WithTenantWorkspace(ctx context.Context, workspaceID uuid.UUID) context.Context {
	return WithTenant(ctx, TenantContext{WorkspaceID: workspaceID})
}

// TenantFrom returns the tenant identity carried by a context, if any.
func TenantFrom(ctx context.Context) TenantContext {
	tenant, _ := ctx.Value(tenantContextKey).(TenantContext)
	return tenant
}

// applyTenantContext sets the tenant identity on an open transaction.
//
// `SET LOCAL`, never `SET`: the setting is discarded at commit or rollback, so
// a pooled connection cannot carry one request's tenant into the next. A
// session-scoped setting would leak across requests and produce precisely the
// cross-tenant read the policies exist to prevent.
//
// set_config is used rather than literal `SET LOCAL` because the values are
// bound parameters, and `SET` accepts no placeholders.
func applyTenantContext(ctx context.Context, tx pgx.Tx, tenant TenantContext) error {
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.user_id', $1, true)", uuidSetting(tenant.UserID)); err != nil {
		return fmt.Errorf("set tenant user context: %w", err)
	}
	if _, err := tx.Exec(ctx,
		"SELECT set_config('app.workspace_id', $1, true)", uuidSetting(tenant.WorkspaceID)); err != nil {
		return fmt.Errorf("set tenant workspace context: %w", err)
	}
	return nil
}

// uuidSetting renders an identifier for set_config, mapping the zero value to
// the empty string. The policy helpers turn an empty setting into NULL, which
// matches no row — so an unset workspace narrows access rather than widening
// it.
func uuidSetting(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}

// inTenantTx runs fn inside a transaction carrying the caller's tenant
// context.
//
// Every tenant-owned read and write goes through here. Reads need it as much
// as writes: without the context the policies match nothing, so a read outside
// a transaction would silently return an empty result rather than failing
// loudly.
func inTenantTx(ctx context.Context, pool *pgxpool.Pool, fn func(*postgresdb.Queries) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		// Rollback after a successful commit is a no-op, so this is safe
		// unconditionally and covers the panic path.
		_ = tx.Rollback(ctx)
	}()

	if err := applyTenantContext(ctx, tx, TenantFrom(ctx)); err != nil {
		return err
	}

	if err := fn(postgresdb.New(tx)); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}
