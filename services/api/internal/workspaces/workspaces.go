// Package workspaces serves the workspace and membership endpoints.
//
// Authorization here has two distinct steps, and conflating them is the
// mistake this package is arranged to prevent:
//
//  1. Membership — may this caller see this workspace at all? Answered once,
//     by the middleware, and a non-member gets 404.
//  2. Permission — may this member do this particular thing? Answered per
//     handler, against the permission matrix, and a member without it gets
//     403.
//
// Membership alone authorizes nothing.
package workspaces

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/api/internal/auth"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

type contextKey int

const membershipKey contextKey = iota

// maxBodyBytes bounds request bodies. These endpoints carry a name and a
// version; anything larger is a mistake or an attempt to exhaust memory.
const maxBodyBytes = 16 << 10

// Handler serves the workspace routes.
type Handler struct {
	service *application.WorkspaceService
	logger  *slog.Logger
}

// NewHandler constructs the workspace handler.
func NewHandler(service *application.WorkspaceService, logger *slog.Logger) *Handler {
	return &Handler{service: service, logger: logger}
}

// Register mounts every workspace route on mux.
//
// Workspace-scoped routes are registered through `scoped`, which applies the
// membership middleware. Routes are therefore protected by how they are
// registered rather than by each handler remembering to check — the same
// arrangement the API uses for the whole /v1 subtree.
func (h *Handler) Register(mux *http.ServeMux) {
	scoped := func(pattern string, handler http.HandlerFunc) {
		mux.Handle(pattern, h.requireMembership(handler))
	}

	// Not workspace-scoped: creating one, and listing the caller's own.
	mux.Handle("POST /v1/workspaces", http.HandlerFunc(h.create))
	mux.Handle("GET /v1/workspaces", http.HandlerFunc(h.list))

	scoped("GET /v1/workspaces/{workspaceID}", h.get)
	scoped("PATCH /v1/workspaces/{workspaceID}", h.rename)
	scoped("GET /v1/workspaces/{workspaceID}/members", h.listMembers)
	scoped("PATCH /v1/workspaces/{workspaceID}/members/{userID}", h.changeMemberRole)
	scoped("DELETE /v1/workspaces/{workspaceID}/members/{userID}", h.removeMember)
}

// requireMembership resolves the workspace from the path and loads the
// caller's membership, rejecting a non-member with 404.
//
// 404 rather than 403 is deliberate. A 403 confirms the workspace exists,
// which turns identifier guessing into tenant enumeration. To anyone outside
// a workspace it must be indistinguishable from one that was never created.
func (h *Handler) requireMembership(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		user, ok := auth.UserFrom(ctx)
		if !ok {
			httpx.WriteError(ctx, w, http.StatusUnauthorized, httpx.CodeUnauthenticated,
				"Authentication required. Present a valid bearer token.")
			return
		}

		workspaceID, err := uuid.Parse(r.PathValue("workspaceID"))
		if err != nil {
			// A malformed identifier is answered the same way as one that is
			// well formed but not the caller's, so the shape of a valid
			// identifier reveals nothing.
			h.notFound(ctx, w)
			return
		}

		membership, err := h.service.Membership(ctx, workspaceID, user.ID)
		if err != nil {
			if errors.Is(err, application.ErrMemberNotFound) || errors.Is(err, application.ErrWorkspaceNotFound) {
				h.notFound(ctx, w)
				return
			}
			h.serverError(ctx, w, "load membership", err)
			return
		}

		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, membershipKey, membership)))
	})
}

// membershipFrom returns the membership the middleware established.
func membershipFrom(ctx context.Context) (domain.Membership, bool) {
	membership, ok := ctx.Value(membershipKey).(domain.Membership)
	return membership, ok
}

// --- Handlers ---------------------------------------------------------------

type createWorkspaceRequest struct {
	Name string `json:"name"`
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	user, ok := auth.UserFrom(ctx)
	if !ok {
		httpx.WriteError(ctx, w, http.StatusUnauthorized, httpx.CodeUnauthenticated,
			"Authentication required. Present a valid bearer token.")
		return
	}

	var body createWorkspaceRequest
	if !h.decode(ctx, w, r, &body) {
		return
	}

	workspace, err := h.service.Create(ctx, user.ID, body.Name)
	if err != nil {
		h.writeError(ctx, w, err, "create workspace")
		return
	}

	// The creator is the owner, by construction.
	httpx.WriteJSON(ctx, w, http.StatusCreated, toWorkspaceResponse(workspace, domain.RoleOwner))
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	user, ok := auth.UserFrom(ctx)
	if !ok {
		httpx.WriteError(ctx, w, http.StatusUnauthorized, httpx.CodeUnauthenticated,
			"Authentication required. Present a valid bearer token.")
		return
	}

	memberships, err := h.service.List(ctx, user.ID)
	if err != nil {
		h.writeError(ctx, w, err, "list workspaces")
		return
	}

	items := make([]workspaceResponse, 0, len(memberships))
	for _, m := range memberships {
		items = append(items, toWorkspaceResponse(m.Workspace, m.Role))
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, listResponse[workspaceResponse]{Items: items})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		h.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	workspace, err := h.service.Get(ctx, membership.WorkspaceID, membership.UserID)
	if err != nil {
		h.writeError(ctx, w, err, "get workspace")
		return
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, toWorkspaceResponse(workspace, membership.Role))
}

type renameWorkspaceRequest struct {
	Name string `json:"name"`
	// Version is the value the caller last read. Required: without it a rename
	// would silently overwrite a concurrent edit.
	Version *int `json:"version"`
}

func (h *Handler) rename(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		h.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	var body renameWorkspaceRequest
	if !h.decode(ctx, w, r, &body) {
		return
	}
	if body.Version == nil {
		httpx.WriteError(ctx, w, http.StatusBadRequest, httpx.CodeInvalidRequest,
			"A version is required. Send the version returned by the last read.")
		return
	}

	workspace, err := h.service.Rename(ctx, membership, *body.Version, body.Name)
	if err != nil {
		h.writeError(ctx, w, err, "rename workspace")
		return
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, toWorkspaceResponse(workspace, membership.Role))
}

func (h *Handler) listMembers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		h.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	members, err := h.service.ListMembers(ctx, membership)
	if err != nil {
		h.writeError(ctx, w, err, "list members")
		return
	}

	items := make([]memberResponse, 0, len(members))
	for _, member := range members {
		items = append(items, toMemberResponse(member))
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, listResponse[memberResponse]{Items: items})
}

type changeRoleRequest struct {
	Role string `json:"role"`
}

func (h *Handler) changeMemberRole(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		h.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	targetID, err := uuid.Parse(r.PathValue("userID"))
	if err != nil {
		h.notFound(ctx, w)
		return
	}

	var body changeRoleRequest
	if !h.decode(ctx, w, r, &body) {
		return
	}

	role, err := domain.ParseRole(body.Role)
	if err != nil {
		h.writeError(ctx, w, err, "parse role")
		return
	}

	updated, err := h.service.ChangeMemberRole(ctx, membership, targetID, role)
	if err != nil {
		h.writeError(ctx, w, err, "change member role")
		return
	}

	httpx.WriteJSON(ctx, w, http.StatusOK, membershipResponse{
		UserID: updated.UserID.String(),
		Role:   updated.Role.String(),
	})
}

func (h *Handler) removeMember(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		h.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	targetID, err := uuid.Parse(r.PathValue("userID"))
	if err != nil {
		h.notFound(ctx, w)
		return
	}

	if err := h.service.RemoveMember(ctx, membership, targetID); err != nil {
		h.writeError(ctx, w, err, "remove member")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Responses --------------------------------------------------------------

type listResponse[T any] struct {
	Items []T `json:"items"`
}

type workspaceResponse struct {
	ID   string `json:"id"`
	Slug string `json:"slug"`
	Name string `json:"name"`
	// Version must be echoed back on the next update.
	Version   int       `json:"version"`
	Role      string    `json:"role"`
	CreatedAt time.Time `json:"created_at"`
	// Permissions lets a client hide what the caller cannot do, without
	// reimplementing the matrix. The server still enforces it.
	Permissions []string `json:"permissions"`
}

type memberResponse struct {
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
	Role        string `json:"role"`
}

type membershipResponse struct {
	UserID string `json:"user_id"`
	Role   string `json:"role"`
}

func toWorkspaceResponse(workspace domain.Workspace, role domain.Role) workspaceResponse {
	held := domain.PermissionsFor(role)
	permissions := make([]string, 0, len(held))
	for _, permission := range held {
		permissions = append(permissions, string(permission))
	}

	return workspaceResponse{
		ID:          workspace.ID.String(),
		Slug:        workspace.Slug,
		Name:        workspace.Name,
		Version:     workspace.Version,
		Role:        role.String(),
		CreatedAt:   workspace.CreatedAt,
		Permissions: permissions,
	}
}

func toMemberResponse(member domain.MemberProfile) memberResponse {
	return memberResponse{
		UserID:      member.UserID.String(),
		Email:       member.Email,
		DisplayName: member.DisplayName,
		AvatarURL:   member.AvatarURL,
		Role:        member.Role.String(),
	}
}

// --- Plumbing ---------------------------------------------------------------

// decode reads a bounded JSON body, reporting failure to the client itself.
//
// MaxBytesReader rather than io.LimitReader: the latter presents the cutoff as
// a clean EOF, so an oversized body silently decodes whatever fitted instead of
// being rejected. The trailing-token check then rejects a second JSON value
// after the first, which Decode alone would ignore.
func (h *Handler) decode(ctx context.Context, w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(target); err != nil {
		httpx.WriteError(ctx, w, http.StatusBadRequest, httpx.CodeInvalidRequest,
			"The request body could not be read as JSON.")
		return false
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httpx.WriteError(ctx, w, http.StatusBadRequest, httpx.CodeInvalidRequest,
			"The request body must contain exactly one JSON value.")
		return false
	}
	return true
}

func (h *Handler) notFound(ctx context.Context, w http.ResponseWriter) {
	httpx.WriteError(ctx, w, http.StatusNotFound, httpx.CodeNotFound, "Workspace not found.")
}

func (h *Handler) serverError(ctx context.Context, w http.ResponseWriter, operation string, err error) {
	h.logger.ErrorContext(ctx, "workspace request failed",
		slog.String("operation", operation),
		slog.String("error", err.Error()),
		slog.String("request_id", httpx.RequestID(ctx)),
	)
	httpx.WriteError(ctx, w, http.StatusInternalServerError, httpx.CodeInternal,
		"Something went wrong. The request was not completed.")
}

// writeError maps a domain or application error onto the HTTP response.
//
// Anything unrecognised becomes a 500 and is logged, rather than leaking an
// internal message to the caller.
func (h *Handler) writeError(ctx context.Context, w http.ResponseWriter, err error, operation string) {
	switch {
	case errors.Is(err, application.ErrWorkspaceNotFound), errors.Is(err, application.ErrMemberNotFound):
		h.notFound(ctx, w)

	case errors.Is(err, application.ErrPermissionDenied):
		httpx.WriteError(ctx, w, http.StatusForbidden, httpx.CodePermissionDenied,
			"Your role does not allow this action.")

	case errors.Is(err, application.ErrVersionConflict):
		httpx.WriteError(ctx, w, http.StatusConflict, httpx.CodeConflict,
			"The workspace changed since you last read it. Reload and try again.")

	case errors.Is(err, domain.ErrInvitationNotUsable):
		// One response for unknown, expired, revoked and already-accepted.
		// Distinguishing them would tell someone probing tokens which guesses
		// were once real.
		httpx.WriteError(ctx, w, http.StatusNotFound, httpx.CodeNotFound,
			"This invitation link is not valid. It may have expired or been withdrawn.")

	case errors.Is(err, domain.ErrInvitationWrongRecipient):
		httpx.WriteError(ctx, w, http.StatusForbidden, httpx.CodePermissionDenied,
			"This invitation was sent to a different email address. Sign in with that address to accept it.")

	case errors.Is(err, application.ErrInvitationNotFound):
		httpx.WriteError(ctx, w, http.StatusNotFound, httpx.CodeNotFound, "Invitation not found.")

	case errors.Is(err, application.ErrInvitationOutstanding):
		httpx.WriteError(ctx, w, http.StatusConflict, httpx.CodeConflict,
			"An invitation to that address is already outstanding. Revoke it first to issue a new one.")

	case errors.Is(err, domain.ErrInvalidInvitation):
		httpx.WriteError(ctx, w, http.StatusBadRequest, httpx.CodeInvalidRequest, err.Error())

	case errors.Is(err, domain.ErrCannotGrantRole):
		httpx.WriteError(ctx, w, http.StatusForbidden, httpx.CodePermissionDenied,
			"You cannot grant a role with more authority than your own.")

	case errors.Is(err, domain.ErrLastOwner):
		httpx.WriteError(ctx, w, http.StatusConflict, httpx.CodeConflict,
			"A workspace must keep at least one owner. Promote another member first.")

	case errors.Is(err, application.ErrAlreadyMember):
		httpx.WriteError(ctx, w, http.StatusConflict, httpx.CodeConflict,
			"That user is already a member of this workspace.")

	case errors.Is(err, domain.ErrInvalidWorkspace):
		// Validation messages describe the caller's own input, so they are
		// safe to return.
		httpx.WriteError(ctx, w, http.StatusBadRequest, httpx.CodeInvalidRequest, err.Error())

	default:
		h.serverError(ctx, w, operation, err)
	}
}
