//go:build vercel_live

// The live acceptance test for the Vercel backend (M5.4b): ADR-013's checklist
// run against a real Vercel account. Opt-in behind a build tag and never run
// in CI — the testing rules forbid a passing test that depends on a paid or
// external service. Run it with `make test-vercel-live`, which builds the
// runner, reads the account from .env, and needs the development tunnel up.
//
// Reachability is judged by an HTTP status or a protocol banner, never by a
// successful connect: the provider spike found blocked connections that look
// open at the TCP layer. Every sandbox is destroyed in cleanup, teardown
// errors are surfaced — the spike's cleanup swallowed them and could not say
// why one sandbox survived — and the scope is checked empty at the end.
package vercelsandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
)

func liveBackend(t *testing.T, scope string, lease time.Duration) *Backend {
	t.Helper()
	token, team, project := os.Getenv("RUNNER_VERCEL_TOKEN"), os.Getenv("RUNNER_VERCEL_TEAM_ID"), os.Getenv("RUNNER_VERCEL_PROJECT_ID")
	if token == "" || team == "" || project == "" {
		t.Skip("RUNNER_VERCEL_TOKEN, _TEAM_ID and _PROJECT_ID are not set; run `make test-vercel-live`")
	}
	binary, err := os.ReadFile(os.Getenv("RUNNER_VERCEL_BINARY"))
	if err != nil {
		t.Fatalf("read the runner binary (make runner-binary): %v", err)
	}
	b, err := New(Config{
		AppEnv: "test", Token: token, TeamID: team, ProjectID: project, Region: "iad1", Scope: scope,
		MaxSession: 45 * time.Minute, Lease: lease, RunnerBinary: binary,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Long enough for a filesystem search to finish inside one waited call.
	b.api.timeout = 5 * time.Minute
	return b
}

func ingressHost(t *testing.T) string {
	t.Helper()
	host := os.Getenv("WEAVE_LIVE_INGRESS_HOST")
	if host == "" {
		t.Skip("WEAVE_LIVE_INGRESS_HOST is not set; start the tunnel with `scripts/dev-tunnel.sh start`")
	}
	return host
}

// sandbox creates a runner sandbox with the production policy and the runner
// installed, destroyed at the end of the test.
func sandbox(t *testing.T, b *Backend, rules []application.EgressRule) (uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	policy, err := buildPolicy(rules)
	if err != nil {
		t.Fatal(err)
	}
	created, err := b.create(ctx, b.sandboxName(id), application.RunnerSpec{RunnerID: id, SessionID: uuid.New()}, policy)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { destroyAndConfirm(t, b, id) })
	if created.Sandbox.Persistent {
		t.Fatal("Vercel created a persistent sandbox despite persistent: false")
	}
	return id, created.Sandbox.CurrentSessionID
}

func destroyAndConfirm(t *testing.T, b *Backend, id uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	if err := b.Destroy(ctx, id, ""); err != nil {
		t.Errorf("teardown of %s: %v", b.sandboxName(id), err)
		return
	}
	if _, err := b.getSandbox(ctx, b.sandboxName(id)); !isNotFound(err) {
		t.Errorf("%s still exists after teardown (%v)", b.sandboxName(id), err)
	}
	if snaps, err := b.snapshotsOf(ctx, b.sandboxName(id)); err != nil || len(snaps) != 0 {
		t.Errorf("%s: snapshots after teardown = %v, %v; want none", b.sandboxName(id), snaps, err)
	}
}

// plainRules allows hosts with no forwarding.
func plainRules(hosts ...string) []application.EgressRule {
	rules := make([]application.EgressRule, 0, len(hosts))
	for _, host := range hosts {
		rules = append(rules, application.EgressRule{Host: host})
	}
	return rules
}

func isNotFound(err error) bool { return errors.Is(err, errNotFound) }

// sh runs a script in the sandbox and returns its exit code.
func sh(t *testing.T, b *Backend, session string, sudo bool, script string) int {
	t.Helper()
	code, err := b.runAndWait(context.Background(), session, commandRequest{
		Command: "bash", Args: []string{"-c", script}, Sudo: sudo,
	})
	if err != nil {
		t.Fatalf("run %q: %v", script, err)
	}
	return code
}

// reaches is a script that exits 0 only if an HTTP response came back from
// the URL — any status at all. A refused, reset or unresolvable destination
// gets none.
func reaches(url string, extra ...string) string {
	return fmt.Sprintf(`code=$(curl -sS -o /dev/null -w '%%{http_code}' --max-time 10 %s %q 2>/dev/null); `+
		`[ -n "$code" ] && [ "$code" != "000" ]`, strings.Join(extra, " "), url)
}

// TestLiveEgressIsExactlyThePolicy is ADR-013's egress item, on the policy the
// runner manager builds: the default allowlist and the event ingress reached,
// everything else refused — by an HTTP response, a banner, or a name that
// does not resolve.
func TestLiveEgressIsExactlyThePolicy(t *testing.T) {
	b := liveBackend(t, "live"+uuid.NewString()[:6], DefaultLease)
	ingress := ingressHost(t)
	_, session := sandbox(t, b, plainRules(append(append([]string(nil), application.DefaultEgressHosts...), ingress)...))

	if sh(t, b, session, false, "command -v curl && command -v getent") != 0 {
		t.Fatal("curl or getent is missing from the image; the checks below would prove nothing")
	}
	for _, c := range []struct {
		name, script string
		reached      bool
	}{
		{"github.com (allowlisted)", reaches("https://github.com"), true},
		{"registry.npmjs.org (allowlisted)", reaches("https://registry.npmjs.org/"), true},
		{"the event ingress (allowlisted)", reaches("https://" + ingress + "/"), true},
		{"example.com (not listed)", reaches("https://example.com"), false},
		{"api.github.com (a subdomain, not listed)", reaches("https://api.github.com"), false},
		{"github.com on port 80", reaches("http://github.com"), false},
		{"GitHub by raw IP", reaches("https://140.82.112.3", "-k"), false},
		{"cloud metadata", reaches("http://169.254.169.254/latest/meta-data/"), false},
		{"a private address", reaches("http://10.0.0.1/"), false},
		{"github.com on port 22 (SSH banner)",
			`timeout 10 bash -c 'exec 3<>/dev/tcp/github.com/22; head -c 4 <&3' 2>/dev/null | grep -q SSH`, false},
		{"an unlisted name resolving", `getent hosts example.com >/dev/null`, false},
		// An allowlisted SNI dialled at another address still reaches the
		// real host: Vercel resolves the name itself, so --resolve cannot
		// point an allowed name somewhere else.
		{"github.com's SNI aimed at another IP reaches the real GitHub",
			`curl -sS -D - -o /dev/null --max-time 10 --resolve github.com:443:93.184.215.14 https://github.com 2>/dev/null | grep -qi '^server: github.com'`, true},
	} {
		got := sh(t, b, session, false, c.script) == 0
		t.Logf("egress: %-60s reached=%v", c.name, got)
		if got != c.reached {
			t.Errorf("egress: %s: reached=%v, want %v", c.name, got, c.reached)
		}
	}
}

// TestLiveTheRunnerUserHasNoPrivilege: the sandbox's default user has
// passwordless sudo; the runner's user must have none — not through sudo,
// with or without no-new-privs, and no capabilities.
func TestLiveTheRunnerUserHasNoPrivilege(t *testing.T) {
	b := liveBackend(t, "live"+uuid.NewString()[:6], DefaultLease)
	_, session := sandbox(t, b, nil)
	if err := b.install(context.Background(), session); err != nil {
		t.Fatalf("install: %v", err)
	}
	as := "setpriv --reuid=" + runnerUser + " --regid=" + runnerUser + " --clear-groups "
	for _, c := range []struct {
		name, script string
		want         int
	}{
		{"the default user can sudo (the control)", "sudo -n true", 0},
		{"the runner user cannot sudo", as + "sudo -n true", 1},
		{"the runner user cannot sudo under no-new-privs", as + "--no-new-privs sudo -n true", 1},
		{"the runner user holds no capabilities", as + `--no-new-privs grep -q '^CapEff:[[:space:]]*0000000000000000$' /proc/self/status`, 0},
		{"the runner is installed root-owned and not writable by its user", as + "test ! -w " + runnerPath, 0},
		{"the workspace belongs to the runner user", as + "test -w " + workspaceDir, 0},
	} {
		sudo := c.name != "the default user can sudo (the control)"
		got := sh(t, b, session, sudo, c.script)
		ok := (got == 0) == (c.want == 0)
		t.Logf("privilege: %-60s exit=%d", c.name, got)
		if !ok {
			t.Errorf("privilege: %s: exit %d, want %s", c.name, got, map[bool]string{true: "0", false: "non-zero"}[c.want == 0])
		}
	}
}

// TestLiveTheRunnerStartsAndItsSecretsStayOffDisk goes through Provision
// itself — upload, install, checksum, start as the runner user — and then
// searches the whole filesystem, as root, for the secret it was given. The
// clone is pointed at a repository that does not exist, so the runner runs as
// far as its checkout and exits there (clone failed), which also proves the
// start path works as the runner user.
func TestLiveTheRunnerStartsAndItsSecretsStayOffDisk(t *testing.T) {
	b := liveBackend(t, "live"+uuid.NewString()[:6], DefaultLease)
	ctx := context.Background()
	marker := "weave-live-secret-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	sessionID := uuid.New()
	spec := application.RunnerSpec{
		RunnerID: uuid.New(), SessionID: sessionID, WorkspaceID: uuid.New(),
		CloneURL: "https://github.com/weave-live-acceptance/does-not-exist.git", Branch: "weave/live",
		Commit: strings.Repeat("a", 40), GitToken: marker, NATSURL: "wss://" + ingressHost(t),
		NATS: application.RunnerCredentials{Creds: marker + "-nats",
			Subject: "weave.session." + sessionID.String() + ".events", InboxPrefix: "_INBOX_x"},
		Provider: "fake", Model: "deterministic-v1",
		Egress: plainRules(append(append([]string(nil), application.DefaultEgressHosts...), ingressHost(t))...),
	}
	handle, err := b.Provision(ctx, spec)
	t.Cleanup(func() { destroyAndConfirm(t, b, spec.RunnerID) })
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	again, err := b.Provision(ctx, spec)
	if err != nil || again != handle {
		t.Errorf("a redelivered provision = %q, %v; want the same runner %q", again, err, handle)
	}

	var status application.RunnerStatus
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if status, err = b.Status(ctx, handle); err != nil {
			t.Fatal(err)
		}
		if !status.Running {
			break
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("runner: status %+v", status)
	h, _ := parseHandle(handle)
	if !status.Exists || status.Running || status.ExitCode != application.RunnerExitCloneFailed {
		// The runner's own output, to say why. It never prints a secret.
		t.Logf("runner output:\n%s", commandOutput(t, b, h.session, h.command))
		t.Errorf("the runner = %+v; want it to have run to its checkout and exited %d (clone failed)",
			status, application.RunnerExitCloneFailed)
	}

	found := sh(t, b, h.session, true, `grep -rIlqF --exclude-dir=proc --exclude-dir=sys --exclude-dir=dev `+marker+` /`)
	t.Logf("secrets: the git token or broker credential on disk (searched as root): %v", found == 0)
	if found == 0 {
		t.Error("a secret given to the runner's start command was found on the sandbox's filesystem")
	}
	if sh(t, b, h.session, true, "id -u") != 0 {
		t.Error("the search did not run as root")
	}
}

// TestLiveALapsedLeaseStopsTheSandboxAndKeepsNothing: with no runner manager
// extending it, a sandbox stops on its own at its lease — teardown on lost
// heartbeat, from the provider's side — and leaves no snapshot. So does an
// explicit stop.
func TestLiveALapsedLeaseStopsTheSandboxAndKeepsNothing(t *testing.T) {
	const lease = time.Minute
	b := liveBackend(t, "live"+uuid.NewString()[:6], lease)
	ctx := context.Background()
	lapsed, _ := sandbox(t, b, nil)
	stoppedID, stoppedSession := sandbox(t, b, nil)

	if err := b.api.do(ctx, request{method: "POST", path: "/v2/sandboxes/sessions/" + stoppedSession + "/stop",
		body: map[string]any{}, accept: []int{200, 202, 204}}, nil); err != nil {
		t.Fatalf("stop: %v", err)
	}

	start := time.Now()
	var status string
	for time.Since(start) < lease+3*time.Minute {
		got, err := b.getSandbox(ctx, b.sandboxName(lapsed))
		if err != nil {
			t.Fatal(err)
		}
		if status = got.Sandbox.Status; status == "stopped" {
			break
		}
		time.Sleep(5 * time.Second)
	}
	t.Logf("lease: an unextended %s sandbox was %s after %s", lease, status, time.Since(start).Round(time.Second))
	if status != "stopped" {
		t.Errorf("an unextended sandbox is %s %s after creation; want stopped by its lease", status, time.Since(start))
	}
	for name, id := range map[string]uuid.UUID{"after the lease lapsed": lapsed, "after an explicit stop": stoppedID} {
		snaps, err := b.snapshotsOf(ctx, b.sandboxName(id))
		t.Logf("snapshots %s: %v", name, snaps)
		if err != nil || len(snaps) != 0 {
			t.Errorf("snapshots %s = %v, %v; want none", name, snaps, err)
		}
	}
}

// commandOutput reads a command's output through the logs API, for
// diagnosing a runner that did not behave.
func commandOutput(t *testing.T, b *Backend, session, command string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, b.api.base+"/v2/sandboxes/sessions/"+session+"/cmd/"+command+
		"/logs?teamId="+b.api.teamID, nil)
	req.Header.Set("Authorization", "Bearer "+b.api.token)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	resp, err := b.api.http.Do(req.WithContext(ctx))
	if err != nil {
		return "(logs unavailable: " + err.Error() + ")"
	}
	defer func() { _ = resp.Body.Close() }()
	var out strings.Builder
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1<<20))
	for {
		var line struct {
			Data string `json:"data"`
		}
		if decoder.Decode(&line) != nil {
			break
		}
		out.WriteString(line.Data)
	}
	return out.String()
}

// TestLiveALeaseExtendedByRunnerIDOutlivesItsFirstExpiry: renewal the way the
// runner manager does it — by runner id, on a sandbox with no runner started,
// as during provisioning (review of PR #24) — really moves Vercel's expiry,
// and the sandbox is still running past the moment its first lease ended.
func TestLiveALeaseExtendedByRunnerIDOutlivesItsFirstExpiry(t *testing.T) {
	const lease = time.Minute
	b := liveBackend(t, "live"+uuid.NewString()[:6], lease)
	ctx := context.Background()
	id, _ := sandbox(t, b, nil)
	before, err := b.getSandbox(ctx, b.sandboxName(id))
	if err != nil {
		t.Fatal(err)
	}
	firstExpiry := time.UnixMilli(before.Sandbox.ExpiresAt)

	time.Sleep(30 * time.Second)
	if err := b.ExtendLease(ctx, id); err != nil {
		t.Fatalf("extend: %v", err)
	}
	after, err := b.getSandbox(ctx, b.sandboxName(id))
	if err != nil {
		t.Fatal(err)
	}
	newExpiry := time.UnixMilli(after.Sandbox.ExpiresAt)
	t.Logf("lease: expiry moved by %s", newExpiry.Sub(firstExpiry).Round(time.Second))
	if newExpiry.Sub(firstExpiry) < 20*time.Second {
		t.Errorf("expiry moved by %s after an extension 30s in; want about 30s", newExpiry.Sub(firstExpiry))
	}

	time.Sleep(time.Until(firstExpiry.Add(15 * time.Second)))
	now, err := b.getSandbox(ctx, b.sandboxName(id))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("lease: 15s past its first expiry the sandbox is %s", now.Sandbox.Status)
	if now.Sandbox.Status != "running" {
		t.Errorf("15s past its first expiry the extended sandbox is %s; want running", now.Sandbox.Status)
	}
}
