package temporal_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/adapters/eventstream"
	"github.com/jigmetnamgyal/weave/internal/adapters/natsauth"
	weavetemporal "github.com/jigmetnamgyal/weave/internal/adapters/temporal"
	"github.com/jigmetnamgyal/weave/internal/application"
)

// TestTheDrainCannotOutliveTheStream is the invariant M5.5b's drain proof rests
// on, checked over the real constants so changing any one of them fails the
// build rather than silently breaking the proof.
//
// "The stream holds nothing for this session" means "every confirmed event was
// ingested" only if no event can expire first. An event's whole life in the
// stream is at most provisioning, the provider's run and the drain; that must
// sit inside the stream's maximum age with a wide margin — a day — for an
// ingestor outage of that length to still show up as undrained rather than as
// drained.
//
// Since M5.4b the life counted is MaxRunnerLifetime — every provisioning
// attempt, marking the session running, the run, halt and drain, and a
// scheduling margin — not a single RunnerReadyTimeout: more conservative, and
// the same bound a sandbox's session cap is held to.
func TestTheDrainCannotOutliveTheStream(t *testing.T) {
	const margin = 24 * time.Hour
	life := weavetemporal.MaxRunnerLifetime()
	if life+margin >= eventstream.StreamMaxAge {
		t.Errorf("an event can live %s in the stream, plus a %s margin, against a stream maximum age of %s: "+
			"a drain could read an expired event as an ingested one", life, margin, eventstream.StreamMaxAge)
	}
}

// TestTheBrokerCredentialOutlivesTheRun: a runner's credential must still be
// valid when its provider publishes its last event.
//
// The credential is minted once, with the environment, and reused by a
// retried Provision, so its clock runs from the first provisioning attempt.
// It must outlive the longest the environment holding it can live.
func TestTheBrokerCredentialOutlivesTheRun(t *testing.T) {
	need := weavetemporal.MaxRunnerLifetime()
	if need >= natsauth.RunnerCredentialLifetime {
		t.Errorf("a run may need the broker for %s, but its credential lives %s", need, natsauth.RunnerCredentialLifetime)
	}
}

// TestAWaitPastItsDeadlineExpiresAtOnce: an attempt that starts after the
// run's deadline — a retry late in a run — expires rather than waiting again.
// The service is nil: reaching it at all would be the bug.
func TestAWaitPastItsDeadlineExpiresAtOnce(t *testing.T) {
	activities := weavetemporal.NewRunnerActivities(nil)
	exit, err := activities.AwaitRunnerExit(context.Background(), weavetemporal.AwaitExitInput{
		WorkspaceID: uuid.NewString(), SessionID: uuid.NewString(), Deadline: time.Now().Add(-time.Minute),
	})
	if err != nil || !exit.Expired {
		t.Errorf("exit = %+v, %v; want expired", exit, err)
	}
}

// TestTheReconcilerOutwaitsALiveDrain: the reconciler finishes a halted
// runner of a running session only after HaltedRunnerGrace, taking its
// workflow to be gone. A live workflow's halt and drain, every retry
// included, must fit well inside that, or the reconciler would end a runner
// whose events are still draining and they would be refused.
func TestTheReconcilerOutwaitsALiveDrain(t *testing.T) {
	if spent := 2 * weavetemporal.DrainScheduleToClose; spent+15*time.Minute > application.HaltedRunnerGrace {
		t.Errorf("halt and drain may take %s; the reconciler's grace of %s must exceed that by at least 15m",
			spent, application.HaltedRunnerGrace)
	}
}
