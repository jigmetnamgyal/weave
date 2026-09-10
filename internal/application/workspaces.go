package application

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Errors the workspace use cases raise.
var (
	// ErrWorkspaceNotFound is returned when no workspace matches, or when the
	// caller is not a member of it. The two are deliberately indistinguishable
	// to the caller: telling someone a workspace exists but is not theirs lets
	// them enumerate tenants.
	ErrWorkspaceNotFound = errors.New("workspace not found")
	// ErrMemberNotFound is returned when the target user is not a member.
	ErrMemberNotFound = errors.New("workspace member not found")
	// ErrPermissionDenied is returned when a member lacks the permission an
	// operation requires. Distinct from ErrWorkspaceNotFound: this caller can
	// see the workspace, they just may not do this.
	ErrPermissionDenied = errors.New("permission denied")
	// ErrVersionConflict is returned when an update carries a stale version.
	ErrVersionConflict = errors.New("workspace was modified by someone else")
	// ErrAlreadyMember is returned when adding a user who already belongs.
	ErrAlreadyMember = errors.New("user is already a member of this workspace")
)

// AuditEvent is a security-relevant change worth keeping a record of.
type AuditEvent struct {
	WorkspaceID uuid.UUID
	ActorUserID uuid.UUID
	Action      string
	Target      string
	Detail      map[string]any
}

// Audit action names. Stable strings: they are queried by operators and must
// not change meaning between releases.
const (
	AuditWorkspaceCreated   = "workspace.created"
	AuditWorkspaceRenamed   = "workspace.renamed"
	AuditMemberRoleChanged  = "workspace.member.role_changed"
	AuditMemberRemoved      = "workspace.member.removed"
	AuditWorkspaceCreatedBy = "created_by"
)

// WorkspaceStore persists workspaces, membership and the audit log.
//
// Several methods are documented as atomic. That is a requirement of the port,
// not an implementation detail: a role change whose audit row is missing, or
// an audit row for a change that did not commit, is worse than either alone.
// The transaction lives in the adapter so that the application layer does not
// import a database driver.
type WorkspaceStore interface {
	// CreateWithOwner creates the workspace, adds the creator as owner, and
	// appends the audit event — atomically, or not at all.
	CreateWithOwner(ctx context.Context, workspace domain.Workspace, event AuditEvent) (domain.Workspace, error)

	// SlugExists reports whether a slug is taken. Advisory only: creation
	// still relies on the unique constraint to settle a race.
	SlugExists(ctx context.Context, slug string) (bool, error)

	// GetForMember returns a workspace only if the user is a member of it,
	// and returns ErrWorkspaceNotFound otherwise.
	GetForMember(ctx context.Context, workspaceID, userID uuid.UUID) (domain.Workspace, error)

	// ListForUser returns the workspaces a user belongs to, with their role.
	ListForUser(ctx context.Context, userID uuid.UUID) ([]WorkspaceMembership, error)

	// GetMembership returns a user's membership, or ErrMemberNotFound.
	GetMembership(ctx context.Context, workspaceID, userID uuid.UUID) (domain.Membership, error)

	// ListMembers returns every member of a workspace with their profile.
	ListMembers(ctx context.Context, workspaceID uuid.UUID) ([]domain.MemberProfile, error)

	// Rename applies a new name if expectedVersion still matches, and appends
	// the audit event atomically. Returns ErrVersionConflict on a mismatch.
	Rename(ctx context.Context, workspaceID uuid.UUID, expectedVersion int, name string, event AuditEvent) (domain.Workspace, error)

	// ChangeMemberRole updates a member's role and appends the audit event
	// atomically. It must refuse to demote the last owner, deciding that under
	// a lock so two concurrent demotions cannot both succeed.
	ChangeMemberRole(ctx context.Context, workspaceID, userID uuid.UUID, role domain.Role, event AuditEvent) (domain.Membership, error)

	// RemoveMember deletes a membership and appends the audit event
	// atomically, under the same last-owner rule as ChangeMemberRole.
	RemoveMember(ctx context.Context, workspaceID, userID uuid.UUID, event AuditEvent) error
}

// WorkspaceMembership pairs a workspace with the caller's role in it.
type WorkspaceMembership struct {
	Workspace domain.Workspace
	Role      domain.Role
}

// WorkspaceService holds the workspace use cases.
type WorkspaceService struct {
	store WorkspaceStore
}

// NewWorkspaceService constructs the service.
func NewWorkspaceService(store WorkspaceStore) *WorkspaceService {
	return &WorkspaceService{store: store}
}

// slugSuffixAttempts bounds how many times a derived slug is disambiguated
// before falling back to one built from the workspace's own identifier.
const slugSuffixAttempts = 5

// Create makes a workspace and installs the caller as its owner.
func (s *WorkspaceService) Create(ctx context.Context, actorUserID uuid.UUID, name string) (domain.Workspace, error) {
	validName, err := domain.ValidateWorkspaceName(name)
	if err != nil {
		return domain.Workspace{}, err
	}

	id, err := domain.NewWorkspaceID()
	if err != nil {
		return domain.Workspace{}, err
	}

	slug, err := s.availableSlug(ctx, validName, id)
	if err != nil {
		return domain.Workspace{}, err
	}

	workspace := domain.Workspace{
		ID:        id,
		Slug:      slug,
		Name:      validName,
		CreatedBy: actorUserID,
		Version:   1,
	}

	created, err := s.store.CreateWithOwner(ctx, workspace, AuditEvent{
		WorkspaceID: id,
		ActorUserID: actorUserID,
		Action:      AuditWorkspaceCreated,
		Target:      id.String(),
		Detail:      map[string]any{"name": validName, "slug": slug},
	})
	if err != nil {
		return domain.Workspace{}, fmt.Errorf("create workspace: %w", err)
	}
	return created, nil
}

// availableSlug derives a slug from the name and disambiguates collisions.
//
// The check is advisory — two concurrent creations can both see a slug free —
// so the unique constraint remains the real guarantee. This only keeps the
// common case tidy.
func (s *WorkspaceService) availableSlug(ctx context.Context, name string, id uuid.UUID) (string, error) {
	base := domain.SlugFromName(name)
	if base == "" {
		// A name with nothing slug-safe in it, such as one written entirely in
		// a non-Latin script. Fall back to the identifier rather than refusing
		// a perfectly good name.
		return "w-" + strings.ToLower(id.String()[:8]), nil
	}

	candidate := base
	for attempt := range slugSuffixAttempts {
		taken, err := s.store.SlugExists(ctx, candidate)
		if err != nil {
			return "", fmt.Errorf("check slug availability: %w", err)
		}
		if !taken {
			return candidate, nil
		}
		candidate = fmt.Sprintf("%s-%d", base, attempt+2)
	}

	return base + "-" + strings.ToLower(id.String()[:8]), nil
}

// List returns the workspaces a user belongs to.
func (s *WorkspaceService) List(ctx context.Context, userID uuid.UUID) ([]WorkspaceMembership, error) {
	workspaces, err := s.store.ListForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("list workspaces: %w", err)
	}
	return workspaces, nil
}

// Get returns a workspace the caller is a member of.
func (s *WorkspaceService) Get(ctx context.Context, workspaceID, userID uuid.UUID) (domain.Workspace, error) {
	workspace, err := s.store.GetForMember(ctx, workspaceID, userID)
	if err != nil {
		return domain.Workspace{}, err
	}
	return workspace, nil
}

// Membership returns the caller's membership, used by the authorization step.
func (s *WorkspaceService) Membership(ctx context.Context, workspaceID, userID uuid.UUID) (domain.Membership, error) {
	return s.store.GetMembership(ctx, workspaceID, userID)
}

// ListMembers returns a workspace's members.
func (s *WorkspaceService) ListMembers(ctx context.Context, actor domain.Membership) ([]domain.MemberProfile, error) {
	if err := require(actor, domain.PermissionWorkspaceRead); err != nil {
		return nil, err
	}
	members, err := s.store.ListMembers(ctx, actor.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("list members: %w", err)
	}
	return members, nil
}

// Rename changes a workspace's name under optimistic concurrency.
func (s *WorkspaceService) Rename(ctx context.Context, actor domain.Membership, expectedVersion int, name string) (domain.Workspace, error) {
	if err := require(actor, domain.PermissionWorkspaceManage); err != nil {
		return domain.Workspace{}, err
	}

	validName, err := domain.ValidateWorkspaceName(name)
	if err != nil {
		return domain.Workspace{}, err
	}

	updated, err := s.store.Rename(ctx, actor.WorkspaceID, expectedVersion, validName, AuditEvent{
		WorkspaceID: actor.WorkspaceID,
		ActorUserID: actor.UserID,
		Action:      AuditWorkspaceRenamed,
		Target:      actor.WorkspaceID.String(),
		Detail:      map[string]any{"name": validName},
	})
	if err != nil {
		return domain.Workspace{}, err
	}
	return updated, nil
}

// ChangeMemberRole updates another member's role.
func (s *WorkspaceService) ChangeMemberRole(ctx context.Context, actor domain.Membership, targetUserID uuid.UUID, role domain.Role) (domain.Membership, error) {
	if err := require(actor, domain.PermissionMemberManage); err != nil {
		return domain.Membership{}, err
	}
	if !role.Valid() {
		return domain.Membership{}, fmt.Errorf("%w: unknown role %q", domain.ErrInvalidWorkspace, role)
	}

	updated, err := s.store.ChangeMemberRole(ctx, actor.WorkspaceID, targetUserID, role, AuditEvent{
		WorkspaceID: actor.WorkspaceID,
		ActorUserID: actor.UserID,
		Action:      AuditMemberRoleChanged,
		Target:      targetUserID.String(),
		Detail:      map[string]any{"role": role.String()},
	})
	if err != nil {
		return domain.Membership{}, err
	}
	return updated, nil
}

// RemoveMember removes a member from a workspace.
//
// A member may always remove themselves; removing anyone else requires
// member:manage. Either way the last owner cannot go, which the store decides
// under a lock.
func (s *WorkspaceService) RemoveMember(ctx context.Context, actor domain.Membership, targetUserID uuid.UUID) error {
	if targetUserID != actor.UserID {
		if err := require(actor, domain.PermissionMemberManage); err != nil {
			return err
		}
	}

	return s.store.RemoveMember(ctx, actor.WorkspaceID, targetUserID, AuditEvent{
		WorkspaceID: actor.WorkspaceID,
		ActorUserID: actor.UserID,
		Action:      AuditMemberRemoved,
		Target:      targetUserID.String(),
		Detail:      map[string]any{"self": targetUserID == actor.UserID},
	})
}

// require is the single gate every use case passes through.
func require(actor domain.Membership, permission domain.Permission) error {
	if !actor.Can(permission) {
		return fmt.Errorf("%w: %s requires %s", ErrPermissionDenied, actor.Role, permission)
	}
	return nil
}
