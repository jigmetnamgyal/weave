package postgres_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// TestEditingAProfileLeavesTheOldVersionUntouched is the reason agent_versions
// exists as a separate table.
//
// Invariants 9 and 10 make session history append-only and terminal history
// immutable, which cannot hold if the profile a session ran under can be
// edited afterwards. A session pinned to version 1 must keep reading the
// settings it actually ran under, byte for byte, after the profile moves on.
func TestEditingAProfileLeavesTheOldVersionUntouchedIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewAgentStore(pool)

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Agent Versions Workspace")
	ctx := postgres.WithTenant(context.Background(), postgres.TenantContext{
		UserID: owner.ID, WorkspaceID: workspace.ID,
	})
	actor := application.Actor{UserID: owner.ID, Required: domain.PermissionWorkspaceManage}

	agentID, _ := domain.NewAgentID()
	versionID, _ := domain.NewAgentVersionID()
	_, first, err := store.Create(ctx,
		domain.Agent{ID: agentID, WorkspaceID: workspace.ID, Name: "reviewer", CreatedBy: owner.ID},
		domain.AgentVersion{
			ID: versionID, AgentID: agentID, WorkspaceID: workspace.ID, Version: 1,
			Provider: domain.ProviderFake, Model: "deterministic-v1",
			Capabilities: []domain.Capability{domain.CapabilityPause},
			ToolPolicy:   []byte(`{"allow":["read"]}`), CreatedBy: owner.ID,
		}, actor, application.AuditEvent{
			WorkspaceID: workspace.ID, ActorUserID: owner.ID,
			Action: application.AuditAgentCreated, Target: agentID.String(),
		})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The profile is edited: a different model, different capabilities, a
	// different tool policy.
	nextID, _ := domain.NewAgentVersionID()
	second, err := store.AddVersion(ctx, domain.AgentVersion{
		ID: nextID, AgentID: agentID, WorkspaceID: workspace.ID,
		Provider: domain.ProviderFake, Model: "deterministic-v2",
		Capabilities: []domain.Capability{domain.CapabilityCancel, domain.CapabilityTokenAccounting},
		ToolPolicy:   []byte(`{"allow":["read","write"]}`), CreatedBy: owner.ID,
	}, actor, application.AuditEvent{
		WorkspaceID: workspace.ID, ActorUserID: owner.ID,
		Action: application.AuditAgentVersionCreated, Target: agentID.String(),
	})
	if err != nil {
		t.Fatalf("AddVersion: %v", err)
	}
	if second.Version != 2 {
		t.Errorf("second version numbered %d, want 2", second.Version)
	}

	// The first version is exactly as it was. This is the assertion the table
	// exists for: read it back and compare every field a session would care
	// about.
	reread, err := store.GetVersion(ctx, first.ID, workspace.ID)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if reread.Model != "deterministic-v1" {
		t.Errorf("model = %q, want the original", reread.Model)
	}
	if string(reread.ToolPolicy) != `{"allow": ["read"]}` && string(reread.ToolPolicy) != `{"allow":["read"]}` {
		t.Errorf("tool policy = %s, want the original", reread.ToolPolicy)
	}
	if len(reread.Capabilities) != 1 || reread.Capabilities[0] != domain.CapabilityPause {
		t.Errorf("capabilities = %v, want the original", reread.Capabilities)
	}

	// And the agent now points at the new one, which is the only thing that
	// changed.
	agent, err := store.Get(ctx, agentID, workspace.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if agent.CurrentVersionID == nil || *agent.CurrentVersionID != second.ID {
		t.Errorf("agent points at %v, want the new version", agent.CurrentVersionID)
	}
}

// TestAgentVersionsAreAppendOnly proves the database refuses what the
// application never attempts.
//
// The store has no update or delete path, so this is about the guarantee
// surviving a future one — and about TRUNCATE, which fires no row-level
// trigger and would otherwise erase the table in a single statement.
func TestAgentVersionsAreAppendOnlyIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewAgentStore(pool)
	ctx := context.Background()

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Append Only Workspace")
	tenantCtx := postgres.WithTenant(ctx, postgres.TenantContext{
		UserID: owner.ID, WorkspaceID: workspace.ID,
	})

	agentID, _ := domain.NewAgentID()
	versionID, _ := domain.NewAgentVersionID()
	if _, _, err := store.Create(tenantCtx,
		domain.Agent{ID: agentID, WorkspaceID: workspace.ID, Name: "frozen", CreatedBy: owner.ID},
		domain.AgentVersion{
			ID: versionID, AgentID: agentID, WorkspaceID: workspace.ID, Version: 1,
			Provider: domain.ProviderFake, Model: "m", CreatedBy: owner.ID,
		}, application.Actor{UserID: owner.ID, Required: domain.PermissionWorkspaceManage},
		application.AuditEvent{
			WorkspaceID: workspace.ID, ActorUserID: owner.ID,
			Action: application.AuditAgentCreated, Target: agentID.String(),
		}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// The owner pool bypasses row-level security, which is what makes this a
	// test of the triggers rather than of the policies.
	if _, err := pool.Exec(ctx,
		"UPDATE agent_versions SET model = 'rewritten' WHERE id = $1", versionID); err == nil {
		t.Error("an agent version was updated; a finished session's settings can be rewritten")
	}
	if _, err := pool.Exec(ctx, "DELETE FROM agent_versions WHERE id = $1", versionID); err == nil {
		t.Error("an agent version was deleted")
	}
	// TRUNCATE separately, because a row trigger does not fire for it.
	if _, err := pool.Exec(ctx, "TRUNCATE agent_versions"); err == nil {
		t.Error("agent_versions was truncated; the row trigger alone is not enough")
	}

	// The row survived all three.
	version, err := store.GetVersion(tenantCtx, versionID, workspace.ID)
	if err != nil {
		t.Fatalf("GetVersion after the refused mutations: %v", err)
	}
	if version.Model != "m" {
		t.Errorf("model = %q, want it unchanged", version.Model)
	}
}

// TestTaskAndAgentAreInvisibleAcrossTenants covers the visibility half of
// authorization: another workspace's records are absent, not forbidden.
func TestTaskAndAgentAreInvisibleAcrossTenantsIntegration(t *testing.T) {
	pool := newPool(t)
	tasks := postgres.NewTaskStore(pool)
	agents := postgres.NewAgentStore(pool)

	a, b, ownerA, ownerB := twoTenants(t, pool)
	ctxA := postgres.WithTenant(context.Background(), postgres.TenantContext{UserID: ownerA.ID, WorkspaceID: a.ID})
	ctxB := postgres.WithTenant(context.Background(), postgres.TenantContext{UserID: ownerB.ID, WorkspaceID: b.ID})

	taskID, _ := domain.NewTaskID()
	if _, err := tasks.Create(ctxA, domain.Task{
		ID: taskID, WorkspaceID: a.ID, Title: "tenant A work",
		Body: "private", Status: domain.TaskDraft, CreatedBy: ownerA.ID,
	}, application.Actor{UserID: ownerA.ID, Required: domain.PermissionSessionCreate},
		application.AuditEvent{WorkspaceID: a.ID, ActorUserID: ownerA.ID, Action: application.AuditTaskCreated, Target: taskID.String()}); err != nil {
		t.Fatalf("Create task: %v", err)
	}

	agentID, _ := domain.NewAgentID()
	versionID, _ := domain.NewAgentVersionID()
	if _, _, err := agents.Create(ctxA,
		domain.Agent{ID: agentID, WorkspaceID: a.ID, Name: "tenant-a", CreatedBy: ownerA.ID},
		domain.AgentVersion{
			ID: versionID, AgentID: agentID, WorkspaceID: a.ID, Version: 1,
			Provider: domain.ProviderFake, Model: "m", CreatedBy: ownerA.ID,
		}, application.Actor{UserID: ownerA.ID, Required: domain.PermissionWorkspaceManage},
		application.AuditEvent{WorkspaceID: a.ID, ActorUserID: ownerA.ID, Action: application.AuditAgentCreated, Target: agentID.String()}); err != nil {
		t.Fatalf("Create agent: %v", err)
	}

	// Tenant B naming tenant A's ids explicitly.
	if _, err := tasks.Get(ctxB, taskID, b.ID); !errors.Is(err, application.ErrTaskNotFound) {
		t.Errorf("cross-tenant task read returned %v, want ErrTaskNotFound", err)
	}
	if _, err := agents.Get(ctxB, agentID, b.ID); !errors.Is(err, application.ErrAgentNotFound) {
		t.Errorf("cross-tenant agent read returned %v, want ErrAgentNotFound", err)
	}

	listed, err := tasks.List(ctxB, b.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("tenant B sees %d tasks, want 0", len(listed))
	}
}

// TestTaskAndAgentTablesAreClosedWithoutContext extends the row-level security
// checks to the tables this unit adds.
//
// Connecting as the application role is the only way this proves anything: the
// owner is a superuser locally and bypasses every policy, so the same
// assertions against `newPool` would pass with no policies at all.
func TestTaskAndAgentTablesAreClosedWithoutContextIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	ctx := context.Background()

	owner := seedUser(t, ownerPool)
	workspace := seedWorkspace(t, ownerPool, owner, "RLS Task Workspace")
	tenantCtx := postgres.WithTenant(ctx, postgres.TenantContext{UserID: owner.ID, WorkspaceID: workspace.ID})

	taskID, _ := domain.NewTaskID()
	if _, err := postgres.NewTaskStore(ownerPool).Create(tenantCtx, domain.Task{
		ID: taskID, WorkspaceID: workspace.ID, Title: "hidden", Body: "", Status: domain.TaskDraft, CreatedBy: owner.ID,
	}, application.Actor{UserID: owner.ID, Required: domain.PermissionSessionCreate},
		application.AuditEvent{WorkspaceID: workspace.ID, ActorUserID: owner.ID, Action: application.AuditTaskCreated, Target: taskID.String()}); err != nil {
		t.Fatalf("Create task: %v", err)
	}

	agentID, _ := domain.NewAgentID()
	versionID, _ := domain.NewAgentVersionID()
	if _, _, err := postgres.NewAgentStore(ownerPool).Create(tenantCtx,
		domain.Agent{ID: agentID, WorkspaceID: workspace.ID, Name: "hidden", CreatedBy: owner.ID},
		domain.AgentVersion{
			ID: versionID, AgentID: agentID, WorkspaceID: workspace.ID, Version: 1,
			Provider: domain.ProviderFake, Model: "m", CreatedBy: owner.ID,
		}, application.Actor{UserID: owner.ID, Required: domain.PermissionWorkspaceManage},
		application.AuditEvent{WorkspaceID: workspace.ID, ActorUserID: owner.ID, Action: application.AuditAgentCreated, Target: agentID.String()}); err != nil {
		t.Fatalf("Create agent: %v", err)
	}

	// No tenant context at all: every policy matches nothing.
	for _, table := range []string{"tasks", "agents", "agent_versions"} {
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
	otherWorkspace := seedWorkspace(t, ownerPool, other, "Other RLS Task Workspace")

	tx, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	setTenant(t, ctx, tx, other.ID.String(), otherWorkspace.ID.String())

	for _, table := range []string{"tasks", "agents", "agent_versions"} {
		var count int
		if err := tx.QueryRow(ctx,
			"SELECT count(*) FROM "+table+" WHERE workspace_id = $1", workspace.ID).Scan(&count); err != nil {
			t.Fatalf("cross-tenant select %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s returned %d rows for another tenant's workspace, want 0", table, count)
		}
	}
}

// TestAReadyTaskDoesNotMakeAWorkspaceUndeletableIntegration pins the two
// halves of the repository foreign key's behaviour.
//
// `ON DELETE SET NULL (repository_id)` was the first choice and was wrong: a
// ready task's new NULL violates tasks_ready_has_repository, so the deletion
// aborts anyway — the same refusal NO ACTION gives, reached by accident and
// reported as a check-constraint violation on a row the caller never touched.
// Both behaviours are asserted here because the pair is the point: deleting a
// tenant must work, and deleting a repository out from under a live task must
// not.
func TestAReadyTaskDoesNotMakeAWorkspaceUndeletableIntegration(t *testing.T) {
	pool := newPool(t)
	installations := postgres.NewInstallationStore(pool)
	tasks := postgres.NewTaskStore(pool)

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Ready Task Workspace")
	installation := connectInstallation(t, installations, workspace, owner, nextGitHubID())
	ctx := postgres.WithTenant(context.Background(), postgres.TenantContext{
		UserID: owner.ID, WorkspaceID: workspace.ID,
	})

	if err := installations.Reconcile(ctx, installation.ID, workspace.ID,
		domain.SelectionSelected, []domain.Repository{
			{GitHubID: nextGitHubID(), Owner: "acme", Name: "ready", DefaultBranch: "main"},
		}, domain.RequiredPermissions); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	repositories, err := installations.ListRepositories(ctx, workspace.ID)
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(repositories) != 1 {
		t.Fatalf("got %d repositories, want 1", len(repositories))
	}
	repository := repositories[0]

	taskID, _ := domain.NewTaskID()
	if _, err := tasks.Create(ctx, domain.Task{
		ID: taskID, WorkspaceID: workspace.ID, RepositoryID: &repository.ID,
		Title: "ready to run", Status: domain.TaskReady, CreatedBy: owner.ID,
	}, application.Actor{UserID: owner.ID, Required: domain.PermissionSessionCreate},
		application.AuditEvent{
			WorkspaceID: workspace.ID, ActorUserID: owner.ID,
			Action: application.AuditTaskCreated, Target: taskID.String(),
		}); err != nil {
		t.Fatalf("Create task: %v", err)
	}

	// Deleting the repository under a live task is refused, and the refusal
	// names the task's foreign key rather than a constraint on a row the
	// caller was not touching.
	_, err = pool.Exec(context.Background(),
		"DELETE FROM repositories WHERE id = $1", repository.ID)
	if err == nil {
		t.Fatal("deleting a repository a ready task references should be refused")
	}
	if !strings.Contains(err.Error(), "tasks_repository_fkey") {
		t.Errorf("refusal does not name the foreign key, so the cause is unclear: %v", err)
	}

	// Deleting the workspace still works, which is what the cleanup in every
	// other test depends on.
	if _, err := pool.Exec(context.Background(),
		"DELETE FROM workspaces WHERE id = $1", workspace.ID); err != nil {
		t.Fatalf("a ready task made its workspace undeletable: %v", err)
	}
}

// TestAnAgentCannotPointAtAnotherAgentsVersionIntegration pins the constraint
// behind the current-version pointer.
//
// The first version keyed it on the workspace, which stops an agent pointing
// at another tenant's version but allows it to point at a sibling's — after
// which the profile reports a model and a tool policy belonging to a different
// agent, and a session pinned through it records settings that were never
// chosen for it. Asserted with SQL because no store method offers the mistake;
// the constraint is what has to refuse it.
func TestAnAgentCannotPointAtAnotherAgentsVersionIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewAgentStore(pool)

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Sibling Agents Workspace")
	ctx := postgres.WithTenant(context.Background(), postgres.TenantContext{
		UserID: owner.ID, WorkspaceID: workspace.ID,
	})
	actor := application.Actor{UserID: owner.ID, Required: domain.PermissionWorkspaceManage}

	create := func(name string) (domain.Agent, domain.AgentVersion) {
		t.Helper()
		agentID, _ := domain.NewAgentID()
		versionID, _ := domain.NewAgentVersionID()
		agent, version, err := store.Create(ctx,
			domain.Agent{ID: agentID, WorkspaceID: workspace.ID, Name: name, CreatedBy: owner.ID},
			domain.AgentVersion{
				ID: versionID, AgentID: agentID, WorkspaceID: workspace.ID, Version: 1,
				Provider: domain.ProviderFake, Model: "deterministic-" + name,
				ToolPolicy: []byte(`{}`), CreatedBy: owner.ID,
			}, actor, application.AuditEvent{
				WorkspaceID: workspace.ID, ActorUserID: owner.ID,
				Action: application.AuditAgentCreated, Target: agentID.String(),
			})
		if err != nil {
			t.Fatalf("Create %s: %v", name, err)
		}
		return agent, version
	}

	alpha, _ := create("alpha")
	_, betaVersion := create("beta")

	_, err := pool.Exec(context.Background(),
		"UPDATE agents SET current_version_id = $1 WHERE id = $2",
		betaVersion.ID, alpha.ID)
	if err == nil {
		t.Fatal("an agent was allowed to point at a sibling agent's version")
	}
	if !strings.Contains(err.Error(), "agents_current_version_fkey") {
		t.Errorf("refusal does not name the pointer's constraint: %v", err)
	}
}

// TestConcurrentPatchesDoNotLoseEachOtherIntegration is why the merge runs
// inside the store's transaction rather than above it.
//
// A PATCH is a read-modify-write. With the read in one transaction and the
// write in another, two edits to different fields both read the same row and
// the second writes its own field alongside stale copies of everything else —
// so one person's edit silently disappears while both requests return 200.
//
// What this pins is the transaction boundary, not the row lock: authorizeActor
// already takes LockWorkspace on every mutation, so removing FOR UPDATE from
// the task read changes nothing. Reinstating the old split-transaction shape
// fails this test on every run, which is how it was checked — a concurrency
// test that has never been seen to fail is not evidence of anything.
//
// Both goroutines are released together and each patches a different field,
// so a passing run means both survived rather than that they did not overlap.
func TestConcurrentPatchesDoNotLoseEachOtherIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewTaskStore(pool)

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Concurrent Patch Workspace")
	ctx := postgres.WithTenant(context.Background(), postgres.TenantContext{
		UserID: owner.ID, WorkspaceID: workspace.ID,
	})
	actor := application.Actor{UserID: owner.ID, Required: domain.PermissionSessionCreate}
	event := func(domain.Task) application.AuditEvent {
		return application.AuditEvent{
			WorkspaceID: workspace.ID, ActorUserID: owner.ID,
			Action: application.AuditTaskUpdated, Target: "concurrent",
		}
	}

	taskID, _ := domain.NewTaskID()
	if _, err := store.Create(ctx, domain.Task{
		ID: taskID, WorkspaceID: workspace.ID,
		Title: "original title", Body: "original body",
		Status: domain.TaskDraft, CreatedBy: owner.ID,
	}, actor, application.AuditEvent{
		WorkspaceID: workspace.ID, ActorUserID: owner.ID,
		Action: application.AuditTaskCreated, Target: taskID.String(),
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Each patch holds its read for a moment before writing, so the two
	// overlap the way two requests would.
	patch := func(mutate func(*domain.Task)) error {
		_, err := store.Update(ctx, taskID, workspace.ID,
			func(existing domain.Task) (domain.Task, error) {
				time.Sleep(50 * time.Millisecond)
				mutate(&existing)
				return existing, nil
			}, actor, event)
		return err
	}

	// Warm the pool before starting. pgxpool creates connections lazily, and
	// the second connection's handshake alone took long enough to stagger the
	// two patches — the second read landing after the first had committed, so
	// the test passed without the two ever overlapping. Measured while
	// checking this test against the unlocked code, which it wrongly passed.
	warm := make([]*pgxpool.Conn, 0, 2)
	for range 2 {
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("warm pool: %v", err)
		}
		warm = append(warm, conn)
	}
	for _, conn := range warm {
		conn.Release()
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	wait.Add(2)

	go func() {
		defer wait.Done()
		<-start
		errs <- patch(func(task *domain.Task) { task.Title = "patched title" })
	}()
	go func() {
		defer wait.Done()
		<-start
		errs <- patch(func(task *domain.Task) { task.Body = "patched body" })
	}()

	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent patch: %v", err)
		}
	}

	final, err := store.Get(ctx, taskID, workspace.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.Title != "patched title" {
		t.Errorf("title = %q — the title patch was overwritten by the other request", final.Title)
	}
	if final.Body != "patched body" {
		t.Errorf("body = %q — the body patch was overwritten by the other request", final.Body)
	}
}
