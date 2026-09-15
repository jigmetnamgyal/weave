package workspaces

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

// RegisterAgents mounts the agent-profile routes.
func (h *Handler) RegisterAgents(mux *http.ServeMux, agents *application.AgentService) {
	routes := &agentRoutes{handler: h, service: agents}

	mux.Handle("POST /v1/workspaces/{workspaceID}/agents",
		h.requireMembership(http.HandlerFunc(routes.create)))
	mux.Handle("GET /v1/workspaces/{workspaceID}/agents",
		h.requireMembership(http.HandlerFunc(routes.list)))
	mux.Handle("GET /v1/workspaces/{workspaceID}/agents/{agentID}",
		h.requireMembership(http.HandlerFunc(routes.get)))
	mux.Handle("POST /v1/workspaces/{workspaceID}/agents/{agentID}/versions",
		h.requireMembership(http.HandlerFunc(routes.addVersion)))
	mux.Handle("GET /v1/workspaces/{workspaceID}/agents/{agentID}/versions",
		h.requireMembership(http.HandlerFunc(routes.listVersions)))
}

type agentRoutes struct {
	handler *Handler
	service *application.AgentService
}

type createAgentRequest struct {
	Name         string          `json:"name"`
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	Capabilities []string        `json:"capabilities"`
	ToolPolicy   json.RawMessage `json:"tool_policy"`
}

type addVersionRequest struct {
	Provider     string          `json:"provider"`
	Model        string          `json:"model"`
	Capabilities []string        `json:"capabilities"`
	ToolPolicy   json.RawMessage `json:"tool_policy"`
}

type agentResponse struct {
	ID string `json:"id"`
	// CurrentVersionID is what a session will be pinned to. Absent only in the
	// window between the two writes that create an agent, which share a
	// transaction — so in practice never.
	CurrentVersionID string    `json:"current_version_id,omitempty"`
	Name             string    `json:"name"`
	CreatedBy        string    `json:"created_by"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type agentVersionResponse struct {
	ID       string `json:"id"`
	AgentID  string `json:"agent_id"`
	Version  int32  `json:"version"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	// Capabilities is what the provider supports. The interface derives its
	// controls from this rather than assuming: no feature may assume every
	// provider offers pause, structured tool calls or token accounting.
	Capabilities []string        `json:"capabilities"`
	ToolPolicy   json.RawMessage `json:"tool_policy"`
	CreatedBy    string          `json:"created_by"`
	CreatedAt    time.Time       `json:"created_at"`
}

func (a *agentRoutes) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		a.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	var body createAgentRequest
	if !a.handler.decode(ctx, w, r, &body) {
		return
	}

	agent, version, err := a.service.Create(ctx, membership, application.CreateAgentCommand{
		Name:         body.Name,
		Provider:     body.Provider,
		Model:        body.Model,
		Capabilities: body.Capabilities,
		ToolPolicy:   body.ToolPolicy,
	})
	if err != nil {
		a.handler.writeError(ctx, w, err, "create agent")
		return
	}

	httpx.WriteJSON(ctx, w, http.StatusCreated, map[string]any{
		"agent":   toAgentResponse(agent),
		"version": toAgentVersionResponse(version),
	})
}

func (a *agentRoutes) addVersion(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		a.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	agentID, err := uuid.Parse(r.PathValue("agentID"))
	if err != nil {
		a.handler.notFound(ctx, w)
		return
	}

	var body addVersionRequest
	if !a.handler.decode(ctx, w, r, &body) {
		return
	}

	version, err := a.service.AddVersion(ctx, membership, application.AddVersionCommand{
		AgentID:      agentID,
		Provider:     body.Provider,
		Model:        body.Model,
		Capabilities: body.Capabilities,
		ToolPolicy:   body.ToolPolicy,
	})
	if err != nil {
		a.handler.writeError(ctx, w, err, "add agent version")
		return
	}
	httpx.WriteJSON(ctx, w, http.StatusCreated, toAgentVersionResponse(version))
}

func (a *agentRoutes) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		a.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	agentID, err := uuid.Parse(r.PathValue("agentID"))
	if err != nil {
		a.handler.notFound(ctx, w)
		return
	}

	agent, err := a.service.Get(ctx, membership, agentID)
	if err != nil {
		a.handler.writeError(ctx, w, err, "get agent")
		return
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, toAgentResponse(agent))
}

func (a *agentRoutes) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		a.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	agents, err := a.service.List(ctx, membership)
	if err != nil {
		a.handler.writeError(ctx, w, err, "list agents")
		return
	}

	items := make([]agentResponse, 0, len(agents))
	for _, agent := range agents {
		items = append(items, toAgentResponse(agent))
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, map[string]any{"items": items})
}

func (a *agentRoutes) listVersions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		a.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	agentID, err := uuid.Parse(r.PathValue("agentID"))
	if err != nil {
		a.handler.notFound(ctx, w)
		return
	}

	versions, err := a.service.ListVersions(ctx, membership, agentID)
	if err != nil {
		a.handler.writeError(ctx, w, err, "list agent versions")
		return
	}

	items := make([]agentVersionResponse, 0, len(versions))
	for _, version := range versions {
		items = append(items, toAgentVersionResponse(version))
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, map[string]any{"items": items})
}

func toAgentResponse(agent domain.Agent) agentResponse {
	response := agentResponse{
		ID:        agent.ID.String(),
		Name:      agent.Name,
		CreatedBy: agent.CreatedBy.String(),
		CreatedAt: agent.CreatedAt,
		UpdatedAt: agent.UpdatedAt,
	}
	if agent.CurrentVersionID != nil {
		response.CurrentVersionID = agent.CurrentVersionID.String()
	}
	return response
}

func toAgentVersionResponse(version domain.AgentVersion) agentVersionResponse {
	policy := version.ToolPolicy
	if len(policy) == 0 {
		policy = []byte("{}")
	}
	return agentVersionResponse{
		ID:           version.ID.String(),
		AgentID:      version.AgentID.String(),
		Version:      version.Version,
		Provider:     string(version.Provider),
		Model:        version.Model,
		Capabilities: domain.CapabilityStrings(version.Capabilities),
		ToolPolicy:   policy,
		CreatedBy:    version.CreatedBy.String(),
		CreatedAt:    version.CreatedAt,
	}
}
