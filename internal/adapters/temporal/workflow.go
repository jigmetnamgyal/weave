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
		StartToCloseTimeout: defaultActivityStartToClose,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    retryInitialInterval,
			BackoffCoefficient: retryBackoffCoefficient,
			MaximumInterval:    retryMaximumInterval,
			MaximumAttempts:    defaultActivityAttempts,
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
	// versionRunnerStep is M5.4a: provisioning, the branch, a runner with a
	// verified checkout, running, then failure — there is no provider yet.
	versionRunnerStep workflow.Version = 3
	// versionProviderStep is M5.5b: the provider runs, the runner is halted,
	// its events drained, the runner torn down, and the session ended by how
	// the run went — review_ready, failed or expired.
	versionProviderStep workflow.Version = 4
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
	version := workflow.GetVersion(ctx, sessionWorkflowChange, workflow.DefaultVersion, versionProviderStep)

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

	if version >= versionRunnerStep {
		return runSession(ctx, input, version)
	}

	// Versions 1 and 2 end here: no runner. The session fails with a
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

// runnerActivityOptions govern provisioning, which runs in the runner manager
// on its own queue.
//
// A heartbeat timeout, because the activity waits for readiness: a runner
// manager that dies mid-wait is noticed within it, and the retry finds the
// runner it had started. Three attempts: provisioning is idempotent, but a
// backend that fails three times is not going to succeed on the fourth.
func runnerActivityOptions(ctx workflow.Context) workflow.ActivityOptions {
	return provisionActivityOptions(RunnerTaskQueue(workflow.GetInfo(ctx).TaskQueueName))
}

// provisionActivityOptions are runnerActivityOptions for a known queue, so the
// bound MaxRunnerLifetime counts can be checked against the options
// themselves without a workflow context.
func provisionActivityOptions(queue string) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		TaskQueue:           queue,
		StartToCloseTimeout: RunnerReadyTimeout + provisionOverhead,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    retryInitialInterval,
			BackoffCoefficient: retryBackoffCoefficient,
			MaximumInterval:    retryMaximumInterval,
			MaximumAttempts:    provisionAttempts,
			NonRetryableErrorTypes: []string{
				ErrorTypeNotFound, ErrorTypeTransitionNotAllowed, ErrorTypeRunnerRefused,
			},
		},
	}
}

// teardownActivityOptions govern teardown, which must not give up easily: a
// runner that outlives its session holds a checkout ADR-013 says is gone.
// Retried without an attempt limit, bounded by an hour; the reconciler is the
// backstop past that.
func teardownActivityOptions(ctx workflow.Context) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		TaskQueue:              RunnerTaskQueue(workflow.GetInfo(ctx).TaskQueueName),
		StartToCloseTimeout:    2 * time.Minute,
		ScheduleToCloseTimeout: time.Hour,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    time.Minute,
			MaximumAttempts:    0,
		},
	}
}

// runSession gives the session a runner, moves it to running, and ends it.
//
// **Once provisioning has been attempted, every path out tears the runner
// down** — a runner that outlives its session is the failure this unit exists
// to prevent. Teardown runs on a disconnected context, so a cancelled workflow
// still cleans up, and it is idempotent, so tearing down a runner that never
// started is success.
//
// With no provider adapter (M5.5), a ready runner has nothing to run: it is
// torn down and the session fails naming that, rather than sitting in
// `running` with nothing that will ever move it.
func runSession(ctx workflow.Context, input SessionWorkflowInput, version workflow.Version) error {
	logger := workflow.GetLogger(ctx)
	teardown := func(failed bool, cause string) error {
		cleanup, cancel := workflow.NewDisconnectedContext(ctx)
		defer cancel()
		cleanup = workflow.WithActivityOptions(cleanup, teardownActivityOptions(ctx))
		err := workflow.ExecuteActivity(cleanup, ActivityTeardownRunner, TeardownInput{
			WorkspaceID: input.WorkspaceID, SessionID: input.SessionID, Failed: failed, Cause: cause,
		}).Get(cleanup, nil)
		if err != nil {
			// The reconciler finds a runner whose session has ended, so this
			// is not the last word — but it must be loud.
			logger.Error("runner teardown did not complete; the reconciler will finish it",
				"session_id", input.SessionID, "error", err.Error())
		}
		return err
	}

	runnerCtx := workflow.WithActivityOptions(ctx, runnerActivityOptions(ctx))
	var runner RunnerResult
	if err := workflow.ExecuteActivity(runnerCtx, ActivityProvisionRunner, input).Get(ctx, &runner); err != nil {
		cause, fail := runnerFailureCause(err)
		_ = teardown(true, cause)
		if !fail {
			return err
		}
		logger.Warn("the session's runner could not be provisioned", "session_id", input.SessionID, "cause", cause)
		return workflow.ExecuteActivity(ctx, ActivityFailSession, FailSessionInput{
			WorkspaceID: input.WorkspaceID, SessionID: input.SessionID, Cause: cause,
		}).Get(ctx, nil)
	}

	if err := workflow.ExecuteActivity(ctx, ActivityMarkRunning, input).Get(ctx, nil); err != nil {
		_ = teardown(true, string(application.RunnerFailureUnavailable))
		return err
	}
	logger.Info("the session is running", "session_id", input.SessionID, "runner_id", runner.RunnerID)

	if version >= versionProviderStep {
		return finishRun(ctx, input, teardown)
	}

	// Version 3: no provider adapter yet. The runner is torn down in the
	// ordinary way — it did its job — and the session fails saying why. The
	// session is failed even if teardown did not complete: the reconciler
	// tears down runners of *ended* sessions, so leaving this one in
	// `running` would leave its runner with nothing that will ever remove it.
	_ = teardown(false, string(application.RunnerFailureNoProvider))
	return workflow.ExecuteActivity(ctx, ActivityFailSession, FailSessionInput{
		WorkspaceID: input.WorkspaceID, SessionID: input.SessionID,
		Cause: string(application.RunnerFailureNoProvider),
	}).Get(ctx, nil)
}

// runnerFailureCause decides what a failed provisioning means for the session:
// a named refusal, not this workflow's to fail (session gone or moved on), or
// — retries spent — the runner was unavailable.
func runnerFailureCause(err error) (string, bool) {
	var applicationErr *temporal.ApplicationError
	if errors.As(err, &applicationErr) {
		switch applicationErr.Type() {
		case ErrorTypeRunnerRefused:
			var cause string
			if applicationErr.HasDetails() && applicationErr.Details(&cause) == nil && cause != "" {
				return cause, true
			}
		case ErrorTypeNotFound, ErrorTypeTransitionNotAllowed:
			return "", false
		}
	}
	return string(application.RunnerFailureUnavailable), true
}

// runExitActivityOptions govern waiting for the provider, which may run for
// hours: bounded by the session's maximum run time inside the activity, a
// heartbeat so a dead runner manager is noticed, and a retry that finds the
// same runner.
func runExitActivityOptions(ctx workflow.Context) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		TaskQueue:           RunnerTaskQueue(workflow.GetInfo(ctx).TaskQueueName),
		StartToCloseTimeout: application.SessionMaxRunTime + 5*time.Minute,
		HeartbeatTimeout:    time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 30 * time.Second,
			MaximumAttempts: 5,
		},
	}
}

// DrainScheduleToClose bounds halting, and separately draining, across every
// retry: a retried drain must not restart its ten minutes indefinitely, and
// the reconciler's HaltedRunnerGrace must outlast both (held by a test).
const DrainScheduleToClose = application.DrainTimeout + 5*time.Minute

// drainActivityOptions govern halting and draining: short, heartbeating, and
// retried, since both are idempotent.
func drainActivityOptions(ctx workflow.Context) workflow.ActivityOptions {
	return drainActivityOptionsFor(RunnerTaskQueue(workflow.GetInfo(ctx).TaskQueueName))
}

// drainActivityOptionsFor are drainActivityOptions for a known queue.
func drainActivityOptionsFor(queue string) workflow.ActivityOptions {
	return workflow.ActivityOptions{
		TaskQueue:              queue,
		ScheduleToCloseTimeout: DrainScheduleToClose,
		StartToCloseTimeout:    application.DrainTimeout + 2*time.Minute,
		HeartbeatTimeout:       time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 30 * time.Second,
			MaximumAttempts: 5,
		},
	}
}

// finishRun is version 4's end of a session: the provider has started.
//
//	exit (or maximum run time) → halt → drain → teardown → complete
//
// **The order is the fix for the race M5.3 and M5.4a left here.** A runner's
// last events may still be in the stream when it exits. Halting marks it
// terminating — its events still accepted — and destroys its environment;
// the drain then waits until the stream holds nothing for the session, so
// every event the stream confirmed is stored; only then is the runner ended
// and the session given a final state. Ending either one first would have the
// ingestor refuse those events as `runner_not_bound` or `session_terminal`.
// Loosening those refusals instead is not the fix: they are M5.3's gate and
// invariant 10.
func finishRun(ctx workflow.Context, input SessionWorkflowInput, teardown func(bool, string) error) error {
	logger := workflow.GetLogger(ctx)

	// The deadline is fixed here, once, and recorded in history: a retried
	// wait keeps it rather than starting a fresh maximum run.
	var exit RunnerExit
	runCtx := workflow.WithActivityOptions(ctx, runExitActivityOptions(ctx))
	if err := workflow.ExecuteActivity(runCtx, ActivityAwaitRunnerExit, AwaitExitInput{
		WorkspaceID: input.WorkspaceID, SessionID: input.SessionID,
		Deadline: workflow.Now(ctx).Add(application.SessionMaxRunTime),
	}).Get(ctx, &exit); err != nil {
		logger.Warn("the runner's exit could not be observed", "session_id", input.SessionID, "error", err.Error())
		exit = RunnerExit{ExitCode: runnerLostExit}
	}
	interrupted := ctx.Err() != nil

	// Disconnected, so a cancelled workflow still halts, drains, tears down
	// and ends the session — a session left in running with its runner gone
	// is one nothing would ever move.
	cleanup, cancel := workflow.NewDisconnectedContext(ctx)
	defer cancel()
	drainCtx := workflow.WithActivityOptions(cleanup, drainActivityOptions(ctx))
	halted := true
	if err := workflow.ExecuteActivity(drainCtx, ActivityHaltRunner, input).Get(cleanup, nil); err != nil {
		// Not halted, the runner may still be publishing, so an empty stream
		// proves nothing: the run cannot be vouched for.
		logger.Error("the runner could not be halted; its history cannot be vouched for",
			"session_id", input.SessionID, "error", err.Error())
		halted = false
	}
	var drain DrainResult
	if err := workflow.ExecuteActivity(drainCtx, ActivityDrainRunnerEvents, input).Get(cleanup, &drain); err != nil {
		logger.Error("the drain could not complete", "session_id", input.SessionID, "error", err.Error())
		drain.Drained = false
	}
	undrained := !halted || !drain.Drained

	failed := interrupted || exit.Expired || exit.ExitCode != 0 || undrained
	_ = teardown(failed, "")

	completeCtx := workflow.WithActivityOptions(cleanup, workflow.GetActivityOptions(ctx))
	return workflow.ExecuteActivity(completeCtx, ActivityCompleteSession, CompleteInput{
		WorkspaceID: input.WorkspaceID, SessionID: input.SessionID,
		ExitCode: exit.ExitCode, Expired: exit.Expired, Undrained: undrained, Interrupted: interrupted,
	}).Get(cleanup, nil)
}
