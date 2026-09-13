package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jigmetnamgyal/weave/internal/adapters/github"
)

// writeKey generates a throwaway RSA key in the requested PEM encoding.
//
// Generated rather than checked in, so no test fixture is ever mistaken for a
// real credential.
func writeKey(t *testing.T, encoding string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	var block *pem.Block
	switch encoding {
	case "pkcs1":
		block = &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	case "pkcs8":
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatalf("marshal pkcs8: %v", err)
		}
		block = &pem.Block{Type: "PRIVATE KEY", Bytes: der}
	default:
		t.Fatalf("unknown encoding %q", encoding)
	}

	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return path
}

// memoryCache is a TokenCache that records what it was asked to store, so a
// test can assert on the caching behaviour rather than only its effect.
type memoryCache struct {
	mu     sync.Mutex
	values map[string]string
	ttls   map[string]time.Duration
	writes int
}

func newMemoryCache() *memoryCache {
	return &memoryCache{values: map[string]string{}, ttls: map[string]time.Duration{}}
}

func (c *memoryCache) Get(_ context.Context, key string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[key], nil
}

func (c *memoryCache) Set(_ context.Context, key, token string, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.values[key] = token
	c.ttls[key] = ttl
	c.writes++
	return nil
}

// TestNewClientAcceptsBothKeyEncodings covers a real trap.
//
// GitHub has issued PKCS#1 keys historically and PKCS#8 more recently.
// Accepting only one would reject a perfectly good key with an error that says
// nothing about which format was expected.
func TestNewClientAcceptsBothKeyEncodings(t *testing.T) {
	for _, encoding := range []string{"pkcs1", "pkcs8"} {
		t.Run(encoding, func(t *testing.T) {
			if _, err := github.NewClient("123", writeKey(t, encoding), nil); err != nil {
				t.Errorf("NewClient rejected a %s key: %v", encoding, err)
			}
		})
	}
}

func TestNewClientRejectsBadConfiguration(t *testing.T) {
	valid := writeKey(t, "pkcs1")

	t.Run("missing app id", func(t *testing.T) {
		if _, err := github.NewClient("", valid, nil); err == nil {
			t.Error("accepted an empty app id")
		}
	})

	// The App id becomes the JWT `iss` claim. A non-numeric value is rejected
	// here rather than at GitHub, where it surfaces as an opaque 401 that
	// looks like a wrong key.
	t.Run("non-numeric app id", func(t *testing.T) {
		if _, err := github.NewClient("weave-app", valid, nil); err == nil {
			t.Error("accepted a non-numeric app id")
		}
	})

	t.Run("missing key file", func(t *testing.T) {
		if _, err := github.NewClient("123", filepath.Join(t.TempDir(), "absent.pem"), nil); err == nil {
			t.Error("accepted a missing key file")
		}
	})

	t.Run("key that is not PEM", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "junk.pem")
		if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		if _, err := github.NewClient("123", path, nil); err == nil {
			t.Error("accepted a file that is not PEM")
		}
	})
}

// TestAppJWTIsWellFormed inspects the token the client actually sends.
//
// Faking GitHub at the HTTP boundary is what makes this possible: the
// Authorization header is an output of the code under test, so its claims can
// be asserted. An interface mock would have skipped the part most likely to be
// wrong.
func TestAppJWTIsWellFormed(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"id":123,"slug":"weave-test"}`))
	}))
	defer server.Close()

	client, err := github.NewClient("123", writeKey(t, "pkcs1"), nil, github.WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.App(context.Background()); err != nil {
		t.Fatalf("App: %v", err)
	}

	token, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok {
		t.Fatalf("Authorization = %q, want a bearer token", authorization)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments, want 3", len(parts))
	}

	var claims struct {
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
		Iss string `json:"iss"`
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}

	if claims.Iss != "123" {
		t.Errorf("iss = %q, want the app id", claims.Iss)
	}
	// GitHub rejects a JWT whose iat is in the future by its own clock, so the
	// client backdates. A machine running slightly fast would otherwise mint
	// tokens that are always refused.
	if claims.Iat > time.Now().Unix() {
		t.Errorf("iat is in the future, so ordinary clock skew would break every call")
	}
	// GitHub refuses anything over ten minutes.
	if window := claims.Exp - claims.Iat; window > 600 {
		t.Errorf("token is valid for %ds, which GitHub refuses (max 600)", window)
	}
}

// TestInstallationTokenIsCached pins that a token is minted once and reused,
// and that it is never cached without an expiry.
func TestInstallationTokenIsCached(t *testing.T) {
	var minted int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		minted++
		expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"token":"ghs_installation_token","expires_at":%q}`, expires)
	}))
	defer server.Close()

	cache := newMemoryCache()
	client, err := github.NewClient("123", writeKey(t, "pkcs1"), cache, github.WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		token, err := client.InstallationToken(ctx, 42)
		if err != nil {
			t.Fatalf("InstallationToken: %v", err)
		}
		if token != "ghs_installation_token" {
			t.Fatalf("token = %q", token)
		}
	}

	if minted != 1 {
		t.Errorf("minted %d tokens for 3 calls, want 1", minted)
	}
	if cache.writes != 1 {
		t.Errorf("wrote the cache %d times, want 1", cache.writes)
	}
	for key, ttl := range cache.ttls {
		if ttl <= 0 {
			t.Errorf("cached %q with ttl %v: an installation token must always expire", key, ttl)
		}
		if ttl >= time.Hour {
			t.Errorf("cached %q for %v, which is not inside GitHub's own hour", key, ttl)
		}
	}
}

// TestInstallationTokenSurvivesACacheOutage pins that Redis being down makes
// GitHub calls slower rather than impossible.
func TestInstallationTokenSurvivesACacheOutage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		fmt.Fprintf(w, `{"token":"ghs_token","expires_at":%q}`, expires)
	}))
	defer server.Close()

	client, err := github.NewClient("123", writeKey(t, "pkcs1"), failingCache{}, github.WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.InstallationToken(context.Background(), 42); err != nil {
		t.Errorf("a cache outage stopped a token being minted: %v", err)
	}
}

type failingCache struct{}

func (failingCache) Get(context.Context, string) (string, error) {
	return "", errors.New("cache is down")
}
func (failingCache) Set(context.Context, string, string, time.Duration) error {
	return errors.New("cache is down")
}

// TestStatusMapping checks that GitHub's status codes become errors callers
// can act on, rather than one opaque failure.
func TestStatusMapping(t *testing.T) {
	tests := map[string]struct {
		status int
		body   string
		want   error
	}{
		"404 is not found":            {http.StatusNotFound, `{"message":"Not Found"}`, github.ErrNotFound},
		"401 is rejected credentials": {http.StatusUnauthorized, `{"message":"Bad credentials"}`, github.ErrUnauthorized},
		"403 naming suspension":       {http.StatusForbidden, `{"message":"This installation has been suspended"}`, github.ErrSuspended},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			client, err := github.NewClient("123", writeKey(t, "pkcs1"), nil, github.WithBaseURL(server.URL))
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			if _, err := client.Installation(context.Background(), 42); !errors.Is(err, tt.want) {
				t.Errorf("Installation error = %v, want %v", err, tt.want)
			}
		})
	}
}

// TestInstallationRepositoriesPaginates covers an account with more
// repositories than one page holds.
func TestInstallationRepositoriesPaginates(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			expires := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			fmt.Fprintf(w, `{"token":"ghs_token","expires_at":%q}`, expires)
			return
		}

		page := r.URL.Query().Get("page")
		switch page {
		case "1":
			// A full page, which is what tells the client to ask for another.
			repositories := make([]string, 0, 100)
			for i := 0; i < 100; i++ {
				repositories = append(repositories, fmt.Sprintf(
					`{"id":%d,"name":"repo%d","owner":{"login":"acme"},"default_branch":"main","private":true}`, i+1, i+1))
			}
			fmt.Fprintf(w, `{"total_count":101,"repositories":[%s]}`, strings.Join(repositories, ","))
		case "2":
			fmt.Fprint(w, `{"total_count":101,"repositories":[{"id":101,"name":"last","owner":{"login":"acme"},"default_branch":"trunk","private":false}]}`)
		default:
			t.Errorf("asked for page %q, which should not have been requested", page)
			fmt.Fprint(w, `{"total_count":101,"repositories":[]}`)
		}
	}))
	defer server.Close()

	client, err := github.NewClient("123", writeKey(t, "pkcs1"), nil, github.WithBaseURL(server.URL))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	repositories, err := client.InstallationRepositories(context.Background(), 42)
	if err != nil {
		t.Fatalf("InstallationRepositories: %v", err)
	}
	if len(repositories) != 101 {
		t.Fatalf("got %d repositories, want 101 across two pages", len(repositories))
	}
	if repositories[100].DefaultBranch != "trunk" {
		t.Errorf("the second page was not merged: last default branch = %q", repositories[100].DefaultBranch)
	}
}
