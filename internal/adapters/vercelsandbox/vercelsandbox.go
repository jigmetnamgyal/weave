// Package vercelsandbox runs session runners in Vercel Sandbox — Firecracker
// microVMs with a hostname-enforcing egress firewall — selected against
// ADR-013's checklist on 2026-09-29 (Unit M5.4b).
//
// **This is the production isolation boundary.** Untrusted code will run here
// from M6. Every choice below that touches it says why:
//
//   - **Non-persistent, always.** Vercel sandboxes persist by default — a
//     snapshot of the filesystem on stop, kept for days and not removed with
//     the sandbox. That is source retained after teardown, which ADR-013
//     forbids. Every create states `persistent: false`, and Destroy verifies
//     no snapshot remains before reporting success.
//   - **An explicit allowlist, always.** The network policy is built from the
//     exact hostnames the runner manager supplies, never "allow-all"; with
//     none it is "deny-all".
//   - **Secrets only in the runner's start command.** Create-time environment
//     is sandbox configuration, returned by the API and inherited by every
//     later command — an agent's in M6. The start command's own environment
//     is not.
//   - **A dedicated runner user without sudo.** The sandbox's default user
//     has passwordless sudo; the runner runs as `weave-runner`, created
//     without it, dropped to with `setpriv --no-new-privs`.
//   - **A lease.** The sandbox is created with a short timeout the runner
//     manager extends while the runner is live, so a runner manager that
//     stops leaves nothing running past one lease. No lease or extension may
//     pass the plan's session cap.
package vercelsandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// BackendName selects this backend in configuration. The name stored on
// runner rows adds the scope — see Name.
const BackendName = "vercel"

// DefaultLease is how long a sandbox lives without an extension. Five
// minutes: ten of the runner manager's 30-second reconciliation passes, so a
// few failed extensions cost nothing, and short enough that a runner manager
// that has stopped leaves no sandbox running for long (M5.4b's decision).
const DefaultLease = 5 * time.Minute

// Where things live inside the sandbox.
const (
	runnerUser   = "weave-runner"
	runnerPath   = "/usr/local/bin/weave-runner"
	installDir   = "/tmp/weave-install"
	extractDir   = "/tmp"
	workspaceDir = "/workspace"
	// startedMarker makes starting the runner happen at most once per
	// sandbox. mkdir is atomic; a redelivered Provision whose first start
	// reached the sandbox finds the marker and its duplicate exits at once
	// with startedDuplicateExit, never running a second runner with the same
	// credentials.
	startedMarker        = "/run/weave-runner-started"
	startedDuplicateExit = 75
)

// Tags mark what this backend owns. List filters on the scope tag — the API
// accepts one tag filter — so a developer's scope and a test's never see each
// other's sandboxes. Separate projects per environment remain the rule; the
// tag is the second layer.
const (
	tagScope   = "weave-scope"
	tagRunner  = "weave-runner"
	tagSession = "weave-session"
)

var scopePattern = regexp.MustCompile(`^[a-z0-9]{1,20}$`)

// ErrSnapshotRetained means a snapshot of a destroyed runner's sandbox still
// exists after teardown tried to remove it. A retained snapshot is retained
// source (ADR-013): teardown fails, is retried, and is alerted on if it
// persists.
var ErrSnapshotRetained = errors.New("vercelsandbox: a snapshot of the runner's sandbox is still retained")

// ErrCapBelowLifetime means the plan's session cap is shorter than a runner
// can need, outside development.
var ErrCapBelowLifetime = errors.New("vercelsandbox: the plan's session cap is below the longest a runner can live")

// Config sets up the backend. Token, team and project are secrets or
// deployment facts; none is ever logged.
type Config struct {
	AppEnv    string
	Token     string
	TeamID    string
	ProjectID string
	Region    string
	// Scope separates runner sets sharing a project: "dev" for `make dev`,
	// a unique value per test.
	Scope string
	// MaxSession is the plan's hard cap on one sandbox's life: 45 minutes on
	// Hobby, 24 hours on Pro (RUNNER_VERCEL_MAX_SESSION). No lease or
	// extension this backend requests may pass it.
	MaxSession time.Duration
	// RequiredLifetime is the longest a runner can live from its creation
	// (temporal.MaxRunnerLifetime). Outside development and test a cap
	// below it is refused: a runner using its full run time would be stopped
	// by Vercel before its workflow confirmed its events.
	RequiredLifetime time.Duration
	// Lease is the timeout a sandbox is created with and extended to.
	// DefaultLease when zero.
	Lease time.Duration
	// RunnerBinary is the runner, built for the sandbox: linux/amd64, which
	// the default image is.
	RunnerBinary []byte

	// BaseURL, HTTPClient and Now exist for tests.
	BaseURL    string
	HTTPClient *http.Client
	Now        func() time.Time
}

// Backend implements application.RunnerBackend on Vercel Sandbox.
type Backend struct {
	api        *client
	project    string
	region     string
	scope      string
	lease      time.Duration
	maxSession time.Duration
	binary     []byte
	binarySum  string
	now        func() time.Time
	statusPoll time.Duration

	// ready remembers handles whose runner has reported ready. Readiness is
	// monotonic — a runner never becomes un-ready while it runs — so once
	// seen it is not asked again: otherwise every one-second poll of an
	// eight-hour run would start a command in the sandbox.
	readyMu sync.Mutex
	ready   map[string]bool
}

var (
	_ application.RunnerBackend = (*Backend)(nil)
	_ application.RunnerLeaser  = (*Backend)(nil)
)

// New builds the backend, refusing a configuration that could not keep
// ADR-013's promises.
func New(cfg Config) (*Backend, error) {
	var missing []string
	for name, value := range map[string]string{
		"token": cfg.Token, "team id": cfg.TeamID, "project id": cfg.ProjectID, "region": cfg.Region,
	} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("vercelsandbox: missing %s", strings.Join(missing, ", "))
	}
	if !scopePattern.MatchString(cfg.Scope) {
		return nil, fmt.Errorf("vercelsandbox: scope %q must be 1-20 lowercase letters or digits", cfg.Scope)
	}
	if cfg.Lease == 0 {
		cfg.Lease = DefaultLease
	}
	if cfg.MaxSession <= 0 || cfg.Lease <= 0 || cfg.Lease > cfg.MaxSession {
		return nil, fmt.Errorf("vercelsandbox: lease %s and session cap %s must be positive, lease within cap",
			cfg.Lease, cfg.MaxSession)
	}
	if err := checkCap(cfg.AppEnv, cfg.MaxSession, cfg.RequiredLifetime); err != nil {
		return nil, err
	}
	if err := checkBinary(cfg.RunnerBinary); err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(cfg.BaseURL, "/")
	if base == "" {
		base = "https://api.vercel.com"
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	sum := sha256.Sum256(cfg.RunnerBinary)
	return &Backend{
		api: &client{http: httpClient, base: base, token: cfg.Token, teamID: cfg.TeamID,
			sleep: sleepContext, timeout: callTimeout},
		project: cfg.ProjectID, region: cfg.Region, scope: cfg.Scope,
		lease: cfg.Lease, maxSession: cfg.MaxSession,
		binary: cfg.RunnerBinary, binarySum: hex.EncodeToString(sum[:]),
		now: now, ready: map[string]bool{}, statusPoll: defaultStatusPoll,
	}, nil
}

// checkCap refuses, in staging and production, a plan cap below the longest a
// runner can live. In development a shorter cap is allowed — Hobby's 45
// minutes — and a session that outlives it loses its sandbox and fails
// through the lost-runner path, which is harmless with the fake provider.
func checkCap(appEnv string, maxSession, required time.Duration) error {
	switch strings.ToLower(strings.TrimSpace(appEnv)) {
	case "development", "test":
		return nil
	}
	if required <= 0 {
		return fmt.Errorf("%w: no required lifetime was given (APP_ENV=%q)", ErrCapBelowLifetime, appEnv)
	}
	if maxSession < required {
		return fmt.Errorf("%w: cap %s, a runner can live %s (APP_ENV=%q); Vercel Pro allows 24h",
			ErrCapBelowLifetime, maxSession, required, appEnv)
	}
	return nil
}

// checkBinary refuses a runner binary the sandbox could not run: the default
// image is x86_64 Linux, measured with `uname -m` in M5.4b's probe.
func checkBinary(binary []byte) error {
	file, err := elf.NewFile(bytes.NewReader(binary))
	if err != nil {
		return errors.New("vercelsandbox: the runner binary is not an ELF executable; build it for linux/amd64")
	}
	defer func() { _ = file.Close() }()
	if file.Machine != elf.EM_X86_64 || file.OSABI != elf.ELFOSABI_NONE && file.OSABI != elf.ELFOSABI_LINUX {
		return fmt.Errorf("vercelsandbox: the runner binary is for %s, but the sandbox is linux/amd64", file.Machine)
	}
	return nil
}

// Name identifies this backend and its scope on runner rows, as the dev
// backend's does: reconciliation judges only runners whose backend name is
// its own.
func (b *Backend) Name() string { return BackendName + "-" + b.scope }

// sandboxName is deterministic from the runner id. Names are unique per
// project, so a retried Provision finds the sandbox it made.
func (b *Backend) sandboxName(runnerID uuid.UUID) string {
	return "weave-runner-" + b.scope + "-" + runnerID.String()
}

// handle is what a runner row records: the sandbox, its session, and the
// runner's command, which Status needs and which the name alone cannot give.
type handle struct {
	sandbox, session, command string
}

func (h handle) String() string { return h.sandbox + "/" + h.session + "/" + h.command }

func parseHandle(raw string) (handle, error) {
	parts := strings.Split(raw, "/")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return handle{}, fmt.Errorf("vercelsandbox: %q is not a runner handle", raw)
	}
	return handle{sandbox: parts[0], session: parts[1], command: parts[2]}, nil
}

// API shapes, as much of each as this backend reads.
type (
	sandboxInfo struct {
		Name             string            `json:"name"`
		Status           string            `json:"status"`
		CurrentSessionID string            `json:"currentSessionId"`
		ExpiresAt        int64             `json:"expiresAt"`
		Tags             map[string]string `json:"tags"`
		Persistent       bool              `json:"persistent"`
	}
	sessionInfo struct {
		ID      string `json:"id"`
		Status  string `json:"status"`
		Timeout int64  `json:"timeout"`
	}
	sandboxResponse struct {
		Sandbox sandboxInfo `json:"sandbox"`
		Session sessionInfo `json:"session"`
	}
	commandInfo struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		Args     []string `json:"args"`
		ExitCode *int     `json:"exitCode"`
	}
	commandResponse struct {
		Command commandInfo `json:"command"`
	}
)

// createRequest is the create body. Every field is stated rather than left to
// a default, because the defaults are what ADR-013 refuses: `persistent`
// defaults to true.
type createRequest struct {
	Name          string            `json:"name"`
	ProjectID     string            `json:"projectId"`
	Persistent    bool              `json:"persistent"`
	Timeout       int64             `json:"timeout"`
	Region        string            `json:"region"`
	Resources     resources         `json:"resources"`
	NetworkPolicy networkPolicy     `json:"networkPolicy"`
	Tags          map[string]string `json:"tags"`
}

type resources struct {
	VCPUs  int `json:"vcpus"`
	Memory int `json:"memory"`
}

// commandRequest starts a command. Env is the command's own and is not
// returned by the API; it is the only place a secret is ever put.
type commandRequest struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Sudo    bool              `json:"sudo"`
}

// Provision creates the runner's sandbox, installs the runner, and starts it.
//
// **Idempotent, step by step**, because Temporal redelivers: the sandbox is
// found by name before one is created, and a create that loses a race (409)
// reads the winner's; the install is safe to repeat; and the start is
// guarded inside the sandbox by an atomic marker, so a second start exits
// without running. A runner whose sandbox has already stopped is lost.
func (b *Backend) Provision(ctx context.Context, spec application.RunnerSpec) (string, error) {
	policy, err := buildPolicy(spec.EgressHosts)
	if err != nil {
		return "", err
	}
	name := b.sandboxName(spec.RunnerID)

	sandbox, err := b.getSandbox(ctx, name)
	if errors.Is(err, errNotFound) {
		sandbox, err = b.create(ctx, name, spec, policy)
	}
	if err != nil {
		return "", err
	}
	if sandbox.Sandbox.Persistent {
		// Never expected — the create says false — but if Vercel ever
		// ignored it, this sandbox would keep a snapshot of the checkout.
		_ = b.Destroy(ctx, spec.RunnerID, "")
		return "", fmt.Errorf("vercelsandbox: sandbox %s was created persistent; refusing to run in it", name)
	}
	if !running(sandbox.Sandbox.Status) {
		return "", fmt.Errorf("%w: its sandbox is %s", application.ErrRunnerLost, sandbox.Sandbox.Status)
	}
	session := sandbox.Sandbox.CurrentSessionID

	if existing, found, err := b.findRunnerCommand(ctx, session); err != nil {
		return "", err
	} else if found {
		return handle{sandbox: name, session: session, command: existing}.String(), nil
	}

	if err := b.install(ctx, session); err != nil {
		return "", err
	}
	var started commandResponse
	if err := b.api.do(ctx, request{
		method: http.MethodPost, path: "/v2/sandboxes/sessions/" + url.PathEscape(session) + "/cmd",
		body: commandRequest{
			Command: "bash", Args: []string{"-c", startScript}, Cwd: workspaceDir,
			Env: runnerEnv(spec), Sudo: true,
		},
		accept:    []int{http.StatusOK},
		sensitive: true,
	}, &started); err != nil {
		return "", fmt.Errorf("vercelsandbox: start the runner: %w", err)
	}

	// A concurrent start may have won the marker; the command that holds it
	// is the runner, whichever call started it.
	if existing, found, err := b.findRunnerCommand(ctx, session); err == nil && found {
		return handle{sandbox: name, session: session, command: existing}.String(), nil
	}
	return handle{sandbox: name, session: session, command: started.Command.ID}.String(), nil
}

// startScript starts the runner at most once, as the runner user, with the
// command's environment — which carries its secrets — passed straight
// through. `exec` all the way down: no parent process is left holding that
// environment, and the runner's own re-exec then moves the secrets out of
// its /proc/<pid>/environ (M5.4a). --no-new-privs keeps sudo, or any setuid
// binary, from raising it back to root.
const startScript = `mkdir ` + startedMarker + ` 2>/dev/null || exit 75
exec setpriv --reuid=` + runnerUser + ` --regid=` + runnerUser + ` --clear-groups --no-new-privs ` + runnerPath

// installScript prepares the sandbox. Every step is idempotent. The binary's
// checksum is verified after it is installed, so a truncated or altered
// upload fails provisioning rather than running.
func installScript(sum string) string {
	return `set -eu
id -u ` + runnerUser + ` >/dev/null 2>&1 || useradd --system --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin ` + runnerUser + `
if [ -f ` + installDir + `/weave-runner ]; then install -o root -g root -m 0755 ` + installDir + `/weave-runner ` + runnerPath + `; fi
rm -rf ` + installDir + `
echo "` + sum + `  ` + runnerPath + `" | sha256sum --check --quiet --strict -
install -d -o ` + runnerUser + ` -g ` + runnerUser + ` -m 0700 ` + workspaceDir
}

// runnerEnv is everything the runner is told at start. The secrets — the git
// token and the NATS credential — are here and nowhere else.
func runnerEnv(spec application.RunnerSpec) map[string]string {
	return map[string]string{
		"WEAVE_RUNNER_ID":      spec.RunnerID.String(),
		"WEAVE_SESSION_ID":     spec.SessionID.String(),
		"WEAVE_WORKSPACE_ID":   spec.WorkspaceID.String(),
		"WEAVE_AGENT_PROVIDER": string(spec.Provider),
		"WEAVE_AGENT_MODEL":    spec.Model,
		"WEAVE_CLONE_URL":      spec.CloneURL,
		"WEAVE_BRANCH":         spec.Branch,
		"WEAVE_COMMIT":         spec.Commit,
		"WEAVE_GIT_TOKEN":      spec.GitToken,
		"WEAVE_NATS_URL":       spec.NATSURL,
		"WEAVE_NATS_CREDS":     spec.NATS.Creds,
		"WEAVE_EVENT_SUBJECT":  spec.NATS.Subject,
		"WEAVE_NATS_INBOX":     spec.NATS.InboxPrefix,
		"HOME":                 "/tmp",
	}
}

func (b *Backend) create(ctx context.Context, name string, spec application.RunnerSpec, policy networkPolicy) (sandboxResponse, error) {
	var created sandboxResponse
	err := b.api.do(ctx, request{
		method: http.MethodPost, path: "/v4/sandboxes",
		body: createRequest{
			Name: name, ProjectID: b.project, Persistent: false,
			Timeout: b.lease.Milliseconds(), Region: b.region,
			Resources:     resources{VCPUs: 2, Memory: 4096},
			NetworkPolicy: policy,
			Tags: map[string]string{
				tagScope: b.scope, tagRunner: spec.RunnerID.String(), tagSession: spec.SessionID.String(),
			},
		},
		accept: []int{http.StatusOK, http.StatusCreated},
	}, &created)
	if errors.Is(err, errConflict) {
		// Another delivery created it first.
		return b.getSandbox(ctx, name)
	}
	if err != nil {
		return sandboxResponse{}, fmt.Errorf("vercelsandbox: create sandbox: %w", err)
	}
	return created, nil
}

// install uploads the runner binary and prepares the sandbox.
func (b *Backend) install(ctx context.Context, session string) error {
	// Extracted into /tmp, which always exists, with the install directory
	// as an entry in the archive: Vercel extracts into an existing directory
	// and refuses the upload ("could not be extracted") when X-Cwd names one
	// that does not — found by the live acceptance test, not the fake.
	archive, err := tarball(strings.TrimPrefix(installDir, extractDir+"/"), "weave-runner", b.binary)
	if err != nil {
		return err
	}
	if err := b.api.do(ctx, request{
		method: http.MethodPost, path: "/v2/sandboxes/sessions/" + url.PathEscape(session) + "/fs/write",
		raw: archive, contentType: "application/gzip",
		header: http.Header{"X-Cwd": []string{extractDir}},
		accept: []int{http.StatusOK, http.StatusCreated, http.StatusNoContent},
	}, nil); err != nil {
		return fmt.Errorf("vercelsandbox: upload the runner: %w", err)
	}
	code, err := b.runAndWait(ctx, session, commandRequest{
		Command: "bash", Args: []string{"-c", installScript(b.binarySum)}, Sudo: true,
	})
	if err != nil {
		return fmt.Errorf("vercelsandbox: prepare the sandbox: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("vercelsandbox: preparing the sandbox exited %d (the runner binary's checksum, or creating its user)", code)
	}
	return nil
}

// runAndWait starts a short command and waits for its exit code.
func (b *Backend) runAndWait(ctx context.Context, session string, cmd commandRequest) (int, error) {
	var started commandResponse
	if err := b.api.do(ctx, request{
		method: http.MethodPost, path: "/v2/sandboxes/sessions/" + url.PathEscape(session) + "/cmd",
		body: cmd, accept: []int{http.StatusOK},
	}, &started); err != nil {
		return 0, err
	}
	var done commandResponse
	if err := b.api.do(ctx, request{
		method: http.MethodGet,
		path:   "/v2/sandboxes/sessions/" + url.PathEscape(session) + "/cmd/" + url.PathEscape(started.Command.ID),
		query:  url.Values{"wait": []string{"true"}},
		accept: []int{http.StatusOK},
	}, &done); err != nil {
		return 0, err
	}
	if done.Command.ExitCode == nil {
		return 0, errors.New("the command did not report an exit code")
	}
	return *done.Command.ExitCode, nil
}

// defaultStatusPoll bounds how long a status read waits on a running
// command.
const defaultStatusPoll = 2 * time.Second

// commandExit reports a command's exit code, or nil while it runs.
//
// **Only a read with wait=true reveals an exit.** Measured against the live
// API in M5.4b: a command that exited in 26 ms still read exitCode null, on a
// plain GET and in the list, more than half a minute later; a wait=true read
// returned its code at once, and only after that did the others show it. The
// reference does not say so, and the fake did not model it until the live
// test found it — without this, a finished runner would have read as running
// until its session expired. So the command is read with wait=true, bounded:
// a finished one answers immediately, and a running one reaching the bound is
// still running. Read from the backend, never from the runner.
func (b *Backend) commandExit(ctx context.Context, session, command string) (*int, error) {
	var cmd commandResponse
	err := b.api.do(ctx, request{
		method: http.MethodGet,
		path:   "/v2/sandboxes/sessions/" + url.PathEscape(session) + "/cmd/" + url.PathEscape(command),
		query:  url.Values{"wait": []string{"true"}},
		poll:   b.statusPoll,
		accept: []int{http.StatusOK},
	}, &cmd)
	if errors.Is(err, errStillWaiting) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cmd.Command.ExitCode, nil
}

// findRunnerCommand finds the command that holds the start marker: the
// runner's start, not a duplicate that found the marker taken and exited 75.
//
// The list reports exit codes only once something has read them with
// wait=true (see commandExit), and lists newest first, so a redelivered
// start would come before the real one. Candidates are therefore taken
// oldest first, and each one's exit resolved before it is chosen.
func (b *Backend) findRunnerCommand(ctx context.Context, session string) (string, bool, error) {
	var listed struct {
		Commands []struct {
			commandInfo
			StartedAt int64 `json:"startedAt"`
		} `json:"commands"`
	}
	err := b.api.do(ctx, request{
		method: http.MethodGet, path: "/v2/sandboxes/sessions/" + url.PathEscape(session) + "/cmd",
		accept: []int{http.StatusOK},
	}, &listed)
	if errors.Is(err, errNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("vercelsandbox: list commands: %w", err)
	}
	starts := listed.Commands[:0]
	for _, cmd := range listed.Commands {
		if len(cmd.Args) == 2 && cmd.Args[1] == startScript {
			starts = append(starts, cmd)
		}
	}
	sort.SliceStable(starts, func(i, j int) bool { return starts[i].StartedAt < starts[j].StartedAt })
	for _, cmd := range starts {
		exit := cmd.ExitCode
		if exit == nil {
			if exit, err = b.commandExit(ctx, session, cmd.ID); err != nil {
				return "", false, fmt.Errorf("vercelsandbox: read a start command: %w", err)
			}
		}
		if exit != nil && *exit == startedDuplicateExit {
			continue
		}
		return cmd.ID, true, nil
	}
	return "", false, nil
}

func (b *Backend) getSandbox(ctx context.Context, name string) (sandboxResponse, error) {
	var got sandboxResponse
	// resume is never set: a stopped sandbox must not be brought back, from
	// a snapshot or otherwise.
	err := b.api.do(ctx, request{
		method: http.MethodGet, path: "/v2/sandboxes/" + url.PathEscape(name),
		query:  url.Values{"projectId": []string{b.project}},
		accept: []int{http.StatusOK},
	}, &got)
	return got, err
}

func running(status string) bool { return status == "running" || status == "pending" }

// Status reports the runner's state.
//
// The **session** decides whether the environment exists: a stopped sandbox
// cannot be restarted — the lease lapsed, the cap was reached, or it was
// destroyed — so it is gone, and a runner still inside it is lost. Only while
// the session runs does the runner's command say more: finished, with its exit
// code, or running, and whether it has reported ready.
func (b *Backend) Status(ctx context.Context, raw string) (application.RunnerStatus, error) {
	h, err := parseHandle(raw)
	if err != nil {
		return application.RunnerStatus{}, err
	}
	var session struct {
		Session sessionInfo `json:"session"`
	}
	err = b.api.do(ctx, request{
		method: http.MethodGet, path: "/v2/sandboxes/sessions/" + url.PathEscape(h.session),
		accept: []int{http.StatusOK},
	}, &session)
	if errors.Is(err, errNotFound) {
		return application.RunnerStatus{Exists: false}, nil
	}
	if err != nil {
		return application.RunnerStatus{}, fmt.Errorf("vercelsandbox: read session: %w", err)
	}
	if !running(session.Session.Status) {
		return application.RunnerStatus{Exists: false}, nil
	}

	exit, err := b.commandExit(ctx, h.session, h.command)
	if errors.Is(err, errNotFound) {
		return application.RunnerStatus{Exists: false}, nil
	}
	if err != nil {
		return application.RunnerStatus{}, fmt.Errorf("vercelsandbox: read the runner command: %w", err)
	}
	if exit != nil {
		return application.RunnerStatus{Exists: true, Running: false, ExitCode: *exit}, nil
	}

	ready, err := b.isReady(ctx, h)
	if err != nil {
		return application.RunnerStatus{}, err
	}
	return application.RunnerStatus{Exists: true, Running: true, Ready: ready}, nil
}

// isReady asks the runner's own healthcheck, through the command API, until
// it first says yes.
func (b *Backend) isReady(ctx context.Context, h handle) (bool, error) {
	key := h.String()
	b.readyMu.Lock()
	known := b.ready[key]
	b.readyMu.Unlock()
	if known {
		return true, nil
	}
	code, err := b.runAndWait(ctx, h.session, commandRequest{Command: runnerPath, Args: []string{"healthcheck"}})
	if err != nil {
		return false, fmt.Errorf("vercelsandbox: healthcheck: %w", err)
	}
	if code != 0 {
		return false, nil
	}
	b.readyMu.Lock()
	b.ready[key] = true
	b.readyMu.Unlock()
	return true, nil
}

// Destroy deletes the runner's sandbox, by runner id, and verifies that no
// snapshot of it is retained. Already gone is success.
//
// Deletion asks Vercel to remove orphaned snapshots too, but asynchronously,
// so the snapshot list is then read and anything left deleted explicitly.
// A snapshot that survives both is ErrSnapshotRetained: teardown fails and is
// retried rather than reporting a checkout gone that is not.
func (b *Backend) Destroy(ctx context.Context, runnerID uuid.UUID, _ string) error {
	name := b.sandboxName(runnerID)
	err := b.api.do(ctx, request{
		method: http.MethodDelete, path: "/v2/sandboxes/" + url.PathEscape(name),
		query:  url.Values{"projectId": []string{b.project}, "deleteOrphanSnapshots": []string{"true"}},
		accept: []int{http.StatusOK, http.StatusNoContent, http.StatusAccepted},
	}, nil)
	if err != nil && !errors.Is(err, errNotFound) {
		return fmt.Errorf("vercelsandbox: delete sandbox: %w", err)
	}
	b.forgetReady(name)

	for attempt := 0; attempt < 2; attempt++ {
		snapshots, err := b.snapshotsOf(ctx, name)
		if err != nil {
			return err
		}
		if len(snapshots) == 0 {
			return nil
		}
		for _, id := range snapshots {
			if err := b.api.do(ctx, request{
				method: http.MethodDelete, path: "/v2/sandboxes/snapshots/" + url.PathEscape(id),
				accept: []int{http.StatusOK, http.StatusNoContent, http.StatusAccepted},
			}, nil); err != nil && !errors.Is(err, errNotFound) {
				return fmt.Errorf("vercelsandbox: delete snapshot %s: %w", id, err)
			}
		}
	}
	return fmt.Errorf("%w (sandbox %s)", ErrSnapshotRetained, name)
}

func (b *Backend) snapshotsOf(ctx context.Context, name string) ([]string, error) {
	var listed struct {
		Snapshots []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"snapshots"`
	}
	if err := b.api.do(ctx, request{
		method: http.MethodGet, path: "/v2/sandboxes/snapshots",
		query:  url.Values{"project": []string{b.project}, "name": []string{name}},
		accept: []int{http.StatusOK},
	}, &listed); err != nil {
		return nil, fmt.Errorf("vercelsandbox: list snapshots: %w", err)
	}
	ids := make([]string, 0, len(listed.Snapshots))
	for _, s := range listed.Snapshots {
		if s.Status != "deleted" {
			ids = append(ids, s.ID)
		}
	}
	return ids, nil
}

func (b *Backend) forgetReady(sandbox string) {
	b.readyMu.Lock()
	defer b.readyMu.Unlock()
	for key := range b.ready {
		if strings.HasPrefix(key, sandbox+"/") {
			delete(b.ready, key)
		}
	}
}

// HandleFor finds a runner's environment by runner id: a sandbox whose runner
// has been started. A sandbox created but never given its runner is not an
// environment that can run; Provision finishes it.
func (b *Backend) HandleFor(ctx context.Context, runnerID uuid.UUID) (string, bool, error) {
	name := b.sandboxName(runnerID)
	sandbox, err := b.getSandbox(ctx, name)
	if errors.Is(err, errNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("vercelsandbox: read sandbox: %w", err)
	}
	if !running(sandbox.Sandbox.Status) {
		return "", false, nil
	}
	command, found, err := b.findRunnerCommand(ctx, sandbox.Sandbox.CurrentSessionID)
	if err != nil || !found {
		return "", false, err
	}
	return handle{sandbox: name, session: sandbox.Sandbox.CurrentSessionID, command: command}.String(), true, nil
}

// List returns every runner sandbox in this scope, a page at a time.
//
// Filtered by the scope tag at the API and checked again here, name and tag
// both, so nothing this backend did not create is ever reported — and so
// never destroyed by the orphan sweep.
func (b *Backend) List(ctx context.Context) ([]application.BackendRunner, error) {
	var out []application.BackendRunner
	prefix := "weave-runner-" + b.scope + "-"
	cursor := ""
	for page := 0; page < 200; page++ {
		query := url.Values{
			"project": []string{b.project}, "tags": []string{tagScope + ":" + b.scope}, "limit": []string{"50"},
		}
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		var listed struct {
			Sandboxes  []sandboxInfo `json:"sandboxes"`
			Pagination struct {
				Next *string `json:"next"`
			} `json:"pagination"`
		}
		if err := b.api.do(ctx, request{
			method: http.MethodGet, path: "/v2/sandboxes", query: query, accept: []int{http.StatusOK},
		}, &listed); err != nil {
			return nil, fmt.Errorf("vercelsandbox: list sandboxes: %w", err)
		}
		for _, s := range listed.Sandboxes {
			if s.Tags[tagScope] != b.scope || !strings.HasPrefix(s.Name, prefix) {
				continue
			}
			id, err := uuid.Parse(s.Tags[tagRunner])
			if err != nil || s.Name != b.sandboxName(id) {
				continue
			}
			out = append(out, application.BackendRunner{RunnerID: id, Handle: s.Name})
		}
		if listed.Pagination.Next == nil || *listed.Pagination.Next == "" {
			return out, nil
		}
		cursor = *listed.Pagination.Next
	}
	return nil, errors.New("vercelsandbox: list sandboxes: too many pages")
}

// ExtendLease pushes the sandbox's expiry to a lease from now, never past the
// plan's session cap. Vercel's extension adds to the current timeout, so the
// amount is computed from the expiry it reports. A sandbox that has stopped
// or gone has nothing to extend.
func (b *Backend) ExtendLease(ctx context.Context, raw string) error {
	h, err := parseHandle(raw)
	if err != nil {
		return err
	}
	sandbox, err := b.getSandbox(ctx, h.sandbox)
	if errors.Is(err, errNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("vercelsandbox: read sandbox: %w", err)
	}
	if !running(sandbox.Sandbox.Status) || sandbox.Sandbox.ExpiresAt == 0 {
		return nil
	}
	expires := time.UnixMilli(sandbox.Sandbox.ExpiresAt)
	add := b.now().Add(b.lease).Sub(expires)
	if room := b.maxSession - time.Duration(sandbox.Session.Timeout)*time.Millisecond; add > room {
		add = room
	}
	if add < time.Second {
		// Already a lease ahead, or at the cap. At the cap the sandbox
		// stops when it is reached; in development that is the lost-runner
		// path, and in staging and production the cap exceeds any runner's
		// life (checkCap).
		return nil
	}
	err = b.api.do(ctx, request{
		method: http.MethodPost,
		path:   "/v2/sandboxes/sessions/" + url.PathEscape(sandbox.Sandbox.CurrentSessionID) + "/extend-timeout",
		body:   map[string]int64{"duration": add.Milliseconds()},
		accept: []int{http.StatusOK},
	}, nil)
	if errors.Is(err, errNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("vercelsandbox: extend lease: %w", err)
	}
	return nil
}

// tarball packs one executable file, inside a directory, for the
// file-write API, which takes a gzipped tarball and extracts it where the
// X-Cwd header says.
func tarball(dir, name string, contents []byte) ([]byte, error) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: dir + "/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		return nil, err
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: dir + "/" + name, Mode: 0o755, Size: int64(len(contents)), Typeflag: tar.TypeReg,
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(contents); err != nil {
		return nil, err
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
