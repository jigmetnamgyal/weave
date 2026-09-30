package application_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// runnerWorld is RunnerService over a fake backend and store that record
// every call, in order — the orderings are part of what is being tested.
type runnerWorld struct {
	drain   *fakeDrain
	calls   []string
	backend *fakeBackend
	store   *fakeRunnerStore
	service *application.RunnerService
	session domain.Session
	// added is the host list every runner's snapshot holds; missing makes
	// the snapshot absent, as a row the database never wrote would be.
	added   []string
	missing bool
}

// fakeSnapshots answers as the trigger-written row would: one snapshot per
// runner, holding the world's added hosts, or none at all when missing.
type fakeSnapshots struct{ w *runnerWorld }

func (w *runnerWorld) snapshots() fakeSnapshots { return fakeSnapshots{w} }

func (f fakeSnapshots) Snapshot(_ context.Context, workspaceID, runnerID uuid.UUID) (domain.RunnerEgressSnapshot, error) {
	if f.w.missing {
		return domain.RunnerEgressSnapshot{}, application.ErrEgressSnapshotMissing
	}
	return domain.RunnerEgressSnapshot{RunnerID: runnerID, WorkspaceID: workspaceID, SessionID: f.w.session.ID,
		Hosts: append([]string{}, f.w.added...)}, nil
}

func newRunnerTestWorld(t *testing.T) *runnerWorld {
	t.Helper()
	w := &runnerWorld{}
	w.session = domain.Session{
		ID: uuid.New(), WorkspaceID: uuid.New(), RepositoryID: uuid.New(),
		State: domain.SessionProvisioning, BranchName: "weave/x", BranchSHA: strings.Repeat("a", 40),
	}
	w.backend = &fakeBackend{w: w, envs: map[string]application.RunnerStatus{}}
	w.store = &fakeRunnerStore{w: w, runners: map[uuid.UUID]domain.Runner{}}
	w.drain = &fakeDrain{}
	w.service = application.NewRunnerService(w.store, fakeSessionReader{w}, w.backend, fakeMinter{},
		fakeBrokerIssuer{}, "nats://broker.test:4222", fakeAgents{}, w.drain,
		func(ctx context.Context, _ uuid.UUID) context.Context { return ctx }, "https://github.test",
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithEgress(w.snapshots(), "", nil)
	return w
}

func (w *runnerWorld) log(call string) { w.calls = append(w.calls, call) }

func TestProvisionIsIdempotentAgainstRedelivery(t *testing.T) {
	w := newRunnerTestWorld(t)
	first, err := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || w.backend.provisioned != 1 {
		t.Errorf("redelivery gave %s (backend provisioned %d times), want %s once", second.ID, w.backend.provisioned, first.ID)
	}
	if !strings.HasPrefix(w.backend.lastSpec.CloneURL, "https://github.test/acme/app.git") ||
		w.backend.lastSpec.Commit != w.session.BranchSHA || w.backend.lastSpec.Branch != w.session.BranchName {
		t.Errorf("spec = %+v, want the session's repository, branch and recorded commit", w.backend.lastSpec)
	}
}

// TestAHandleNeverRecordedIsFoundNotReprovisioned: the process died between
// the backend creating the environment and the row recording its handle.
func TestAHandleNeverRecordedIsFoundNotReprovisioned(t *testing.T) {
	w := newRunnerTestWorld(t)
	runner := domain.Runner{ID: uuid.New(), SessionID: w.session.ID, WorkspaceID: w.session.WorkspaceID,
		Backend: "fake", State: domain.RunnerProvisioning}
	w.store.runners[runner.ID] = runner
	w.backend.envs["env-"+runner.ID.String()] = application.RunnerStatus{Exists: true, Running: true}

	got, err := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Handle != "env-"+runner.ID.String() || w.backend.provisioned != 0 {
		t.Errorf("handle %q, provisioned %d; want the existing environment found and none started", got.Handle, w.backend.provisioned)
	}
}

func TestAwaitReadyClassifiesByExitCode(t *testing.T) {
	cases := map[string]struct {
		status application.RunnerStatus
		want   error
	}{
		"checkout mismatch": {application.RunnerStatus{Exists: true, ExitCode: application.RunnerExitCheckoutMismatch}, application.ErrRunnerCheckoutMismatch},
		"clone failed":      {application.RunnerStatus{Exists: true, ExitCode: application.RunnerExitCloneFailed}, application.ErrRunnerLost},
		"vanished":          {application.RunnerStatus{Exists: false}, application.ErrRunnerLost},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := newRunnerTestWorld(t)
			runner, _ := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID)
			w.backend.envs[runner.Handle] = tc.status
			_, err := w.service.AwaitReady(context.Background(), w.session.WorkspaceID, runner, time.Millisecond, func() {})
			if !errors.Is(err, tc.want) {
				t.Errorf("AwaitReady = %v, want %v", err, tc.want)
			}
			if _, terminal := application.ClassifyRunnerFailure(err); !terminal {
				t.Error("classified as transient; retrying cannot help")
			}
		})
	}
}

// TestTeardownMarksBeforeItDestroys: marked terminating before the backend is
// asked, ended only after it confirms — the other order records an
// environment as gone that may still hold a checkout.
func TestTeardownMarksBeforeItDestroys(t *testing.T) {
	w := newRunnerTestWorld(t)
	runner, _ := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID)
	w.calls = nil

	if err := w.service.Teardown(context.Background(), w.session.WorkspaceID, w.session.ID, false, ""); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(w.calls, " → "); got != "store.terminating → backend.destroy → store.end" {
		t.Errorf("teardown order = %s", got)
	}
	if _, exists := w.backend.envs[runner.Handle]; exists {
		t.Error("the environment survived teardown")
	}
	// Idempotent.
	if err := w.service.Teardown(context.Background(), w.session.WorkspaceID, w.session.ID, false, ""); err != nil {
		t.Errorf("a second teardown = %v, want success", err)
	}
}

// TestReconcileListsTheBackendBeforeTheDatabase: a runner row is committed
// before its environment is requested, so listing the backend first means any
// environment seen has a row the later read will see. The other order would
// destroy an environment provisioned between the two reads.
func TestReconcileListsTheBackendBeforeTheDatabase(t *testing.T) {
	w := newRunnerTestWorld(t)
	w.backend.envs["orphan"] = application.RunnerStatus{Exists: true, Running: true}
	w.backend.orphans = map[string]uuid.UUID{"orphan": uuid.New()}

	if _, err := w.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.calls) < 2 || w.calls[0] != "backend.list" || w.calls[1] != "store.list" {
		t.Errorf("reconcile began %v, want the backend listed before the database", w.calls)
	}
	if _, exists := w.backend.envs["orphan"]; exists {
		t.Error("an environment no runner names survived reconciliation")
	}
}

// TestReconcileReachesRunnersPastTheFirstPage is a review finding on PR #19.
// The first version read one batch of the oldest live runners, so a backlog of
// healthy ones hid every newer runner whose session had ended, and a full
// batch switched the orphan sweep off. Here the one runner needing cleanup is
// the newest of 501, and an orphaned environment must still be swept.
func TestReconcileReachesRunnersPastTheFirstPage(t *testing.T) {
	w := newRunnerTestWorld(t)
	w.store.sessionStates = map[uuid.UUID]domain.SessionState{}
	base := time.Now().Add(-time.Hour)
	var newest domain.Runner
	for i := 0; i <= application.ReconcileBatch; i++ {
		r := domain.Runner{
			ID: uuid.New(), SessionID: uuid.New(), WorkspaceID: w.session.WorkspaceID,
			Backend: "fake", State: domain.RunnerRunning, CreatedAt: base.Add(time.Duration(i) * time.Second),
		}
		r.Handle = "env-" + r.ID.String()
		w.backend.envs[r.Handle] = application.RunnerStatus{Exists: true, Running: true, Ready: true}
		w.store.runners[r.ID] = r
		w.store.sessionStates[r.SessionID] = domain.SessionRunning
		newest = r
	}
	w.store.sessionStates[newest.SessionID] = domain.SessionFailed
	w.backend.envs["orphan"] = application.RunnerStatus{Exists: true, Running: true}
	w.backend.orphans = map[string]uuid.UUID{"orphan": uuid.New()}

	if _, err := w.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.store.runners[newest.ID].State.Live() {
		t.Error("the newest runner, on the second page, was never reached")
	}
	if _, exists := w.backend.envs["orphan"]; exists {
		t.Error("a full first page switched off the orphan sweep")
	}
}

// --- fakes -----------------------------------------------------------------

type fakeSessionReader struct{ w *runnerWorld }

func (f fakeSessionReader) Get(_ context.Context, id, workspace uuid.UUID) (domain.Session, error) {
	if id != f.w.session.ID || workspace != f.w.session.WorkspaceID {
		return domain.Session{}, application.ErrSessionNotFound
	}
	return f.w.session, nil
}

type fakeAgents struct{}

func (fakeAgents) GetVersion(context.Context, uuid.UUID, uuid.UUID) (domain.AgentVersion, error) {
	return domain.AgentVersion{Provider: domain.ProviderFake, Model: "deterministic-v1"}, nil
}

// fakeDrain reports a pending count that falls to zero after a set number of
// reads, or never.
type fakeDrain struct {
	pendingReads int
	never        bool
	reads        int
}

func (d *fakeDrain) Pending(context.Context, uuid.UUID) (uint64, error) {
	d.reads++
	if d.never || d.reads <= d.pendingReads {
		return 3, nil
	}
	return 0, nil
}

type fakeBrokerIssuer struct{}

func (fakeBrokerIssuer) IssueRunner(runnerID, sessionID uuid.UUID) (application.RunnerCredentials, error) {
	return application.RunnerCredentials{Creds: "creds-" + runnerID.String(),
		Subject: "weave.session." + sessionID.String() + ".events", InboxPrefix: "_INBOX_x"}, nil
}

type fakeMinter struct{}

func (fakeMinter) CloneCredentialsAsSystem(context.Context, uuid.UUID, uuid.UUID) (application.CloneCredentials, error) {
	return application.CloneCredentials{Owner: "acme", Name: "app", Token: "ghs_scoped"}, nil
}

type fakeBackend struct {
	w           *runnerWorld
	envs        map[string]application.RunnerStatus
	orphans     map[string]uuid.UUID
	provisioned int
	lastSpec    application.RunnerSpec
}

func (b *fakeBackend) Name() string { return "fake" }
func (b *fakeBackend) Provision(_ context.Context, spec application.RunnerSpec) (string, error) {
	b.w.log("backend.provision")
	b.provisioned++
	b.lastSpec = spec
	handle := "env-" + spec.RunnerID.String()
	b.envs[handle] = application.RunnerStatus{Exists: true, Running: true, Ready: true}
	return handle, nil
}
func (b *fakeBackend) Status(_ context.Context, handle string) (application.RunnerStatus, error) {
	return b.envs[handle], nil
}
func (b *fakeBackend) Destroy(_ context.Context, runnerID uuid.UUID, handle string) error {
	b.w.log("backend.destroy")
	delete(b.envs, handle)
	delete(b.envs, "env-"+runnerID.String())
	if b.orphans != nil {
		for h, id := range b.orphans {
			if id == runnerID {
				delete(b.envs, h)
			}
		}
	}
	return nil
}
func (b *fakeBackend) HandleFor(_ context.Context, id uuid.UUID) (string, bool, error) {
	handle := "env-" + id.String()
	_, ok := b.envs[handle]
	return handle, ok, nil
}
func (b *fakeBackend) List(context.Context) ([]application.BackendRunner, error) {
	b.w.log("backend.list")
	var out []application.BackendRunner
	for handle, id := range b.orphans {
		if _, ok := b.envs[handle]; ok {
			out = append(out, application.BackendRunner{RunnerID: id, Handle: handle})
		}
	}
	return out, nil
}

type fakeRunnerStore struct {
	stateSince map[uuid.UUID]time.Time
	w          *runnerWorld
	runners    map[uuid.UUID]domain.Runner
	// sessionStates overrides the world's session state per runner session.
	sessionStates map[uuid.UUID]domain.SessionState
}

func (s *fakeRunnerStore) Create(_ context.Context, r domain.Runner) (domain.Runner, error) {
	for _, existing := range s.runners {
		if existing.SessionID == r.SessionID && existing.State.Live() {
			return domain.Runner{}, application.ErrRunnerExists
		}
	}
	s.runners[r.ID] = r
	return r, nil
}
func (s *fakeRunnerStore) LiveForSession(_ context.Context, session, _ uuid.UUID) (domain.Runner, error) {
	for _, r := range s.runners {
		if r.SessionID == session && r.State.Live() {
			return r, nil
		}
	}
	return domain.Runner{}, domain.ErrRunnerNotFound
}
func (s *fakeRunnerStore) update(id uuid.UUID, edit func(*domain.Runner)) (domain.Runner, error) {
	r, ok := s.runners[id]
	if !ok || !r.State.Live() {
		return domain.Runner{}, domain.ErrRunnerNotFound
	}
	edit(&r)
	s.runners[id] = r
	return r, nil
}
func (s *fakeRunnerStore) SetHandle(_ context.Context, id, _ uuid.UUID, handle string) (domain.Runner, error) {
	return s.update(id, func(r *domain.Runner) { r.Handle = handle })
}
func (s *fakeRunnerStore) MarkRunning(_ context.Context, id, _ uuid.UUID) (domain.Runner, error) {
	return s.update(id, func(r *domain.Runner) { r.State = domain.RunnerRunning })
}
func (s *fakeRunnerStore) MarkTerminating(_ context.Context, id, _ uuid.UUID) (domain.Runner, error) {
	s.w.log("store.terminating")
	return s.update(id, func(r *domain.Runner) { r.State = domain.RunnerTerminating })
}
func (s *fakeRunnerStore) End(_ context.Context, id, _ uuid.UUID, state domain.RunnerState, _ string) (domain.Runner, error) {
	s.w.log("store.end")
	return s.update(id, func(r *domain.Runner) { r.State = state })
}

// ListToReconcile pages as the real function does: (created_at, id) order,
// strictly after the cursor, at most limit.
func (s *fakeRunnerStore) ListToReconcile(_ context.Context, limit int, after application.ReconcileCursor) ([]application.RunnerToReconcile, error) {
	s.w.log("store.list")
	var live []domain.Runner
	for _, r := range s.runners {
		if r.State.Live() {
			live = append(live, r)
		}
	}
	sort.Slice(live, func(i, j int) bool {
		if !live[i].CreatedAt.Equal(live[j].CreatedAt) {
			return live[i].CreatedAt.Before(live[j].CreatedAt)
		}
		return live[i].ID.String() < live[j].ID.String()
	})
	var out []application.RunnerToReconcile
	for _, r := range live {
		if after.ID != uuid.Nil && (r.CreatedAt.Before(after.CreatedAt) ||
			(r.CreatedAt.Equal(after.CreatedAt) && r.ID.String() <= after.ID.String())) {
			continue
		}
		state := s.w.session.State
		if override, ok := s.sessionStates[r.SessionID]; ok {
			state = override
		}
		since := time.Now()
		if at, ok := s.stateSince[r.ID]; ok {
			since = at
		}
		out = append(out, application.RunnerToReconcile{Runner: r, SessionState: state, StateSince: since})
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// TestACancelledWaitIsNotAVerdictOnTheRunner is a review finding on PR #19.
// A runner manager shutting down mid-wait cancels the activity's context; the
// first version called that "not ready in time" — terminal — and failed the
// session over a deploy. Only an expired deadline is a verdict.
func TestACancelledWaitIsNotAVerdictOnTheRunner(t *testing.T) {
	w := newRunnerTestWorld(t)
	runner, _ := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID)
	w.backend.envs[runner.Handle] = application.RunnerStatus{Exists: true, Running: true} // never ready

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := w.service.AwaitReady(cancelled, w.session.WorkspaceID, runner, time.Millisecond, func() {})
	if _, terminal := application.ClassifyRunnerFailure(err); terminal {
		t.Errorf("a cancelled wait (%v) was classified terminal; it must be retried", err)
	}

	expired, cancelExpired := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancelExpired()
	_, err = w.service.AwaitReady(expired, w.session.WorkspaceID, runner, time.Millisecond, func() {})
	if cause, terminal := application.ClassifyRunnerFailure(err); !terminal || cause != application.RunnerFailureNotReady {
		t.Errorf("an expired wait = %v (%q, terminal %v), want runner_not_ready", err, cause, terminal)
	}
}

// TestAForeignRowDoesNotShieldOurEnvironment is a review finding on PR #19:
// a live row under another backend's name used to count as "known", so an
// environment in this backend's listing with the same runner id escaped the
// orphan sweep for as long as that row stayed live.
func TestAForeignRowDoesNotShieldOurEnvironment(t *testing.T) {
	w := newRunnerTestWorld(t)
	id := uuid.New()
	w.store.runners[id] = domain.Runner{ID: id, SessionID: uuid.New(), WorkspaceID: w.session.WorkspaceID,
		Backend: "someone-else", State: domain.RunnerRunning, Handle: "theirs"}
	w.backend.envs["ours"] = application.RunnerStatus{Exists: true, Running: true}
	w.backend.orphans = map[string]uuid.UUID{"ours": id}

	if _, err := w.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, exists := w.backend.envs["ours"]; exists {
		t.Error("an environment only a foreign backend's row names escaped the orphan sweep")
	}
	if !w.store.runners[id].State.Live() {
		t.Error("the foreign backend's row was judged by this backend")
	}
}

// TestAFastProviderIsNotMistakenForALostRunner: the fake finishes in
// milliseconds, before a health check reports healthy. Its exit proves it was
// ready — the runner starts the provider only after both verifications — so
// readiness must read it as ready, not lost.
func TestAFastProviderIsNotMistakenForALostRunner(t *testing.T) {
	for _, code := range []int{0, application.RunnerExitProviderFailed} {
		w := newRunnerTestWorld(t)
		runner, _ := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID)
		w.backend.envs[runner.Handle] = application.RunnerStatus{Exists: true, Running: false, ExitCode: code}
		got, err := w.service.AwaitReady(context.Background(), w.session.WorkspaceID, runner, time.Millisecond, func() {})
		if err != nil || got.State != domain.RunnerRunning {
			t.Errorf("exit %d before readiness was observed = (%v, %v), want running", code, got.State, err)
		}
	}
	// Any other early exit is still lost.
	w := newRunnerTestWorld(t)
	runner, _ := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID)
	w.backend.envs[runner.Handle] = application.RunnerStatus{Exists: true, ExitCode: application.RunnerExitCloneFailed}
	if _, err := w.service.AwaitReady(context.Background(), w.session.WorkspaceID, runner, time.Millisecond, func() {}); !errors.Is(err, application.ErrRunnerLost) {
		t.Errorf("a clone failure = %v, want lost", err)
	}
}

func TestTheSpecPassesTheAgentsProviderAndModel(t *testing.T) {
	w := newRunnerTestWorld(t)
	if _, err := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID); err != nil {
		t.Fatal(err)
	}
	if w.backend.lastSpec.Provider != domain.ProviderFake || w.backend.lastSpec.Model != "deterministic-v1" {
		t.Errorf("spec = %s/%s, want the pinned agent version's", w.backend.lastSpec.Provider, w.backend.lastSpec.Model)
	}
}

func TestTheDrainWaitsForZeroAndGivesUpAtItsBound(t *testing.T) {
	w := newRunnerTestWorld(t)
	w.drain.pendingReads = 3
	if err := w.service.AwaitDrain(context.Background(), w.session.ID, time.Millisecond, func() {}); err != nil {
		t.Errorf("drain = %v, want success once the count reached zero", err)
	}
	if w.drain.reads < 4 {
		t.Errorf("read %d times, want it to wait for zero", w.drain.reads)
	}

	w.drain.never = true
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := w.service.AwaitDrain(ctx, w.session.ID, time.Millisecond, func() {}); !errors.Is(err, application.ErrEventsNotDrained) {
		t.Errorf("drain at its bound = %v, want ErrEventsNotDrained", err)
	}
}

// TestHaltKeepsTheRunnerLive: halted is terminating, not ended, so the events
// still draining are accepted rather than refused as runner_not_bound.
func TestHaltKeepsTheRunnerLive(t *testing.T) {
	w := newRunnerTestWorld(t)
	runner, _ := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID)
	if err := w.service.Halt(context.Background(), w.session.WorkspaceID, w.session.ID); err != nil {
		t.Fatal(err)
	}
	if got := w.store.runners[runner.ID].State; got != domain.RunnerTerminating {
		t.Errorf("halted runner = %s, want terminating", got)
	}
	if _, exists := w.backend.envs[runner.Handle]; exists {
		t.Error("halt left the environment standing")
	}
}

// TestAStoppedRunnerIsTheWorkflowsNotTheReconcilers: a finished provider's
// container is stopped between its exit and the workflow halting it. Ending
// it then would have its draining events refused as runner_not_bound. Only a
// vanished environment is lost.
func TestAStoppedRunnerIsTheWorkflowsNotTheReconcilers(t *testing.T) {
	w := newRunnerTestWorld(t)
	runner, err := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID)
	if err != nil || runner.Handle == "" {
		t.Fatalf("provision: %+v, %v", runner, err)
	}
	w.session.State = domain.SessionRunning
	w.store.runners[runner.ID] = func() domain.Runner { r := w.store.runners[runner.ID]; r.State = domain.RunnerRunning; return r }()
	w.backend.envs[runner.Handle] = application.RunnerStatus{Exists: true, Running: false, ExitCode: 0}

	if _, err := w.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.store.runners[runner.ID].State; got != domain.RunnerRunning {
		t.Errorf("a stopped runner of a running session was reconciled to %s; it is the workflow's to end", got)
	}
}

// TestAHaltedRunnerIsLeftToDrain: halted, a runner is terminating with its
// environment already destroyed — by design — while its events drain. The
// reconciler must neither finish it nor call it lost until the session ends.
func TestAHaltedRunnerIsLeftToDrain(t *testing.T) {
	w := newRunnerTestWorld(t)
	runner, err := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID)
	if err != nil || runner.Handle == "" {
		t.Fatalf("provision: %+v, %v", runner, err)
	}
	w.session.State = domain.SessionRunning
	w.store.runners[runner.ID] = func() domain.Runner { r := w.store.runners[runner.ID]; r.State = domain.RunnerRunning; return r }()
	if err := w.service.Halt(context.Background(), w.session.WorkspaceID, w.session.ID); err != nil {
		t.Fatal(err)
	}

	if _, err := w.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.store.runners[runner.ID].State; got != domain.RunnerTerminating {
		t.Errorf("a halted runner of a running session was reconciled to %s; it is draining", got)
	}

	// Once the session ends, it is the reconciler's.
	w.session.State = domain.SessionReviewReady
	if _, err := w.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.store.runners[runner.ID].State; got == domain.RunnerTerminating || got == domain.RunnerRunning {
		t.Errorf("after the session ended the runner is still %s", got)
	}
}

// TestAnAbandonedHaltedRunnerIsFinishedAfterTheGrace: a workflow terminated
// between halting and ending leaves its runner terminating and its session
// running. Past the grace the reconciler finishes the teardown — so a halt
// whose destroy failed does not leave its environment forever.
func TestAnAbandonedHaltedRunnerIsFinishedAfterTheGrace(t *testing.T) {
	w := newRunnerTestWorld(t)
	runner, err := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID)
	if err != nil {
		t.Fatal(err)
	}
	w.session.State = domain.SessionRunning
	// Halted, but the destroy never happened: the environment still stands.
	w.store.runners[runner.ID] = func() domain.Runner { r := w.store.runners[runner.ID]; r.State = domain.RunnerTerminating; return r }()
	w.store.stateSince = map[uuid.UUID]time.Time{runner.ID: time.Now().Add(-application.HaltedRunnerGrace - time.Minute)}

	if _, err := w.service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := w.store.runners[runner.ID].State; got != domain.RunnerTerminated {
		t.Errorf("an abandoned halted runner = %s, want terminated", got)
	}
	if _, exists := w.backend.envs[runner.Handle]; exists {
		t.Error("its environment was left standing")
	}
}

// TestARunnerMayReachExactlyTheDefaultsAndItsIngress: the egress a backend
// enforces is ADR-013's defaults plus the event ingress (ADR-015) — exact
// hostnames, nothing from the session or its repository. With a registry
// proxy (ADR-016), **every** registry host is forwarded to its own route on
// it; git and the ingress never are. Without one, nothing is forwarded.
func TestARunnerMayReachExactlyTheDefaultsAndItsIngress(t *testing.T) {
	for _, proxy := range []string{"", "https://registry-proxy.test"} {
		w := newRunnerTestWorld(t)
		w.service.WithRegistryProxy(proxy)
		if _, err := w.service.Provision(context.Background(), w.session.WorkspaceID, w.session.ID); err != nil {
			t.Fatal(err)
		}
		var hosts []string
		for _, rule := range w.backend.lastSpec.Egress {
			hosts = append(hosts, rule.Host)
			if strings.ContainsAny(rule.Host, "*:/") || rule.Host != strings.ToLower(rule.Host) {
				t.Errorf("egress host %q is not an exact lowercase hostname", rule.Host)
			}
			forwarded := rule.ForwardURL != ""
			wantForwarded := proxy != "" && application.IsRegistryHost(rule.Host)
			if forwarded != wantForwarded {
				t.Errorf("proxy %q: %s forwarded=%v, want %v", proxy, rule.Host, forwarded, wantForwarded)
			}
			if forwarded && rule.ForwardURL != proxy+"/r/"+rule.Host {
				t.Errorf("%s forwards to %q, want its own route", rule.Host, rule.ForwardURL)
			}
		}
		want := append(append([]string(nil), application.DefaultEgressHosts...), "broker.test")
		if strings.Join(hosts, ",") != strings.Join(want, ",") {
			t.Errorf("egress hosts = %v, want %v", hosts, want)
		}
	}
}

// TestABackendRefusalIsNamedAsTheBackends: distinct from a lost runner or a
// GitHub failure, and terminal.
func TestABackendRefusalIsNamedAsTheBackends(t *testing.T) {
	cause, terminal := application.ClassifyRunnerFailure(fmt.Errorf("vercel: %w: 402", application.ErrRunnerBackendRefused))
	if !terminal || cause != application.RunnerFailureBackendRefused {
		t.Fatalf("classified as %q, %v; want backend_refused, terminal", cause, terminal)
	}
	if reason := application.SessionFailureReason(string(cause)); !strings.Contains(reason, "runner backend") {
		t.Errorf("reason %q does not name the backend", reason)
	}
}

// leasingBackend is a backend with leases (M5.4b), recording extensions and
// status reads, with a configurable cost for each.
type leasingBackend struct {
	*fakeBackend
	mu          sync.Mutex
	extended    []uuid.UUID
	statusReads int
	fail        bool
	// extendCost and statusCost stand in for API round trips; statusCost is
	// Vercel's two-second wait on a running command, scaled down.
	extendCost, statusCost time.Duration
	inFlight, maxInFlight  int
}

var _ application.RunnerLeaser = (*leasingBackend)(nil)

func (b *leasingBackend) ExtendLease(_ context.Context, runnerID uuid.UUID) error {
	b.mu.Lock()
	b.extended = append(b.extended, runnerID)
	b.inFlight++
	b.maxInFlight = max(b.maxInFlight, b.inFlight)
	b.mu.Unlock()
	time.Sleep(b.extendCost)
	b.mu.Lock()
	b.inFlight--
	b.mu.Unlock()
	if b.fail {
		return errors.New("vercel unavailable")
	}
	return nil
}

func (b *leasingBackend) Status(ctx context.Context, handle string) (application.RunnerStatus, error) {
	b.mu.Lock()
	b.statusReads++
	b.mu.Unlock()
	time.Sleep(b.statusCost)
	return b.fakeBackend.Status(ctx, handle)
}

func leasingWorld(t *testing.T) (*runnerWorld, *leasingBackend, *application.RunnerService) {
	w := newRunnerTestWorld(t)
	backend := &leasingBackend{fakeBackend: w.backend}
	service := application.NewRunnerService(w.store, fakeSessionReader{w}, backend, fakeMinter{},
		fakeBrokerIssuer{}, "nats://broker.test:4222", fakeAgents{}, w.drain,
		func(ctx context.Context, _ uuid.UUID) context.Context { return ctx }, "https://github.test",
		slog.New(slog.NewTextHandler(io.Discard, nil))).WithEgress(w.snapshots(), "", nil)
	if w.store.sessionStates == nil {
		w.store.sessionStates = map[uuid.UUID]domain.SessionState{}
	}
	return w, backend, service
}

func addRunner(w *runnerWorld, backend string, state domain.RunnerState, handle string, session domain.SessionState) domain.Runner {
	r := domain.Runner{ID: uuid.New(), SessionID: uuid.New(), WorkspaceID: w.session.WorkspaceID,
		Backend: backend, State: state, Handle: handle, CreatedAt: time.Now()}
	w.store.runners[r.ID] = r
	w.store.sessionStates[r.SessionID] = session
	if handle != "" {
		w.backend.envs[handle] = application.RunnerStatus{Exists: true, Running: true, Ready: true}
	}
	return r
}

// TestExtendLeasesKeepsExactlyTheLiveRunnersAlive: provisioning or running
// runners of a live session, **with or without a handle** — the lease starts
// at creation, before provisioning records one (review of PR #24) — and no
// others. A failing extension does not stop the rest.
func TestExtendLeasesKeepsExactlyTheLiveRunnersAlive(t *testing.T) {
	w, backend, service := leasingWorld(t)
	name := backend.Name()
	running := addRunner(w, name, domain.RunnerRunning, "h-running", domain.SessionRunning)
	provisioning := addRunner(w, name, domain.RunnerProvisioning, "h-provisioning", domain.SessionProvisioning)
	noHandle := addRunner(w, name, domain.RunnerProvisioning, "", domain.SessionProvisioning)
	addRunner(w, name, domain.RunnerTerminating, "h-halted", domain.SessionRunning)
	addRunner(w, name, domain.RunnerTerminating, "h-moved-on", domain.SessionReviewReady)
	addRunner(w, name, domain.RunnerRunning, "h-ended", domain.SessionFailed)
	addRunner(w, "another-backend", domain.RunnerRunning, "h-foreign", domain.SessionRunning)

	backend.fail = true
	if _, err := service.ExtendLeases(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := map[uuid.UUID]bool{}
	for _, id := range backend.extended {
		got[id] = true
	}
	want := map[uuid.UUID]bool{running.ID: true, provisioning.ID: true, noHandle.ID: true}
	if len(got) != len(want) || len(backend.extended) != len(want) {
		t.Errorf("extended %d leases (%v); want exactly the running, the provisioning and the handle-less provisioning runner",
			len(backend.extended), backend.extended)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("runner %s was not extended", id)
		}
	}
}

// TestLeaseRenewalIsNotHeldUpByStatusReads is a review finding on PR #24:
// extensions ran inside the reconcile pass, each after the previous runner's
// status read — up to two seconds on Vercel — so the gap between two
// extensions of one lease grew with the number of live runners, and around a
// hundred and fifty of them it passed the five-minute lease. Scaled down: 160
// running runners, a status read costing 20 ms and an extension 10 ms. The
// lease pass must read no status at all, run extensions concurrently, and
// finish in a fraction of the time the serial pass needed; and Reconcile must
// no longer extend anything.
func TestLeaseRenewalIsNotHeldUpByStatusReads(t *testing.T) {
	w, backend, service := leasingWorld(t)
	backend.extendCost, backend.statusCost = 10*time.Millisecond, 20*time.Millisecond
	const runners = 160
	for i := 0; i < runners; i++ {
		addRunner(w, backend.Name(), domain.RunnerRunning, fmt.Sprintf("h-%d", i), domain.SessionRunning)
	}

	start := time.Now()
	extended, err := service.ExtendLeases(context.Background())
	elapsed := time.Since(start)
	if err != nil || extended != runners {
		t.Fatalf("extended %d, %v; want %d", extended, err, runners)
	}
	serial := runners * (backend.extendCost + backend.statusCost)
	t.Logf("lease pass over %d runners: %s (the serial reconcile-bound pass: %s)", runners, elapsed.Round(time.Millisecond), serial)
	if backend.statusReads != 0 {
		t.Errorf("the lease pass read %d statuses; it must read none", backend.statusReads)
	}
	if backend.maxInFlight < 2 {
		t.Errorf("at most %d extension in flight; they must run concurrently", backend.maxInFlight)
	}
	if elapsed > serial/4 {
		t.Errorf("the lease pass took %s against a serial %s; renewal must not scale with status reads", elapsed, serial)
	}

	before := len(backend.extended)
	if _, err := service.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(backend.extended) != before {
		t.Errorf("Reconcile extended %d leases; renewal belongs to ExtendLeases alone", len(backend.extended)-before)
	}
}
