package workspaces

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/api/internal/auth"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

// TestACreatedBranchIsReturnedEvenWhenItsAuditFails is a transport-level test
// for a transport-level defect.
//
// The service already returned the branch alongside the error, and a service
// test asserted exactly that — and passed, while the API still answered with a
// bare 500 carrying neither the name nor the SHA of a ref that had just been
// written to a customer's repository. The caller was told nothing happened
// when something irreversible had, and could not identify what.
//
// The lesson is about where the test lived rather than what it checked:
// asserting a return value one layer below the behaviour proves the layer
// below, and nothing about what a caller actually receives.
func TestACreatedBranchIsReturnedEvenWhenItsAuditFails(t *testing.T) {
	rec := serveBranchRequest(t, errors.New("database went away"))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 — the branch was created", rec.Code)
	}

	var body branchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Name != "weave/task-abcde" {
		t.Errorf("name = %q, want the created branch so the caller can identify it", body.Name)
	}
	if body.SHA != "basesha" {
		t.Errorf("sha = %q, want the commit it points at", body.SHA)
	}
	if !body.Created {
		t.Error("created = false for a branch that was just created")
	}
}

// TestABranchIsReturnedNormallyWhenItsAuditSucceeds is the control: without it
// the test above would pass against a handler that ignored every error.
func TestABranchIsReturnedNormallyWhenItsAuditSucceeds(t *testing.T) {
	rec := serveBranchRequest(t, nil)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	var body branchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Name != "weave/task-abcde" {
		t.Errorf("name = %q", body.Name)
	}
}

// serveBranchRequest runs one create-branch request, with the audit write
// failing when auditErr is non-nil.
func serveBranchRequest(t *testing.T, auditErr error) *httptest.ResponseRecorder {
	t.Helper()

	// The workspace fake only recognises this member, so the membership
	// middleware ahead of the handler resolves rather than answering 404.
	owner := memberID
	installationID := uuid.New()
	repositoryID := uuid.New()

	installations := &branchFakeStore{
		auditErr: auditErr,
		installation: domain.Installation{
			ID: installationID, WorkspaceID: workspaceA, GitHubID: 42,
			AccountLogin: "acme", AccountType: domain.AccountOrganization,
			RepositorySelection: domain.SelectionAll,
		},
		repository: domain.Repository{
			ID: repositoryID, WorkspaceID: workspaceA, InstallationID: installationID,
			GitHubID: 900, Owner: "acme", Name: "app", DefaultBranch: "main", Granted: true,
		},
	}
	service := application.NewInstallationService(installations, nil, &branchFakeAPI{},
		func(ctx context.Context, _ uuid.UUID) context.Context { return ctx }, "weave-test")

	handler := NewHandler(application.NewWorkspaceService(&fakeStore{role: domain.RoleOwner}), discardLogger())
	mux := http.NewServeMux()
	handler.Register(mux)
	handler.RegisterGitHub(mux, service, "secret")

	path := "/v1/workspaces/" + workspaceA.String() + "/repositories/" + repositoryID.String() + "/branches"
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"name":"weave/task-abcde"}`))
	req = req.WithContext(auth.ContextWithUser(req.Context(), domain.User{ID: owner, Email: "dev@example.com"}))

	rec := httptest.NewRecorder()
	httpx.WithRequestID(mux).ServeHTTP(rec, req)
	return rec
}

// --- fakes -----------------------------------------------------------------

type branchFakeAPI struct{}

func (branchFakeAPI) Installation(context.Context, int64) (application.RemoteInstallation, error) {
	return application.RemoteInstallation{
		AccountLogin: "acme", AccountType: "Organization", RepositorySelection: "all",
		Permissions: map[string]string{"contents": "write", "metadata": "read"},
	}, nil
}

func (branchFakeAPI) InstallationRepositories(context.Context, int64) ([]application.RemoteRepository, error) {
	return []application.RemoteRepository{
		{GitHubID: 900, Owner: "acme", Name: "app", DefaultBranch: "main"},
	}, nil
}

func (branchFakeAPI) Branch(_ context.Context, _ int64, _, _, branch string) (application.RemoteBranch, error) {
	if branch == "main" {
		return application.RemoteBranch{Name: "main", SHA: "basesha"}, nil
	}
	return application.RemoteBranch{}, application.ErrRemoteNotFound
}

func (branchFakeAPI) CreateBranch(_ context.Context, _ int64, _, _, name, sha string) (application.RemoteBranch, error) {
	return application.RemoteBranch{Name: name, SHA: sha}, nil
}

func (branchFakeAPI) BranchRules(context.Context, int64, string, string, string) (application.BranchRule, error) {
	return application.BranchRule{}, nil
}

type branchFakeStore struct {
	installation domain.Installation
	repository   domain.Repository
	auditErr     error
}

func (f *branchFakeStore) Get(context.Context, uuid.UUID, uuid.UUID) (domain.Installation, error) {
	return f.installation, nil
}
func (f *branchFakeStore) GetRepository(context.Context, uuid.UUID, uuid.UUID) (domain.Repository, error) {
	return f.repository, nil
}
func (f *branchFakeStore) Reconcile(context.Context, uuid.UUID, uuid.UUID, domain.RepositorySelection, []domain.Repository, map[string]string) error {
	return nil
}
func (f *branchFakeStore) ListPermissions(context.Context, uuid.UUID, uuid.UUID) (map[string]string, error) {
	return map[string]string{"contents": "write"}, nil
}
func (f *branchFakeStore) AppendAudit(context.Context, application.AuditEvent) error {
	return f.auditErr
}
func (f *branchFakeStore) Connect(context.Context, domain.Installation, application.Actor, application.AuditEvent) (domain.Installation, error) {
	return domain.Installation{}, nil
}
func (f *branchFakeStore) Resolve(context.Context, int64) (application.InstallationRef, error) {
	return application.InstallationRef{}, application.ErrInstallationNotFound
}
func (f *branchFakeStore) List(context.Context, uuid.UUID) ([]domain.Installation, error) {
	return nil, nil
}
func (f *branchFakeStore) SetSuspended(context.Context, uuid.UUID, uuid.UUID, *time.Time, application.AuditEvent) error {
	return nil
}
func (f *branchFakeStore) MarkDeleted(context.Context, uuid.UUID, uuid.UUID, application.AuditEvent) error {
	return nil
}
func (f *branchFakeStore) ListRepositories(context.Context, uuid.UUID) ([]domain.Repository, error) {
	return nil, nil
}
func (f *branchFakeStore) ClaimDelivery(context.Context, string, string, string) (application.DeliveryClaim, error) {
	return application.DeliveryClaimed, nil
}
func (f *branchFakeStore) CompleteDelivery(context.Context, string) error { return nil }
func (f *branchFakeStore) PruneDeliveries(context.Context, time.Duration) (int64, error) {
	return 0, nil
}
