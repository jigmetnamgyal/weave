package vercelsandbox

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
)

// networkPolicy is the body Vercel's firewall enforces for a sandbox.
type networkPolicy struct {
	Mode           string   `json:"mode"`
	AllowedDomains []string `json:"allowedDomains,omitempty"`
}

// ErrInvalidEgressHost means a host given for the policy is not an exact
// hostname. Refused rather than dropped: a silently shortened allowlist is a
// policy nobody wrote.
var ErrInvalidEgressHost = errors.New("vercelsandbox: not an exact hostname")

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
// **No hosts is "deny-all"**, not an empty custom list: a policy builder that
// loses every entry must fail closed. Vercel's deny-all also refuses DNS,
// which the spike and M5.4b's probe both observed.
func buildPolicy(hosts []string) (networkPolicy, error) {
	allowed := make([]string, 0, len(hosts))
	for _, host := range hosts {
		if err := validHost(host); err != nil {
			return networkPolicy{}, err
		}
		if !slices.Contains(allowed, host) {
			allowed = append(allowed, host)
		}
	}
	if len(allowed) == 0 {
		return networkPolicy{Mode: "deny-all"}, nil
	}
	return networkPolicy{Mode: "custom", AllowedDomains: allowed}, nil
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
