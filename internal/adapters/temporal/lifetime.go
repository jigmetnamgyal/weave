package temporal

import (
	"time"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// Retry shapes the session workflow uses, named so that MaxRunnerLifetime is
// computed from the same values the activity options are built from:
// changing one changes the other (Unit M5.4b).
const (
	// defaultActivityStartToClose and defaultActivityAttempts govern
	// activityOptions — among them marking the session running, which sits
	// between provisioning and the run's deadline.
	defaultActivityStartToClose = 30 * time.Second
	defaultActivityAttempts     = 5

	// provisionAttempts and provisionOverhead govern provisioning: each
	// attempt may take RunnerReadyTimeout waiting for readiness, plus
	// provisionOverhead for creating the environment itself.
	provisionAttempts = 3
	provisionOverhead = time.Minute

	// The backoff both use between attempts.
	retryInitialInterval    = time.Second
	retryBackoffCoefficient = 2.0
	retryMaximumInterval    = 30 * time.Second
)

// SchedulingMargin covers what no timeout bounds: workflow and activity task
// latency between the steps MaxRunnerLifetime counts, and the last poll of a
// wait that ends just past its deadline. A worker down for longer than this
// takes the lost-runner path — the session fails, it is not left running.
const SchedulingMargin = 10 * time.Minute

// retrySpan is the longest an activity can take across every attempt: each
// attempt running to its start-to-close timeout, and the backoff between
// them. Schedule-to-start is not bounded by Temporal and is SchedulingMargin's.
func retrySpan(attempts int32, startToClose, initial time.Duration, coefficient float64, maximum time.Duration) time.Duration {
	total := time.Duration(attempts) * startToClose
	interval := initial
	for i := int32(1); i < attempts; i++ {
		total += min(interval, maximum)
		interval = time.Duration(float64(interval) * coefficient)
	}
	return total
}

// ProvisioningBound is the longest provisioning can take across every
// attempt. A retried Provision finds the environment the first attempt
// created, and the runner's broker credential is minted with it, so both
// count from the first attempt, not the last.
func ProvisioningBound() time.Duration {
	return retrySpan(provisionAttempts, RunnerReadyTimeout+provisionOverhead,
		retryInitialInterval, retryBackoffCoefficient, retryMaximumInterval)
}

// markRunningBound is the longest marking the session running can take.
func markRunningBound() time.Duration {
	return retrySpan(defaultActivityAttempts, defaultActivityStartToClose,
		retryInitialInterval, retryBackoffCoefficient, retryMaximumInterval)
}

// MaxRunnerLifetime is the longest a runner's environment can live **from its
// creation** while its workflow is healthy.
//
// A sandbox provider counts its session cap from creation, but the run's
// deadline is fixed only in finishRun, after provisioning has waited for
// readiness and the session has been marked running. SessionMaxRunTime plus
// the drain is therefore not enough: every minute before the deadline comes
// off the end, and a runner using its full run time would be stopped by the
// provider before the workflow halted it and confirmed its events — reported
// as lost, not the outcome it reached. So the bound counts:
//
//   - provisioning, every attempt (ProvisioningBound);
//   - marking the session running (markRunningBound);
//   - SessionMaxRunTime, from the deadline finishRun fixes;
//   - halt and drain, each bounded by DrainScheduleToClose. Halt destroys the
//     environment before the drain begins, so today only the halt needs it
//     alive; the drain is counted anyway, so a later change that keeps the
//     runner up while its events drain does not silently reopen this;
//   - SchedulingMargin.
//
// A backend whose cap is below this is refused in staging and production
// (M5.4b), and a test fails if this is ever less than the sum of the parts as
// the activity options actually configure them.
func MaxRunnerLifetime() time.Duration {
	return ProvisioningBound() + markRunningBound() + application.SessionMaxRunTime +
		2*DrainScheduleToClose + SchedulingMargin
}
