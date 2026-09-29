package temporal_test

import (
	"testing"
	"time"

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
func TestTheDrainCannotOutliveTheStream(t *testing.T) {
	const margin = 24 * time.Hour
	life := weavetemporal.RunnerReadyTimeout + application.SessionMaxRunTime + application.DrainTimeout
	if life+margin >= eventstream.StreamMaxAge {
		t.Errorf("an event can live %s in the stream, plus a %s margin, against a stream maximum age of %s: "+
			"a drain could read an expired event as an ingested one", life, margin, eventstream.StreamMaxAge)
	}
}

// TestTheBrokerCredentialOutlivesTheRun: a runner's credential must still be
// valid when its provider publishes its last event.
func TestTheBrokerCredentialOutlivesTheRun(t *testing.T) {
	need := weavetemporal.RunnerReadyTimeout + application.SessionMaxRunTime
	if need >= natsauth.RunnerCredentialLifetime {
		t.Errorf("a run may need the broker for %s, but its credential lives %s", need, natsauth.RunnerCredentialLifetime)
	}
}
