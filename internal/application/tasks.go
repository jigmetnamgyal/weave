package application

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Audit action names for tasks and agents. Stable strings: operators query
// them and they must not change meaning between releases.
const (
	AuditTaskCreated         = "task.created"
	AuditTaskUpdated         = "task.updated"
	AuditAgentCreated        = "agent.created"
	AuditAgentVersionCreated = "agent.version.created"
)

// TaskRepository is the persistence port for tasks.
type TaskRepository interface {
	Create(ctx context.Context, task domain.Task, actor Actor, event AuditEvent) (domain.Task, error)
	Update(ctx context.Context, task domain.Task, actor Actor, event AuditEvent) (domain.Task, error)
	Get(ctx context.Context, taskID, workspaceID uuid.UUID) (domain.Task, error)
	List(ctx context.Context, workspaceID uuid.UUID) ([]domain.Task, error)
}

// TaskService creates and edits tasks.
type TaskService struct {
	tasks TaskRepository
}

// NewTaskService wires the service.
func NewTaskService(tasks TaskRepository) *TaskService {
	return &TaskService{tasks: tasks}
}

// taskPermission is the permission governing tasks.
//
// `session:create`, reused rather than a new `task:manage`, and the reasoning
// is worth keeping next to the decision. A task is the input to a session:
// anyone who may start one must be able to describe the work, and anyone who
// may not has no use for a task. The matrix grants `session:create` to owner,
// admin and developer — exactly the set a `task:manage` permission would have
// been granted to, which would make it a permission that distinguishes
// nothing while adding a row every future role has to answer for.
const taskPermission = domain.PermissionSessionCreate

// CreateTaskCommand is a request to create a task.
type CreateTaskCommand struct {
	Title string
	// Body is untrusted input, carried through unaltered.
	Body         string
	RepositoryID *uuid.UUID
	Status       domain.TaskStatus
}

// Create records a task.
func (s *TaskService) Create(
	ctx context.Context,
	membership domain.Membership,
	command CreateTaskCommand,
) (domain.Task, error) {
	if !membership.Can(taskPermission) {
		return domain.Task{}, fmt.Errorf("%w: %s requires %s",
			ErrPermissionDenied, membership.Role, taskPermission)
	}

	title, err := domain.ValidateTaskTitle(command.Title)
	if err != nil {
		return domain.Task{}, err
	}
	body, err := domain.ValidateTaskBody(command.Body)
	if err != nil {
		return domain.Task{}, err
	}

	status := command.Status
	if status == "" {
		status = domain.TaskDraft
	}
	if err := domain.ReadyRequiresRepository(status, command.RepositoryID); err != nil {
		return domain.Task{}, err
	}

	id, err := domain.NewTaskID()
	if err != nil {
		return domain.Task{}, err
	}

	task := domain.Task{
		ID:           id,
		WorkspaceID:  membership.WorkspaceID,
		RepositoryID: command.RepositoryID,
		Title:        title,
		Body:         body,
		Status:       status,
		CreatedBy:    membership.UserID,
	}

	// The audit detail carries the title and never the body. A body is
	// untrusted input of arbitrary length, and an audit trail is read by
	// operators and machinery that was not written expecting it.
	return s.tasks.Create(ctx, task,
		Actor{UserID: membership.UserID, Required: taskPermission},
		AuditEvent{
			WorkspaceID: membership.WorkspaceID,
			ActorUserID: membership.UserID,
			Action:      AuditTaskCreated,
			Target:      id.String(),
			Detail:      map[string]any{"title": title, "status": string(status)},
		})
}

// UpdateTaskCommand is a request to change a task.
type UpdateTaskCommand struct {
	TaskID       uuid.UUID
	Title        string
	Body         string
	RepositoryID *uuid.UUID
	Status       domain.TaskStatus
}

// Update replaces a task's editable fields.
func (s *TaskService) Update(
	ctx context.Context,
	membership domain.Membership,
	command UpdateTaskCommand,
) (domain.Task, error) {
	if !membership.Can(taskPermission) {
		return domain.Task{}, fmt.Errorf("%w: %s requires %s",
			ErrPermissionDenied, membership.Role, taskPermission)
	}

	// Read first, so that updating a task belonging to another workspace is a
	// not-found rather than a write that the policies happen to match nothing
	// for — the caller gets the same answer either way, but only one of them
	// is a deliberate decision.
	if _, err := s.tasks.Get(ctx, command.TaskID, membership.WorkspaceID); err != nil {
		return domain.Task{}, err
	}

	title, err := domain.ValidateTaskTitle(command.Title)
	if err != nil {
		return domain.Task{}, err
	}
	body, err := domain.ValidateTaskBody(command.Body)
	if err != nil {
		return domain.Task{}, err
	}
	if err := domain.ReadyRequiresRepository(command.Status, command.RepositoryID); err != nil {
		return domain.Task{}, err
	}

	return s.tasks.Update(ctx, domain.Task{
		ID:           command.TaskID,
		WorkspaceID:  membership.WorkspaceID,
		RepositoryID: command.RepositoryID,
		Title:        title,
		Body:         body,
		Status:       command.Status,
	}, Actor{UserID: membership.UserID, Required: taskPermission},
		AuditEvent{
			WorkspaceID: membership.WorkspaceID,
			ActorUserID: membership.UserID,
			Action:      AuditTaskUpdated,
			Target:      command.TaskID.String(),
			Detail:      map[string]any{"title": title, "status": string(command.Status)},
		})
}

// readPermission governs reading tasks and agent profiles.
//
// `workspace:read`, not the permission that governs writing them, and the
// distinction is the whole point of having two. A viewer holds `workspace:read`
// and not `session:create`: the role exists to see a workspace's work without
// changing it, so gating these reads on the write permission would not harden
// anything, it would make the viewer role blind to the work it was invited to
// watch. Every role holds `workspace:read`, so this denies nobody today. It is
// stated rather than left implicit because a read path that checks nothing
// reads as an oversight, and because the next role added has to answer for it.
const readPermission = domain.PermissionWorkspaceRead

// Get returns one task.
func (s *TaskService) Get(ctx context.Context, membership domain.Membership, taskID uuid.UUID) (domain.Task, error) {
	if !membership.Can(readPermission) {
		return domain.Task{}, fmt.Errorf("%w: %s requires %s",
			ErrPermissionDenied, membership.Role, readPermission)
	}
	return s.tasks.Get(ctx, taskID, membership.WorkspaceID)
}

// List returns a workspace's tasks.
func (s *TaskService) List(ctx context.Context, membership domain.Membership) ([]domain.Task, error) {
	if !membership.Can(readPermission) {
		return nil, fmt.Errorf("%w: %s requires %s",
			ErrPermissionDenied, membership.Role, readPermission)
	}
	return s.tasks.List(ctx, membership.WorkspaceID)
}
