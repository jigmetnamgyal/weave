package domain

import "fmt"

// Role is a member's position in a workspace.
//
// A role is never interpreted anywhere except this file. Handlers, stores and
// UI components ask whether a permission is held; they do not compare role
// names. That is what makes adding a role a single-file change instead of a
// hunt through the codebase for `== "admin"`.
type Role string

// The roles a workspace member may hold, ordered from most to least
// privileged. The database constrains its `role` column to exactly these.
const (
	RoleOwner     Role = "owner"
	RoleAdmin     Role = "admin"
	RoleDeveloper Role = "developer"
	RoleViewer    Role = "viewer"
)

// Roles is every valid role. Order is significant only for display.
var Roles = []Role{RoleOwner, RoleAdmin, RoleDeveloper, RoleViewer}

// Valid reports whether r is a role the product recognises.
func (r Role) Valid() bool {
	for _, known := range Roles {
		if r == known {
			return true
		}
	}
	return false
}

// String satisfies fmt.Stringer.
func (r Role) String() string { return string(r) }

// Permission names an action, not a person.
//
// Naming permissions after actions rather than roles is what lets the role
// model change without touching call sites: a handler declares that it
// performs `member:manage`, and stays correct when a new role is introduced
// that also may manage members.
type Permission string

// The permissions the product recognises, following `architecture.md`.
const (
	// PermissionWorkspaceRead covers seeing a workspace and its members.
	PermissionWorkspaceRead Permission = "workspace:read"
	// PermissionWorkspaceManage covers renaming and settings.
	PermissionWorkspaceManage Permission = "workspace:manage"
	// PermissionMemberInvite covers issuing invitations (M2.3).
	PermissionMemberInvite Permission = "member:invite"
	// PermissionMemberManage covers changing roles and removing members.
	PermissionMemberManage Permission = "member:manage"
	// PermissionSessionCreate covers starting an agent session.
	PermissionSessionCreate Permission = "session:create"
	// PermissionSessionControl covers pausing, resuming and cancelling.
	PermissionSessionControl Permission = "session:control"
	// PermissionActionApprove covers approving a risk-sensitive tool action.
	PermissionActionApprove Permission = "action:approve"
	// PermissionRepositoryManage covers connecting and configuring repositories.
	PermissionRepositoryManage Permission = "repository:manage"
	// PermissionBillingManage covers plans, payment methods and invoices.
	PermissionBillingManage Permission = "billing:manage"
)

// Permissions is every permission the product recognises.
var Permissions = []Permission{
	PermissionWorkspaceRead,
	PermissionWorkspaceManage,
	PermissionMemberInvite,
	PermissionMemberManage,
	PermissionSessionCreate,
	PermissionSessionControl,
	PermissionActionApprove,
	PermissionRepositoryManage,
	PermissionBillingManage,
}

// rolePermissions is the authorization matrix: the single place in the
// codebase where a role implies anything.
//
// Every role must appear, and every permission must be decided for it —
// including the ones it does not get, written out as false rather than
// omitted. An omission reads identically to a considered denial at runtime
// but not to a reviewer, and `TestMatrixIsExhaustive` rejects both.
//
// The shape of the matrix:
//
//   - owner has everything, including billing. There is always at least one.
//   - admin runs the workspace day to day but cannot touch billing, so
//     operational delegation does not hand over the company card.
//   - developer does the work — starts and steers sessions — but does not
//     approve risk-sensitive actions. Separating who acts from who approves
//     is the point of having approvals at all.
//   - viewer reads. Clients and stakeholders sit here.
var rolePermissions = map[Role]map[Permission]bool{
	RoleOwner: {
		PermissionWorkspaceRead:    true,
		PermissionWorkspaceManage:  true,
		PermissionMemberInvite:     true,
		PermissionMemberManage:     true,
		PermissionSessionCreate:    true,
		PermissionSessionControl:   true,
		PermissionActionApprove:    true,
		PermissionRepositoryManage: true,
		PermissionBillingManage:    true,
	},
	RoleAdmin: {
		PermissionWorkspaceRead:    true,
		PermissionWorkspaceManage:  true,
		PermissionMemberInvite:     true,
		PermissionMemberManage:     true,
		PermissionSessionCreate:    true,
		PermissionSessionControl:   true,
		PermissionActionApprove:    true,
		PermissionRepositoryManage: true,
		PermissionBillingManage:    false,
	},
	RoleDeveloper: {
		PermissionWorkspaceRead:    true,
		PermissionWorkspaceManage:  false,
		PermissionMemberInvite:     false,
		PermissionMemberManage:     false,
		PermissionSessionCreate:    true,
		PermissionSessionControl:   true,
		PermissionActionApprove:    false,
		PermissionRepositoryManage: false,
		PermissionBillingManage:    false,
	},
	RoleViewer: {
		PermissionWorkspaceRead:    true,
		PermissionWorkspaceManage:  false,
		PermissionMemberInvite:     false,
		PermissionMemberManage:     false,
		PermissionSessionCreate:    false,
		PermissionSessionControl:   false,
		PermissionActionApprove:    false,
		PermissionRepositoryManage: false,
		PermissionBillingManage:    false,
	},
}

// Can reports whether a role holds a permission.
//
// Deny by default in both directions: an unknown role holds nothing, and a
// permission absent from a known role's entry is denied rather than assumed.
func (r Role) Can(permission Permission) bool {
	return rolePermissions[r][permission]
}

// CanGrant reports whether a member holding role r may assign role other to
// someone.
//
// The rule is that you cannot grant authority you do not hold yourself.
// Without it, `member:manage` alone is a privilege-escalation primitive: an
// admin holds it, so an admin could promote themselves to owner and pick up
// `billing:manage` — a permission the matrix deliberately withholds from them.
//
// Expressed as a permission subset rather than a role ranking, so it stays
// correct if a future role is added that is not neatly above or below the
// others.
func (r Role) CanGrant(other Role) bool {
	if !r.Valid() || !other.Valid() {
		return false
	}
	for _, permission := range Permissions {
		if other.Can(permission) && !r.Can(permission) {
			return false
		}
	}
	return true
}

// PermissionsFor returns the permissions a role holds, in the order declared
// by Permissions. Used to tell a client what it may do without exposing the
// matrix itself.
func PermissionsFor(role Role) []Permission {
	held := make([]Permission, 0, len(Permissions))
	for _, permission := range Permissions {
		if role.Can(permission) {
			held = append(held, permission)
		}
	}
	return held
}

// ParseRole converts external input into a Role, rejecting anything unknown.
func ParseRole(value string) (Role, error) {
	role := Role(value)
	if !role.Valid() {
		return "", fmt.Errorf("%w: unknown role %q", ErrInvalidWorkspace, value)
	}
	return role, nil
}
