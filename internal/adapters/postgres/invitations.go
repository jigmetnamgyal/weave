package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// InvitationStore persists workspace invitations.
type InvitationStore struct {
	pool    *pgxpool.Pool
	queries *postgresdb.Queries
}

// compile-time check that the adapter satisfies the port.
var _ application.InvitationStore = (*InvitationStore)(nil)

// NewInvitationStore constructs an InvitationStore.
func NewInvitationStore(pool *pgxpool.Pool) *InvitationStore {
	return &InvitationStore{pool: pool, queries: postgresdb.New(pool)}
}

// inTx runs fn inside a transaction, rolling back on error or panic.
func (s *InvitationStore) inTx(ctx context.Context, fn func(*postgresdb.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(s.queries.WithTx(tx)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// Create records an invitation, superseding an expired one if it holds the
// slot, and appends the audit event.
func (s *InvitationStore) Create(ctx context.Context, invitation domain.Invitation, tokenHash []byte, actor application.Actor, supersede uuid.UUID, event application.AuditEvent) (domain.Invitation, error) {
	var created domain.Invitation

	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		// Same in-transaction re-check as every other mutation: the handler
		// authorized from a snapshot, and the inviter may have been removed
		// or demoted since.
		if err := authorizeActor(ctx, q, invitation.WorkspaceID, actor); err != nil {
			return err
		}

		if supersede != uuid.Nil {
			// Revoked rather than deleted: the expired invitation is a fact
			// about what was offered, and superseding it should not erase it.
			if _, err := q.RevokeInvitation(ctx, postgresdb.RevokeInvitationParams{
				ID:          supersede,
				WorkspaceID: invitation.WorkspaceID,
				RevokedBy:   pgtype.UUID{Bytes: actor.UserID, Valid: true},
			}); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("supersede expired invitation: %w", err)
			}
		}

		row, err := q.CreateInvitation(ctx, postgresdb.CreateInvitationParams{
			ID:          invitation.ID,
			WorkspaceID: invitation.WorkspaceID,
			Email:       invitation.Email,
			Role:        string(invitation.Role),
			TokenHash:   tokenHash,
			InvitedBy:   invitation.InvitedBy,
			ExpiresAt:   pgtype.Timestamptz{Time: invitation.ExpiresAt, Valid: true},
		})
		if err != nil {
			if isUniqueViolation(err) {
				// Another request took the slot between our check and this
				// insert. The unique index is what actually enforces "one
				// outstanding", so report it as such rather than as a driver
				// error.
				return application.ErrInvitationOutstanding
			}
			return fmt.Errorf("insert invitation: %w", err)
		}

		if err := appendAudit(ctx, q, event); err != nil {
			return err
		}

		created = invitationToDomain(row)
		return nil
	})
	if err != nil {
		return domain.Invitation{}, err
	}
	return created, nil
}

// Outstanding returns the invitation holding the one-outstanding slot.
func (s *InvitationStore) Outstanding(ctx context.Context, workspaceID uuid.UUID, email string) (domain.Invitation, error) {
	row, err := s.queries.GetOutstandingInvitationForEmail(ctx, postgresdb.GetOutstandingInvitationForEmailParams{
		WorkspaceID: workspaceID,
		Email:       email,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Invitation{}, application.ErrInvitationNotFound
		}
		return domain.Invitation{}, fmt.Errorf("select outstanding invitation: %w", err)
	}
	return invitationToDomain(row), nil
}

// ListForWorkspace returns a workspace's invitations, newest first.
func (s *InvitationStore) ListForWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]application.InvitationRecord, error) {
	rows, err := s.queries.ListInvitationsForWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}

	records := make([]application.InvitationRecord, 0, len(rows))
	for _, row := range rows {
		records = append(records, application.InvitationRecord{
			// Status is left unset: only the service knows which instant to
			// resolve it against.
			Invitation: domain.Invitation{
				ID:          row.ID,
				WorkspaceID: row.WorkspaceID,
				Email:       row.Email,
				Role:        domain.Role(row.Role),
				InvitedBy:   row.InvitedBy,
				ExpiresAt:   timestamp(row.ExpiresAt),
				CreatedAt:   timestamp(row.CreatedAt),
				AcceptedAt:  timestamp(row.AcceptedAt),
				AcceptedBy:  optionalUUID(row.AcceptedBy),
				RevokedAt:   timestamp(row.RevokedAt),
				RevokedBy:   optionalUUID(row.RevokedBy),
			},
			InvitedByEmail:       row.InvitedByEmail,
			InvitedByDisplayName: derefText(row.InvitedByDisplayName),
		})
	}
	return records, nil
}

// Revoke withdraws an outstanding invitation.
func (s *InvitationStore) Revoke(ctx context.Context, workspaceID, invitationID uuid.UUID, actor application.Actor, event application.AuditEvent) error {
	return s.inTx(ctx, func(q *postgresdb.Queries) error {
		if err := authorizeActor(ctx, q, workspaceID, actor); err != nil {
			return err
		}

		// Scoped by workspace, so an invitation id belonging to another tenant
		// matches nothing rather than being revoked from the wrong workspace.
		if _, err := q.RevokeInvitation(ctx, postgresdb.RevokeInvitationParams{
			ID:          invitationID,
			WorkspaceID: workspaceID,
			RevokedBy:   pgtype.UUID{Bytes: actor.UserID, Valid: true},
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrInvitationNotFound
			}
			return fmt.Errorf("revoke invitation: %w", err)
		}

		return appendAudit(ctx, q, event)
	})
}

// ByTokenHash looks an invitation up by its token hash.
func (s *InvitationStore) ByTokenHash(ctx context.Context, tokenHash []byte) (domain.Invitation, error) {
	row, err := s.queries.GetInvitationByTokenHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Invitation{}, application.ErrInvitationNotFound
		}
		return domain.Invitation{}, fmt.Errorf("select invitation by token: %w", err)
	}
	return invitationToDomain(row), nil
}

// Context returns the workspace name and inviter behind an invitation.
func (s *InvitationStore) Context(ctx context.Context, invitationID uuid.UUID) (application.InvitationPreview, error) {
	row, err := s.queries.GetInvitationWorkspaceContext(ctx, invitationID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return application.InvitationPreview{}, application.ErrInvitationNotFound
		}
		return application.InvitationPreview{}, fmt.Errorf("select invitation context: %w", err)
	}
	return application.InvitationPreview{
		WorkspaceName:        row.WorkspaceName,
		InvitedByEmail:       row.InvitedByEmail,
		InvitedByDisplayName: derefText(row.InvitedByDisplayName),
	}, nil
}

// Accept claims the invitation and creates the membership.
func (s *InvitationStore) Accept(ctx context.Context, invitationID, userID uuid.UUID, role domain.Role, event application.AuditEvent) (domain.Membership, error) {
	var membership domain.Membership

	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		// The claim comes first and is conditional on the invitation still
		// being usable. Two concurrent accepts race on this single statement;
		// exactly one matches a row, so exactly one membership is created.
		claimed, err := q.ClaimInvitation(ctx, postgresdb.ClaimInvitationParams{
			ID:         invitationID,
			AcceptedBy: pgtype.UUID{Bytes: userID, Valid: true},
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Someone else claimed it, or it expired or was revoked
				// between the read and here.
				return domain.ErrInvitationNotUsable
			}
			return fmt.Errorf("claim invitation: %w", err)
		}

		if _, err := q.AddWorkspaceMember(ctx, postgresdb.AddWorkspaceMemberParams{
			WorkspaceID: claimed.WorkspaceID,
			UserID:      userID,
			Role:        string(role),
		}); err != nil {
			if isUniqueViolation(err) {
				// Already a member. Rolling back leaves the invitation
				// unclaimed, which is right: it was never used.
				return application.ErrAlreadyMember
			}
			return fmt.Errorf("insert membership: %w", err)
		}

		if err := appendAudit(ctx, q, event); err != nil {
			return err
		}

		membership = domain.Membership{
			WorkspaceID: claimed.WorkspaceID,
			UserID:      userID,
			Role:        role,
		}
		return nil
	})
	if err != nil {
		return domain.Membership{}, err
	}
	return membership, nil
}

// invitationToDomain converts a generated row into the domain entity.
func invitationToDomain(row postgresdb.WorkspaceInvitation) domain.Invitation {
	return domain.Invitation{
		ID:          row.ID,
		WorkspaceID: row.WorkspaceID,
		Email:       row.Email,
		Role:        domain.Role(row.Role),
		InvitedBy:   row.InvitedBy,
		ExpiresAt:   timestamp(row.ExpiresAt),
		CreatedAt:   timestamp(row.CreatedAt),
		AcceptedAt:  timestamp(row.AcceptedAt),
		AcceptedBy:  optionalUUID(row.AcceptedBy),
		RevokedAt:   timestamp(row.RevokedAt),
		RevokedBy:   optionalUUID(row.RevokedBy),
	}
}

// optionalUUID unwraps a nullable uuid column.
func optionalUUID(value pgtype.UUID) uuid.UUID {
	if !value.Valid {
		return uuid.Nil
	}
	return value.Bytes
}
