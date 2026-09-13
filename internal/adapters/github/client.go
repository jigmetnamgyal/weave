// Package github talks to the GitHub REST API as a GitHub App.
//
// Two credentials, and the distinction matters. The *App JWT* is signed with
// the App's private key and authenticates as the App itself; it can read the
// App's own record and mint installation tokens, and nothing else. An
// *installation token* is what actually reaches a customer's repositories, is
// issued per installation, and expires after an hour.
//
// The private key is the root credential for every installation. It is read
// once at startup from a path and held in memory; it is never written to the
// database, never logged, and never crosses a process boundary.
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// apiBase is GitHub's REST endpoint. A field on Client rather than a constant
// so tests can point it at an httptest server — the signature handling and
// error mapping are most of what is worth testing, and an interface mock would
// skip both.
const apiBase = "https://api.github.com"

// Errors this package raises.
var (
	// ErrNotFound is returned when GitHub says the installation or repository
	// does not exist, or exists but is not visible to this App. The two are
	// deliberately indistinguishable here, because GitHub does not reliably
	// distinguish them either.
	ErrNotFound = errors.New("github: not found")
	// ErrSuspended is returned when GitHub refuses because the installation is
	// suspended.
	ErrSuspended = errors.New("github: installation suspended")
	// ErrUnauthorized is returned when GitHub rejects our credentials, which
	// almost always means a wrong App id or a key that is not the App's.
	ErrUnauthorized = errors.New("github: credentials rejected")
)

// appJWTLifetime is how long a minted App JWT is valid.
//
// GitHub refuses anything over ten minutes. Two is plenty for a single call
// and keeps a leaked token useless quickly.
const appJWTLifetime = 2 * time.Minute

// appJWTBackdate offsets `iat` into the past.
//
// GitHub rejects a JWT whose `iat` is in the future by its own clock, so a
// machine running slightly fast would mint tokens that are refused. Thirty
// seconds absorbs ordinary skew.
const appJWTBackdate = 30 * time.Second

// TokenCache stores installation tokens for their lifetime.
//
// An interface rather than Redis directly, so the client can be tested without
// one, and so the storage decision stays in the composition root. Whatever
// implements it must expire entries: installation tokens are credentials that
// reach customer repositories, and a cache that kept them indefinitely would
// be a store of live credentials in something chosen for speed rather than
// durability.
type TokenCache interface {
	// Get returns the cached token, or "" when absent.
	Get(ctx context.Context, key string) (string, error)
	// Set stores a token under a time to live.
	Set(ctx context.Context, key, token string, ttl time.Duration) error
}

// Client is a GitHub App API client.
type Client struct {
	appID      string
	privateKey *rsa.PrivateKey
	http       *http.Client
	cache      TokenCache
	baseURL    string
	now        func() time.Time
}

// Option adjusts a Client. Used by tests to redirect the base URL and fix the
// clock; production uses the defaults.
type Option func(*Client)

// WithBaseURL points the client at a different API host.
func WithBaseURL(url string) Option {
	return func(c *Client) { c.baseURL = strings.TrimSuffix(url, "/") }
}

// WithHTTPClient replaces the HTTP client.
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithClock replaces the time source.
func WithClock(now func() time.Time) Option { return func(c *Client) { c.now = now } }

// NewClient reads the App private key and returns a client.
//
// The key is read here, at startup, rather than on first use. A missing or
// malformed key is a misconfiguration, and the moment to discover it is
// deployment — not the first time someone tries to connect a repository.
func NewClient(appID, privateKeyPath string, cache TokenCache, opts ...Option) (*Client, error) {
	if strings.TrimSpace(appID) == "" {
		return nil, errors.New("github: app id is required")
	}
	if _, err := strconv.ParseInt(appID, 10, 64); err != nil {
		return nil, fmt.Errorf("github: app id must be numeric, got %q", appID)
	}

	raw, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("github: read private key: %w", err)
	}
	key, err := parsePrivateKey(raw)
	if err != nil {
		return nil, err
	}

	client := &Client{
		appID:      appID,
		privateKey: key,
		http:       &http.Client{Timeout: 15 * time.Second},
		cache:      cache,
		baseURL:    apiBase,
		now:        time.Now,
	}
	for _, opt := range opts {
		opt(client)
	}
	return client, nil
}

// parsePrivateKey accepts both PEM encodings GitHub has issued over time.
//
// Older downloads are PKCS#1 ("BEGIN RSA PRIVATE KEY"), newer ones PKCS#8
// ("BEGIN PRIVATE KEY"). Accepting only one would reject a perfectly good key
// with an error that says nothing about which format was expected.
func parsePrivateKey(raw []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("github: private key is not PEM encoded")
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github: private key is neither PKCS#1 nor PKCS#8: %w", err)
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("github: private key is %T, want RSA", parsed)
	}
	return key, nil
}

// base64url encodes without padding, as JWT requires.
func base64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// appJWT mints a short-lived token authenticating as the App itself.
func (c *Client) appJWT() (string, error) {
	now := c.now()
	header := base64url([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{
		"iat": now.Add(-appJWTBackdate).Unix(),
		"exp": now.Add(appJWTLifetime).Unix(),
		"iss": c.appID,
	})
	if err != nil {
		return "", fmt.Errorf("github: encode jwt claims: %w", err)
	}

	signing := header + "." + base64url(claims)
	digest := sha256.Sum256([]byte(signing))
	signature, err := rsa.SignPKCS1v15(rand.Reader, c.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("github: sign jwt: %w", err)
	}
	return signing + "." + base64url(signature), nil
}

// do issues a request and maps GitHub's status codes onto this package's
// errors. The body is returned for the caller to decode.
func (c *Client) do(ctx context.Context, method, path, auth string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return nil, fmt.Errorf("github: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("github: read response: %w", err)
	}

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return payload, nil
	case resp.StatusCode == http.StatusUnauthorized:
		return nil, ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		return nil, ErrNotFound
	case resp.StatusCode == http.StatusForbidden && bytes.Contains(payload, []byte("suspended")):
		return nil, ErrSuspended
	default:
		// The body is included because GitHub's messages are specific and
		// usually name the exact permission or resource at fault. It is
		// truncated because a 8MB error in a log line helps nobody.
		return nil, fmt.Errorf("github: %s %s returned %d: %s",
			method, path, resp.StatusCode, truncate(string(payload), 512))
	}
}

func truncate(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// App describes the App itself, used to verify credentials at startup.
type App struct {
	ID     int64    `json:"id"`
	Slug   string   `json:"slug"`
	Name   string   `json:"name"`
	Events []string `json:"events"`
}

// App reads the App's own record.
//
// Used as a startup credential check: it is the cheapest call that proves the
// App id and the private key are a matching pair, which is otherwise only
// discovered when a user tries to install.
func (c *Client) App(ctx context.Context) (App, error) {
	token, err := c.appJWT()
	if err != nil {
		return App{}, err
	}
	body, err := c.do(ctx, http.MethodGet, "/app", token, nil)
	if err != nil {
		return App{}, err
	}
	var app App
	if err := json.Unmarshal(body, &app); err != nil {
		return App{}, fmt.Errorf("github: decode app: %w", err)
	}
	return app, nil
}

// InstallationAccount is what GitHub reports about an installation.
type InstallationAccount struct {
	ID                  int64             `json:"id"`
	RepositorySelection string            `json:"repository_selection"`
	Permissions         map[string]string `json:"permissions"`
	Events              []string          `json:"events"`
	SuspendedAt         *time.Time        `json:"suspended_at"`
	Account             struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
}

// Installation reads one installation as the App.
//
// This is what turns a claimed installation id from a redirect into a fact.
// The redirect is attacker-controllable; this response is not.
func (c *Client) Installation(ctx context.Context, installationID int64) (InstallationAccount, error) {
	token, err := c.appJWT()
	if err != nil {
		return InstallationAccount{}, err
	}
	path := "/app/installations/" + strconv.FormatInt(installationID, 10)
	body, err := c.do(ctx, http.MethodGet, path, token, nil)
	if err != nil {
		return InstallationAccount{}, err
	}
	var installation InstallationAccount
	if err := json.Unmarshal(body, &installation); err != nil {
		return InstallationAccount{}, fmt.Errorf("github: decode installation: %w", err)
	}
	return installation, nil
}

// installationTokenTTL is how long a minted installation token is cached.
//
// GitHub issues them for an hour. Caching for fifty minutes leaves ten
// minutes of margin, so a token taken from the cache cannot expire mid-request
// because of clock differences or a slow call.
const installationTokenTTL = 50 * time.Minute

// InstallationToken returns a token that can reach the installation's
// repositories, minting one if the cache has none.
//
// Cached in Redis rather than stored in PostgreSQL, deliberately. These are
// live credentials for customer source code with a one-hour life; a durable
// store would keep them long past their usefulness and turn a database backup
// into a set of working repository credentials.
func (c *Client) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	key := "github:installation-token:" + strconv.FormatInt(installationID, 10)

	if c.cache != nil {
		cached, err := c.cache.Get(ctx, key)
		if err == nil && cached != "" {
			return cached, nil
		}
		// A cache miss and a cache failure are both survivable: mint a fresh
		// token. Redis being down should slow GitHub calls, not stop them.
	}

	appToken, err := c.appJWT()
	if err != nil {
		return "", err
	}
	path := "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	body, err := c.do(ctx, http.MethodPost, path, appToken, nil)
	if err != nil {
		return "", err
	}

	var minted struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &minted); err != nil {
		return "", fmt.Errorf("github: decode installation token: %w", err)
	}
	if minted.Token == "" {
		return "", errors.New("github: installation token response carried no token")
	}

	if c.cache != nil {
		ttl := installationTokenTTL
		if until := time.Until(minted.ExpiresAt) - 10*time.Minute; until > 0 && until < ttl {
			ttl = until
		}
		// A cache write failure is not worth failing the request over: the
		// token in hand is valid, and the next call simply mints another.
		_ = c.cache.Set(ctx, key, minted.Token, ttl)
	}
	return minted.Token, nil
}

// RemoteRepository is a repository as GitHub describes it.
type RemoteRepository struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	FullName      string `json:"full_name"`
	Private       bool   `json:"private"`
	DefaultBranch string `json:"default_branch"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// repositoryPageSize is GitHub's maximum, used to keep the number of round
// trips down on accounts with many repositories.
const repositoryPageSize = 100

// maxRepositoryPages bounds the pagination loop.
//
// A bound rather than trusting the end condition: this loop is driven by a
// remote service, and an unbounded loop driven by someone else's responses is
// how a request hangs forever. Ten thousand repositories is far past any real
// installation.
const maxRepositoryPages = 100

// InstallationRepositories lists every repository the installation grants.
//
// This is the reconciliation source of truth. Webhooks say what changed;
// this says what is true now, which is what the product must act on.
func (c *Client) InstallationRepositories(ctx context.Context, installationID int64) ([]RemoteRepository, error) {
	token, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}

	var all []RemoteRepository
	for page := 1; page <= maxRepositoryPages; page++ {
		path := fmt.Sprintf("/installation/repositories?per_page=%d&page=%d", repositoryPageSize, page)
		body, err := c.do(ctx, http.MethodGet, path, token, nil)
		if err != nil {
			return nil, err
		}
		var response struct {
			TotalCount   int                `json:"total_count"`
			Repositories []RemoteRepository `json:"repositories"`
		}
		if err := json.Unmarshal(body, &response); err != nil {
			return nil, fmt.Errorf("github: decode repositories: %w", err)
		}
		all = append(all, response.Repositories...)
		if len(response.Repositories) < repositoryPageSize {
			return all, nil
		}
	}
	return nil, fmt.Errorf("github: repository listing exceeded %d pages", maxRepositoryPages)
}
