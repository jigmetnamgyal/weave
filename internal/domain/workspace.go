package domain

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
)

// Errors the workspace domain raises. Callers distinguish them with errors.Is
// so transport can map them to responses without matching on message text.
var (
	// ErrInvalidWorkspace is returned when input fails a workspace invariant.
	ErrInvalidWorkspace = errors.New("invalid workspace")
	// ErrLastOwner is returned when a change would leave a workspace with no
	// owner. A workspace with no owner cannot be administered or deleted by
	// anyone, so this is refused rather than repaired afterwards.
	ErrLastOwner = errors.New("workspace must keep at least one owner")
	// ErrCannotGrantRole is returned when an actor tries to assign a role
	// holding permissions the actor does not hold — an admin promoting someone
	// to owner, for instance.
	ErrCannotGrantRole = errors.New("cannot grant a role with more authority than your own")
)

// Workspace name and slug bounds. The slug bounds match the CHECK constraints
// in the migration; keep them in step.
const (
	workspaceNameMaxLen = 80
	workspaceSlugMinLen = 2
	workspaceSlugMaxLen = 64
)

// slugPattern mirrors workspaces_slug_format in the migration.
var slugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Workspace is a tenant: the boundary every other record is scoped by.
type Workspace struct {
	ID        uuid.UUID
	Slug      string
	Name      string
	CreatedBy uuid.UUID
	// Version supports optimistic concurrency. A caller renaming a workspace
	// submits the version it read; a mismatch is a conflict, not an overwrite.
	Version   int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Membership records that a user may act in a workspace, and at what role.
type Membership struct {
	WorkspaceID uuid.UUID
	UserID      uuid.UUID
	Role        Role
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Can reports whether this membership holds a permission.
func (m Membership) Can(permission Permission) bool { return m.Role.Can(permission) }

// MemberProfile is a membership together with enough of the user record to
// render a member list without a second lookup.
type MemberProfile struct {
	Membership
	Email       string
	DisplayName string
	AvatarURL   string
}

// NewWorkspaceID returns an identifier for a new workspace.
func NewWorkspaceID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate workspace id: %w", err)
	}
	return id, nil
}

// ValidateWorkspaceName checks a user-supplied workspace name.
func ValidateWorkspaceName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", fmt.Errorf("%w: name is required", ErrInvalidWorkspace)
	}
	if len([]rune(trimmed)) > workspaceNameMaxLen {
		return "", fmt.Errorf("%w: name may be at most %d characters",
			ErrInvalidWorkspace, workspaceNameMaxLen)
	}
	// Control characters would corrupt logs and terminal output downstream.
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: name may not contain control characters", ErrInvalidWorkspace)
		}
	}
	return trimmed, nil
}

// ValidateSlug checks a slug against the same shape the database enforces.
func ValidateSlug(slug string) error {
	if len(slug) < workspaceSlugMinLen || len(slug) > workspaceSlugMaxLen {
		return fmt.Errorf("%w: slug must be between %d and %d characters",
			ErrInvalidWorkspace, workspaceSlugMinLen, workspaceSlugMaxLen)
	}
	if !slugPattern.MatchString(slug) {
		return fmt.Errorf("%w: slug must be lowercase letters, digits and internal hyphens",
			ErrInvalidWorkspace)
	}
	return nil
}

// TruncateSlug shortens a slug so that appending a suffix of suffixLen
// characters still fits within the maximum length.
//
// Needed because SlugFromName may already return the full 64 characters, and
// naively appending "-2" to disambiguate a collision would produce a slug the
// database rejects — turning a duplicate name into an error instead of a
// disambiguated slug.
func TruncateSlug(slug string, suffixLen int) string {
	limit := workspaceSlugMaxLen - suffixLen
	if limit < 1 {
		return ""
	}
	if len(slug) <= limit {
		return strings.TrimRight(slug, "-")
	}
	// TrimRight because cutting mid-slug can leave a trailing hyphen, which
	// the format constraint rejects.
	return strings.TrimRight(slug[:limit], "-")
}

// SlugFromName derives a URL-safe slug from a workspace name.
//
// The result is a suggestion: slugs are unique, so the caller is responsible
// for resolving a collision. Returns an empty string when the name contains
// nothing usable — a name of only punctuation, or only non-Latin script —
// which the caller turns into a generated slug rather than an error.
func SlugFromName(name string) string {
	var b strings.Builder
	lastHyphen := true // leading hyphens are not allowed

	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			// Collapse any run of separators into a single hyphen.
			b.WriteByte('-')
			lastHyphen = true
		}
	}

	slug := strings.Trim(b.String(), "-")
	if len(slug) > workspaceSlugMaxLen {
		slug = strings.Trim(slug[:workspaceSlugMaxLen], "-")
	}
	if len(slug) < workspaceSlugMinLen {
		return ""
	}
	return slug
}
