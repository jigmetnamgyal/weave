package workspaces

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

// RegisterTasks mounts the task routes.
func (h *Handler) RegisterTasks(mux *http.ServeMux, tasks *application.TaskService) {
	routes := &taskRoutes{handler: h, service: tasks}

	mux.Handle("POST /v1/workspaces/{workspaceID}/tasks",
		h.requireMembership(http.HandlerFunc(routes.create)))
	mux.Handle("GET /v1/workspaces/{workspaceID}/tasks",
		h.requireMembership(http.HandlerFunc(routes.list)))
	mux.Handle("GET /v1/workspaces/{workspaceID}/tasks/{taskID}",
		h.requireMembership(http.HandlerFunc(routes.get)))
	mux.Handle("PATCH /v1/workspaces/{workspaceID}/tasks/{taskID}",
		h.requireMembership(http.HandlerFunc(routes.update)))
}

type taskRoutes struct {
	handler *Handler
	service *application.TaskService
}

type taskRequest struct {
	Title string `json:"title"`
	// Body is carried through unaltered in both directions. Sanitising on the
	// way out would make the stored value and the returned value disagree,
	// after which nobody can tell what is actually stored.
	Body         string `json:"body"`
	RepositoryID string `json:"repository_id"`
	Status       string `json:"status"`
}

// taskPatchRequest is the PATCH body, where an omitted field and a field sent
// as null are different requests.
//
// Plain `string` fields cannot express that difference — both arrive as "" —
// which is what made the first version of this handler destroy a body it was
// never asked to touch. `optional` records whether the key was present at all.
type taskPatchRequest struct {
	Title        optional[string] `json:"title"`
	Body         optional[string] `json:"body"`
	RepositoryID optional[string] `json:"repository_id"`
	Status       optional[string] `json:"status"`
}

// optional distinguishes three states of a JSON field: absent, null, and set.
//
// UnmarshalJSON is called only when the key is present, which is what makes
// Present meaningful — there is no other way to learn it after decoding.
type optional[T any] struct {
	Present bool
	Value   *T
}

func (o *optional[T]) UnmarshalJSON(data []byte) error {
	o.Present = true
	if string(data) == "null" {
		o.Value = nil
		return nil
	}
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	o.Value = &value
	return nil
}

type taskResponse struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Body         string    `json:"body"`
	RepositoryID string    `json:"repository_id,omitempty"`
	Status       string    `json:"status"`
	CreatedBy    string    `json:"created_by"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (t *taskRoutes) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		t.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	var body taskRequest
	if !t.handler.decode(ctx, w, r, &body) {
		return
	}

	repositoryID, status, ok := t.parse(ctx, w, body)
	if !ok {
		return
	}

	task, err := t.service.Create(ctx, membership, application.CreateTaskCommand{
		Title:        body.Title,
		Body:         body.Body,
		RepositoryID: repositoryID,
		Status:       status,
	})
	if err != nil {
		t.handler.writeError(ctx, w, err, "create task")
		return
	}
	httpx.WriteJSON(ctx, w, http.StatusCreated, toTaskResponse(task))
}

func (t *taskRoutes) update(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		t.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	taskID, err := uuid.Parse(r.PathValue("taskID"))
	if err != nil {
		t.handler.notFound(ctx, w)
		return
	}

	var body taskPatchRequest
	if !t.handler.decode(ctx, w, r, &body) {
		return
	}

	// `repository_id` is the only nullable field in the schema, so null in any
	// other is a malformed request rather than a clear. Refused rather than
	// treated as an omission, which would answer 200 to a request that asked
	// for something the contract does not offer.
	for field, sent := range map[string]bool{
		"title":  body.Title.Present && body.Title.Value == nil,
		"body":   body.Body.Present && body.Body.Value == nil,
		"status": body.Status.Present && body.Status.Value == nil,
	} {
		if sent {
			httpx.WriteError(ctx, w, http.StatusBadRequest, httpx.CodeInvalidRequest,
				field+" cannot be null.")
			return
		}
	}

	command := application.UpdateTaskCommand{
		TaskID: taskID,
		Title:  body.Title.Value,
		Body:   body.Body.Value,
	}
	if body.Status.Present {
		status, err := domain.ParseTaskStatus(*body.Status.Value)
		if err != nil {
			t.handler.writeError(ctx, w, err, "parse status")
			return
		}
		command.Status = &status
	}
	if body.RepositoryID.Present {
		command.RepositoryIDSet = true
		if body.RepositoryID.Value != nil {
			parsed, err := uuid.Parse(*body.RepositoryID.Value)
			if err != nil {
				httpx.WriteError(ctx, w, http.StatusBadRequest, httpx.CodeInvalidRequest,
					"repository_id must be a UUID.")
				return
			}
			command.RepositoryID = &parsed
		}
	}

	task, err := t.service.Update(ctx, membership, command)
	if err != nil {
		t.handler.writeError(ctx, w, err, "update task")
		return
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, toTaskResponse(task))
}

func (t *taskRoutes) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		t.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	taskID, err := uuid.Parse(r.PathValue("taskID"))
	if err != nil {
		t.handler.notFound(ctx, w)
		return
	}

	task, err := t.service.Get(ctx, membership, taskID)
	if err != nil {
		t.handler.writeError(ctx, w, err, "get task")
		return
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, toTaskResponse(task))
}

func (t *taskRoutes) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		t.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	tasks, err := t.service.List(ctx, membership)
	if err != nil {
		t.handler.writeError(ctx, w, err, "list tasks")
		return
	}

	items := make([]taskResponse, 0, len(tasks))
	for _, task := range tasks {
		items = append(items, toTaskResponse(task))
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, map[string]any{"items": items})
}

// parse turns the two string fields that are not plain text into their typed
// forms, answering the caller directly when either is malformed.
func (t *taskRoutes) parse(ctx context.Context, w http.ResponseWriter, body taskRequest) (*uuid.UUID, domain.TaskStatus, bool) {
	var repositoryID *uuid.UUID
	if body.RepositoryID != "" {
		parsed, err := uuid.Parse(body.RepositoryID)
		if err != nil {
			httpx.WriteError(ctx, w, http.StatusBadRequest, httpx.CodeInvalidRequest,
				"repository_id must be a UUID.")
			return nil, "", false
		}
		repositoryID = &parsed
	}

	status := domain.TaskDraft
	if body.Status != "" {
		parsed, err := domain.ParseTaskStatus(body.Status)
		if err != nil {
			t.handler.writeError(ctx, w, err, "parse status")
			return nil, "", false
		}
		status = parsed
	}
	return repositoryID, status, true
}

func toTaskResponse(task domain.Task) taskResponse {
	response := taskResponse{
		ID:        task.ID.String(),
		Title:     task.Title,
		Body:      task.Body,
		Status:    string(task.Status),
		CreatedBy: task.CreatedBy.String(),
		CreatedAt: task.CreatedAt,
		UpdatedAt: task.UpdatedAt,
	}
	if task.RepositoryID != nil {
		response.RepositoryID = task.RepositoryID.String()
	}
	return response
}
