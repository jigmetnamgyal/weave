package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// registryWorld is a session with a live Vercel-style runner, and the stores
// the registry proxy uses, on the application role.
type registryWorld struct {
	fixture sessionFixture
	session domain.Session
	runner  domain.Runner
	store   *postgres.RegistryStore
}

func newRegistryWorld(t *testing.T, name string) registryWorld {
	t.Helper()
	ownerPool, appPool := newPool(t), newAppPool(t)
	fixture := seedSessionFixture(t, ownerPool, name)
	session, err := fixture.createSession(t, postgres.NewSessionStore(ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	runner, err := postgres.NewRunnerStore(appPool).Create(postgres.WithTenantWorkspace(context.Background(), fixture.workspace.ID),
		domain.Runner{ID: uuid.New(), SessionID: session.ID, WorkspaceID: fixture.workspace.ID, Backend: "vercel-it"})
	if err != nil {
		t.Fatalf("create runner: %v", err)
	}
	return registryWorld{fixture: fixture, session: session, runner: runner, store: postgres.NewRegistryStore(appPool)}
}

// TestTheRegistryProxysLookupIsNarrowIntegration: one runner id in, its
// session and workspace out, with no tenant context — and nothing for an
// unknown id, and an error for NULL, so it cannot enumerate.
func TestTheRegistryProxysLookupIsNarrowIntegration(t *testing.T) {
	w := newRegistryWorld(t, "Registry Lookup")
	got, err := w.store.RunnerForRegistryRequest(context.Background(), w.runner.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SessionID != w.session.ID || got.WorkspaceID != w.fixture.workspace.ID || got.Backend != "vercel-it" ||
		got.State != domain.RunnerProvisioning {
		t.Errorf("resolved %+v", got)
	}
	if _, err := w.store.RunnerForRegistryRequest(context.Background(), uuid.New()); !errors.Is(err, domain.ErrRunnerNotFound) {
		t.Errorf("unknown runner = %v, want ErrRunnerNotFound", err)
	}
	pool := newAppPool(t)
	if _, err := pool.Exec(context.Background(), `SELECT * FROM weave_runner_for_registry_request(NULL)`); err == nil {
		t.Error("a NULL runner id was accepted")
	}
}

// TestARegistryRequestIsRecordedInItsOwnTenantOnlyIntegration: the recorder,
// end to end on the application role — the row lands in the runner's
// workspace, is visible there and nowhere else, and cannot be written under
// another tenant's context.
func TestARegistryRequestIsRecordedInItsOwnTenantOnlyIntegration(t *testing.T) {
	a := newRegistryWorld(t, "Registry Tenant A")
	b := newRegistryWorld(t, "Registry Tenant B")
	recorder := application.NewRegistryRecorder(a.store, postgres.WithTenantWorkspace, "vercel-", time.Now)

	recorded, err := recorder.Record(context.Background(), a.runner.ID, "registry.npmjs.org", "GET",
		"/left-pad/-/left-pad-1.3.0.tgz?token=never-stored")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	own, err := a.store.ListRegistryRequests(postgres.WithTenantWorkspace(context.Background(), a.fixture.workspace.ID),
		a.session.ID, a.fixture.workspace.ID)
	if err != nil || len(own) != 1 || own[0].ID != recorded.ID || own[0].Path != "/left-pad/-/left-pad-1.3.0.tgz" {
		t.Fatalf("tenant A reads %+v, %v; want its one request, without the query", own, err)
	}
	if other, _ := a.store.ListRegistryRequests(postgres.WithTenantWorkspace(context.Background(), b.fixture.workspace.ID),
		a.session.ID, a.fixture.workspace.ID); len(other) != 0 {
		t.Errorf("tenant B read %d of A's registry requests", len(other))
	}
	if none, _ := a.store.ListRegistryRequests(context.Background(), a.session.ID, a.fixture.workspace.ID); len(none) != 0 {
		t.Errorf("no tenant read %d registry requests", len(none))
	}

	forged := recorded
	forged.ID = uuid.New()
	if err := a.store.RecordRegistryRequest(postgres.WithTenantWorkspace(context.Background(), b.fixture.workspace.ID), forged); err == nil {
		t.Error("a registry request for workspace A was written under workspace B's context")
	}
}

// TestTheRegistryRecordIsAppendOnlyAndBoundedIntegration: no update, delete or
// truncate, even by the owner; and the table itself refuses a query string or
// an unknown method, should anything get past the recorder.
func TestTheRegistryRecordIsAppendOnlyAndBoundedIntegration(t *testing.T) {
	w := newRegistryWorld(t, "Registry Append Only")
	recorder := application.NewRegistryRecorder(w.store, postgres.WithTenantWorkspace, "vercel-", time.Now)
	recorded, err := recorder.Record(context.Background(), w.runner.ID, "pypi.org", "GET", "/simple/requests/")
	if err != nil {
		t.Fatal(err)
	}
	owner := newPool(t)
	for name, statement := range map[string]string{
		"update":   `UPDATE registry_requests SET path = '/other' WHERE id = $1`,
		"delete":   `DELETE FROM registry_requests WHERE id = $1`,
		"truncate": `TRUNCATE registry_requests`,
	} {
		var err error
		if name == "truncate" {
			_, err = owner.Exec(context.Background(), statement)
		} else {
			_, err = owner.Exec(context.Background(), statement, recorded.ID)
		}
		if err == nil {
			t.Errorf("%s of a registry request was allowed", name)
		}
	}

	tenant := postgres.WithTenantWorkspace(context.Background(), w.fixture.workspace.ID)
	for name, request := range map[string]application.RegistryRequest{
		"a query in the path": {Host: "pypi.org", Method: "GET", Path: "/simple/?token=x"},
		"an unknown method":   {Host: "pypi.org", Method: "CONNECT", Path: "/simple/"},
		"a relative path":     {Host: "pypi.org", Method: "GET", Path: "simple/"},
		"an uppercase host":   {Host: "PyPI.org", Method: "GET", Path: "/simple/"},
	} {
		request.ID, request.WorkspaceID, request.SessionID, request.RunnerID = uuid.New(), w.fixture.workspace.ID, w.session.ID, w.runner.ID
		if err := w.store.RecordRegistryRequest(tenant, request); err == nil {
			t.Errorf("the table accepted %s", name)
		}
	}
}
