package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Errors the session application service raises.
var (
	// ErrSessionNotFound is returned when no session matches, for any reason.
	// A caller who is not a member and one naming a session that never existed
	// receive the same thing, so probing cannot distinguish them.
	ErrSessionNotFound = errors.New("session not found")
	// ErrAgentVersionNotFound is returned when no agent version matches.
	ErrAgentVersionNotFound = errors.New("agent version not found")
)

// Audit action names for sessions. Stable strings: operators query them and
// they must not change meaning between releases.
const (
	AuditSessionCreated = "session.created"
	// AuditSessionTransitioned records a state change. One action rather than
	// one per state: an operator querying "what happened to this session"
	// wants the sequence, and the states are in the detail.
	AuditSessionTransitioned = "session.transitioned"
)

// Outbox topics. Stable strings for the same reason, and the consumer
// dispatches on them — a rename is a new topic, not a changed meaning.
const TopicSessionCreated = "session.created"

// OutboxEvent is a durable promise that something happens elsewhere.
//
// It is written in the same transaction as the thing that caused it, which is
// the entire mechanism: writing the session and then publishing leaves a
// window where the session exists and nothing will ever pick it up, and that
// window is open exactly when the process dies.
type OutboxEvent struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	Topic       string
	SubjectID   uuid.UUID
	Payload     []byte
	// Attempts is read back from the store. It is not set by a caller: a row
	// is enqueued having been tried zero times.
	Attempts int32
}

// SessionRepository is the persistence port for sessions.
type SessionRepository interface {
	// Create writes the session, its participant, its first transition, the
	// audit event and the outbox record in one transaction. All of it or none
	// of it — a session with no promise attached is a session nothing will
	// ever start.
	//
	// verifyTask is called with the task row locked inside that transaction.
	// The check cannot be done above this: a task read in an earlier
	// transaction can be patched back to a draft, or pointed at a different
	// repository, before this one takes its locks — after which a queued
	// session would reference a task that is no longer runnable, or carry a
	// branch intent for a repository the task has moved off.
	Create(
		ctx context.Context,
		session domain.Session,
		participant domain.SessionParticipant,
		transition domain.SessionStateTransition,
		event OutboxEvent,
		verifyTask func(domain.Task) error,
		actor Actor,
		audit AuditEvent,
	) (domain.Session, error)
	// Transition locks the session, hands it to decide, and writes the new
	// state and the history row together. decide carries the domain rules —
	// which pairs are legal and whether the observed version is current — and
	// runs with the row locked, which is what makes "the version has not
	// moved" true at the moment it is acted on.
	Transition(
		ctx context.Context,
		sessionID, workspaceID uuid.UUID,
		decide func(domain.Session) (domain.SessionStateTransition, error),
		actor Actor,
		audit func(domain.Session) AuditEvent,
	) (domain.Session, error)
	Get(ctx context.Context, sessionID, workspaceID uuid.UUID) (domain.Session, error)
	List(ctx context.Context, workspaceID uuid.UUID) ([]domain.Session, error)
	ListParticipants(ctx context.Context, sessionID, workspaceID uuid.UUID) ([]domain.SessionParticipant, error)
	ListTransitions(ctx context.Context, sessionID, workspaceID uuid.UUID) ([]domain.SessionStateTransition, error)
}

// SessionTaskReader reads the task a session is created from.
//
// Narrower than TaskRepository on purpose: this service needs to read one task
// and must not be able to write one.
type SessionTaskReader interface {
	Get(ctx context.Context, taskID, workspaceID uuid.UUID) (domain.Task, error)
}

// SessionAgentReader reads the agent version a session pins.
type SessionAgentReader interface {
	GetVersion(ctx context.Context, versionID, workspaceID uuid.UUID) (domain.AgentVersion, error)
}

// SessionService creates sessions.
type SessionService struct {
	sessions SessionRepository
	tasks    SessionTaskReader
	agents   SessionAgentReader
}

// NewSessionService wires the service.
func NewSessionService(
	sessions SessionRepository,
	tasks SessionTaskReader,
	agents SessionAgentReader,
) *SessionService {
	return &SessionService{sessions: sessions, tasks: tasks, agents: agents}
}

// sessionPermission governs creating a session.
//
// `session:create`, which already exists and already means this — it is the
// permission the matrix was written with, and the one M4.1 reused for tasks on
// the argument that a task is the input to a session. This is the unit that
// makes that argument testable rather than asserted.
const sessionPermission = domain.PermissionSessionCreate

// CreateSessionCommand is a request to start a session.
type CreateSessionCommand struct {
	TaskID         uuid.UUID
	AgentVersionID uuid.UUID
	// BaseBranch is optional. Empty means the repository's default, resolved
	// by the workflow against GitHub rather than here: the default can change
	// between this row and the branch being cut, and the workflow is the thing
	// holding a token.
	BaseBranch string
}

// Create records a session and the promise that something will run it.
//
// Ordering is worth stating, because it is the safety of the operation:
//
//  1. Permission, before anything is read.
//  2. Read the task, which must be `ready` — `ready` is what guarantees a
//     repository, and the branch intent has nowhere to point without one.
//  3. Read the agent version, so a session cannot pin one that does not exist
//     or belongs to another workspace.
//  4. Derive the branch name from the session id, so the workflow retrying an
//     activity finds the branch it already made rather than cutting a second.
//  5. Write everything in one transaction, including the outbox row.
//
// Nothing here talks to GitHub. Creating the branch is step 3 of the session
// execution sequence in context/architecture.md and belongs to the workflow in
// M5 — a network call inside this transaction would break invariant 2 and
// would leave a branch behind whenever the transaction rolled back.
func (s *SessionService) Create(
	ctx context.Context,
	membership domain.Membership,
	command CreateSessionCommand,
) (domain.Session, error) {
	if !membership.Can(sessionPermission) {
		return domain.Session{}, fmt.Errorf("%w: %s requires %s",
			ErrPermissionDenied, membership.Role, sessionPermission)
	}

	task, err := s.tasks.Get(ctx, command.TaskID, membership.WorkspaceID)
	if err != nil {
		return domain.Session{}, err
	}
	if task.Status != domain.TaskReady {
		return domain.Session{}, fmt.Errorf("%w: this task is %s",
			domain.ErrTaskNotRunnable, task.Status)
	}
	// A ready task always names a repository — the database enforces it with
	// tasks_ready_has_repository — so this cannot fire. It is checked anyway
	// because the alternative is dereferencing a pointer on the strength of a
	// constraint in another file.
	if task.RepositoryID == nil {
		return domain.Session{}, fmt.Errorf("%w: it names no repository", domain.ErrTaskNotRunnable)
	}

	version, err := s.agents.GetVersion(ctx, command.AgentVersionID, membership.WorkspaceID)
	if err != nil {
		return domain.Session{}, err
	}

	sessionID, err := domain.NewSessionID()
	if err != nil {
		return domain.Session{}, err
	}
	branchName, err := domain.SessionBranchName(sessionID, task.Title)
	if err != nil {
		return domain.Session{}, err
	}
	transitionID, err := domain.NewTransitionID()
	if err != nil {
		return domain.Session{}, err
	}
	eventID, err := domain.NewOutboxEventID()
	if err != nil {
		return domain.Session{}, err
	}

	session := domain.Session{
		ID:             sessionID,
		WorkspaceID:    membership.WorkspaceID,
		TaskID:         task.ID,
		AgentVersionID: version.ID,
		// Created straight into `queued`. `draft` is for a session being
		// composed, and this one is already complete: it names its task, its
		// agent version and the branch it will use.
		State:        domain.SessionQueued,
		RepositoryID: *task.RepositoryID,
		BranchName:   branchName,
		BaseBranch:   command.BaseBranch,
		CreatedBy:    membership.UserID,
	}

	participant := domain.SessionParticipant{
		SessionID:   sessionID,
		WorkspaceID: membership.WorkspaceID,
		UserID:      membership.UserID,
		Capacity:    domain.ParticipantOwner,
	}

	// The first transition has no previous state. A session comes into
	// existence already queued, and recording a move from somewhere would be a
	// fiction in a trail that cannot be corrected by editing.
	transition := domain.SessionStateTransition{
		ID:              transitionID,
		SessionID:       sessionID,
		WorkspaceID:     membership.WorkspaceID,
		NextState:       domain.SessionQueued,
		ObservedVersion: 1,
		Reason:          "session created",
		ActorUserID:     &membership.UserID,
	}

	// The payload carries identifiers and nothing else. The consumer reads the
	// session row for the rest, so a payload cannot go stale against it — and
	// a task title copied in here would be untrusted input travelling through
	// a queue into a workflow.
	payload := []byte(fmt.Sprintf(
		`{"session_id":%q,"workspace_id":%q}`, sessionID, membership.WorkspaceID))

	event := OutboxEvent{
		ID:          eventID,
		WorkspaceID: membership.WorkspaceID,
		Topic:       TopicSessionCreated,
		SubjectID:   sessionID,
		Payload:     payload,
	}

	// Re-checked against the locked row, not against the one read above.
	//
	// The read that produced `task` happened in its own transaction, and the
	// task PATCH route can commit between the two — this is the same shape as
	// the lost update M4.1 fixed in TaskService.Update, arriving here in a
	// different disguise. The closure runs with the row locked, so what it
	// sees is what the session is about to reference.
	verifyTask := func(locked domain.Task) error {
		if locked.Status != domain.TaskReady {
			return fmt.Errorf("%w: this task is %s", domain.ErrTaskNotRunnable, locked.Status)
		}
		// The repository is checked as well as the status, because the branch
		// intent was derived from the earlier read. A task repointed at
		// another repository would leave the session naming a branch to be cut
		// somewhere the task no longer concerns.
		if locked.RepositoryID == nil || *locked.RepositoryID != session.RepositoryID {
			return fmt.Errorf("%w: its repository changed while the session was being created",
				domain.ErrTaskNotRunnable)
		}
		return nil
	}

	// The audit detail names the task and the agent version, not the task
	// body: an audit trail is read by operators and machinery that was not
	// written expecting arbitrary untrusted text.
	return s.sessions.Create(ctx, session, participant, transition, event, verifyTask,
		Actor{UserID: membership.UserID, Required: sessionPermission},
		AuditEvent{
			WorkspaceID: membership.WorkspaceID,
			ActorUserID: membership.UserID,
			Action:      AuditSessionCreated,
			Target:      sessionID.String(),
			Detail: map[string]any{
				"task_id":          task.ID.String(),
				"agent_version_id": version.ID.String(),
				"branch_name":      branchName,
			},
		})
}

// TransitionSessionCommand is a request to move a session to another state.
type TransitionSessionCommand struct {
	SessionID uuid.UUID
	To        domain.SessionState
	// ObservedVersion is the version the caller read. The move is refused if
	// the session has changed since — two callers acting on the same reading
	// must not both succeed.
	ObservedVersion int32
	Reason          string
}

// Transition moves a session, refusing an illegal move or a stale reading.
//
// `session:control` rather than `session:create`: pausing, resuming and
// cancelling are what this permission was written for, and the matrix already
// separates the two. Nothing routes to this in M4.2 — M5's workflow is the
// first caller — but the rules live here rather than in the workflow, because
// a state machine enforced by its callers is enforced by whoever remembers to.
func (s *SessionService) Transition(
	ctx context.Context,
	membership domain.Membership,
	command TransitionSessionCommand,
) (domain.Session, error) {
	if !membership.Can(domain.PermissionSessionControl) {
		return domain.Session{}, fmt.Errorf("%w: %s requires %s",
			ErrPermissionDenied, membership.Role, domain.PermissionSessionControl)
	}

	reason, err := domain.ValidateTransitionReason(command.Reason)
	if err != nil {
		return domain.Session{}, err
	}
	if !command.To.Valid() {
		return domain.Session{}, fmt.Errorf("%w: unknown state %q", domain.ErrInvalidSession, command.To)
	}

	transitionID, err := domain.NewTransitionID()
	if err != nil {
		return domain.Session{}, err
	}

	decide := func(current domain.Session) (domain.SessionStateTransition, error) {
		// Version first. A caller acting on a stale reading may be asking for
		// a move that is legal from the state they saw and wrong from the
		// state the session is actually in, so refusing on the version says
		// what went wrong more usefully than refusing on the pair would.
		if current.Version != command.ObservedVersion {
			return domain.SessionStateTransition{}, fmt.Errorf(
				"%w: you read version %d and it is now %d",
				domain.ErrSessionVersionConflict, command.ObservedVersion, current.Version)
		}
		// Terminal states need no special case: the table gives them no
		// outgoing edges at all, which is invariant 10 stated as data rather
		// than as a rule someone has to remember to check.
		if !domain.CanTransition(current.State, command.To) {
			return domain.SessionStateTransition{}, fmt.Errorf(
				"%w: %s cannot become %s",
				domain.ErrTransitionNotAllowed, current.State, command.To)
		}
		return domain.SessionStateTransition{
			ID:              transitionID,
			SessionID:       command.SessionID,
			WorkspaceID:     membership.WorkspaceID,
			NextState:       command.To,
			ObservedVersion: command.ObservedVersion,
			Reason:          reason,
			ActorUserID:     &membership.UserID,
		}, nil
	}

	return s.sessions.Transition(ctx, command.SessionID, membership.WorkspaceID, decide,
		Actor{UserID: membership.UserID, Required: domain.PermissionSessionControl},
		func(moved domain.Session) AuditEvent {
			return AuditEvent{
				WorkspaceID: membership.WorkspaceID,
				ActorUserID: membership.UserID,
				Action:      AuditSessionTransitioned,
				Target:      command.SessionID.String(),
				Detail: map[string]any{
					"to":      string(moved.State),
					"version": moved.Version,
				},
			}
		})
}

// Get returns one session.
//
// Reads are governed by readPermission, for the reasoning recorded beside it
// in tasks.go.
func (s *SessionService) Get(
	ctx context.Context,
	membership domain.Membership,
	sessionID uuid.UUID,
) (domain.Session, error) {
	if !membership.Can(readPermission) {
		return domain.Session{}, fmt.Errorf("%w: %s requires %s",
			ErrPermissionDenied, membership.Role, readPermission)
	}
	return s.sessions.Get(ctx, sessionID, membership.WorkspaceID)
}

// List returns a workspace's sessions.
func (s *SessionService) List(
	ctx context.Context,
	membership domain.Membership,
) ([]domain.Session, error) {
	if !membership.Can(readPermission) {
		return nil, fmt.Errorf("%w: %s requires %s",
			ErrPermissionDenied, membership.Role, readPermission)
	}
	return s.sessions.List(ctx, membership.WorkspaceID)
}

// ListTransitions returns a session's history, oldest first.
func (s *SessionService) ListTransitions(
	ctx context.Context,
	membership domain.Membership,
	sessionID uuid.UUID,
) ([]domain.SessionStateTransition, error) {
	if !membership.Can(readPermission) {
		return nil, fmt.Errorf("%w: %s requires %s",
			ErrPermissionDenied, membership.Role, readPermission)
	}
	// Read the session first, so asking for the history of another
	// workspace's session is a not-found rather than an empty list — an empty
	// list would say the session exists and has no history.
	if _, err := s.sessions.Get(ctx, sessionID, membership.WorkspaceID); err != nil {
		return nil, err
	}
	return s.sessions.ListTransitions(ctx, sessionID, membership.WorkspaceID)
}

// ListParticipants returns who is in a session.
func (s *SessionService) ListParticipants(
	ctx context.Context,
	membership domain.Membership,
	sessionID uuid.UUID,
) ([]domain.SessionParticipant, error) {
	if !membership.Can(readPermission) {
		return nil, fmt.Errorf("%w: %s requires %s",
			ErrPermissionDenied, membership.Role, readPermission)
	}
	if _, err := s.sessions.Get(ctx, sessionID, membership.WorkspaceID); err != nil {
		return nil, err
	}
	return s.sessions.ListParticipants(ctx, sessionID, membership.WorkspaceID)
}
