package workspaces

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

// fakeEgress simulates transaction authorization and completion; DB atomicity is
// covered separately by PostgreSQL integration tests.
type fakeEgress struct {
	keys       *fakeKeys
	entries    []domain.EgressHost
	denyReplay bool
	calls      int
}

func (f *fakeEgress) Authorize(context.Context, uuid.UUID, application.Actor) error {
	if f.denyReplay {
		return application.ErrPermissionDenied
	}
	return nil
}
func (f *fakeEgress) List(context.Context, uuid.UUID, application.Actor) ([]domain.EgressHost, error) {
	return f.entries, nil
}
func (f *fakeEgress) Add(_ context.Context, h domain.EgressHost, _ application.Actor, _ application.AuditEvent, c *application.EgressCompletion) (domain.EgressHost, error) {
	f.calls++
	h.CreatedAt = time.Unix(0, 0).UTC()
	f.entries = append(f.entries, h)
	if c != nil {
		s, b, e := c.Render(h)
		if e != nil {
			return h, e
		}
		f.keys.complete(c.Claim.Scope, c.Claim.Key, s, b, c.Claim.OriginRequestID)
	}
	return h, nil
}
func (f *fakeEgress) Remove(_ context.Context, _ uuid.UUID, id uuid.UUID, _ application.Actor, _ application.AuditEvent, c *application.EgressCompletion) error {
	f.calls++
	if c != nil {
		s, b, e := c.Render(domain.EgressHost{ID: id})
		if e != nil {
			return e
		}
		f.keys.complete(c.Claim.Scope, c.Claim.Key, s, b, c.Claim.OriginRequestID)
	}
	return nil
}
func (f *fakeEgress) Snapshot(context.Context, uuid.UUID, uuid.UUID) (domain.RunnerEgressSnapshot, error) {
	return domain.RunnerEgressSnapshot{}, nil
}

// newEgressMux builds real handlers around fake storage and idempotency seams.
func newEgressMux(t *testing.T, role domain.Role, store *fakeEgress) http.Handler {
	t.Helper()
	handler := NewHandler(application.NewWorkspaceService(&fakeStore{role: role}), discardLogger())
	mux := http.NewServeMux()
	handler.Register(mux)
	service, err := application.NewEgressService(store, []string{"weave.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	handler.RegisterEgress(mux, service, store.keys)
	return httpx.WithRequestID(mux)
}

// TestEgressRoutesRequireWorkspaceManage keeps non-admin roles from reading or
// changing policy and ensures failed validation happens before DNS or storage.
func TestEgressRoutesRequireWorkspaceManage(t *testing.T) {
	path := "/v1/workspaces/" + workspaceA.String() + "/egress-hosts"
	for _, role := range []domain.Role{domain.RoleDeveloper, domain.RoleViewer} {
		for _, method := range []string{"GET", "POST"} {
			store := &fakeEgress{keys: &fakeKeys{}}
			rec, _ := serveSessionWith(t, newEgressMux(t, role, store), method, path, `{"hostname":"docs.example.com"}`, nil)
			if rec.Code != 403 || store.calls != 0 {
				t.Errorf("%s %s: %d calls=%d", role, method, rec.Code, store.calls)
			}
		}
	}
	for _, host := range []string{"api.weave.example.com", "https://docs.example.com", "127.0.0.1"} {
		store := &fakeEgress{keys: &fakeKeys{}}
		rec, _ := serveSessionWith(t, newEgressMux(t, domain.RoleAdmin, store), "POST", path, `{"hostname":"`+host+`"}`, nil)
		if rec.Code != 400 || store.calls != 0 {
			t.Errorf("%s accepted: %d", host, rec.Code)
		}
	}
}

// TestEgressReplayRequiresCurrentAuthority prevents a completed response from
// being disclosed or treated as a successful mutation after demotion/removal.
func TestEgressReplayRequiresCurrentAuthority(t *testing.T) {
	keys := &fakeKeys{}
	store := &fakeEgress{keys: keys}
	mux := newEgressMux(t, domain.RoleOwner, store)
	path := "/v1/workspaces/" + workspaceA.String() + "/egress-hosts"
	headers := map[string]string{"Idempotency-Key": "same"}
	first, _ := serveSessionWith(t, mux, "POST", path, `{"hostname":"DOCS.example.com"}`, headers)
	if first.Code != 201 {
		t.Fatal(first.Body.String())
	}
	replay, _ := serveSessionWith(t, mux, "POST", path, `{"hostname":"docs.example.com"}`, headers)
	if replay.Code != 201 || replay.Body.String() != first.Body.String() || store.calls != 1 {
		t.Fatalf("canonical replay: %d calls=%d", replay.Code, store.calls)
	}
	store.denyReplay = true
	denied, _ := serveSessionWith(t, mux, "POST", path, `{"hostname":"docs.example.com"}`, headers)
	if denied.Code != 403 || strings.Contains(denied.Body.String(), "docs.example.com") {
		t.Fatalf("unauthorized replay: %d %s", denied.Code, denied.Body.String())
	}
	mismatch, _ := serveSessionWith(t, mux, "POST", path, `{"hostname":"other.example.com"}`, headers)
	if mismatch.Code != 403 {
		t.Fatalf("authorization must precede mismatch disclosure: %d", mismatch.Code)
	}
}

// TestEgressDeleteReplayHasNoBody exercises the special 204 replay path.
func TestEgressDeleteReplayHasNoBody(t *testing.T) {
	keys := &fakeKeys{}
	store := &fakeEgress{keys: keys}
	mux := newEgressMux(t, domain.RoleOwner, store)
	id := uuid.New()
	path := "/v1/workspaces/" + workspaceA.String() + "/egress-hosts/" + id.String()
	headers := map[string]string{"Idempotency-Key": "remove"}
	for i := 0; i < 2; i++ {
		rec, _ := serveSessionWith(t, mux, "DELETE", path, "", headers)
		if rec.Code != 204 || rec.Body.Len() != 0 || rec.Header().Get("Content-Type") != "" {
			t.Fatalf("attempt %d: %d %q %q", i, rec.Code, rec.Body.String(), rec.Header().Get("Content-Type"))
		}
	}
	if store.calls != 1 {
		t.Fatalf("delete executed %d times", store.calls)
	}
}
