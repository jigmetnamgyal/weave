package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Errors raised while running a session's environment. Each is terminal for
// the session: waiting will not change it.
var (
	// ErrRunnerNoBranch: the session reached provisioning without a recorded
	// branch commit, so there is nothing exact to check out.
	ErrRunnerNoBranch = errors.New("the session has no recorded branch commit")
	// ErrRunnerLost: the environment stopped before it was ready, or vanished.
	ErrRunnerLost = errors.New("the runner stopped before it was ready")
	// ErrRunnerCheckoutMismatch: the checkout was not at the recorded commit.
	// The runner refuses to proceed rather than work on unknown code.
	ErrRunnerCheckoutMismatch = errors.New("the runner's checkout is not at the session's recorded commit")
	// ErrRunnerNotReady: readiness did not arrive within the allowed time.
	ErrRunnerNotReady = errors.New("the runner did not become ready in time")
)

// Exit codes the runner uses, so the backend's status can say *why* it
// stopped without the control plane reading anything the runner wrote.
const (
	RunnerExitCloneFailed      = 2
	RunnerExitCheckoutMismatch = 3
)

// RunnerSpec is everything an environment needs at start. Delivered at start,
// never baked into an image.
type RunnerSpec struct {
	RunnerID    uuid.UUID
	SessionID   uuid.UUID
	WorkspaceID uuid.UUID
	// CloneURL is the repository's git URL. Carries no credential.
	CloneURL string
	// Branch is the session branch, cut in M5.2.
	Branch string
	// Commit is sessions.branch_sha. The checkout must be at exactly this.
	Commit string
	// GitToken is a short-lived installation token scoped to this one
	// repository with contents:read. Never logged, never persisted.
	GitToken string
}

// RunnerStatus is what a backend reports about an environment.
type RunnerStatus struct {
	// Exists is false once the environment is gone.
	Exists  bool
	Running bool
	// Ready means the runner reported its checkout complete.
	Ready    bool
	ExitCode int
}

// BackendRunner is an environment a backend holds, for reconciliation.
type BackendRunner struct {
	RunnerID uuid.UUID
	Handle   string
}

// RunnerBackend creates and destroys execution environments.
//
// The port ADR-013 puts between the product and whichever isolation provider
// runs untrusted code. The dev backend implements it with Docker and is not a
// security boundary; M5.4b's production backend implements it with a vendor.
//
// Every method is idempotent, because Temporal redelivers and the reconciler
// repeats: provisioning a runner id that already has an environment returns
// that environment, and destroying one that is already gone succeeds.
type RunnerBackend interface {
	// Name is stored on the runner row, so a runner is always torn down by
	// the backend that made it.
	Name() string
	Provision(ctx context.Context, spec RunnerSpec) (handle string, err error)
	Status(ctx context.Context, handle string) (RunnerStatus, error)
	// Destroy removes the environment and every writable volume it had —
	// ADR-013 retains no source after teardown.
	Destroy(ctx context.Context, handle string) error
	// HandleFor finds a runner's environment by runner id, for a runner whose
	// handle was never recorded because the process died in between.
	HandleFor(ctx context.Context, runnerID uuid.UUID) (handle string, found bool, err error)
	// List returns every environment this backend holds.
	List(ctx context.Context) ([]BackendRunner, error)
}

// RunnerToReconcile is a live runner seen across every workspace.
type RunnerToReconcile struct {
	Runner       domain.Runner
	SessionState domain.SessionState
}

// ReconcileCursor pages through live runners. The zero value is the first
// page.
type ReconcileCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// RunnerStore persists runners. Every method but ListToReconcile runs in the
// tenant context on ctx.
type RunnerStore interface {
	// Create inserts a runner. ErrRunnerExists if the session already has a
	// live one.
	Create(ctx context.Context, runner domain.Runner) (domain.Runner, error)
	LiveForSession(ctx context.Context, sessionID, workspaceID uuid.UUID) (domain.Runner, error)
	SetHandle(ctx context.Context, runnerID, workspaceID uuid.UUID, handle string) (domain.Runner, error)
	MarkRunning(ctx context.Context, runnerID, workspaceID uuid.UUID) (domain.Runner, error)
	MarkTerminating(ctx context.Context, runnerID, workspaceID uuid.UUID) (domain.Runner, error)
	// End records the runner as terminated or failed. ErrRunnerNotFound if it
	// is no longer live — already ended by someone else.
	End(ctx context.Context, runnerID, workspaceID uuid.UUID, state domain.RunnerState, reason string) (domain.Runner, error)
	// ListToReconcile reads one page of live runners across every workspace,
	// in (created_at, id) order after the cursor, through a bounded
	// privileged function. No tenant needed.
	ListToReconcile(ctx context.Context, limit int, after ReconcileCursor) ([]RunnerToReconcile, error)
}

// ErrRunnerExists is returned by RunnerStore.Create when a live runner is
// already there — the one-live-runner index doing its job.
var ErrRunnerExists = errors.New("the session already has a live runner")

// CloneCredentials is how a runner reaches its repository.
type CloneCredentials struct {
	Owner string
	Name  string
	// Token is scoped to this repository with contents:read, and expires
	// within the hour. Never logged, never persisted.
	Token string
}

// CloneCredentialMinter issues repository-scoped read credentials on the
// product's behalf. Implemented by InstallationService.
type CloneCredentialMinter interface {
	CloneCredentialsAsSystem(ctx context.Context, workspaceID, repositoryID uuid.UUID) (CloneCredentials, error)
}

// RunnerSessionReader reads the session a runner is for.
type RunnerSessionReader interface {
	Get(ctx context.Context, sessionID, workspaceID uuid.UUID) (domain.Session, error)
}

// RunnerService provisions, watches and destroys session environments.
type RunnerService struct {
	runners  RunnerStore
	sessions RunnerSessionReader
	backend  RunnerBackend
	creds    CloneCredentialMinter
	bind     TenantBinder
	// gitBase is where repositories are cloned from: https://github.com in
	// production, a local git server in tests.
	gitBase string
	logger  *slog.Logger
}

// NewRunnerService wires the service.
func NewRunnerService(
	runners RunnerStore,
	sessions RunnerSessionReader,
	backend RunnerBackend,
	creds CloneCredentialMinter,
	bind TenantBinder,
	gitBase string,
	logger *slog.Logger,
) *RunnerService {
	return &RunnerService{
		runners: runners, sessions: sessions, backend: backend, creds: creds,
		bind: bind, gitBase: gitBase, logger: logger,
	}
}

// Provision gives a provisioning session its runner, and returns once the
// backend has an environment for it. Readiness is AwaitReady's.
//
// **Idempotent against redelivery**, which is what Temporal will do:
//
//   - a live runner for the session is reused, never replaced — the
//     one-live-runner index makes a second impossible, and this finds the
//     first rather than tripping on it;
//   - a runner row without a handle means the process died between creating
//     the row and recording what the backend returned, so the backend is
//     asked by runner id before anything new is started;
//   - the backend itself is idempotent by runner id.
//
// Everything is read from the session row: repository, branch, commit.
func (s *RunnerService) Provision(ctx context.Context, workspaceID, sessionID uuid.UUID) (domain.Runner, error) {
	session, err := s.sessions.Get(ctx, sessionID, workspaceID)
	if err != nil {
		return domain.Runner{}, err
	}
	if session.BranchSHA == "" {
		return domain.Runner{}, ErrRunnerNoBranch
	}

	runner, err := s.runners.LiveForSession(ctx, sessionID, workspaceID)
	switch {
	case errors.Is(err, domain.ErrRunnerNotFound):
		// Only a provisioning session gets a new runner. One that has moved
		// on — cancelled, or failed by something else — must not have an
		// environment started for it now.
		if session.State != domain.SessionProvisioning {
			return domain.Runner{}, fmt.Errorf("%w: it is %s", ErrSessionNotProvisioning, session.State)
		}
		runner, err = s.create(ctx, session)
		if err != nil {
			return domain.Runner{}, err
		}
	case err != nil:
		return domain.Runner{}, err
	}

	if runner.Handle != "" {
		return runner, nil
	}

	// A handle may exist in the backend without being recorded.
	if handle, found, err := s.backend.HandleFor(ctx, runner.ID); err != nil {
		return domain.Runner{}, fmt.Errorf("look for the runner's environment: %w", err)
	} else if found {
		return s.runners.SetHandle(ctx, runner.ID, workspaceID, handle)
	}

	creds, err := s.creds.CloneCredentialsAsSystem(ctx, workspaceID, session.RepositoryID)
	if err != nil {
		return domain.Runner{}, err
	}
	handle, err := s.backend.Provision(ctx, RunnerSpec{
		RunnerID:    runner.ID,
		SessionID:   sessionID,
		WorkspaceID: workspaceID,
		CloneURL:    s.gitBase + "/" + creds.Owner + "/" + creds.Name + ".git",
		Branch:      session.BranchName,
		Commit:      session.BranchSHA,
		GitToken:    creds.Token,
	})
	if err != nil {
		return domain.Runner{}, fmt.Errorf("provision runner: %w", err)
	}
	return s.runners.SetHandle(ctx, runner.ID, workspaceID, handle)
}

// create inserts the runner row before anything exists in the backend.
//
// Row first, environment second: if the process dies between the two, the
// reconciler finds a runner with no handle and asks the backend by runner id.
// The other order would leave an environment no row names — which is exactly
// what the orphan sweep has to guess about.
func (s *RunnerService) create(ctx context.Context, session domain.Session) (domain.Runner, error) {
	id, err := domain.NewRunnerID()
	if err != nil {
		return domain.Runner{}, err
	}
	runner, err := s.runners.Create(ctx, domain.Runner{
		ID: id, SessionID: session.ID, WorkspaceID: session.WorkspaceID,
		Backend: s.backend.Name(), State: domain.RunnerProvisioning,
	})
	if errors.Is(err, ErrRunnerExists) {
		// A concurrent delivery created it first.
		return s.runners.LiveForSession(ctx, session.ID, session.WorkspaceID)
	}
	return runner, err
}

// AwaitReady waits for a provisioned runner to report its checkout complete,
// then records it running.
//
// heartbeat is called on every poll, so a Temporal activity waiting here
// keeps its heartbeat alive and a dead worker is noticed. An environment that
// stops before it is ready is classified by its exit code — the only thing
// the control plane reads from it — never by anything it printed.
func (s *RunnerService) AwaitReady(
	ctx context.Context,
	workspaceID uuid.UUID,
	runner domain.Runner,
	poll time.Duration,
	heartbeat func(),
) (domain.Runner, error) {
	if runner.State == domain.RunnerRunning {
		return runner, nil
	}
	for {
		status, err := s.backend.Status(ctx, runner.Handle)
		if err != nil {
			return domain.Runner{}, fmt.Errorf("read runner status: %w", err)
		}
		switch {
		case status.Ready && status.Running:
			running, err := s.runners.MarkRunning(ctx, runner.ID, workspaceID)
			if errors.Is(err, domain.ErrRunnerNotFound) {
				// Already marked by an earlier delivery.
				return runner, nil
			}
			return running, err
		case !status.Exists:
			return domain.Runner{}, ErrRunnerLost
		case !status.Running:
			if status.ExitCode == RunnerExitCheckoutMismatch {
				return domain.Runner{}, ErrRunnerCheckoutMismatch
			}
			return domain.Runner{}, fmt.Errorf("%w: exit code %d", ErrRunnerLost, status.ExitCode)
		}

		heartbeat()
		select {
		case <-ctx.Done():
			// Only an expired readiness deadline is a verdict on the runner.
			// A cancellation — the runner manager shutting down mid-wait, a
			// workflow cancelled — is not, and must stay retryable: the retry
			// finds the same runner. The first version classified every
			// ctx.Done() as not-ready, so a deploy during the wait failed the
			// session for good.
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return domain.Runner{}, fmt.Errorf("%w: %w", ErrRunnerNotReady, ctx.Err())
			}
			return domain.Runner{}, fmt.Errorf("await runner readiness: %w", ctx.Err())
		case <-time.After(poll):
		}
	}
}

// Teardown destroys the session's live runner, if it has one.
//
// Idempotent: no live runner is success, an environment already gone is
// success. The runner is marked terminating **before** the backend is asked,
// so a crash mid-teardown leaves a runner the reconciler knows to finish,
// and ended only after the backend confirms — never the other way round,
// which would record an environment as gone that still holds a checkout.
func (s *RunnerService) Teardown(
	ctx context.Context,
	workspaceID, sessionID uuid.UUID,
	failed bool,
	reason string,
) error {
	runner, err := s.runners.LiveForSession(ctx, sessionID, workspaceID)
	if errors.Is(err, domain.ErrRunnerNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.teardown(ctx, runner, failed, reason)
}

func (s *RunnerService) teardown(ctx context.Context, runner domain.Runner, failed bool, reason string) error {
	if _, err := s.runners.MarkTerminating(ctx, runner.ID, runner.WorkspaceID); err != nil {
		if errors.Is(err, domain.ErrRunnerNotFound) {
			return nil
		}
		return fmt.Errorf("mark runner terminating: %w", err)
	}

	handle := runner.Handle
	if handle == "" {
		found, ok, err := s.backend.HandleFor(ctx, runner.ID)
		if err != nil {
			return fmt.Errorf("look for the runner's environment: %w", err)
		}
		if ok {
			handle = found
		}
	}
	if handle != "" {
		if err := s.backend.Destroy(ctx, handle); err != nil {
			return fmt.Errorf("destroy runner: %w", err)
		}
	}

	state := domain.RunnerTerminated
	if failed {
		state = domain.RunnerFailed
	}
	if _, err := s.runners.End(ctx, runner.ID, runner.WorkspaceID, state, reason); err != nil &&
		!errors.Is(err, domain.ErrRunnerNotFound) {
		return fmt.Errorf("record runner ended: %w", err)
	}
	s.logger.InfoContext(ctx, "runner torn down",
		slog.String("runner_id", runner.ID.String()),
		slog.String("session_id", runner.SessionID.String()),
		slog.String("state", string(state)))
	return nil
}

// ReconcileBatch is the page size for reading live runners.
const ReconcileBatch = 500

// reconcileMaxPages bounds one pass. Past it the pass stops early and skips
// the orphan sweep, which needs the complete live set to be safe; the next
// pass starts again from the beginning. Two hundred pages is a hundred
// thousand live runners, far beyond anything this backend will hold.
const reconcileMaxPages = 200

// Reconcile finds runners a crash, a lost environment or an ended session has
// left behind, and tears them down. Run at startup and periodically.
//
// Three cases:
//
//  1. **The session has ended** but its runner is live — a workflow that
//     died before tearing down. Torn down.
//  2. **The environment is gone or stopped** while the runner is recorded
//     live — lost heartbeat, in the dev backend's terms. Torn down as
//     failed.
//  3. **An environment exists that no live runner names** — the manager
//     died after the backend created it and before anything recorded
//     otherwise, or a runner ended with its destroy half-done. Destroyed.
//
// The backend is listed **before** the database is read. A runner row is
// committed before its environment is requested, so any environment present
// at the backend listing has a row the later read will see. The other order
// would destroy an environment provisioned between the two reads.
func (s *RunnerService) Reconcile(ctx context.Context) (int, error) {
	environments, err := s.backend.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list backend environments: %w", err)
	}

	// Every live runner, a page at a time. The first version read one batch
	// of the oldest, so a backlog of healthy old runners hid every newer one
	// — including ended sessions and lost environments — and a full batch
	// switched the orphan sweep off for good.
	var (
		live     []RunnerToReconcile
		cursor   ReconcileCursor
		complete bool
	)
	for page := 0; page < reconcileMaxPages; page++ {
		batch, err := s.runners.ListToReconcile(ctx, ReconcileBatch, cursor)
		if err != nil {
			return 0, fmt.Errorf("list live runners: %w", err)
		}
		live = append(live, batch...)
		if len(batch) < ReconcileBatch {
			complete = true
			break
		}
		last := batch[len(batch)-1].Runner
		cursor = ReconcileCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	cleaned := 0
	known := make(map[uuid.UUID]bool, len(live))
	for _, item := range live {
		runner := item.Runner
		known[runner.ID] = true
		if runner.Backend != s.backend.Name() {
			continue // another backend's runner; not ours to judge
		}
		tenant := s.bind(ctx, runner.WorkspaceID)

		if item.SessionState.Terminal() {
			if err := s.teardown(tenant, runner, false, "the session ended"); err != nil {
				s.logger.WarnContext(ctx, "reconcile: teardown failed", slog.String("runner_id", runner.ID.String()), slog.String("error", err.Error()))
				continue
			}
			cleaned++
			continue
		}

		if runner.Handle == "" {
			// Being provisioned right now, or died before recording its
			// handle. The provisioning activity resolves the first; the
			// session ending resolves the second through case 1.
			continue
		}
		status, err := s.backend.Status(ctx, runner.Handle)
		if err != nil {
			continue
		}
		lost := !status.Exists || (runner.State == domain.RunnerRunning && !status.Running)
		if runner.State == domain.RunnerTerminating || lost {
			reason := "the runner's environment stopped"
			failed := true
			if runner.State == domain.RunnerTerminating {
				reason, failed = "teardown finished by the reconciler", false
			}
			if err := s.teardown(tenant, runner, failed, reason); err == nil {
				cleaned++
			}
		}
	}

	// Only with the complete live set: an environment whose runner was simply
	// on an unread page would otherwise look orphaned.
	if complete {
		for _, environment := range environments {
			if known[environment.RunnerID] {
				continue
			}
			if err := s.backend.Destroy(ctx, environment.Handle); err != nil {
				s.logger.WarnContext(ctx, "reconcile: could not destroy an orphaned environment",
					slog.String("runner_id", environment.RunnerID.String()), slog.String("error", err.Error()))
				continue
			}
			s.logger.InfoContext(ctx, "reconcile: destroyed an environment no live runner names",
				slog.String("runner_id", environment.RunnerID.String()))
			cleaned++
		}
	}
	return cleaned, nil
}

// RunnerFailure is why a session's runner could not serve it, as a stable
// code carried through workflow history. Like BranchFailure, only this
// package's own words become the reason people read.
type RunnerFailure string

// The runner causes a session can fail with.
const (
	RunnerFailureUnavailable      RunnerFailure = "runner_unavailable"
	RunnerFailureLost             RunnerFailure = "runner_lost"
	RunnerFailureNotReady         RunnerFailure = "runner_not_ready"
	RunnerFailureCheckoutMismatch RunnerFailure = "checkout_mismatch"
	RunnerFailureNoBranch         RunnerFailure = "no_branch"
	// RunnerFailureNoProvider is how every session ends until M5.5: the
	// runner came up with a verified checkout, and there is no provider
	// adapter to run in it yet.
	RunnerFailureNoProvider RunnerFailure = "no_provider"
)

// ClassifyRunnerFailure reports whether err is a runner failure retrying the
// same activity cannot fix, and which.
func ClassifyRunnerFailure(err error) (RunnerFailure, bool) {
	switch {
	case errors.Is(err, ErrRunnerCheckoutMismatch):
		return RunnerFailureCheckoutMismatch, true
	case errors.Is(err, ErrRunnerLost):
		return RunnerFailureLost, true
	case errors.Is(err, ErrRunnerNotReady):
		return RunnerFailureNotReady, true
	case errors.Is(err, ErrRunnerNoBranch):
		return RunnerFailureNoBranch, true
	default:
		return "", false
	}
}

// SessionFailureReason is the transition reason for a failure cause, whether
// it came from the branch step or the runner step.
func SessionFailureReason(cause string) string {
	switch RunnerFailure(cause) {
	case RunnerFailureUnavailable:
		return "the session's runner could not be started"
	case RunnerFailureLost:
		return "the session's runner stopped before it was ready"
	case RunnerFailureNotReady:
		return "the session's runner did not become ready in time"
	case RunnerFailureCheckoutMismatch:
		return "the runner's checkout was not at the session's recorded commit, so it refused to continue"
	case RunnerFailureNoBranch:
		return "the session has no recorded branch commit to check out"
	case RunnerFailureNoProvider:
		return "the runner is ready with a verified checkout, but no provider adapter exists yet to run this session"
	}
	return BranchFailure(cause).Reason()
}
