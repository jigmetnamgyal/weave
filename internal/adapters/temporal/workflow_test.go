package temporal_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"

	weavetemporal "github.com/jigmetnamgyal/weave/internal/adapters/temporal"
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

	var marked, failed bool
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error {
			marked = true
			return nil
		},
		activityOptions(weavetemporal.ActivityMarkProvisioning))
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error {
			failed = true
			return nil
		},
		activityOptions(weavetemporal.ActivityFailUnprovisionable))

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
	if !marked {
		t.Error("the session was never moved to provisioning")
	}
	if !failed {
		t.Error("the session was left in provisioning rather than ending somewhere legal")
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

	var failCalled bool
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error {
			return errRefused
		},
		activityOptions(weavetemporal.ActivityMarkProvisioning))
	env.RegisterActivityWithOptions(
		func(_ context.Context, _ weavetemporal.SessionWorkflowInput) error {
			failCalled = true
			return nil
		},
		activityOptions(weavetemporal.ActivityFailUnprovisionable))

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
}

// errRefused stands in for a transition the domain will not allow.
var errRefused = errors.New("the session cannot become provisioning")

// activityOptions names a registered activity, matching how the worker
// registers them — by a stable string rather than by function name, so a
// running workflow is not stranded by a rename.
func activityOptions(name string) activity.RegisterOptions {
	return activity.RegisterOptions{Name: name}
}
