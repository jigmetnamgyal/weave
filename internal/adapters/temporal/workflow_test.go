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

// TestSessionWorkflowEndsSomewhereLegal pins the route the state machine
// allows, using Temporal's own test environment so the workflow is exercised
// rather than described.
//
// `queued → provisioning → failed`. It does not return to `queued`: the
// transition table forbids that edge, and an earlier draft of the M5 plan
// described exactly that loop before a reviewer caught it. Adding the edge to
// make a first workflow tidy would be changing a state machine to suit a demo.
func TestSessionWorkflowEndsSomewhereLegal(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	var steps []string
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error {
			steps = append(steps, "provisioning")
			return nil
		},
		activityOptions(weavetemporal.ActivityMarkProvisioning))
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error {
			steps = append(steps, "branch")
			return nil
		},
		activityOptions(weavetemporal.ActivityCreateBranch))
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error {
			steps = append(steps, "failed")
			return nil
		},
		activityOptions(weavetemporal.ActivityFailUnprovisionable))
	registerFailSession(env, nil)

	env.ExecuteWorkflow(weavetemporal.SessionWorkflow, weavetemporal.SessionWorkflowInput{
		WorkspaceID: uuid.NewString(),
		SessionID:   uuid.NewString(),
	})

	if !env.IsWorkflowCompleted() {
		t.Fatal("the workflow did not complete")
	}
	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	// The branch is cut while provisioning, and the session still ends
	// somewhere legal afterwards: there is no runner until M5.4.
	if got := strings.Join(steps, " → "); got != "provisioning → branch → failed" {
		t.Errorf("steps = %s, want provisioning → branch → failed", got)
	}
}

// TestSessionWorkflowStopsWhenProvisioningIsRefused checks the first failure
// short-circuits the second step.
//
// A session that could not be moved to provisioning must not then be told it
// failed to provision — the history would record a move it never made.
func TestSessionWorkflowStopsWhenProvisioningIsRefused(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()

	var failCalled, branchCalled bool
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error {
			return errRefused
		},
		activityOptions(weavetemporal.ActivityMarkProvisioning))
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error {
			branchCalled = true
			return nil
		},
		activityOptions(weavetemporal.ActivityCreateBranch))
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error {
			failCalled = true
			return nil
		},
		activityOptions(weavetemporal.ActivityFailUnprovisionable))
	registerFailSession(env, nil)

	env.ExecuteWorkflow(weavetemporal.SessionWorkflow, weavetemporal.SessionWorkflowInput{
		WorkspaceID: uuid.NewString(),
		SessionID:   uuid.NewString(),
	})

	if !env.IsWorkflowCompleted() {
		t.Fatal("the workflow did not complete")
	}
	if env.GetWorkflowError() == nil {
		t.Error("the workflow reported success after its first step failed")
	}
	if failCalled {
		t.Error("the workflow marked a session failed-to-provision after never provisioning it")
	}
	if branchCalled {
		t.Error("the workflow cut a branch for a session it never moved to provisioning")
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

// branchOutcome runs the workflow with a branch activity that fails with err,
// and reports which of the two failure activities ran and with what.
type branchOutcome struct {
	env               *testsuite.TestWorkflowEnvironment
	unprovisionable   bool
	failSessionCauses []string
	branchAttempts    int
}

func runWithBranchError(t *testing.T, branchErr error) *branchOutcome {
	t.Helper()
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	outcome := &branchOutcome{env: env}

	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error { return nil },
		activityOptions(weavetemporal.ActivityMarkProvisioning))
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error {
			outcome.branchAttempts++
			return branchErr
		},
		activityOptions(weavetemporal.ActivityCreateBranch))
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error {
			outcome.unprovisionable = true
			return nil
		},
		activityOptions(weavetemporal.ActivityFailUnprovisionable))
	registerFailSession(env, &outcome.failSessionCauses)

	env.ExecuteWorkflow(weavetemporal.SessionWorkflow, weavetemporal.SessionWorkflowInput{
		WorkspaceID: uuid.NewString(),
		SessionID:   uuid.NewString(),
	})
	if !env.IsWorkflowCompleted() {
		t.Fatal("the workflow did not complete")
	}
	return outcome
}

// TestARefusedBranchFailsTheSessionWithItsCause: a refusal a person has to fix
// lands the session in `failed` with the reason named, spending no retries.
func TestARefusedBranchFailsTheSessionWithItsCause(t *testing.T) {
	outcome := runWithBranchError(t, temporal.NewNonRetryableApplicationError(
		"no write access", weavetemporal.ErrorTypeBranchRefused, nil,
		string(application.BranchFailureNoWriteAccess)))

	if err := outcome.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v — the session was failed properly, which is success", err)
	}
	if outcome.branchAttempts != 1 {
		t.Errorf("the branch was attempted %d times; a terminal refusal must not be retried",
			outcome.branchAttempts)
	}
	if len(outcome.failSessionCauses) != 1 ||
		outcome.failSessionCauses[0] != string(application.BranchFailureNoWriteAccess) {
		t.Errorf("FailSession causes = %v, want [%s]",
			outcome.failSessionCauses, application.BranchFailureNoWriteAccess)
	}
	if outcome.unprovisionable {
		t.Error("the session was also failed as unprovisionable — two failures for one session")
	}
}

// TestAnUnreachableGitHubStillEndsTheSession: once the bounded retries for a
// transient error are spent, the session is failed rather than left in
// `provisioning` for someone to find.
func TestAnUnreachableGitHubStillEndsTheSession(t *testing.T) {
	outcome := runWithBranchError(t, errors.New("github: 502"))

	if err := outcome.env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if outcome.branchAttempts < 2 {
		t.Errorf("the branch was attempted %d times; a transient failure should be retried",
			outcome.branchAttempts)
	}
	if len(outcome.failSessionCauses) != 1 ||
		outcome.failSessionCauses[0] != string(application.BranchFailureUnreachable) {
		t.Errorf("FailSession causes = %v, want [%s]",
			outcome.failSessionCauses, application.BranchFailureUnreachable)
	}
}

// TestASessionThatMovedOnIsNotFailed: a session a member cancelled between the
// activities is not this workflow's to fail.
func TestASessionThatMovedOnIsNotFailed(t *testing.T) {
	outcome := runWithBranchError(t, temporal.NewNonRetryableApplicationError(
		"moved on", weavetemporal.ErrorTypeTransitionNotAllowed, nil))

	if outcome.env.GetWorkflowError() == nil {
		t.Error("the workflow reported success for a session it could not act on")
	}
	if len(outcome.failSessionCauses) != 0 || outcome.unprovisionable {
		t.Error("the workflow failed a session that had already moved on without it")
	}
}

// TestAVersionOneExecutionTakesNoBranchStep is the versioning guarantee.
//
// An execution that recorded version 1 before this change must finish on the
// path it started: meeting a new activity mid-replay would fail it
// non-deterministically.
func TestAVersionOneExecutionTakesNoBranchStep(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.OnGetVersion("session-workflow", workflow.DefaultVersion, workflow.Version(2)).Return(workflow.Version(1))

	var branched, failed bool
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error { return nil },
		activityOptions(weavetemporal.ActivityMarkProvisioning))
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error {
			branched = true
			return nil
		},
		activityOptions(weavetemporal.ActivityCreateBranch))
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error {
			failed = true
			return nil
		},
		activityOptions(weavetemporal.ActivityFailUnprovisionable))
	registerFailSession(env, nil)

	env.ExecuteWorkflow(weavetemporal.SessionWorkflow, weavetemporal.SessionWorkflowInput{
		WorkspaceID: uuid.NewString(),
		SessionID:   uuid.NewString(),
	})

	if err := env.GetWorkflowError(); err != nil {
		t.Fatalf("workflow error: %v", err)
	}
	if branched {
		t.Error("a version-1 execution cut a branch it was started without")
	}
	if !failed {
		t.Error("a version-1 execution did not finish on its original path")
	}
}

// registerFailSession registers the FailSession activity, recording causes.
func registerFailSession(env *testsuite.TestWorkflowEnvironment, causes *[]string) {
	env.RegisterActivityWithOptions(
		func(_ context.Context, input weavetemporal.FailSessionInput) error {
			if causes != nil {
				*causes = append(*causes, input.Cause)
			}
			return nil
		},
		activityOptions(weavetemporal.ActivityFailSession))
}
