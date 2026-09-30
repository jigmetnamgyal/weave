//go:build vercel_live

// The egress proxy's live acceptance test (M5.4d.3a, ADR-017): a real Vercel
// sandbox, the real firewall's forwardURL, the real OIDC verifier and the real
// guarded transport, reached through the egress tunnel. Opt-in, never in CI.
// Run with `make test-vercel-live` after `scripts/dev-tunnel.sh start`.
package proxy_test

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/adapters/guardedhttp"
	"github.com/jigmetnamgyal/weave/internal/adapters/vercelsandbox"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/services/egress-proxy/internal/proxy"
)

// liveAuthorizer stands in for the snapshot: exactly these hosts for exactly
// this runner. It records what it was asked, to show traffic went through it.
type liveAuthorizer struct {
	mu      sync.Mutex
	runner  uuid.UUID
	allowed map[string]bool
	asked   []string
}

func (l *liveAuthorizer) Authorize(_ context.Context, runner uuid.UUID, host string) (application.RegistryRunner, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.asked = append(l.asked, host)
	if runner != l.runner || !l.allowed[host] {
		return application.RegistryRunner{}, application.ErrEgressHostNotAuthorized
	}
	return application.RegistryRunner{SessionID: uuid.New()}, nil
}

// recordingUpstream is the real guarded transport, with each outcome kept: the
// development tunnel replaces a 502's body with its own page, so the sandbox
// cannot see why a request was refused — this can.
type recordingUpstream struct {
	inner *guardedhttp.Client
	mu    sync.Mutex
	errs  map[string]error
}

func (r *recordingUpstream) Do(req *http.Request) (*http.Response, error) {
	resp, err := r.inner.Do(req)
	r.mu.Lock()
	r.errs[req.URL.Hostname()] = err
	r.mu.Unlock()
	return resp, err
}

func TestLiveAddedHostsGoOnlyThroughTheGuardedProxy(t *testing.T) {
	token, team, project := os.Getenv("RUNNER_VERCEL_TOKEN"), os.Getenv("RUNNER_VERCEL_TEAM_ID"), os.Getenv("RUNNER_VERCEL_PROJECT_ID")
	egressHost := os.Getenv("WEAVE_LIVE_EGRESS_HOST")
	if token == "" || team == "" || project == "" || egressHost == "" {
		t.Skip("Vercel credentials and WEAVE_LIVE_EGRESS_HOST are required; run `make test-vercel-live`")
	}
	publicBase := "https://" + egressHost
	ctx := context.Background()

	verifier, err := vercelsandbox.NewOIDCVerifier(ctx, vercelsandbox.OIDCConfig{TeamID: team, ProjectID: project})
	if err != nil {
		t.Fatal(err)
	}
	runner := uuid.New()
	// example.com: a benign public origin, in the snapshot. example.org: in
	// the policy but not the snapshot — the proxy must refuse it.
	// 10.0.0.1.nip.io: in both, but it resolves to a private address — the
	// guard must refuse it. (Not a loopback name: a sandbox connects to its own
	// loopback directly, so such a request never reaches the firewall.)
	authorizer := &liveAuthorizer{runner: runner, allowed: map[string]bool{"example.com": true, "10.0.0.1.nip.io": true}}
	listener, err := net.Listen("tcp", "127.0.0.1:8097")
	if err != nil {
		t.Fatalf("the egress tunnel forwards to 127.0.0.1:8097, which is taken: %v", err)
	}
	upstream := &recordingUpstream{inner: guardedhttp.New(), errs: map[string]error{}}
	server := &http.Server{Handler: proxy.New(proxy.Config{PublicBase: publicBase, Verifier: verifier, Authorizer: authorizer, Upstream: upstream,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))}),
		ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	binary, err := os.ReadFile(os.Getenv("RUNNER_VERCEL_BINARY"))
	if err != nil {
		t.Fatal(err)
	}
	backend, err := vercelsandbox.New(vercelsandbox.Config{AppEnv: "test", Token: token, TeamID: team, ProjectID: project,
		Region: "iad1", Scope: "live" + uuid.NewString()[:6], MaxSession: 45 * time.Minute, RunnerBinary: binary})
	if err != nil {
		t.Fatal(err)
	}
	rules := []application.EgressRule{{Host: "github.com"}}
	for _, host := range []string{"example.com", "example.org", "10.0.0.1.nip.io"} {
		rules = append(rules, application.EgressRule{Host: host, ForwardURL: application.EgressForwardURL(publicBase, host)})
	}
	session, err := backend.LiveSandbox(ctx, runner, rules)
	t.Cleanup(func() {
		if err := backend.Destroy(context.Background(), runner, ""); err != nil {
			t.Errorf("teardown: %v", err)
		}
		if snaps, err := backend.LiveSnapshots(context.Background(), runner); err != nil || len(snaps) != 0 {
			t.Errorf("snapshots after teardown: %v, %v", snaps, err)
		}
	})
	if err != nil {
		t.Fatalf("sandbox: %v", err)
	}

	fetch := func(url string) (string, string) {
		script := "rm -f /tmp/body; curl -sS --max-time 30 -o /tmp/body -w '%{http_code}' '" + url + "' 2>/dev/null; echo; head -c 300 /tmp/body 2>/dev/null"
		_, out, err := backend.LiveRun(ctx, session, script)
		if err != nil {
			t.Fatal(err)
		}
		code, body, _ := strings.Cut(out, "\n")
		return strings.TrimSpace(code), body
	}
	for _, c := range []struct {
		name, url, code, contains string
	}{
		{"an added host, through the proxy", "https://example.com/", "200", "Example Domain"},
		{"a forwarded host missing from the snapshot", "https://example.org/", "403", "may not reach"},
		// The body is the tunnel's error page; why is checked below.
		{"an added host resolving to a private address", "https://10.0.0.1.nip.io/", "502", ""},
	} {
		code, body := fetch(c.url)
		t.Logf("%s: %s %q", c.name, code, strings.TrimSpace(body))
		if code != c.code || !strings.Contains(body, c.contains) {
			t.Errorf("%s: got %s %q; want %s containing %q", c.name, code, body, c.code, c.contains)
		}
	}
	// Not in the policy at all: the firewall refuses, and the name does not
	// resolve — and a subdomain of an added host is not added.
	for _, host := range []string{"www.example.com", "iana.org"} {
		exit, _, err := backend.LiveRun(ctx, session, "getent hosts "+host+" >/dev/null")
		if err != nil {
			t.Fatal(err)
		}
		if exit == 0 {
			t.Errorf("%s resolved inside the sandbox; only listed hosts may", host)
		}
	}
	authorizer.mu.Lock()
	t.Logf("the proxy was asked about: %v", authorizer.asked)
	authorizer.mu.Unlock()
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if err, reached := upstream.errs["10.0.0.1.nip.io"]; !reached || !errors.Is(err, guardedhttp.ErrDestination) {
		t.Errorf("the private-address host: guard outcome %v (reached=%v), want ErrDestination", err, reached)
	}
	if err := upstream.errs["example.com"]; err != nil {
		t.Errorf("example.com through the guard: %v", err)
	}
	if _, reached := upstream.errs["example.org"]; reached {
		t.Error("a host missing from the snapshot reached the upstream")
	}
}
