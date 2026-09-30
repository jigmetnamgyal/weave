package workspaces

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

// RegisterEgress mounts actual configuration operations; no network policy changes.
func (h *Handler) RegisterEgress(mux *http.ServeMux, service *application.EgressService, keys application.IdempotencyRepository) {
	routes := &egressRoutes{handler: h, service: service, keys: &idempotency{keys: keys}}
	mux.Handle("GET /v1/workspaces/{workspaceID}/egress-hosts", h.requireMembership(http.HandlerFunc(routes.list)))
	mux.Handle("POST /v1/workspaces/{workspaceID}/egress-hosts", h.requireMembership(http.HandlerFunc(routes.add)))
	mux.Handle("DELETE /v1/workspaces/{workspaceID}/egress-hosts/{egressHostID}", h.requireMembership(http.HandlerFunc(routes.remove)))
}

type egressRoutes struct {
	handler *Handler
	service *application.EgressService
	keys    *idempotency
}
type egressResponse struct {
	ID          uuid.UUID `json:"id"`
	WorkspaceID uuid.UUID `json:"workspace_id"`
	Hostname    string    `json:"hostname"`
	CreatedBy   uuid.UUID `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

func toEgressResponse(h domain.EgressHost) egressResponse {
	return egressResponse{h.ID, h.WorkspaceID, h.Hostname, h.CreatedBy, h.CreatedAt}
}

func (e *egressRoutes) membership(w http.ResponseWriter, r *http.Request) (domain.Membership, bool) {
	m, ok := membershipFrom(r.Context())
	if !ok {
		e.handler.serverError(r.Context(), w, "membership missing", errors.New("route not wrapped"))
		return m, false
	}
	if !m.Can(domain.PermissionWorkspaceManage) {
		e.handler.writeError(r.Context(), w, application.ErrPermissionDenied, "manage egress")
		return m, false
	}
	return m, true
}

func (e *egressRoutes) list(w http.ResponseWriter, r *http.Request) {
	m, ok := e.membership(w, r)
	if !ok {
		return
	}
	entries, err := e.service.List(r.Context(), m)
	if err != nil {
		e.writeError(r.Context(), w, err)
		return
	}
	items := make([]egressResponse, 0, len(entries))
	for _, entry := range entries {
		items = append(items, toEgressResponse(entry))
	}
	httpx.WriteJSON(r.Context(), w, 200, map[string]any{"items": items, "limit": domain.MaxWorkspaceEgressHosts})
}

func (e *egressRoutes) claim(w http.ResponseWriter, r *http.Request, m domain.Membership, endpoint string, fields []string) (claimed, bool) {
	return e.keys.claimChecked(r.Context(), w, r, e.handler, domain.IdempotencyScope{WorkspaceID: m.WorkspaceID, UserID: m.UserID, Endpoint: endpoint}, fields, func() error { return e.service.Authorize(r.Context(), m) })
}

func (e *egressRoutes) add(w http.ResponseWriter, r *http.Request) {
	m, ok := e.membership(w, r)
	if !ok {
		return
	}
	var body struct {
		Hostname string `json:"hostname"`
	}
	if !e.handler.decode(r.Context(), w, r, &body) {
		return
	}
	host, err := e.service.Validate(body.Hostname)
	if err != nil {
		e.writeError(r.Context(), w, err)
		return
	}
	claim, ok := e.claim(w, r, m, "addEgressHost", []string{host})
	if !ok || !claim.proceed {
		return
	}
	var completion *application.EgressCompletion
	if claim.completion != nil {
		completion = &application.EgressCompletion{Claim: *claim.completion, Render: func(h domain.EgressHost) (int, []byte, error) {
			return renderJSON(http.StatusCreated, toEgressResponse(h))
		}}
	}
	entry, err := e.service.Add(r.Context(), m, host, completion)
	if err != nil {
		e.keys.release(r.Context(), claim.completion)
		e.writeError(r.Context(), w, err)
		return
	}
	httpx.WriteJSON(r.Context(), w, http.StatusCreated, toEgressResponse(entry))
}

func (e *egressRoutes) remove(w http.ResponseWriter, r *http.Request) {
	m, ok := e.membership(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("egressHostID"))
	if err != nil {
		e.handler.notFound(r.Context(), w)
		return
	}
	claim, ok := e.claim(w, r, m, "removeEgressHost", []string{id.String()})
	if !ok || !claim.proceed {
		return
	}
	var completion *application.EgressCompletion
	if claim.completion != nil {
		completion = &application.EgressCompletion{Claim: *claim.completion, Render: func(domain.EgressHost) (int, []byte, error) { return http.StatusNoContent, []byte{}, nil }}
	}
	if err := e.service.Remove(r.Context(), m, id, completion); err != nil {
		e.keys.release(r.Context(), claim.completion)
		e.writeError(r.Context(), w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (e *egressRoutes) writeError(ctx context.Context, w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrInvalidEgressHost), errors.Is(err, domain.ErrReservedEgressHost):
		httpx.WriteError(ctx, w, 400, httpx.CodeInvalidRequest, err.Error())
	case errors.Is(err, application.ErrEgressHostExists):
		httpx.WriteError(ctx, w, 409, "egress_host_exists", "This hostname is already configured.")
	case errors.Is(err, application.ErrEgressHostLimit):
		httpx.WriteError(ctx, w, 409, "egress_host_limit", "At most 20 additional hostnames are allowed.")
	case errors.Is(err, application.ErrEgressHostNotFound):
		e.handler.notFound(ctx, w)
	default:
		e.handler.writeError(ctx, w, err, "egress configuration")
	}
}
