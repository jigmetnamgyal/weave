package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// seedUser inserts a user and schedules its removal.
func seedUser(t *testing.T, pool *pgxpool.Pool) domain.User {
	t.Helper()

	subject := uniqueSubject(t)
	cleanup(t, pool, subject)

	store := postgres.NewUserStore(pool)
	user, err := store.UpsertByExternalID(context.Background(), newUser(t, subject))
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return user
}

// seedWorkspace creates a workspace owned by the given user.
func seedWorkspace(t *testing.T, pool *pgxpool.Pool, owner domain.User, name string) domain.Workspace {
	t.Helper()

	service := application.NewWorkspaceService(postgres.NewWorkspaceStore(pool))
	workspace, err := service.Create(context.Background(), owner.ID, name)
	if err != nil {
		t.Fatalf("seed workspace: %v", err)
	}

	t.Cleanup(func() {
		// Members cascade; audit rows deliberately do not, and cannot be
		// deleted at all, which is the behaviour under test elsewhere.
		if _, err := pool.Exec(context.Background(),
			"DELETE FROM workspaces WHERE id = $1", workspace.ID); err != nil {
			t.Errorf("cleanup workspace: %v", err)
		}
	})
	return workspace
}

// addMember puts a user into a workspace at a role.
func addMember(t *testing.T, pool *pgxpool.Pool, workspaceID, userID uuid.UUID, role domain.Role) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1,$2,$3)",
		workspaceID, userID, string(role)); err != nil {
		t.Fatalf("add member: %v", err)
	}
}

func auditCount(t *testing.T, pool *pgxpool.Pool, workspaceID uuid.UUID, action string) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM audit_events WHERE workspace_id = $1 AND action = $2",
		workspaceID, action).Scan(&count); err != nil {
		t.Fatalf("count audit events: %v", err)
	}
	return count
}

// TestCreateInstallsOwnerAndAudit covers the atomic three-part write.
func TestCreateInstallsOwnerAndAudit(t *testing.T) {
	pool := newPool(t)
	owner := seedUser(t, pool)

	workspace := seedWorkspace(t, pool, owner, "Acme Engineering")

	if workspace.Slug != "acme-engineering" {
		t.Errorf("slug = %q, want acme-engineering", workspace.Slug)
	}
	if workspace.Version != 1 {
		t.Errorf("version = %d, want 1", workspace.Version)
	}

	store := postgres.NewWorkspaceStore(pool)
	membership, err := store.GetMembership(context.Background(), workspace.ID, owner.ID)
	if err != nil {
		t.Fatalf("creator has no membership: %v", err)
	}
	if membership.Role != domain.RoleOwner {
		t.Errorf("creator role = %q, want owner", membership.Role)
	}
	if got := auditCount(t, pool, workspace.ID, application.AuditWorkspaceCreated); got != 1 {
		t.Errorf("workspace.created audit rows = %d, want 1", got)
	}
}

// TestCrossTenantReadsReturnNothing is the isolation guarantee this unit
// exists for: a workspace identifier is not enough to read a workspace.
func TestCrossTenantReadsReturnNothing(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewWorkspaceStore(pool)
	ctx := context.Background()

	insider := seedUser(t, pool)
	outsider := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, insider, "Private Workspace")

	// The outsider holds a perfectly valid identifier.
	if _, err := store.GetForMember(ctx, workspace.ID, outsider.ID); !errors.Is(err, application.ErrWorkspaceNotFound) {
		t.Errorf("GetForMember for a non-member returned %v, want ErrWorkspaceNotFound", err)
	}
	if _, err := store.GetMembership(ctx, workspace.ID, outsider.ID); !errors.Is(err, application.ErrMemberNotFound) {
		t.Errorf("GetMembership for a non-member returned %v, want ErrMemberNotFound", err)
	}

	listed, err := store.ListForUser(ctx, outsider.ID)
	if err != nil {
		t.Fatalf("ListForUser: %v", err)
	}
	for _, item := range listed {
		if item.Workspace.ID == workspace.ID {
			t.Error("a workspace the user does not belong to appeared in their list")
		}
	}

	// And the insider still can, so the check is not simply refusing everyone.
	if _, err := store.GetForMember(ctx, workspace.ID, insider.ID); err != nil {
		t.Errorf("member cannot read their own workspace: %v", err)
	}
}

// TestLastOwnerCannotBeDemotedOrRemoved covers the invariant that keeps a
// workspace administrable.
func TestLastOwnerCannotBeDemotedOrRemoved(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewWorkspaceStore(pool)
	ctx := context.Background()

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Solo Workspace")

	event := application.AuditEvent{WorkspaceID: workspace.ID, ActorUserID: owner.ID, Action: "test"}

	_, err := store.ChangeMemberRole(ctx, workspace.ID, owner.ID, domain.RoleAdmin, event)
	if !errors.Is(err, domain.ErrLastOwner) {
		t.Errorf("demoting the last owner returned %v, want ErrLastOwner", err)
	}

	if err := store.RemoveMember(ctx, workspace.ID, owner.ID, event); !errors.Is(err, domain.ErrLastOwner) {
		t.Errorf("removing the last owner returned %v, want ErrLastOwner", err)
	}

	// Still an owner afterwards.
	membership, err := store.GetMembership(ctx, workspace.ID, owner.ID)
	if err != nil || membership.Role != domain.RoleOwner {
		t.Errorf("owner membership was damaged: role=%q err=%v", membership.Role, err)
	}

	// A refused change must leave no audit row.
	if got := auditCount(t, pool, workspace.ID, "test"); got != 0 {
		t.Errorf("refused changes wrote %d audit rows, want 0", got)
	}
}

// TestSecondOwnerCanBeDemoted proves the last-owner rule blocks only the last
// one, rather than freezing ownership entirely.
func TestSecondOwnerCanBeDemoted(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewWorkspaceStore(pool)
	ctx := context.Background()

	owner := seedUser(t, pool)
	second := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Shared Workspace")
	addMember(t, pool, workspace.ID, second.ID, domain.RoleOwner)

	updated, err := store.ChangeMemberRole(ctx, workspace.ID, second.ID, domain.RoleDeveloper,
		application.AuditEvent{WorkspaceID: workspace.ID, ActorUserID: owner.ID, Action: application.AuditMemberRoleChanged})
	if err != nil {
		t.Fatalf("demoting a second owner failed: %v", err)
	}
	if updated.Role != domain.RoleDeveloper {
		t.Errorf("role = %q, want developer", updated.Role)
	}
	if got := auditCount(t, pool, workspace.ID, application.AuditMemberRoleChanged); got != 1 {
		t.Errorf("role change audit rows = %d, want 1", got)
	}
}

// TestConcurrentOwnerDemotionsKeepOneOwner is why the store takes a lock.
// Without it, two callers each see two owners and each demote one.
func TestConcurrentOwnerDemotionsKeepOneOwner(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewWorkspaceStore(pool)

	first := seedUser(t, pool)
	second := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, first, "Race Workspace")
	addMember(t, pool, workspace.ID, second.ID, domain.RoleOwner)

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)

	for i, target := range []uuid.UUID{first.ID, second.ID} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, errs[i] = store.ChangeMemberRole(context.Background(), workspace.ID, target,
				domain.RoleDeveloper,
				application.AuditEvent{WorkspaceID: workspace.ID, ActorUserID: first.ID, Action: application.AuditMemberRoleChanged})
		}()
	}

	close(start)
	wg.Wait()

	var owners int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM workspace_members WHERE workspace_id = $1 AND role = 'owner'",
		workspace.ID).Scan(&owners); err != nil {
		t.Fatalf("count owners: %v", err)
	}
	if owners != 1 {
		t.Errorf("workspace has %d owners after concurrent demotions, want 1 (errors: %v, %v)",
			owners, errs[0], errs[1])
	}
}

// TestRenameHonoursVersion covers optimistic concurrency.
func TestRenameHonoursVersion(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewWorkspaceStore(pool)
	ctx := context.Background()

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Original Name")
	event := application.AuditEvent{WorkspaceID: workspace.ID, ActorUserID: owner.ID, Action: application.AuditWorkspaceRenamed}

	renamed, err := store.Rename(ctx, workspace.ID, workspace.Version, "New Name", event)
	if err != nil {
		t.Fatalf("rename with the current version failed: %v", err)
	}
	if renamed.Name != "New Name" {
		t.Errorf("name = %q, want New Name", renamed.Name)
	}
	if renamed.Version != workspace.Version+1 {
		t.Errorf("version = %d, want %d", renamed.Version, workspace.Version+1)
	}

	// The original version is now stale.
	if _, err := store.Rename(ctx, workspace.ID, workspace.Version, "Third Name", event); !errors.Is(err, application.ErrVersionConflict) {
		t.Errorf("stale rename returned %v, want ErrVersionConflict", err)
	}

	// And the refused rename left nothing behind.
	current, err := store.GetForMember(ctx, workspace.ID, owner.ID)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if current.Name != "New Name" {
		t.Errorf("name = %q after a refused rename, want New Name", current.Name)
	}
	if got := auditCount(t, pool, workspace.ID, application.AuditWorkspaceRenamed); got != 1 {
		t.Errorf("rename audit rows = %d, want 1 — the refused rename wrote one", got)
	}
}

// TestSlugCollisionsAreDisambiguated covers two workspaces sharing a name.
func TestSlugCollisionsAreDisambiguated(t *testing.T) {
	pool := newPool(t)
	owner := seedUser(t, pool)

	first := seedWorkspace(t, pool, owner, "Duplicate Name")
	second := seedWorkspace(t, pool, owner, "Duplicate Name")

	if first.Slug == second.Slug {
		t.Fatalf("both workspaces got slug %q", first.Slug)
	}
	if err := domain.ValidateSlug(second.Slug); err != nil {
		t.Errorf("disambiguated slug %q is not valid: %v", second.Slug, err)
	}
}

// TestAuditRowsSurviveWorkspaceDeletion covers the deliberate absence of a
// foreign key: the record must outlive what it describes.
func TestAuditRowsSurviveWorkspaceDeletion(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	service := application.NewWorkspaceService(postgres.NewWorkspaceStore(pool))

	workspace, err := service.Create(ctx, owner.ID, "Doomed Workspace")
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if _, err := pool.Exec(ctx, "DELETE FROM workspaces WHERE id = $1", workspace.ID); err != nil {
		t.Fatalf("delete workspace: %v", err)
	}

	if got := auditCount(t, pool, workspace.ID, application.AuditWorkspaceCreated); got != 1 {
		t.Errorf("audit rows after deleting the workspace = %d, want 1", got)
	}

	// Membership, by contrast, is expected to cascade.
	var members int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM workspace_members WHERE workspace_id = $1", workspace.ID).Scan(&members); err != nil {
		t.Fatalf("count members: %v", err)
	}
	if members != 0 {
		t.Errorf("members after deleting the workspace = %d, want 0", members)
	}
}

// TestAuditLogIsAppendOnly proves the guarantee is enforced by the database,
// not merely by the code that writes to it.
func TestAuditLogIsAppendOnly(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Audited Workspace")

	for _, statement := range []struct {
		name string
		sql  string
	}{
		{"update", "UPDATE audit_events SET action = 'tampered' WHERE workspace_id = $1"},
		{"delete", "DELETE FROM audit_events WHERE workspace_id = $1"},
	} {
		if _, err := pool.Exec(ctx, statement.sql, workspace.ID); err == nil {
			t.Errorf("%s against audit_events succeeded, want rejection", statement.name)
		}
	}

	// TRUNCATE takes no parameter and fires only a statement-level trigger.
	if _, err := pool.Exec(ctx, "TRUNCATE audit_events"); err == nil {
		t.Error("TRUNCATE against audit_events succeeded, want rejection")
	}
}

// TestRemoveMemberIsScopedToWorkspace proves a membership in one workspace
// cannot be removed by naming another workspace's identifier.
func TestRemoveMemberIsScopedToWorkspace(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewWorkspaceStore(pool)
	ctx := context.Background()

	ownerA := seedUser(t, pool)
	ownerB := seedUser(t, pool)
	member := seedUser(t, pool)

	workspaceA := seedWorkspace(t, pool, ownerA, "Workspace A")
	workspaceB := seedWorkspace(t, pool, ownerB, "Workspace B")
	addMember(t, pool, workspaceA.ID, member.ID, domain.RoleDeveloper)

	// Ask workspace B to remove a member who only belongs to workspace A.
	err := store.RemoveMember(ctx, workspaceB.ID, member.ID,
		application.AuditEvent{WorkspaceID: workspaceB.ID, ActorUserID: ownerB.ID, Action: application.AuditMemberRemoved})
	if !errors.Is(err, application.ErrMemberNotFound) {
		t.Errorf("cross-workspace removal returned %v, want ErrMemberNotFound", err)
	}

	if _, err := store.GetMembership(ctx, workspaceA.ID, member.ID); err != nil {
		t.Errorf("membership in workspace A was affected: %v", err)
	}
}
