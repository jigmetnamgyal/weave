package github_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jigmetnamgyal/weave/internal/adapters/github"
)

// tokenAndThen answers the installation-token request, then delegates.
//
// Every repository call mints a token first, so a fake that does not serve
// that endpoint fails before reaching the behaviour under test.
func tokenAndThen(t *testing.T, next http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			fmt.Fprintf(w, `{"token":"ghs_token","expires_at":%q}`, expires)
			return
		}
		next(w, r)
	}))
}

func clientFor(t *testing.T, server *httptest.Server) *github.Client {
	t.Helper()
	client, err := github.NewClient("123", writeKey(t, "pkcs1"), nil, github.WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// TestCreateBranchTranslatesTheDuplicateRefusal is the behaviour invariant 5
// rests on.
//
// GitHub answers 422 both for a ref that already exists and for one it will
// not accept, with no distinct status or error code — only the message
// separates them. A retry finding its own earlier work must be told apart from
// a genuine rejection, because the caller's response to each is opposite.
func TestCreateBranchTranslatesTheDuplicateRefusal(t *testing.T) {
	t.Run("already exists", func(t *testing.T) {
		server := tokenAndThen(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Reference already exists"}`)
		})
		defer server.Close()

		_, err := clientFor(t, server).CreateBranch(context.Background(), 1, "acme", "app", "weave/x", "abc123")
		if !errors.Is(err, github.ErrRefExists) {
			t.Errorf("CreateBranch = %v, want ErrRefExists", err)
		}
	})

	// A different 422 must stay an error. Translating every unprocessable
	// request into "already exists" would report success for a ref GitHub
	// refused outright.
	t.Run("some other unprocessable request", func(t *testing.T) {
		server := tokenAndThen(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Object does not exist"}`)
		})
		defer server.Close()

		_, err := clientFor(t, server).CreateBranch(context.Background(), 1, "acme", "app", "weave/x", "abc123")
		if err == nil {
			t.Fatal("CreateBranch succeeded on a 422 that was not a duplicate")
		}
		if errors.Is(err, github.ErrRefExists) {
			t.Error("an unrelated 422 was translated into ErrRefExists")
		}
	})
}

// TestCreateBranchSendsAFullyQualifiedRef pins what is actually sent.
//
// GitHub takes the full `refs/heads/<name>` on creation and the bare name
// everywhere else. Sending the bare name here creates nothing and returns a
// confusing 422.
func TestCreateBranchSendsAFullyQualifiedRef(t *testing.T) {
	var body string
	server := tokenAndThen(t, func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		body = string(buf)
		fmt.Fprint(w, `{"ref":"refs/heads/weave/x","object":{"sha":"deadbeef"}}`)
	})
	defer server.Close()

	ref, err := clientFor(t, server).CreateBranch(context.Background(), 1, "acme", "app", "weave/x", "abc123")
	if err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if !strings.Contains(body, `"ref":"refs/heads/weave/x"`) {
		t.Errorf("sent %s, want a fully qualified refs/heads/ ref", body)
	}
	// And the bare name comes back, because everything downstream speaks in
	// branch names rather than refs.
	if ref.Name != "weave/x" {
		t.Errorf("Name = %q, want the bare branch name", ref.Name)
	}
}

// TestBranchPathsAreEscaped covers a branch name containing slashes.
//
// `weave/task-abc` is one path segment to GitHub and two to a URL parser.
// Escaping each component keeps the slashes meaningful while refusing anything
// that would change the path's shape.
func TestBranchPathsAreEscaped(t *testing.T) {
	var requested string
	server := tokenAndThen(t, func(w http.ResponseWriter, r *http.Request) {
		requested = r.URL.EscapedPath()
		fmt.Fprint(w, `{"name":"weave/task-abc","protected":false,"commit":{"sha":"abc"}}`)
	})
	defer server.Close()

	if _, err := clientFor(t, server).Branch(context.Background(), 1, "acme", "app", "weave/task-abc"); err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if !strings.HasSuffix(requested, "/branches/weave/task-abc") {
		t.Errorf("requested %q, want the slash preserved in the branch name", requested)
	}
}

// TestBranchReportsProtection pins that protection comes from GitHub rather
// than from a list of names we keep.
//
// Protection rules match patterns and rulesets, so a name-based guess would be
// wrong for exactly the repositories most likely to have rules at all.
func TestBranchReportsProtection(t *testing.T) {
	server := tokenAndThen(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"name":"main","protected":true,"commit":{"sha":"abc123"}}`)
	})
	defer server.Close()

	state, err := clientFor(t, server).Branch(context.Background(), 1, "acme", "app", "main")
	if err != nil {
		t.Fatalf("Branch: %v", err)
	}
	if !state.Protected {
		t.Error("Protected = false for a branch GitHub reports as protected")
	}
}

// TestBranchNotFoundIsNotAFailure: absence is the ordinary answer before
// creating a branch, and must be distinguishable from an error.
func TestBranchNotFoundIsNotAFailure(t *testing.T) {
	server := tokenAndThen(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Branch not found"}`)
	})
	defer server.Close()

	_, err := clientFor(t, server).Branch(context.Background(), 1, "acme", "app", "weave/absent")
	if !errors.Is(err, github.ErrNotFound) {
		t.Errorf("Branch = %v, want ErrNotFound", err)
	}
}
