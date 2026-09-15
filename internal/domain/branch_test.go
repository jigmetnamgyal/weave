package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// TestValidateBranchNameRejects covers the inputs that turn a ref into
// something else.
//
// Every one of these is checked before any request leaves the process. Relying
// on GitHub to refuse them would mean the refusal depends on a remote service
// being strict, and `..` in particular is the difference between naming a
// branch and naming something outside the namespace entirely.
func TestValidateBranchNameRejects(t *testing.T) {
	tests := map[string]string{
		"empty":                      "",
		"outside our namespace":      "main",
		"parent traversal":           "weave/../../etc/passwd",
		"refs prefix":                "refs/heads/weave/x",
		"trailing slash":             "weave/x/",
		"leading slash":              "/weave/x",
		"empty path segment":         "weave//x",
		"trailing dot":               "weave/x.",
		"lock suffix":                "weave/x.lock",
		"segment beginning with dot": "weave/.hidden",
		"segment ending in lock":     "weave/x.lock/y",
		"space":                      "weave/a b",
		"tilde":                      "weave/a~b",
		"caret":                      "weave/a^b",
		"colon":                      "weave/a:b",
		"question mark":              "weave/a?b",
		"asterisk":                   "weave/a*b",
		"open bracket":               "weave/a[b",
		"backslash":                  `weave/a\b`,
		"newline":                    "weave/a\nb",
		"null byte":                  "weave/a\x00b",
		"delete character":           "weave/a\x7fb",
		"longer than the bound":      "weave/" + strings.Repeat("a", 300),
	}

	for name, candidate := range tests {
		t.Run(name, func(t *testing.T) {
			if err := domain.ValidateBranchName(candidate); !errors.Is(err, domain.ErrInvalidBranchName) {
				t.Errorf("ValidateBranchName(%q) = %v, want ErrInvalidBranchName", candidate, err)
			}
		})
	}
}

// TestValidateBranchNameAcceptsOrdinaryNames is the control: without it every
// rejection above would pass against a function that refuses everything.
func TestValidateBranchNameAcceptsOrdinaryNames(t *testing.T) {
	for _, candidate := range []string{
		"weave/task-abcde",
		"weave/fix-the-thing-2f4k7",
		"weave/a",
		"weave/nested/name",
		// A path component named "@" is legal. Git forbids a ref that *is*
		// the single character "@", which the namespace prefix already makes
		// impossible — an earlier revision conflated the two and rejected
		// this.
		"weave/@",
		"weave/@-suffix",
	} {
		if err := domain.ValidateBranchName(candidate); err != nil {
			t.Errorf("ValidateBranchName(%q) = %v, want nil", candidate, err)
		}
	}
}

// TestSessionBranchNameIsValidAndUnique checks that derivation cannot produce
// something validation would reject, including from hostile input, and that
// two sessions do not land on the same branch.
func TestSessionBranchNameIsValidAndUnique(t *testing.T) {
	hostile := []string{
		"../../etc/passwd",
		"refs/heads/main",
		"a b\tc\nd",
		"........",
		"",
		strings.Repeat("x", 500),
		"Ünïcødé Title!",
	}

	seen := map[string]bool{}
	for _, slug := range hostile {
		for i := 0; i < 20; i++ {
			id, err := domain.NewSessionID()
			if err != nil {
				t.Fatalf("NewSessionID: %v", err)
			}
			name, err := domain.SessionBranchName(id, slug)
			if err != nil {
				t.Fatalf("SessionBranchName(%q) = %v", slug, err)
			}
			if err := domain.ValidateBranchName(name); err != nil {
				t.Fatalf("derived %q from %q, which validation rejects: %v", name, slug, err)
			}
			if !strings.HasPrefix(name, domain.BranchPrefix) {
				t.Fatalf("derived %q, which is outside the namespace", name)
			}
			if seen[name] {
				t.Fatalf("derived %q twice from different session ids", name)
			}
			seen[name] = true
		}
	}
}

// TestSessionBranchNameIsAPureFunctionOfTheSessionID is the property the
// whole derivation exists for.
//
// The branch is cut by a workflow activity that retries. If the name were
// regenerated per attempt, a retry after a lost response would create a second
// branch rather than finding the first — which is precisely the bug M3.3
// shipped and then removed, and the reason its endpoint requires the caller to
// supply a name. Same id and same slug must give the same name, always.
func TestSessionBranchNameIsAPureFunctionOfTheSessionID(t *testing.T) {
	id, err := domain.NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}

	first, err := domain.SessionBranchName(id, "add rate limiting")
	if err != nil {
		t.Fatalf("SessionBranchName: %v", err)
	}
	for i := 0; i < 50; i++ {
		again, err := domain.SessionBranchName(id, "add rate limiting")
		if err != nil {
			t.Fatalf("SessionBranchName: %v", err)
		}
		if again != first {
			t.Fatalf("derivation is not pure: %q then %q", first, again)
		}
	}

	// A different session must not land on the same branch, or two sessions
	// would write to one ref.
	other, err := domain.NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	if name, err := domain.SessionBranchName(other, "add rate limiting"); err != nil {
		t.Fatalf("SessionBranchName: %v", err)
	} else if name == first {
		t.Errorf("two sessions derived the same branch name %q", name)
	}
}

// TestSessionBranchNameRefusesTheNilID guards the case where a caller derives
// a name before generating an id — every session would then share one branch,
// and the failure would look like a collision rather than a missing id.
func TestSessionBranchNameRefusesTheNilID(t *testing.T) {
	if _, err := domain.SessionBranchName(uuid.Nil, "anything"); err == nil {
		t.Error("SessionBranchName(uuid.Nil) = nil, want an error")
	}
}
