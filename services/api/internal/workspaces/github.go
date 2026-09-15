package workspaces

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	githubadapter "github.com/jigmetnamgyal/weave/internal/adapters/github"
	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/api/internal/auth"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

// maxWebhookBytes bounds a webhook body.
//
// GitHub's largest payloads are well under this. The bound exists because the
// body must be read in full before the signature can be checked — verification
// is over the raw bytes — so this is memory spent on input that has not been
// authenticated yet, and it must not be unbounded.
const maxWebhookBytes = 4 << 20

// RegisterGitHub mounts the GitHub installation routes.
func (h *Handler) RegisterGitHub(
	mux *http.ServeMux,
	installations *application.InstallationService,
	webhookSecret string,
) {
	gh := &githubRoutes{handler: h, service: installations, webhookSecret: webhookSecret}

	mux.Handle("POST /v1/workspaces/{workspaceID}/github/install",
		h.requireMembership(http.HandlerFunc(gh.beginInstall)))
	mux.Handle("GET /v1/workspaces/{workspaceID}/github/installations",
		h.requireMembership(http.HandlerFunc(gh.listInstallations)))
	mux.Handle("GET /v1/workspaces/{workspaceID}/github/installations/{installationID}/health",
		h.requireMembership(http.HandlerFunc(gh.health)))
	mux.Handle("POST /v1/workspaces/{workspaceID}/github/installations/{installationID}/reconcile",
		h.requireMembership(http.HandlerFunc(gh.reconcile)))
	mux.Handle("GET /v1/workspaces/{workspaceID}/repositories",
		h.requireMembership(http.HandlerFunc(gh.listRepositories)))
	mux.Handle("POST /v1/workspaces/{workspaceID}/repositories/{repositoryID}/branches",
		h.requireMembership(http.HandlerFunc(gh.createBranch)))

	// Authenticated but not workspace-scoped: which workspace this belongs to
	// comes from the state token, not from the caller. That is the whole point
	// of the state token — taking it from the request would let the caller
	// choose the workspace.
	mux.Handle("POST /v1/github/installations", http.HandlerFunc(gh.completeInstall))

}

// RegisterGitHubWebhook mounts the webhook endpoint on the public mux.
//
// Separate from RegisterGitHub, and mounted outside the authenticated subtree,
// because GitHub cannot present a session token. Its authentication is the
// HMAC signature over the body, checked before anything else happens.
//
// It is registered on the public mux by its full pattern, which takes
// precedence over the "/v1/" prefix the authentication middleware is mounted
// on — so this one route is reachable unauthenticated while everything else
// under /v1 stays protected by default.
func (h *Handler) RegisterGitHubWebhook(
	mux *http.ServeMux,
	installations *application.InstallationService,
	webhookSecret string,
) {
	gh := &githubRoutes{handler: h, service: installations, webhookSecret: webhookSecret}
	mux.Handle("POST /v1/github/webhook", http.HandlerFunc(gh.webhook))
}

type githubRoutes struct {
	handler       *Handler
	service       *application.InstallationService
	webhookSecret string
}

type installURLResponse struct {
	InstallURL string `json:"install_url"`
}

type installationResponse struct {
	ID                  string     `json:"id"`
	AccountLogin        string     `json:"account_login"`
	AccountType         string     `json:"account_type"`
	RepositorySelection string     `json:"repository_selection"`
	Suspended           bool       `json:"suspended"`
	SuspendedAt         *time.Time `json:"suspended_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
}

type repositoryResponse struct {
	ID string `json:"id"`
	// InstallationID lets the interface group repositories by the account they
	// came from. A workspace may connect several — a personal account and an
	// organisation, say — and presenting them as one flat list hides which
	// connection is failing when one of them is.
	InstallationID string `json:"installation_id"`
	Owner          string `json:"owner"`
	Name           string `json:"name"`
	FullName       string `json:"full_name"`
	DefaultBranch  string `json:"default_branch"`
	Private        bool   `json:"private"`
	// Granted is false for a repository the installation no longer reaches.
	// Reported rather than filtered out, so the interface can say access was
	// removed instead of silently losing something someone used yesterday.
	Granted bool `json:"granted"`
}

type healthResponse struct {
	InstallationID      string   `json:"installation_id"`
	AccountLogin        string   `json:"account_login"`
	Reachable           bool     `json:"reachable"`
	Suspended           bool     `json:"suspended"`
	MissingPermissions  []string `json:"missing_permissions"`
	GrantedRepositories int      `json:"granted_repositories"`
	Error               string   `json:"error,omitempty"`
}

func (g *githubRoutes) beginInstall(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		g.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	url, err := g.service.BeginInstall(ctx, membership)
	if err != nil {
		g.handler.writeError(ctx, w, err, "begin github install")
		return
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, installURLResponse{InstallURL: url})
}

type completeInstallRequest struct {
	State string `json:"state"`
	// InstallationID is a claim, not a fact. It is confirmed with GitHub
	// before anything is written.
	InstallationID int64 `json:"installation_id"`
}

func (g *githubRoutes) completeInstall(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	user, ok := auth.UserFrom(ctx)
	if !ok {
		httpx.WriteError(ctx, w, http.StatusUnauthorized, httpx.CodeUnauthenticated,
			"Authentication required. Present a valid bearer token.")
		return
	}

	var body completeInstallRequest
	if !g.handler.decode(ctx, w, r, &body) {
		return
	}
	if body.InstallationID <= 0 {
		httpx.WriteError(ctx, w, http.StatusBadRequest, httpx.CodeInvalidRequest,
			"An installation id is required.")
		return
	}

	installation, err := g.service.CompleteInstall(ctx, user.ID, body.State, body.InstallationID)
	if err != nil {
		g.handler.writeError(ctx, w, err, "complete github install")
		return
	}
	httpx.WriteJSON(ctx, w, http.StatusCreated, toInstallationResponse(installation))
}

func (g *githubRoutes) listInstallations(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		g.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	installations, err := g.service.ListInstallations(ctx, membership.WorkspaceID)
	if err != nil {
		g.handler.writeError(ctx, w, err, "list installations")
		return
	}

	response := make([]installationResponse, 0, len(installations))
	for _, installation := range installations {
		response = append(response, toInstallationResponse(installation))
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, map[string]any{"installations": response})
}

func (g *githubRoutes) listRepositories(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		g.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	repositories, err := g.service.ListRepositories(ctx, membership.WorkspaceID)
	if err != nil {
		g.handler.writeError(ctx, w, err, "list repositories")
		return
	}

	response := make([]repositoryResponse, 0, len(repositories))
	for _, repository := range repositories {
		response = append(response, repositoryResponse{
			ID:             repository.ID.String(),
			InstallationID: repository.InstallationID.String(),
			Owner:          repository.Owner,
			Name:           repository.Name,
			FullName:       repository.FullName(),
			DefaultBranch:  repository.DefaultBranch,
			Private:        repository.Private,
			Granted:        repository.Granted,
		})
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, map[string]any{"repositories": response})
}

func (g *githubRoutes) health(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		g.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	installationID, err := uuid.Parse(r.PathValue("installationID"))
	if err != nil {
		g.handler.notFound(ctx, w)
		return
	}

	health, err := g.service.Health(ctx, installationID, membership.WorkspaceID)
	if err != nil {
		g.handler.writeError(ctx, w, err, "installation health")
		return
	}

	missing := health.MissingPermissions
	if missing == nil {
		missing = []string{}
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, healthResponse{
		InstallationID:      health.InstallationID.String(),
		AccountLogin:        health.AccountLogin,
		Reachable:           health.Reachable,
		Suspended:           health.Suspended,
		MissingPermissions:  missing,
		GrantedRepositories: health.GrantedRepositories,
		Error:               health.Error,
	})
}

func (g *githubRoutes) reconcile(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		g.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}
	if !membership.Can(domain.PermissionRepositoryManage) {
		httpx.WriteError(ctx, w, http.StatusForbidden, httpx.CodePermissionDenied,
			"Your role cannot manage repositories in this workspace.")
		return
	}

	installationID, err := uuid.Parse(r.PathValue("installationID"))
	if err != nil {
		g.handler.notFound(ctx, w)
		return
	}

	if err := g.service.Reconcile(ctx, installationID, membership.WorkspaceID); err != nil {
		g.handler.writeError(ctx, w, err, "reconcile installation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// webhook receives a delivery from GitHub.
//
// The order of operations is the security of this endpoint, so it is worth
// naming: read the raw body, verify the signature over those exact bytes,
// deduplicate, and only then parse and act. Parsing before verifying would
// mean decoding attacker-controlled JSON on an unauthenticated request, and
// verifying a re-encoding of parsed input would not be verifying what was
// sent.
//
// Every rejection is a bare status with no body. A webhook sender is not a
// user, has no interface to show an error in, and telling an unauthenticated
// caller why their forgery failed only helps them forge better.
func (g *githubRoutes) webhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBytes))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if err := githubadapter.VerifySignature(g.webhookSecret, body, r.Header.Get(githubadapter.SignatureHeader)); err != nil {
		// Logged, not returned. Which of missing, malformed or mismatched it
		// was matters a great deal when debugging a misconfigured secret, and
		// not at all to the sender.
		g.handler.logger.WarnContext(ctx, "github webhook rejected",
			slog.String("reason", err.Error()),
			slog.String("delivery", r.Header.Get(githubadapter.DeliveryHeader)),
			slog.String("event", r.Header.Get(githubadapter.EventHeader)),
		)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	delivery := r.Header.Get(githubadapter.DeliveryHeader)
	event := r.Header.Get(githubadapter.EventHeader)
	if delivery == "" || event == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if err := g.service.HandleWebhook(ctx, delivery, event, body); err != nil {
		// Another attempt holds this delivery and has not finished. Not a
		// failure, and not something to alarm on — but it must not be
		// acknowledged either, because GitHub does not retry what it believes
		// succeeded, and the attempt in flight may yet fail.
		if errors.Is(err, application.ErrDeliveryInFlight) {
			g.handler.logger.InfoContext(ctx, "github webhook already in flight",
				slog.String("delivery", delivery),
				slog.String("event", event),
			)
			w.WriteHeader(http.StatusConflict)
			return
		}

		// 500 rather than 200: GitHub retries a failed delivery, and a retry
		// is exactly what a transient database failure deserves. Deduplication
		// makes the retry safe.
		g.handler.logger.ErrorContext(ctx, "github webhook failed",
			slog.String("delivery", delivery),
			slog.String("event", event),
			slog.String("error", err.Error()),
		)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type createBranchRequest struct {
	// Name is required. A caller that may retry needs the same request to name
	// the same branch — generating one here would make each attempt create
	// another, which is the opposite of the idempotency this endpoint claims.
	Name string `json:"name"`
	Base string `json:"base"`
}

type branchResponse struct {
	Name string `json:"name"`
	SHA  string `json:"sha"`
	Base string `json:"base"`
	// Created is false when the branch already existed at the requested base.
	// Reported rather than hidden: the caller asked for a branch at a commit
	// and has one, but "already there" and "just made" are different facts.
	Created bool `json:"created"`
}

func (g *githubRoutes) createBranch(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		g.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	repositoryID, err := uuid.Parse(r.PathValue("repositoryID"))
	if err != nil {
		g.handler.notFound(ctx, w)
		return
	}

	var body createBranchRequest
	if !g.handler.decode(ctx, w, r, &body) {
		return
	}

	branch, err := g.service.CreateBranch(ctx, membership, application.CreateBranchCommand{
		RepositoryID: repositoryID,
		Name:         body.Name,
		Base:         body.Base,
	})
	if err != nil {
		// The branch exists and its audit row does not. Those pull in opposite
		// directions, and an earlier revision resolved that by returning the
		// error — which this handler then turned into a bare 500, hiding the
		// name and SHA of a ref that had just been written to a customer's
		// repository. The caller could not identify what had been created.
		//
		// The caller gets what it asked for, because that is what happened.
		// The gap is an operator's problem, so it goes to the log with
		// everything needed to reconstruct the row, at error level.
		if errors.Is(err, application.ErrAuditNotRecorded) && branch.Name != "" {
			g.handler.logger.ErrorContext(ctx, "branch created but not audited",
				slog.String("workspace_id", membership.WorkspaceID.String()),
				slog.String("actor_user_id", membership.UserID.String()),
				slog.String("repository_id", repositoryID.String()),
				slog.String("branch", branch.Name),
				slog.String("sha", branch.SHA),
				slog.String("base", branch.Base),
				slog.String("error", err.Error()),
				slog.String("request_id", httpx.RequestID(ctx)),
			)
		} else {
			g.handler.writeError(ctx, w, err, "create branch")
			return
		}
	}

	// 201 only when something was made. A retry that found its own earlier
	// work gets 200: the resource exists either way, and the status is the
	// only place that distinction survives into the response.
	status := http.StatusOK
	if branch.Created {
		status = http.StatusCreated
	}
	httpx.WriteJSON(ctx, w, status, branchResponse{
		Name:    branch.Name,
		SHA:     branch.SHA,
		Base:    branch.Base,
		Created: branch.Created,
	})
}

func toInstallationResponse(installation domain.Installation) installationResponse {
	return installationResponse{
		ID:                  installation.ID.String(),
		AccountLogin:        installation.AccountLogin,
		AccountType:         string(installation.AccountType),
		RepositorySelection: string(installation.RepositorySelection),
		Suspended:           installation.Suspended(),
		SuspendedAt:         installation.SuspendedAt,
		CreatedAt:           installation.CreatedAt,
	}
}

// Compile-time assertion that the tenant binder main wires in matches what the
// service expects, so a signature change is caught here rather than at startup.
var _ application.TenantBinder = postgres.WithTenantWorkspace
