package postgres_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// githubIDs hands out installation ids that are unique within a process.
//
// The ids must not be fixed constants. github_installation_id is unique across
// the whole table, and the integration database is not reset between runs —
// these tests only get away with it today because seedWorkspace's cleanup
// cascades the installations away with the workspace. That is a dependency on
// a helper in another file behaving a particular way, and it would fail
// confusingly the moment the cascade changed.
var githubIDs atomic.Int64

func nextGitHubID() int64 {
	// A per-process base keeps ids distinct from any left behind by an earlier
	// run whose cleanup did not complete.
	return time.Now().UnixNano()/1000*1000 + githubIDs.Add(1)
}

// connectInstallation binds an installation for a workspace, failing the test
// if it cannot.
func connectInstallation(
	t *testing.T,
	store *postgres.InstallationStore,
	workspace domain.Workspace,
	owner domain.User,
	githubID int64,
) domain.Installation {
	t.Helper()

	id, err := domain.NewInstallationID()
	if err != nil {
		t.Fatalf("new installation id: %v", err)
	}
	ctx := postgres.WithTenant(context.Background(), postgres.TenantContext{
		UserID:      owner.ID,
		WorkspaceID: workspace.ID,
	})

	installation, err := store.Connect(ctx, domain.Installation{
		ID:                  id,
		WorkspaceID:         workspace.ID,
		GitHubID:            githubID,
		AccountLogin:        "acme",
		AccountType:         domain.AccountOrganization,
		RepositorySelection: domain.SelectionSelected,
		ConnectedBy:         owner.ID,
	}, application.Actor{UserID: owner.ID, Required: domain.PermissionRepositoryManage},
		application.AuditEvent{
			WorkspaceID: workspace.ID,
			ActorUserID: owner.ID,
			Action:      application.AuditInstallationConnected,
			Target:      "test",
		})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return installation
}

// TestInstallationCannotBeBoundTwice is the constraint that keeps a repository
// grant from moving between tenants.
//
// GitHub hands back an installation id and nothing identifying the workspace,
// so a replayed callback naming another workspace is the obvious attack. The
// refusal comes from the unique index rather than a prior read, which is what
// makes it race-free — checking first and inserting second leaves a window
// where two requests both see nothing.
func TestInstallationCannotBeBoundTwiceIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewInstallationStore(pool)

	a, b, ownerA, ownerB := twoTenants(t, pool)
	githubID := nextGitHubID()

	connectInstallation(t, store, a, ownerA, githubID)

	// The same GitHub installation, claimed by a different workspace.
	id, err := domain.NewInstallationID()
	if err != nil {
		t.Fatalf("new id: %v", err)
	}
	ctx := postgres.WithTenant(context.Background(), postgres.TenantContext{
		UserID:      ownerB.ID,
		WorkspaceID: b.ID,
	})
	_, err = store.Connect(ctx, domain.Installation{
		ID:                  id,
		WorkspaceID:         b.ID,
		GitHubID:            githubID,
		AccountLogin:        "acme",
		AccountType:         domain.AccountOrganization,
		RepositorySelection: domain.SelectionSelected,
		ConnectedBy:         ownerB.ID,
	}, application.Actor{UserID: ownerB.ID, Required: domain.PermissionRepositoryManage},
		application.AuditEvent{WorkspaceID: b.ID, ActorUserID: ownerB.ID, Action: "test", Target: "test"})

	if !errors.Is(err, domain.ErrInstallationBoundElsewhere) {
		t.Fatalf("second binding returned %v, want ErrInstallationBoundElsewhere", err)
	}

	// And the first binding is untouched: refusing must not have rebound it.
	installations, err := store.List(postgres.WithTenant(context.Background(),
		postgres.TenantContext{UserID: ownerA.ID, WorkspaceID: a.ID}), a.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(installations) != 1 || installations[0].GitHubID != githubID {
		t.Errorf("workspace A holds %d installations, want its original one", len(installations))
	}
}

// TestInstallationIsInvisibleAcrossTenants covers the visibility half of
// authorization: another tenant's installation is absent, not forbidden.
func TestInstallationIsInvisibleAcrossTenantsIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewInstallationStore(pool)

	a, b, ownerA, ownerB := twoTenants(t, pool)
	installation := connectInstallation(t, store, a, ownerA, nextGitHubID())

	// Tenant B naming tenant A's installation id explicitly.
	ctxB := postgres.WithTenant(context.Background(), postgres.TenantContext{
		UserID:      ownerB.ID,
		WorkspaceID: b.ID,
	})
	if _, err := store.Get(ctxB, installation.ID, b.ID); !errors.Is(err, application.ErrInstallationNotFound) {
		t.Errorf("cross-tenant Get returned %v, want ErrInstallationNotFound", err)
	}

	installations, err := store.List(ctxB, b.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(installations) != 0 {
		t.Errorf("tenant B sees %d installations, want 0", len(installations))
	}
}

// TestReconcileWithdrawsWhatIsNoLongerGranted is the behaviour that keeps a
// stale record from outliving the grant it describes.
func TestReconcileWithdrawsWhatIsNoLongerGrantedIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewInstallationStore(pool)

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Reconcile Workspace")
	installation := connectInstallation(t, store, workspace, owner, nextGitHubID())

	ctx := postgres.WithTenant(context.Background(), postgres.TenantContext{
		UserID:      owner.ID,
		WorkspaceID: workspace.ID,
	})

	both := []domain.Repository{
		{GitHubID: 900001, Owner: "acme", Name: "alpha", DefaultBranch: "main", Private: true},
		{GitHubID: 900002, Owner: "acme", Name: "beta", DefaultBranch: "main", Private: false},
	}
	if err := store.Reconcile(ctx, installation.ID, workspace.ID,
		domain.SelectionSelected, both, domain.RequiredPermissions); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The account deselects beta on GitHub.
	if err := store.Reconcile(ctx, installation.ID, workspace.ID,
		domain.SelectionSelected, both[:1], domain.RequiredPermissions); err != nil {
		t.Fatalf("Reconcile after deselection: %v", err)
	}

	repositories, err := store.ListRepositories(ctx, workspace.ID)
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(repositories) != 2 {
		t.Fatalf("got %d repositories, want 2 — a withdrawn one is kept, not deleted", len(repositories))
	}

	granted := map[string]bool{}
	for _, repository := range repositories {
		granted[repository.Name] = repository.Granted
	}
	if !granted["alpha"] {
		t.Error("alpha should still be granted")
	}
	if granted["beta"] {
		t.Error("beta was deselected on GitHub but is still marked granted")
	}
}

// TestRenameKeepsRepositoryIdentity pins that owner and name are descriptions,
// not identity.
//
// A rename that produced a second row would leave the workspace with two
// records for one repository, and any reference to the old one pointing at
// something that no longer exists.
func TestRenameKeepsRepositoryIdentityIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewInstallationStore(pool)

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Rename Workspace")
	installation := connectInstallation(t, store, workspace, owner, nextGitHubID())

	ctx := postgres.WithTenant(context.Background(), postgres.TenantContext{
		UserID:      owner.ID,
		WorkspaceID: workspace.ID,
	})

	before := []domain.Repository{
		{GitHubID: 900010, Owner: "acme", Name: "old-name", DefaultBranch: "main", Private: true},
	}
	if err := store.Reconcile(ctx, installation.ID, workspace.ID,
		domain.SelectionSelected, before, domain.RequiredPermissions); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	first, err := store.ListRepositories(ctx, workspace.ID)
	if err != nil || len(first) != 1 {
		t.Fatalf("ListRepositories = %v, %v", first, err)
	}

	// Same GitHub id, renamed and transferred to a different owner.
	after := []domain.Repository{
		{GitHubID: 900010, Owner: "acme-labs", Name: "new-name", DefaultBranch: "trunk", Private: false},
	}
	if err := store.Reconcile(ctx, installation.ID, workspace.ID,
		domain.SelectionSelected, after, domain.RequiredPermissions); err != nil {
		t.Fatalf("Reconcile after rename: %v", err)
	}

	second, err := store.ListRepositories(ctx, workspace.ID)
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(second) != 1 {
		t.Fatalf("a rename produced %d rows, want 1 — the GitHub id is the identity", len(second))
	}
	if second[0].ID != first[0].ID {
		t.Error("the row identity changed across a rename")
	}
	if second[0].Name != "new-name" || second[0].Owner != "acme-labs" || second[0].DefaultBranch != "trunk" {
		t.Errorf("rename did not update the description: %+v", second[0])
	}
	if !second[0].Granted {
		t.Error("a renamed repository should still be granted")
	}
}

// TestSuspensionIsRecordedWithoutLosingRepositories pins that unsuspending
// restores rather than requiring a fresh install.
func TestSuspensionIsRecordedWithoutLosingRepositoriesIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewInstallationStore(pool)

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Suspension Workspace")
	installation := connectInstallation(t, store, workspace, owner, nextGitHubID())

	ctx := postgres.WithTenant(context.Background(), postgres.TenantContext{
		UserID:      owner.ID,
		WorkspaceID: workspace.ID,
	})
	repositories := []domain.Repository{
		{GitHubID: 900020, Owner: "acme", Name: "gamma", DefaultBranch: "main", Private: true},
	}
	if err := store.Reconcile(ctx, installation.ID, workspace.ID,
		domain.SelectionSelected, repositories, domain.RequiredPermissions); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	at := time.Now().UTC()
	event := application.AuditEvent{WorkspaceID: workspace.ID, Action: application.AuditInstallationSuspended, Target: "test"}
	if err := store.SetSuspended(ctx, installation.ID, workspace.ID, &at, event); err != nil {
		t.Fatalf("SetSuspended: %v", err)
	}

	suspended, err := store.Get(ctx, installation.ID, workspace.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !suspended.Suspended() {
		t.Fatal("installation is not marked suspended")
	}

	// The repositories survive, which is what makes unsuspending a restore.
	held, err := store.ListRepositories(ctx, workspace.ID)
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(held) != 1 || !held[0].Granted {
		t.Errorf("suspension lost the repository records: %+v", held)
	}

	unsuspend := application.AuditEvent{WorkspaceID: workspace.ID, Action: application.AuditInstallationUnsuspended, Target: "test"}
	if err := store.SetSuspended(ctx, installation.ID, workspace.ID, nil, unsuspend); err != nil {
		t.Fatalf("unsuspend: %v", err)
	}
	restored, err := store.Get(ctx, installation.ID, workspace.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if restored.Suspended() {
		t.Error("unsuspending did not clear the flag")
	}
}

// TestDeliveryDeduplication pins that a retried delivery produces one effect.
func TestDeliveryDeduplicationIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewInstallationStore(pool)
	ctx := context.Background()

	delivery := "delivery-" + uuid.NewString()

	first, err := store.RecordDelivery(ctx, delivery, "installation", "created")
	if err != nil {
		t.Fatalf("RecordDelivery: %v", err)
	}
	if !first {
		t.Fatal("the first delivery was reported as already seen")
	}

	second, err := store.RecordDelivery(ctx, delivery, "installation", "created")
	if err != nil {
		t.Fatalf("RecordDelivery on retry: %v", err)
	}
	if second {
		t.Error("a retried delivery was reported as new, so it would be applied twice")
	}
}

// TestGitHubTablesAreClosedWithoutContext extends the row-level security
// checks to the tables this unit adds.
//
// Connecting as the application role is the only way this proves anything: the
// other integration tests connect as the owner, a superuser locally, and
// bypass the policies entirely.
func TestGitHubTablesAreClosedWithoutContextIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	ctx := context.Background()

	owner := seedUser(t, ownerPool)
	workspace := seedWorkspace(t, ownerPool, owner, "RLS GitHub Workspace")
	store := postgres.NewInstallationStore(ownerPool)
	installation := connectInstallation(t, store, workspace, owner, nextGitHubID())

	ownerCtx := postgres.WithTenant(ctx, postgres.TenantContext{UserID: owner.ID, WorkspaceID: workspace.ID})
	if err := store.Reconcile(ownerCtx, installation.ID, workspace.ID, domain.SelectionSelected,
		[]domain.Repository{{GitHubID: 900030, Owner: "acme", Name: "delta", DefaultBranch: "main"}},
		domain.RequiredPermissions); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// No tenant context at all: the policies must match nothing.
	for _, table := range []string{"github_installations", "repositories", "repository_permissions"} {
		var count int
		if err := appPool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("select %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s returned %d rows with no tenant context, want 0", table, count)
		}
	}

	// And with another tenant's context, naming this workspace explicitly.
	other := seedUser(t, ownerPool)
	otherWorkspace := seedWorkspace(t, ownerPool, other, "Other RLS Workspace")

	tx, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	setTenant(t, ctx, tx, other.ID.String(), otherWorkspace.ID.String())

	var count int
	if err := tx.QueryRow(ctx,
		"SELECT count(*) FROM github_installations WHERE workspace_id = $1", workspace.ID).Scan(&count); err != nil {
		t.Fatalf("cross-tenant select: %v", err)
	}
	if count != 0 {
		t.Errorf("naming another tenant's workspace returned %d installations, want 0", count)
	}
}

// TestRemovingAnInstallationKeepsRepositoryHistory is the regression test for
// a defect review caught.
//
// The handler deleted the installation row, and the composite foreign key
// cascaded straight through to the repositories. Those rows are the record
// that access once existed, which is what makes an old audit entry or a
// finished session readable afterwards — deleting them destroys history that
// nothing else holds.
func TestRemovingAnInstallationKeepsRepositoryHistoryIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewInstallationStore(pool)

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Removal Workspace")
	installation := connectInstallation(t, store, workspace, owner, nextGitHubID())

	ctx := postgres.WithTenant(context.Background(), postgres.TenantContext{
		UserID:      owner.ID,
		WorkspaceID: workspace.ID,
	})
	if err := store.Reconcile(ctx, installation.ID, workspace.ID, domain.SelectionSelected,
		[]domain.Repository{
			{GitHubID: 900040, Owner: "acme", Name: "kept", DefaultBranch: "main", Private: true},
		}, domain.RequiredPermissions); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if err := store.MarkDeleted(ctx, installation.ID, workspace.ID, application.AuditEvent{
		WorkspaceID: workspace.ID,
		Action:      application.AuditInstallationDeleted,
		Target:      "test",
	}); err != nil {
		t.Fatalf("MarkDeleted: %v", err)
	}

	// The repository row survives, withdrawn.
	repositories, err := store.ListRepositories(ctx, workspace.ID)
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(repositories) != 1 {
		t.Fatalf("removal left %d repository rows, want 1 kept for history", len(repositories))
	}
	if repositories[0].Granted {
		t.Error("a removed installation still reports its repository as granted")
	}

	// And it is no longer a connection.
	installations, err := store.List(ctx, workspace.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, candidate := range installations {
		if candidate.ID == installation.ID {
			t.Error("a removed installation is still listed as connected")
		}
	}
}
