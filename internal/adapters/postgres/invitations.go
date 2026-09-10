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

// inTx runs fn inside a transaction carrying the caller's tenant context.
// Reads go through it too — see WorkspaceStore.inTx.
func (s *InvitationStore) inTx(ctx context.Context, fn func(*postgresdb.Queries) error) error {
	return inTenantTx(ctx, s.pool, fn)
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
	var invitation domain.Invitation
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		row, err := q.GetOutstandingInvitationForEmail(ctx, postgresdb.GetOutstandingInvitationForEmailParams{
			WorkspaceID: workspaceID,
			Email:       email,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrInvitationNotFound
			}
			return fmt.Errorf("select outstanding invitation: %w", err)
		}
		invitation = invitationToDomain(row)
		return nil
	})
	return invitation, err
}

// ListForWorkspace returns a workspace's invitations, newest first.
func (s *InvitationStore) ListForWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]application.InvitationRecord, error) {
	var rows []postgresdb.ListInvitationsForWorkspaceRow
	if err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		found, err := q.ListInvitationsForWorkspace(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("list invitations: %w", err)
		}
		rows = found
		return nil
	}); err != nil {
		return nil, err
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
// ByTokenHash looks an invitation up by its token hash.
//
// This is the one read that cannot be workspace-scoped: the caller is not a
// member of anything, and the token is the authorization. It therefore goes
// through weave_invitation_by_token, a SECURITY DEFINER function that returns
// exactly the row whose hash was presented — a deliberate, single-row hole in
// the policies rather than a general exemption.
func (s *InvitationStore) ByTokenHash(ctx context.Context, tokenHash []byte) (domain.Invitation, error) {
	var invitation domain.Invitation
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		row, err := q.GetInvitationByTokenHash(ctx, tokenHash)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrInvitationNotFound
			}
			return fmt.Errorf("select invitation by token: %w", err)
		}
		invitation = invitationToDomain(row)
		return nil
	})
	return invitation, err
}

// Context returns the workspace name and inviter behind an invitation.
func (s *InvitationStore) Context(ctx context.Context, invitationID uuid.UUID) (application.InvitationPreview, error) {
	var preview application.InvitationPreview
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		row, err := q.GetInvitationWorkspaceContext(ctx, invitationID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrInvitationNotFound
			}
			return fmt.Errorf("select invitation context: %w", err)
		}
		preview = application.InvitationPreview{
			WorkspaceName:        row.WorkspaceName,
			InvitedByEmail:       row.InvitedByEmail,
			InvitedByDisplayName: derefText(row.InvitedByDisplayName),
		}
		return nil
	})
	return preview, err
}

// Accept claims the invitation and creates the membership.
func (s *InvitationStore) Accept(ctx context.Context, invitationID, workspaceID, userID uuid.UUID, role domain.Role, event application.AuditEvent) (domain.Membership, error) {
	var membership domain.Membership

	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		// Membership is inserted before the invitation is claimed, and the
		// order is required by the policies rather than chosen for style.
		// Acceptance runs with no workspace context — the acceptor is not a
		// member yet — so the invitation and audit policies authorise through
		// weave_is_member, which only becomes true once this row exists. The
		// insert is visible to the rest of this transaction immediately.
		//
		// Single use is unaffected: the claim below is still conditional, and
		// the membership primary key settles concurrent accepts just as
		// firmly. Either way exactly one succeeds, and a failure at the claim
		// rolls the membership back with it.
		if _, err := q.AddWorkspaceMember(ctx, postgresdb.AddWorkspaceMemberParams{
			WorkspaceID: workspaceID,
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

		claimed, err := q.ClaimInvitation(ctx, postgresdb.ClaimInvitationParams{
			ID:         invitationID,
			AcceptedBy: pgtype.UUID{Bytes: userID, Valid: true},
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Claimed by someone else, or expired or revoked since the
				// read. The membership above rolls back with it.
				return domain.ErrInvitationNotUsable
			}
			return fmt.Errorf("claim invitation: %w", err)
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
