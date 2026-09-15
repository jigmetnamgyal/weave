package domain

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
)

// Errors the branch domain raises.
var (
	// ErrInvalidBranchName is returned when a name would not be a valid Git
	// ref, or would be one we refuse to create.
	ErrInvalidBranchName = errors.New("invalid branch name")
	// ErrBranchProtected is returned when the target of a creation is a
	// protected branch. Invariant 11: protected branches never receive direct
	// agent writes.
	ErrBranchProtected = errors.New("branch is protected")
	// ErrBranchConflict is returned when the branch already exists and points
	// somewhere other than the requested base. Handing back a branch pointing
	// at unknown work would be worse than refusing.
	ErrBranchConflict = errors.New("branch already exists at a different commit")
	// ErrBaseNotFound is returned when the base to branch from does not exist.
	ErrBaseNotFound = errors.New("base branch not found")
)

// branchNameMaxLen bounds a generated or supplied name. Git itself has no
// limit, but filesystems holding loose refs do, and a name long enough to
// matter is a mistake rather than a requirement.
const branchNameMaxLen = 200

// BranchPrefix is the namespace every branch Weave creates lives under.
//
// A namespace rather than bare names, so that a repository's own branches and
// Weave's are distinguishable at a glance, and so a protection rule can cover
// or exclude ours as a group.
const BranchPrefix = "weave/"

// branchSuffixBytes is the entropy in a generated name's suffix. Enough that
// two sessions in the same repository do not collide; short enough to read.
const branchSuffixBytes = 5

// suffixEncoding is lowercase base32 without padding: valid in a ref, readable
// aloud, and case-insensitive-safe on filesystems that fold case.
var suffixEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// NewBranchName generates a namespaced branch name.
//
// The caller supplies a slug describing the purpose; the random suffix is what
// makes it unique. Both halves are validated, so a slug from a task title
// cannot smuggle path traversal into a ref.
func NewBranchName(slug string) (string, error) {
	cleaned := slugify(slug)
	if cleaned == "" {
		cleaned = "task"
	}

	raw := make([]byte, branchSuffixBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate branch suffix: %w", err)
	}

	name := BranchPrefix + cleaned + "-" + suffixEncoding.EncodeToString(raw)
	if err := ValidateBranchName(name); err != nil {
		return "", err
	}
	return name, nil
}

// slugify reduces arbitrary text to the characters a ref may safely carry.
//
// Everything outside [a-z0-9-] becomes a hyphen, runs collapse, and the ends
// are trimmed. It is deliberately narrower than Git allows: a ref may contain
// a great deal that is legal and still awful to type, quote in a shell, or
// read in a log.
func slugify(value string) string {
	var b strings.Builder
	lastHyphen := true // trims leading hyphens without a second pass
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	slug := strings.Trim(b.String(), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	return slug
}

// ValidateBranchName checks a name against Git's rules and ours.
//
// Checked here, before any request, because these are the inputs that turn a
// ref into something else: `..` walks out of the namespace, a `refs/` prefix
// escapes `refs/heads/` into whatever the caller names, and control characters
// or a trailing `.lock` produce refs Git will not address afterwards.
//
// The rules are git-check-ref-format's, restated rather than shelled out to:
// this must run identically in a test with no Git installed.
func ValidateBranchName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("%w: a branch name is required", ErrInvalidBranchName)
	case len(name) > branchNameMaxLen:
		return fmt.Errorf("%w: longer than %d characters", ErrInvalidBranchName, branchNameMaxLen)
	case !strings.HasPrefix(name, BranchPrefix):
		// Ours to create, so ours to namespace. Without this, a caller could
		// name any branch in the repository, including one a human relies on.
		return fmt.Errorf("%w: must begin with %q", ErrInvalidBranchName, BranchPrefix)
	case strings.HasPrefix(name, "refs/"):
		return fmt.Errorf("%w: must not start with refs/", ErrInvalidBranchName)
	case strings.Contains(name, ".."):
		return fmt.Errorf("%w: must not contain a %q sequence", ErrInvalidBranchName, "..")
	case strings.Contains(name, "@{"):
		// Git's reflog syntax. A ref containing it cannot be addressed
		// afterwards, so GitHub would refuse it — but refusing here returns
		// the documented invalid-request response instead of a relayed 422.
		//
		// Git's neighbouring rule, that a ref cannot *be* the single character
		// "@", needs no case: the namespace check above already guarantees
		// every name begins with the prefix, so no name can be "@" alone. A
		// component named "@" — `weave/@` — is a legal ref, and an earlier
		// revision rejected it here under a comment claiming it was the
		// single-character case. It was not.
		return fmt.Errorf("%w: must not contain a %q sequence", ErrInvalidBranchName, "@{")
	case strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/"):
		return fmt.Errorf("%w: must not begin or end with /", ErrInvalidBranchName)
	case strings.Contains(name, "//"):
		return fmt.Errorf("%w: must not contain an empty path segment", ErrInvalidBranchName)
	case strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".lock"):
		return fmt.Errorf("%w: must not end with . or .lock", ErrInvalidBranchName)
	case strings.HasPrefix(name, "-"):
		return fmt.Errorf("%w: must not begin with -", ErrInvalidBranchName)
	}

	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			return fmt.Errorf("%w: must not contain control characters", ErrInvalidBranchName)
		case r == ' ' || r == '~' || r == '^' || r == ':' || r == '?' || r == '*' || r == '[' || r == '\\':
			return fmt.Errorf("%w: must not contain %q", ErrInvalidBranchName, string(r))
		}
	}

	// Each slash-separated component has its own rules; a component may not
	// begin with a dot or end with .lock even when the whole name does not.
	for _, component := range strings.Split(name, "/") {
		if component == "" {
			return fmt.Errorf("%w: must not contain an empty path segment", ErrInvalidBranchName)
		}
		if strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".lock") {
			return fmt.Errorf("%w: no path segment may begin with . or end with .lock", ErrInvalidBranchName)
		}
	}
	return nil
}

// Branch is a ref Weave created or found in a repository.
type Branch struct {
	Name string
	// SHA is the commit the branch points at.
	SHA string
	// Base is the branch it was created from.
	Base string
	// Created is false when the branch already existed at the requested base.
	// Reported rather than hidden: the caller asked for a branch at a commit
	// and got one, but "already there" and "just made" are different facts
	// and the audit trail should say which.
	Created bool
}
