package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// TestOneLiveRunnerPerSessionIntegration: the index, not a check, is what
// makes a second live runner impossible — and an ended runner does not count.
func TestOneLiveRunnerPerSessionIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	fixture := seedSessionFixture(t, ownerPool, "One Runner Workspace")
	session, err := fixture.createSession(t, postgres.NewSessionStore(ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	store := postgres.NewRunnerStore(appPool)
	tenant := postgres.WithTenantWorkspace(context.Background(), fixture.workspace.ID)
	runner := func() domain.Runner {
		return domain.Runner{ID: uuid.New(), SessionID: session.ID, WorkspaceID: fixture.workspace.ID, Backend: "test"}
	}

	first, err := store.Create(tenant, runner())
	if err != nil {
		t.Fatalf("first runner: %v", err)
	}
	if _, err := store.Create(tenant, runner()); !errors.Is(err, application.ErrRunnerExists) {
		t.Fatalf("second live runner = %v, want ErrRunnerExists", err)
	}
	if _, err := store.End(tenant, first.ID, fixture.workspace.ID, domain.RunnerTerminated, ""); err != nil {
		t.Fatalf("end: %v", err)
	}
	if _, err := store.Create(tenant, runner()); err != nil {
		t.Errorf("a runner after the first ended = %v, want allowed", err)
	}
	// Ending twice finds nothing: a redelivered teardown is already done.
	if _, err := store.End(tenant, first.ID, fixture.workspace.ID, domain.RunnerTerminated, ""); !errors.Is(err, domain.ErrRunnerNotFound) {
		t.Errorf("ending an ended runner = %v, want ErrRunnerNotFound", err)
	}
}

// TestRunnersAreTenantScopedAndReconcileIsBoundedIntegration.
func TestRunnersAreTenantScopedAndReconcileIsBoundedIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	a := seedSessionFixture(t, ownerPool, "Runner Tenant A")
	b := seedSessionFixture(t, ownerPool, "Runner Tenant B")
	session, err := a.createSession(t, postgres.NewSessionStore(ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	store := postgres.NewRunnerStore(appPool)
	created, err := store.Create(postgres.WithTenantWorkspace(context.Background(), a.workspace.ID),
		domain.Runner{ID: uuid.New(), SessionID: session.ID, WorkspaceID: a.workspace.ID, Backend: "test"})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}

	if _, err := store.LiveForSession(postgres.WithTenantWorkspace(context.Background(), b.workspace.ID),
		session.ID, a.workspace.ID); !errors.Is(err, domain.ErrRunnerNotFound) {
		t.Errorf("workspace B reading A's runner = %v, want not found", err)
	}

	// The reconciler's cross-tenant read, as weave_app with no tenant.
	live, err := store.ListToReconcile(context.Background(), application.ReconcileBatch)
	if err != nil {
		t.Fatalf("list to reconcile: %v", err)
	}
	found := false
	for _, item := range live {
		if item.Runner.ID == created.ID {
			found = item.Runner.WorkspaceID == a.workspace.ID
		}
	}
	if !found {
		t.Error("the reconciler did not find a live runner, or not with its own workspace")
	}
	for _, bad := range []int{0, 501} {
		if _, err := store.ListToReconcile(context.Background(), bad); err == nil {
			t.Errorf("batch size %d was accepted", bad)
		}
	}
}

// TestOnlyALiveRunnerOfTheSessionMayWriteItsHistoryIntegration closes the
// gate M5.3 handed to M5.4a. Watched failing with the binding check removed:
// every one of these producers had its event stored.
func TestOnlyALiveRunnerOfTheSessionMayWriteItsHistoryIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	fixture := seedSessionFixture(t, ownerPool, "Runner Binding Workspace")
	sessions := postgres.NewSessionStore(ownerPool)
	session, err := fixture.createSession(t, sessions, nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	other, err := fixture.createSession(t, sessions, nil)
	if err != nil {
		t.Fatalf("create other session: %v", err)
	}
	events := postgres.NewEventStore(appPool)
	tenant := postgres.WithTenantWorkspace(context.Background(), fixture.workspace.ID)

	own := bindRunner(t, ownerPool, session)
	foreign := bindRunner(t, ownerPool, other)

	stored := eventFor(session, own)
	if _, ok, err := events.AppendEvent(tenant, stored); err != nil || !ok {
		t.Fatalf("the session's own runner = (%v, %v), want stored", ok, err)
	}
	for name, producer := range map[string]uuid.UUID{
		"not a runner at all":      uuid.New(),
		"another session's runner": foreign,
	} {
		if _, _, err := events.AppendEvent(tenant, eventFor(session, producer)); !errors.Is(err, application.ErrEventRunnerNotBound) {
			t.Errorf("%s: %v, want ErrEventRunnerNotBound", name, err)
		}
	}

	// Torn down: new events refused, a stored one redelivered is a duplicate.
	if _, err := ownerPool.Exec(context.Background(),
		`UPDATE runners SET state = 'terminated', terminated_at = now() WHERE id = $1`, own); err != nil {
		t.Fatal(err)
	}
	if _, _, err := events.AppendEvent(tenant, eventFor(session, own)); !errors.Is(err, application.ErrEventRunnerNotBound) {
		t.Errorf("a new event from a torn-down runner = %v, want ErrEventRunnerNotBound", err)
	}
	if _, ok, err := events.AppendEvent(tenant, stored); err != nil || ok {
		t.Errorf("a stored event redelivered after teardown = (%v, %v), want a duplicate", ok, err)
	}
}
