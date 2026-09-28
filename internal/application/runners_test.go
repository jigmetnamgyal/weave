package application_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// runnerWorld is RunnerService over a fake backend and store that record
// every call, in order — the orderings are part of what is being tested.
type runnerWorld struct {
	calls   []string
	backend *fakeBackend
	store   *fakeRunnerStore
	service *application.RunnerService
	session domain.Session
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
	w.service = application.NewRunnerService(w.store, fakeSessionReader{w}, w.backend, fakeMinter{},
		func(ctx context.Context, _ uuid.UUID) context.Context { return ctx }, "https://github.test",
		slog.New(slog.NewTextHandler(io.Discard, nil)))
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
func (b *fakeBackend) Destroy(_ context.Context, handle string) error {
	b.w.log("backend.destroy")
	delete(b.envs, handle)
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
	w       *runnerWorld
	runners map[uuid.UUID]domain.Runner
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
		out = append(out, application.RunnerToReconcile{Runner: r, SessionState: state})
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
