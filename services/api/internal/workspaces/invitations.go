package workspaces

import (
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/api/internal/auth"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

// RegisterInvitations mounts the invitation routes.
//
// The workspace-scoped routes go through the same membership middleware as
// every other workspace route, so a non-member sees 404. The accept and
// preview routes deliberately do not: their caller is not a member yet, and
// the token is what authorizes them.
func (h *Handler) RegisterInvitations(mux *http.ServeMux, invitations *application.InvitationService) {
	inv := &invitationRoutes{handler: h, service: invitations}

	mux.Handle("POST /v1/workspaces/{workspaceID}/invitations", h.requireMembership(http.HandlerFunc(inv.issue)))
	mux.Handle("GET /v1/workspaces/{workspaceID}/invitations", h.requireMembership(http.HandlerFunc(inv.list)))
	mux.Handle("DELETE /v1/workspaces/{workspaceID}/invitations/{invitationID}", h.requireMembership(http.HandlerFunc(inv.revoke)))

	// Authenticated, but not workspace-scoped.
	mux.Handle("POST /v1/invitations/preview", http.HandlerFunc(inv.preview))
	mux.Handle("POST /v1/invitations/accept", http.HandlerFunc(inv.accept))
}

// invitationRoutes carries the shared dependencies for the routes above.
type invitationRoutes struct {
	handler *Handler
	service *application.InvitationService
}

type issueInvitationRequest struct {
	Email string `json:"email"`
	Role  string `json:"role"`
}

// issuedInvitationResponse carries the token. This is the only response that
// ever does, and the only moment it exists outside the issuer's browser.
type issuedInvitationResponse struct {
	invitationResponse
	// Token is shown once and is not recoverable afterwards; only its hash is
	// stored.
	Token string `json:"token"`
}

type invitationResponse struct {
	ID                   string    `json:"id"`
	Email                string    `json:"email"`
	Role                 string    `json:"role"`
	Status               string    `json:"status"`
	InvitedByEmail       string    `json:"invited_by_email,omitempty"`
	InvitedByDisplayName string    `json:"invited_by_display_name,omitempty"`
	ExpiresAt            time.Time `json:"expires_at"`
	CreatedAt            time.Time `json:"created_at"`
}

func (i *invitationRoutes) issue(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		i.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	var body issueInvitationRequest
	if !i.handler.decode(ctx, w, r, &body) {
		return
	}

	role, err := domain.ParseRole(body.Role)
	if err != nil {
		i.handler.writeError(ctx, w, err, "parse role")
		return
	}

	issued, err := i.service.Issue(ctx, membership, body.Email, role)
	if err != nil {
		i.handler.writeError(ctx, w, err, "issue invitation")
		return
	}

	httpx.WriteJSON(ctx, w, http.StatusCreated, issuedInvitationResponse{
		invitationResponse: toInvitationResponse(issued.Invitation, issued.Status, "", ""),
		Token:              issued.Token,
	})
}

func (i *invitationRoutes) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		i.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	// Absent means every status; anything unrecognised is rejected rather
	// than silently ignored, which would return the whole list under a filter
	// the caller thinks is applied.
	var status domain.InvitationStatus
	if raw := r.URL.Query().Get("status"); raw != "" {
		parsed, err := domain.ParseInvitationStatus(raw)
		if err != nil {
			i.handler.writeError(ctx, w, err, "parse status filter")
			return
		}
		status = parsed
	}

	records, err := i.service.List(ctx, membership, status)
	if err != nil {
		i.handler.writeError(ctx, w, err, "list invitations")
		return
	}

	items := make([]invitationResponse, 0, len(records))
	for _, record := range records {
		items = append(items, toInvitationResponse(
			record.Invitation, record.Status, record.InvitedByEmail, record.InvitedByDisplayName))
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, listResponse[invitationResponse]{Items: items})
}

func (i *invitationRoutes) revoke(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		i.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	invitationID, err := uuid.Parse(r.PathValue("invitationID"))
	if err != nil {
		i.handler.notFound(ctx, w)
		return
	}

	if err := i.service.Revoke(ctx, membership, invitationID); err != nil {
		i.handler.writeError(ctx, w, err, "revoke invitation")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type tokenRequest struct {
	Token string `json:"token"`
}

// invitationPreviewResponse carries no invited address — see
// application.InvitationPreview for why.
type invitationPreviewResponse struct {
	WorkspaceName        string    `json:"workspace_name"`
	Role                 string    `json:"role"`
	InvitedByEmail       string    `json:"invited_by_email"`
	InvitedByDisplayName string    `json:"invited_by_display_name,omitempty"`
	ExpiresAt            time.Time `json:"expires_at"`
}

// preview is POST rather than GET because the token is in the body. A token in
// a query string ends up in server logs, proxy logs and Referer headers.
func (i *invitationRoutes) preview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if _, ok := auth.UserFrom(ctx); !ok {
		httpx.WriteError(ctx, w, http.StatusUnauthorized, httpx.CodeUnauthenticated,
			"Authentication required. Present a valid bearer token.")
		return
	}

	var body tokenRequest
	if !i.handler.decode(ctx, w, r, &body) {
		return
	}

	preview, err := i.service.Preview(ctx, body.Token)
	if err != nil {
		i.handler.writeError(ctx, w, err, "preview invitation")
		return
	}

	httpx.WriteJSON(ctx, w, http.StatusOK, invitationPreviewResponse{
		WorkspaceName:        preview.WorkspaceName,
		Role:                 preview.Role.String(),
		InvitedByEmail:       preview.InvitedByEmail,
		InvitedByDisplayName: preview.InvitedByDisplayName,
		ExpiresAt:            preview.ExpiresAt,
	})
}

type acceptedInvitationResponse struct {
	WorkspaceID string `json:"workspace_id"`
	Role        string `json:"role"`
}

func (i *invitationRoutes) accept(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	user, ok := auth.UserFrom(ctx)
	if !ok {
		httpx.WriteError(ctx, w, http.StatusUnauthorized, httpx.CodeUnauthenticated,
			"Authentication required. Present a valid bearer token.")
		return
	}

	var body tokenRequest
	if !i.handler.decode(ctx, w, r, &body) {
		return
	}

	membership, err := i.service.Accept(ctx, user, body.Token)
	if err != nil {
		i.handler.writeError(ctx, w, err, "accept invitation")
		return
	}

	httpx.WriteJSON(ctx, w, http.StatusOK, acceptedInvitationResponse{
		WorkspaceID: membership.WorkspaceID.String(),
		Role:        membership.Role.String(),
	})
}

// toInvitationResponse converts an invitation for the wire.
//
// The status is supplied rather than computed. Asking the clock here would
// answer a slightly later question than the one the list was filtered by, so
// a response to `?status=pending` could contain an item labelled `expired`.
//
// It carries no token and no hash: the token exists on the wire exactly once,
// in the response to issuing.
func toInvitationResponse(invitation domain.Invitation, status domain.InvitationStatus, invitedByEmail, invitedByDisplayName string) invitationResponse {
	return invitationResponse{
		ID:                   invitation.ID.String(),
		Email:                invitation.Email,
		Role:                 invitation.Role.String(),
		Status:               string(status),
		InvitedByEmail:       invitedByEmail,
		InvitedByDisplayName: invitedByDisplayName,
		ExpiresAt:            invitation.ExpiresAt,
		CreatedAt:            invitation.CreatedAt,
	}
}
