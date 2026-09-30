package vercelsandbox

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// The network policy Vercel's firewall enforces for a sandbox, in its rules
// format: every allowed host, each with the rules applied to its requests.
//
// The rules format, not M5.4b's mode/allowedDomains format, because only it
// carries `forwardURL` (measured in M5.4c's spike). A host with no rules is
// plainly allowed; a host whose one rule forwards has every request sent to
// the registry proxy.
type rulesPolicy struct {
	Allow map[string][]policyRule `json:"allow"`
}

// policyRule forwards a host's requests. **It never carries `match`**: a
// request that no rule matches is sent straight to the origin, unrecorded, so
// a logging rule that matched only some requests would be a hole with a
// comment on it.
type policyRule struct {
	ForwardURL string `json:"forwardURL"`
}

// denyAll is the policy for no hosts at all.
type denyAll struct {
	Mode string `json:"mode"`
}

// ErrInvalidEgressHost means a host given for the policy is not an exact
// hostname. Refused rather than dropped: a silently shortened allowlist is a
// policy nobody wrote.
var ErrInvalidEgressHost = errors.New("vercelsandbox: not an exact hostname")

// ErrInvalidForwardURL means a rule's forwardURL is not one Vercel accepts or
// not the registry proxy's route for its host.
var ErrInvalidForwardURL = errors.New("vercelsandbox: not a valid forward URL")

// hostnameLabel is one DNS label: letters, digits and inner hyphens.
var hostnameLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// buildPolicy is the network policy for a runner: **always** an explicit
// allowlist of exact hostnames, never "allow-all" and never a wildcard.
//
// Vercel matches on the TLS SNI and does not stop domain fronting on shared
// CDNs, so narrow single-purpose hostnames are the mitigation; a wildcard
// over a registrable domain would reach whatever else it fronts. Raw IP
// addresses are refused (ADR-013 always refuses them), and so is anything
// carrying a port, a path or a scheme.
//
// **No hosts is "deny-all"**: a policy builder that loses every entry must
// fail closed. Vercel's deny-all also refuses DNS.
func buildPolicy(rules []application.EgressRule) (any, error) {
	allow := make(map[string][]policyRule, len(rules))
	for _, rule := range rules {
		if err := validHost(rule.Host); err != nil {
			return nil, err
		}
		var next []policyRule
		if rule.ForwardURL != "" {
			if err := validForwardURL(rule.Host, rule.ForwardURL); err != nil {
				return nil, err
			}
			next = []policyRule{{ForwardURL: rule.ForwardURL}}
		} else {
			next = []policyRule{}
		}
		if existing, seen := allow[rule.Host]; seen {
			if len(existing) != len(next) || (len(next) == 1 && existing[0] != next[0]) {
				return nil, fmt.Errorf("%w: %q is given twice with different rules", ErrInvalidEgressHost, rule.Host)
			}
			continue
		}
		allow[rule.Host] = next
	}
	if len(allow) == 0 {
		return denyAll{Mode: "deny-all"}, nil
	}
	return rulesPolicy{Allow: allow}, nil
}

// validForwardURL holds Vercel's rule — no user information, query or
// fragment — and Weave's: HTTPS, and the registry proxy's route for this exact
// host (…/r/<host>), so the token Vercel signs for it names this host and no
// other.
func validForwardURL(host, raw string) error {
	parsed, err := url.Parse(raw)
	switch {
	case err != nil:
		return fmt.Errorf("%w: %q", ErrInvalidForwardURL, raw)
	case parsed.Scheme != "https" || parsed.Host == "":
		return fmt.Errorf("%w: %q must be https", ErrInvalidForwardURL, raw)
	case parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || strings.Contains(raw, "#"):
		return fmt.Errorf("%w: %q carries user information, a query or a fragment", ErrInvalidForwardURL, raw)
	case !strings.HasSuffix(parsed.Path, "/r/"+host):
		return fmt.Errorf("%w: %q is not the registry proxy's route for %s", ErrInvalidForwardURL, raw, host)
	}
	return nil
}

func validHost(host string) error {
	switch {
	case host == "" || len(host) > 253:
		return fmt.Errorf("%w: %q", ErrInvalidEgressHost, host)
	case host != strings.ToLower(host):
		return fmt.Errorf("%w: %q is not lowercase", ErrInvalidEgressHost, host)
	case strings.Contains(host, "*"):
		return fmt.Errorf("%w: %q is a wildcard", ErrInvalidEgressHost, host)
	case net.ParseIP(strings.Trim(host, "[]")) != nil:
		return fmt.Errorf("%w: %q is an IP address", ErrInvalidEgressHost, host)
	case !strings.Contains(host, "."):
		return fmt.Errorf("%w: %q is not a qualified hostname", ErrInvalidEgressHost, host)
	}
	for _, label := range strings.Split(host, ".") {
		if !hostnameLabel.MatchString(label) {
			return fmt.Errorf("%w: %q", ErrInvalidEgressHost, host)
		}
	}
	// A hostname whose last label is all digits is an IPv4 address in
	// disguise to some resolvers ("10.0.0.010").
	labels := strings.Split(host, ".")
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return fmt.Errorf("%w: %q ends in a numeric label", ErrInvalidEgressHost, host)
	}
	return nil
}
