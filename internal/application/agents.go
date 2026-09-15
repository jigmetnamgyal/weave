package application

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// AgentRepository is the persistence port for agent profiles.
type AgentRepository interface {
	Create(ctx context.Context, agent domain.Agent, version domain.AgentVersion, actor Actor, event AuditEvent) (domain.Agent, domain.AgentVersion, error)
	AddVersion(ctx context.Context, version domain.AgentVersion, actor Actor, event AuditEvent) (domain.AgentVersion, error)
	Get(ctx context.Context, agentID, workspaceID uuid.UUID) (domain.Agent, error)
	List(ctx context.Context, workspaceID uuid.UUID) ([]domain.Agent, error)
	ListVersions(ctx context.Context, agentID, workspaceID uuid.UUID) ([]domain.AgentVersion, error)
	GetVersion(ctx context.Context, versionID, workspaceID uuid.UUID) (domain.AgentVersion, error)
}

// AgentService creates agent profiles and their versions.
type AgentService struct {
	agents AgentRepository
}

// NewAgentService wires the service.
func NewAgentService(agents AgentRepository) *AgentService {
	return &AgentService{agents: agents}
}

// agentPermission is the permission governing agent profiles.
//
// `workspace:manage`, not `session:create`, and the difference from tasks is
// deliberate. A version carries the tool policy, which decides what an agent
// is allowed to do inside a customer's repository — a security control, and
// configuration rather than day-to-day work. The matrix already restricts
// that class of change to owner and admin, so this reuses it rather than
// adding an `agent:manage` that would be granted to the same two roles.
//
// A developer may therefore run agents and not redefine what they may do,
// which is the separation worth having.
const agentPermission = domain.PermissionWorkspaceManage

// CreateAgentCommand is a request to create a profile and its first version.
type CreateAgentCommand struct {
	Name         string
	Provider     string
	Model        string
	Capabilities []string
	ToolPolicy   []byte
}

// Create records an agent and its first version.
func (s *AgentService) Create(
	ctx context.Context,
	membership domain.Membership,
	command CreateAgentCommand,
) (domain.Agent, domain.AgentVersion, error) {
	if !membership.Can(agentPermission) {
		return domain.Agent{}, domain.AgentVersion{}, fmt.Errorf("%w: %s requires %s",
			ErrPermissionDenied, membership.Role, agentPermission)
	}

	name, provider, model, capabilities, policy, err := s.validate(command.Name,
		command.Provider, command.Model, command.Capabilities, command.ToolPolicy)
	if err != nil {
		return domain.Agent{}, domain.AgentVersion{}, err
	}

	agentID, err := domain.NewAgentID()
	if err != nil {
		return domain.Agent{}, domain.AgentVersion{}, err
	}
	versionID, err := domain.NewAgentVersionID()
	if err != nil {
		return domain.Agent{}, domain.AgentVersion{}, err
	}

	return s.agents.Create(ctx,
		domain.Agent{
			ID:          agentID,
			WorkspaceID: membership.WorkspaceID,
			Name:        name,
			CreatedBy:   membership.UserID,
		},
		domain.AgentVersion{
			ID:           versionID,
			AgentID:      agentID,
			WorkspaceID:  membership.WorkspaceID,
			Version:      1,
			Provider:     provider,
			Model:        model,
			Capabilities: capabilities,
			ToolPolicy:   policy,
			CreatedBy:    membership.UserID,
		},
		Actor{UserID: membership.UserID, Required: agentPermission},
		AuditEvent{
			WorkspaceID: membership.WorkspaceID,
			ActorUserID: membership.UserID,
			Action:      AuditAgentCreated,
			Target:      agentID.String(),
			Detail: map[string]any{
				"name":     name,
				"provider": string(provider),
				"model":    model,
			},
		})
}

// AddVersionCommand is a request to record new settings for an agent.
type AddVersionCommand struct {
	AgentID      uuid.UUID
	Provider     string
	Model        string
	Capabilities []string
	ToolPolicy   []byte
}

// AddVersion records new settings and points the agent at them.
//
// This is what "editing a profile" means: the previous version is untouched,
// because a session pinned to it must keep describing what it actually ran
// under. Nothing in this path can modify an existing row — the table refuses
// it — so the only way to get it wrong is to not write a new one.
func (s *AgentService) AddVersion(
	ctx context.Context,
	membership domain.Membership,
	command AddVersionCommand,
) (domain.AgentVersion, error) {
	if !membership.Can(agentPermission) {
		return domain.AgentVersion{}, fmt.Errorf("%w: %s requires %s",
			ErrPermissionDenied, membership.Role, agentPermission)
	}

	if _, err := s.agents.Get(ctx, command.AgentID, membership.WorkspaceID); err != nil {
		return domain.AgentVersion{}, err
	}

	_, provider, model, capabilities, policy, err := s.validate("placeholder",
		command.Provider, command.Model, command.Capabilities, command.ToolPolicy)
	if err != nil {
		return domain.AgentVersion{}, err
	}

	versionID, err := domain.NewAgentVersionID()
	if err != nil {
		return domain.AgentVersion{}, err
	}

	return s.agents.AddVersion(ctx, domain.AgentVersion{
		ID:           versionID,
		AgentID:      command.AgentID,
		WorkspaceID:  membership.WorkspaceID,
		Provider:     provider,
		Model:        model,
		Capabilities: capabilities,
		ToolPolicy:   policy,
		CreatedBy:    membership.UserID,
	}, Actor{UserID: membership.UserID, Required: agentPermission},
		AuditEvent{
			WorkspaceID: membership.WorkspaceID,
			ActorUserID: membership.UserID,
			Action:      AuditAgentVersionCreated,
			Target:      command.AgentID.String(),
			Detail: map[string]any{
				"provider": string(provider),
				"model":    model,
			},
		})
}

// validate checks the fields a version carries.
//
// The name is validated alongside them because Create needs both and
// AddVersion needs all but the name; splitting them would put the same four
// checks in two places, which is how they drift.
func (s *AgentService) validate(
	name, provider, model string,
	capabilities []string,
	policy []byte,
) (string, domain.Provider, string, []domain.Capability, []byte, error) {
	validName, err := domain.ValidateAgentName(name)
	if err != nil {
		return "", "", "", nil, nil, err
	}
	validProvider, err := domain.ParseProvider(provider)
	if err != nil {
		return "", "", "", nil, nil, err
	}
	validModel, err := domain.ValidateModel(model)
	if err != nil {
		return "", "", "", nil, nil, err
	}
	validCapabilities, err := domain.ParseCapabilities(capabilities)
	if err != nil {
		return "", "", "", nil, nil, err
	}

	// An absent policy is an empty object rather than SQL NULL: the column is
	// NOT NULL, and "no policy yet" and "policy that permits nothing" should
	// not be the same value by accident.
	if len(policy) == 0 {
		policy = []byte("{}")
	}
	return validName, validProvider, validModel, validCapabilities, policy, nil
}

// Get returns one agent.
func (s *AgentService) Get(ctx context.Context, workspaceID, agentID uuid.UUID) (domain.Agent, error) {
	return s.agents.Get(ctx, agentID, workspaceID)
}

// List returns a workspace's agents.
func (s *AgentService) List(ctx context.Context, workspaceID uuid.UUID) ([]domain.Agent, error) {
	return s.agents.List(ctx, workspaceID)
}

// ListVersions returns an agent's versions, newest first.
func (s *AgentService) ListVersions(ctx context.Context, workspaceID, agentID uuid.UUID) ([]domain.AgentVersion, error) {
	if _, err := s.agents.Get(ctx, agentID, workspaceID); err != nil {
		return nil, err
	}
	return s.agents.ListVersions(ctx, agentID, workspaceID)
}
