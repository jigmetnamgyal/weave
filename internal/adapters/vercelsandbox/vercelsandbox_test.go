package vercelsandbox

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// elfHeader is the smallest ELF64 file debug/elf accepts, for a machine.
func elfHeader(machine uint16) []byte {
	h := make([]byte, 64)
	copy(h, "\x7fELF")
	h[4], h[5], h[6] = 2, 1, 1 // 64-bit, little-endian, version 1
	binary.LittleEndian.PutUint16(h[16:], 2)
	binary.LittleEndian.PutUint16(h[18:], machine)
	binary.LittleEndian.PutUint32(h[20:], 1)
	binary.LittleEndian.PutUint16(h[52:], 64)
	binary.LittleEndian.PutUint16(h[54:], 56)
	binary.LittleEndian.PutUint16(h[58:], 64)
	return h
}

const (
	secretToken = "ghs_TESTTOKENnotreal"             // ggignore — synthetic
	secretCreds = "-----BEGIN NATS USER JWT-----xyz" // synthetic
)

// fakeVercel is Vercel's sandbox API, as much of it as the backend calls,
// recording every request so tests can say where each value travelled.
type fakeVercel struct {
	t  *testing.T
	mu sync.Mutex

	sandboxes map[string]*fakeSandbox
	snapshots map[string][]string // sandbox name -> snapshot ids
	requests  []recorded

	creates, starts, healthchecks, extends int
	healthy                                bool
	// keepSnapshots makes snapshots survive deletion, as a Vercel fault would.
	keepSnapshots bool
	// failCreate answers creates with this status when non-zero.
	failCreate int
	// rateLimitOnce answers the next request 429 with Retry-After.
	rateLimitOnce bool
	clock         int64
	// echoStart refuses the runner's start command, echoing its body in the
	// error message, as a hostile or buggy server might.
	echoStart  bool
	lastExtend int64
}

type fakeSandbox struct {
	name, session, status string
	persistent            bool
	timeout               int64
	createdAt             time.Time
	tags                  map[string]string
	policy                json.RawMessage
	commands              []*fakeCommand
}

type fakeCommand struct {
	id, name string
	args     []string
	exit     *int
	env      map[string]string
	sudo     bool
	// startedAt orders commands; Vercel lists newest first.
	startedAt int64
	// revealed: as measured live, Vercel reports a command's exit only once
	// something has read it with wait=true; until then every plain read and
	// the list say exitCode null.
	revealed bool
}

type recorded struct {
	method, path, query string
	body                []byte
}

func newFakeVercel(t *testing.T) (*fakeVercel, *httptest.Server) {
	f := &fakeVercel{t: t, sandboxes: map[string]*fakeSandbox{}, snapshots: map[string][]string{}, healthy: true}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	return f, server
}

func (f *fakeVercel) byName(name string) *fakeSandbox { return f.sandboxes[name] }

func (f *fakeVercel) bySession(session string) *fakeSandbox {
	for _, s := range f.sandboxes {
		if s.session == session {
			return s
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeVercel) sandboxJSON(s *fakeSandbox) map[string]any {
	return map[string]any{
		"sandbox": map[string]any{
			"name": s.name, "status": s.status, "currentSessionId": s.session, "persistent": s.persistent,
			"tags": s.tags, "expiresAt": s.createdAt.Add(time.Duration(s.timeout) * time.Millisecond).UnixMilli(),
		},
		"session": map[string]any{"id": s.session, "status": s.status, "timeout": s.timeout},
	}
}

func commandJSON(c *fakeCommand) map[string]any {
	var exit *int
	if c.revealed {
		exit = c.exit
	}
	return map[string]any{"command": map[string]any{"id": c.id, "name": c.name, "args": c.args,
		"exitCode": exit, "startedAt": c.startedAt}}
}

func (f *fakeVercel) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	f.requests = append(f.requests, recorded{r.Method, r.URL.Path, r.URL.RawQuery, body})
	if r.Header.Get("Authorization") != "Bearer test-token" || r.URL.Query().Get("teamId") != "team_1" {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": map[string]string{"code": "forbidden"}})
		return
	}
	if f.rateLimitOnce {
		f.rateLimitOnce = false
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]string{"code": "rate_limited"}})
		return
	}
	path := r.URL.Path
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	switch {
	case r.Method == http.MethodPost && path == "/v4/sandboxes":
		if f.failCreate != 0 {
			writeJSON(w, f.failCreate, map[string]any{"error": map[string]string{"code": "payment_required", "message": "plan limit"}})
			return
		}
		var req struct {
			Name       string            `json:"name"`
			Persistent *bool             `json:"persistent"`
			Timeout    int64             `json:"timeout"`
			Tags       map[string]string `json:"tags"`
			Policy     json.RawMessage   `json:"networkPolicy"`
		}
		_ = json.Unmarshal(body, &req)
		if _, exists := f.sandboxes[req.Name]; exists {
			writeJSON(w, http.StatusConflict, map[string]any{"error": map[string]string{"code": "conflict"}})
			return
		}
		f.creates++
		persistent := req.Persistent == nil || *req.Persistent // Vercel's default is true
		s := &fakeSandbox{name: req.Name, session: "sbx_" + uuid.NewString()[:8], status: "running",
			persistent: persistent, timeout: req.Timeout, createdAt: time.Now(), tags: req.Tags, policy: req.Policy}
		f.sandboxes[req.Name] = s
		writeJSON(w, http.StatusOK, f.sandboxJSON(s))

	case r.Method == http.MethodGet && path == "/v2/sandboxes":
		tag := strings.SplitN(r.URL.Query().Get("tags"), ":", 2)
		var list []any
		for _, s := range f.sandboxes {
			if len(tag) == 2 && s.tags[tag[0]] == tag[1] {
				list = append(list, f.sandboxJSON(s)["sandbox"])
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"sandboxes": list, "pagination": map[string]any{"count": len(list), "next": nil}})

	case r.Method == http.MethodGet && path == "/v2/sandboxes/snapshots":
		var list []any
		for _, id := range f.snapshots[r.URL.Query().Get("name")] {
			list = append(list, map[string]any{"id": id, "status": "created"})
		}
		writeJSON(w, http.StatusOK, map[string]any{"snapshots": list, "pagination": map[string]any{"count": len(list), "next": nil}})

	case r.Method == http.MethodDelete && len(parts) == 4 && parts[2] == "snapshots":
		if !f.keepSnapshots {
			for name, ids := range f.snapshots {
				var kept []string
				for _, id := range ids {
					if id != parts[3] {
						kept = append(kept, id)
					}
				}
				f.snapshots[name] = kept
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{})

	case len(parts) == 3 && parts[1] == "sandboxes" && parts[2] != "sessions":
		s := f.byName(parts[2])
		if r.URL.Query().Get("resume") != "" {
			f.t.Errorf("a get asked to resume %s; a stopped sandbox must never be brought back", parts[2])
		}
		if s == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found"}})
			return
		}
		if r.Method == http.MethodDelete {
			if r.URL.Query().Get("deleteOrphanSnapshots") != "true" {
				f.t.Error("a delete did not ask for orphaned snapshots to be removed")
			}
			delete(f.sandboxes, parts[2])
			if !f.keepSnapshots {
				delete(f.snapshots, parts[2])
			}
			writeJSON(w, http.StatusOK, map[string]any{})
			return
		}
		writeJSON(w, http.StatusOK, f.sandboxJSON(s))

	case len(parts) >= 4 && parts[2] == "sessions":
		s := f.bySession(parts[3])
		if s == nil {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found"}})
			return
		}
		f.serveSession(w, r, s, parts[4:], body)

	default:
		f.t.Errorf("unexpected request %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeVercel) serveSession(w http.ResponseWriter, r *http.Request, s *fakeSandbox, rest []string, body []byte) {
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"session": f.sandboxJSON(s)["session"]})
	case len(rest) == 2 && rest[0] == "fs" && rest[1] == "write":
		if r.Header.Get("Content-Type") != "application/gzip" || r.Header.Get("X-Cwd") != extractDir {
			f.t.Errorf("upload with content type %q to %q", r.Header.Get("Content-Type"), r.Header.Get("X-Cwd"))
		}
		writeJSON(w, http.StatusOK, map[string]any{})
	case len(rest) == 1 && rest[0] == "extend-timeout":
		var req struct {
			Duration int64 `json:"duration"`
		}
		_ = json.Unmarshal(body, &req)
		f.extends++
		f.lastExtend = req.Duration
		s.timeout += req.Duration
		writeJSON(w, http.StatusOK, map[string]any{"session": f.sandboxJSON(s)["session"]})
	case len(rest) == 1 && rest[0] == "cmd" && r.Method == http.MethodGet:
		var list []any
		for i := len(s.commands) - 1; i >= 0; i-- { // newest first, as Vercel lists
			list = append(list, commandJSON(s.commands[i])["command"])
		}
		writeJSON(w, http.StatusOK, map[string]any{"commands": list})
	case len(rest) == 1 && rest[0] == "cmd" && r.Method == http.MethodPost:
		var req commandRequest
		_ = json.Unmarshal(body, &req)
		f.clock++
		c := &fakeCommand{id: "cmd_" + uuid.NewString()[:8], name: req.Command, args: req.Args, env: req.Env,
			sudo: req.Sudo, startedAt: f.clock}
		zero, one, dup := 0, 1, startedDuplicateExit
		if f.echoStart && len(req.Args) == 2 && req.Args[1] == startScript {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "bad_request", "message": string(body)}})
			return
		}
		switch {
		case len(req.Args) == 2 && req.Args[1] == startScript:
			// The in-sandbox marker: only the first start runs.
			for _, other := range s.commands {
				if len(other.args) == 2 && other.args[1] == startScript {
					c.exit = &dup
				}
			}
			if c.exit == nil {
				f.starts++
			}
		case req.Command == runnerPath && len(req.Args) == 1 && req.Args[0] == "healthcheck":
			f.healthchecks++
			if f.healthy {
				c.exit = &zero
			} else {
				c.exit = &one
			}
		default:
			c.exit = &zero
		}
		s.commands = append(s.commands, c)
		writeJSON(w, http.StatusOK, commandJSON(c))
	case len(rest) == 2 && rest[0] == "cmd" && r.Method == http.MethodGet:
		for _, c := range s.commands {
			if c.id != rest[1] {
				continue
			}
			if r.URL.Query().Get("wait") == "true" {
				if c.exit == nil {
					// A running command: wait=true blocks until it ends,
					// or until the caller gives up.
					f.mu.Unlock()
					<-r.Context().Done()
					f.mu.Lock()
					return
				}
				c.revealed = true
			}
			writeJSON(w, http.StatusOK, commandJSON(c))
			return
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]string{"code": "not_found"}})
	default:
		f.t.Errorf("unexpected session request %s %v", r.Method, rest)
		w.WriteHeader(http.StatusNotFound)
	}
}

// finish marks the runner's command as exited.
func (f *fakeVercel) finish(sandbox string, code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.sandboxes[sandbox].commands {
		if len(c.args) == 2 && c.args[1] == startScript && c.exit == nil {
			c.exit = &code
		}
	}
}

// inProcess serves the fake in the calling goroutine, with the caller's
// context. A wait=true read of a running command blocks in the fake until the
// caller's poll bound and then returns within the same call — so no handler
// outlives the request that started it. Over a real socket, a request the
// client had already given up on could reach its handler afterwards and touch
// the fake while a test read it; the race detector found exactly that.
type inProcess struct{ handler http.Handler }

func (t inProcess) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Body == nil {
		r.Body = http.NoBody // as a server would present a bodiless request
	}
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, r)
	if err := r.Context().Err(); err != nil {
		return nil, err
	}
	return recorder.Result(), nil
}

func testConfig(server *httptest.Server) Config {
	return Config{
		AppEnv: "test", Token: "test-token", TeamID: "team_1", ProjectID: "prj_1", Region: "iad1",
		Scope: "t1", MaxSession: 45 * time.Minute, Lease: 5 * time.Minute,
		RunnerBinary: elfHeader(62), BaseURL: server.URL, HTTPClient: server.Client(),
	}
}

func newTestBackend(t *testing.T) (*Backend, *fakeVercel) {
	t.Helper()
	fake, server := newFakeVercel(t)
	backend, err := New(testConfig(server))
	if err != nil {
		t.Fatal(err)
	}
	backend.api.sleep = func(context.Context, time.Duration) error { return nil }
	backend.statusPoll = 50 * time.Millisecond
	// In-process, synchronously: see inProcess.
	backend.api.http = &http.Client{Transport: inProcess{http.HandlerFunc(fake.serve)}}
	return backend, fake
}

var testHosts = []string{"github.com", "codeload.github.com", "events.example.test"}

func testSpec() application.RunnerSpec {
	return application.RunnerSpec{
		RunnerID: uuid.New(), SessionID: uuid.New(), WorkspaceID: uuid.New(),
		CloneURL: "https://github.com/acme/app.git", Branch: "weave/x", Commit: strings.Repeat("a", 40),
		GitToken: secretToken, NATSURL: "wss://events.example.test",
		NATS:     application.RunnerCredentials{Creds: secretCreds, Subject: "weave.session.x.events", InboxPrefix: "_INBOX_x"},
		Provider: "fake", Model: "deterministic-v1",
		EgressHosts: testHosts,
	}
}

// TestACreateIsNonPersistentWithExactlyItsHosts: the two defaults ADR-013
// refuses — a persistent sandbox and an open network — are stated away in
// every create.
func TestACreateIsNonPersistentWithExactlyItsHosts(t *testing.T) {
	backend, fake := newTestBackend(t)
	spec := testSpec()
	if _, err := backend.Provision(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	var create map[string]any
	for _, r := range fake.requests {
		if r.method == http.MethodPost && r.path == "/v4/sandboxes" {
			_ = json.Unmarshal(r.body, &create)
		}
	}
	if persistent, stated := create["persistent"]; !stated || persistent != false {
		t.Errorf("persistent = %v (stated %v); every create must say false", persistent, stated)
	}
	policy, _ := json.Marshal(create["networkPolicy"])
	if want := `{"allowedDomains":["github.com","codeload.github.com","events.example.test"],"mode":"custom"}`; string(policy) != want {
		t.Errorf("network policy = %s, want %s", policy, want)
	}
	if _, has := create["env"]; has {
		t.Error("the create carried an environment; it is sandbox configuration every command inherits")
	}
	if _, has := create["ports"]; has {
		t.Error("the create exposed ports; a runner has no inbound route")
	}
	if create["timeout"] != float64((5 * time.Minute).Milliseconds()) {
		t.Errorf("timeout = %v, want the lease", create["timeout"])
	}
	if create["name"] != "weave-runner-t1-"+spec.RunnerID.String() {
		t.Errorf("name = %v, want one derived from the runner id", create["name"])
	}
	tags, _ := create["tags"].(map[string]any)
	if tags[tagScope] != "t1" || tags[tagRunner] != spec.RunnerID.String() || tags[tagSession] != spec.SessionID.String() {
		t.Errorf("tags = %v", tags)
	}
	if backend.Name() != "vercel-t1" {
		t.Errorf("name on runner rows = %q", backend.Name())
	}
}

// TestSecretsTravelOnlyInTheStartCommandsEnvironment: the git token and the
// broker credential appear in exactly one request, in its env, never in an
// argument — arguments are returned by the API and visible in the process
// table.
func TestSecretsTravelOnlyInTheStartCommandsEnvironment(t *testing.T) {
	backend, fake := newTestBackend(t)
	if _, err := backend.Provision(context.Background(), testSpec()); err != nil {
		t.Fatal(err)
	}
	carrying := 0
	for _, r := range fake.requests {
		for _, secret := range []string{secretToken, "NATS USER JWT"} {
			if strings.Contains(r.query, secret) || strings.Contains(r.path, secret) {
				t.Errorf("%s %s carries a secret in its URL", r.method, r.path)
			}
		}
		if !strings.Contains(string(r.body), secretToken) {
			continue
		}
		carrying++
		var cmd commandRequest
		if err := json.Unmarshal(r.body, &cmd); err != nil || cmd.Env["WEAVE_GIT_TOKEN"] != secretToken ||
			cmd.Env["WEAVE_NATS_CREDS"] != secretCreds {
			t.Errorf("%s %s carries the token outside a command's env", r.method, r.path)
		}
		for _, arg := range cmd.Args {
			if strings.Contains(arg, secretToken) || strings.Contains(arg, "NATS USER") {
				t.Error("a secret is in a command's arguments")
			}
		}
		if !cmd.Sudo || len(cmd.Args) != 2 || cmd.Args[1] != startScript {
			t.Errorf("the secrets went to a command other than the runner's start: %+v", cmd.Args)
		}
	}
	if carrying != 1 {
		t.Errorf("the git token travelled in %d requests, want exactly the start command", carrying)
	}
}

// TestTheRunnerStartsAsItsOwnUserWithoutPrivilege: the start script drops to
// the runner user with no-new-privs, and exec's so no parent keeps the
// secrets' environment.
func TestTheRunnerStartsAsItsOwnUserWithoutPrivilege(t *testing.T) {
	for _, want := range []string{"--reuid=weave-runner", "--regid=weave-runner", "--clear-groups", "--no-new-privs", "exec setpriv", "mkdir /run/weave-runner-started"} {
		if !strings.Contains(startScript, want) {
			t.Errorf("the start script lacks %q", want)
		}
	}
	install := installScript("abc")
	for _, want := range []string{"useradd --system", "sha256sum --check", "install -d -o weave-runner"} {
		if !strings.Contains(install, want) {
			t.Errorf("the install script lacks %q", want)
		}
	}
	if strings.Contains(install, "sudo") || strings.Contains(install, "usermod") {
		t.Error("the install script grants the runner user something")
	}
}

// TestProvisionIsIdempotent: a redelivered Provision finds the sandbox and
// the runner it made; a create that loses a race reads the winner's; and a
// start reaching a sandbox whose runner already runs does not run twice.
func TestProvisionIsIdempotent(t *testing.T) {
	backend, fake := newTestBackend(t)
	spec := testSpec()
	first, err := backend.Provision(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	second, err := backend.Provision(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || fake.creates != 1 || fake.starts != 1 {
		t.Errorf("handles %q / %q; %d creates, %d starts; want one of each and the same handle",
			first, second, fake.creates, fake.starts)
	}
	found, ok, err := backend.HandleFor(context.Background(), spec.RunnerID)
	if err != nil || !ok || found != first {
		t.Errorf("HandleFor = %q, %v, %v; want %q", found, ok, err, first)
	}

	// A lost race on create: the sandbox exists, the create answers 409.
	other := testSpec()
	fake.mu.Lock()
	fake.sandboxes[backend.sandboxName(other.RunnerID)] = &fakeSandbox{
		name: backend.sandboxName(other.RunnerID), session: "sbx_race", status: "running", createdAt: time.Now(),
		timeout: 300000, tags: map[string]string{tagScope: "t1", tagRunner: other.RunnerID.String()},
	}
	fake.mu.Unlock()
	if _, err := backend.create(context.Background(), backend.sandboxName(other.RunnerID), other, networkPolicy{Mode: "deny-all"}); err != nil {
		t.Errorf("a create that lost a race = %v, want the existing sandbox", err)
	}
}

// TestASandboxWithoutItsRunnerIsNotAnEnvironment: a Provision that died after
// creating the sandbox and before starting the runner left a sandbox that
// cannot run. HandleFor does not answer for it; the next Provision finishes
// it rather than creating a second.
func TestASandboxWithoutItsRunnerIsNotAnEnvironment(t *testing.T) {
	backend, fake := newTestBackend(t)
	spec := testSpec()
	if _, err := backend.create(context.Background(), backend.sandboxName(spec.RunnerID), spec, networkPolicy{Mode: "deny-all"}); err != nil {
		t.Fatal(err)
	}
	if _, found, err := backend.HandleFor(context.Background(), spec.RunnerID); err != nil || found {
		t.Errorf("HandleFor answered for a sandbox with no runner: %v, %v", found, err)
	}
	if _, err := backend.Provision(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if fake.creates != 1 || fake.starts != 1 {
		t.Errorf("%d creates, %d starts; want the existing sandbox finished", fake.creates, fake.starts)
	}
}

// TestAStoppedSandboxIsALostRunner: never resumed, never replaced.
func TestAStoppedSandboxIsALostRunner(t *testing.T) {
	backend, fake := newTestBackend(t)
	spec := testSpec()
	h, err := backend.Provision(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	fake.sandboxes[backend.sandboxName(spec.RunnerID)].status = "stopped"
	fake.mu.Unlock()
	status, err := backend.Status(context.Background(), h)
	if err != nil || status.Exists {
		t.Errorf("status of a stopped sandbox = %+v, %v; want gone", status, err)
	}
	if _, err := backend.Provision(context.Background(), spec); !errors.Is(err, application.ErrRunnerLost) {
		t.Errorf("provisioning into a stopped sandbox = %v, want ErrRunnerLost", err)
	}
	if fake.creates != 1 {
		t.Errorf("%d creates; a stopped sandbox must not be replaced by a second", fake.creates)
	}
}

// TestStatusReportsReadinessOnceAndTheExit: readiness from the runner's own
// healthcheck, asked until it first says yes; then the command's exit code.
func TestStatusReportsReadinessOnceAndTheExit(t *testing.T) {
	backend, fake := newTestBackend(t)
	spec := testSpec()
	h, err := backend.Provision(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	fake.healthy = false
	if status, _ := backend.Status(context.Background(), h); !status.Exists || !status.Running || status.Ready {
		t.Errorf("before ready: %+v", status)
	}
	fake.healthy = true
	for i := 0; i < 5; i++ {
		if status, _ := backend.Status(context.Background(), h); !status.Ready {
			t.Errorf("after ready: %+v", status)
		}
	}
	if fake.healthchecks != 2 {
		t.Errorf("%d healthchecks; readiness once seen must not be asked again", fake.healthchecks)
	}
	fake.finish(backend.sandboxName(spec.RunnerID), 6)
	if status, _ := backend.Status(context.Background(), h); !status.Exists || status.Running || status.ExitCode != 6 {
		t.Errorf("after exit: %+v, want exited 6", status)
	}
}

// TestDestroyLeavesNoSnapshot: 404 is success; a snapshot left behind is
// deleted; one that survives is an error, never a success.
func TestDestroyLeavesNoSnapshot(t *testing.T) {
	backend, fake := newTestBackend(t)
	if err := backend.Destroy(context.Background(), uuid.New(), ""); err != nil {
		t.Errorf("destroying what is already gone = %v, want success", err)
	}

	spec := testSpec()
	name := backend.sandboxName(spec.RunnerID)
	if _, err := backend.Provision(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	fake.keepSnapshots = true // deletion leaves a snapshot behind...
	fake.snapshots[name] = []string{"snap_1"}
	fake.keepSnapshots = false // ...which an explicit delete then removes
	fake.mu.Lock()
	delete(fake.sandboxes, name) // as though the sandbox went but its snapshot lingered
	fake.mu.Unlock()
	if err := backend.Destroy(context.Background(), spec.RunnerID, ""); err != nil {
		t.Errorf("destroy with a lingering snapshot = %v, want it deleted", err)
	}
	if len(fake.snapshots[name]) != 0 {
		t.Errorf("snapshots left: %v", fake.snapshots[name])
	}

	fake.snapshots[name] = []string{"snap_stuck"}
	fake.keepSnapshots = true
	if err := backend.Destroy(context.Background(), spec.RunnerID, ""); !errors.Is(err, ErrSnapshotRetained) {
		t.Errorf("destroy with a snapshot that will not go = %v, want ErrSnapshotRetained", err)
	}
}

// TestListSeesOnlyThisScopesRunners: another scope's sandbox, or one whose
// name and tag disagree, is never reported — and so never destroyed as an
// orphan.
func TestListSeesOnlyThisScopesRunners(t *testing.T) {
	backend, fake := newTestBackend(t)
	spec := testSpec()
	if _, err := backend.Provision(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	foreign := uuid.New()
	fake.sandboxes["weave-runner-other-"+foreign.String()] = &fakeSandbox{name: "weave-runner-other-" + foreign.String(),
		status: "running", tags: map[string]string{tagScope: "other", tagRunner: foreign.String()}}
	fake.sandboxes["someone-elses"] = &fakeSandbox{name: "someone-elses", status: "running",
		tags: map[string]string{tagScope: "t1", tagRunner: foreign.String()}}
	listed, err := backend.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].RunnerID != spec.RunnerID {
		t.Errorf("listed %+v, want only this scope's runner", listed)
	}
}

// TestTheLeaseIsExtendedToALeaseFromNowNeverPastTheCap.
func TestTheLeaseIsExtendedToALeaseFromNowNeverPastTheCap(t *testing.T) {
	backend, fake := newTestBackend(t)
	spec := testSpec()
	h, err := backend.Provision(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	name := backend.sandboxName(spec.RunnerID)
	start := fake.sandboxes[name].createdAt

	// Three minutes in, the lease has two minutes left: extend by three.
	backend.now = func() time.Time { return start.Add(3 * time.Minute) }
	if err := backend.ExtendLease(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if fake.lastExtend != (3 * time.Minute).Milliseconds() {
		t.Errorf("extended by %s, want 3m", time.Duration(fake.lastExtend)*time.Millisecond)
	}

	// Near the 45-minute cap: extend only up to it.
	backend.now = func() time.Time { return start.Add(42 * time.Minute) }
	fake.sandboxes[name].timeout = (43 * time.Minute).Milliseconds()
	if err := backend.ExtendLease(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if fake.lastExtend != (2*time.Minute).Milliseconds() || fake.sandboxes[name].timeout != (45*time.Minute).Milliseconds() {
		t.Errorf("extended by %s to %s; want clamped at the 45m cap",
			time.Duration(fake.lastExtend)*time.Millisecond, time.Duration(fake.sandboxes[name].timeout)*time.Millisecond)
	}
	extends := fake.extends
	if err := backend.ExtendLease(context.Background(), h); err != nil || fake.extends != extends {
		t.Errorf("at the cap: %v, %d extensions; want none", err, fake.extends-extends)
	}

	// Stopped: nothing to keep alive, and not an error.
	fake.sandboxes[name].status = "stopped"
	if err := backend.ExtendLease(context.Background(), h); err != nil || fake.extends != extends {
		t.Errorf("a stopped sandbox: %v, %d extensions", err, fake.extends-extends)
	}
}

// TestNoHostsIsDenyAllAndABadHostIsRefused.
func TestNoHostsIsDenyAllAndABadHostIsRefused(t *testing.T) {
	policy, err := buildPolicy(nil)
	if err != nil || policy.Mode != "deny-all" || len(policy.AllowedDomains) != 0 {
		t.Errorf("no hosts = %+v, %v; want deny-all", policy, err)
	}
	for _, bad := range []string{"*.github.com", "github.*", "140.82.112.3", "[::1]", "GitHub.com", "github.com:443",
		"https://github.com", "localhost", "a..b", "-x.com", "10.0.0.010", ""} {
		if _, err := buildPolicy([]string{"github.com", bad}); !errors.Is(err, ErrInvalidEgressHost) {
			t.Errorf("%q was accepted into a policy", bad)
		}
	}
	policy, err = buildPolicy([]string{"github.com", "github.com", "registry.npmjs.org"})
	if err != nil || policy.Mode != "custom" || strings.Join(policy.AllowedDomains, ",") != "github.com,registry.npmjs.org" {
		t.Errorf("= %+v, %v", policy, err)
	}
}

// TestTheCapIsRefusedBelowARunnersLifetimeOutsideDevelopment: at the bound
// and one second below it, both sides.
func TestTheCapIsRefusedBelowARunnersLifetimeOutsideDevelopment(t *testing.T) {
	_, server := newFakeVercel(t)
	lifetime := 9*time.Hour + 48*time.Second
	for _, tc := range []struct {
		env string
		cap time.Duration
		ok  bool
	}{
		{"production", lifetime, true},
		{"production", lifetime - time.Second, false},
		{"staging", lifetime - time.Second, false},
		{"staging", 24 * time.Hour, true},
		{"development", 45 * time.Minute, true},
		{"test", 45 * time.Minute, true},
		{"prod", 45 * time.Minute, false},
	} {
		cfg := testConfig(server)
		cfg.AppEnv, cfg.MaxSession, cfg.RequiredLifetime = tc.env, tc.cap, lifetime
		_, err := New(cfg)
		if (err == nil) != tc.ok || (!tc.ok && !errors.Is(err, ErrCapBelowLifetime)) {
			t.Errorf("APP_ENV=%s cap %s: %v, want ok=%v", tc.env, tc.cap, err, tc.ok)
		}
	}
}

// TestNewRefusesABinaryTheSandboxCannotRun and an incomplete configuration.
func TestNewRefusesABinaryTheSandboxCannotRun(t *testing.T) {
	_, server := newFakeVercel(t)
	for name, binary := range map[string][]byte{
		"arm64":      elfHeader(183),
		"not an ELF": []byte("#!/bin/sh\n"),
	} {
		cfg := testConfig(server)
		cfg.RunnerBinary = binary
		if _, err := New(cfg); err == nil {
			t.Errorf("a %s runner binary was accepted", name)
		}
	}
	cfg := testConfig(server)
	cfg.Token = ""
	if _, err := New(cfg); err == nil {
		t.Error("a configuration without a token was accepted")
	}
	cfg = testConfig(server)
	cfg.Lease = time.Hour
	if _, err := New(cfg); err == nil {
		t.Error("a lease longer than the session cap was accepted")
	}
}

// TestRefusalsAreTerminalAndRateLimitsAreRetried: a plan limit, a rejected
// token and a request Vercel will not accept all fail the session naming the
// backend; a rate limit is waited out.
func TestRefusalsAreTerminalAndRateLimitsAreRetried(t *testing.T) {
	backend, fake := newTestBackend(t)
	fake.rateLimitOnce = true
	if _, err := backend.Provision(context.Background(), testSpec()); err != nil {
		t.Errorf("a single 429 failed provisioning: %v", err)
	}

	for _, status := range []int{http.StatusPaymentRequired, http.StatusForbidden, http.StatusUnauthorized, http.StatusBadRequest} {
		backend, fake := newTestBackend(t)
		fake.failCreate = status
		_, err := backend.Provision(context.Background(), testSpec())
		if !errors.Is(err, application.ErrRunnerBackendRefused) {
			t.Errorf("create answered %d: %v, want ErrRunnerBackendRefused", status, err)
		}
		if cause, terminal := application.ClassifyRunnerFailure(err); !terminal || cause != application.RunnerFailureBackendRefused {
			t.Errorf("create answered %d: classified %q, %v", status, cause, terminal)
		}
	}

	backend, _ = newTestBackend(t)
	backend.api.token = "wrong"
	_, err := backend.Provision(context.Background(), testSpec())
	if err == nil || strings.Contains(err.Error(), "wrong") || strings.Contains(err.Error(), secretToken) {
		t.Errorf("error %v must be a refusal carrying no credential", err)
	}
}

// TestAFailedStartNeverCarriesItsSecretsIntoAnError: the start command's body
// holds the runner's secrets. Even against a server that echoes the request in
// its error message, the error the backend returns names the failure and not
// the body.
func TestAFailedStartNeverCarriesItsSecretsIntoAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]string{"code": "bad_request", "message": string(body)}})
	}))
	t.Cleanup(server.Close)
	c := &client{http: server.Client(), base: server.URL, token: "t", teamID: "x", timeout: time.Second,
		sleep: func(context.Context, time.Duration) error { return nil }}
	req := request{method: http.MethodPost, path: "/v2/sandboxes/sessions/s/cmd",
		body: commandRequest{Command: "bash", Env: map[string]string{"WEAVE_GIT_TOKEN": secretToken}}, accept: []int{200}}

	req.sensitive = true
	err := c.do(context.Background(), req, nil)
	if err == nil || strings.Contains(err.Error(), secretToken) || !strings.Contains(err.Error(), "bad_request") {
		t.Errorf("sensitive request error = %v; want Vercel's code and no secret", err)
	}
	// And the control: without the flag the echoed message would have carried
	// it, which is why the start command sets it.
	req.sensitive = false
	if err := c.do(context.Background(), req, nil); err == nil || !strings.Contains(err.Error(), secretToken) {
		t.Errorf("control: an echoing server's message was not included (%v); the test above proves nothing", err)
	}
}

// TestProvisionMarksItsStartSensitive: through the backend, not only the
// client — a refused start's error names the refusal and carries no secret.
func TestProvisionMarksItsStartSensitive(t *testing.T) {
	backend, fake := newTestBackend(t)
	fake.echoStart = true
	_, err := backend.Provision(context.Background(), testSpec())
	// Not only the secrets: none of the body. Vercel's message is cut at 300
	// characters, and the env comes after a long script in the args, so
	// checking for the secret alone passed on truncation luck (found by
	// mutation: without the flag this test still passed).
	if err == nil || strings.Contains(err.Error(), secretToken) || strings.Contains(err.Error(), "NATS USER") ||
		strings.Contains(err.Error(), `"command"`) || strings.Contains(err.Error(), "setpriv") {
		t.Errorf("a refused start = %v; want Vercel's code and none of the request body", err)
	}
}

// TestAFinishedRunnerIsSeenThoughPlainReadsHideItsExit is the defect the live
// acceptance test found: Vercel reports a command's exit only to a read with
// wait=true. Read any other way, a runner that had exited looked like one
// still running, and its session would have run on to its deadline.
func TestAFinishedRunnerIsSeenThoughPlainReadsHideItsExit(t *testing.T) {
	backend, fake := newTestBackend(t)
	spec := testSpec()
	h, err := backend.Provision(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	fake.finish(backend.sandboxName(spec.RunnerID), 4)
	status, err := backend.Status(context.Background(), h)
	if err != nil || !status.Exists || status.Running || status.ExitCode != 4 {
		t.Errorf("status of an exited runner = %+v, %v; want exited 4", status, err)
	}
}

// TestADuplicateStartIsNotTakenForTheRunner: a redelivered start that found
// the marker taken exits 75. Vercel lists it first — newest first — with its
// exit hidden, and the first version would have taken it for the runner.
func TestADuplicateStartIsNotTakenForTheRunner(t *testing.T) {
	backend, fake := newTestBackend(t)
	spec := testSpec()
	first, err := backend.Provision(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := parseHandle(first)
	if err := backend.api.do(context.Background(), request{method: http.MethodPost,
		path:   "/v2/sandboxes/sessions/" + h.session + "/cmd",
		body:   commandRequest{Command: "bash", Args: []string{"-c", startScript}, Sudo: true},
		accept: []int{http.StatusOK}}, nil); err != nil {
		t.Fatal(err)
	}
	if fake.starts != 1 {
		t.Fatalf("%d starts ran; the marker must stop the second", fake.starts)
	}
	found, ok, err := backend.HandleFor(context.Background(), spec.RunnerID)
	if err != nil || !ok || found != first {
		t.Errorf("HandleFor = %q, %v, %v; want the real runner %q, not the duplicate", found, ok, err, first)
	}
}
