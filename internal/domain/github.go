package domain

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Errors the GitHub integration raises. Callers distinguish them with
// errors.Is so transport can map them without matching on message text.
var (
	// ErrInvalidInstallation is returned when input fails an installation
	// invariant.
	ErrInvalidInstallation = errors.New("invalid github installation")
	// ErrInstallationBoundElsewhere is returned when a GitHub installation is
	// already attached to a different workspace. It is refused rather than
	// rebound: silently rebinding would move a repository grant between
	// tenants on a single request.
	ErrInstallationBoundElsewhere = errors.New("installation is already connected to another workspace")
	// ErrInstallationSuspended is returned when an installation exists but
	// GitHub has suspended it, so it authorizes nothing.
	ErrInstallationSuspended = errors.New("installation is suspended")
	// ErrRepositoryNotGranted is returned when a repository is known but the
	// installation no longer grants it. Distinct from "not found" on purpose:
	// the caller is a member who may see it, and the honest answer is that
	// access was withdrawn.
	ErrRepositoryNotGranted = errors.New("installation no longer grants this repository")
)

// AccountType is the kind of GitHub account an installation belongs to.
type AccountType string

// The account types GitHub reports.
const (
	AccountUser         AccountType = "User"
	AccountOrganization AccountType = "Organization"
)

// ParseAccountType validates a value GitHub sent us.
//
// Unknown values are refused rather than stored, because the column has a
// CHECK constraint and a rejected insert late in a transaction is a worse
// error to debug than a rejected parse early in one.
func ParseAccountType(value string) (AccountType, error) {
	switch AccountType(strings.TrimSpace(value)) {
	case AccountUser:
		return AccountUser, nil
	case AccountOrganization:
		return AccountOrganization, nil
	default:
		return "", fmt.Errorf("%w: unknown account type %q", ErrInvalidInstallation, value)
	}
}

// RepositorySelection is whether an installation grants every repository in
// the account or a chosen subset.
type RepositorySelection string

// The selection modes GitHub reports.
const (
	// SelectionAll grants every repository the account owns, including ones
	// created later. There is no event naming a newly created repository as
	// granted, which is one reason access is reconciled on use.
	SelectionAll RepositorySelection = "all"
	// SelectionSelected grants a chosen subset.
	SelectionSelected RepositorySelection = "selected"
)

// ParseRepositorySelection validates a value GitHub sent us.
func ParseRepositorySelection(value string) (RepositorySelection, error) {
	switch RepositorySelection(strings.TrimSpace(value)) {
	case SelectionAll:
		return SelectionAll, nil
	case SelectionSelected:
		return SelectionSelected, nil
	default:
		return "", fmt.Errorf("%w: unknown repository selection %q", ErrInvalidInstallation, value)
	}
}

// Installation is a GitHub App installation bound to exactly one workspace.
type Installation struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	// GitHubID is GitHub's own installation identifier, and the only thing a
	// webhook gives us to find this row by.
	GitHubID            int64
	AccountLogin        string
	AccountType         AccountType
	RepositorySelection RepositorySelection
	ConnectedBy         uuid.UUID
	SuspendedAt         *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// Suspended reports whether GitHub has suspended the installation.
//
// A suspended installation keeps its records: unsuspending must restore the
// previous state rather than require a fresh install, so the rows are the
// thing that makes that possible.
func (i Installation) Suspended() bool { return i.SuspendedAt != nil }

// Repository is a repository an installation grants, as we last understood it.
//
// Every field here is a cache of something GitHub owns. The row is what we
// believe; GitHub decides. Authorization must therefore never rest on Granted
// alone without a reconciliation that can correct it.
type Repository struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	// InstallationID is ours, not GitHub's — the local installation row.
	InstallationID uuid.UUID
	// GitHubID is the stable identity. Owner and Name both change on rename
	// and transfer, and a rename must not read as a different repository.
	GitHubID      int64
	Owner         string
	Name          string
	DefaultBranch string
	Private       bool
	Granted       bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// FullName is the owner/name form GitHub uses in URLs and API paths.
func (r Repository) FullName() string { return r.Owner + "/" + r.Name }

// RequiredPermissions is what the App must hold for the product to work.
//
// Checked against what an installation actually grants rather than against
// what the App requests. The two drift: an App's permission set can be
// widened later, and existing installations keep the set they accepted until
// someone approves the change. An installation missing one of these is not
// broken in a way any amount of retrying fixes — it needs a human to approve
// the new permissions on GitHub.
var RequiredPermissions = map[string]string{
	"contents":      "write",
	"metadata":      "read",
	"pull_requests": "write",
}

// permissionRank orders access levels so a stronger grant satisfies a weaker
// requirement: an installation granting contents:admin satisfies
// contents:write.
var permissionRank = map[string]int{"read": 1, "write": 2, "admin": 3}

// MissingPermissions reports which required permissions an installation does
// not hold, as "permission:access" strings, sorted for a stable message.
//
// It returns what is missing rather than a bare boolean because the only
// useful thing to tell someone whose workspace has stopped working is which
// permission to approve.
func MissingPermissions(granted map[string]string) []string {
	var missing []string
	for permission, required := range RequiredPermissions {
		held, ok := granted[permission]
		if !ok || permissionRank[held] < permissionRank[required] {
			missing = append(missing, permission+":"+required)
		}
	}
	sort.Strings(missing)
	return missing
}

// installStateBytes is the entropy behind an install-state value. The same 256
// bits used for invitation tokens, and for the same reason: it is the only
// thing standing between a replayed callback and a mis-bound installation.
const installStateBytes = 32

// InstallStateLifetime bounds how long an install may sit on GitHub's consent
// screen. Long enough to read the permissions being requested and pick
// repositories; short enough that an abandoned link is not still live an hour
// later.
const InstallStateLifetime = 15 * time.Minute

// InstallState is the single-use value that carries intent across the
// installation round trip.
//
// GitHub returns an installation id and nothing identifying which workspace
// the user meant. If the workspace came from anything the caller controls at
// that moment, whoever held an installation id could attach it to a workspace
// of their choosing — or attach someone else's installation to their own
// workspace and read repositories they were never granted. So the workspace is
// decided before the redirect, recorded server-side, and looked up by a random
// value on the way back.
type InstallState struct {
	// Value travels in the `state` query parameter. GitHub preserves it across
	// install, authentication and update.
	Value string
	// WorkspaceID is the workspace the installation will be bound to.
	WorkspaceID uuid.UUID
	// UserID is who started the flow. Recorded for the audit entry, and not
	// trusted as proof of current authority — permission is re-checked on the
	// way back, because membership can change while a browser sits on GitHub.
	UserID uuid.UUID
}

// NewInstallState mints an unguessable state value for one installation
// attempt.
func NewInstallState(workspaceID, userID uuid.UUID) (InstallState, error) {
	raw := make([]byte, installStateBytes)
	if _, err := rand.Read(raw); err != nil {
		return InstallState{}, fmt.Errorf("generate install state: %w", err)
	}
	// URL-safe and unpadded: it travels in a query string, and padding invites
	// mangling by clients that re-encode URLs.
	return InstallState{
		Value:       base64.RawURLEncoding.EncodeToString(raw),
		WorkspaceID: workspaceID,
		UserID:      userID,
	}, nil
}

// NewInstallationID returns an identifier for a new installation row.
func NewInstallationID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate installation id: %w", err)
	}
	return id, nil
}

// NewRepositoryID returns an identifier for a new repository row.
func NewRepositoryID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate repository id: %w", err)
	}
	return id, nil
}

// ValidateAccountLogin checks a login GitHub reported.
func ValidateAccountLogin(login string) (string, error) {
	trimmed := strings.TrimSpace(login)
	if trimmed == "" {
		return "", fmt.Errorf("%w: an account login is required", ErrInvalidInstallation)
	}
	if len(trimmed) > 39 { // GitHub's own maximum for account names.
		return "", fmt.Errorf("%w: account login is longer than GitHub allows", ErrInvalidInstallation)
	}
	return trimmed, nil
}
