package application

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Registry request recording (Unit M5.4c, ADR-016).
//
// Every package-registry request a runner makes on Vercel reaches the registry
// proxy, which records it here **before** forwarding it. The record is what
// makes ADR-013's registry side channel visible: a session fetching packages
// its repository names nowhere shows up in it.

// GitHubEgressHosts are the hosts a runner reaches for git over HTTPS
// (ADR-013). Plainly allowed: git is not a registry, and its traffic carries
// the installation token, which no proxy of ours should see.
var GitHubEgressHosts = []string{"github.com", "codeload.github.com"}

// RegistryHosts are ADR-013's default public package registries: npm, PyPI,
// Go, crates.io, RubyGems and Maven Central. **Every one is forwarded to the
// registry proxy** when one is configured, and the proxy fetches from nothing
// else — the list is both the policy and the proxy's upstream allowlist.
var RegistryHosts = []string{
	"registry.npmjs.org",
	"pypi.org", "files.pythonhosted.org",
	"proxy.golang.org", "sum.golang.org",
	"crates.io", "static.crates.io", "index.crates.io",
	"rubygems.org",
	"repo1.maven.org", "repo.maven.apache.org",
}

// IsRegistryHost reports whether host is on RegistryHosts.
func IsRegistryHost(host string) bool {
	for _, h := range RegistryHosts {
		if h == host {
			return true
		}
	}
	return false
}

// EgressRule is one host a runner's environment may reach. ForwardURL, when
// set, sends every request to that host through the registry proxy instead of
// straight to the host — every request: a backend must attach no path or
// method match, because a request no rule matches goes to the origin
// unrecorded (measured in M5.4c's spike).
type EgressRule struct {
	Host       string
	ForwardURL string
}

// RegistryForwardURL is where the proxy at base receives a registry host's
// requests. The route names the host, so the token Vercel signs for it — whose
// audience is this exact URL — cannot be replayed for another host.
func RegistryForwardURL(base, host string) string {
	return strings.TrimSuffix(base, "/") + "/r/" + host
}

// ValidateRegistryProxyURL holds the rules the registry proxy's public base
// URL must meet, for both the proxy (checking tokens' audiences against it)
// and the runner manager (building forwardURLs from it): https — sandboxes'
// registry traffic crosses the internet to reach it — and no user
// information, query or fragment, which Vercel refuses in a forwardURL.
func ValidateRegistryProxyURL(raw string) error {
	parsed, err := url.Parse(raw)
	switch {
	case err != nil || parsed.Host == "":
		return fmt.Errorf("%q is not a URL", raw)
	case parsed.Scheme != "https":
		return fmt.Errorf("%q must be https", raw)
	case parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(raw, "#"):
		return fmt.Errorf("%q must carry no user information, query or fragment", raw)
	}
	return nil
}

// RegistryRunner is what the proxy may learn about a runner: which session and
// workspace it serves, which backend made it, and whether it is live.
type RegistryRunner struct {
	SessionID   uuid.UUID
	WorkspaceID uuid.UUID
	Backend     string
	State       domain.RunnerState
}

// RegistryRequest is one recorded request.
type RegistryRequest struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	SessionID   uuid.UUID
	RunnerID    uuid.UUID
	Host        string
	Method      string
	Path        string
	RequestedAt time.Time
}

// RegistryStore persists the record.
type RegistryStore interface {
	// RunnerForRegistryRequest resolves a runner by id alone, through a
	// bounded privileged function: no tenant context needed.
	// domain.ErrRunnerNotFound if there is none.
	RunnerForRegistryRequest(ctx context.Context, runnerID uuid.UUID) (RegistryRunner, error)
	// RecordRegistryRequest appends one row, in the tenant context on ctx.
	RecordRegistryRequest(ctx context.Context, request RegistryRequest) error
}

// Errors the recorder refuses with. Each means the request must not be
// forwarded.
var (
	// ErrRegistryRunnerNotLive: the runner exists but has ended, or was not
	// made by a sandbox backend that forwards registry traffic.
	ErrRegistryRunnerNotLive = errors.New("the runner is not a live sandbox runner")
	// ErrRegistryRequestInvalid: a method or path the record will not hold.
	ErrRegistryRequestInvalid = errors.New("the registry request is not one the record accepts")
)

// MaxRegistryPathBytes caps a recorded path. Package paths are short; a
// longer one is cut on a rune boundary rather than refused, so an odd request
// is still recorded and still forwarded.
const MaxRegistryPathBytes = 2048

var registryMethods = map[string]bool{
	"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "OPTIONS": true,
}

// RegistryRecorder records registry requests for the proxy.
type RegistryRecorder struct {
	store RegistryStore
	bind  TenantBinder
	// backendPrefix is the backend name a forwarding runner carries
	// ("vercel-"): only a sandbox whose firewall forwards to the proxy can
	// legitimately send it anything.
	backendPrefix string
	now           func() time.Time
}

// NewRegistryRecorder wires the recorder.
func NewRegistryRecorder(store RegistryStore, bind TenantBinder, backendPrefix string, now func() time.Time) *RegistryRecorder {
	return &RegistryRecorder{store: store, bind: bind, backendPrefix: backendPrefix, now: now}
}

// Record resolves the runner and appends the request to its session's record.
//
// **Called before anything is forwarded**, and an error means the request is
// refused: the control fails closed. The runner must exist, have been made by
// a forwarding backend, and be live — a runner that has ended has no business
// fetching anything, and its token (valid 24 hours) must not keep working
// after it. The query string is dropped before anything is stored.
func (r *RegistryRecorder) Record(ctx context.Context, runnerID uuid.UUID, host, method, rawPath string) (RegistryRequest, error) {
	method = strings.ToUpper(method)
	if !registryMethods[method] {
		return RegistryRequest{}, fmt.Errorf("%w: method %q", ErrRegistryRequestInvalid, method)
	}
	path, _, _ := strings.Cut(rawPath, "?")
	if !strings.HasPrefix(path, "/") {
		return RegistryRequest{}, fmt.Errorf("%w: path must be absolute", ErrRegistryRequestInvalid)
	}
	path = domain.SafeText(path, MaxRegistryPathBytes)
	if !IsRegistryHost(host) {
		return RegistryRequest{}, fmt.Errorf("%w: %q is not a registry host", ErrRegistryRequestInvalid, host)
	}

	runner, err := r.store.RunnerForRegistryRequest(ctx, runnerID)
	if err != nil {
		return RegistryRequest{}, err
	}
	if !runner.State.Live() || !strings.HasPrefix(runner.Backend, r.backendPrefix) {
		return RegistryRequest{}, fmt.Errorf("%w: %s runner, %s", ErrRegistryRunnerNotLive, runner.Backend, runner.State)
	}

	id, err := uuid.NewV7()
	if err != nil {
		return RegistryRequest{}, err
	}
	request := RegistryRequest{
		ID: id, WorkspaceID: runner.WorkspaceID, SessionID: runner.SessionID, RunnerID: runnerID,
		Host: host, Method: method, Path: path, RequestedAt: r.now().UTC(),
	}
	if err := r.store.RecordRegistryRequest(r.bind(ctx, runner.WorkspaceID), request); err != nil {
		return RegistryRequest{}, fmt.Errorf("record registry request: %w", err)
	}
	return request, nil
}
