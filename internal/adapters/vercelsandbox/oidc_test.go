package vercelsandbox_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/adapters/vercelsandbox"
	"github.com/jigmetnamgyal/weave/internal/adapters/vercelsandbox/vercelsandboxtest"
	"github.com/jigmetnamgyal/weave/internal/application"
)

const (
	testTeam    = "team_test"
	testProject = "prj_runners"
	route       = "https://proxy.example.test/r/registry.npmjs.org"
)

func verifier(t *testing.T, issuer *vercelsandboxtest.Issuer) *vercelsandbox.OIDCVerifier {
	t.Helper()
	v, err := vercelsandbox.NewOIDCVerifier(context.Background(), vercelsandbox.OIDCConfig{
		TeamID: testTeam, ProjectID: testProject, IssuerBase: issuer.Server.URL, HTTPClient: issuer.Server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestAVercelSandboxTokenIsVerifiedForItsRoute: a token as Vercel issues it
// passes, and every way of getting one wrong is refused as invalid.
func TestAVercelSandboxTokenIsVerifiedForItsRoute(t *testing.T) {
	issuer := vercelsandboxtest.NewIssuer(t, testTeam)
	v := verifier(t, issuer)
	name := "weave-runner-live-" + uuid.NewString()
	good := vercelsandboxtest.Token{Audience: []string{route}, ProjectID: testProject, SandboxName: name}

	got, err := v.Verify(context.Background(), issuer.Mint(t, good), route)
	if err != nil || got.SandboxName != name || got.ProjectID != testProject {
		t.Fatalf("a good token = %+v, %v", got, err)
	}

	for label, token := range map[string]vercelsandboxtest.Token{
		"signed with a key the issuer does not publish": {Audience: good.Audience, ProjectID: testProject, SandboxName: name, Key: vercelsandboxtest.ForeignKey(t)},
		"another issuer":    {Issuer: "https://oidc.vercel.com/team_other", Audience: good.Audience, ProjectID: testProject, SandboxName: name},
		"another project":   {Audience: good.Audience, ProjectID: "prj_other", SandboxName: name},
		"another route":     {Audience: []string{"https://proxy.example.test/r/pypi.org"}, ProjectID: testProject, SandboxName: name},
		"a second audience": {Audience: []string{route, "https://elsewhere.example"}, ProjectID: testProject, SandboxName: name},
		"expired":           {Audience: good.Audience, ProjectID: testProject, SandboxName: name, Expires: time.Now().Add(-time.Hour)},
		"naming no sandbox": {Audience: good.Audience, ProjectID: testProject},
	} {
		if _, err := v.Verify(context.Background(), issuer.Mint(t, token), route); !errors.Is(err, application.ErrTokenInvalid) {
			t.Errorf("%s: %v, want ErrTokenInvalid", label, err)
		}
	}
	if _, err := v.Verify(context.Background(), "", route); !errors.Is(err, application.ErrTokenMissing) {
		t.Errorf("no token: %v, want ErrTokenMissing", err)
	}
	if _, err := v.Verify(context.Background(), "not.a.jwt", route); !errors.Is(err, application.ErrTokenInvalid) {
		t.Errorf("garbage: %v, want ErrTokenInvalid", err)
	}
	// alg: none, the classic: a header claiming no signature.
	none := "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0.eyJhdWQiOiJ4In0."
	if _, err := v.Verify(context.Background(), none, route); !errors.Is(err, application.ErrTokenInvalid) {
		t.Errorf("alg none: %v, want ErrTokenInvalid", err)
	}
}

// TestUnreachableKeysAreAnOutageNotABadToken: refused, and reported as the
// verifier being unavailable, so an outage is not mistaken for forgery.
func TestUnreachableKeysAreAnOutageNotABadToken(t *testing.T) {
	issuer := vercelsandboxtest.NewIssuer(t, testTeam)
	token := issuer.Mint(t, vercelsandboxtest.Token{Audience: []string{route}, ProjectID: testProject,
		SandboxName: "weave-runner-live-" + uuid.NewString()})
	v, err := vercelsandbox.NewOIDCVerifier(context.Background(), vercelsandbox.OIDCConfig{
		TeamID: testTeam, ProjectID: testProject, IssuerBase: "http://127.0.0.1:1", LookupTimeout: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Verify(context.Background(), token, route); !errors.Is(err, application.ErrVerifierUnavailable) {
		t.Errorf("unreachable keys: %v, want ErrVerifierUnavailable", err)
	}
}

// TestASandboxNameGivesBackItsRunner: the inverse of the backend's naming,
// and nothing else parses.
func TestASandboxNameGivesBackItsRunner(t *testing.T) {
	id := uuid.New()
	got, scope, err := vercelsandbox.RunnerFromSandboxName("weave-runner-live-" + id.String())
	if err != nil || got != id || scope != "live" {
		t.Errorf("= %s, %q, %v", got, scope, err)
	}
	for _, bad := range []string{
		"weave-fwdprobe-45dbeb1732", "weave-runner-" + id.String(), "weave-runner-Live-" + id.String(),
		"weave-runner-live-not-a-uuid-at-all-not-a-uuid-at-all0", "other-live-" + id.String(), "",
	} {
		if _, _, err := vercelsandbox.RunnerFromSandboxName(bad); !errors.Is(err, vercelsandbox.ErrNotARunnerSandbox) {
			t.Errorf("%q parsed as a runner sandbox", bad)
		}
	}
}
