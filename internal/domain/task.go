package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Errors the task domain raises.
var (
	// ErrInvalidTask is returned when input fails a task invariant.
	ErrInvalidTask = errors.New("invalid task")
	// ErrTaskNotReady is returned when a task is moved to ready without
	// naming the repository it runs against.
	ErrTaskNotReady = errors.New("a task cannot be ready without a repository")
)

// Task bounds. These match the CHECK constraints in the migration; keep them
// in step. The database is the one that matters — a path that skips validation
// still cannot write something unbounded — and these exist so the caller gets
// a useful message rather than a constraint violation.
const (
	taskTitleMaxLen = 200
	taskBodyMaxLen  = 50000
)

// TaskStatus is where a task sits between being written and being run.
type TaskStatus string

// The statuses a task may hold. A closed set rather than free text: a status
// nothing recognises is a task that silently never runs.
const (
	// TaskDraft is still being written, and may not name a repository yet.
	TaskDraft TaskStatus = "draft"
	// TaskReady can be turned into a session, which is why it must name a
	// repository.
	TaskReady TaskStatus = "ready"
	// TaskArchived is kept for the record and not offered for new sessions.
	TaskArchived TaskStatus = "archived"
)

// ParseTaskStatus validates a status from a caller.
func ParseTaskStatus(value string) (TaskStatus, error) {
	switch TaskStatus(strings.TrimSpace(value)) {
	case TaskDraft:
		return TaskDraft, nil
	case TaskReady:
		return TaskReady, nil
	case TaskArchived:
		return TaskArchived, nil
	default:
		return "", fmt.Errorf("%w: unknown status %q", ErrInvalidTask, value)
	}
}

// Task is a unit of work someone wants done.
type Task struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	// RepositoryID is absent while the task is a draft, and required once it
	// is ready: a task that cannot name what it runs against cannot become a
	// session.
	RepositoryID *uuid.UUID
	Title        string
	// Body is untrusted input. It is stored and returned as data and is never
	// interpolated into a command, a prompt template, a ref name, a log format
	// string, or an audit detail something later parses.
	Body      string
	Status    TaskStatus
	CreatedBy uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ValidateTaskTitle normalises and checks a title.
//
// Trimmed because leading and trailing whitespace in a title is never meant,
// and a title of only whitespace is empty in every way that matters.
func ValidateTaskTitle(title string) (string, error) {
	trimmed := strings.TrimSpace(title)
	switch {
	case trimmed == "":
		return "", fmt.Errorf("%w: a title is required", ErrInvalidTask)
	case len(trimmed) > taskTitleMaxLen:
		return "", fmt.Errorf("%w: a title may be at most %d characters", ErrInvalidTask, taskTitleMaxLen)
	}
	return trimmed, nil
}

// ValidateTaskBody checks a body.
//
// Deliberately not trimmed, and deliberately not otherwise altered. The body
// is the person's own description of what they want; leading whitespace may be
// an indented code block, and sanitising would make the stored value and the
// returned value disagree — after which nobody can tell what is actually
// stored. It is bounded, and nothing else.
func ValidateTaskBody(body string) (string, error) {
	if len(body) > taskBodyMaxLen {
		return "", fmt.Errorf("%w: a body may be at most %d characters", ErrInvalidTask, taskBodyMaxLen)
	}
	return body, nil
}

// ReadyRequiresRepository enforces the invariant the database also carries.
//
// Checked here so the caller gets a message naming the problem, and there so
// no path that skips this can write a row a session would later fail on.
func ReadyRequiresRepository(status TaskStatus, repositoryID *uuid.UUID) error {
	if status == TaskReady && repositoryID == nil {
		return ErrTaskNotReady
	}
	return nil
}

// NewTaskID returns an identifier for a new task.
func NewTaskID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate task id: %w", err)
	}
	return id, nil
}
