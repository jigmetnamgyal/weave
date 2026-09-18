package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// uniqueViolation is the PostgreSQL SQLSTATE for a unique constraint breach.
const uniqueViolation = "23505"

// isUniqueViolation reports whether err is a unique constraint breach.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == uniqueViolation
}

// WorkspaceStore persists workspaces, membership and audit events.
type WorkspaceStore struct {
	pool    *pgxpool.Pool
	queries *postgresdb.Queries
}

// compile-time check that the adapter satisfies the port.
var _ application.WorkspaceStore = (*WorkspaceStore)(nil)

// NewWorkspaceStore constructs a WorkspaceStore.
func NewWorkspaceStore(pool *pgxpool.Pool) *WorkspaceStore {
	return &WorkspaceStore{pool: pool, queries: postgresdb.New(pool)}
}

// inTx runs fn inside a transaction carrying the caller's tenant context.
//
// Every method goes through here, reads included. Writes need it for
// atomicity — a change and its audit row commit together or not at all — and
// reads need it because row-level security reads the tenant context from the
// transaction. A read issued outside one matches no policy and returns
// nothing.
func (s *WorkspaceStore) inTx(ctx context.Context, fn func(*postgresdb.Queries) error) error {
	return inTenantTx(ctx, s.pool, fn)
}

// CreateWithOwner creates a workspace, its owner membership and the audit row.
func (s *WorkspaceStore) CreateWithOwner(ctx context.Context, workspace domain.Workspace, event application.AuditEvent) (domain.Workspace, error) {
	var created domain.Workspace

	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		row, err := q.CreateWorkspace(ctx, postgresdb.CreateWorkspaceParams{
			ID:        workspace.ID,
			Slug:      workspace.Slug,
			Name:      workspace.Name,
			CreatedBy: workspace.CreatedBy,
		})
		if err != nil {
			if isUniqueViolation(err) {
				// The advisory slug check lost a race with a concurrent
				// creation. Surfacing it as a domain error lets the caller
				// retry with a different slug rather than see a driver error.
				return fmt.Errorf("%w: slug %q is already taken",
					domain.ErrInvalidWorkspace, workspace.Slug)
			}
			return fmt.Errorf("insert workspace: %w", err)
		}

		if _, err := q.AddWorkspaceMember(ctx, postgresdb.AddWorkspaceMemberParams{
			WorkspaceID: row.ID,
			UserID:      workspace.CreatedBy,
			Role:        string(domain.RoleOwner),
		}); err != nil {
			return fmt.Errorf("insert owner membership: %w", err)
		}

		if err := appendAudit(ctx, q, event); err != nil {
			return err
		}

		created = workspaceToDomain(row)
		return nil
	})
	if err != nil {
		return domain.Workspace{}, err
	}
	return created, nil
}

// SlugExists reports whether a slug is already in use.
func (s *WorkspaceStore) SlugExists(ctx context.Context, slug string) (bool, error) {
	var exists bool
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		found, err := q.SlugExists(ctx, slug)
		if err != nil {
			return fmt.Errorf("check slug: %w", err)
		}
		exists = found
		return nil
	})
	return exists, err
}

// GetForMember returns a workspace only when the user is a member.
func (s *WorkspaceStore) GetForMember(ctx context.Context, workspaceID, userID uuid.UUID) (domain.Workspace, error) {
	var workspace domain.Workspace
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		row, err := q.GetWorkspaceForMember(ctx, postgresdb.GetWorkspaceForMemberParams{
			ID:     workspaceID,
			UserID: userID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrWorkspaceNotFound
			}
			return fmt.Errorf("select workspace: %w", err)
		}
		workspace = workspaceToDomain(row)
		return nil
	})
	return workspace, err
}

// ListForUser returns the workspaces a user belongs to, with their role.
func (s *WorkspaceStore) ListForUser(ctx context.Context, userID uuid.UUID) ([]application.WorkspaceMembership, error) {
	var rows []postgresdb.ListWorkspacesForUserRow
	if err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		found, err := q.ListWorkspacesForUser(ctx, userID)
		if err != nil {
			return fmt.Errorf("list workspaces: %w", err)
		}
		rows = found
		return nil
	}); err != nil {
		return nil, err
	}

	result := make([]application.WorkspaceMembership, 0, len(rows))
	for _, row := range rows {
		result = append(result, application.WorkspaceMembership{
			Workspace: domain.Workspace{
				ID:        row.ID,
				Slug:      row.Slug,
				Name:      row.Name,
				CreatedBy: row.CreatedBy,
				Version:   int(row.Version),
				CreatedAt: timestamp(row.CreatedAt),
				UpdatedAt: timestamp(row.UpdatedAt),
			},
			Role: domain.Role(row.Role),
		})
	}
	return result, nil
}

// GetMembership returns a user's membership in a workspace.
func (s *WorkspaceStore) GetMembership(ctx context.Context, workspaceID, userID uuid.UUID) (domain.Membership, error) {
	var membership domain.Membership
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		row, err := q.GetWorkspaceMember(ctx, postgresdb.GetWorkspaceMemberParams{
			WorkspaceID: workspaceID,
			UserID:      userID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrMemberNotFound
			}
			return fmt.Errorf("select membership: %w", err)
		}
		membership = membershipToDomain(row)
		return nil
	})
	return membership, err
}

// ListMembers returns every member of a workspace.
func (s *WorkspaceStore) ListMembers(ctx context.Context, workspaceID uuid.UUID) ([]domain.MemberProfile, error) {
	var rows []postgresdb.ListWorkspaceMembersRow
	if err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		found, err := q.ListWorkspaceMembers(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("list members: %w", err)
		}
		rows = found
		return nil
	}); err != nil {
		return nil, err
	}

	members := make([]domain.MemberProfile, 0, len(rows))
	for _, row := range rows {
		members = append(members, domain.MemberProfile{
			Membership: domain.Membership{
				WorkspaceID: row.WorkspaceID,
				UserID:      row.UserID,
				Role:        domain.Role(row.Role),
				CreatedAt:   timestamp(row.CreatedAt),
				UpdatedAt:   timestamp(row.UpdatedAt),
			},
			Email:       row.Email,
			DisplayName: derefText(row.DisplayName),
			AvatarURL:   derefText(row.AvatarUrl),
		})
	}
	return members, nil
}

// Rename applies a new name under optimistic concurrency.
func (s *WorkspaceStore) Rename(ctx context.Context, workspaceID uuid.UUID, actor application.Actor, expectedVersion int, name string, event application.AuditEvent) (domain.Workspace, error) {
	var updated domain.Workspace

	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		if err := authorizeActor(ctx, q, workspaceID, actor); err != nil {
			return err
		}

		// The caller bounds expectedVersion to int32 before reaching here, so
		// this conversion cannot wrap.
		row, err := q.RenameWorkspace(ctx, postgresdb.RenameWorkspaceParams{
			ID:      workspaceID,
			Version: int32(expectedVersion), //nolint:gosec // range-checked in WorkspaceService.Rename
			Name:    name,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The row exists — authorization already proved membership —
				// so no match means the version moved under us.
				return application.ErrVersionConflict
			}
			return fmt.Errorf("update workspace: %w", err)
		}

		if err := appendAudit(ctx, q, event); err != nil {
			return err
		}

		updated = workspaceToDomain(row)
		return nil
	})
	if err != nil {
		return domain.Workspace{}, err
	}
	return updated, nil
}

// ChangeMemberRole updates a member's role, refusing to remove the last owner.
func (s *WorkspaceStore) ChangeMemberRole(ctx context.Context, workspaceID, userID uuid.UUID, actor application.Actor, role domain.Role, event application.AuditEvent) (domain.Membership, error) {
	var updated domain.Membership

	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		current, err := lockAndLoadMember(ctx, q, workspaceID, userID)
		if err != nil {
			return err
		}

		// Under the lock taken above, so a concurrent demotion of the actor
		// either completes before this check or waits behind it.
		if err := authorizeActorLocked(ctx, q, workspaceID, actor, role); err != nil {
			return err
		}

		// Only a demotion of a current owner can strand the workspace.
		if domain.Role(current.Role) == domain.RoleOwner && role != domain.RoleOwner {
			if err := ensureAnotherOwnerExists(ctx, q, workspaceID); err != nil {
				return err
			}
		}

		row, err := q.UpdateWorkspaceMemberRole(ctx, postgresdb.UpdateWorkspaceMemberRoleParams{
			WorkspaceID: workspaceID,
			UserID:      userID,
			Role:        string(role),
		})
		if err != nil {
			return fmt.Errorf("update member role: %w", err)
		}

		if err := appendAudit(ctx, q, event); err != nil {
			return err
		}

		updated = membershipToDomain(row)
		return nil
	})
	if err != nil {
		return domain.Membership{}, err
	}
	return updated, nil
}

// RemoveMember deletes a membership, refusing to remove the last owner.
func (s *WorkspaceStore) RemoveMember(ctx context.Context, workspaceID, userID uuid.UUID, actor application.Actor, event application.AuditEvent) error {
	return s.inTx(ctx, func(q *postgresdb.Queries) error {
		current, err := lockAndLoadMember(ctx, q, workspaceID, userID)
		if err != nil {
			return err
		}

		if err := authorizeActorLocked(ctx, q, workspaceID, actor, ""); err != nil {
			return err
		}

		if domain.Role(current.Role) == domain.RoleOwner {
			if err := ensureAnotherOwnerExists(ctx, q, workspaceID); err != nil {
				return err
			}
		}

		removed, err := q.DeleteWorkspaceMember(ctx, postgresdb.DeleteWorkspaceMemberParams{
			WorkspaceID: workspaceID,
			UserID:      userID,
		})
		if err != nil {
			return fmt.Errorf("delete member: %w", err)
		}
		if removed == 0 {
			return application.ErrMemberNotFound
		}

		return appendAudit(ctx, q, event)
	})
}

// lockAndLoadMember takes the workspace lock, then reads the target member.
//
// The lock is taken before the read so that concurrent membership changes on
// the same workspace serialise. Without it two callers could each observe two
// owners and each demote one, leaving none.
func lockAndLoadMember(ctx context.Context, q *postgresdb.Queries, workspaceID, userID uuid.UUID) (postgresdb.WorkspaceMember, error) {
	if _, err := q.LockWorkspace(ctx, workspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return postgresdb.WorkspaceMember{}, application.ErrWorkspaceNotFound
		}
		return postgresdb.WorkspaceMember{}, fmt.Errorf("lock workspace: %w", err)
	}

	member, err := q.GetWorkspaceMember(ctx, postgresdb.GetWorkspaceMemberParams{
		WorkspaceID: workspaceID,
		UserID:      userID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return postgresdb.WorkspaceMember{}, application.ErrMemberNotFound
		}
		return postgresdb.WorkspaceMember{}, fmt.Errorf("select member: %w", err)
	}
	return member, nil
}

// authorizeActor takes the workspace lock and then re-checks the actor.
func authorizeActor(ctx context.Context, q *postgresdb.Queries, workspaceID uuid.UUID, actor application.Actor) error {
	if _, err := q.LockWorkspace(ctx, workspaceID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return application.ErrWorkspaceNotFound
		}
		return fmt.Errorf("lock workspace: %w", err)
	}
	return authorizeActorLocked(ctx, q, workspaceID, actor, "")
}

// authorizeActorLocked re-reads the actor's membership inside the transaction
// and confirms they are still entitled to act.
//
// The handler authorized the actor from a snapshot read before the
// transaction opened. Without this, a member removed or demoted in that window
// could still land a privileged write — using authority they no longer hold.
//
// grant, when non-empty, is the role being assigned; the actor must be
// entitled to grant it, re-checked here for the same reason.
func authorizeActorLocked(ctx context.Context, q *postgresdb.Queries, workspaceID uuid.UUID, actor application.Actor, grant domain.Role) error {
	// The system is authorized by being the system.
	//
	// There is no membership to re-read and no permission to hold: a workflow
	// provisioning a session is the product acting on its own, and the
	// alternative — borrowing the member who created it — would attribute an
	// automated change to a person. The workspace lock above still applies, so
	// a system write is serialised with everything else.
	//
	// Nothing reachable from a request handler constructs one; see
	// application.SystemActor.
	if actor.System {
		return nil
	}

	if actor.UserID == uuid.Nil {
		return application.ErrPermissionDenied
	}

	member, err := q.GetWorkspaceMember(ctx, postgresdb.GetWorkspaceMemberParams{
		WorkspaceID: workspaceID,
		UserID:      actor.UserID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Removed since the handler read them.
			return application.ErrPermissionDenied
		}
		return fmt.Errorf("re-read actor membership: %w", err)
	}

	role := domain.Role(member.Role)
	if actor.Required != "" && !role.Can(actor.Required) {
		return application.ErrPermissionDenied
	}
	if grant != "" && !role.CanGrant(grant) {
		return domain.ErrCannotGrantRole
	}
	return nil
}

// ensureAnotherOwnerExists fails when the workspace has only one owner.
func ensureAnotherOwnerExists(ctx context.Context, q *postgresdb.Queries, workspaceID uuid.UUID) error {
	owners, err := q.CountWorkspaceOwners(ctx, workspaceID)
	if err != nil {
		return fmt.Errorf("count owners: %w", err)
	}
	if owners <= 1 {
		return domain.ErrLastOwner
	}
	return nil
}

// appendAudit writes the audit row for a change, inside the caller's
// transaction.
func appendAudit(ctx context.Context, q *postgresdb.Queries, event application.AuditEvent) error {
	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("generate audit id: %w", err)
	}

	detail := []byte("{}")
	if event.Detail != nil {
		detail, err = json.Marshal(event.Detail)
		if err != nil {
			return fmt.Errorf("encode audit detail: %w", err)
		}
	}

	if _, err := q.AppendAuditEvent(ctx, postgresdb.AppendAuditEventParams{
		ID:          id,
		WorkspaceID: event.WorkspaceID,
		ActorUserID: pgtype.UUID{Bytes: event.ActorUserID, Valid: event.ActorUserID != uuid.Nil},
		Action:      event.Action,
		Target:      optionalText(event.Target),
		Detail:      detail,
	}); err != nil {
		return fmt.Errorf("append audit event: %w", err)
	}
	return nil
}

// workspaceToDomain converts a generated row into the domain entity.
func workspaceToDomain(row postgresdb.Workspace) domain.Workspace {
	return domain.Workspace{
		ID:        row.ID,
		Slug:      row.Slug,
		Name:      row.Name,
		CreatedBy: row.CreatedBy,
		Version:   int(row.Version),
		CreatedAt: timestamp(row.CreatedAt),
		UpdatedAt: timestamp(row.UpdatedAt),
	}
}

// membershipToDomain converts a generated row into the domain entity.
func membershipToDomain(row postgresdb.WorkspaceMember) domain.Membership {
	return domain.Membership{
		WorkspaceID: row.WorkspaceID,
		UserID:      row.UserID,
		Role:        domain.Role(row.Role),
		CreatedAt:   timestamp(row.CreatedAt),
		UpdatedAt:   timestamp(row.UpdatedAt),
	}
}
