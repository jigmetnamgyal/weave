package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
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
	// ErrRunnerBrokerRefused: the runner's broker credential failed its own
	// check at start.
	ErrRunnerBrokerRefused = errors.New("the runner's event broker credential did not work as scoped")
	// ErrRunnerNotReady: readiness did not arrive within the allowed time.
	ErrRunnerNotReady = errors.New("the runner did not become ready in time")
	// ErrRunnerBackendRefused: the runner backend refused to create or start
	// the environment — credentials rejected, a plan limit reached, or a
	// request it will not accept. Retrying the same request cannot fix it, and
	// the session's reason names the backend rather than GitHub or a checkout
	// (M5.4b).
	ErrRunnerBackendRefused = errors.New("the runner backend refused to start the session's environment")
)

// DefaultEgressHosts is ADR-013's default allowlist, as exact hostnames: git
// over HTTPS to GitHub and the public package registries. The model
// provider's host is M6's to add; the fake provider needs none. Workspace
// additions are M5.4c's. Every host is exact — no wildcards — because a
// hostname-enforcing firewall matches on the TLS SNI and a wildcard over a
// shared domain reaches whatever else is behind it.
var DefaultEgressHosts = []string{
	"github.com", "codeload.github.com",
	"registry.npmjs.org",
	"pypi.org", "files.pythonhosted.org",
	"proxy.golang.org", "sum.golang.org",
	"crates.io", "static.crates.io", "index.crates.io",
	"rubygems.org",
	"repo1.maven.org", "repo.maven.apache.org",
}

// Exit codes the runner uses, so the backend's status can say *why* it
// stopped without the control plane reading anything the runner wrote.
const (
	RunnerExitCloneFailed      = 2
	RunnerExitCheckoutMismatch = 3
	// RunnerExitBrokerRefused: the runner's broker credential did not do what
	// it must — publish its own subject — or did what it must not — publish
	// another session's. It refuses to become ready rather than fail later.
	RunnerExitBrokerRefused = 5
	// RunnerExitProviderFailed: the provider ran and reported failure.
	RunnerExitProviderFailed = 6
	// RunnerExitProviderUnavailable: no adapter for the session's provider,
	// or the adapter refused the model.
	RunnerExitProviderUnavailable = 7
	// RunnerExitPublishFailed: an event could not be confirmed by the stream.
	RunnerExitPublishFailed = 8
	// RunnerExitInterrupted: the runner was signalled to stop while its
	// provider was still running. Never success: the provider did not finish.
	RunnerExitInterrupted = 9
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
	// NATSURL is the event broker as the runner reaches it.
	NATSURL string
	// NATS is the runner's own broker credential (M5.5a): publish to its
	// session's subject only. A secret, delivered like GitToken.
	NATS RunnerCredentials
	// Provider and Model select the adapter the runner runs (M5.5b), read
	// from the session's pinned agent version.
	Provider domain.Provider
	Model    string
	// EgressHosts is every hostname the environment may reach, exactly:
	// DefaultEgressHosts plus the event ingress the runner publishes to
	// (ADR-015). Built by the runner manager, never from input. A backend
	// that enforces egress allows these and nothing else; the dev backend
	// enforces nothing and is refused outside development (ADR-013).
	EgressHosts []string
}

// RunnerCredentials is a runner's NATS identity.
type RunnerCredentials struct {
	// Creds is the decorated JWT and seed, in the form NATS clients read. A
	// secret: never logged, never persisted.
	Creds string
	// Subject is the one subject it may publish to.
	Subject string
	// InboxPrefix is the one reply inbox it may subscribe under.
	InboxPrefix string
	ExpiresAt   time.Time
}

// RunnerCredentialIssuer mints a runner's NATS credential at provisioning.
type RunnerCredentialIssuer interface {
	IssueRunner(runnerID, sessionID uuid.UUID) (RunnerCredentials, error)
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
	// Destroy removes everything the backend holds for a runner — the
	// environment and every writable volume — ADR-013 retains no source after
	// teardown. By runner id, with the handle as a hint when one was recorded:
	// a provision that failed partway may have left storage before any
	// handle existed, and teardown must remove that too.
	Destroy(ctx context.Context, runnerID uuid.UUID, handle string) error
	// HandleFor finds a runner's environment by runner id, for a runner whose
	// handle was never recorded because the process died in between.
	HandleFor(ctx context.Context, runnerID uuid.UUID) (handle string, found bool, err error)
	// List returns every environment this backend holds.
	List(ctx context.Context) ([]BackendRunner, error)
}

// RunnerLeaser is a backend whose environments stop on their own unless kept
// alive: each is created with a short lease the runner manager extends while
// its runner is live (M5.4b). A runner manager that stops extends nothing, so
// its environments stop within one lease — teardown on lost heartbeat, from
// the provider's side. Optional: the dev backend has no leases.
type RunnerLeaser interface {
	// ExtendLease pushes the environment's expiry to a lease from now,
	// never past the backend's hard cap. An environment already stopped or
	// gone is not an error: there is nothing left to keep alive.
	ExtendLease(ctx context.Context, handle string) error
}

// RunnerToReconcile is a live runner seen across every workspace.
type RunnerToReconcile struct {
	Runner       domain.Runner
	SessionState domain.SessionState
	// StateSince is when the runner last changed state — for a terminating
	// runner, when it was halted.
	StateSince time.Time
}

// HaltedRunnerGrace is how long the reconciler leaves a halted runner of a
// running session to its workflow's drain. Past it, the workflow is taken to
// be gone — terminated between halting and ending — and the reconciler
// finishes the teardown, so a failed destroy cannot leave an environment
// forever. It must exceed everything a live workflow can spend halting and
// draining, which a test holds against the activities' own bounds.
const HaltedRunnerGrace = time.Hour

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

// RunnerAgentReader reads the agent version a session pinned, which says which
// provider adapter its runner runs.
type RunnerAgentReader interface {
	GetVersion(ctx context.Context, versionID, workspaceID uuid.UUID) (domain.AgentVersion, error)
}

// EventDrain reports how many of a session's events the stream still holds —
// published and confirmed, not yet stored or quarantined by the ingestor.
type EventDrain interface {
	Pending(ctx context.Context, sessionID uuid.UUID) (uint64, error)
}

// Bounds on a session's run, and the invariant between them and the stream.
//
// The drain's proof — "the stream holds nothing for this session, so every
// event it confirmed was stored or quarantined" — holds only if no event can
// expire out of the stream before the drain ends: a zero count after the
// stream's retention would read a deletion as an ingestion. So the whole life
// of an event in the stream — at most provisioning, the run, and the drain —
// must sit well inside the stream's maximum age. TestTheDrainCannotOutliveTheStream
// checks the real constants, so changing any one of them breaks the build
// rather than the proof.
const (
	// SessionMaxRunTime bounds how long a provider may run before the session
	// is expired. Well inside the runner's twelve-hour broker credential.
	SessionMaxRunTime = 8 * time.Hour
	// DrainTimeout bounds how long a finished session waits for its events to
	// be ingested. Past it the session fails, naming undrained events, rather
	// than ending as though its history were complete.
	DrainTimeout = 10 * time.Minute
)

// ErrEventsNotDrained: the stream still held the session's events when the
// drain's bound was reached — the ingestor down, or behind. Terminal for the
// session: its history cannot be vouched for.
var ErrEventsNotDrained = errors.New("the session's events were not ingested in time")

// ErrRunTimeExceeded: the provider was still running at SessionMaxRunTime.
var ErrRunTimeExceeded = errors.New("the session's provider ran past its maximum run time")

// RunnerService provisions, watches and destroys session environments.
type RunnerService struct {
	runners  RunnerStore
	sessions RunnerSessionReader
	backend  RunnerBackend
	creds    CloneCredentialMinter
	// nats issues each runner its broker credential, and natsURL is the
	// broker as runners reach it.
	nats    RunnerCredentialIssuer
	natsURL string
	agents  RunnerAgentReader
	drain   EventDrain
	bind    TenantBinder
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
	nats RunnerCredentialIssuer,
	natsURL string,
	agents RunnerAgentReader,
	drain EventDrain,
	bind TenantBinder,
	gitBase string,
	logger *slog.Logger,
) *RunnerService {
	return &RunnerService{
		runners: runners, sessions: sessions, backend: backend, creds: creds,
		nats: nats, natsURL: natsURL, agents: agents, drain: drain,
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

	version, err := s.agents.GetVersion(ctx, session.AgentVersionID, workspaceID)
	if err != nil {
		return domain.Runner{}, fmt.Errorf("read the session's agent version: %w", err)
	}

	creds, err := s.creds.CloneCredentialsAsSystem(ctx, workspaceID, session.RepositoryID)
	if err != nil {
		return domain.Runner{}, err
	}
	// Minted per runner, at the moment it is needed, and never stored: the
	// runner holds the only copy, and it expires on its own (ADR-014).
	natsCreds, err := s.nats.IssueRunner(runner.ID, sessionID)
	if err != nil {
		return domain.Runner{}, fmt.Errorf("issue runner broker credential: %w", err)
	}
	handle, err := s.backend.Provision(ctx, RunnerSpec{
		RunnerID:    runner.ID,
		SessionID:   sessionID,
		WorkspaceID: workspaceID,
		CloneURL:    s.gitBase + "/" + creds.Owner + "/" + creds.Name + ".git",
		Branch:      session.BranchName,
		Commit:      session.BranchSHA,
		GitToken:    creds.Token,
		NATSURL:     s.natsURL,
		NATS:        natsCreds,
		Provider:    version.Provider,
		Model:       version.Model,
		EgressHosts: egressHosts(s.natsURL),
	})
	if err != nil {
		return domain.Runner{}, fmt.Errorf("provision runner: %w", err)
	}
	return s.runners.SetHandle(ctx, runner.ID, workspaceID, handle)
}

// egressHosts is what a runner may reach: ADR-013's defaults and the event
// ingress it publishes to, which ADR-015 makes the one Weave-operated host on
// every allowlist. Only the hostname: the port is the firewall's business,
// and the ingress is 443 wherever it is enforced.
func egressHosts(natsURL string) []string {
	hosts := append([]string(nil), DefaultEgressHosts...)
	if parsed, err := url.Parse(natsURL); err == nil && parsed.Hostname() != "" {
		hosts = append(hosts, strings.ToLower(parsed.Hostname()))
	}
	return hosts
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
		case !status.Running && (status.ExitCode == 0 || status.ExitCode == RunnerExitProviderFailed ||
			status.ExitCode == RunnerExitInterrupted):
			// A fast provider can finish before a health check ever reports
			// healthy. These two exits prove readiness anyway: the runner
			// starts its provider only after the checkout and the broker
			// credential are verified, so reaching the provider's end at all
			// means both checks passed.
			running, err := s.runners.MarkRunning(ctx, runner.ID, workspaceID)
			if errors.Is(err, domain.ErrRunnerNotFound) {
				return runner, nil
			}
			return running, err
		case !status.Running:
			switch status.ExitCode {
			case RunnerExitCheckoutMismatch:
				return domain.Runner{}, ErrRunnerCheckoutMismatch
			case RunnerExitBrokerRefused:
				return domain.Runner{}, ErrRunnerBrokerRefused
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

// AwaitExit waits for the session's runner to stop, and returns its exit code.
//
// The exit is read from the backend, never from the runner — the control
// plane's rule since M5.4a. ctx bounds the wait at the session's maximum run
// time; reaching it is ErrRunTimeExceeded, and a cancellation is returned as
// is so it stays retryable.
func (s *RunnerService) AwaitExit(
	ctx context.Context,
	workspaceID, sessionID uuid.UUID,
	poll time.Duration,
	heartbeat func(),
) (int, error) {
	runner, err := s.runners.LiveForSession(ctx, sessionID, workspaceID)
	if err != nil {
		return 0, err
	}
	for {
		status, err := s.backend.Status(ctx, runner.Handle)
		if err != nil {
			return 0, fmt.Errorf("read runner status: %w", err)
		}
		if !status.Exists {
			return 0, ErrRunnerLost
		}
		if !status.Running {
			return status.ExitCode, nil
		}
		heartbeat()
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return 0, fmt.Errorf("%w: %w", ErrRunTimeExceeded, ctx.Err())
			}
			return 0, fmt.Errorf("await runner exit: %w", ctx.Err())
		case <-time.After(poll):
		}
	}
}

// Halt stops the session's runner without ending it: marked terminating, and
// its environment destroyed.
//
// Deliberately short of Teardown. A terminating runner's events are still
// accepted by the ingestor (M5.4a's rule), so anything it published and the
// stream confirmed is stored during the drain that follows — not quarantined
// as `runner_not_bound`, which is what would happen if the runner were ended
// first. The environment is gone either way: ADR-013 keeps no source past the
// provider's end.
func (s *RunnerService) Halt(ctx context.Context, workspaceID, sessionID uuid.UUID) error {
	runner, err := s.runners.LiveForSession(ctx, sessionID, workspaceID)
	if errors.Is(err, domain.ErrRunnerNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := s.runners.MarkTerminating(ctx, runner.ID, workspaceID); err != nil &&
		!errors.Is(err, domain.ErrRunnerNotFound) {
		return fmt.Errorf("mark runner terminating: %w", err)
	}
	if err := s.backend.Destroy(ctx, runner.ID, runner.Handle); err != nil {
		return fmt.Errorf("destroy runner: %w", err)
	}
	return nil
}

// AwaitDrain waits until the stream holds none of the session's events.
//
// The count is the stream's, not the runner's: with work-queue retention an
// unacknowledged message is one the ingestor has not finished with. Zero proves
// every confirmed event was stored or quarantined — given the runner waited
// for every PubAck, the ingestor acknowledges only after its write commits,
// and no event can have expired first (see SessionMaxRunTime).
//
// Reaching ctx's deadline is ErrEventsNotDrained.
func (s *RunnerService) AwaitDrain(ctx context.Context, sessionID uuid.UUID, poll time.Duration, heartbeat func()) error {
	for {
		pending, err := s.drain.Pending(ctx, sessionID)
		if err == nil && pending == 0 {
			return nil
		}
		if err != nil {
			s.logger.WarnContext(ctx, "drain: could not read the stream", slog.String("error", err.Error()))
		}
		heartbeat()
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return fmt.Errorf("%w: %w", ErrEventsNotDrained, ctx.Err())
			}
			return fmt.Errorf("await drain: %w", ctx.Err())
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

	// Always, even with no handle recorded. The previous revision destroyed
	// only when it could find a handle, and HandleFor answers only for a
	// runnable environment — so a provision that created the workspace volume
	// and then failed left that volume behind at teardown, holding what
	// ADR-013 says is gone, until some later orphan sweep.
	if err := s.backend.Destroy(ctx, runner.ID, runner.Handle); err != nil {
		return fmt.Errorf("destroy runner: %w", err)
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
//  2. **The environment is gone** while the runner is recorded running —
//     lost, in the dev backend's terms. Torn down as failed. A *stopped*
//     environment is not lost: since M5.5b it is a finished provider waiting
//     for the workflow to drain its events. Nor is a *terminating* runner of
//     a running session: its workflow halted it and is draining its events.
//     It is left alone until the session moves on — review_ready is not
//     terminal, so that is when it is finished here, not at case 1 — or for
//     at most HaltedRunnerGrace, should its workflow be gone.
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
		if runner.Backend != s.backend.Name() {
			// Another backend's runner; not ours to judge. Nor is it "known"
			// to this backend: an environment in *our* listing that only a
			// foreign row names is one no runner of ours accounts for, and
			// counting it known would shield it from the orphan sweep for as
			// long as that row stayed live.
			continue
		}
		known[runner.ID] = true
		tenant := s.bind(ctx, runner.WorkspaceID)

		if item.SessionState.Terminal() {
			if err := s.teardown(tenant, runner, false, "the session ended"); err != nil {
				s.logger.WarnContext(ctx, "reconcile: teardown failed", slog.String("runner_id", runner.ID.String()), slog.String("error", err.Error()))
				continue
			}
			cleaned++
			continue
		}

		if runner.State == domain.RunnerTerminating && item.SessionState == domain.SessionRunning &&
			time.Since(item.StateSince) < HaltedRunnerGrace {
			// Halted by its workflow and draining (M5.5b): its environment is
			// already gone by design, and its events must still be accepted
			// until the drain finishes. Ending it here — every 30 seconds,
			// against a drain of up to ten minutes — would refuse them as
			// runner_not_bound. The workflow ends it and then the session;
			// once the session has moved on — or past the grace, if its
			// workflow never comes back — the case below finishes it.
			continue
		}
		if runner.Handle == "" {
			// Being provisioned right now, or died before recording its
			// handle. The provisioning activity resolves the first; the
			// session ending resolves the second through case 1.
			continue
		}
		s.extendLease(ctx, runner)
		status, err := s.backend.Status(ctx, runner.Handle)
		if err != nil {
			continue
		}
		// Only a *vanished* environment is lost. A stopped one is how a
		// finished provider looks between its exit and the workflow halting
		// it (M5.5b); ending it here would have the ingestor refuse the events
		// still draining as runner_not_bound — the loss the drain exists to
		// prevent. The workflow always halts and ends it; a session that ends
		// without that is cleaned up by the first case above.
		if runner.State == domain.RunnerTerminating || !status.Exists {
			reason, failed := "the runner's environment stopped", true
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
			if err := s.backend.Destroy(ctx, environment.RunnerID, environment.Handle); err != nil {
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

// extendLease keeps a live runner's environment alive for another lease, on
// a backend that has leases (M5.4b).
//
// Here, in the reconciler, because it already visits every live runner every
// pass, and because the property wanted follows for free: a runner manager
// that is down runs no pass and extends nothing, so its environments stop
// within one lease. Only a runner that should still be running is extended —
// provisioning or running, of a session that has not ended (case 1 has
// already torn those down). A terminating runner's environment is being
// destroyed on purpose. A failure is logged, never fatal to the pass: one
// sandbox's API error must not stop the others being reconciled, and a lease
// has several passes' slack.
func (s *RunnerService) extendLease(ctx context.Context, runner domain.Runner) {
	leaser, ok := s.backend.(RunnerLeaser)
	if !ok || (runner.State != domain.RunnerProvisioning && runner.State != domain.RunnerRunning) {
		return
	}
	if err := leaser.ExtendLease(ctx, runner.Handle); err != nil {
		s.logger.WarnContext(ctx, "reconcile: could not extend a runner's lease",
			slog.String("runner_id", runner.ID.String()), slog.String("error", err.Error()))
	}
}

// RunnerFailure is why a session's runner could not serve it, as a stable
// code carried through workflow history. Like BranchFailure, only this
// package's own words become the reason people read.
type RunnerFailure string

// The runner causes a session can fail with.
const (
	RunnerFailureUnavailable         RunnerFailure = "runner_unavailable"
	RunnerFailureLost                RunnerFailure = "runner_lost"
	RunnerFailureNotReady            RunnerFailure = "runner_not_ready"
	RunnerFailureCheckoutMismatch    RunnerFailure = "checkout_mismatch"
	RunnerFailureNoBranch            RunnerFailure = "no_branch"
	RunnerFailureBrokerRefused       RunnerFailure = "broker_refused"
	RunnerFailureProviderUnavailable RunnerFailure = "provider_unavailable"
	RunnerFailureEventsUnconfirmed   RunnerFailure = "events_unconfirmed"
	RunnerFailureEventsNotDrained    RunnerFailure = "events_not_drained"
	RunnerFailureBackendRefused      RunnerFailure = "backend_refused"
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
	case errors.Is(err, ErrRunnerBrokerRefused):
		return RunnerFailureBrokerRefused, true
	case errors.Is(err, ErrRunnerBackendRefused):
		return RunnerFailureBackendRefused, true
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
	case RunnerFailureBackendRefused:
		return "the runner backend refused to start the session's environment"
	case RunnerFailureLost:
		return "the session's runner stopped before it was ready"
	case RunnerFailureNotReady:
		return "the session's runner did not become ready in time"
	case RunnerFailureCheckoutMismatch:
		return "the runner's checkout was not at the session's recorded commit, so it refused to continue"
	case RunnerFailureNoBranch:
		return "the session has no recorded branch commit to check out"
	case RunnerFailureBrokerRefused:
		return "the session's runner could not confirm its event broker credential"
	case RunnerFailureProviderUnavailable:
		return "the session's runner has no adapter for its agent's provider or model"
	case RunnerFailureEventsUnconfirmed:
		return "the session's runner could not get its events confirmed by the event stream"
	case RunnerFailureEventsNotDrained:
		return "the session's events were not ingested in time, so its history cannot be vouched for"
	case RunnerFailureNoProvider:
		return "the runner is ready with a verified checkout, but no provider adapter exists yet to run this session"
	}
	return BranchFailure(cause).Reason()
}
