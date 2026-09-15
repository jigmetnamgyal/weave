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

// TestTaskBodySurvivesTheRoundTripUnaltered is a transport-level test for a
// transport-level promise.
//
// The domain test proves ValidateTaskBody does not alter its input, and would
// keep passing if a handler trimmed, escaped or re-encoded on the way out.
// What the contract promises is what a *caller* gets back, so that is where
// this is asserted — the M3.3 lesson, where a service test passed while the
// handler discarded the value it was asserting.
func TestTaskBodySurvivesTheRoundTripUnaltered(t *testing.T) {
	bodies := map[string]string{
		"leading and trailing spaces": "  spaces  ",
		"an indented code block":      "\n    func main() {}\n",
		"shell metacharacters":        "${injected} $(also) `and` \\escape",
		"html":                        "<script>alert('x')</script>",
		"unicode":                     "ünïcødé 🙂 汉字",
		"windows line endings":        "one\r\ntwo",
	}

	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			payload, err := json.Marshal(map[string]string{"title": "t", "body": body})
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}

			rec := serveTask(t, domain.RoleDeveloper, http.MethodPost,
				"/v1/workspaces/"+workspaceA.String()+"/tasks", string(payload))
			if rec.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
			}

			var response taskResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if response.Body != body {
				t.Errorf("body came back altered:\n given %q\n  got  %q", body, response.Body)
			}
		})
	}
}

// TestAViewerCannotCreateATask pins the permission decision at the transport.
//
// Tasks are governed by `session:create`: a task is the input to a session, so
// anyone who may start one must be able to describe the work — and a viewer,
// who may not, has no use for a task.
func TestAViewerCannotCreateATask(t *testing.T) {
	rec := serveTask(t, domain.RoleViewer, http.MethodPost,
		"/v1/workspaces/"+workspaceA.String()+"/tasks", `{"title":"t"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for a viewer", rec.Code)
	}
}

// TestADeveloperCanCreateATaskButNotAnAgent is the separation the two
// permissions exist to make.
//
// An agent version carries the tool policy — what an agent may do inside a
// customer's repository — so redefining one is configuration, governed by
// `workspace:manage`. A developer may run agents and not change what they are
// allowed to do.
func TestADeveloperCanCreateATaskButNotAnAgent(t *testing.T) {
	task := serveTask(t, domain.RoleDeveloper, http.MethodPost,
		"/v1/workspaces/"+workspaceA.String()+"/tasks", `{"title":"t"}`)
	if task.Code != http.StatusCreated {
		t.Errorf("task status = %d, want 201 for a developer", task.Code)
	}

	agent := serveTask(t, domain.RoleDeveloper, http.MethodPost,
		"/v1/workspaces/"+workspaceA.String()+"/agents",
		`{"name":"a","provider":"fake","model":"m"}`)
	if agent.Code != http.StatusForbidden {
		t.Errorf("agent status = %d, want 403 for a developer", agent.Code)
	}
}

// TestAnUnknownCapabilityIsRefused checks the closed set reaches the caller as
// a bad request rather than a constraint violation.
func TestAnUnknownCapabilityIsRefused(t *testing.T) {
	rec := serveTask(t, domain.RoleOwner, http.MethodPost,
		"/v1/workspaces/"+workspaceA.String()+"/agents",
		`{"name":"a","provider":"fake","model":"m","capabilities":["teleport"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "teleport") {
		t.Errorf("response %s does not name the capability it refused", rec.Body.String())
	}
}

// TestAViewerCanReadTasksAndAgents is the other half of the permission
// decision, and the half a reviewer read as a missing check.
//
// Reads are governed by `workspace:read`, which every role holds, and not by
// the permission that governs writing. A viewer exists to see a workspace's
// work without changing it; gating these reads on `session:create` would have
// hardened nothing and made the role blind to what it was invited to watch.
// Asserted at the transport because that is where the answer is either 200 or
// 403.
func TestAViewerCanReadTasksAndAgents(t *testing.T) {
	paths := map[string]string{
		"tasks":  "/v1/workspaces/" + workspaceA.String() + "/tasks",
		"agents": "/v1/workspaces/" + workspaceA.String() + "/agents",
	}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			rec := serveTask(t, domain.RoleViewer, http.MethodGet, path, "")
			if rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 for a viewer reading %s: %s",
					rec.Code, name, rec.Body.String())
			}
		})
	}
}

// TestATitleIsBoundedInCharactersNotBytes pins the bound the database and the
// contract both use.
//
// PostgreSQL's length() counts characters and Go's len() counts bytes, so a
// byte count here would refuse a title the column would have stored — and the
// message would tell the caller they exceeded 200 characters while they were
// nowhere near it. 200 CJK characters is 600 bytes.
func TestATitleIsBoundedInCharactersNotBytes(t *testing.T) {
	atTheLimit := strings.Repeat("漢", 200)
	payload, err := json.Marshal(map[string]string{"title": atTheLimit})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec := serveTask(t, domain.RoleDeveloper, http.MethodPost,
		"/v1/workspaces/"+workspaceA.String()+"/tasks", string(payload))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 for a 200-character title: %s", rec.Code, rec.Body.String())
	}

	overIt, err := json.Marshal(map[string]string{"title": strings.Repeat("漢", 201)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	rec = serveTask(t, domain.RoleDeveloper, http.MethodPost,
		"/v1/workspaces/"+workspaceA.String()+"/tasks", string(overIt))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for a 201-character title", rec.Code)
	}
}

// TestAToolPolicyMustBeAJSONObject checks the shape the contract promises.
//
// `json.RawMessage` accepts any JSON value and the `jsonb` column stores any
// JSON value, so without this a policy could be stored as null, a number or an
// array. It is opaque to this unit, which is why the shape has to be enforced
// here: M7 interprets it, and finding a string there is finding it too late.
func TestAToolPolicyMustBeAJSONObject(t *testing.T) {
	refused := map[string]string{
		"null":   `null`,
		"array":  `[{"allow":"*"}]`,
		"string": `"everything"`,
		"number": `7`,
	}
	for name, policy := range refused {
		t.Run(name, func(t *testing.T) {
			rec := serveTask(t, domain.RoleOwner, http.MethodPost,
				"/v1/workspaces/"+workspaceA.String()+"/agents",
				`{"name":"a","provider":"fake","model":"m","tool_policy":`+policy+`}`)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400 for a %s policy: %s",
					rec.Code, name, rec.Body.String())
			}
		})
	}

	rec := serveTask(t, domain.RoleOwner, http.MethodPost,
		"/v1/workspaces/"+workspaceA.String()+"/agents",
		`{"name":"a","provider":"fake","model":"m","tool_policy":{"allow":["read"]}}`)
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201 for an object policy: %s", rec.Code, rec.Body.String())
	}
}

// TestAPatchLeavesOmittedFieldsAlone is what PATCH means.
//
// The schema requires only a title, so a client renaming a task sends only a
// title — and replacement semantics under a PATCH verb then silently wipe the
// body and revert a ready task to draft. The fake store echoes what it is
// given, so what comes back is what would have been written.
func TestAPatchLeavesOmittedFieldsAlone(t *testing.T) {
	rec := serveTask(t, domain.RoleDeveloper, http.MethodPatch,
		"/v1/workspaces/"+workspaceA.String()+"/tasks/"+existingTask.String(),
		`{"title":"renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var response taskResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.Title != "renamed" {
		t.Errorf("title = %q, want the patched value", response.Title)
	}
	if response.Body != storedBody {
		t.Errorf("an omitted body was overwritten:\n want %q\n  got %q", storedBody, response.Body)
	}
	if response.Status != string(domain.TaskReady) {
		t.Errorf("status = %q, want it left at ready — an omitted status reverted the task",
			response.Status)
	}
	if response.RepositoryID != storedRepository.String() {
		t.Errorf("repository_id = %q, want it left alone", response.RepositoryID)
	}
}

// TestAPatchCanStillClearTheRepository separates "omitted" from "sent as
// null", which is the distinction that makes a partial update usable: without
// it there is no way to move a ready task back to a draft with no repository.
func TestAPatchCanStillClearTheRepository(t *testing.T) {
	rec := serveTask(t, domain.RoleDeveloper, http.MethodPatch,
		"/v1/workspaces/"+workspaceA.String()+"/tasks/"+existingTask.String(),
		`{"status":"draft","repository_id":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var response taskResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if response.RepositoryID != "" {
		t.Errorf("repository_id = %q, want it cleared by an explicit null", response.RepositoryID)
	}
	if response.Body != storedBody {
		t.Errorf("clearing the repository also overwrote the body: %q", response.Body)
	}
}

// serveTask runs one request through the task and agent routes.
func serveTask(t *testing.T, role domain.Role, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	handler := NewHandler(application.NewWorkspaceService(&fakeStore{role: role}), discardLogger())
	mux := http.NewServeMux()
	handler.Register(mux)
	handler.RegisterTasks(mux, application.NewTaskService(&fakeTasks{}))
	handler.RegisterAgents(mux, application.NewAgentService(&fakeAgents{}))

	// Typed nil, not a nil interface, is what a `*strings.Reader` variable
	// left unset would give httptest — which panics on it. No test passed an
	// empty body until the read tests did.
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

// The task the store already holds, which the patch tests merge onto. It is a
// ready task with a body and a repository, because those are the three fields
// a partial update could silently destroy.
var (
	existingTask     = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	storedRepository = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	storedBody       = "the description someone wrote"
)

// fakeTasks echoes what it was given, which is the point: a store that altered
// the body would hide the very thing the round-trip test is checking.
type fakeTasks struct{}

func (fakeTasks) Create(_ context.Context, task domain.Task, _ application.Actor, _ application.AuditEvent) (domain.Task, error) {
	return task, nil
}
func (fakeTasks) Update(_ context.Context, task domain.Task, _ application.Actor, _ application.AuditEvent) (domain.Task, error) {
	return task, nil
}
func (fakeTasks) Get(_ context.Context, taskID, workspaceID uuid.UUID) (domain.Task, error) {
	if taskID == existingTask {
		repository := storedRepository
		return domain.Task{
			ID: taskID, WorkspaceID: workspaceID, Title: "as stored",
			Body: storedBody, RepositoryID: &repository, Status: domain.TaskReady,
		}, nil
	}
	return domain.Task{ID: taskID, WorkspaceID: workspaceID, Title: "t", Status: domain.TaskDraft}, nil
}
func (fakeTasks) List(context.Context, uuid.UUID) ([]domain.Task, error) { return nil, nil }

type fakeAgents struct{}

func (fakeAgents) Create(_ context.Context, agent domain.Agent, version domain.AgentVersion, _ application.Actor, _ application.AuditEvent) (domain.Agent, domain.AgentVersion, error) {
	return agent, version, nil
}
func (fakeAgents) AddVersion(_ context.Context, version domain.AgentVersion, _ application.Actor, _ application.AuditEvent) (domain.AgentVersion, error) {
	return version, nil
}
func (fakeAgents) Get(_ context.Context, agentID, workspaceID uuid.UUID) (domain.Agent, error) {
	return domain.Agent{ID: agentID, WorkspaceID: workspaceID, Name: "a"}, nil
}
func (fakeAgents) List(context.Context, uuid.UUID) ([]domain.Agent, error) { return nil, nil }
func (fakeAgents) ListVersions(context.Context, uuid.UUID, uuid.UUID) ([]domain.AgentVersion, error) {
	return nil, nil
}
func (fakeAgents) GetVersion(_ context.Context, versionID, workspaceID uuid.UUID) (domain.AgentVersion, error) {
	return domain.AgentVersion{ID: versionID, WorkspaceID: workspaceID}, nil
}
