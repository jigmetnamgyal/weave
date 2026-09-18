package temporal

import (
	"context"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
)

// postgresTenant puts the workspace into the context the stores read.
//
// An activity arrives with no tenant, and under FORCE row-level security an
// absent one matches no row — a read that finds nothing and reports success,
// which is how M5.0's retention sweep silently never ran. The workspace comes
// from the workflow input, which came from the claimed outbox row, which is
// the only place it is safe to take one from.
//
// There is no user: the workflow acts as the system, and the write path is
// authorized by that rather than by a membership.
func postgresTenant(ctx context.Context, workspaceID uuid.UUID) context.Context {
	return postgres.WithTenant(ctx, postgres.TenantContext{WorkspaceID: workspaceID})
}
