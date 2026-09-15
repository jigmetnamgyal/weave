package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// TaskStore persists tasks.
type TaskStore struct {
	pool *pgxpool.Pool
}

// NewTaskStore returns a store backed by pool.
func NewTaskStore(pool *pgxpool.Pool) *TaskStore {
	return &TaskStore{pool: pool}
}

// inTx runs fn inside a transaction carrying the caller's tenant context.
func (s *TaskStore) inTx(ctx context.Context, fn func(*postgresdb.Queries) error) error {
	return inTenantTx(ctx, s.pool, fn)
}

// Create records a task and its audit event.
func (s *TaskStore) Create(
	ctx context.Context,
	task domain.Task,
	actor application.Actor,
	event application.AuditEvent,
) (domain.Task, error) {
	var created domain.Task

	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		// The same in-transaction re-check every other mutation does: the
		// handler authorized from a snapshot and the actor may have been
		// removed or demoted since.
		if err := authorizeActor(ctx, q, task.WorkspaceID, actor); err != nil {
			return err
		}

		row, err := q.CreateTask(ctx, postgresdb.CreateTaskParams{
			ID:           task.ID,
			WorkspaceID:  task.WorkspaceID,
			RepositoryID: nullableUUID(task.RepositoryID),
			Title:        task.Title,
			Body:         task.Body,
			Status:       string(task.Status),
			CreatedBy:    task.CreatedBy,
		})
		if err != nil {
			return translateTaskError(err)
		}
		created = taskToDomain(row)
		return appendAudit(ctx, q, event)
	})
	return created, err
}

// Update replaces a task's editable fields.
func (s *TaskStore) Update(
	ctx context.Context,
	task domain.Task,
	actor application.Actor,
	event application.AuditEvent,
) (domain.Task, error) {
	var updated domain.Task

	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		if err := authorizeActor(ctx, q, task.WorkspaceID, actor); err != nil {
			return err
		}

		row, err := q.UpdateTask(ctx, postgresdb.UpdateTaskParams{
			ID:           task.ID,
			WorkspaceID:  task.WorkspaceID,
			RepositoryID: nullableUUID(task.RepositoryID),
			Title:        task.Title,
			Body:         task.Body,
			Status:       string(task.Status),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrTaskNotFound
			}
			return translateTaskError(err)
		}
		updated = taskToDomain(row)
		return appendAudit(ctx, q, event)
	})
	return updated, err
}

// Get returns one task, scoped to the workspace in context.
func (s *TaskStore) Get(ctx context.Context, taskID, workspaceID uuid.UUID) (domain.Task, error) {
	var task domain.Task
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		row, err := q.GetTaskForWorkspace(ctx, postgresdb.GetTaskForWorkspaceParams{
			ID:          taskID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrTaskNotFound
			}
			return fmt.Errorf("select task: %w", err)
		}
		task = taskToDomain(row)
		return nil
	})
	return task, err
}

// List returns a workspace's tasks, newest first.
func (s *TaskStore) List(ctx context.Context, workspaceID uuid.UUID) ([]domain.Task, error) {
	var tasks []domain.Task
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		rows, err := q.ListTasksForWorkspace(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("list tasks: %w", err)
		}
		tasks = make([]domain.Task, 0, len(rows))
		for _, row := range rows {
			tasks = append(tasks, taskToDomain(row))
		}
		return nil
	})
	return tasks, err
}

// translateTaskError turns the constraints into errors a caller can act on.
//
// The database carries these checks so that no path can write a row a session
// would later fail on. Relaying a raw constraint violation would be accurate
// and useless, so each one that a caller can actually fix is named.
func translateTaskError(err error) error {
	switch {
	case constraintViolated(err, "tasks_ready_has_repository"):
		return domain.ErrTaskNotReady
	case constraintViolated(err, "tasks_title_length"), constraintViolated(err, "tasks_title_not_blank"):
		return fmt.Errorf("%w: the title is empty or too long", domain.ErrInvalidTask)
	case constraintViolated(err, "tasks_body_length"):
		return fmt.Errorf("%w: the body is too long", domain.ErrInvalidTask)
	case constraintViolated(err, "tasks_status_valid"):
		return fmt.Errorf("%w: unknown status", domain.ErrInvalidTask)
	case constraintViolated(err, "tasks_repository_fkey"):
		return application.ErrRepositoryNotFound
	default:
		return fmt.Errorf("write task: %w", err)
	}
}

// taskToDomain converts a generated row.
func taskToDomain(row postgresdb.Task) domain.Task {
	task := domain.Task{
		ID:          row.ID,
		WorkspaceID: row.WorkspaceID,
		Title:       row.Title,
		Body:        row.Body,
		Status:      domain.TaskStatus(row.Status),
		CreatedBy:   row.CreatedBy,
		CreatedAt:   timestamp(row.CreatedAt),
		UpdatedAt:   timestamp(row.UpdatedAt),
	}
	if row.RepositoryID.Valid {
		id := uuid.UUID(row.RepositoryID.Bytes)
		task.RepositoryID = &id
	}
	return task
}
