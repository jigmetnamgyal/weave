package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Errors the GitHub application service raises.
var (
	// ErrInstallationNotFound is returned when no installation matches, for
	// any reason. A caller who is not a member and a caller naming an
	// installation that never existed receive the same thing, so probing
	// cannot distinguish them.
	ErrInstallationNotFound = errors.New("installation not found")
	// ErrRepositoryNotFound is returned when no repository matches.
	ErrRepositoryNotFound = errors.New("repository not found")
	// ErrTaskNotFound is returned when no task matches, for any reason. A
	// caller who is not a member and one naming a task that never existed
	// receive the same thing, so probing cannot distinguish them.
	ErrTaskNotFound = errors.New("task not found")
	// ErrAgentNotFound is returned when no agent matches.
	ErrAgentNotFound = errors.New("agent not found")
	// ErrInstallStateInvalid is returned when the state value on a callback is
	// unknown, expired, or already used. One error for all three: they are
	// indistinguishable to the caller by design, because telling an attacker
	// which of the three happened tells them whether they guessed a real
	// value.
	ErrInstallStateInvalid = errors.New("installation state is not valid")
)

// DeliveryClaim is the outcome of trying to take ownership of a webhook
// delivery.
type DeliveryClaim int

const (
	// DeliveryClaimed means this caller should process the delivery.
	DeliveryClaimed DeliveryClaim = iota
	// DeliveryAlreadyDone means the effect is durable and a retry should be
	// acknowledged without repeating it.
	DeliveryAlreadyDone
	// DeliveryInFlight means another attempt holds the delivery and has not
	// finished. It must not be acknowledged: GitHub does not retry what it
	// believes succeeded, so answering 2xx here would discard the delivery if
	// the attempt in flight then failed.
	DeliveryInFlight
)

// ErrAuditNotRecorded is returned when an external write succeeded but its
// audit row did not.
//
// A distinct error because the two facts it carries pull in opposite
// directions: the operation did what was asked, and a record that should exist
// does not. Collapsing it into a generic failure tells the caller nothing
// happened when something irreversible did.
var ErrAuditNotRecorded = errors.New("the change was made but could not be recorded in the audit trail")

// ErrDeliveryInFlight is returned when another attempt is still processing a
// delivery. The caller answers with a non-2xx status so GitHub retries later.
var ErrDeliveryInFlight = errors.New("delivery is already being processed")

// InstallationRef is the little we can learn about an installation without
// tenant context: which workspace it belongs to, and whether it is suspended.
type InstallationRef struct {
	InstallationID uuid.UUID
	WorkspaceID    uuid.UUID
	Suspended      bool
}

// InstallationRepository is the persistence port for installations.
type InstallationRepository interface {
	Connect(ctx context.Context, installation domain.Installation, actor Actor, event AuditEvent) (domain.Installation, error)
	Resolve(ctx context.Context, githubID int64) (InstallationRef, error)
	Get(ctx context.Context, installationID, workspaceID uuid.UUID) (domain.Installation, error)
	List(ctx context.Context, workspaceID uuid.UUID) ([]domain.Installation, error)
	SetSuspended(ctx context.Context, installationID, workspaceID uuid.UUID, suspendedAt *time.Time, event AuditEvent) error
	MarkDeleted(ctx context.Context, installationID, workspaceID uuid.UUID, event AuditEvent) error
	Reconcile(ctx context.Context, installationID, workspaceID uuid.UUID, selection domain.RepositorySelection, repositories []domain.Repository, permissions map[string]string) error
	ListRepositories(ctx context.Context, workspaceID uuid.UUID) ([]domain.Repository, error)
	GetRepository(ctx context.Context, repositoryID, workspaceID uuid.UUID) (domain.Repository, error)
	ListPermissions(ctx context.Context, installationID, workspaceID uuid.UUID) (map[string]string, error)
	// AppendAudit records an event with no other change. Every other write
	// here audits inside its own transaction; a branch is created on GitHub,
	// so there is no local transaction to join and the record stands alone.
	AppendAudit(ctx context.Context, event AuditEvent) error
	ClaimDelivery(ctx context.Context, deliveryID, event, action string) (DeliveryClaim, error)
	CompleteDelivery(ctx context.Context, deliveryID string) error
	PruneDeliveries(ctx context.Context, retention time.Duration) (int64, error)
}

// RemoteInstallation is what GitHub reports about an installation.
type RemoteInstallation struct {
	AccountLogin        string
	AccountType         string
	RepositorySelection string
	Permissions         map[string]string
	SuspendedAt         *time.Time
}

// RemoteRepository is what GitHub reports about a repository.
type RemoteRepository struct {
	GitHubID      int64
	Owner         string
	Name          string
	DefaultBranch string
	Private       bool
}

// RemoteBranch is what GitHub reports about an existing branch.
type RemoteBranch struct {
	Name string
	SHA  string
	// Protected is GitHub's own answer, which accounts for pattern rules and
	// rulesets that a list of names would not.
	Protected bool
}

// BranchRule is whether a ruleset governs writing a particular ref.
type BranchRule struct {
	// Restricted is true when a rule governs creating or moving the ref.
	Restricted bool
	// Rule names the rule that restricts it, for the refusal message.
	Rule string
}

// GitHubAPI is the port onto GitHub itself.
type GitHubAPI interface {
	Installation(ctx context.Context, githubInstallationID int64) (RemoteInstallation, error)
	InstallationRepositories(ctx context.Context, githubInstallationID int64) ([]RemoteRepository, error)
	// Branch returns ErrInstallationNotFound's remote equivalent when absent;
	// callers distinguish "no such branch" from a failure by that error.
	Branch(ctx context.Context, githubInstallationID int64, owner, repo, branch string) (RemoteBranch, error)
	CreateBranch(ctx context.Context, githubInstallationID int64, owner, repo, name, sha string) (RemoteBranch, error)
	// BranchRules answers for names that do not exist yet, which is the only
	// way to know whether a ruleset governs a branch about to be created.
	BranchRules(ctx context.Context, githubInstallationID int64, owner, repo, branch string) (BranchRule, error)
}

// ErrRemoteNotFound is the port's "no such thing on GitHub". Adapters
// translate their own not-found into it so the service does not import them.
var ErrRemoteNotFound = errors.New("not found on github")

// ErrRemoteRefExists is the port's "a ref of that name is already there". It
// is not a failure — see CreateBranch.
var ErrRemoteRefExists = errors.New("reference already exists on github")

// ErrRemoteRefused is the port's "GitHub was reached and said no" — a 403 that
// is not a suspension, or a 422 that is not a duplicate ref. Terminal: the
// same request will be refused again, so retrying only delays a failure and
// then misreports it as an outage.
var ErrRemoteRefused = errors.New("refused by github")

// InstallStateRepository stores the single-use value that carries intent
// across the installation round trip.
type InstallStateRepository interface {
	// Put stores a state for at most ttl.
	Put(ctx context.Context, state domain.InstallState, ttl time.Duration) error
	// Take returns a state and removes it in one operation.
	//
	// One operation, not a read followed by a delete: two requests replaying
	// the same callback concurrently must not both succeed, and a read-then-
	// delete leaves exactly that window open.
	Take(ctx context.Context, value string) (domain.InstallState, error)
}

// TenantBinder attaches a workspace to a context so row-level security applies
// to the work that follows.
//
// A port rather than a direct call, because the tenant context helper lives in
// the PostgreSQL adapter and this package must not depend on it. It exists for
// the webhook path: a delivery arrives with no user and no workspace, so the
// handler establishes the tenant only after resolving the installation.
type TenantBinder func(ctx context.Context, workspaceID uuid.UUID) context.Context

// InstallationService connects workspaces to GitHub.
type InstallationService struct {
	installations InstallationRepository
	states        InstallStateRepository
	api           GitHubAPI
	bind          TenantBinder
	appSlug       string
}

// NewInstallationService wires the service.
func NewInstallationService(
	installations InstallationRepository,
	states InstallStateRepository,
	api GitHubAPI,
	bind TenantBinder,
	appSlug string,
) *InstallationService {
	return &InstallationService{
		installations: installations,
		states:        states,
		api:           api,
		bind:          bind,
		appSlug:       appSlug,
	}
}

// Audit action names for the GitHub integration. Stable strings: operators
// query them and they must not change meaning between releases.
const (
	AuditInstallationConnected    = "github.installation.connected"
	AuditInstallationDeleted      = "github.installation.deleted"
	AuditInstallationSuspended    = "github.installation.suspended"
	AuditInstallationUnsuspended  = "github.installation.unsuspended"
	AuditRepositoriesReconciled   = "github.repositories.reconciled"
	AuditInstallationRebindDenied = "github.installation.rebind_denied"
	AuditBranchCreated            = "github.branch.created"
)

// BeginInstall returns the URL to send someone to, having first recorded which
// workspace the resulting installation belongs to.
//
// The recording is the point. GitHub hands back an installation id and nothing
// identifying the workspace, so if the workspace came from anything the caller
// controls at that moment, whoever held an installation id could bind it where
// they liked.
func (s *InstallationService) BeginInstall(ctx context.Context, membership domain.Membership) (string, error) {
	if !membership.Can(domain.PermissionRepositoryManage) {
		return "", fmt.Errorf("%w: %s requires %s",
			ErrPermissionDenied, membership.Role, domain.PermissionRepositoryManage)
	}

	state, err := domain.NewInstallState(membership.WorkspaceID, membership.UserID)
	if err != nil {
		return "", err
	}
	if err := s.states.Put(ctx, state, domain.InstallStateLifetime); err != nil {
		return "", fmt.Errorf("store install state: %w", err)
	}
	return fmt.Sprintf("https://github.com/apps/%s/installations/new?state=%s",
		url.PathEscape(s.appSlug), url.QueryEscape(state.Value)), nil
}

// CompleteInstall binds an installation to the workspace the state names.
//
// The order matters. The state is consumed first, so a replayed callback fails
// before anything else happens. Permission is re-checked next, because the
// state proves intent and not current authority — a browser can sit on
// GitHub's consent screen long enough for membership to change. Only then is
// the installation id confirmed with GitHub, which turns a claim in a query
// string into a fact.
func (s *InstallationService) CompleteInstall(
	ctx context.Context,
	userID uuid.UUID,
	stateValue string,
	githubInstallationID int64,
) (domain.Installation, error) {
	state, err := s.states.Take(ctx, stateValue)
	if err != nil {
		return domain.Installation{}, ErrInstallStateInvalid
	}

	remote, err := s.api.Installation(ctx, githubInstallationID)
	if err != nil {
		return domain.Installation{}, fmt.Errorf("read installation from github: %w", err)
	}

	accountType, err := domain.ParseAccountType(remote.AccountType)
	if err != nil {
		return domain.Installation{}, err
	}
	selection, err := domain.ParseRepositorySelection(remote.RepositorySelection)
	if err != nil {
		return domain.Installation{}, err
	}
	login, err := domain.ValidateAccountLogin(remote.AccountLogin)
	if err != nil {
		return domain.Installation{}, err
	}

	id, err := domain.NewInstallationID()
	if err != nil {
		return domain.Installation{}, err
	}

	installation := domain.Installation{
		ID:                  id,
		WorkspaceID:         state.WorkspaceID,
		GitHubID:            githubInstallationID,
		AccountLogin:        login,
		AccountType:         accountType,
		RepositorySelection: selection,
		ConnectedBy:         userID,
	}

	// The store re-checks the actor's permission inside the same transaction
	// as the insert, so a demotion between here and there is still caught.
	tenantCtx := s.bind(ctx, state.WorkspaceID)
	connected, err := s.installations.Connect(tenantCtx, installation,
		Actor{UserID: userID, Required: domain.PermissionRepositoryManage},
		AuditEvent{
			WorkspaceID: state.WorkspaceID,
			ActorUserID: userID,
			Action:      AuditInstallationConnected,
			Target:      fmt.Sprintf("%d", githubInstallationID),
			Detail: map[string]any{
				"account":              login,
				"account_type":         string(accountType),
				"repository_selection": string(selection),
			},
		})
	if errors.Is(err, domain.ErrInstallationBoundElsewhere) {
		// Not necessarily another tenant. The same flow is used to *change*
		// which repositories an existing installation shares, and GitHub sends
		// the browser back with the installation id it already has — so the
		// insert collides with the row we put there ourselves.
		//
		// Which of the two it is turns on the workspace, and the workspace
		// comes from the state we recorded before the redirect, never from the
		// callback. If they match, this is an update and the right response is
		// to reconcile. If they differ, it is the cross-tenant rebind the
		// unique index exists to refuse.
		existing, resolveErr := s.installations.Resolve(ctx, githubInstallationID)
		if resolveErr != nil || existing.WorkspaceID != state.WorkspaceID {
			return domain.Installation{}, domain.ErrInstallationBoundElsewhere
		}

		current, getErr := s.installations.Get(tenantCtx, existing.InstallationID, state.WorkspaceID)
		if getErr != nil {
			return domain.Installation{}, getErr
		}
		if reconcileErr := s.reconcile(tenantCtx, current, &remote); reconcileErr != nil {
			return domain.Installation{}, fmt.Errorf("reconcile after access change: %w", reconcileErr)
		}
		// Re-read rather than returning `current`. Reconciliation persists
		// GitHub's repository_selection, and the value read before it ran is
		// precisely the one the caller changed — returning it would serialise
		// the old selection back to a page that just updated it.
		updated, getErr := s.installations.Get(tenantCtx, existing.InstallationID, state.WorkspaceID)
		if getErr != nil {
			return domain.Installation{}, getErr
		}
		return updated, nil
	}
	if err != nil {
		return domain.Installation{}, err
	}

	// Reconcile immediately, so the workspace has its repository list before
	// anyone looks rather than waiting for a webhook that may be delayed or
	// never arrive.
	//
	// A failure here is deliberately not returned. The binding is the durable
	// fact and it succeeded; the repository list is derived, recoverable, and
	// corrected by the next reconciliation — which happens on the next health
	// check, the next webhook, or the next use. Failing the whole callback
	// would leave the installation connected on GitHub and absent here, which
	// is the one outcome that needs a human to untangle.
	_ = s.reconcile(tenantCtx, connected, &remote)
	return connected, nil
}

// reconcile pulls the current truth from GitHub and writes it over what we
// believed.
//
// `known` lets a caller that has just fetched the installation pass it in.
// That is not micro-optimisation: each GitHub round trip here is close to a
// second, and the installation callback happens inside a browser request with
// a client timeout on it. Fetching the same thing twice in one request was
// enough on its own to exceed that budget.
func (s *InstallationService) reconcile(ctx context.Context, installation domain.Installation, known *RemoteInstallation) error {
	var remote RemoteInstallation
	if known != nil {
		remote = *known
	} else {
		fetched, err := s.api.Installation(ctx, installation.GitHubID)
		if err != nil {
			return fmt.Errorf("read installation from github: %w", err)
		}
		remote = fetched
	}

	selection, err := domain.ParseRepositorySelection(remote.RepositorySelection)
	if err != nil {
		return err
	}

	remoteRepositories, err := s.api.InstallationRepositories(ctx, installation.GitHubID)
	if err != nil {
		return fmt.Errorf("list installation repositories: %w", err)
	}

	repositories := make([]domain.Repository, 0, len(remoteRepositories))
	for _, remote := range remoteRepositories {
		repositories = append(repositories, domain.Repository{
			GitHubID:      remote.GitHubID,
			Owner:         remote.Owner,
			Name:          remote.Name,
			DefaultBranch: remote.DefaultBranch,
			Private:       remote.Private,
		})
	}

	return s.installations.Reconcile(ctx, installation.ID, installation.WorkspaceID,
		selection, repositories, remote.Permissions)
}

// Reconcile refreshes one installation against GitHub.
func (s *InstallationService) Reconcile(ctx context.Context, installationID, workspaceID uuid.UUID) error {
	installation, err := s.installations.Get(ctx, installationID, workspaceID)
	if err != nil {
		return err
	}
	return s.reconcile(ctx, installation, nil)
}

// ListInstallations returns a workspace's installations.
func (s *InstallationService) ListInstallations(ctx context.Context, workspaceID uuid.UUID) ([]domain.Installation, error) {
	return s.installations.List(ctx, workspaceID)
}

// ListRepositories returns a workspace's repositories, withdrawn ones
// included so the interface can say access was removed rather than silently
// dropping something someone used yesterday.
func (s *InstallationService) ListRepositories(ctx context.Context, workspaceID uuid.UUID) ([]domain.Repository, error) {
	return s.installations.ListRepositories(ctx, workspaceID)
}

// InstallationHealth reports whether an installation can still do its job.
type InstallationHealth struct {
	InstallationID uuid.UUID
	AccountLogin   string
	Suspended      bool
	// MissingPermissions names what the installation does not hold, as
	// "permission:access". Naming them is the point: the only useful thing to
	// tell someone whose workspace stopped working is which permission to
	// approve.
	MissingPermissions  []string
	GrantedRepositories int
	Reachable           bool
	// Error is why GitHub could not be reached, when it could not be.
	Error string
}

// Health checks an installation against GitHub and against what it grants.
//
// Deliberately reports rather than fails: someone debugging a broken workspace
// needs to know which of the several possible things is wrong, and an error
// return would collapse them all into one.
func (s *InstallationService) Health(ctx context.Context, installationID, workspaceID uuid.UUID) (InstallationHealth, error) {
	installation, err := s.installations.Get(ctx, installationID, workspaceID)
	if err != nil {
		return InstallationHealth{}, err
	}

	health := InstallationHealth{
		InstallationID: installation.ID,
		AccountLogin:   installation.AccountLogin,
		Suspended:      installation.Suspended(),
	}

	remote, err := s.api.Installation(ctx, installation.GitHubID)
	if err != nil {
		health.Error = err.Error()
		// Fall back to what was recorded at connect time. Stale, and said to
		// be stale, beats saying nothing.
		if permissions, permErr := s.installations.ListPermissions(ctx, installation.ID, workspaceID); permErr == nil {
			health.MissingPermissions = domain.MissingPermissions(permissions)
		}
		return health, nil
	}

	health.Reachable = true
	health.Suspended = health.Suspended || remote.SuspendedAt != nil
	health.MissingPermissions = domain.MissingPermissions(remote.Permissions)

	repositories, err := s.installations.ListRepositories(ctx, workspaceID)
	if err != nil {
		return health, err
	}
	for _, repository := range repositories {
		if repository.Granted && repository.InstallationID == installation.ID {
			health.GrantedRepositories++
		}
	}
	return health, nil
}

// RepositoryForUse returns a repository only if the installation still grants
// it.
//
// The reconciliation half of "webhooks are not the only defence". Webhooks are
// missed, delayed and replayed, so they are not permitted to be the only thing
// standing between a revoked grant and a clone: anything that acts on a
// repository asks here, and this asks GitHub when the record says the grant is
// gone.
func (s *InstallationService) RepositoryForUse(ctx context.Context, repositoryID, workspaceID uuid.UUID) (domain.Repository, error) {
	repository, err := s.installations.GetRepository(ctx, repositoryID, workspaceID)
	if err != nil {
		return domain.Repository{}, err
	}

	installation, err := s.installations.Get(ctx, repository.InstallationID, workspaceID)
	if err != nil {
		return domain.Repository{}, err
	}
	if installation.Suspended() {
		return domain.Repository{}, domain.ErrInstallationSuspended
	}

	// Refresh before answering. The stored row is a belief, and this is the
	// moment where acting on a stale belief would reach a repository the
	// workspace no longer has.
	if err := s.reconcile(ctx, installation, nil); err != nil {
		return domain.Repository{}, fmt.Errorf("reconcile before use: %w", err)
	}

	refreshed, err := s.installations.GetRepository(ctx, repositoryID, workspaceID)
	if err != nil {
		return domain.Repository{}, err
	}
	if !refreshed.Granted {
		return domain.Repository{}, domain.ErrRepositoryNotGranted
	}
	return refreshed, nil
}

// webhookPayload is the part of a delivery this unit acts on.
//
// Deliberately a small struct rather than a map: a webhook body is
// attacker-shaped input once the signature is stripped away, and decoding only
// the fields that are used means a field nobody reads cannot influence
// anything. Every other key in the payload is ignored.
type webhookPayload struct {
	Action       string `json:"action"`
	Installation struct {
		ID          int64      `json:"id"`
		SuspendedAt *time.Time `json:"suspended_at"`
	} `json:"installation"`
}

// HandleWebhook applies a verified delivery.
//
// The signature must already have been checked by the caller. This function
// assumes the body is genuinely from GitHub, and nothing below re-establishes
// that.
//
// Almost everything here ends in a reconciliation rather than an incremental
// edit. A delivery says what changed, but acting on that directly means
// trusting that no delivery was ever missed, delayed or reordered — and all
// three happen. Asking GitHub what is true now is both simpler and correct
// under all three.
func (s *InstallationService) HandleWebhook(ctx context.Context, deliveryID, event string, body []byte) error {
	// Claim first. GitHub retries, and a retry of an effect that already
	// succeeded must not produce a second one — most of all for suspend and
	// removal.
	//
	// A claim is not the same as a completed delivery. One whose effect failed
	// is reclaimable, so the retry is processed rather than collapsed into the
	// record of an attempt that went nowhere.
	claim, err := s.installations.ClaimDelivery(ctx, deliveryID, event, actionOf(body))
	if err != nil {
		return fmt.Errorf("claim delivery: %w", err)
	}
	switch claim {
	case DeliveryAlreadyDone:
		// Already applied. Acknowledging is the whole point of deduplication.
		return nil
	case DeliveryInFlight:
		// Deliberately an error, so the caller answers non-2xx and GitHub
		// retries. Acknowledging would tell GitHub the delivery succeeded
		// while the only attempt at it is still running and may yet fail —
		// and GitHub does not retry what it believes succeeded.
		return ErrDeliveryInFlight
	}

	// The claim stays provisional until the effect is durable. Marking it on
	// the way out, rather than deleting it on failure, is what makes this
	// survive a cancelled request or a process that dies mid-delivery: nothing
	// has to run for the retry to be correct.
	applied := false
	defer func() {
		if applied {
			// A failure here only means the next retry does the work again,
			// and every effect below is idempotent.
			_ = s.installations.CompleteDelivery(ctx, deliveryID)
		}
	}()

	var payload webhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		// Malformed JSON that carried a valid signature is strange enough to
		// report, but not something a retry would fix, so the record stands.
		applied = true
		return nil
	}
	if payload.Installation.ID == 0 {
		// Events that name no installation are not ours to act on.
		applied = true
		return nil
	}

	ref, err := s.installations.Resolve(ctx, payload.Installation.ID)
	if err != nil {
		if errors.Is(err, ErrInstallationNotFound) {
			// Expected, and not an error. `installation.created` routinely
			// arrives before the browser completes the callback that binds it,
			// and an installation belonging to no workspace here is simply not
			// ours to act on. Failing would make GitHub retry forever, so the
			// record stands and the retry stays collapsed.
			applied = true
			return nil
		}
		return fmt.Errorf("resolve installation: %w", err)
	}

	ctx = s.bind(ctx, ref.WorkspaceID)

	switch event {
	case "installation":
		if err := s.handleInstallationEvent(ctx, ref, payload); err != nil {
			return err
		}
		applied = true
		return nil
	case "installation_repositories", "repository":
		// Which repositories changed does not matter: reconciliation replaces
		// the whole set, which also catches anything an earlier missed
		// delivery would have left stale.
		installation, err := s.installations.Get(ctx, ref.InstallationID, ref.WorkspaceID)
		if err != nil {
			return err
		}
		if err := s.reconcile(ctx, installation, nil); err != nil {
			return err
		}
		applied = true
		return nil
	default:
		// Unknown events are ignored deliberately rather than by accident. An
		// App's subscriptions can be widened in GitHub's settings without any
		// code change here, and the first symptom must not be a stream of
		// failing deliveries. Nothing was applied, but nothing needs to be, so
		// the record stands.
		applied = true
		return nil
	}
}

// PruneDeliveries drops delivery records past the retention window.
func (s *InstallationService) PruneDeliveries(ctx context.Context, retention time.Duration) (int64, error) {
	return s.installations.PruneDeliveries(ctx, retention)
}

// handleInstallationEvent applies the installation lifecycle.
func (s *InstallationService) handleInstallationEvent(ctx context.Context, ref InstallationRef, payload webhookPayload) error {
	switch payload.Action {
	case "deleted":
		return s.installations.MarkDeleted(ctx, ref.InstallationID, ref.WorkspaceID, AuditEvent{
			WorkspaceID: ref.WorkspaceID,
			Action:      AuditInstallationDeleted,
			Target:      fmt.Sprintf("%d", payload.Installation.ID),
		})

	case "suspend":
		at := payload.Installation.SuspendedAt
		if at == nil {
			// GitHub said suspended but sent no timestamp. Record the fact
			// rather than dropping it: what matters is that it is suspended.
			now := time.Now().UTC()
			at = &now
		}
		return s.installations.SetSuspended(ctx, ref.InstallationID, ref.WorkspaceID, at, AuditEvent{
			WorkspaceID: ref.WorkspaceID,
			Action:      AuditInstallationSuspended,
			Target:      fmt.Sprintf("%d", payload.Installation.ID),
		})

	case "unsuspend":
		// Clearing the flag restores the previous state, which is why the
		// repository rows are kept through a suspension rather than withdrawn.
		if err := s.installations.SetSuspended(ctx, ref.InstallationID, ref.WorkspaceID, nil, AuditEvent{
			WorkspaceID: ref.WorkspaceID,
			Action:      AuditInstallationUnsuspended,
			Target:      fmt.Sprintf("%d", payload.Installation.ID),
		}); err != nil {
			return err
		}
		installation, err := s.installations.Get(ctx, ref.InstallationID, ref.WorkspaceID)
		if err != nil {
			return err
		}
		// Things may have changed while it was suspended, and no events
		// arrived to say so.
		return s.reconcile(ctx, installation, nil)

	case "new_permissions_accepted":
		installation, err := s.installations.Get(ctx, ref.InstallationID, ref.WorkspaceID)
		if err != nil {
			return err
		}
		return s.reconcile(ctx, installation, nil)

	case "created":
		// Usually a no-op: this arrives while the browser is still being
		// redirected, so the installation is not bound yet and Resolve has
		// already returned early.
		//
		// When it does find one, it is the safety net for the opposite
		// ordering — the binding landed first and its reconciliation failed or
		// was cut short. Without this the workspace would sit with a
		// connection and no repositories until someone pressed refresh.
		installation, err := s.installations.Get(ctx, ref.InstallationID, ref.WorkspaceID)
		if err != nil {
			return err
		}
		return s.reconcile(ctx, installation, nil)

	default:
		return nil
	}
}

// actionOf reads just the action for the delivery record, before the payload
// is trusted enough to decode properly.
func actionOf(body []byte) string {
	var probe struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Action
}
