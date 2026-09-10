package domain

import "testing"

// TestTruncateSlugLeavesRoomForSuffix covers the collision path: a 64-character
// base slug plus a disambiguating suffix must still satisfy the length
// constraint, or a duplicate name becomes an error instead of a second slug.
func TestTruncateSlugLeavesRoomForSuffix(t *testing.T) {
	maxBase := ""
	for range workspaceSlugMaxLen {
		maxBase += "a"
	}
	if len(maxBase) != workspaceSlugMaxLen {
		t.Fatalf("test fixture is %d characters, want %d", len(maxBase), workspaceSlugMaxLen)
	}

	for _, suffix := range []string{"-2", "-6", "-0a1b2c3d"} {
		candidate := TruncateSlug(maxBase, len(suffix)) + suffix

		if len(candidate) > workspaceSlugMaxLen {
			t.Errorf("slug %q is %d characters, over the %d limit",
				candidate, len(candidate), workspaceSlugMaxLen)
		}
		if err := ValidateSlug(candidate); err != nil {
			t.Errorf("slug %q is invalid: %v", candidate, err)
		}
	}
}

// TestTruncateSlugTrimsTrailingHyphen covers cutting mid-slug: the format
// constraint rejects a trailing hyphen.
func TestTruncateSlugTrimsTrailingHyphen(t *testing.T) {
	// Truncating "aaa-bbb" to 4 characters yields "aaa-", which is invalid.
	if got := TruncateSlug("aaa-bbb", workspaceSlugMaxLen-4); got != "aaa" {
		t.Errorf("TruncateSlug = %q, want %q", got, "aaa")
	}
	if got := TruncateSlug("short", 2); got != "short" {
		t.Errorf("a slug that already fits was altered: %q", got)
	}
}

func TestSlugFromNameNeverExceedsLimit(t *testing.T) {
	long := ""
	for range 200 {
		long += "workspace "
	}
	slug := SlugFromName(long)
	if len(slug) > workspaceSlugMaxLen {
		t.Errorf("slug is %d characters, over the %d limit", len(slug), workspaceSlugMaxLen)
	}
	if err := ValidateSlug(slug); err != nil {
		t.Errorf("derived slug %q is invalid: %v", slug, err)
	}
}

// TestSlugFromNameRejectsUnusableNames covers the fallback path: a name with
// nothing slug-safe returns empty so the caller substitutes an identifier
// rather than refusing a legitimate name.
func TestSlugFromNameRejectsUnusableNames(t *testing.T) {
	for _, name := range []string{"", "   ", "!!!", "日本語のみ", "-"} {
		if got := SlugFromName(name); got != "" {
			t.Errorf("SlugFromName(%q) = %q, want empty so the caller can fall back", name, got)
		}
	}
}
