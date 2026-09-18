// Package temporal holds the durable session workflow and the client that
// starts it.
//
// Workflow code is deterministic and replayed: it must not read a clock, call
// a random source, or touch the network directly. Everything that does lives
// in an activity.
package temporal

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// TaskQueue is where session workflows and their activities are scheduled.
const TaskQueue = "weave-sessions"

// SessionWorkflowName is the registered name, stable because a running
// workflow is resumed by it.
const SessionWorkflowName = "SessionWorkflow"

// SessionWorkflowInput is what starting a session workflow requires.
//
// Identifiers only. The workflow reads what it needs through activities, so a
// field copied in here could go stale against the row it came from — and the
// payload travels through a queue into replayed code, where a task title would
// be untrusted input arriving somewhere it has no business being.
type SessionWorkflowInput struct {
	WorkspaceID string `json:"workspace_id"`
	SessionID   string `json:"session_id"`
}

// activityOptions are shared by every activity in this workflow.
//
// Bounded retries with backoff, a start-to-close timeout, and non-retryable
// classes, as `context/code-standards.md` requires. The non-retryable list is
// the important half: a session whose task was archived will never provision,
// and a workflow retrying that for a minute before failing helps nobody.
func activityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    5,
			NonRetryableErrorTypes: []string{
				ErrorTypeNotFound,
				ErrorTypeTransitionNotAllowed,
			},
		},
	}
}

// Error types an activity can raise that retrying cannot fix.
const (
	ErrorTypeNotFound             = "SessionNotFound"
	ErrorTypeTransitionNotAllowed = "TransitionNotAllowed"
)

// SessionWorkflow takes a queued session as far as this milestone can.
//
// `queued → provisioning → failed`, because there is no runner yet and the
// honest end is to say so. It does not return to `queued`: the transition
// table forbids that edge — `provisioning` may become `running`, `cancelling`,
// `failed` or `expired` — and adding one to make a first workflow tidy would
// be changing a state machine to suit a demo.
//
// M5.4 replaces the second step with provisioning a runner. The first step and
// the failure path stay.
func SessionWorkflow(ctx workflow.Context, input SessionWorkflowInput) error {
	ctx = workflow.WithActivityOptions(ctx, activityOptions())
	logger := workflow.GetLogger(ctx)

	// Versioned from the first edit rather than the first problem.
	// `context/code-standards.md` requires workflow code to be safely
	// versionable while sessions are in flight, and that is far easier to
	// honour now — with one workflow and nothing running — than to retrofit.
	_ = workflow.GetVersion(ctx, "session-workflow", workflow.DefaultVersion, 1)

	logger.Info("provisioning a session", "session_id", input.SessionID)

	if err := workflow.ExecuteActivity(ctx, ActivityMarkProvisioning, input).Get(ctx, nil); err != nil {
		return err
	}

	// The runner arrives in M5.4. Until then the session fails here, with a
	// reason that says why rather than leaving it in `provisioning` for
	// someone to find.
	return workflow.ExecuteActivity(ctx, ActivityFailUnprovisionable, input).Get(ctx, nil)
}
