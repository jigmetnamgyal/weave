// Package temporal holds the durable session workflow and the client that
// starts it.
//
// Workflow code is deterministic and replayed: it must not read a clock, call
// a random source, or touch the network directly. Everything that does lives
// in an activity.
package temporal

import (
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/jigmetnamgyal/weave/internal/application"
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
				ErrorTypeBranchRefused,
			},
		},
	}
}

// recordActivityOptions govern recording a branch GitHub has already made.
//
// Far more patient than activityOptions, and on purpose. By the time this
// runs the ref exists and its SHA is in workflow history; the only thing that
// can fail is the database, and giving up after five tries would end the
// session claiming GitHub was unreachable while a real branch sat unrecorded.
// So attempts are unlimited and the whole step is bounded by time instead: an
// hour covers any outage this system is meant to ride out, and past that the
// session is failed with a reason that says the branch exists.
func recordActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout:    30 * time.Second,
		ScheduleToCloseTimeout: time.Hour,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    0,
			NonRetryableErrorTypes: []string{
				ErrorTypeNotFound,
				ErrorTypeBranchRefused,
			},
		},
	}
}

// Error types an activity can raise that retrying cannot fix.
const (
	ErrorTypeNotFound             = "SessionNotFound"
	ErrorTypeTransitionNotAllowed = "TransitionNotAllowed"
	// ErrorTypeBranchRefused carries an application.BranchFailure code as its
	// detail: GitHub refused the branch for a reason a person has to fix.
	ErrorTypeBranchRefused = "BranchRefused"
)

// Workflow versions, for workflow.GetVersion.
//
// A running execution replays against the code that is deployed, so a change
// to the sequence of activities must be gated on the version the execution
// recorded when it first reached this point — otherwise one that began without
// a branch step would meet one mid-replay and fail non-deterministically.
const (
	sessionWorkflowChange = "session-workflow"
	// Version 1 was M5.1: provisioning, then failure.
	//
	// versionBranchStep is M5.2: provisioning, the branch, then failure.
	//
	// Its shape changed once during review — cutting and recording were split
	// into two activities — without a new version, because version 2 had
	// never shipped: every execution that ran it was a local one, and all of
	// them had closed. A closed execution is never replayed.
	versionBranchStep workflow.Version = 2
)

// SessionWorkflow takes a queued session as far as this milestone can.
//
// `queued → provisioning → (cut the branch) → failed`, because there is no
// runner yet and the honest end is to say so. The branch is cut *during*
// `provisioning` rather than as a state of its own: it is part of getting
// ready to run, and a state added to make this tidy would be changing the
// state machine to suit a demo. It does not return to `queued` either — the
// transition table forbids that edge.
//
// The branch survives the failure. M5.4 replaces the last step with
// provisioning a runner; the steps before it stay.
func SessionWorkflow(ctx workflow.Context, input SessionWorkflowInput) error {
	ctx = workflow.WithActivityOptions(ctx, activityOptions())
	logger := workflow.GetLogger(ctx)

	// Versioned from the first edit rather than the first problem, as
	// `context/code-standards.md` requires. An execution that recorded
	// version 1 finishes on the path it started — no branch step — and every
	// new one takes version 2.
	version := workflow.GetVersion(ctx, sessionWorkflowChange, workflow.DefaultVersion, versionBranchStep)

	logger.Info("provisioning a session", "session_id", input.SessionID)

	if err := workflow.ExecuteActivity(ctx, ActivityMarkProvisioning, input).Get(ctx, nil); err != nil {
		return err
	}

	if version >= versionBranchStep {
		var branch BranchResult
		if err := workflow.ExecuteActivity(ctx, ActivityCreateBranch, input).Get(ctx, &branch); err != nil {
			return failForBranch(ctx, input, err, string(application.BranchFailureUnreachable))
		}

		// From here the ref exists and `branch` — in history — is the durable
		// record of it. A failure to write it down is not GitHub being
		// unreachable, and must not be reported as one.
		recordCtx := workflow.WithActivityOptions(ctx, recordActivityOptions())
		if err := workflow.ExecuteActivity(recordCtx, ActivityRecordBranch, RecordBranchInput{
			WorkspaceID: input.WorkspaceID,
			SessionID:   input.SessionID,
			Branch:      branch,
		}).Get(ctx, nil); err != nil {
			logger.Error("the session branch exists on GitHub but could not be recorded",
				"session_id", input.SessionID, "branch", branch.Name, "sha", branch.SHA)
			return failForBranch(ctx, input, err, string(application.BranchFailureUnrecorded))
		}
	}

	// The runner arrives in M5.4. Until then the session fails here, with a
	// reason that says why rather than leaving it in `provisioning` for
	// someone to find.
	return workflow.ExecuteActivity(ctx, ActivityFailUnprovisionable, input).Get(ctx, nil)
}

// failForBranch ends the session after a branch step failed, unless the
// session is not this workflow's to fail.
func failForBranch(ctx workflow.Context, input SessionWorkflowInput, err error, exhausted string) error {
	cause, fail := branchFailureCause(err, exhausted)
	if !fail {
		// The session is gone, or has moved on without us. Neither is this
		// workflow's to record as a failure.
		return err
	}
	workflow.GetLogger(ctx).Warn("the session branch step failed",
		"session_id", input.SessionID, "cause", cause)
	return workflow.ExecuteActivity(ctx, ActivityFailSession, FailSessionInput{
		WorkspaceID: input.WorkspaceID,
		SessionID:   input.SessionID,
		Cause:       cause,
	}).Get(ctx, nil)
}

// branchFailureCause decides what a failed branch activity means for the
// session.
//
// Three answers:
//
//   - refused for a named reason: fail the session with that reason;
//   - the session is gone or no longer provisioning: do not touch it;
//   - anything else — retries or time spent: fail it with `exhausted`, which
//     the caller chooses because it knows which step ran out. Out of retries
//     reaching GitHub is "unreachable"; out of time recording a branch GitHub
//     already made is "unrecorded", and the difference is whether a real ref
//     is left behind.
func branchFailureCause(err error, exhausted string) (string, bool) {
	var applicationErr *temporal.ApplicationError
	if errors.As(err, &applicationErr) {
		switch applicationErr.Type() {
		case ErrorTypeBranchRefused:
			var cause string
			if applicationErr.HasDetails() && applicationErr.Details(&cause) == nil && cause != "" {
				return cause, true
			}
			return exhausted, true
		case ErrorTypeNotFound, ErrorTypeTransitionNotAllowed:
			return "", false
		}
	}
	return exhausted, true
}
