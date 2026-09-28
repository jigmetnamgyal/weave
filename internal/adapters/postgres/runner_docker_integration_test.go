package postgres_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/devdocker"
	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// gitServer serves one real repository over smart HTTP, as GitHub would, so a
// runner's clone is a real clone. Reached from containers through
// host.docker.internal. Returns the base URL and the commit the repository's
// main branch points at.
func gitServer(t *testing.T, owner, name string) (string, string) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	run := func(dir string, args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=weave", "GIT_AUTHOR_EMAIL=test@weave.invalid",
			"GIT_COMMITTER_NAME=weave", "GIT_COMMITTER_EMAIL=test@weave.invalid")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	run(work, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# service\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(work, "add", ".")
	run(work, "commit", "-q", "-m", "initial")
	commit := run(work, "rev-parse", "HEAD")

	bare := filepath.Join(root, "repos", owner, name+".git")
	if err := os.MkdirAll(filepath.Dir(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	run(root, "clone", "-q", "--bare", work, bare)
	// GitHub serves any reachable commit by id; the runner fetches by id.
	run(bare, "config", "uploadpack.allowReachableSHA1InWant", "true")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{
		Path: gitPath, Args: []string{"http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + filepath.Join(root, "repos"), "GIT_HTTP_EXPORT_ALL=1"},
	}
	// Private, as a customer's repository is: a clone without the installation
	// token the fake GitHub minted is refused. This is what proves the token
	// survives the runner's re-exec and reaches git.
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "x-access-token" || !strings.HasPrefix(password, "ghs_fake_") {
			w.Header().Set("WWW-Authenticate", `Basic realm="git"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		backend.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	port := listener.Addr().(*net.TCPAddr).Port
	return "http://host.docker.internal:" + strconv.Itoa(port), commit
}

// dockerBackend is the dev backend on a scope of its own, so it sees only this
// test's containers — never a developer's, never another test's — and removes
// whatever the test left.
func dockerBackend(t *testing.T) *devdocker.Backend {
	t.Helper()
	socket := os.Getenv("TEST_DOCKER_SOCKET")
	if socket == "" {
		t.Skip("TEST_DOCKER_SOCKET is not set; run `make test-integration`")
	}
	backend, err := devdocker.New(devdocker.Config{
		AppEnv: "test", Socket: socket, Image: "weave-runner:dev",
		Scope: "test" + strings.ReplaceAll(uuid.NewString(), "-", "")[:10],
	})
	if err != nil {
		t.Fatalf("dev backend: %v", err)
	}
	t.Cleanup(func() {
		environments, _ := backend.List(context.Background())
		for _, e := range environments {
			_ = backend.Destroy(context.Background(), e.Handle)
		}
	})
	return backend
}

// quiet removes a session's outbox row, so no workflow — a `make dev` worker
// draining the outbox beside the tests, above all — ever runs it. These tests
// drive the session themselves; a worker moving it to failed underneath them
// is the interference M5.2's review first found, and here it made a lost
// runner look like one whose session had ended.
func quiet(t *testing.T, ownerPool *pgxpool.Pool, session domain.Session) domain.Session {
	t.Helper()
	if _, err := ownerPool.Exec(context.Background(),
		`DELETE FROM outbox_events WHERE subject_id = $1`, session.ID); err != nil {
		t.Fatalf("quiet the session: %v", err)
	}
	return session
}

// runnerWorld is a provisioning session with a recorded branch commit, a real
// repository to clone it from, and the real RunnerService over Docker.
type runnerWorld struct {
	ownerPool *pgxpool.Pool
	appPool   *pgxpool.Pool
	fixture   sessionFixture
	session   domain.Session
	commit    string
	backend   *devdocker.Backend
	service   *application.RunnerService
	tenant    context.Context
}

func newRunnerWorld(t *testing.T, name string) *runnerWorld {
	t.Helper()
	backend := dockerBackend(t)
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	fixture := seedSessionFixture(t, ownerPool, name)
	gitBase, commit := gitServer(t, fixture.repository.Owner, fixture.repository.Name)

	github := newFakeGitHubServer(t)
	github.grant(t, ownerPool, fixture)

	session, err := fixture.createSession(t, postgres.NewSessionStore(ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	quiet(t, ownerPool, session)
	tenant := postgres.WithTenantWorkspace(context.Background(), fixture.workspace.ID)
	sessions := application.NewSessionService(postgres.NewSessionStore(appPool), postgres.NewTaskStore(appPool), postgres.NewAgentStore(appPool))
	if _, err := sessions.TransitionAsSystem(tenant, fixture.workspace.ID, session.ID, domain.SessionProvisioning, "test"); err != nil {
		t.Fatalf("provisioning: %v", err)
	}
	if _, _, err := postgres.NewSessionStore(appPool).RecordBranch(tenant, session.ID, fixture.workspace.ID, commit,
		application.SystemActor(), func(domain.Session) application.AuditEvent {
			return application.AuditEvent{WorkspaceID: fixture.workspace.ID, Action: application.AuditBranchCreated, Target: session.ID.String()}
		}); err != nil {
		t.Fatalf("record branch: %v", err)
	}

	service := application.NewRunnerService(postgres.NewRunnerStore(appPool), postgres.NewSessionStore(appPool),
		backend, installationServiceFor(t, appPool, github), postgres.WithTenantWorkspace, gitBase,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return &runnerWorld{ownerPool: ownerPool, appPool: appPool, fixture: fixture, session: session,
		commit: commit, backend: backend, service: service, tenant: tenant}
}

func (w *runnerWorld) ready(t *testing.T) domain.Runner {
	t.Helper()
	runner, err := w.service.Provision(w.tenant, w.fixture.workspace.ID, w.session.ID)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	ctx, cancel := context.WithTimeout(w.tenant, 2*time.Minute)
	defer cancel()
	runner, err = w.service.AwaitReady(ctx, w.fixture.workspace.ID, runner, 300*time.Millisecond, func() {})
	if err != nil {
		t.Fatalf("await ready: %v", err)
	}
	return runner
}

func dockerExec(t *testing.T, container string, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("docker", append([]string{"exec", container}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func dockerExists(t *testing.T, kind, name string) bool {
	t.Helper()
	return exec.Command("docker", kind, "inspect", name).Run() == nil
}

// TestARunnerChecksOutExactlyTheRecordedCommitIntegration is the unit's
// central check: a real container, a real clone, at exactly
// sessions.branch_sha, on the session's branch — hardened as the dev backend
// promises — and gone, with its volume, after teardown.
func TestARunnerChecksOutExactlyTheRecordedCommitIntegration(t *testing.T) {
	w := newRunnerWorld(t, "Checkout Workspace")
	runner := w.ready(t)

	if runner.State != domain.RunnerRunning || runner.Handle == "" {
		t.Fatalf("runner = %+v, want running with a handle", runner)
	}
	head, err := dockerExec(t, runner.Handle, "git", "-C", "/workspace/repo", "rev-parse", "HEAD")
	if err != nil || head != w.commit {
		t.Errorf("HEAD = %q (%v), want the recorded commit %s", head, err, w.commit)
	}
	branch, _ := dockerExec(t, runner.Handle, "git", "-C", "/workspace/repo", "branch", "--show-current")
	if branch != w.session.BranchName {
		t.Errorf("branch = %q, want the session branch %q", branch, w.session.BranchName)
	}

	// The hardening the backend promises, observed rather than assumed.
	if uid, _ := dockerExec(t, runner.Handle, "id", "-u"); uid != "10001" {
		t.Errorf("runs as uid %q, want 10001", uid)
	}
	if _, err := dockerExec(t, runner.Handle, "touch", "/usr/local/bin/x"); err == nil {
		t.Error("the root filesystem is writable")
	}
	if caps, _ := dockerExec(t, runner.Handle, "grep", "CapEff", "/proc/self/status"); !strings.HasSuffix(caps, "0000000000000000") {
		t.Errorf("effective capabilities = %q, want none", caps)
	}
	// The token was delivered and then removed from the process environment.
	if env, _ := dockerExec(t, runner.Handle, "cat", "/proc/1/environ"); strings.Contains(env, "WEAVE_GIT_TOKEN=ghs") {
		t.Error("the git token is still in the runner's environment")
	}

	if err := w.service.Teardown(w.tenant, w.fixture.workspace.ID, w.session.ID, false, ""); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	if dockerExists(t, "container", runner.Handle) || dockerExists(t, "volume", runner.Handle) {
		t.Error("the container or its workspace volume survived teardown; ADR-013 retains no source")
	}
	var state string
	_ = w.ownerPool.QueryRow(context.Background(), `SELECT state FROM runners WHERE id = $1`, runner.ID).Scan(&state)
	if state != string(domain.RunnerTerminated) {
		t.Errorf("runner state after teardown = %q, want terminated", state)
	}
	// Idempotent: a redelivered teardown is success.
	if err := w.service.Teardown(w.tenant, w.fixture.workspace.ID, w.session.ID, false, ""); err != nil {
		t.Errorf("a second teardown = %v, want success", err)
	}
}

// TestAProvisionRedeliveryReusesTheRunnerIntegration: Temporal redelivers;
// the second delivery must find the first runner, not start another.
func TestAProvisionRedeliveryReusesTheRunnerIntegration(t *testing.T) {
	w := newRunnerWorld(t, "Redelivery Runner Workspace")
	first, err := w.service.Provision(w.tenant, w.fixture.workspace.ID, w.session.ID)
	if err != nil {
		t.Fatalf("first provision: %v", err)
	}
	second, err := w.service.Provision(w.tenant, w.fixture.workspace.ID, w.session.ID)
	if err != nil {
		t.Fatalf("redelivered provision: %v", err)
	}
	if second.ID != first.ID || second.Handle != first.Handle {
		t.Errorf("redelivery gave runner %s/%s, want the first %s/%s", second.ID, second.Handle, first.ID, first.Handle)
	}
	environments, _ := w.backend.List(context.Background())
	if len(environments) != 1 {
		t.Errorf("%d environments after a redelivery, want 1", len(environments))
	}
}

// TestACommitThatCannotBeFetchedNeverBecomesReadyIntegration: a runner that
// cannot check out the recorded commit stops, and is reported lost rather
// than ready.
func TestACommitThatCannotBeFetchedNeverBecomesReadyIntegration(t *testing.T) {
	w := newRunnerWorld(t, "Unfetchable Commit Workspace")
	if _, err := w.ownerPool.Exec(context.Background(),
		`UPDATE sessions SET branch_sha = NULL WHERE id = $1`, w.session.ID); err == nil {
		t.Fatal("setup: branch_sha was rewritten; the write-once trigger should refuse")
	}
	// A fresh session whose recorded commit does not exist in the repository.
	other, err := w.fixture.createSession(t, postgres.NewSessionStore(w.ownerPool), nil)
	if err != nil {
		t.Fatal(err)
	}
	quiet(t, w.ownerPool, other)
	if _, err := w.ownerPool.Exec(context.Background(),
		`UPDATE sessions SET state = 'provisioning', branch_sha = $2 WHERE id = $1`,
		other.ID, strings.Repeat("ab", 20)); err != nil {
		t.Fatal(err)
	}
	runner, err := w.service.Provision(w.tenant, w.fixture.workspace.ID, other.ID)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	ctx, cancel := context.WithTimeout(w.tenant, 2*time.Minute)
	defer cancel()
	_, err = w.service.AwaitReady(ctx, w.fixture.workspace.ID, runner, 300*time.Millisecond, func() {})
	if !errors.Is(err, application.ErrRunnerLost) {
		t.Errorf("await ready = %v, want ErrRunnerLost", err)
	}
	if cause, terminal := application.ClassifyRunnerFailure(err); !terminal || cause != application.RunnerFailureLost {
		t.Errorf("classified as %q (terminal %v), want runner_lost", cause, terminal)
	}
}

// TestReconcileTearsDownWhatACrashLeftIntegration: the three things a runner
// manager restart can find, each cleaned up by the next one's reconciliation.
func TestReconcileTearsDownWhatACrashLeftIntegration(t *testing.T) {
	w := newRunnerWorld(t, "Reconcile Workspace")
	ended := w.ready(t)

	// 1. A session that ended while its runner lived — the workflow died
	// before tearing down.
	if _, err := w.ownerPool.Exec(context.Background(),
		`UPDATE sessions SET state = 'failed' WHERE id = $1`, w.session.ID); err != nil {
		t.Fatal(err)
	}

	// 2. A runner whose environment vanished while it was recorded running.
	lostSession, err := w.fixture.createSession(t, postgres.NewSessionStore(w.ownerPool), nil)
	if err != nil {
		t.Fatal(err)
	}
	quiet(t, w.ownerPool, lostSession)
	if _, err := w.ownerPool.Exec(context.Background(),
		`UPDATE sessions SET state = 'provisioning', branch_sha = $2 WHERE id = $1`, lostSession.ID, w.commit); err != nil {
		t.Fatal(err)
	}
	lost, err := w.service.Provision(w.tenant, w.fixture.workspace.ID, lostSession.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(w.tenant, 2*time.Minute)
	defer cancel()
	if lost, err = w.service.AwaitReady(ctx, w.fixture.workspace.ID, lost, 300*time.Millisecond, func() {}); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("docker", "rm", "-f", lost.Handle).CombinedOutput(); err != nil {
		t.Fatalf("kill the container: %v %s", err, out)
	}

	// 3. An environment no runner row names — created and then the process
	// died before anything was recorded.
	orphan := uuid.New()
	orphanHandle, err := w.backend.Provision(context.Background(), application.RunnerSpec{
		RunnerID: orphan, SessionID: uuid.New(), WorkspaceID: w.fixture.workspace.ID,
		CloneURL: "http://host.docker.internal:1/none.git", Branch: "x", Commit: strings.Repeat("0", 40),
	})
	if err != nil {
		t.Fatal(err)
	}

	cleaned, err := w.service.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if cleaned < 3 {
		t.Errorf("reconcile cleaned %d, want at least 3", cleaned)
	}
	for _, handle := range []string{ended.Handle, lost.Handle, orphanHandle} {
		if dockerExists(t, "container", handle) || dockerExists(t, "volume", handle) {
			t.Errorf("%s survived reconciliation", handle)
		}
	}
	states := map[uuid.UUID]string{}
	rows, _ := w.ownerPool.Query(context.Background(), `SELECT id, state FROM runners WHERE id = ANY($1)`,
		[]uuid.UUID{ended.ID, lost.ID})
	for rows.Next() {
		var id uuid.UUID
		var state string
		_ = rows.Scan(&id, &state)
		states[id] = state
	}
	rows.Close()
	if states[ended.ID] != "terminated" {
		t.Errorf("the ended session's runner = %q, want terminated", states[ended.ID])
	}
	if states[lost.ID] != "failed" {
		t.Errorf("the lost runner = %q, want failed", states[lost.ID])
	}
}
