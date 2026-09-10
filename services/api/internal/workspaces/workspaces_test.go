package workspaces

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/api/internal/auth"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

var (
	workspaceA = uuid.MustParse("018f0000-0000-7000-8000-00000000000a")
	workspaceB = uuid.MustParse("018f0000-0000-7000-8000-00000000000b")
	memberID   = uuid.MustParse("018f0000-0000-7000-8000-000000000001")
	outsiderID = uuid.MustParse("018f0000-0000-7000-8000-000000000002")
	targetID   = uuid.MustParse("018f0000-0000-7000-8000-000000000003")
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeStore serves one workspace (workspaceA) whose sole member is memberID,
// at a configurable role.
type fakeStore struct {
	role domain.Role
}

func (f *fakeStore) CreateWithOwner(_ context.Context, w domain.Workspace, _ application.AuditEvent) (domain.Workspace, error) {
	return w, nil
}
func (f *fakeStore) SlugExists(context.Context, string) (bool, error) { return false, nil }

func (f *fakeStore) GetForMember(_ context.Context, workspaceID, userID uuid.UUID) (domain.Workspace, error) {
	if workspaceID != workspaceA || userID != memberID {
		return domain.Workspace{}, application.ErrWorkspaceNotFound
	}
	return domain.Workspace{ID: workspaceA, Slug: "acme", Name: "Acme", Version: 3}, nil
}

func (f *fakeStore) ListForUser(context.Context, uuid.UUID) ([]application.WorkspaceMembership, error) {
	return nil, nil
}

func (f *fakeStore) GetMembership(_ context.Context, workspaceID, userID uuid.UUID) (domain.Membership, error) {
	if workspaceID != workspaceA || userID != memberID {
		return domain.Membership{}, application.ErrMemberNotFound
	}
	return domain.Membership{WorkspaceID: workspaceA, UserID: memberID, Role: f.role}, nil
}

func (f *fakeStore) ListMembers(context.Context, uuid.UUID) ([]domain.MemberProfile, error) {
	return nil, nil
}

func (f *fakeStore) Rename(_ context.Context, _ uuid.UUID, _ application.Actor, _ int, name string, _ application.AuditEvent) (domain.Workspace, error) {
	return domain.Workspace{ID: workspaceA, Slug: "acme", Name: name, Version: 4}, nil
}

func (f *fakeStore) ChangeMemberRole(_ context.Context, _, userID uuid.UUID, _ application.Actor, role domain.Role, _ application.AuditEvent) (domain.Membership, error) {
	return domain.Membership{WorkspaceID: workspaceA, UserID: userID, Role: role}, nil
}

func (f *fakeStore) RemoveMember(context.Context, uuid.UUID, uuid.UUID, application.Actor, application.AuditEvent) error {
	return nil
}

// serveAs runs a request through the router with the given caller identity.
// A nil user means unauthenticated.
func serveAs(t *testing.T, role domain.Role, user *uuid.UUID, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	handler := NewHandler(application.NewWorkspaceService(&fakeStore{role: role}), discardLogger())
	mux := http.NewServeMux()
	handler.Register(mux)

	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)

	// The authentication middleware runs before this package; simulate its
	// result by seeding the context the same way.
	if user != nil {
		ctx := auth.ContextWithUser(req.Context(), domain.User{ID: *user, Email: "dev@example.com"})
		req = req.WithContext(ctx)
	}

	rec := httptest.NewRecorder()
	httpx.WithRequestID(mux).ServeHTTP(rec, req)
	return rec
}

// endpoints is every workspace-scoped route, used to assert a property holds
// across the whole surface rather than on a sample of it.
var endpoints = []struct {
	name   string
	method string
	path   string
	body   string
}{
	{"get workspace", http.MethodGet, "/v1/workspaces/" + workspaceA.String(), ""},
	{"rename workspace", http.MethodPatch, "/v1/workspaces/" + workspaceA.String(), `{"name":"New","version":3}`},
	{"list members", http.MethodGet, "/v1/workspaces/" + workspaceA.String() + "/members", ""},
	{"change role", http.MethodPatch, "/v1/workspaces/" + workspaceA.String() + "/members/" + targetID.String(), `{"role":"admin"}`},
	{"remove member", http.MethodDelete, "/v1/workspaces/" + workspaceA.String() + "/members/" + targetID.String(), ""},
}

// TestNonMemberGetsNotFoundEverywhere is the tenant-isolation guarantee at the
// transport layer. 403 would confirm the workspace exists.
func TestNonMemberGetsNotFoundEverywhere(t *testing.T) {
	for _, endpoint := range endpoints {
		t.Run(endpoint.name, func(t *testing.T) {
			rec := serveAs(t, domain.RoleOwner, &outsiderID, endpoint.method, endpoint.path, endpoint.body)

			if rec.Code != http.StatusNotFound {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusNotFound)
			}

			var body httpx.ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body.Code == httpx.CodePermissionDenied {
				t.Error("a non-member received permission_denied, confirming the workspace exists")
			}
			if body.Code != httpx.CodeNotFound {
				t.Errorf("code = %q, want %q", body.Code, httpx.CodeNotFound)
			}
		})
	}
}

// TestUnknownWorkspaceIsIndistinguishableFromForbidden covers the other half:
// a workspace that does not exist answers exactly as one that is not yours.
func TestUnknownWorkspaceIsIndistinguishableFromForbidden(t *testing.T) {
	unknown := serveAs(t, domain.RoleOwner, &memberID, http.MethodGet,
		"/v1/workspaces/"+workspaceB.String(), "")
	notMine := serveAs(t, domain.RoleOwner, &outsiderID, http.MethodGet,
		"/v1/workspaces/"+workspaceA.String(), "")

	if unknown.Code != notMine.Code {
		t.Errorf("unknown workspace = %d, not-a-member = %d; these must match",
			unknown.Code, notMine.Code)
	}

	// Also true for a malformed identifier, so the shape of a real one leaks
	// nothing either.
	malformed := serveAs(t, domain.RoleOwner, &memberID, http.MethodGet, "/v1/workspaces/not-a-uuid", "")
	if malformed.Code != http.StatusNotFound {
		t.Errorf("malformed id = %d, want %d", malformed.Code, http.StatusNotFound)
	}
}

func TestUnauthenticatedIsRejectedEverywhere(t *testing.T) {
	all := append([]struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"create", http.MethodPost, "/v1/workspaces", `{"name":"New"}`},
		{"list", http.MethodGet, "/v1/workspaces", ""},
	}, endpoints...)

	for _, endpoint := range all {
		t.Run(endpoint.name, func(t *testing.T) {
			rec := serveAs(t, domain.RoleOwner, nil, endpoint.method, endpoint.path, endpoint.body)
			if rec.Code == http.StatusOK || rec.Code == http.StatusCreated || rec.Code == http.StatusNoContent {
				t.Errorf("unauthenticated request succeeded with %d", rec.Code)
			}
		})
	}
}

// TestRolePermissionsAtTheEdge checks the matrix is actually consulted by the
// handlers, not merely present in the domain.
func TestRolePermissionsAtTheEdge(t *testing.T) {
	management := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{"rename workspace", http.MethodPatch, "/v1/workspaces/" + workspaceA.String(), `{"name":"New","version":3}`},
		{"change role", http.MethodPatch, "/v1/workspaces/" + workspaceA.String() + "/members/" + targetID.String(), `{"role":"admin"}`},
		{"remove member", http.MethodDelete, "/v1/workspaces/" + workspaceA.String() + "/members/" + targetID.String(), ""},
	}

	tests := []struct {
		role         domain.Role
		wantAllowed  bool
		wantReadable bool
	}{
		{domain.RoleOwner, true, true},
		{domain.RoleAdmin, true, true},
		{domain.RoleDeveloper, false, true},
		{domain.RoleViewer, false, true},
	}

	for _, tt := range tests {
		for _, endpoint := range management {
			t.Run(string(tt.role)+"/"+endpoint.name, func(t *testing.T) {
				rec := serveAs(t, tt.role, &memberID, endpoint.method, endpoint.path, endpoint.body)

				if tt.wantAllowed {
					if rec.Code >= 400 {
						t.Errorf("%s was refused with %d: %s", tt.role, rec.Code, rec.Body.String())
					}
					return
				}

				if rec.Code != http.StatusForbidden {
					t.Errorf("%s got %d, want %d", tt.role, rec.Code, http.StatusForbidden)
				}
				// A member who lacks a permission gets 403, not 404: they can
				// see the workspace, so hiding it would be misleading.
				var body httpx.ErrorBody
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if body.Code != httpx.CodePermissionDenied {
					t.Errorf("code = %q, want %q", body.Code, httpx.CodePermissionDenied)
				}
			})
		}

		// Every role can read the workspace it belongs to.
		t.Run(string(tt.role)+"/read", func(t *testing.T) {
			rec := serveAs(t, tt.role, &memberID, http.MethodGet, "/v1/workspaces/"+workspaceA.String(), "")
			if rec.Code != http.StatusOK {
				t.Errorf("%s cannot read its own workspace: %d", tt.role, rec.Code)
			}
		})
	}
}

// TestMemberMayRemoveThemselves covers the one case where member:manage is not
// required.
func TestMemberMayRemoveThemselves(t *testing.T) {
	rec := serveAs(t, domain.RoleViewer, &memberID, http.MethodDelete,
		"/v1/workspaces/"+workspaceA.String()+"/members/"+memberID.String(), "")
	if rec.Code != http.StatusNoContent {
		t.Errorf("a viewer removing themselves got %d, want %d", rec.Code, http.StatusNoContent)
	}
}

func TestRenameRequiresVersion(t *testing.T) {
	rec := serveAs(t, domain.RoleOwner, &memberID, http.MethodPatch,
		"/v1/workspaces/"+workspaceA.String(), `{"name":"New"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("rename without a version got %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestInvalidInputIsRejected(t *testing.T) {
	tests := []struct {
		name string
		path string
		body string
	}{
		{"unknown role", "/v1/workspaces/" + workspaceA.String() + "/members/" + targetID.String(), `{"role":"superuser"}`},
		{"unknown field", "/v1/workspaces/" + workspaceA.String() + "/members/" + targetID.String(), `{"role":"admin","escalate":true}`},
		{"malformed json", "/v1/workspaces/" + workspaceA.String() + "/members/" + targetID.String(), `{`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := serveAs(t, domain.RoleOwner, &memberID, http.MethodPatch, tt.path, tt.body)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d (%s)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
		})
	}
}

// TestResponseAdvertisesPermissions checks a client can hide what the caller
// cannot do without reimplementing the matrix.
func TestResponseAdvertisesPermissions(t *testing.T) {
	rec := serveAs(t, domain.RoleViewer, &memberID, http.MethodGet, "/v1/workspaces/"+workspaceA.String(), "")

	var body workspaceResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Role != string(domain.RoleViewer) {
		t.Errorf("role = %q, want viewer", body.Role)
	}
	if len(body.Permissions) != 1 || body.Permissions[0] != string(domain.PermissionWorkspaceRead) {
		t.Errorf("permissions = %v, want [workspace:read]", body.Permissions)
	}
	if body.Version != 3 {
		t.Errorf("version = %d, want 3 — clients need it to send back", body.Version)
	}
}

// TestAdminCannotGrantOwnerOverHTTP covers the privilege-escalation finding at
// the transport layer: member:manage alone must not let an admin mint an owner.
func TestAdminCannotGrantOwnerOverHTTP(t *testing.T) {
	path := "/v1/workspaces/" + workspaceA.String() + "/members/" + targetID.String()

	rec := serveAs(t, domain.RoleAdmin, &memberID, http.MethodPatch, path, `{"role":"owner"}`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("admin granting owner got %d, want %d: %s", rec.Code, http.StatusForbidden, rec.Body.String())
	}

	// An owner may still do it, so the guard is about authority, not the role.
	rec = serveAs(t, domain.RoleOwner, &memberID, http.MethodPatch, path, `{"role":"owner"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("owner granting owner got %d, want %d", rec.Code, http.StatusOK)
	}

	// And an admin may still grant roles at or below their own authority.
	rec = serveAs(t, domain.RoleAdmin, &memberID, http.MethodPatch, path, `{"role":"developer"}`)
	if rec.Code != http.StatusOK {
		t.Errorf("admin granting developer got %d, want %d", rec.Code, http.StatusOK)
	}
}

// TestOversizedVersionDoesNotBypassConcurrency covers the int32 truncation
// path at the transport layer.
func TestOversizedVersionDoesNotBypassConcurrency(t *testing.T) {
	// The fake workspace is at version 3; int32(4294967299) is also 3.
	rec := serveAs(t, domain.RoleOwner, &memberID, http.MethodPatch,
		"/v1/workspaces/"+workspaceA.String(), `{"name":"Hijacked","version":4294967299}`)

	if rec.Code != http.StatusConflict {
		t.Errorf("oversized version got %d, want %d: %s", rec.Code, http.StatusConflict, rec.Body.String())
	}

	for _, version := range []string{"0", "-1"} {
		rec := serveAs(t, domain.RoleOwner, &memberID, http.MethodPatch,
			"/v1/workspaces/"+workspaceA.String(), `{"name":"X","version":`+version+`}`)
		if rec.Code != http.StatusConflict {
			t.Errorf("version %s got %d, want %d", version, rec.Code, http.StatusConflict)
		}
	}
}

// TestTrailingJSONIsRejected covers a body carrying a second JSON value after
// the first, which Decode alone silently ignores.
func TestTrailingJSONIsRejected(t *testing.T) {
	rec := serveAs(t, domain.RoleOwner, &memberID, http.MethodPatch,
		"/v1/workspaces/"+workspaceA.String(),
		`{"name":"First","version":3}{"name":"Second","version":3}`)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("trailing JSON got %d, want %d: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestOversizedBodyIsRejected covers the limit boundary: io.LimitReader would
// present the cutoff as a clean EOF and decode whatever fitted.
func TestOversizedBodyIsRejected(t *testing.T) {
	padding := strings.Repeat("a", maxBodyBytes)
	rec := serveAs(t, domain.RoleOwner, &memberID, http.MethodPatch,
		"/v1/workspaces/"+workspaceA.String(),
		`{"version":3,"name":"`+padding+`"}`)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("oversized body got %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
