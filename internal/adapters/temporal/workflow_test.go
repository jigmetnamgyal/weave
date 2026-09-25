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

	markErr   error
	cutErr    error
	recordErr error

	steps          []string
	cutAttempts    int
	recordAttempts int
	recorded       []weavetemporal.RecordBranchInput
	failCauses     []string
}

func newRun() *run {
	var suite testsuite.WorkflowTestSuite
	return &run{env: suite.NewTestWorkflowEnvironment()}
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
		func(context.Context, weavetemporal.SessionWorkflowInput) error {
			r.steps = append(r.steps, "failed")
			return nil
		},
		activityOptions(weavetemporal.ActivityFailUnprovisionable))
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
	if got := r.route(); got != "provisioning → cut → record → failed" {
		t.Errorf("route = %s, want provisioning → cut → record → failed", got)
	}
	// The branch recorded is the one the cut returned, carried through
	// history rather than re-read.
	if len(r.recorded) != 1 || r.recorded[0].Branch.Name != "weave/x" || !r.recorded[0].Branch.Created {
		t.Errorf("recorded %+v, want the cut branch", r.recorded)
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
	if len(r.steps) != 0 || r.cutAttempts != 0 {
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
	r.env.OnGetVersion("session-workflow", workflow.DefaultVersion, workflow.Version(2)).
		Return(workflow.Version(1))
	r.execute(t)

	if err := r.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if got := r.route(); got != "provisioning → failed" {
		t.Errorf("route = %s, want a version-1 execution to finish on its original path", got)
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
