package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// GetInputs reads frozen task text and the pinned agent settings in the caller's
// tenant scope. Missing/legacy/cross-tenant input is the same safe refusal; never
// recover it from a mutable task. This is an internal store API, not a public
// route or an authorization grant. Callers must authorize before invoking it.
func (s *SessionStore) GetInputs(ctx context.Context, sessionID, workspaceID uuid.UUID) (domain.SessionInput, error) {
	var input domain.SessionInput
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		row, err := q.GetSessionInputForWorkspace(ctx, postgresdb.GetSessionInputForWorkspaceParams{SessionID: sessionID, WorkspaceID: workspaceID})
		if errors.Is(err, pgx.ErrNoRows) {
			return application.ErrSessionInputUnavailable
		}
		if err != nil {
			return fmt.Errorf("read session input: %w", err)
		}
		if row.InputVersion != 1 {
			return application.ErrSessionInputUnavailable
		}
		provider, err := domain.ParseProvider(row.Provider)
		if err != nil {
			return application.ErrSessionInputUnavailable
		}
		capabilities, err := domain.ParseCapabilities(row.Capabilities)
		if err != nil {
			return application.ErrSessionInputUnavailable
		}
		input = domain.SessionInput{
			SessionID: row.SessionID, WorkspaceID: row.WorkspaceID, InputVersion: row.InputVersion,
			TaskID: row.TaskID, TaskTitle: row.TaskTitle, TaskBody: row.TaskBody,
			AgentVersionID: row.AgentVersionID, Provider: provider, Model: row.Model,
			Capabilities: capabilities, ToolPolicy: row.ToolPolicy,
		}
		return nil
	})
	if err != nil {
		return domain.SessionInput{}, err
	}
	return input, nil
}
