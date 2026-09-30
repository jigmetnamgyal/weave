package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// TestEgressStoreAuditAndFencingIntegration verifies transaction rollback and
// one durable response/audit for a real committed claimant on the app role.
func TestEgressStoreAuditAndFencingIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "Egress Store")
	store := postgres.NewEgressStore(app)
	service, err := application.NewEgressService(store, []string{"weave.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	m := domain.Membership{WorkspaceID: f.workspace.ID, UserID: f.owner.ID, Role: domain.RoleOwner}
	keys := postgres.NewIdempotencyStore(app)
	record := domain.IdempotencyRecord{Scope: domain.IdempotencyScope{WorkspaceID: m.WorkspaceID, UserID: m.UserID, Endpoint: "addEgressHost"}, Key: "egress-add", Fingerprint: domain.FingerprintRequest("docs.example.com")}
	outcome, _, token, err := keys.Claim(f.ctx, record, application.IdempotencyLease)
	if err != nil || outcome != application.IdempotencyClaimed {
		t.Fatalf("claim: %v %v", outcome, err)
	}
	render := func(h domain.EgressHost) (int, []byte, error) { b, e := json.Marshal(h); return 201, b, e }
	completion := &application.EgressCompletion{Claim: application.IdempotentCompletion{Scope: record.Scope, Key: record.Key, Claimant: token, OriginRequestID: "egress-test"}, Render: render}
	entry, err := service.Add(f.ctx, m, "DOCS.example.com", completion)
	if err != nil {
		t.Fatal(err)
	}
	outcome, saved, _, err := keys.Claim(f.ctx, record, application.IdempotencyLease)
	if err != nil || outcome != application.IdempotencyComplete || saved.Response == nil || saved.Response.Status != 201 {
		t.Fatalf("completed replay: %v %+v %v", outcome, saved, err)
	}
	// An invalid claimant must roll back both configuration and audit.
	failed := *completion
	failed.Claim.Claimant = uuid.New()
	if _, err := service.Add(f.ctx, m, "other.example.com", &failed); !errors.Is(err, application.ErrIdempotencyFenced) {
		t.Fatalf("fence: %v", err)
	}
	// Rendering a stored response is also part of the transaction.
	failed.Render = func(domain.EgressHost) (int, []byte, error) { return 0, nil, errors.New("render failed") }
	if _, err := service.Add(f.ctx, m, "render.example.com", &failed); err == nil {
		t.Fatal("render failure committed")
	}
	entries, err := service.List(f.ctx, m)
	if err != nil || len(entries) != 1 || entries[0].ID != entry.ID {
		t.Fatalf("rollback: %+v %v", entries, err)
	}
	var count int
	if err := owner.QueryRow(context.Background(), "SELECT count(*) FROM audit_events WHERE workspace_id=$1 AND action='workspace.egress_host.added'", m.WorkspaceID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("audit count %d %v", count, err)
	}
	if err := service.Remove(f.ctx, m, entry.ID, nil); err != nil {
		t.Fatal(err)
	}
	var hostname string
	if err := owner.QueryRow(context.Background(), "SELECT detail->>'hostname' FROM audit_events WHERE workspace_id=$1 AND action='workspace.egress_host.removed'", m.WorkspaceID).Scan(&hostname); err != nil || hostname != "docs.example.com" {
		t.Fatalf("removed audit %q %v", hostname, err)
	}
	if err := service.Remove(f.ctx, m, entry.ID, nil); !errors.Is(err, application.ErrEgressHostNotFound) {
		t.Fatalf("missing remove: %v", err)
	}
}

// TestEgressAuthorizationAndAuditRollbackIntegration pins current-role rechecks,
// rejects system-policy mutation and makes a failed audit roll back the insert.
func TestEgressAuthorizationAndAuditRollbackIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "Egress Authority")
	store := postgres.NewEgressStore(app)
	service, err := application.NewEgressService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := domain.Membership{WorkspaceID: f.workspace.ID, UserID: f.owner.ID, Role: domain.RoleOwner}
	actor := application.Actor{UserID: m.UserID, Required: domain.PermissionWorkspaceManage}
	entry := domain.EgressHost{ID: uuid.New(), WorkspaceID: m.WorkspaceID, Hostname: "docs.example.com", CreatedBy: m.UserID}
	badAudit := application.AuditEvent{WorkspaceID: m.WorkspaceID, ActorUserID: m.UserID, Action: "workspace.egress_host.added", Detail: map[string]any{"not_json": make(chan int)}}
	if _, err := store.Add(f.ctx, entry, actor, badAudit, nil); err == nil {
		t.Fatal("bad audit committed")
	}
	rows, err := service.List(f.ctx, m)
	if err != nil || len(rows) != 0 {
		t.Fatalf("audit rollback: %v %v", rows, err)
	}
	if _, err := store.Add(f.ctx, entry, application.SystemActor(), badAudit, nil); !errors.Is(err, application.ErrPermissionDenied) {
		t.Fatalf("system-policy mutation: %v", err)
	}
	// Simulate demotion after the HTTP membership read, without changing m.
	if _, err := owner.Exec(context.Background(), "UPDATE workspace_members SET role='developer' WHERE workspace_id=$1 AND user_id=$2", m.WorkspaceID, m.UserID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Add(f.ctx, m, "docs.example.com", nil); !errors.Is(err, application.ErrPermissionDenied) {
		t.Fatalf("stale actor mutation: %v", err)
	}
	if err := service.Authorize(f.ctx, m); !errors.Is(err, application.ErrPermissionDenied) {
		t.Fatalf("stale actor replay: %v", err)
	}
	if _, err := store.Snapshot(postgres.WithTenantWorkspace(context.Background(), uuid.New()), uuid.New(), uuid.New()); !errors.Is(err, application.ErrEgressSnapshotMissing) {
		t.Fatalf("missing snapshot: %v", err)
	}
}

// TestTheEgressAuthorizerHonorsTheSnapshotIntegration drives the egress
// proxy's authorization through the real bounded lookup and tenant-scoped
// snapshot read, on the application role. Changes after a runner exists do not
// reach it: removal does not revoke, and an addition is not granted.
func TestTheEgressAuthorizerHonorsTheSnapshotIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "Egress Authorizer")
	session, err := f.createSession(t, postgres.NewSessionStore(owner), nil)
	if err != nil {
		t.Fatal(err)
	}
	store := postgres.NewEgressStore(app)
	service, err := application.NewEgressService(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	m := domain.Membership{WorkspaceID: f.workspace.ID, UserID: f.owner.ID, Role: domain.RoleOwner}
	docs, err := service.Add(f.ctx, m, "docs.example.com", nil)
	if err != nil {
		t.Fatal(err)
	}
	runners := postgres.NewRunnerStore(app)
	tenant := postgres.WithTenantWorkspace(context.Background(), f.workspace.ID)
	runner, err := runners.Create(tenant, domain.Runner{ID: uuid.New(), SessionID: session.ID, WorkspaceID: f.workspace.ID, Backend: "vercel-it"})
	if err != nil {
		t.Fatal(err)
	}
	// The production composition (services/egress-proxy uses the same call).
	authorizer := postgres.NewEgressAuthorizer(app, "vercel-")
	authorize := func(host string) error {
		_, err := authorizer.Authorize(context.Background(), runner.ID, host)
		return err
	}
	if err := authorize("docs.example.com"); err != nil {
		t.Fatalf("a snapshot host: %v", err)
	}
	if err := service.Remove(f.ctx, m, docs.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Add(f.ctx, m, "later.example.com", nil); err != nil {
		t.Fatal(err)
	}
	if err := authorize("docs.example.com"); err != nil {
		t.Errorf("removal revoked a running runner's snapshot host: %v", err)
	}
	if err := authorize("later.example.com"); !errors.Is(err, application.ErrEgressHostNotAuthorized) {
		t.Errorf("a host added after the runner = %v, want not authorized", err)
	}
	if err := authorize("api.docs.example.com"); !errors.Is(err, application.ErrEgressHostNotAuthorized) {
		t.Errorf("a subdomain = %v, want not authorized", err)
	}
	if _, err := runners.End(tenant, runner.ID, f.workspace.ID, domain.RunnerTerminated, ""); err != nil {
		t.Fatal(err)
	}
	if err := authorize("docs.example.com"); !errors.Is(err, application.ErrEgressRunnerNotLive) {
		t.Errorf("an ended runner = %v, want not live", err)
	}
}
