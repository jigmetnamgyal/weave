package workspaces

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
	rec, _ := serveSessionWith(t, newSessionMux(t, role, &fakeKeys{}), method, path, body, nil)
	return rec
}

// newSessionMux builds the routes with a chosen idempotency store, so a test
// can drive the claim outcomes without a database.
func newSessionMux(t *testing.T, role domain.Role, keys application.IdempotencyRepository) http.Handler {
	t.Helper()

	handler := NewHandler(application.NewWorkspaceService(&fakeStore{role: role}), discardLogger())
	mux := http.NewServeMux()
	handler.Register(mux)
	handler.RegisterSessions(mux, application.NewSessionService(
		&fakeSessions{}, &fakeSessionTasks{}, fakeAgents{}), keys)
	return httpx.WithRequestID(mux)
}

func serveSessionWith(
	t *testing.T,
	mux http.Handler,
	method, path, body string,
	headers map[string]string,
) (*httptest.ResponseRecorder, *http.Request) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	req = req.WithContext(auth.ContextWithUser(req.Context(),
		domain.User{ID: memberID, Email: "dev@example.com"}))

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec, req
}

// fakeKeys is an in-memory idempotency store with the same four outcomes the
// real one produces, so the transport's behaviour can be exercised without a
// database. The concurrency itself is tested against real PostgreSQL, because
// that is where the exclusivity lives.
type fakeKeys struct {
	records map[string]*domain.IdempotencyRecord
}

func (f *fakeKeys) key(scope domain.IdempotencyScope, key string) string {
	return scope.WorkspaceID.String() + "|" + scope.UserID.String() + "|" + scope.Endpoint + "|" + key
}

func (f *fakeKeys) Claim(
	_ context.Context,
	record domain.IdempotencyRecord,
	_ time.Duration,
) (application.IdempotencyOutcome, domain.IdempotencyRecord, uuid.UUID, error) {
	if f.records == nil {
		f.records = map[string]*domain.IdempotencyRecord{}
	}
	id := f.key(record.Scope, record.Key)
	existing, found := f.records[id]
	if !found {
		stored := record
		f.records[id] = &stored
		return application.IdempotencyClaimed, domain.IdempotencyRecord{}, uuid.New(), nil
	}
	if !bytes.Equal(existing.Fingerprint, record.Fingerprint) {
		return application.IdempotencyMismatch, *existing, uuid.Nil, nil
	}
	if existing.Response != nil {
		return application.IdempotencyComplete, *existing, uuid.Nil, nil
	}
	return application.IdempotencyInFlight, *existing, uuid.Nil, nil
}

func (f *fakeKeys) Release(_ context.Context, scope domain.IdempotencyScope, key string, _ uuid.UUID) error {
	delete(f.records, f.key(scope, key))
	return nil
}

// complete marks a key finished, standing in for what the store writes inside
// the session transaction.
func (f *fakeKeys) complete(scope domain.IdempotencyScope, key string, status int, body []byte, origin string) {
	if record, found := f.records[f.key(scope, key)]; found {
		record.Response = &domain.IdempotentResponse{Status: status, Body: body, OriginRequestID: origin}
	}
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
	completion *application.IdempotentCompletion,
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

	// The real store renders and records the response inside its transaction,
	// so the fake does too — a handler that stopped passing Render would fail
	// here rather than pass while the completion silently disappeared.
	if completion != nil && completion.Render != nil {
		if _, _, err := completion.Render(session); err != nil {
			return domain.Session{}, err
		}
	}
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

// sessionBody is the request the idempotency tests retry.
func sessionBody(base string) string {
	if base == "" {
		return `{"task_id":"` + readyTask.String() + `","agent_version_id":"` + someVersion.String() + `"}`
	}
	return `{"task_id":"` + readyTask.String() + `","agent_version_id":"` + someVersion.String() +
		`","base_branch":"` + base + `"}`
}

func sessionsPath() string { return "/v1/workspaces/" + workspaceA.String() + "/sessions" }

// TestARequestWithNoIdempotencyKeyIsUnchanged is the compatibility promise.
//
// The header is optional at the API, so a caller that has never sent one must
// keep working exactly as before — this unit must not make an existing request
// start failing for want of something it does not know about.
func TestARequestWithNoIdempotencyKeyIsUnchanged(t *testing.T) {
	mux := newSessionMux(t, domain.RoleDeveloper, &fakeKeys{})
	rec, _ := serveSessionWith(t, mux, http.MethodPost, sessionsPath(), sessionBody(""), nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}
}

// TestAnInFlightKeyIsRefusedRatherThanAnswered is the decision the whole unit
// turns on.
//
// An earlier attempt holds the key and has not finished. Answering "already
// done" would be a fabricated success for work that may yet fail — the mistake
// the M3.1 delivery path made when it acknowledged work GitHub never retried.
// The caller is refused and told when to come back.
func TestAnInFlightKeyIsRefusedRatherThanAnswered(t *testing.T) {
	keys := &fakeKeys{}
	mux := newSessionMux(t, domain.RoleDeveloper, keys)
	headers := map[string]string{"Idempotency-Key": "abc-123"}

	first, _ := serveSessionWith(t, mux, http.MethodPost, sessionsPath(), sessionBody(""), headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201: %s", first.Code, first.Body.String())
	}

	// The fake leaves the record claimed-but-incomplete, which is what a store
	// looks like while the first request is still running.
	second, _ := serveSessionWith(t, mux, http.MethodPost, sessionsPath(), sessionBody(""), headers)
	if second.Code != http.StatusConflict {
		t.Fatalf("second status = %d, want 409: %s", second.Code, second.Body.String())
	}
	if got := second.Header().Get("Retry-After"); got != "1" {
		t.Errorf("Retry-After = %q, want 1 — a refusal with no retry signal is a dead end", got)
	}
}

// TestACompletedKeyReplaysTheStoredResponse checks what replay means: the
// original status and the original body bytes, with the headers that describe
// the body.
func TestACompletedKeyReplaysTheStoredResponse(t *testing.T) {
	keys := &fakeKeys{}
	mux := newSessionMux(t, domain.RoleDeveloper, keys)
	headers := map[string]string{"Idempotency-Key": "abc-123", "X-Request-Id": "first-request"}

	first, _ := serveSessionWith(t, mux, http.MethodPost, sessionsPath(), sessionBody(""), headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.Code)
	}
	stored := first.Body.Bytes()

	keys.complete(domain.IdempotencyScope{
		WorkspaceID: workspaceA, UserID: memberID, Endpoint: "createSession",
	}, "abc-123", http.StatusCreated, stored, "first-request")

	// The retry carries a *different* request id, which is the case that
	// separates the two possible behaviours.
	second, _ := serveSessionWith(t, mux, http.MethodPost, sessionsPath(), sessionBody(""),
		map[string]string{"Idempotency-Key": "abc-123", "X-Request-Id": "second-request"})

	if second.Code != http.StatusCreated {
		t.Fatalf("replay status = %d, want the stored 201", second.Code)
	}
	if !bytes.Equal(second.Body.Bytes(), stored) {
		t.Errorf("replayed body differs from the stored bytes:\n first %q\n again %q",
			stored, second.Body.Bytes())
	}
	if got := second.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q on a replay", got)
	}
	if got := second.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q on a replay", got)
	}
	// The decision, asserted rather than assumed: the response carries the
	// retry's own request id, so it matches the log line for the request
	// actually made. The original is recorded and named in the replay's log
	// line instead.
	if got := second.Header().Get("X-Request-Id"); got != "second-request" {
		t.Errorf("X-Request-Id = %q, want the retry's own id", got)
	}
}

// TestEquivalentBodiesShareAFingerprint is the failure a byte comparison
// would cause: a retry that is the same request being refused because the
// client serialised it differently.
//
// The contract defines an omitted `base_branch` as "the repository default",
// so omitting it and sending it empty are the same request.
func TestEquivalentBodiesShareAFingerprint(t *testing.T) {
	keys := &fakeKeys{}
	mux := newSessionMux(t, domain.RoleDeveloper, keys)
	headers := map[string]string{"Idempotency-Key": "abc-123"}

	// The first attempt omits base_branch entirely.
	first, _ := serveSessionWith(t, mux, http.MethodPost, sessionsPath(), sessionBody(""), headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.Code)
	}

	// Each retry sends the *same* request written differently. A 422 means the
	// fingerprint decided they were different requests, which would refuse a
	// client whose only crime is serialising its JSON another way.
	equivalents := map[string]string{
		"sent empty rather than omitted": `{"task_id":"` + readyTask.String() +
			`","agent_version_id":"` + someVersion.String() + `","base_branch":""}`,
		"sent as whitespace": `{"task_id":"` + readyTask.String() +
			`","agent_version_id":"` + someVersion.String() + `","base_branch":"  "}`,
		"members reordered": `{"agent_version_id":"` + someVersion.String() +
			`","task_id":"` + readyTask.String() + `"}`,
		"whitespace added": `{ "task_id" : "` + readyTask.String() +
			`" ,  "agent_version_id" : "` + someVersion.String() + `" }`,
	}
	for name, body := range equivalents {
		t.Run(name, func(t *testing.T) {
			rec, _ := serveSessionWith(t, mux, http.MethodPost, sessionsPath(), body, headers)
			// Exactly 409, not merely "not 422". The first attempt left the
			// key claimed and incomplete, so an equivalent retry is in flight
			// — and asserting the absence of one status would pass just as
			// happily if the fingerprint logic broke in the other direction.
			if rec.Code != http.StatusConflict {
				t.Errorf("an equivalent retry (%s) returned %d, want 409 in-flight: %s",
					name, rec.Code, rec.Body.String())
			}
		})
	}
}

// TestAKeyReusedForADifferentRequestIsRefused is the other side: the client
// changed what it was asking for, and answering with the first result would
// hand back a session for work it did not request.
func TestAKeyReusedForADifferentRequestIsRefused(t *testing.T) {
	keys := &fakeKeys{}
	mux := newSessionMux(t, domain.RoleDeveloper, keys)
	headers := map[string]string{"Idempotency-Key": "abc-123"}

	first, _ := serveSessionWith(t, mux, http.MethodPost, sessionsPath(), sessionBody(""), headers)
	if first.Code != http.StatusCreated {
		t.Fatalf("first status = %d, want 201", first.Code)
	}

	second, _ := serveSessionWith(t, mux, http.MethodPost, sessionsPath(), sessionBody("develop"), headers)
	if second.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for a key reused with a different body: %s",
			second.Code, second.Body.String())
	}
}

// TestAMalformedIdempotencyKeyIsRefusedBeforeAnyWork keeps a bad header from
// reaching the store, and keeps the refusal distinguishable from a conflict.
func TestAMalformedIdempotencyKeyIsRefusedBeforeAnyWork(t *testing.T) {
	mux := newSessionMux(t, domain.RoleDeveloper, &fakeKeys{})
	for name, key := range map[string]string{
		"blank":        "   ",
		"control char": "abc\x00def",
		"too long":     strings.Repeat("k", 256),
	} {
		t.Run(name, func(t *testing.T) {
			rec, _ := serveSessionWith(t, mux, http.MethodPost, sessionsPath(), sessionBody(""),
				map[string]string{"Idempotency-Key": key})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400: %s", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestAFailedRequestReleasesItsKey is the fourth outcome, which the spec did
// not name.
//
// Without it a caller whose request failed for an unrelated reason — a task
// that is not ready, say — would be refused for the length of the lease,
// having done nothing wrong.
func TestAFailedRequestReleasesItsKey(t *testing.T) {
	keys := &fakeKeys{}
	mux := newSessionMux(t, domain.RoleDeveloper, keys)
	headers := map[string]string{"Idempotency-Key": "abc-123"}

	draft := `{"task_id":"` + draftTask.String() + `","agent_version_id":"` + someVersion.String() + `"}`
	failed, _ := serveSessionWith(t, mux, http.MethodPost, sessionsPath(), draft, headers)
	if failed.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 for a draft task", failed.Code)
	}

	// The same key, now used for a request that will succeed. If the failure
	// had held the key, this would be refused as in flight or as a mismatch.
	retry, _ := serveSessionWith(t, mux, http.MethodPost, sessionsPath(), sessionBody(""), headers)
	if retry.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201 — the failed request kept its key: %s",
			retry.Code, retry.Body.String())
	}
}
