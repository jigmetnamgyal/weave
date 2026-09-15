package workspaces

import (
	"context"
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

	var body taskRequest
	if !t.handler.decode(ctx, w, r, &body) {
		return
	}

	repositoryID, status, ok := t.parse(ctx, w, body)
	if !ok {
		return
	}

	task, err := t.service.Update(ctx, membership, application.UpdateTaskCommand{
		TaskID:       taskID,
		Title:        body.Title,
		Body:         body.Body,
		RepositoryID: repositoryID,
		Status:       status,
	})
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
