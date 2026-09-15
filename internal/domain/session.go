package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Errors the session domain raises.
var (
	// ErrInvalidSession is returned when input fails a session invariant.
	ErrInvalidSession = errors.New("invalid session")
	// ErrTaskNotRunnable is returned when a session is asked for from a task
	// that is not ready. A ready task is one that names a repository, which is
	// what the branch intent needs.
	ErrTaskNotRunnable = errors.New("a session can only be created from a ready task")
	// ErrTransitionNotAllowed is returned when a state move is not in the
	// transition table.
	ErrTransitionNotAllowed = errors.New("state transition is not allowed")
	// ErrSessionVersionConflict is returned when a transition observed a
	// version the session has since moved past.
	ErrSessionVersionConflict = errors.New("session has changed since it was read")
)

// SessionState is where a session sits in its lifecycle.
type SessionState string

// The sixteen states from context/architecture.md. The document is the
// authority on this list; it is reproduced here because the transition table
// below is the only artifact that says what they mean.
const (
	// SessionDraft is being composed and has not been submitted.
	SessionDraft SessionState = "draft"
	// SessionQueued is waiting for a workflow to pick it up. This is where a
	// created session starts.
	SessionQueued SessionState = "queued"
	// SessionProvisioning is having its branch cut and its runner built.
	SessionProvisioning SessionState = "provisioning"
	// SessionRunning has an agent working.
	SessionRunning SessionState = "running"
	// SessionWaitingForInput needs something from a person before it can go on.
	SessionWaitingForInput SessionState = "waiting_for_input"
	// SessionWaitingForApproval has proposed an action someone must approve.
	SessionWaitingForApproval SessionState = "waiting_for_approval"
	// SessionPausing has been asked to stop and has not stopped yet.
	SessionPausing SessionState = "pausing"
	// SessionPaused is stopped and resumable.
	SessionPaused SessionState = "paused"
	// SessionResuming is starting again after a pause.
	SessionResuming SessionState = "resuming"
	// SessionReviewReady has work a person is expected to look at.
	SessionReviewReady SessionState = "review_ready"
	// SessionFinalizing is producing its commit, pull request and summary.
	SessionFinalizing SessionState = "finalizing"
	// SessionCompleted finished. Terminal.
	SessionCompleted SessionState = "completed"
	// SessionCancelling has been asked to stop for good.
	SessionCancelling SessionState = "cancelling"
	// SessionCancelled was stopped by someone. Terminal.
	SessionCancelled SessionState = "cancelled"
	// SessionFailed stopped because something went wrong. Terminal.
	SessionFailed SessionState = "failed"
	// SessionExpired ran out of its allotted time. Terminal.
	SessionExpired SessionState = "expired"
)

// SessionStates is every state, in lifecycle order rather than alphabetical.
//
// Order matters only for display and for the exhaustiveness test's output.
var SessionStates = []SessionState{
	SessionDraft, SessionQueued, SessionProvisioning, SessionRunning,
	SessionWaitingForInput, SessionWaitingForApproval,
	SessionPausing, SessionPaused, SessionResuming,
	SessionReviewReady, SessionFinalizing, SessionCompleted,
	SessionCancelling, SessionCancelled, SessionFailed, SessionExpired,
}

// Valid reports whether a state is one the product recognises.
func (s SessionState) Valid() bool {
	for _, known := range SessionStates {
		if s == known {
			return true
		}
	}
	return false
}

func (s SessionState) String() string { return string(s) }

// Terminal reports whether a session in this state is finished for good.
//
// Invariant 10: terminal session history is immutable, and continuation
// creates a linked new session rather than reopening this one. The transition
// table states the same thing as data — every terminal row is entirely false.
func (s SessionState) Terminal() bool {
	switch s {
	case SessionCompleted, SessionCancelled, SessionFailed, SessionExpired:
		return true
	default:
		return false
	}
}

// ParseSessionState validates a state from a caller or a database row.
func ParseSessionState(value string) (SessionState, error) {
	state := SessionState(strings.TrimSpace(value))
	if !state.Valid() {
		return "", fmt.Errorf("%w: unknown state %q", ErrInvalidSession, value)
	}
	return state, nil
}

// sessionTransitions is the transition table: the single place a state implies
// anything about what may follow it.
//
// Written out in full, including every denial as `false`, for the reason the
// authorization matrix is: an omission denies at runtime but records no
// decision, and a reader cannot tell a considered "no" from a forgotten one.
// TestSessionTransitionsAreExhaustive fails the build when a state is added
// without every pairing being answered in both directions.
//
// Only one edge is exercised in M4.2 — a session is created directly into
// `queued`, so nothing here is reached yet. The table is built now because it
// is the only artifact that says what the sixteen states mean, and because
// discovering in M6 that two states have no defined relationship is the
// expensive version of this work.
var sessionTransitions = map[SessionState]map[SessionState]bool{
	// A draft is submitted or abandoned. It has never run, so it cannot fail.
	SessionDraft: {
		SessionDraft: false, SessionQueued: true, SessionProvisioning: false, SessionRunning: false,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: false, SessionPaused: false, SessionResuming: false,
		SessionReviewReady: false, SessionFinalizing: false, SessionCompleted: false,
		SessionCancelling: false, SessionCancelled: true, SessionFailed: false, SessionExpired: true,
	},
	// Queued work is picked up, cancelled outright — nothing is running, so
	// there is nothing to wind down — or fails before it starts.
	SessionQueued: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: true, SessionRunning: false,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: false, SessionPaused: false, SessionResuming: false,
		SessionReviewReady: false, SessionFinalizing: false, SessionCompleted: false,
		SessionCancelling: false, SessionCancelled: true, SessionFailed: true, SessionExpired: true,
	},
	// Provisioning holds external resources, so stopping it goes through
	// `cancelling` rather than straight to `cancelled`.
	SessionProvisioning: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: false, SessionRunning: true,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: false, SessionPaused: false, SessionResuming: false,
		SessionReviewReady: false, SessionFinalizing: false, SessionCompleted: false,
		SessionCancelling: true, SessionCancelled: false, SessionFailed: true, SessionExpired: true,
	},
	// The busiest state: it can need input, need an approval, be paused, reach
	// something worth reviewing, be cancelled, fail or expire.
	SessionRunning: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: false, SessionRunning: false,
		SessionWaitingForInput: true, SessionWaitingForApproval: true,
		SessionPausing: true, SessionPaused: false, SessionResuming: false,
		SessionReviewReady: true, SessionFinalizing: false, SessionCompleted: false,
		SessionCancelling: true, SessionCancelled: false, SessionFailed: true, SessionExpired: true,
	},
	// Waiting states go back to running when answered. They can be paused —
	// someone may want to leave a question open overnight without holding a
	// runner — and they can expire, which is what makes an unanswered session
	// stop costing money.
	SessionWaitingForInput: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: false, SessionRunning: true,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: true, SessionPaused: false, SessionResuming: false,
		SessionReviewReady: false, SessionFinalizing: false, SessionCompleted: false,
		SessionCancelling: true, SessionCancelled: false, SessionFailed: true, SessionExpired: true,
	},
	SessionWaitingForApproval: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: false, SessionRunning: true,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: true, SessionPaused: false, SessionResuming: false,
		SessionReviewReady: false, SessionFinalizing: false, SessionCompleted: false,
		SessionCancelling: true, SessionCancelled: false, SessionFailed: true, SessionExpired: true,
	},
	// Pausing is in-flight: it lands on paused, or fails, or a cancellation
	// overtakes it. It cannot go back to running — resuming is a separate
	// state precisely so a resume in progress is visible.
	SessionPausing: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: false, SessionRunning: false,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: false, SessionPaused: true, SessionResuming: false,
		SessionReviewReady: false, SessionFinalizing: false, SessionCompleted: false,
		SessionCancelling: true, SessionCancelled: false, SessionFailed: true, SessionExpired: true,
	},
	// A paused session holds no runner, so cancelling it is immediate.
	SessionPaused: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: false, SessionRunning: false,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: false, SessionPaused: false, SessionResuming: true,
		SessionReviewReady: false, SessionFinalizing: false, SessionCompleted: false,
		SessionCancelling: false, SessionCancelled: true, SessionFailed: false, SessionExpired: true,
	},
	SessionResuming: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: false, SessionRunning: true,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: false, SessionPaused: false, SessionResuming: false,
		SessionReviewReady: false, SessionFinalizing: false, SessionCompleted: false,
		SessionCancelling: true, SessionCancelled: false, SessionFailed: true, SessionExpired: true,
	},
	// Review-ready goes forward to delivery or back to running: the revision
	// loop in M7 is a reviewer asking for changes, which is this edge.
	SessionReviewReady: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: false, SessionRunning: true,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: true, SessionPaused: false, SessionResuming: false,
		SessionReviewReady: false, SessionFinalizing: true, SessionCompleted: false,
		SessionCancelling: true, SessionCancelled: false, SessionFailed: true, SessionExpired: true,
	},
	// Finalizing is writing the commit and the pull request. It is not
	// cancellable: stopping half way through delivery would leave a customer's
	// repository in a state nobody asked for, and the work is short.
	SessionFinalizing: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: false, SessionRunning: false,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: false, SessionPaused: false, SessionResuming: false,
		SessionReviewReady: false, SessionFinalizing: false, SessionCompleted: true,
		SessionCancelling: false, SessionCancelled: false, SessionFailed: true, SessionExpired: false,
	},
	// Cancelling is winding down. It can still fail — tearing down a runner
	// can go wrong, and recording that as a clean cancellation would be false.
	SessionCancelling: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: false, SessionRunning: false,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: false, SessionPaused: false, SessionResuming: false,
		SessionReviewReady: false, SessionFinalizing: false, SessionCompleted: false,
		SessionCancelling: false, SessionCancelled: true, SessionFailed: true, SessionExpired: false,
	},

	// The four terminal states. Every pairing is false, which is invariant 10
	// stated as data rather than as a rule someone has to remember: terminal
	// history is immutable, and reopening creates a linked continuation.
	SessionCompleted: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: false, SessionRunning: false,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: false, SessionPaused: false, SessionResuming: false,
		SessionReviewReady: false, SessionFinalizing: false, SessionCompleted: false,
		SessionCancelling: false, SessionCancelled: false, SessionFailed: false, SessionExpired: false,
	},
	SessionCancelled: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: false, SessionRunning: false,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: false, SessionPaused: false, SessionResuming: false,
		SessionReviewReady: false, SessionFinalizing: false, SessionCompleted: false,
		SessionCancelling: false, SessionCancelled: false, SessionFailed: false, SessionExpired: false,
	},
	SessionFailed: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: false, SessionRunning: false,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: false, SessionPaused: false, SessionResuming: false,
		SessionReviewReady: false, SessionFinalizing: false, SessionCompleted: false,
		SessionCancelling: false, SessionCancelled: false, SessionFailed: false, SessionExpired: false,
	},
	SessionExpired: {
		SessionDraft: false, SessionQueued: false, SessionProvisioning: false, SessionRunning: false,
		SessionWaitingForInput: false, SessionWaitingForApproval: false,
		SessionPausing: false, SessionPaused: false, SessionResuming: false,
		SessionReviewReady: false, SessionFinalizing: false, SessionCompleted: false,
		SessionCancelling: false, SessionCancelled: false, SessionFailed: false, SessionExpired: false,
	},
}

// CanTransition reports whether a session may move from one state to another.
func CanTransition(from, to SessionState) bool {
	return sessionTransitions[from][to]
}

// NextStates returns the states a session may move to, in lifecycle order.
//
// The UI derives what it offers from this rather than from a list of its own,
// for the reason the permission list is served rather than reimplemented in
// the browser: two copies of a rule disagree eventually.
func NextStates(from SessionState) []SessionState {
	allowed := make([]SessionState, 0, len(SessionStates))
	for _, state := range SessionStates {
		if sessionTransitions[from][state] {
			allowed = append(allowed, state)
		}
	}
	return allowed
}

// SessionParticipantCapacity is how someone takes part in a session.
type SessionParticipantCapacity string

// The capacities a participant may hold. Deliberately not the workspace roles:
// this says what someone is doing in this session, not what they may do in the
// workspace, and conflating them would put two meanings on one word.
const (
	// ParticipantOwner started the session.
	ParticipantOwner SessionParticipantCapacity = "owner"
	// ParticipantCollaborator joined to take part.
	ParticipantCollaborator SessionParticipantCapacity = "collaborator"
	// ParticipantObserver is watching.
	ParticipantObserver SessionParticipantCapacity = "observer"
)

// Session is a run of an agent against a task.
type Session struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	TaskID      uuid.UUID
	// AgentVersionID pins the settings this session runs under. The version is
	// immutable, which is what makes the pin a policy snapshot on its own.
	AgentVersionID uuid.UUID
	State          SessionState
	Version        int32

	// The branch intent. Nothing has created this branch when the session is
	// written; M5's workflow does, and finds it already there on a retry
	// because the name is a function of the session id.
	RepositoryID uuid.UUID
	BranchName   string
	// BaseBranch empty means the repository's default, resolved by the
	// workflow rather than here.
	BaseBranch string

	// ContinuesID links a continuation to the terminal session it follows.
	ContinuesID *uuid.UUID

	CreatedBy uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
}

// SessionParticipant is someone taking part in a session.
type SessionParticipant struct {
	SessionID   uuid.UUID
	WorkspaceID uuid.UUID
	UserID      uuid.UUID
	Capacity    SessionParticipantCapacity
	CreatedAt   time.Time
}

// SessionStateTransition is one move, recorded.
type SessionStateTransition struct {
	ID          uuid.UUID
	SessionID   uuid.UUID
	WorkspaceID uuid.UUID
	// PreviousState is absent on the first transition. A session comes into
	// existence already in a state, and recording a move it did not make would
	// be a fiction in an append-only trail.
	PreviousState *SessionState
	NextState     SessionState
	// ObservedVersion is the session version this move was made against.
	ObservedVersion int32
	Reason          string
	// ActorUserID is absent when the system moved the session — a timeout is
	// not attributable to a person.
	ActorUserID *uuid.UUID
	CreatedAt   time.Time
}

// NewSessionID returns an identifier for a new session.
func NewSessionID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate session id: %w", err)
	}
	return id, nil
}

// NewTransitionID returns an identifier for a state transition.
func NewTransitionID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate transition id: %w", err)
	}
	return id, nil
}

// NewOutboxEventID returns an identifier for an outbox record.
func NewOutboxEventID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate outbox event id: %w", err)
	}
	return id, nil
}

// transitionReasonMaxLen matches the CHECK constraint in the migration.
const transitionReasonMaxLen = 500

// ValidateTransitionReason bounds the human-readable reason a move carries.
func ValidateTransitionReason(reason string) (string, error) {
	trimmed := strings.TrimSpace(reason)
	if len([]rune(trimmed)) > transitionReasonMaxLen {
		return "", fmt.Errorf("%w: a reason may be at most %d characters",
			ErrInvalidSession, transitionReasonMaxLen)
	}
	return trimmed, nil
}
