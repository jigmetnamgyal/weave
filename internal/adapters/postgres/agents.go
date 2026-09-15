package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// toolPolicyOrEmpty renders an absent policy as an empty object.
//
// The column is NOT NULL, so a nil slice becomes a constraint violation whose
// message says nothing about what the caller did wrong. More importantly,
// "no policy recorded yet" and "a policy that permits nothing" should be the
// same stored value rather than differing by a NULL nobody reads as either.
//
// The service defaults this too; doing it here as well means no caller can get
// it wrong, and this is the last place before the constraint.
func toolPolicyOrEmpty(policy []byte) []byte {
	if len(policy) == 0 {
		return []byte("{}")
	}
	return policy
}

// AgentStore persists agent profiles and their versions.
type AgentStore struct {
	pool *pgxpool.Pool
}

// NewAgentStore returns a store backed by pool.
func NewAgentStore(pool *pgxpool.Pool) *AgentStore {
	return &AgentStore{pool: pool}
}

// inTx runs fn inside a transaction carrying the caller's tenant context.
func (s *AgentStore) inTx(ctx context.Context, fn func(*postgresdb.Queries) error) error {
	return inTenantTx(ctx, s.pool, fn)
}

// Create records an agent and its first version together.
//
// One transaction, because an agent with no version is a profile that cannot
// be used and that nothing would later notice: the pointer would be null, the
// UI would offer it, and a session would fail at the point of running. Either
// both rows exist or neither does.
func (s *AgentStore) Create(
	ctx context.Context,
	agent domain.Agent,
	version domain.AgentVersion,
	actor application.Actor,
	event application.AuditEvent,
) (domain.Agent, domain.AgentVersion, error) {
	var (
		createdAgent   domain.Agent
		createdVersion domain.AgentVersion
	)

	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		if err := authorizeActor(ctx, q, agent.WorkspaceID, actor); err != nil {
			return err
		}

		agentRow, err := q.CreateAgent(ctx, postgresdb.CreateAgentParams{
			ID:          agent.ID,
			WorkspaceID: agent.WorkspaceID,
			Name:        agent.Name,
			CreatedBy:   agent.CreatedBy,
		})
		if err != nil {
			return translateAgentError(err)
		}

		versionRow, err := q.CreateAgentVersion(ctx, postgresdb.CreateAgentVersionParams{
			ID:           version.ID,
			AgentID:      agentRow.ID,
			WorkspaceID:  agent.WorkspaceID,
			Version:      1,
			Provider:     string(version.Provider),
			Model:        version.Model,
			Capabilities: domain.CapabilityStrings(version.Capabilities),
			ToolPolicy:   toolPolicyOrEmpty(version.ToolPolicy),
			CreatedBy:    version.CreatedBy,
		})
		if err != nil {
			return translateAgentError(err)
		}

		pointed, err := q.SetAgentCurrentVersion(ctx, postgresdb.SetAgentCurrentVersionParams{
			ID:               agentRow.ID,
			WorkspaceID:      agent.WorkspaceID,
			CurrentVersionID: nullableUUID(&versionRow.ID),
		})
		if err != nil {
			return fmt.Errorf("point agent at its first version: %w", err)
		}

		createdAgent = agentToDomain(pointed)
		createdVersion = agentVersionToDomain(versionRow)
		return appendAudit(ctx, q, event)
	})
	return createdAgent, createdVersion, err
}

// AddVersion writes a new version and moves the agent's pointer to it.
//
// The agent row is locked before the next version number is computed. Without
// the lock two concurrent edits both read the same maximum and one fails on
// the unique index — an error whose only honest message is "try again", which
// is not something to hand a person.
func (s *AgentStore) AddVersion(
	ctx context.Context,
	version domain.AgentVersion,
	actor application.Actor,
	event application.AuditEvent,
) (domain.AgentVersion, error) {
	var created domain.AgentVersion

	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		if err := authorizeActor(ctx, q, version.WorkspaceID, actor); err != nil {
			return err
		}

		if _, err := q.LockAgentForUpdate(ctx, postgresdb.LockAgentForUpdateParams{
			ID:          version.AgentID,
			WorkspaceID: version.WorkspaceID,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrAgentNotFound
			}
			return fmt.Errorf("lock agent: %w", err)
		}

		next, err := q.NextAgentVersionNumber(ctx, postgresdb.NextAgentVersionNumberParams{
			AgentID:     version.AgentID,
			WorkspaceID: version.WorkspaceID,
		})
		if err != nil {
			return fmt.Errorf("read next version number: %w", err)
		}

		row, err := q.CreateAgentVersion(ctx, postgresdb.CreateAgentVersionParams{
			ID:           version.ID,
			AgentID:      version.AgentID,
			WorkspaceID:  version.WorkspaceID,
			Version:      next,
			Provider:     string(version.Provider),
			Model:        version.Model,
			Capabilities: domain.CapabilityStrings(version.Capabilities),
			ToolPolicy:   toolPolicyOrEmpty(version.ToolPolicy),
			CreatedBy:    version.CreatedBy,
		})
		if err != nil {
			return translateAgentError(err)
		}

		if _, err := q.SetAgentCurrentVersion(ctx, postgresdb.SetAgentCurrentVersionParams{
			ID:               version.AgentID,
			WorkspaceID:      version.WorkspaceID,
			CurrentVersionID: nullableUUID(&row.ID),
		}); err != nil {
			return fmt.Errorf("move agent pointer: %w", err)
		}

		created = agentVersionToDomain(row)
		return appendAudit(ctx, q, event)
	})
	return created, err
}

// Get returns one agent, scoped to the workspace in context.
func (s *AgentStore) Get(ctx context.Context, agentID, workspaceID uuid.UUID) (domain.Agent, error) {
	var agent domain.Agent
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		row, err := q.GetAgentForWorkspace(ctx, postgresdb.GetAgentForWorkspaceParams{
			ID:          agentID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrAgentNotFound
			}
			return fmt.Errorf("select agent: %w", err)
		}
		agent = agentToDomain(row)
		return nil
	})
	return agent, err
}

// List returns a workspace's agents.
func (s *AgentStore) List(ctx context.Context, workspaceID uuid.UUID) ([]domain.Agent, error) {
	var agents []domain.Agent
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		rows, err := q.ListAgentsForWorkspace(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("list agents: %w", err)
		}
		agents = make([]domain.Agent, 0, len(rows))
		for _, row := range rows {
			agents = append(agents, agentToDomain(row))
		}
		return nil
	})
	return agents, err
}

// ListVersions returns an agent's versions, newest first.
func (s *AgentStore) ListVersions(ctx context.Context, agentID, workspaceID uuid.UUID) ([]domain.AgentVersion, error) {
	var versions []domain.AgentVersion
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		rows, err := q.ListAgentVersionsForWorkspace(ctx, postgresdb.ListAgentVersionsForWorkspaceParams{
			AgentID:     agentID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			return fmt.Errorf("list agent versions: %w", err)
		}
		versions = make([]domain.AgentVersion, 0, len(rows))
		for _, row := range rows {
			versions = append(versions, agentVersionToDomain(row))
		}
		return nil
	})
	return versions, err
}

// GetVersion returns one version, scoped to the workspace in context.
//
// M4.2 reads through here: a session is pinned to a version id, and this is
// what resolves it back to the settings that session ran under.
func (s *AgentStore) GetVersion(ctx context.Context, versionID, workspaceID uuid.UUID) (domain.AgentVersion, error) {
	var version domain.AgentVersion
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		row, err := q.GetAgentVersionForWorkspace(ctx, postgresdb.GetAgentVersionForWorkspaceParams{
			ID:          versionID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrAgentNotFound
			}
			return fmt.Errorf("select agent version: %w", err)
		}
		version = agentVersionToDomain(row)
		return nil
	})
	return version, err
}

// translateAgentError names the constraints a caller can act on.
func translateAgentError(err error) error {
	switch {
	case constraintViolated(err, "agents_workspace_name_key"):
		return domain.ErrAgentNameTaken
	case constraintViolated(err, "agent_versions_capabilities_known"):
		return fmt.Errorf("%w: the database refused a capability the application accepted, "+
			"which means the two lists have drifted apart", domain.ErrUnknownCapability)
	case constraintViolated(err, "agent_versions_provider_valid"):
		return fmt.Errorf("%w: unknown provider", domain.ErrInvalidAgent)
	case constraintViolated(err, "agents_name_not_blank"), constraintViolated(err, "agents_name_length"):
		return fmt.Errorf("%w: the name is empty or too long", domain.ErrInvalidAgent)
	default:
		return fmt.Errorf("write agent: %w", err)
	}
}

// agentToDomain converts a generated row.
func agentToDomain(row postgresdb.Agent) domain.Agent {
	agent := domain.Agent{
		ID:          row.ID,
		WorkspaceID: row.WorkspaceID,
		Name:        row.Name,
		CreatedBy:   row.CreatedBy,
		CreatedAt:   timestamp(row.CreatedAt),
		UpdatedAt:   timestamp(row.UpdatedAt),
	}
	if row.CurrentVersionID.Valid {
		id := uuid.UUID(row.CurrentVersionID.Bytes)
		agent.CurrentVersionID = &id
	}
	return agent
}

// agentVersionToDomain converts a generated row.
func agentVersionToDomain(row postgresdb.AgentVersion) domain.AgentVersion {
	capabilities := make([]domain.Capability, 0, len(row.Capabilities))
	for _, value := range row.Capabilities {
		capabilities = append(capabilities, domain.Capability(value))
	}
	return domain.AgentVersion{
		ID:           row.ID,
		AgentID:      row.AgentID,
		WorkspaceID:  row.WorkspaceID,
		Version:      row.Version,
		Provider:     domain.Provider(row.Provider),
		Model:        row.Model,
		Capabilities: capabilities,
		ToolPolicy:   row.ToolPolicy,
		CreatedBy:    row.CreatedBy,
		CreatedAt:    timestamp(row.CreatedAt),
	}
}
