package application

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Forwarding of workspace-added destinations (Unit M5.4d.3a, ADR-017).
//
// A runner's added hosts are forwarded, with no match filter, to the egress
// proxy at <base>/e/<host>. The proxy authorizes each request against the
// runner's own immutable snapshot and connects only to validated public
// addresses. Added hosts are never plain firewall rules.

// EgressForwardURL is where the egress proxy at base receives one added host's
// requests. The route names the host, so the token Vercel signs for it — whose
// audience is this exact URL — cannot be replayed for another host.
func EgressForwardURL(base, host string) string {
	return strings.TrimSuffix(base, "/") + "/e/" + host
}

// EgressSnapshotReader reads a runner's immutable snapshot in the tenant
// context on ctx. ErrEgressSnapshotMissing when no row exists: never an
// implied empty list.
type EgressSnapshotReader interface {
	Snapshot(ctx context.Context, workspaceID, runnerID uuid.UUID) (domain.RunnerEgressSnapshot, error)
}

// RunnerIdentityLookup resolves a runner by id alone, through a bounded
// privileged function (the registry proxy's, one id in and at most one row out).
type RunnerIdentityLookup interface {
	RunnerForRegistryRequest(ctx context.Context, runnerID uuid.UUID) (RegistryRunner, error)
}

// Errors the egress authorizer refuses with. Each means nothing is forwarded.
var (
	// ErrEgressRunnerNotLive: the runner has ended, or was not made by a
	// sandbox backend whose firewall forwards to the proxy.
	ErrEgressRunnerNotLive = errors.New("the runner is not a live sandbox runner")
	// ErrEgressHostNotAuthorized: the host is not in this runner's snapshot.
	ErrEgressHostNotAuthorized = errors.New("the host is not in the runner's egress snapshot")
)

// EgressAuthorizer decides whether one runner may reach one added host.
type EgressAuthorizer struct {
	lookup        RunnerIdentityLookup
	snapshots     EgressSnapshotReader
	bind          TenantBinder
	backendPrefix string
}

// NewEgressAuthorizer wires the authorizer. backendPrefix is the backend name
// a forwarding runner carries ("vercel-").
func NewEgressAuthorizer(lookup RunnerIdentityLookup, snapshots EgressSnapshotReader, bind TenantBinder, backendPrefix string) *EgressAuthorizer {
	return &EgressAuthorizer{lookup: lookup, snapshots: snapshots, bind: bind, backendPrefix: backendPrefix}
}

// Authorize resolves the runner, then reads **its** snapshot in **its own**
// workspace and requires the exact host. It never consults the mutable
// workspace list, and never takes a workspace from the request. Any failure,
// including a database error, is a refusal.
func (a *EgressAuthorizer) Authorize(ctx context.Context, runnerID uuid.UUID, host string) (RegistryRunner, error) {
	runner, err := a.lookup.RunnerForRegistryRequest(ctx, runnerID)
	if err != nil {
		return RegistryRunner{}, err
	}
	if !runner.State.Live() || !strings.HasPrefix(runner.Backend, a.backendPrefix) {
		return RegistryRunner{}, fmt.Errorf("%w: %s runner, %s", ErrEgressRunnerNotLive, runner.Backend, runner.State)
	}
	snapshot, err := a.snapshots.Snapshot(a.bind(ctx, runner.WorkspaceID), runner.WorkspaceID, runnerID)
	if err != nil {
		return RegistryRunner{}, err
	}
	if snapshot.RunnerID != runnerID || snapshot.WorkspaceID != runner.WorkspaceID {
		return RegistryRunner{}, ErrEgressHostNotAuthorized
	}
	for _, allowed := range snapshot.Hosts {
		if allowed == host {
			return runner, nil
		}
	}
	return RegistryRunner{}, ErrEgressHostNotAuthorized
}

// addedEgressRules turns a runner's snapshot into forwarded rules, failing
// closed: a host that is now reserved, or any added host with no proxy
// configured, refuses the whole provisioning rather than falling back to a
// plain rule.
func addedEgressRules(snapshot domain.RunnerEgressSnapshot, proxyBase string, reserved []string) ([]EgressRule, error) {
	if len(snapshot.Hosts) == 0 {
		return nil, nil
	}
	if proxyBase == "" {
		return nil, fmt.Errorf("%w: the workspace has %d added egress hosts and no egress proxy is configured (RUNNER_EGRESS_PROXY_URL)",
			ErrEgressConfiguration, len(snapshot.Hosts))
	}
	builtIn := append(append([]string(nil), GitHubEgressHosts...), RegistryHosts...)
	rules := make([]EgressRule, 0, len(snapshot.Hosts))
	for _, host := range snapshot.Hosts {
		canonical, err := domain.ValidateEgressHostname(host)
		if err != nil || canonical != host {
			return nil, fmt.Errorf("%w: an added egress host is not canonical", ErrEgressConfiguration)
		}
		if domain.EgressHostReserved(host, builtIn) || domain.EgressHostReserved(host, reserved) {
			return nil, fmt.Errorf("%w: an added egress host is now reserved", ErrEgressConfiguration)
		}
		rules = append(rules, EgressRule{Host: host, ForwardURL: EgressForwardURL(proxyBase, host)})
	}
	return rules, nil
}

// ErrEgressConfiguration means a runner cannot be given a policy that honors
// its snapshot. Provisioning fails rather than granting less protection.
var ErrEgressConfiguration = errors.New("the runner's egress policy cannot be built")
