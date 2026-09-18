package temporal

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"go.temporal.io/sdk/temporal"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Activity names, registered and referenced by the workflow. Stable strings: a
// running workflow resumes by them.
const (
	ActivityMarkProvisioning    = "MarkSessionProvisioning"
	ActivityFailUnprovisionable = "FailSessionUnprovisionable"
)

// SessionActivities moves a session through its states on the workflow's
// behalf.
//
// Everything that touches the database lives here rather than in the workflow,
// because workflow code is replayed and must be deterministic.
type SessionActivities struct {
	sessions *application.SessionService
}

// NewSessionActivities wires the activities.
func NewSessionActivities(sessions *application.SessionService) *SessionActivities {
	return &SessionActivities{sessions: sessions}
}

// MarkProvisioning moves a queued session to provisioning.
func (a *SessionActivities) MarkProvisioning(ctx context.Context, input SessionWorkflowInput) error {
	return a.transition(ctx, input, domain.SessionProvisioning,
		"the session workflow began provisioning")
}

// FailUnprovisionable ends a session that cannot be provisioned yet.
//
// The reason names what is missing rather than saying "failed", because a
// person reading a session's history should not have to know which milestone
// they are looking at to understand why it stopped.
func (a *SessionActivities) FailUnprovisionable(ctx context.Context, input SessionWorkflowInput) error {
	return a.transition(ctx, input, domain.SessionFailed,
		"no runner exists yet to provision this session")
}

// transition applies one state change as the system.
//
// **Idempotent against redelivery, which is the whole reason it is not a
// direct call to the store.** An activity that commits and then loses its
// acknowledgement is retried by Temporal; replaying the transition would find
// the session already moved and take the version conflict for a failure, when
// the work in fact succeeded. `TransitionAsSystem` treats a session already in
// the target state as success and writes no second transition.
//
// Errors are classified here rather than by string-matching in the workflow.
// A session that does not exist, or a move the table forbids, will not become
// possible by waiting — so they are non-retryable, and the workflow fails fast
// instead of spending its retry budget on them.
func (a *SessionActivities) transition(
	ctx context.Context,
	input SessionWorkflowInput,
	to domain.SessionState,
	reason string,
) error {
	workspaceID, err := uuid.Parse(input.WorkspaceID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(
			"workspace id is not a uuid", ErrorTypeNotFound, err)
	}
	sessionID, err := uuid.Parse(input.SessionID)
	if err != nil {
		return temporal.NewNonRetryableApplicationError(
			"session id is not a uuid", ErrorTypeNotFound, err)
	}

	tenant := postgresTenant(ctx, workspaceID)
	if _, err := a.sessions.TransitionAsSystem(tenant, workspaceID, sessionID, to, reason); err != nil {
		switch {
		case errors.Is(err, application.ErrSessionNotFound):
			return temporal.NewNonRetryableApplicationError(
				"the session no longer exists", ErrorTypeNotFound, err)
		case errors.Is(err, domain.ErrTransitionNotAllowed):
			return temporal.NewNonRetryableApplicationError(
				fmt.Sprintf("the session cannot become %s", to), ErrorTypeTransitionNotAllowed, err)
		default:
			// Everything else is assumed transient — a database briefly
			// unreachable is the common case — and the retry policy bounds how
			// long that assumption is allowed to hold.
			return err
		}
	}
	return nil
}
