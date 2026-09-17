package workspaces

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

// RegisterSessions mounts the session routes.
//
// keys makes POST retryable. It is a separate collaborator rather than part of
// the service because idempotency is a property of the HTTP exchange — the
// status replayed, the body bytes, the header — and the service knows none of
// those.
func (h *Handler) RegisterSessions(
	mux *http.ServeMux,
	sessions *application.SessionService,
	keys application.IdempotencyRepository,
) {
	routes := &sessionRoutes{handler: h, service: sessions, keys: &idempotency{keys: keys}}

	mux.Handle("POST /v1/workspaces/{workspaceID}/sessions",
		h.requireMembership(http.HandlerFunc(routes.create)))
	mux.Handle("GET /v1/workspaces/{workspaceID}/sessions",
		h.requireMembership(http.HandlerFunc(routes.list)))
	mux.Handle("GET /v1/workspaces/{workspaceID}/sessions/{sessionID}",
		h.requireMembership(http.HandlerFunc(routes.get)))
	mux.Handle("GET /v1/workspaces/{workspaceID}/sessions/{sessionID}/transitions",
		h.requireMembership(http.HandlerFunc(routes.transitions)))
	mux.Handle("GET /v1/workspaces/{workspaceID}/sessions/{sessionID}/participants",
		h.requireMembership(http.HandlerFunc(routes.participants)))
}

type sessionRoutes struct {
	handler *Handler
	service *application.SessionService
	keys    *idempotency
}

type sessionRequest struct {
	TaskID         string `json:"task_id"`
	AgentVersionID string `json:"agent_version_id"`
	BaseBranch     string `json:"base_branch"`
}

// sessionResponse carries the branch intent as well as the session.
//
// `branch_name` is a branch that does **not exist yet**: it is what the
// workflow will create in M5, named now because the session id is the stable
// identity a deterministic name derives from. Returning it is useful — a
// caller can say where the work will land — and misreading it as a branch that
// exists would be easy, so the contract says so in as many words.
type sessionResponse struct {
	ID             string `json:"id"`
	TaskID         string `json:"task_id"`
	AgentVersionID string `json:"agent_version_id"`
	State          string `json:"state"`
	Version        int32  `json:"version"`
	RepositoryID   string `json:"repository_id"`
	BranchName     string `json:"branch_name"`
	BaseBranch     string `json:"base_branch,omitempty"`
	ContinuesID    string `json:"continues_id,omitempty"`
	// NextStates is served rather than reimplemented in the browser, for the
	// reason the permission list is: two copies of a rule disagree eventually,
	// and the transition table is the only place a state implies anything.
	NextStates []string  `json:"next_states"`
	CreatedBy  string    `json:"created_by"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type transitionResponse struct {
	ID              string `json:"id"`
	PreviousState   string `json:"previous_state,omitempty"`
	NextState       string `json:"next_state"`
	ObservedVersion int32  `json:"observed_version"`
	Reason          string `json:"reason,omitempty"`
	// ActorUserID is absent when the system moved the session. A timeout is
	// not attributable to a person, and naming one would be a false statement
	// in a trail that cannot be corrected by editing.
	ActorUserID string    `json:"actor_user_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

type participantResponse struct {
	UserID    string    `json:"user_id"`
	Capacity  string    `json:"capacity"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *sessionRoutes) create(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		s.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	var body sessionRequest
	if !s.handler.decode(ctx, w, r, &body) {
		return
	}

	taskID, err := uuid.Parse(body.TaskID)
	if err != nil {
		httpx.WriteError(ctx, w, http.StatusBadRequest, httpx.CodeInvalidRequest,
			"task_id must be a UUID.")
		return
	}
	agentVersionID, err := uuid.Parse(body.AgentVersionID)
	if err != nil {
		httpx.WriteError(ctx, w, http.StatusBadRequest, httpx.CodeInvalidRequest,
			"agent_version_id must be a UUID.")
		return
	}

	// Claimed after the identifiers are parsed, so a malformed request does
	// not burn a key, and after the body is decoded, because the fingerprint
	// is over what was asked for rather than the bytes it arrived in.
	//
	// base_branch is normalised before it is fingerprinted: the contract says
	// omitting it means the repository default, so a client that sends it
	// empty has not changed its request and its retry must not be refused.
	claim, ok := s.keys.claim(ctx, w, r, s.handler,
		domain.IdempotencyScope{
			WorkspaceID: membership.WorkspaceID,
			UserID:      membership.UserID,
			Endpoint:    "createSession",
		},
		[]string{taskID.String(), agentVersionID.String(), strings.TrimSpace(body.BaseBranch)})
	if !ok {
		return
	}

	if claim.completion != nil {
		// Render runs inside the store's transaction, so the stored answer and
		// the session it describes commit together.
		claim.completion.Render = func(created domain.Session) (int, []byte, error) {
			return renderJSON(http.StatusCreated, toSessionResponse(created))
		}
	}

	session, err := s.service.Create(ctx, membership, application.CreateSessionCommand{
		TaskID:         taskID,
		AgentVersionID: agentVersionID,
		BaseBranch:     body.BaseBranch,
		Completion:     claim.completion,
	})
	if err != nil {
		// The work failed, so the key goes back at once rather than refusing a
		// legitimate retry for the length of the lease.
		s.keys.release(ctx, claim.completion)
		s.handler.writeError(ctx, w, err, "create session")
		return
	}
	httpx.WriteJSON(ctx, w, http.StatusCreated, toSessionResponse(session))
}

func (s *sessionRoutes) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		s.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	sessionID, err := uuid.Parse(r.PathValue("sessionID"))
	if err != nil {
		s.handler.notFound(ctx, w)
		return
	}

	session, err := s.service.Get(ctx, membership, sessionID)
	if err != nil {
		s.handler.writeError(ctx, w, err, "get session")
		return
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, toSessionResponse(session))
}

func (s *sessionRoutes) list(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		s.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	sessions, err := s.service.List(ctx, membership)
	if err != nil {
		s.handler.writeError(ctx, w, err, "list sessions")
		return
	}

	items := make([]sessionResponse, 0, len(sessions))
	for _, session := range sessions {
		items = append(items, toSessionResponse(session))
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, map[string]any{"sessions": items})
}

func (s *sessionRoutes) transitions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		s.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	sessionID, err := uuid.Parse(r.PathValue("sessionID"))
	if err != nil {
		s.handler.notFound(ctx, w)
		return
	}

	transitions, err := s.service.ListTransitions(ctx, membership, sessionID)
	if err != nil {
		s.handler.writeError(ctx, w, err, "list session transitions")
		return
	}

	items := make([]transitionResponse, 0, len(transitions))
	for _, transition := range transitions {
		item := transitionResponse{
			ID:              transition.ID.String(),
			NextState:       string(transition.NextState),
			ObservedVersion: transition.ObservedVersion,
			Reason:          transition.Reason,
			CreatedAt:       transition.CreatedAt,
		}
		if transition.PreviousState != nil {
			item.PreviousState = string(*transition.PreviousState)
		}
		if transition.ActorUserID != nil {
			item.ActorUserID = transition.ActorUserID.String()
		}
		items = append(items, item)
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, map[string]any{"transitions": items})
}

func (s *sessionRoutes) participants(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	membership, ok := membershipFrom(ctx)
	if !ok {
		s.handler.serverError(ctx, w, "membership missing from context", errors.New("route not wrapped"))
		return
	}

	sessionID, err := uuid.Parse(r.PathValue("sessionID"))
	if err != nil {
		s.handler.notFound(ctx, w)
		return
	}

	participants, err := s.service.ListParticipants(ctx, membership, sessionID)
	if err != nil {
		s.handler.writeError(ctx, w, err, "list session participants")
		return
	}

	items := make([]participantResponse, 0, len(participants))
	for _, participant := range participants {
		items = append(items, participantResponse{
			UserID:    participant.UserID.String(),
			Capacity:  string(participant.Capacity),
			CreatedAt: participant.CreatedAt,
		})
	}
	httpx.WriteJSON(ctx, w, http.StatusOK, map[string]any{"participants": items})
}

func toSessionResponse(session domain.Session) sessionResponse {
	next := domain.NextStates(session.State)
	states := make([]string, 0, len(next))
	for _, state := range next {
		states = append(states, string(state))
	}

	response := sessionResponse{
		ID:             session.ID.String(),
		TaskID:         session.TaskID.String(),
		AgentVersionID: session.AgentVersionID.String(),
		State:          string(session.State),
		Version:        session.Version,
		RepositoryID:   session.RepositoryID.String(),
		BranchName:     session.BranchName,
		BaseBranch:     session.BaseBranch,
		NextStates:     states,
		CreatedBy:      session.CreatedBy.String(),
		CreatedAt:      session.CreatedAt,
		UpdatedAt:      session.UpdatedAt,
	}
	if session.ContinuesID != nil {
		response.ContinuesID = session.ContinuesID.String()
	}
	return response
}
