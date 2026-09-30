package temporal

import (
	"testing"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// spanOf is retrySpan computed from an activity's actual options — not from
// the named constants — so an options literal changed without the constant
// is caught here.
func spanOf(t *testing.T, options workflow.ActivityOptions) time.Duration {
	t.Helper()
	policy := options.RetryPolicy
	if policy == nil || policy.MaximumAttempts < 1 {
		t.Fatalf("an activity counted in MaxRunnerLifetime must have a bounded attempt count, got %+v", policy)
	}
	return retrySpan(policy.MaximumAttempts, options.StartToCloseTimeout,
		policy.InitialInterval, policy.BackoffCoefficient, policy.MaximumInterval)
}

// TestMaxRunnerLifetimeCoversEveryStepBeforeTheEnd fails if the bound is ever
// less than the sum of what the workflow can spend from the environment's
// creation to its halt and drain, as the options actually configure it. A new
// pre-run step, more attempts or a longer timeout cannot shorten the margin
// unnoticed.
func TestMaxRunnerLifetimeCoversEveryStepBeforeTheEnd(t *testing.T) {
	provisioning := spanOf(t, provisionActivityOptions("q"))
	markRunning := spanOf(t, activityOptions())
	drain := drainActivityOptionsFor("q")
	if drain.ScheduleToCloseTimeout <= 0 {
		t.Fatal("halt and drain must be bounded by a schedule-to-close timeout")
	}
	parts := provisioning + markRunning + application.SessionMaxRunTime + 2*drain.ScheduleToCloseTimeout
	if got := MaxRunnerLifetime(); got < parts+SchedulingMargin {
		t.Errorf("MaxRunnerLifetime = %s, but the steps it must cover sum to %s plus a %s margin",
			got, parts, SchedulingMargin)
	}
	if SchedulingMargin < 5*time.Minute {
		t.Errorf("SchedulingMargin = %s leaves no room for task latency", SchedulingMargin)
	}
}

// TestProvisioningBoundCountsEveryAttempt: the retry arithmetic itself, on the
// values in use today — three six-minute attempts with 1s and 2s of backoff.
func TestProvisioningBoundCountsEveryAttempt(t *testing.T) {
	if got, want := ProvisioningBound(), 3*(RunnerReadyTimeout+time.Minute)+3*time.Second; got != want {
		t.Errorf("ProvisioningBound = %s, want %s", got, want)
	}
	if got, want := retrySpan(6, time.Second, 20*time.Second, 2, 30*time.Second), 6*time.Second+20*time.Second+4*30*time.Second; got != want {
		t.Errorf("backoff is not capped at the maximum interval: %s, want %s", got, want)
	}
}
