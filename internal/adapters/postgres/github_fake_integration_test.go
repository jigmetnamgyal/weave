package postgres_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	githubadapter "github.com/jigmetnamgyal/weave/internal/adapters/github"
	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// baseSHA is the commit every fake repository's default branch points at. A
// real forty-character SHA, because sessions.branch_sha checks the shape.
const baseSHA = "5f1c0a9e3b7d2c4f6a8e0b1d3c5e7f9a1b3c5d7e"

// fakeGitHubServer is GitHub at the HTTP boundary, as in M3.3.
//
// Faked here rather than behind the application's port because the token
// minting, the 422 translation and the not-found mapping live in the real
// client, and those are most of what can go wrong between a workflow and a
// ref. An interface mock would skip all of them.
type fakeGitHubServer struct {
	server *httptest.Server

	mu sync.Mutex
	// granted lists, per installation, the repositories GitHub says it grants.
	granted map[int64][]fakeRepository
	// branches maps "owner/name" to branch name to SHA.
	branches map[string]map[string]string
	// refsCreated counts successful POST /git/refs, per branch name.
	refsCreated map[string]int
	calls       int
}

type fakeRepository struct {
	GitHubID      int64
	Owner, Name   string
	DefaultBranch string
}

func newFakeGitHubServer(t *testing.T) *fakeGitHubServer {
	t.Helper()
	fake := &fakeGitHubServer{
		granted:     map[int64][]fakeRepository{},
		branches:    map[string]map[string]string{},
		refsCreated: map[string]int{},
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

// grant makes GitHub report the fixture's repository as granted to its
// installation, with its default branch at baseSHA.
func (f *fakeGitHubServer) grant(t *testing.T, pool *pgxpool.Pool, fixture sessionFixture) {
	t.Helper()
	var installationID int64
	if err := pool.QueryRow(context.Background(),
		`SELECT github_installation_id FROM github_installations WHERE id = $1`,
		fixture.repository.InstallationID).Scan(&installationID); err != nil {
		t.Fatalf("read installation github id: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	repository := fakeRepository{
		GitHubID: fixture.repository.GitHubID,
		Owner:    fixture.repository.Owner, Name: fixture.repository.Name,
		DefaultBranch: fixture.repository.DefaultBranch,
	}
	f.granted[installationID] = append(f.granted[installationID], repository)
	key := repository.Owner + "/" + repository.Name
	if f.branches[key] == nil {
		f.branches[key] = map[string]string{}
	}
	f.branches[key][repository.DefaultBranch] = baseSHA
}

func (f *fakeGitHubServer) created(branch string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refsCreated[branch]
}

func (f *fakeGitHubServer) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeGitHubServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++

	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && strings.HasSuffix(path, "/access_tokens"):
		// The token names its installation, so later calls can be answered
		// for the right one. Never logged, and synthetic.
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/app/installations/"), "/access_tokens")
		expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		writeJSON(w, http.StatusCreated, map[string]any{"token": "ghs_fake_" + id, "expires_at": expires})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/app/installations/"):
		id, _ := strconv.ParseInt(strings.TrimPrefix(path, "/app/installations/"), 10, 64)
		writeJSON(w, http.StatusOK, map[string]any{
			"id": id, "repository_selection": "selected",
			"permissions": domain.RequiredPermissions,
			"account":     map[string]string{"login": "acme", "type": "Organization"},
		})

	case r.Method == http.MethodGet && path == "/installation/repositories":
		id, _ := strconv.ParseInt(strings.TrimPrefix(
			strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), "ghs_fake_"), 10, 64)
		repositories := []map[string]any{}
		for _, repository := range f.granted[id] {
			repositories = append(repositories, map[string]any{
				"id": repository.GitHubID, "name": repository.Name,
				"full_name": repository.Owner + "/" + repository.Name, "private": true,
				"default_branch": repository.DefaultBranch,
				"owner":          map[string]string{"login": repository.Owner},
			})
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"total_count": len(repositories), "repositories": repositories,
		})

	case strings.HasPrefix(path, "/repos/"):
		f.serveRepository(w, r)

	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func (f *fakeGitHubServer) serveRepository(w http.ResponseWriter, r *http.Request) {
	parts := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/repos/"), "/", 3)
	if len(parts) < 3 {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	key, rest := parts[0]+"/"+parts[1], parts[2]
	branches, known := f.branches[key]
	if !known {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}

	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "branches/"):
		name := strings.TrimPrefix(rest, "branches/")
		sha, ok := branches[name]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Branch not found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"name": name, "protected": false, "commit": map[string]string{"sha": sha},
		})

	case r.Method == http.MethodGet && strings.HasPrefix(rest, "rules/branches/"):
		writeJSON(w, http.StatusOK, []any{})

	case r.Method == http.MethodPost && rest == "git/refs":
		var body struct{ Ref, SHA string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "bad body"})
			return
		}
		name := strings.TrimPrefix(body.Ref, "refs/heads/")
		if _, exists := branches[name]; exists {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "Reference already exists"})
			return
		}
		branches[name] = body.SHA
		f.refsCreated[name]++
		writeJSON(w, http.StatusCreated, map[string]any{
			"ref": body.Ref, "object": map[string]string{"sha": body.SHA},
		})

	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// installationServiceFor builds the real installation service against the
// fake, through the real GitHub client.
func installationServiceFor(t *testing.T, pool *pgxpool.Pool, fake *fakeGitHubServer) *application.InstallationService {
	t.Helper()
	client, err := githubadapter.NewClient("123", writeTestKey(t), nil, githubadapter.WithBaseURL(fake.server.URL))
	if err != nil {
		t.Fatalf("github client: %v", err)
	}
	return application.NewInstallationService(
		postgres.NewInstallationStore(pool), nil, githubadapter.NewPort(client),
		postgres.WithTenantWorkspace, "")
}

// writeTestKey writes a throwaway App private key. Synthetic, generated per
// test, and never a real credential.
func writeTestKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	path := filepath.Join(t.TempDir(), fmt.Sprintf("app-%s.pem", uuid.NewString()))
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path
}
