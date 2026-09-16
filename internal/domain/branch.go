package domain

import (
	"encoding/base32"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
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

// branchSuffixBytes is how much of the session id the suffix carries. Enough
// that two sessions in the same repository do not collide; short enough to
// read aloud.
const branchSuffixBytes = 5

// suffixEncoding is lowercase base32 without padding: valid in a ref, readable
// aloud, and case-insensitive-safe on filesystems that fold case.
var suffixEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// SessionBranchName derives a branch name from a session id.
//
// **A pure function of the id, deliberately.** The name is decided when the
// session row is written and the branch is cut later by a workflow that
// retries — so the name has to be the same on every attempt, or a retry after
// a lost response creates a *second* branch rather than finding the first.
// M3.3 shipped a version that generated a fresh random name per attempt and
// had exactly that bug; the endpoint now requires a caller-supplied name
// because it had no stable identity to derive one from. This is that identity.
//
// The suffix is the tail of the session id rather than `crypto/rand`, which is
// the whole difference. A UUIDv7's low bytes are random, so five of them
// collide no more readily than five random ones would, while staying
// reproducible from the row.
//
// The slug describes the purpose and comes from a task title, so it is
// slugified and the finished name validated: a title cannot smuggle path
// traversal into a ref.
func SessionBranchName(sessionID uuid.UUID, slug string) (string, error) {
	if sessionID == uuid.Nil {
		return "", fmt.Errorf("%w: a session id is required to derive a branch name",
			ErrInvalidBranchName)
	}

	cleaned := slugify(slug)
	if cleaned == "" {
		cleaned = "session"
	}

	suffix := suffixEncoding.EncodeToString(sessionID[len(sessionID)-branchSuffixBytes:])

	// Truncate the slug rather than the suffix if the whole name would be too
	// long. The suffix is what makes the name unique and reproducible; the
	// slug is only there to make it readable, so it is the half that gives.
	room := branchNameMaxLen - len(BranchPrefix) - len(suffix) - 1
	if len(cleaned) > room {
		cleaned = strings.TrimRight(cleaned[:room], "-")
	}
	if cleaned == "" {
		cleaned = "session"
	}

	name := BranchPrefix + cleaned + "-" + suffix
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
