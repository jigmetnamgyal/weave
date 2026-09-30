package domain

import (
	"errors"
	"net/netip"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MaxWorkspaceEgressHosts is the approved cap, excluding built-in destinations.
const MaxWorkspaceEgressHosts = 20

// ErrInvalidEgressHost means input is not a supported canonicalizable hostname.
var ErrInvalidEgressHost = errors.New("invalid egress hostname")

// ErrReservedEgressHost means a deployment or built-in destination cannot be added.
var ErrReservedEgressHost = errors.New("reserved egress hostname")

// EgressHost is one workspace-admin-approved destination, not network permission
// by itself. A runner must snapshot it and forward through ADR-017's guard.
type EgressHost struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	Hostname    string
	CreatedBy   uuid.UUID
	CreatedAt   time.Time
}

// RunnerEgressSnapshot is an immutable list captured when a runner is allocated.
// Missing snapshots must be reported as errors, not synthesized as empty lists.
type RunnerEgressSnapshot struct {
	RunnerID    uuid.UUID
	WorkspaceID uuid.UUID
	SessionID   uuid.UUID
	Hosts       []string
	CreatedAt   time.Time
}

// NewEgressHostID generates a sortable identifier for a new configuration entry.
func NewEgressHostID() (uuid.UUID, error) { return uuid.NewV7() }

// ValidateEgressHostname lowercases ASCII without trimming or resolving DNS.
// It deliberately rejects Unicode and punycode, URLs, raw IPs and local names.
// Reserved deployment destinations require an additional policy check by callers.
func ValidateEgressHostname(raw string) (string, error) {
	if len(raw) == 0 || len(raw) > 253 {
		return "", ErrInvalidEgressHost
	}
	for _, c := range raw {
		if c > 127 || c <= 32 || c == 127 {
			return "", ErrInvalidEgressHost
		}
	}
	host := strings.ToLower(raw)
	if _, err := netip.ParseAddr(host); err == nil {
		return "", ErrInvalidEgressHost
	}
	labels := strings.Split(host, ".")
	if len(labels) < 2 {
		return "", ErrInvalidEgressHost
	}
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' || strings.HasPrefix(label, "xn--") {
			return "", ErrInvalidEgressHost
		}
		for _, c := range label {
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return "", ErrInvalidEgressHost
			}
		}
	}
	// A numeric final label also refuses legacy shorthand IP forms like 127.1.
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return "", ErrInvalidEgressHost
	}
	for _, suffix := range []string{"localhost", "local", "localdomain", "internal", "home.arpa", "onion"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return "", ErrInvalidEgressHost
		}
	}
	return host, nil
}

// EgressHostReserved reports whether a canonical host is at or below a reserved
// namespace. Inputs must already be validated/canonicalized; an empty list does
// not disable syntactic/local-name rejection in ValidateEgressHostname.
func EgressHostReserved(host string, reserved []string) bool {
	for _, name := range reserved {
		if name != "" && (host == name || strings.HasSuffix(host, "."+name)) {
			return true
		}
	}
	return false
}
