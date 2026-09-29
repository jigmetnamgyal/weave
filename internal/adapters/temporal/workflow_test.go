package temporal_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	weavetemporal "github.com/jigmetnamgyal/weave/internal/adapters/temporal"
	"github.com/jigmetnamgyal/weave/internal/application"
)

// run records what the workflow did, with each activity's behaviour set by the
// test. Every activity the workflow can call is registered, so a test cannot
// pass by the workflow calling something unregistered and failing early.
type run struct {
	env *testsuite.TestWorkflowEnvironment

	markErr      error
	cutErr       error
	recordErr    error
	provisionErr error
	runningErr   error
	exit         weavetemporal.RunnerExit
	exitErr      error
	drained      bool

	steps          []string
	cutAttempts    int
	recordAttempts int
	recorded       []weavetemporal.RecordBranchInput
	failCauses     []string
	teardowns      []weavetemporal.TeardownInput
	completed      []weavetemporal.CompleteInput
}

func newRun() *run {
	var suite testsuite.WorkflowTestSuite
	return &run{env: suite.NewTestWorkflowEnvironment(), drained: true}
}

func (r *run) execute(t *testing.T) {
	t.Helper()
	r.env.RegisterActivityWithOptions(
		func(context.Context, weavetemporal.SessionWorkflowInput) error {
			if r.markErr != nil {
				return r.markErr
			}
			r.steps = append(r.steps, "provisioning")
			return nil
		},
		activityOptions(weavetemporal.ActivityMarkProvisioning))
	r.env.RegisterActivityWithOptions(
		func(context.Context, weavetemporal.SessionWorkflowInput) (weavetemporal.BranchResult, error) {
			r.cutAttempts++
			if r.cutErr != nil {
				return weavetemporal.BranchResult{}, r.cutErr
			}
			r.steps = append(r.steps, "cut")
			return weavetemporal.BranchResult{
				Name: "weave/x", SHA: "5f1c0a9e3b7d2c4f6a8e0b1d3c5e7f9a1b3c5d7e", Base: "main", Created: true,
			}, nil
		},
		activityOptions(weavetemporal.ActivityCreateBranch))
	r.env.RegisterActivityWithOptions(
		func(_ context.Context, input weavetemporal.RecordBranchInput) error {
			r.recordAttempts++
			if r.recordErr != nil {
				return r.recordErr
			}
			r.steps = append(r.steps, "record")
			r.recorded = append(r.recorded, input)
			return nil
		},
		activityOptions(weavetemporal.ActivityRecordBranch))
	r.env.RegisterActivityWithOptions(
		func(context.Context, weavetemporal.SessionWorkflowInput) (weavetemporal.RunnerResult, error) {
			if r.provisionErr != nil {
				return weavetemporal.RunnerResult{}, r.provisionErr
			}
			r.steps = append(r.steps, "runner")
			return weavetemporal.RunnerResult{RunnerID: uuid.NewString()}, nil
		},
		activityOptions(weavetemporal.ActivityProvisionRunner))
	r.env.RegisterActivityWithOptions(
		func(_ context.Context, input weavetemporal.TeardownInput) error {
			r.steps = append(r.steps, "teardown")
			r.teardowns = append(r.teardowns, input)
			return nil
		},
		activityOptions(weavetemporal.ActivityTeardownRunner))
	r.env.RegisterActivityWithOptions(
		func(context.Context, weavetemporal.SessionWorkflowInput) error {
			if r.runningErr != nil {
				return r.runningErr
			}
			r.steps = append(r.steps, "running")
			return nil
		},
		activityOptions(weavetemporal.ActivityMarkRunning))
	r.env.RegisterActivityWithOptions(
		func(context.Context, weavetemporal.SessionWorkflowInput) error {
			r.steps = append(r.steps, "failed")
			return nil
		},
		activityOptions(weavetemporal.ActivityFailUnprovisionable))
	r.env.RegisterActivityWithOptions(
		func(context.Context, weavetemporal.SessionWorkflowInput) (weavetemporal.RunnerExit, error) {
			if r.exitErr != nil {
				return weavetemporal.RunnerExit{}, r.exitErr
			}
			r.steps = append(r.steps, "exit")
			return r.exit, nil
		},
		activityOptions(weavetemporal.ActivityAwaitRunnerExit))
	r.env.RegisterActivityWithOptions(
		func(context.Context, weavetemporal.SessionWorkflowInput) error {
			r.steps = append(r.steps, "halt")
			return nil
		},
		activityOptions(weavetemporal.ActivityHaltRunner))
	r.env.RegisterActivityWithOptions(
		func(context.Context, weavetemporal.SessionWorkflowInput) (weavetemporal.DrainResult, error) {
			r.steps = append(r.steps, "drain")
			return weavetemporal.DrainResult{Drained: r.drained}, nil
		},
		activityOptions(weavetemporal.ActivityDrainRunnerEvents))
	r.env.RegisterActivityWithOptions(
		func(_ context.Context, input weavetemporal.CompleteInput) error {
			r.steps = append(r.steps, "complete")
			r.completed = append(r.completed, input)
			return nil
		},
		activityOptions(weavetemporal.ActivityCompleteSession))
	r.env.RegisterActivityWithOptions(
		func(_ context.Context, input weavetemporal.FailSessionInput) error {
			r.steps = append(r.steps, "failed:"+input.Cause)
			r.failCauses = append(r.failCauses, input.Cause)
			return nil
		},
		activityOptions(weavetemporal.ActivityFailSession))

	r.env.ExecuteWorkflow(weavetemporal.SessionWorkflow, weavetemporal.SessionWorkflowInput{
		WorkspaceID: uuid.NewString(),
		SessionID:   uuid.NewString(),
	})
	if !r.env.IsWorkflowCompleted() {
		t.Fatal("the workflow did not complete")
	}
}

func (r *run) route() string { return strings.Join(r.steps, " → ") }

// TestSessionWorkflowEndsSomewhereLegal pins the route the state machine
// allows, using Temporal's own test environment so the workflow is exercised
// rather than described.
//
// The branch is cut and recorded while provisioning, and the session still
// ends in `failed` — there is no runner until M5.4. It does not return to
// `queued`: the transition table forbids that edge, and an earlier draft of
// the M5 plan described exactly that loop before a reviewer caught it.
func TestSessionWorkflowEndsSomewhereLegal(t *testing.T) {
	r := newRun()
	r.execute(t)

	if err := r.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	// Version 4: the branch, a runner with a verified checkout, running, the
	// provider's run — then halt, drain, teardown, and only then the end.
	want := "provisioning → cut → record → runner → running → exit → halt → drain → teardown → complete"
	if got := r.route(); got != want {
		t.Errorf("route = %s\nwant    %s", got, want)
	}
	if len(r.recorded) != 1 || r.recorded[0].Branch.Name != "weave/x" || !r.recorded[0].Branch.Created {
		t.Errorf("recorded %+v, want the cut branch", r.recorded)
	}
	if len(r.teardowns) != 1 || r.teardowns[0].Failed {
		t.Errorf("teardowns = %+v, want one ordinary teardown after a clean run", r.teardowns)
	}
	if c := r.completed; len(c) != 1 || c[0].ExitCode != 0 || c[0].Expired || c[0].Undrained {
		t.Errorf("completed with %+v, want a clean exit, drained", c)
	}
}

// TestWhatTheWorkflowObservesReachesTheOutcome: each way a run can end is
// carried to CompleteSession, and the runner is torn down as failed for all of
// them — halted and drained first every time.
func TestWhatTheWorkflowObservesReachesTheOutcome(t *testing.T) {
	cases := map[string]struct {
		arrange func(*run)
		want    weavetemporal.CompleteInput
	}{
		"provider failed": {func(r *run) { r.exit = weavetemporal.RunnerExit{ExitCode: 6} },
			weavetemporal.CompleteInput{ExitCode: 6}},
		"ran too long": {func(r *run) { r.exit = weavetemporal.RunnerExit{Expired: true} },
			weavetemporal.CompleteInput{Expired: true}},
		"not drained": {func(r *run) { r.drained = false },
			weavetemporal.CompleteInput{Undrained: true}},
		"exit not observed": {func(r *run) { r.exitErr = errors.New("backend unreachable") },
			weavetemporal.CompleteInput{ExitCode: -1}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRun()
			tc.arrange(r)
			r.execute(t)
			if len(r.completed) != 1 {
				t.Fatalf("route %s: completed %d times", r.route(), len(r.completed))
			}
			got := r.completed[0]
			if got.ExitCode != tc.want.ExitCode || got.Expired != tc.want.Expired || got.Undrained != tc.want.Undrained {
				t.Errorf("completed with %+v, want %+v", got, tc.want)
			}
			if len(r.teardowns) != 1 || !r.teardowns[0].Failed {
				t.Errorf("teardowns = %+v, want one, as failed", r.teardowns)
			}
			if !strings.Contains(r.route(), "halt → drain → teardown → complete") {
				t.Errorf("route = %s; the end must be halt, drain, teardown, complete, in that order", r.route())
			}
		})
	}
}

// TestAVersionThreeExecutionEndsWithoutAProvider: executions that recorded
// version 3 before M5.5b finish as they started — no provider run.
func TestAVersionThreeExecutionEndsWithoutAProvider(t *testing.T) {
	r := newRun()
	r.env.OnGetVersion("session-workflow", workflow.DefaultVersion, workflow.Version(4)).Return(workflow.Version(3))
	r.execute(t)
	want := "provisioning → cut → record → runner → running → teardown → failed:" + string(application.RunnerFailureNoProvider)
	if got := r.route(); got != want {
		t.Errorf("route = %s\nwant    %s", got, want)
	}
}

// TestEveryPathOutOfProvisioningTearsDown: a runner that outlives its session
// is the failure M5.4a exists to prevent, so each way provisioning can go
// wrong ends in a teardown before the session is failed.
func TestEveryPathOutOfProvisioningTearsDown(t *testing.T) {
	cases := map[string]struct {
		arrange   func(*run)
		wantCause string
		wantFail  bool
	}{
		"a refused checkout": {func(r *run) {
			r.provisionErr = temporal.NewNonRetryableApplicationError("mismatch",
				weavetemporal.ErrorTypeRunnerRefused, nil, string(application.RunnerFailureCheckoutMismatch))
		}, string(application.RunnerFailureCheckoutMismatch), true},
		"a backend that keeps failing": {func(r *run) {
			r.provisionErr = errors.New("docker: connection refused")
		}, string(application.RunnerFailureUnavailable), true},
		"a session that moved on": {func(r *run) {
			r.provisionErr = temporal.NewNonRetryableApplicationError("moved on",
				weavetemporal.ErrorTypeTransitionNotAllowed, nil)
		}, "", false},
		"running refused after the runner came up": {func(r *run) {
			r.runningErr = temporal.NewNonRetryableApplicationError("moved on",
				weavetemporal.ErrorTypeTransitionNotAllowed, nil)
		}, "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := newRun()
			tc.arrange(r)
			r.execute(t)

			if len(r.teardowns) != 1 {
				t.Fatalf("route %s: %d teardowns, want exactly 1", r.route(), len(r.teardowns))
			}
			if !r.teardowns[0].Failed {
				t.Error("a failed provisioning was torn down as an ordinary end")
			}
			if tc.wantFail {
				if len(r.failCauses) != 1 || r.failCauses[0] != tc.wantCause {
					t.Errorf("fail causes = %v, want [%s]", r.failCauses, tc.wantCause)
				}
			} else if len(r.failCauses) != 0 {
				t.Errorf("failed a session that was not this workflow's to fail: %v", r.failCauses)
			}
		})
	}
}

// TestSessionWorkflowStopsWhenProvisioningIsRefused checks the first failure
// short-circuits everything after it.
//
// A session that could not be moved to provisioning must not then have a
// branch cut for it, or be told it failed to provision — the history would
// record a move it never made.
func TestSessionWorkflowStopsWhenProvisioningIsRefused(t *testing.T) {
	r := newRun()
	r.markErr = errRefused
	r.execute(t)

	if r.env.GetWorkflowError() == nil {
		t.Error("the workflow reported success after its first step failed")
	}
	if len(r.steps) != 0 || r.cutAttempts != 0 || len(r.teardowns) != 0 {
		t.Errorf("the workflow went on to %s after never provisioning", r.route())
	}
}

// TestARefusedBranchFailsTheSessionWithItsCause: a refusal a person has to fix
// lands the session in `failed` with the reason named, spending no retries.
func TestARefusedBranchFailsTheSessionWithItsCause(t *testing.T) {
	r := newRun()
	r.cutErr = temporal.NewNonRetryableApplicationError(
		"no write access", weavetemporal.ErrorTypeBranchRefused, nil,
		string(application.BranchFailureNoWriteAccess))
	r.execute(t)

	if err := r.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v — the session was failed properly, which is success", err)
	}
	if r.cutAttempts != 1 {
		t.Errorf("the branch was attempted %d times; a terminal refusal must not be retried", r.cutAttempts)
	}
	if got := r.route(); got != "provisioning → failed:"+string(application.BranchFailureNoWriteAccess) {
		// No runner is started for a session whose branch was refused.
		t.Errorf("route = %s, want one failure naming the cause and nothing recorded", got)
	}
}

// TestAnUnreachableGitHubStillEndsTheSession: once the bounded retries for a
// transient error are spent, the session is failed rather than left in
// `provisioning` for someone to find.
func TestAnUnreachableGitHubStillEndsTheSession(t *testing.T) {
	r := newRun()
	r.cutErr = errors.New("github: 502")
	r.execute(t)

	if err := r.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if r.cutAttempts < 2 {
		t.Errorf("the branch was attempted %d times; a transient failure should be retried", r.cutAttempts)
	}
	if len(r.failCauses) != 1 || r.failCauses[0] != string(application.BranchFailureUnreachable) {
		t.Errorf("fail causes = %v, want [%s]", r.failCauses, application.BranchFailureUnreachable)
	}
}

// TestABranchThatCannotBeRecordedIsNotCalledAnOutage is the review finding on
// PR #17.
//
// GitHub made the ref; the database will not take its SHA. The earlier shape
// retried the whole step, cut and record together, five times and then
// failed the session as "GitHub could not be reached" — false, and it left a
// real ref with no record. Now the cut is not repeated, recording outlasts
// the GitHub retry budget many times over, and if it still cannot land the
// reason says the branch exists.
func TestABranchThatCannotBeRecordedIsNotCalledAnOutage(t *testing.T) {
	r := newRun()
	r.recordErr = errors.New("database: connection refused")
	r.execute(t)

	if err := r.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if r.cutAttempts != 1 {
		t.Errorf("GitHub was asked %d times; the cut succeeded once and must not be repeated", r.cutAttempts)
	}
	// The GitHub step's policy stops at five. Recording must be far more
	// patient than that, or a brief database outage forgets a real ref.
	if r.recordAttempts <= 5 {
		t.Errorf("recording was attempted %d times; it must outlast the GitHub retry budget",
			r.recordAttempts)
	}
	if len(r.failCauses) != 1 || r.failCauses[0] != string(application.BranchFailureUnrecorded) {
		t.Errorf("fail causes = %v, want [%s] — the branch exists, and the reason must say so",
			r.failCauses, application.BranchFailureUnrecorded)
	}
}

// TestASessionThatMovedOnIsNotFailed: a session a member cancelled between the
// activities is not this workflow's to fail.
func TestASessionThatMovedOnIsNotFailed(t *testing.T) {
	r := newRun()
	r.cutErr = temporal.NewNonRetryableApplicationError(
		"moved on", weavetemporal.ErrorTypeTransitionNotAllowed, nil)
	r.execute(t)

	if r.env.GetWorkflowError() == nil {
		t.Error("the workflow reported success for a session it could not act on")
	}
	if len(r.failCauses) != 0 || strings.Contains(r.route(), "failed") {
		t.Errorf("route = %s; the workflow failed a session that had moved on without it", r.route())
	}
}

// TestAVersionOneExecutionTakesNoBranchStep is the versioning guarantee.
//
// An execution that recorded version 1 before this change must finish on the
// path it started: meeting a new activity mid-replay would fail it
// non-deterministically.
func TestAVersionOneExecutionTakesNoBranchStep(t *testing.T) {
	r := newRun()
	r.env.OnGetVersion("session-workflow", workflow.DefaultVersion, workflow.Version(4)).
		Return(workflow.Version(1))
	r.execute(t)

	if err := r.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if got := r.route(); got != "provisioning → failed" {
		t.Errorf("route = %s, want a version-1 execution to finish on its original path", got)
	}
}

// TestAVersionTwoExecutionTakesNoRunnerStep: executions that recorded version
// 2 before M5.4a finish without a runner, as they started.
func TestAVersionTwoExecutionTakesNoRunnerStep(t *testing.T) {
	r := newRun()
	r.env.OnGetVersion("session-workflow", workflow.DefaultVersion, workflow.Version(4)).
		Return(workflow.Version(2))
	r.execute(t)

	if err := r.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if got := r.route(); got != "provisioning → cut → record → failed" {
		t.Errorf("route = %s, want a version-2 execution to finish without a runner", got)
	}
}

// errRefused stands in for a transition the domain will not allow.
var errRefused = errors.New("the session cannot become provisioning")

// activityOptions names a registered activity, matching how the worker
// registers them — by a stable string rather than by function name, so a
// running workflow is not stranded by a rename.
func activityOptions(name string) activity.RegisterOptions {
	return activity.RegisterOptions{Name: name}
}
