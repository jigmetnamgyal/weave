package domain

import "testing"

// TestMatrixIsExhaustive is the guard that makes the matrix trustworthy.
//
// A permission missing from a role's entry denies at runtime, which looks
// identical to a considered denial — but nobody decided it. This fails the
// build instead, so adding a role or a permission forces every pairing to be
// written down.
func TestMatrixIsExhaustive(t *testing.T) {
	if len(rolePermissions) != len(Roles) {
		t.Errorf("matrix has %d roles, but %d are declared", len(rolePermissions), len(Roles))
	}

	for _, role := range Roles {
		entry, ok := rolePermissions[role]
		if !ok {
			t.Errorf("role %q is declared but absent from the matrix", role)
			continue
		}
		for _, permission := range Permissions {
			if _, decided := entry[permission]; !decided {
				t.Errorf("role %q does not decide permission %q — write it out, even as false",
					role, permission)
			}
		}
		if len(entry) != len(Permissions) {
			t.Errorf("role %q decides %d permissions, but %d are declared — an unknown key is present",
				role, len(entry), len(Permissions))
		}
	}

	// A role in the matrix that is not declared would grant permissions that
	// no validation accepts, which is confusing rather than dangerous — but
	// still wrong.
	for role := range rolePermissions {
		if !role.Valid() {
			t.Errorf("matrix contains undeclared role %q", role)
		}
	}
}

// TestAuthorizationMatrix pins every role-permission pairing.
//
// Written out in full rather than derived, so that changing the matrix
// requires changing this table too. A test that computed the expectation from
// the same map would pass no matter what the map said.
func TestAuthorizationMatrix(t *testing.T) {
	expected := map[Role]map[Permission]bool{
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

	for _, role := range Roles {
		for _, permission := range Permissions {
			t.Run(string(role)+"/"+string(permission), func(t *testing.T) {
				if got, want := role.Can(permission), expected[role][permission]; got != want {
					t.Errorf("%s.Can(%s) = %v, want %v", role, permission, got, want)
				}
			})
		}
	}
}

// TestUnknownRoleHoldsNothing covers the deny-by-default direction that
// matters most: a role read from a corrupted row, or from a future migration
// rolled back, must grant nothing rather than everything.
func TestUnknownRoleHoldsNothing(t *testing.T) {
	for _, role := range []Role{"", "root", "admin ", "Owner", "superuser"} {
		for _, permission := range Permissions {
			if role.Can(permission) {
				t.Errorf("unknown role %q was granted %q", role, permission)
			}
		}
		if role.Valid() {
			t.Errorf("role %q reported itself valid", role)
		}
	}
}

// TestBillingIsOwnerOnly pins the one separation that is easy to erode:
// delegating day-to-day administration must not hand over billing.
func TestBillingIsOwnerOnly(t *testing.T) {
	for _, role := range Roles {
		got := role.Can(PermissionBillingManage)
		if want := role == RoleOwner; got != want {
			t.Errorf("%s.Can(billing:manage) = %v, want %v", role, got, want)
		}
	}
}

// TestApprovalIsSeparateFromExecution pins the separation that gives approvals
// their meaning: developer can run sessions but cannot approve their own
// risk-sensitive actions.
func TestApprovalIsSeparateFromExecution(t *testing.T) {
	if !RoleDeveloper.Can(PermissionSessionCreate) {
		t.Error("developer cannot create sessions, which is the role's whole purpose")
	}
	if RoleDeveloper.Can(PermissionActionApprove) {
		t.Error("developer can approve actions, collapsing the separation approvals exist for")
	}
}

func TestPermissionsFor(t *testing.T) {
	if got := len(PermissionsFor(RoleOwner)); got != len(Permissions) {
		t.Errorf("owner holds %d permissions, want all %d", got, len(Permissions))
	}
	if got := PermissionsFor(RoleViewer); len(got) != 1 || got[0] != PermissionWorkspaceRead {
		t.Errorf("viewer holds %v, want only workspace:read", got)
	}
	if got := len(PermissionsFor(Role("nope"))); got != 0 {
		t.Errorf("unknown role holds %d permissions, want 0", got)
	}
}

func TestParseRole(t *testing.T) {
	for _, role := range Roles {
		if got, err := ParseRole(string(role)); err != nil || got != role {
			t.Errorf("ParseRole(%q) = %q, %v", role, got, err)
		}
	}
	for _, invalid := range []string{"", "root", "OWNER", "owner "} {
		if _, err := ParseRole(invalid); err == nil {
			t.Errorf("ParseRole(%q) accepted an invalid role", invalid)
		}
	}
}
