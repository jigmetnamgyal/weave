package workspaces

import (
	"context"
	"encoding/json"
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

// serveTask runs one request through the task and agent routes.
func serveTask(t *testing.T, role domain.Role, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	handler := NewHandler(application.NewWorkspaceService(&fakeStore{role: role}), discardLogger())
	mux := http.NewServeMux()
	handler.Register(mux)
	handler.RegisterTasks(mux, application.NewTaskService(&fakeTasks{}))
	handler.RegisterAgents(mux, application.NewAgentService(&fakeAgents{}))

	var reader *strings.Reader
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
