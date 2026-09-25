package application_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// branchWorld is the smallest arrangement that exercises CreateBranch: one
// workspace, one installation, one repository, and a GitHub whose answers the
// test controls.
type branchWorld struct {
	store       *fakeInstallations
	service     *application.InstallationService
	membership  domain.Membership
	repository  domain.Repository
	api         *fakeGitHub
	audits      *[]application.AuditEvent
	permissions map[string]string
}

func newBranchWorld(t *testing.T) *branchWorld {
	t.Helper()

	workspaceID := uuid.New()
	installationID := uuid.New()
	repositoryID := uuid.New()
	permissions := map[string]string{"contents": "write", "metadata": "read", "pull_requests": "write"}

	repository := domain.Repository{
		ID: repositoryID, WorkspaceID: workspaceID, InstallationID: installationID,
		GitHubID: 900, Owner: "acme", Name: "app", DefaultBranch: "main", Granted: true,
	}
	installation := domain.Installation{
		ID: installationID, WorkspaceID: workspaceID, GitHubID: 42,
		AccountLogin: "acme", AccountType: domain.AccountOrganization,
		RepositorySelection: domain.SelectionAll,
	}

	api := &fakeGitHub{
		permissions:     permissions,
		restrictedNames: map[string]string{},
		branches: map[string]application.RemoteBranch{
			"main": {Name: "main", SHA: "basesha", Protected: true},
		},
	}
	audits := &[]application.AuditEvent{}
	store := &fakeInstallations{
		installation: installation,
		repository:   repository,
		permissions:  permissions,
		audits:       audits,
	}

	service := application.NewInstallationService(store, nil, api,
		func(ctx context.Context, _ uuid.UUID) context.Context { return ctx }, "weave-test")

	return &branchWorld{
		store:       store,
		service:     service,
		membership:  domain.Membership{WorkspaceID: workspaceID, UserID: uuid.New(), Role: domain.RoleOwner},
		repository:  repository,
		api:         api,
		audits:      audits,
		permissions: permissions,
	}
}

// TestCreateBranchIsIdempotentByName is invariant 5 for this operation.
//
// A retried request must find its own earlier work and report success. GitHub
// refuses the duplicate ref; surfacing that as a failure would make a request
// that did exactly the right thing look broken.
func TestCreateBranchIsIdempotentByName(t *testing.T) {
	world := newBranchWorld(t)
	ctx := context.Background()

	first, err := world.service.CreateBranch(ctx, world.membership, application.CreateBranchCommand{
		RepositoryID: world.repository.ID, Name: "weave/task-abcde",
	})
	if err != nil {
		t.Fatalf("first CreateBranch: %v", err)
	}
	if !first.Created {
		t.Error("the first creation reported Created = false")
	}

	second, err := world.service.CreateBranch(ctx, world.membership, application.CreateBranchCommand{
		RepositoryID: world.repository.ID, Name: "weave/task-abcde",
	})
	if err != nil {
		t.Fatalf("repeating the request failed: %v", err)
	}
	if second.Created {
		t.Error("the repeat reported Created = true; nothing was made the second time")
	}
	if second.SHA != first.SHA || second.Name != first.Name {
		t.Errorf("repeat returned %+v, want the same branch as %+v", second, first)
	}

	// One effect, one audit row. A trail recording attempts rather than
	// effects stops being a record of what happened.
	if len(*world.audits) != 1 {
		t.Errorf("wrote %d audit rows for one branch, want 1", len(*world.audits))
	}
}

// TestCreateBranchRefusesADifferentBase covers the third outcome.
//
// Handing back a branch pointing at unknown work, as though it were the one
// asked for, is worse than an error: the caller would build on a commit it
// never chose.
func TestCreateBranchRefusesADifferentBase(t *testing.T) {
	world := newBranchWorld(t)
	world.api.branches["weave/taken"] = application.RemoteBranch{Name: "weave/taken", SHA: "somethingelse"}

	_, err := world.service.CreateBranch(context.Background(), world.membership,
		application.CreateBranchCommand{RepositoryID: world.repository.ID, Name: "weave/taken"})
	if !errors.Is(err, domain.ErrBranchConflict) {
		t.Errorf("CreateBranch = %v, want ErrBranchConflict", err)
	}
	if len(*world.audits) != 0 {
		t.Error("a refused creation wrote an audit row")
	}
}

// TestCreateBranchRefusesAProtectedTarget is invariant 11 becoming real.
func TestCreateBranchRefusesAProtectedTarget(t *testing.T) {
	world := newBranchWorld(t)
	world.api.branches["weave/guarded"] = application.RemoteBranch{
		Name: "weave/guarded", SHA: "basesha", Protected: true,
	}

	_, err := world.service.CreateBranch(context.Background(), world.membership,
		application.CreateBranchCommand{RepositoryID: world.repository.ID, Name: "weave/guarded"})
	if !errors.Is(err, domain.ErrBranchProtected) {
		t.Errorf("CreateBranch = %v, want ErrBranchProtected", err)
	}
}

// TestCreateBranchFromAProtectedBaseIsNormal is the control for the test
// above. Refusing to branch *from* main would make the feature useless.
func TestCreateBranchFromAProtectedBaseIsNormal(t *testing.T) {
	world := newBranchWorld(t)
	if world.api.branches["main"].Protected != true {
		t.Fatal("the fixture's base is not protected, so this proves nothing")
	}

	branch, err := world.service.CreateBranch(context.Background(), world.membership,
		application.CreateBranchCommand{RepositoryID: world.repository.ID, Name: "weave/from-main"})
	if err != nil {
		t.Fatalf("creating from a protected base failed: %v", err)
	}
	if branch.Base != "main" || branch.SHA != "basesha" {
		t.Errorf("branch = %+v, want it rooted at the protected base", branch)
	}
}

// TestCreateBranchRefusesAMissingBase.
func TestCreateBranchRefusesAMissingBase(t *testing.T) {
	world := newBranchWorld(t)
	_, err := world.service.CreateBranch(context.Background(), world.membership,
		application.CreateBranchCommand{
			RepositoryID: world.repository.ID, Name: "weave/x", Base: "no-such-branch",
		})
	if !errors.Is(err, domain.ErrBaseNotFound) {
		t.Errorf("CreateBranch = %v, want ErrBaseNotFound", err)
	}
}

// TestCreateBranchNamesTheMissingPermission.
//
// An App's permission set can be widened after an installation accepted the
// old one, and the installation keeps what it accepted until a human approves
// the change — so this is not transient and no retry fixes it. Relaying
// GitHub's 403 would say none of that.
func TestCreateBranchNamesTheMissingPermission(t *testing.T) {
	world := newBranchWorld(t)
	world.api.permissions = map[string]string{"contents": "read", "metadata": "read"}

	_, err := world.service.CreateBranch(context.Background(), world.membership,
		application.CreateBranchCommand{RepositoryID: world.repository.ID, Name: "weave/x"})
	if !errors.Is(err, application.ErrPermissionDenied) {
		t.Fatalf("CreateBranch = %v, want ErrPermissionDenied", err)
	}
	if !contains(err.Error(), "contents:write") {
		t.Errorf("error %q does not name the missing permission", err)
	}
}

// TestCreateBranchRefusesAWithdrawnRepository proves reach is checked against
// GitHub rather than our records.
//
// The stored row still says granted; GitHub no longer lists the repository.
// Acting on the stored belief is how a revoked grant reaches a write.
func TestCreateBranchRefusesAWithdrawnRepository(t *testing.T) {
	world := newBranchWorld(t)
	world.api.withdrawn = true

	_, err := world.service.CreateBranch(context.Background(), world.membership,
		application.CreateBranchCommand{RepositoryID: world.repository.ID, Name: "weave/x"})
	if !errors.Is(err, domain.ErrRepositoryNotGranted) {
		t.Errorf("CreateBranch = %v, want ErrRepositoryNotGranted", err)
	}
	if world.api.created > 0 {
		t.Error("a withdrawn repository was written to")
	}
}

// TestCreateBranchRequiresRepositoryManage.
func TestCreateBranchRequiresRepositoryManage(t *testing.T) {
	world := newBranchWorld(t)
	world.membership.Role = domain.RoleViewer

	_, err := world.service.CreateBranch(context.Background(), world.membership,
		application.CreateBranchCommand{RepositoryID: world.repository.ID, Name: "weave/x"})
	if !errors.Is(err, application.ErrPermissionDenied) {
		t.Errorf("CreateBranch as a viewer = %v, want ErrPermissionDenied", err)
	}
	if world.api.created > 0 {
		t.Error("a viewer's request reached GitHub")
	}
}

// TestCreateBranchRejectsBadNamesBeforeReachingGitHub.
func TestCreateBranchRejectsBadNamesBeforeReachingGitHub(t *testing.T) {
	for _, name := range []string{"weave/../escape", "refs/heads/weave/x", "main", "weave/x\x00y"} {
		world := newBranchWorld(t)
		_, err := world.service.CreateBranch(context.Background(), world.membership,
			application.CreateBranchCommand{RepositoryID: world.repository.ID, Name: name})
		if !errors.Is(err, domain.ErrInvalidBranchName) {
			t.Errorf("CreateBranch(%q) = %v, want ErrInvalidBranchName", name, err)
		}
		if world.api.calls > 0 {
			t.Errorf("CreateBranch(%q) reached GitHub before validating", name)
		}
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}

// --- fakes -----------------------------------------------------------------

type fakeGitHub struct {
	permissions map[string]string
	branches    map[string]application.RemoteBranch
	// restrictedNames are names a ruleset governs, whether or not a branch of
	// that name exists.
	restrictedNames map[string]string
	rulesErr        error
	createErr       error
	withdrawn       bool
	created         int
	calls           int
}

func (f *fakeGitHub) BranchRules(_ context.Context, _ int64, _, _, branch string) (application.BranchRule, error) {
	f.calls++
	if f.rulesErr != nil {
		return application.BranchRule{}, f.rulesErr
	}
	if rule, ok := f.restrictedNames[branch]; ok {
		return application.BranchRule{Restricted: true, Rule: rule}, nil
	}
	return application.BranchRule{}, nil
}

func (f *fakeGitHub) Installation(context.Context, int64) (application.RemoteInstallation, error) {
	f.calls++
	return application.RemoteInstallation{
		AccountLogin: "acme", AccountType: "Organization",
		RepositorySelection: "all", Permissions: f.permissions,
	}, nil
}

func (f *fakeGitHub) InstallationRepositories(context.Context, int64) ([]application.RemoteRepository, error) {
	f.calls++
	if f.withdrawn {
		return nil, nil
	}
	return []application.RemoteRepository{
		{GitHubID: 900, Owner: "acme", Name: "app", DefaultBranch: "main"},
	}, nil
}

func (f *fakeGitHub) Branch(_ context.Context, _ int64, _, _, branch string) (application.RemoteBranch, error) {
	f.calls++
	if found, ok := f.branches[branch]; ok {
		return found, nil
	}
	return application.RemoteBranch{}, application.ErrRemoteNotFound
}

func (f *fakeGitHub) CreateBranch(_ context.Context, _ int64, _, _, name, sha string) (application.RemoteBranch, error) {
	f.calls++
	if f.createErr != nil {
		return application.RemoteBranch{}, f.createErr
	}
	f.created++
	if _, exists := f.branches[name]; exists {
		return application.RemoteBranch{}, application.ErrRemoteRefExists
	}
	created := application.RemoteBranch{Name: name, SHA: sha}
	f.branches[name] = created
	return created, nil
}

type fakeInstallations struct {
	installation domain.Installation
	repository   domain.Repository
	permissions  map[string]string
	audits       *[]application.AuditEvent
	auditErr     error
	withdrawn    bool
}

func (f *fakeInstallations) Get(_ context.Context, _, _ uuid.UUID) (domain.Installation, error) {
	return f.installation, nil
}

func (f *fakeInstallations) GetRepository(_ context.Context, _, _ uuid.UUID) (domain.Repository, error) {
	repository := f.repository
	repository.Granted = !f.withdrawn
	return repository, nil
}

func (f *fakeInstallations) Reconcile(_ context.Context, _, _ uuid.UUID, _ domain.RepositorySelection,
	repositories []domain.Repository, _ map[string]string) error {
	// Mirrors the real store: a repository GitHub no longer lists is withdrawn.
	f.withdrawn = len(repositories) == 0
	return nil
}

func (f *fakeInstallations) ListPermissions(context.Context, uuid.UUID, uuid.UUID) (map[string]string, error) {
	return f.permissions, nil
}

func (f *fakeInstallations) AppendAudit(_ context.Context, event application.AuditEvent) error {
	if f.auditErr != nil {
		return f.auditErr
	}
	*f.audits = append(*f.audits, event)
	return nil
}

func (f *fakeInstallations) Connect(context.Context, domain.Installation, application.Actor, application.AuditEvent) (domain.Installation, error) {
	return domain.Installation{}, nil
}
func (f *fakeInstallations) Resolve(context.Context, int64) (application.InstallationRef, error) {
	return application.InstallationRef{}, application.ErrInstallationNotFound
}
func (f *fakeInstallations) List(context.Context, uuid.UUID) ([]domain.Installation, error) {
	return nil, nil
}
func (f *fakeInstallations) SetSuspended(context.Context, uuid.UUID, uuid.UUID, *time.Time, application.AuditEvent) error {
	return nil
}
func (f *fakeInstallations) MarkDeleted(context.Context, uuid.UUID, uuid.UUID, application.AuditEvent) error {
	return nil
}
func (f *fakeInstallations) ListRepositories(context.Context, uuid.UUID) ([]domain.Repository, error) {
	return nil, nil
}
func (f *fakeInstallations) ClaimDelivery(context.Context, string, string, string) (application.DeliveryClaim, error) {
	return application.DeliveryClaimed, nil
}
func (f *fakeInstallations) CompleteDelivery(context.Context, string) error { return nil }
func (f *fakeInstallations) PruneDeliveries(context.Context, time.Duration) (int64, error) {
	return 0, nil
}

// TestCreateBranchRefusesANameARulesetGoverns closes the hole the existing
// `protected` check could not see.
//
// A branch that does not exist has no `protected` flag to read, and rulesets
// match patterns — one covering `weave/*` applies to every branch this unit
// creates. Checking only existing branches would satisfy invariant 11
// everywhere it was tested and violate it everywhere it mattered.
func TestCreateBranchRefusesANameARulesetGoverns(t *testing.T) {
	world := newBranchWorld(t)
	world.api.restrictedNames["weave/governed"] = "creation"

	_, err := world.service.CreateBranch(context.Background(), world.membership,
		application.CreateBranchCommand{RepositoryID: world.repository.ID, Name: "weave/governed"})
	if !errors.Is(err, domain.ErrBranchProtected) {
		t.Errorf("CreateBranch = %v, want ErrBranchProtected", err)
	}
	if world.api.created > 0 {
		t.Error("a name governed by a ruleset was created anyway")
	}
	if !contains(err.Error(), "creation") {
		t.Errorf("error %q does not name the rule that refused it", err)
	}
}

// TestCreateBranchFailsClosedWhenRulesCannotBeRead.
//
// A protection check that proceeds when it cannot see the answer looks like
// protection and is not — the same mistake as row-level security matching
// every row when tenant context is missing. The cost is that a GitHub outage
// blocks creation, which is the right way round for a control whose whole
// purpose is stopping writes.
func TestCreateBranchFailsClosedWhenRulesCannotBeRead(t *testing.T) {
	world := newBranchWorld(t)
	world.api.rulesErr = errors.New("github is unreachable")

	_, err := world.service.CreateBranch(context.Background(), world.membership,
		application.CreateBranchCommand{RepositoryID: world.repository.ID, Name: "weave/x"})
	if err == nil {
		t.Fatal("creation proceeded while the protection check was unanswerable")
	}
	if world.api.created > 0 {
		t.Error("a branch was created without knowing whether a rule governs it")
	}
}

// TestAnUnrecordableCreationIsReportedNotRepaired.
//
// Creation and its audit are two writes to two systems, and GitHub goes first
// because it cannot be rolled back. When the audit then fails, both facts
// reach the caller: the branch, because something irreversible happened, and
// an error naming it, because an unaudited write must not look like a clean
// success.
//
// What it deliberately does *not* do is record the audit on a later retry.
// Nothing distinguishes our own interrupted attempt from a branch someone
// created by hand at the same commit, so a repair would assert — permanently,
// in an append-only trail — that a member created something they did not. A
// missing row is a gap; a false row is a false statement about a person.
func TestAnUnrecordableCreationIsReportedNotRepaired(t *testing.T) {
	world := newBranchWorld(t)
	ctx := context.Background()
	command := application.CreateBranchCommand{RepositoryID: world.repository.ID, Name: "weave/orphan"}

	world.store.auditErr = errors.New("database went away")
	branch, err := world.service.CreateBranch(ctx, world.membership, command)
	if err == nil {
		t.Fatal("an unrecorded creation was reported as a clean success")
	}
	// The branch comes back regardless: the caller must know GitHub changed.
	if branch.Name != "weave/orphan" {
		t.Errorf("branch = %+v, want the created branch returned alongside the error", branch)
	}
	if !contains(err.Error(), "audit") {
		t.Errorf("error %q does not say the creation went unrecorded", err)
	}

	// A retry finds the branch and does not invent attribution for it.
	world.store.auditErr = nil
	retried, err := world.service.CreateBranch(ctx, world.membership, command)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if retried.Created {
		t.Error("the retry reported Created = true; nothing was made the second time")
	}
	if len(*world.audits) != 0 {
		t.Errorf("audits = %d; a branch of unknown provenance was attributed to the retrying member",
			len(*world.audits))
	}
}

// TestCreateBranchRequiresAName is the idempotency the endpoint claims.
//
// Generating a name when one is omitted made each attempt produce a different
// branch, so a retry after a lost response created a second one — idempotent
// by name, while quietly making the name non-deterministic.
func TestCreateBranchRequiresAName(t *testing.T) {
	world := newBranchWorld(t)

	_, err := world.service.CreateBranch(context.Background(), world.membership,
		application.CreateBranchCommand{RepositoryID: world.repository.ID})
	if !errors.Is(err, domain.ErrInvalidBranchName) {
		t.Errorf("CreateBranch without a name = %v, want ErrInvalidBranchName", err)
	}
	if world.api.created > 0 {
		t.Error("a nameless request created a branch")
	}
}
