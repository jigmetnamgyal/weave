package domain_test

import (
	"errors"
	"strings"
	"testing"

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
	} {
		if err := domain.ValidateBranchName(candidate); err != nil {
			t.Errorf("ValidateBranchName(%q) = %v, want nil", candidate, err)
		}
	}
}

// TestNewBranchNameIsValidAndUnique checks that generation cannot produce
// something validation would reject, including from hostile input.
func TestNewBranchNameIsValidAndUnique(t *testing.T) {
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
			name, err := domain.NewBranchName(slug)
			if err != nil {
				t.Fatalf("NewBranchName(%q) = %v", slug, err)
			}
			if err := domain.ValidateBranchName(name); err != nil {
				t.Fatalf("generated %q from %q, which validation rejects: %v", name, slug, err)
			}
			if !strings.HasPrefix(name, domain.BranchPrefix) {
				t.Fatalf("generated %q, which is outside the namespace", name)
			}
			if seen[name] {
				t.Fatalf("generated %q twice; the suffix is not doing its job", name)
			}
			seen[name] = true
		}
	}
}
