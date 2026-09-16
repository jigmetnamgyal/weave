package workspaces

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/api/internal/auth"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

var (
	readyTask   = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	draftTask   = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	otherTask   = uuid.MustParse("55555555-5555-4555-8555-555555555555")
	someVersion = uuid.MustParse("66666666-6666-4666-8666-666666666666")
	taskRepo    = uuid.MustParse("77777777-7777-4777-8777-777777777777")
)

// TestASessionIsCreatedQueuedWithItsBranchIntent is the unit's headline
// behaviour asserted where a caller sees it.
//
// A created session is `queued` rather than `draft` — it already names its
// task, its agent version and the branch it will use, so there is nothing left
// to compose — and it carries a branch name that does not exist yet.
func TestASessionIsCreatedQueuedWithItsBranchIntent(t *testing.T) {
	rec := serveSession(t, domain.RoleDeveloper, http.MethodPost,
		"/v1/workspaces/"+workspaceA.String()+"/sessions",
		`{"task_id":"`+readyTask.String()+`","agent_version_id":"`+someVersion.String()+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	var response sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.State != string(domain.SessionQueued) {
		t.Errorf("state = %q, want queued", response.State)
	}
	if response.Version != 1 {
		t.Errorf("version = %d, want 1", response.Version)
	}
	if response.RepositoryID != taskRepo.String() {
		t.Errorf("repository_id = %q, want the task's repository", response.RepositoryID)
	}
	if !strings.HasPrefix(response.BranchName, domain.BranchPrefix) {
		t.Errorf("branch_name = %q, want it inside the %q namespace",
			response.BranchName, domain.BranchPrefix)
	}
	// The branch name must be derivable from the session id alone, which is
	// what makes a retried workflow activity find the branch rather than cut a
	// second one. Recomputing it here proves the stored name is that function
	// and not something the handler invented.
	id, err := uuid.Parse(response.ID)
	if err != nil {
		t.Fatalf("parse session id: %v", err)
	}
	derived, err := domain.SessionBranchName(id, "ready to run")
	if err != nil {
		t.Fatalf("SessionBranchName: %v", err)
	}
	if response.BranchName != derived {
		t.Errorf("branch_name = %q, want %q derived from the session id",
			response.BranchName, derived)
	}
}

// TestACreatedSessionOffersOnlyTheTransitionsTheTableAllows checks that the
// next states are served rather than left for the browser to reimplement.
func TestACreatedSessionOffersOnlyTheTransitionsTheTableAllows(t *testing.T) {
	rec := serveSession(t, domain.RoleDeveloper, http.MethodPost,
		"/v1/workspaces/"+workspaceA.String()+"/sessions",
		`{"task_id":"`+readyTask.String()+`","agent_version_id":"`+someVersion.String()+`"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	var response sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}

	want := map[string]bool{}
	for _, state := range domain.NextStates(domain.SessionQueued) {
		want[string(state)] = true
	}
	if len(response.NextStates) != len(want) {
		t.Fatalf("next_states = %v, want the %d the table allows from queued",
			response.NextStates, len(want))
	}
	for _, state := range response.NextStates {
		if !want[state] {
			t.Errorf("next_states offers %q, which the transition table denies", state)
		}
	}
}

// TestASessionCannotBeCreatedFromATaskThatIsNotReady pins the rule that keeps
// the branch intent pointing somewhere.
//
// A ready task is one that names a repository — the database enforces it — so
// a draft has nothing for the branch to be cut in. 409 rather than 400:
// nothing about the request is malformed.
func TestASessionCannotBeCreatedFromATaskThatIsNotReady(t *testing.T) {
	rec := serveSession(t, domain.RoleDeveloper, http.MethodPost,
		"/v1/workspaces/"+workspaceA.String()+"/sessions",
		`{"task_id":"`+draftTask.String()+`","agent_version_id":"`+someVersion.String()+`"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "ready") {
		t.Errorf("response %s does not say what is wrong with the task", rec.Body.String())
	}
}

// TestATaskFromAnotherWorkspaceIsNotFound keeps the two-step authorization
// answer consistent: absent, not forbidden.
//
// A 403 would confirm the task exists somewhere, which turns identifier
// guessing into tenant enumeration.
func TestATaskFromAnotherWorkspaceIsNotFound(t *testing.T) {
	rec := serveSession(t, domain.RoleDeveloper, http.MethodPost,
		"/v1/workspaces/"+workspaceA.String()+"/sessions",
		`{"task_id":"`+otherTask.String()+`","agent_version_id":"`+someVersion.String()+`"}`)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for a task in another workspace: %s",
			rec.Code, rec.Body.String())
	}
}

// TestAViewerCannotCreateASession pins the permission decision at the
// transport, and the other half of it: a viewer still reads.
func TestAViewerCannotCreateASession(t *testing.T) {
	rec := serveSession(t, domain.RoleViewer, http.MethodPost,
		"/v1/workspaces/"+workspaceA.String()+"/sessions",
		`{"task_id":"`+readyTask.String()+`","agent_version_id":"`+someVersion.String()+`"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("create status = %d, want 403 for a viewer", rec.Code)
	}

	list := serveSession(t, domain.RoleViewer, http.MethodGet,
		"/v1/workspaces/"+workspaceA.String()+"/sessions", "")
	if list.Code != http.StatusOK {
		t.Errorf("list status = %d, want 200 — a viewer watches without changing: %s",
			list.Code, list.Body.String())
	}
}

// TestSessionCreationRejectsMalformedIdentifiers checks the parse happens
// before anything is read, so a typo is a 400 rather than a 404 implying the
// identifier was looked for.
func TestSessionCreationRejectsMalformedIdentifiers(t *testing.T) {
	bodies := map[string]string{
		"task_id":          `{"task_id":"not-a-uuid","agent_version_id":"` + someVersion.String() + `"}`,
		"agent_version_id": `{"task_id":"` + readyTask.String() + `","agent_version_id":"nope"}`,
	}
	for field, body := range bodies {
		t.Run(field, func(t *testing.T) {
			rec := serveSession(t, domain.RoleDeveloper, http.MethodPost,
				"/v1/workspaces/"+workspaceA.String()+"/sessions", body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), field) {
				t.Errorf("response %s does not name the field it refused", rec.Body.String())
			}
		})
	}
}

// serveSession runs one request through the session routes.
func serveSession(t *testing.T, role domain.Role, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	handler := NewHandler(application.NewWorkspaceService(&fakeStore{role: role}), discardLogger())
	mux := http.NewServeMux()
	handler.Register(mux)
	handler.RegisterSessions(mux, application.NewSessionService(
		&fakeSessions{}, &fakeSessionTasks{}, fakeAgents{}))

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req = req.WithContext(auth.ContextWithUser(req.Context(),
		domain.User{ID: memberID, Email: "dev@example.com"}))

	rec := httptest.NewRecorder()
	httpx.WithRequestID(mux).ServeHTTP(rec, req)
	return rec
}

// --- fakes -----------------------------------------------------------------

// fakeSessionTasks answers with three tasks: one ready, one still a draft, and
// one that belongs to somebody else.
type fakeSessionTasks struct{}

func (fakeSessionTasks) Get(_ context.Context, taskID, workspaceID uuid.UUID) (domain.Task, error) {
	switch taskID {
	case readyTask:
		repository := taskRepo
		return domain.Task{
			ID: taskID, WorkspaceID: workspaceID, Title: "ready to run",
			RepositoryID: &repository, Status: domain.TaskReady,
		}, nil
	case draftTask:
		return domain.Task{
			ID: taskID, WorkspaceID: workspaceID, Title: "still writing",
			Status: domain.TaskDraft,
		}, nil
	default:
		return domain.Task{}, application.ErrTaskNotFound
	}
}

// fakeSessions echoes what it was given, which is what lets the branch-name
// assertion mean something: a store that rewrote the name would hide it.
type fakeSessions struct{}

func (f fakeSessions) Create(
	ctx context.Context,
	session domain.Session,
	_ domain.SessionParticipant,
	_ domain.SessionStateTransition,
	_ application.OutboxEvent,
	verifyTask func(domain.Task) error,
	_ application.Actor,
	_ application.AuditEvent,
) (domain.Session, error) {
	// The fake runs the verification the real store runs against the locked
	// row, so a handler that stopped passing it would fail here rather than
	// pass while the check silently disappeared.
	locked, err := fakeSessionTasks{}.Get(ctx, session.TaskID, session.WorkspaceID)
	if err != nil {
		return domain.Session{}, err
	}
	if err := verifyTask(locked); err != nil {
		return domain.Session{}, err
	}
	session.Version = 1
	return session, nil
}

func (fakeSessions) Transition(
	_ context.Context,
	sessionID, workspaceID uuid.UUID,
	decide func(domain.Session) (domain.SessionStateTransition, error),
	_ application.Actor,
	_ func(domain.Session) application.AuditEvent,
) (domain.Session, error) {
	current := domain.Session{
		ID: sessionID, WorkspaceID: workspaceID,
		State: domain.SessionQueued, Version: 1,
	}
	transition, err := decide(current)
	if err != nil {
		return domain.Session{}, err
	}
	current.State = transition.NextState
	current.Version++
	return current, nil
}

func (fakeSessions) Get(_ context.Context, sessionID, workspaceID uuid.UUID) (domain.Session, error) {
	return domain.Session{ID: sessionID, WorkspaceID: workspaceID, State: domain.SessionQueued}, nil
}

func (fakeSessions) List(context.Context, uuid.UUID) ([]domain.Session, error) { return nil, nil }

func (fakeSessions) ListParticipants(context.Context, uuid.UUID, uuid.UUID) ([]domain.SessionParticipant, error) {
	return nil, nil
}

func (fakeSessions) ListTransitions(context.Context, uuid.UUID, uuid.UUID) ([]domain.SessionStateTransition, error) {
	return nil, nil
}
