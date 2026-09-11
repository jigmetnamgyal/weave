package postgres_test

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// newAppPool connects as the application role — the one the policies apply to.
//
// The other integration tests connect as the owner, which is a superuser
// locally and therefore bypasses RLS entirely. That is fine for testing the
// application's own filtering, but it means those tests say nothing about the
// policies. These do.
func newAppPool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("TEST_APP_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_APP_DATABASE_URL is not set; run `make test-integration`")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connect as the application role: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping as the application role: %v (has `make migrate-up` run?)", err)
	}
	t.Cleanup(pool.Close)

	// A test that silently ran as a superuser would pass while proving
	// nothing, so refuse to continue if the connection can bypass RLS.
	var bypasses bool
	if err := pool.QueryRow(ctx,
		"SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user").Scan(&bypasses); err != nil {
		t.Fatalf("inspect current role: %v", err)
	}
	if bypasses {
		t.Fatal("TEST_APP_DATABASE_URL connects as a role that bypasses RLS; these tests would prove nothing")
	}

	return pool
}

// twoTenants seeds two workspaces owned by two different users, using the
// owner connection so the seeding itself is not subject to the policies.
func twoTenants(t *testing.T, pool *pgxpool.Pool) (a, b domain.Workspace, ownerA, ownerB domain.User) {
	t.Helper()

	ownerA = seedUser(t, pool)
	ownerB = seedUser(t, pool)
	a = seedWorkspace(t, pool, ownerA, "RLS Tenant A")
	b = seedWorkspace(t, pool, ownerB, "RLS Tenant B")
	return a, b, ownerA, ownerB
}

// TestPolicyFiltersWithoutAnyApplicationFilter is the point of this unit.
//
// The query asks for every workspace, with no WHERE clause at all — the
// mistake the convention is supposed to prevent and cannot guarantee. The
// database returns only the tenant in context.
func TestPolicyFiltersWithoutAnyApplicationFilter(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)

	a, b, ownerA, _ := twoTenants(t, ownerPool)

	ctx := postgres.WithTenant(context.Background(), postgres.TenantContext{
		UserID:      ownerA.ID,
		WorkspaceID: a.ID,
	})

	tx, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.user_id', $1, true)", ownerA.ID.String()); err != nil {
		t.Fatalf("set user context: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", a.ID.String()); err != nil {
		t.Fatalf("set workspace context: %v", err)
	}

	rows, err := tx.Query(ctx, "SELECT id FROM workspaces")
	if err != nil {
		t.Fatalf("unfiltered select: %v", err)
	}
	defer rows.Close()

	var seen []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen = append(seen, id)
	}

	if len(seen) != 1 || seen[0] != a.ID {
		t.Fatalf("an unfiltered SELECT returned %v; want only tenant A (%s)", seen, a.ID)
	}
	for _, id := range seen {
		if id == b.ID {
			t.Error("tenant B's workspace was returned to tenant A")
		}
	}
}

// TestPolicyRefusesAnotherTenantNamedExplicitly covers the caller who has the
// other tenant's identifier and asks for it directly.
// insufficientPrivilege is the SQLSTATE PostgreSQL raises when a row-level
// security policy rejects a write.
const insufficientPrivilege = "42501"

// setTenant establishes tenant context and fails the test if it cannot.
//
// Discarding these errors would be the quiet way to break every test in this
// file: absent context denies by design, so a set_config that silently failed
// would produce exactly the empty results the tests assert on, and they would
// pass while exercising nothing.
func setTenant(t *testing.T, ctx context.Context, tx pgx.Tx, userID, workspaceID string) {
	t.Helper()
	if _, err := tx.Exec(ctx, "SELECT set_config('app.user_id', $1, true)", userID); err != nil {
		t.Fatalf("set app.user_id: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", workspaceID); err != nil {
		t.Fatalf("set app.workspace_id: %v", err)
	}
}

func TestPolicyRefusesAnotherTenantNamedExplicitly(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)

	a, b, ownerA, _ := twoTenants(t, ownerPool)
	ctx := context.Background()

	tx, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	setTenant(t, ctx, tx, ownerA.ID.String(), a.ID.String())

	var count int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM workspaces WHERE id = $1", b.ID).Scan(&count); err != nil {
		t.Fatalf("select: %v", err)
	}
	if count != 0 {
		t.Errorf("naming tenant B's id returned %d rows, want 0", count)
	}

	// Membership and audit rows are equally out of reach.
	for _, table := range []string{"workspace_members", "workspace_invitations", "audit_events"} {
		var reachable int
		if err := tx.QueryRow(ctx,
			"SELECT count(*) FROM "+table+" WHERE workspace_id = $1", b.ID).Scan(&reachable); err != nil {
			t.Fatalf("select from %s: %v", table, err)
		}
		if reachable != 0 {
			t.Errorf("%s returned %d of tenant B's rows, want 0", table, reachable)
		}
	}
}

// TestAbsentContextDenies proves the policies fail closed. RLS that returns
// everything when context is missing would be worse than none at all.
func TestAbsentContextDenies(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)

	twoTenants(t, ownerPool)
	ctx := context.Background()

	for _, table := range []string{"workspaces", "workspace_members", "workspace_invitations", "audit_events"} {
		var count int
		if err := appPool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("select from %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s returned %d rows with no tenant context, want 0", table, count)
		}
	}
}

// TestContextDoesNotSurviveTheTransaction is the pooling hazard.
//
// SET LOCAL is discarded at commit, so the next transaction on the same
// connection starts with no tenant. If it persisted, a pooled connection would
// hand one request's tenant to the next — the exact cross-tenant read the
// policies exist to stop, introduced by the mechanism meant to stop it.
func TestContextDoesNotSurviveTheTransaction(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)

	a, _, ownerA, _ := twoTenants(t, ownerPool)
	ctx := context.Background()

	// Pin a single connection so the second transaction demonstrably reuses
	// the one the first ran on.
	conn, err := appPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()

	first, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin first: %v", err)
	}
	_, _ = first.Exec(ctx, "SELECT set_config('app.user_id', $1, true)", ownerA.ID.String())
	_, _ = first.Exec(ctx, "SELECT set_config('app.workspace_id', $1, true)", a.ID.String())

	var visible int
	if err := first.QueryRow(ctx, "SELECT count(*) FROM workspaces").Scan(&visible); err != nil {
		t.Fatalf("first select: %v", err)
	}
	if visible != 1 {
		t.Fatalf("tenant A saw %d workspaces inside its own transaction, want 1", visible)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatalf("commit first: %v", err)
	}

	// Same connection, new transaction, no context set.
	second, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin second: %v", err)
	}
	defer func() { _ = second.Rollback(ctx) }()

	var leaked int
	if err := second.QueryRow(ctx, "SELECT count(*) FROM workspaces").Scan(&leaked); err != nil {
		t.Fatalf("second select: %v", err)
	}
	if leaked != 0 {
		t.Errorf("the previous transaction's tenant context survived: %d rows visible, want 0", leaked)
	}
}

// TestPolicyConstrainsWrites covers the other direction: a caller cannot write
// into a tenant it is not in.
func TestPolicyConstrainsWrites(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)

	a, b, ownerA, _ := twoTenants(t, ownerPool)
	ctx := context.Background()

	tx, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	setTenant(t, ctx, tx, ownerA.ID.String(), a.ID.String())

	// Renaming the other tenant's workspace is different in kind: an UPDATE
	// whose USING clause matches nothing is not an error, it simply touches
	// no rows. Requiring success-with-zero-rows distinguishes that from the
	// statement failing for some unrelated reason.
	//
	// It runs before the insert below because a policy violation aborts the
	// transaction, and every statement after one is refused outright.
	tag, err := tx.Exec(ctx, "UPDATE workspaces SET name = 'seized' WHERE id = $1", b.ID)
	if err != nil {
		t.Fatalf("update across tenants returned an error, want zero rows affected: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Errorf("updated %d rows in another tenant, want 0", tag.RowsAffected())
	}

	// An audit row attributed to the other tenant. The specific SQLSTATE
	// matters: accepting any error would let a schema or connection mistake
	// pass for tenant isolation.
	_, err = tx.Exec(ctx,
		"INSERT INTO audit_events (id, workspace_id, action) VALUES ($1, $2, 'forged')",
		uuid.New(), b.ID)
	if err == nil {
		t.Fatal("wrote an audit row into another tenant, want a policy violation")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != insufficientPrivilege {
		t.Fatalf("insert failed with %v, want SQLSTATE %s (row-level security violation)", err, insufficientPrivilege)
	}

}

// TestInvitationLookupIsLimitedToOneRow covers the deliberate hole.
//
// weave_invitation_by_token is SECURITY DEFINER, so it reads past the
// policies. That is necessary — the acceptor is in no workspace — and is
// exactly why it must not become a general way to read invitations.
func TestInvitationLookupIsLimitedToOneRow(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	ctx := context.Background()

	owner := seedUser(t, ownerPool)
	workspace := seedWorkspace(t, ownerPool, owner, "Token Lookup Workspace")
	invitations, _ := invitationServices(ownerPool, time.Now)

	issued, err := invitations.Issue(
		postgres.WithTenant(ctx, postgres.TenantContext{UserID: owner.ID, WorkspaceID: workspace.ID}),
		ownerOf(workspace.ID, owner.ID), "token-lookup@example.com", domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	hash := domain.HashInvitationToken(issued.Token)

	// With the correct hash it returns exactly one row, with no tenant context.
	var found int
	if err := appPool.QueryRow(ctx,
		"SELECT count(*) FROM weave_invitation_by_token($1)", hash).Scan(&found); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if found != 1 {
		t.Errorf("lookup returned %d rows, want 1", found)
	}

	// It cannot be widened: a wrong hash returns nothing, and there is no
	// argument that returns more than the row matching one hash.
	var none int
	if err := appPool.QueryRow(ctx,
		"SELECT count(*) FROM weave_invitation_by_token($1)", []byte("not-a-real-hash-value-000000000000")).Scan(&none); err != nil {
		t.Fatalf("lookup with a wrong hash: %v", err)
	}
	if none != 0 {
		t.Errorf("a wrong hash returned %d rows, want 0", none)
	}

	// And the table itself stays closed without context.
	var direct int
	if err := appPool.QueryRow(ctx, "SELECT count(*) FROM workspace_invitations").Scan(&direct); err != nil {
		t.Fatalf("direct select: %v", err)
	}
	if direct != 0 {
		t.Errorf("the invitations table returned %d rows without context, want 0", direct)
	}
}

// TestPreviewReachesANonMember is the regression test for the defect this
// unit introduced and review caught.
//
// The preview behind an invitation link reads the invitation joined to its
// workspace. Both tables are policy-protected, and the viewer is by
// definition not a member of anything yet, so with no workspace context both
// policies matched nothing and a legitimate invitee was told their invitation
// did not exist.
//
// It is worth being precise about why the existing suite missed it: those
// tests connect as the owner, which is a superuser locally and bypasses RLS,
// so they exercised the application's filtering and said nothing about the
// policies. This one runs as the application role, which is the only way the
// failure is visible.
func TestPreviewReachesANonMember(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	ctx := context.Background()

	owner := seedUser(t, ownerPool)
	workspace := seedWorkspace(t, ownerPool, owner, "Preview Workspace")
	invitations, _ := invitationServices(ownerPool, time.Now)

	issued, err := invitations.Issue(
		postgres.WithTenant(ctx, postgres.TenantContext{UserID: owner.ID, WorkspaceID: workspace.ID}),
		ownerOf(workspace.ID, owner.ID), "preview@example.com", domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	hash := domain.HashInvitationToken(issued.Token)

	// The invitee: authenticated, but a member of nothing, so no workspace
	// context exists to read under. This is the real shape of the request.
	stranger := seedUser(t, ownerPool)
	strangerCtx := postgres.WithTenant(ctx, postgres.TenantContext{UserID: stranger.ID})

	store := postgres.NewInvitationStore(appPool)
	preview, err := store.Context(strangerCtx, hash)
	if err != nil {
		t.Fatalf("Context for a non-member invitee: %v", err)
	}
	if preview.WorkspaceName != "Preview Workspace" {
		t.Errorf("workspace name = %q, want %q", preview.WorkspaceName, "Preview Workspace")
	}
	if preview.InvitedByEmail != owner.Email {
		t.Errorf("inviter email = %q, want %q", preview.InvitedByEmail, owner.Email)
	}

	// The preview is keyed by the token and returns nothing else: a wrong
	// hash reveals no workspace, and it cannot be used to enumerate.
	if _, err := store.Context(strangerCtx, []byte("not-a-real-hash-value-000000000000")); err == nil {
		t.Error("a wrong hash returned a preview, want not found")
	}

	// And the underlying tables stay closed to that same caller.
	for _, table := range []string{"workspaces", "workspace_invitations"} {
		var count int
		if err := appPool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("select %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s returned %d rows without context, want 0", table, count)
		}
	}
}

// TestPreviewRefusesAnUnusableTokenWithoutTheService checks the backstop
// rather than the gate.
//
// InvitationService.Preview already refuses an expired, revoked or accepted
// token before reaching the store, and there are tests for that. This one
// deliberately skips the service and calls the store directly, because
// weave_invitation_preview_by_token is a SECURITY DEFINER hole in the
// policies: what it returns is reachable by any future caller, including one
// that forgets the check. A hole that depends on its caller checking first is
// not a hole anyone can reason about.
func TestPreviewRefusesAnUnusableTokenWithoutTheService(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	ctx := context.Background()

	owner := seedUser(t, ownerPool)
	stranger := seedUser(t, ownerPool)
	workspace := seedWorkspace(t, ownerPool, owner, "Backstop Workspace")
	invitations, _ := invitationServices(ownerPool, time.Now)
	actor := ownerOf(workspace.ID, owner.ID)
	tenantCtx := postgres.WithTenant(ctx, postgres.TenantContext{UserID: owner.ID, WorkspaceID: workspace.ID})

	revoked, err := invitations.Issue(tenantCtx, actor, "revoked@example.com", domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue revoked: %v", err)
	}
	if err := invitations.Revoke(tenantCtx, actor, revoked.Invitation.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	accepted, err := invitations.Issue(tenantCtx, actor, stranger.Email, domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue accepted: %v", err)
	}
	if _, err := invitations.Accept(
		postgres.WithTenant(ctx, postgres.TenantContext{UserID: stranger.ID}),
		stranger, accepted.Token); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	store := postgres.NewInvitationStore(appPool)
	viewerCtx := postgres.WithTenant(ctx, postgres.TenantContext{UserID: seedUser(t, ownerPool).ID})

	for name, token := range map[string]string{
		"revoked":  revoked.Token,
		"accepted": accepted.Token,
	} {
		t.Run(name, func(t *testing.T) {
			preview, err := store.Context(viewerCtx, domain.HashInvitationToken(token))
			if err == nil {
				t.Fatalf("a %s token disclosed workspace %q and inviter %q, want not found",
					name, preview.WorkspaceName, preview.InvitedByEmail)
			}
		})
	}

	// The control: an invitation that is still usable does resolve, so the
	// test above is measuring the predicates and not a broken lookup.
	usable, err := invitations.Issue(tenantCtx, actor, "usable@example.com", domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue usable: %v", err)
	}
	if _, err := store.Context(viewerCtx, domain.HashInvitationToken(usable.Token)); err != nil {
		t.Fatalf("a usable token was refused: %v", err)
	}
}
