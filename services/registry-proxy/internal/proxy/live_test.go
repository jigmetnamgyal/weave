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
	ingress := os.Getenv("WEAVE_LIVE_INGRESS_HOST")
	if ingress != "" {
		rules = append(rules, application.EgressRule{Host: ingress})
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
		// Validate actual wheel content, not just an HTTP response: a proxy
		// 502/504 must never count as files.pythonhosted.org being reached.
		{"a Python wheel through the proxy", `timeout 60 python3 -c 'import urllib.request,json,io,zipfile; j=json.load(urllib.request.urlopen("https://pypi.org/pypi/idna/json",timeout=30)); u=next(f["url"] for f in j["urls"] if f["filename"].endswith(".whl")); assert u.startswith("https://files.pythonhosted.org/"); b=urllib.request.urlopen(u,timeout=30).read(); z=zipfile.ZipFile(io.BytesIO(b)); assert "idna/__init__.py" in z.namelist(); print("verified idna wheel")'`, "files.pythonhosted.org", "/packages/", "verified idna wheel"},
		// A scoped package is requested as /@scope%2fname: refused by the
		// first version, which compared the route decoded (review of PR #26).
		{"a scoped npm package through the proxy", "cd /tmp && timeout 60 npm view @types/left-pad name", "registry.npmjs.org", "/@types%2fleft-pad", "@types/left-pad"},
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
	// The egress matrix, under **the policy that ships** — registries
	// forwarded, git and the ingress plain — rather than a plain-rules policy
	// (review of PR #26). Reached means an HTTP response or a banner came
	// back; a connect is never evidence.
	reaches := func(url string, extra ...string) string {
		return "code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 " + strings.Join(extra, " ") + " '" + url +
			"' 2>/dev/null); [ -n \"$code\" ] && [ \"$code\" != 000 ]"
	}
	matrix := []struct {
		name, script string
		reached      bool
	}{
		{"github.com (plain)", reaches("https://github.com"), true},
		{"example.com (not listed)", reaches("https://example.com"), false},
		{"api.github.com (a subdomain, not listed)", reaches("https://api.github.com"), false},
		{"registry.npmjs.org on port 80", reaches("http://registry.npmjs.org/left-pad"), false},
		{"GitHub by raw IP", reaches("https://140.82.112.3", "-k"), false},
		{"cloud metadata", reaches("http://169.254.169.254/latest/meta-data/"), false},
		{"a private address", reaches("http://10.0.0.1/"), false},
		{"github.com on port 22 (SSH banner)", "timeout 10 bash -c 'exec 3<>/dev/tcp/github.com/22; head -c 4 <&3' 2>/dev/null | grep -q SSH", false},
		{"an unlisted name resolving", "getent hosts example.com >/dev/null", false},
	}
	if ingress != "" {
		matrix = append(matrix, struct {
			name, script string
			reached      bool
		}{"the event ingress (plain)", reaches("https://" + ingress + "/"), true})
	}
	for _, c := range matrix {
		exit, _, err := backend.LiveRun(ctx, session, c.script)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("egress under the shipped policy: %-72s reached=%v", c.name, exit == 0)
		if (exit == 0) != c.reached {
			t.Errorf("egress: %s: reached=%v, want %v", c.name, exit == 0, c.reached)
		}
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
