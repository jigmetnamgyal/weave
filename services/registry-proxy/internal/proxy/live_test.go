//go:build vercel_live

// The registry proxy's live acceptance test (M5.4c): a real Vercel sandbox,
// with the policy the runner manager builds, whose registry requests Vercel's
// firewall forwards through the development tunnel to this proxy — the real
// handler and verifier, fetching from the real registries. Opt-in behind a
// build tag, never in CI. Run with `make test-vercel-live`, tunnels up.
package proxy_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/adapters/vercelsandbox"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/services/registry-proxy/internal/proxy"
)

// liveRecorder keeps what the proxy recorded, per runner.
type liveRecorder struct {
	mu    sync.Mutex
	calls []recorded
}

func (l *liveRecorder) Record(_ context.Context, runner uuid.UUID, host, method, path string) (application.RegistryRequest, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, _, _ := strings.Cut(path, "?")
	l.calls = append(l.calls, recorded{runner, host, method, p})
	return application.RegistryRequest{SessionID: uuid.New(), Method: method, Path: p}, nil
}

func (l *liveRecorder) saw(runner uuid.UUID, host, pathPrefix string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.calls {
		if c.runner == runner && c.host == host && strings.HasPrefix(c.path, pathPrefix) {
			return true
		}
	}
	return false
}

func TestLiveRegistryRequestsAreRecordedAndForwarded(t *testing.T) {
	token, team, project := os.Getenv("RUNNER_VERCEL_TOKEN"), os.Getenv("RUNNER_VERCEL_TEAM_ID"), os.Getenv("RUNNER_VERCEL_PROJECT_ID")
	registryHost := os.Getenv("WEAVE_LIVE_REGISTRY_HOST")
	if token == "" || team == "" || project == "" || registryHost == "" {
		t.Skip("Vercel credentials and WEAVE_LIVE_REGISTRY_HOST are required; run `make test-vercel-live`")
	}
	publicBase := "https://" + registryHost
	ctx := context.Background()

	// The proxy, on the port the registry tunnel forwards to.
	verifier, err := vercelsandbox.NewOIDCVerifier(ctx, vercelsandbox.OIDCConfig{TeamID: team, ProjectID: project})
	if err != nil {
		t.Fatal(err)
	}
	recorder := &liveRecorder{}
	listener, err := net.Listen("tcp", "127.0.0.1:8095")
	if err != nil {
		t.Fatalf("the registry tunnel forwards to 127.0.0.1:8095, which is taken: %v", err)
	}
	server := &http.Server{Handler: proxy.New(proxy.Config{PublicBase: publicBase, Verifier: verifier, Recorder: recorder,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))}),
		ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	// A sandbox with the policy the runner manager builds.
	binary, err := os.ReadFile(os.Getenv("RUNNER_VERCEL_BINARY"))
	if err != nil {
		t.Fatal(err)
	}
	backend, err := vercelsandbox.New(vercelsandbox.Config{AppEnv: "test", Token: token, TeamID: team, ProjectID: project,
		Region: "iad1", Scope: "live" + uuid.NewString()[:6], MaxSession: 45 * time.Minute, RunnerBinary: binary})
	if err != nil {
		t.Fatal(err)
	}
	runner := uuid.New()
	var rules []application.EgressRule
	for _, host := range application.GitHubEgressHosts {
		rules = append(rules, application.EgressRule{Host: host})
	}
	for _, host := range application.RegistryHosts {
		rules = append(rules, application.EgressRule{Host: host, ForwardURL: application.RegistryForwardURL(publicBase, host)})
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

	for _, c := range []struct {
		name, script, host, path string
		want                     string
	}{
		{"npm resolves a package through the proxy", "cd /tmp && timeout 60 npm view left-pad version", "registry.npmjs.org", "/left-pad", "1.3.0"},
		{"a pip index page through the proxy", "curl -sS --max-time 30 https://pypi.org/simple/left-pad/ | head -c 200", "pypi.org", "/simple/left-pad/", "left"},
	} {
		exit, out, err := backend.LiveRun(ctx, session, c.script)
		t.Logf("%s: exit=%d output=%q", c.name, exit, strings.TrimSpace(out))
		if err != nil || exit != 0 || !strings.Contains(out, c.want) {
			t.Errorf("%s: exit %d, %v; want success with %q", c.name, exit, err, c.want)
		}
		if !recorder.saw(runner, c.host, c.path) {
			t.Errorf("%s: no record of %s %s for runner %s", c.name, c.host, c.path, runner)
		}
	}
	exit, out, _ := backend.LiveRun(ctx, session, "curl -sS -o /dev/null -w '%{http_code}' --max-time 10 https://example.com 2>&1")
	t.Logf("an unlisted host: exit=%d %q", exit, strings.TrimSpace(out))
	if exit == 0 && strings.HasPrefix(strings.TrimSpace(out), "2") {
		t.Error("an unlisted host was reached under the rules-format policy")
	}

	// From outside: no token, or forged forwarding headers, is refused.
	for name, header := range map[string]http.Header{
		"no token": {"Vercel-Forwarded-Host": {"registry.npmjs.org"}, "Vercel-Forwarded-Path": {"/left-pad"},
			"Vercel-Forwarded-Scheme": {"https"}},
		"a made-up token": {"Vercel-Sandbox-Oidc-Token": {"eyJhbGciOiJSUzI1NiJ9.e30.c2ln"}, "Vercel-Forwarded-Host": {"registry.npmjs.org"},
			"Vercel-Forwarded-Path": {"/left-pad"}, "Vercel-Forwarded-Scheme": {"https"}},
	} {
		req, _ := http.NewRequest(http.MethodGet, publicBase+"/r/registry.npmjs.org/left-pad", nil)
		req.Header = header
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Logf("from outside, %s: %d %s", name, resp.StatusCode, strings.TrimSpace(string(body)))
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("from outside, %s: %d, want 401", name, resp.StatusCode)
		}
	}
	recorder.mu.Lock()
	t.Logf("recorded %d registry requests, all for runner %s:", len(recorder.calls), runner)
	for _, c := range recorder.calls {
		t.Logf("  %s %s%s", c.method, c.host, c.path)
		if c.runner != runner {
			t.Errorf("a request recorded for runner %s", c.runner)
		}
	}
	recorder.mu.Unlock()
}
