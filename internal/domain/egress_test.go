package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// TestEgressHostnameValidation checks canonicalization and rejected input classes
// without DNS, transport or deployment-specific policy dependencies.
func TestEgressHostnameValidation(t *testing.T) {
	atCap := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 61)
	for raw, want := range map[string]string{"docs.example.com": "docs.example.com", "DOCS.Example.COM": "docs.example.com", "cdn-1.example.com": "cdn-1.example.com", atCap: atCap} {
		got, err := domain.ValidateEgressHostname(raw)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", " docs.example.com", "docs.example.com ", "docs.example.com\n", "docs.\x00example.com", "docs.example.com.", "docs..example.com", "localhost", "foo.localhost", "foo.local", "foo.localdomain", "foo.internal", "foo.home.arpa", "home.arpa", "foo.onion", "https://docs.example.com", "docs.example.com:443", "docs.example.com/path", "docs.example.com?token=x", "user@docs.example.com", "*.example.com", "127.0.0.1", "127.1", "2130706433", "[::1]", "2001:4860::1", "éxample.com", "xn--bcher-kva.example", "foo.XN--bcher-kva.example", "-foo.example.com", "foo-.example.com", "foo_bar.example.com", strings.Repeat("a", 64) + ".example.com", atCap + "a"} {
		if got, err := domain.ValidateEgressHostname(raw); !errors.Is(err, domain.ErrInvalidEgressHost) || got != "" {
			t.Errorf("%q: %q %v, want invalid", raw, got, err)
		}
	}
}

// TestReservedEgressNamespaces prevents both exact service names and descendants
// from becoming plain additions, without confusing siblings or suffix lookalikes.
func TestReservedEgressNamespaces(t *testing.T) {
	reserved := []string{"weave.example.com", "registry.npmjs.org", ""}
	for _, host := range []string{"weave.example.com", "api.weave.example.com", "deep.api.weave.example.com", "registry.npmjs.org", "sub.registry.npmjs.org"} {
		if !domain.EgressHostReserved(host, reserved) {
			t.Errorf("reserved host accepted: %s", host)
		}
	}
	for _, host := range []string{"notweave.example.com", "weave.example.com.attacker.com", "npmjs.org", "other.example.com"} {
		if domain.EgressHostReserved(host, reserved) {
			t.Errorf("unrelated host refused: %s", host)
		}
	}
}

// TestEgressHostIdentityAndCap pins the approved cap and UUIDv7 identity format.
func TestEgressHostIdentityAndCap(t *testing.T) {
	if domain.MaxWorkspaceEgressHosts != 20 {
		t.Fatal("approved cap changed")
	}
	id, err := domain.NewEgressHostID()
	if err != nil || id.Version() != 7 {
		t.Fatalf("id=%v err=%v", id, err)
	}
}
