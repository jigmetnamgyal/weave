package temporal

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Runner activity names. Registered by the runner manager, on the runner
// queue — never by the workflow worker, which holds no backend credentials.
const (
	ActivityProvisionRunner = "ProvisionRunner"
	ActivityTeardownRunner  = "TeardownRunner"
	ActivityMarkRunning     = "MarkSessionRunning"
)

// ErrorTypeRunnerRefused carries an application.RunnerFailure (or a branch
// cause, for a withdrawn grant found while minting clone credentials) as its
// detail.
const ErrorTypeRunnerRefused = "RunnerRefused"

// RunnerTaskQueue is where runner activities are scheduled for a session
// workflow on queue.
//
// Derived rather than fixed, so a test's per-test session queue gets its own
// runner queue and can never be served by a `make dev` runner manager — the
// interference M5.2's review found between a dev worker and the tests.
func RunnerTaskQueue(sessionQueue string) string { return sessionQueue + "-runners" }

// RunnerReadyTimeout bounds how long provisioning waits for a verified
// checkout. Generous for a cold image pull and a large clone; a runner not
// ready by then is failed rather than waited on indefinitely.
const RunnerReadyTimeout = 5 * time.Minute

// RunnerResult is what provisioning hands the workflow. Identifiers only.
type RunnerResult struct {
	RunnerID string `json:"runner_id"`
}

// TeardownInput names the runner to destroy by its session, and whether it
// ended in failure.
type TeardownInput struct {
	WorkspaceID string `json:"workspace_id"`
	SessionID   string `json:"session_id"`
	Failed      bool   `json:"failed"`
	// Cause is a RunnerFailure or BranchFailure code; the reason recorded is
	// derived from it, never taken as free text.
	Cause string `json:"cause"`
}

// RunnerActivities run in the runner manager.
type RunnerActivities struct {
	runners *application.RunnerService
}

// NewRunnerActivities wires the activities.
func NewRunnerActivities(runners *application.RunnerService) *RunnerActivities {
	return &RunnerActivities{runners: runners}
}

// ProvisionRunner starts the session's runner and waits for its checkout to
// be verified.
//
// Heartbeats while it waits, so a runner manager that dies mid-provisioning is
// noticed and the activity retried — and a retry finds the runner already
// started, because Provision is idempotent by session.
func (a *RunnerActivities) ProvisionRunner(ctx context.Context, input SessionWorkflowInput) (RunnerResult, error) {
	workspaceID, sessionID, err := parseIdentifiers(input)
	if err != nil {
		return RunnerResult{}, err
	}
	tenant := postgresTenant(ctx, workspaceID)

	runner, err := a.runners.Provision(tenant, workspaceID, sessionID)
	if err != nil {
		return RunnerResult{}, classifyRunnerError(err)
	}

	waitCtx, cancel := context.WithTimeout(tenant, RunnerReadyTimeout)
	defer cancel()
	runner, err = a.runners.AwaitReady(waitCtx, workspaceID, runner, 500*time.Millisecond,
		func() { activity.RecordHeartbeat(ctx) })
	if err != nil {
		return RunnerResult{}, classifyRunnerError(err)
	}
	return RunnerResult{RunnerID: runner.ID.String()}, nil
}

// TeardownRunner destroys the session's runner. Idempotent: no live runner,
// or an environment already gone, is success.
func (a *RunnerActivities) TeardownRunner(ctx context.Context, input TeardownInput) error {
	workspaceID, sessionID, err := parseIdentifiers(SessionWorkflowInput{
		WorkspaceID: input.WorkspaceID, SessionID: input.SessionID,
	})
	if err != nil {
		return err
	}
	reason := ""
	if input.Cause != "" {
		reason = application.SessionFailureReason(input.Cause)
	}
	return a.runners.Teardown(postgresTenant(ctx, workspaceID), workspaceID, sessionID, input.Failed, reason)
}

// classifyRunnerError turns a provisioning error into what Temporal acts on.
func classifyRunnerError(err error) error {
	switch {
	case errors.Is(err, application.ErrSessionNotFound):
		return temporal.NewNonRetryableApplicationError("the session no longer exists", ErrorTypeNotFound, err)
	case errors.Is(err, application.ErrSessionNotProvisioning):
		return temporal.NewNonRetryableApplicationError("the session is no longer provisioning", ErrorTypeTransitionNotAllowed, err)
	}
	if cause, terminal := application.ClassifyRunnerFailure(err); terminal {
		return temporal.NewNonRetryableApplicationError(application.SessionFailureReason(string(cause)),
			ErrorTypeRunnerRefused, err, string(cause))
	}
	// A repository withdrawn, or refused, while minting clone credentials:
	// the same classification as cutting the branch.
	if cause, terminal := application.ClassifyBranchFailure(err); terminal {
		return temporal.NewNonRetryableApplicationError(cause.Reason(), ErrorTypeRunnerRefused, err, string(cause))
	}
	return err
}

// MarkRunning moves a provisioning session to running — the first time any
// session has reached it.
func (a *SessionActivities) MarkRunning(ctx context.Context, input SessionWorkflowInput) error {
	return a.transition(ctx, input, domain.SessionRunning, "the session's runner is ready with a verified checkout")
}

// Activity names for the provider's run (M5.5b). The first three run in the
// runner manager, which holds the backend and the drain; CompleteSession runs
// in the workflow worker, which holds the session.
const (
	ActivityAwaitRunnerExit   = "AwaitRunnerExit"
	ActivityHaltRunner        = "HaltRunner"
	ActivityDrainRunnerEvents = "DrainRunnerEvents"
	ActivityCompleteSession   = "CompleteSession"
)

// RunnerExit is how the runner's run ended, as the backend reported it.
type RunnerExit struct {
	ExitCode int  `json:"exit_code"`
	Expired  bool `json:"expired"`
}

// DrainResult says whether the session's events were all ingested in time.
type DrainResult struct {
	Drained bool `json:"drained"`
}

// CompleteInput is everything the workflow observed about the run.
type CompleteInput struct {
	WorkspaceID string `json:"workspace_id"`
	SessionID   string `json:"session_id"`
	ExitCode    int    `json:"exit_code"`
	Expired     bool   `json:"expired"`
	Undrained   bool   `json:"undrained"`
}

// runnerLostExit stands for "the runner could not be observed to exit".
const runnerLostExit = -1

// AwaitRunnerExit waits for the runner to stop, bounded by the session's
// maximum run time, heartbeating as it waits. Reaching the bound is an
// outcome — the session expires — not an activity failure.
func (a *RunnerActivities) AwaitRunnerExit(ctx context.Context, input SessionWorkflowInput) (RunnerExit, error) {
	workspaceID, sessionID, err := parseIdentifiers(input)
	if err != nil {
		return RunnerExit{}, err
	}
	runCtx, cancel := context.WithTimeout(postgresTenant(ctx, workspaceID), application.SessionMaxRunTime)
	defer cancel()
	code, err := a.runners.AwaitExit(runCtx, workspaceID, sessionID, time.Second,
		func() { activity.RecordHeartbeat(ctx) })
	switch {
	case err == nil:
		return RunnerExit{ExitCode: code}, nil
	case errors.Is(err, application.ErrRunTimeExceeded):
		return RunnerExit{Expired: true}, nil
	case errors.Is(err, application.ErrRunnerLost), errors.Is(err, domain.ErrRunnerNotFound):
		return RunnerExit{ExitCode: runnerLostExit}, nil
	default:
		return RunnerExit{}, err
	}
}

// HaltRunner stops the runner without ending it, so its last events are still
// accepted while they drain. Idempotent.
func (a *RunnerActivities) HaltRunner(ctx context.Context, input SessionWorkflowInput) error {
	workspaceID, sessionID, err := parseIdentifiers(input)
	if err != nil {
		return err
	}
	return a.runners.Halt(postgresTenant(ctx, workspaceID), workspaceID, sessionID)
}

// DrainRunnerEvents waits, bounded, until the stream holds none of the
// session's events. Reaching the bound is an outcome, not an activity failure.
func (a *RunnerActivities) DrainRunnerEvents(ctx context.Context, input SessionWorkflowInput) (DrainResult, error) {
	_, sessionID, err := parseIdentifiers(input)
	if err != nil {
		return DrainResult{}, err
	}
	drainCtx, cancel := context.WithTimeout(ctx, application.DrainTimeout)
	defer cancel()
	err = a.runners.AwaitDrain(drainCtx, sessionID, 500*time.Millisecond, func() { activity.RecordHeartbeat(ctx) })
	switch {
	case err == nil:
		return DrainResult{Drained: true}, nil
	case errors.Is(err, application.ErrEventsNotDrained):
		return DrainResult{Drained: false}, nil
	default:
		return DrainResult{}, err
	}
}

// CompleteSession ends the session as its run leaves it. Runs in the workflow
// worker, after the drain.
func (a *SessionActivities) CompleteSession(ctx context.Context, input CompleteInput) error {
	workspaceID, sessionID, err := parseIdentifiers(SessionWorkflowInput{
		WorkspaceID: input.WorkspaceID, SessionID: input.SessionID,
	})
	if err != nil {
		return err
	}
	_, err = a.outcomes.Complete(postgresTenant(ctx, workspaceID), workspaceID, sessionID, application.RunOutcome{
		ExitCode: input.ExitCode, Expired: input.Expired, Undrained: input.Undrained,
	})
	if errors.Is(err, application.ErrSessionNotFound) {
		return temporal.NewNonRetryableApplicationError("the session no longer exists", ErrorTypeNotFound, err)
	}
	if errors.Is(err, domain.ErrTransitionNotAllowed) {
		return temporal.NewNonRetryableApplicationError("the session cannot end this way", ErrorTypeTransitionNotAllowed, err)
	}
	return err
}
