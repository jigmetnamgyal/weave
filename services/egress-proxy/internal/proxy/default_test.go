package proxy

import (
	"testing"

	"github.com/jigmetnamgyal/weave/internal/adapters/guardedhttp"
)

// TestTheDefaultUpstreamIsTheGuardedTransport: with no upstream configured,
// production forwards only through the address guard (ADR-017). An ordinary
// client here would dial whatever a hostname resolved to.
func TestTheDefaultUpstreamIsTheGuardedTransport(t *testing.T) {
	if _, ok := New(Config{}).upstream.(*guardedhttp.Client); !ok {
		t.Fatal("the default upstream is not the guarded transport")
	}
}
