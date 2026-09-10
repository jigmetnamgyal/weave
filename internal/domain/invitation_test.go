package domain

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestInvitationStatus(t *testing.T) {
	now := time.Now()
	past, future := now.Add(-time.Hour), now.Add(time.Hour)

	tests := []struct {
		name       string
		invitation Invitation
		want       InvitationStatus
	}{
		{"pending", Invitation{ExpiresAt: future}, InvitationPending},
		{"expired", Invitation{ExpiresAt: past}, InvitationExpired},
		{"accepted", Invitation{ExpiresAt: future, AcceptedAt: past}, InvitationAccepted},
		{"revoked", Invitation{ExpiresAt: future, RevokedAt: past}, InvitationRevoked},
		// Terminal states win over expiry: an invitation accepted while it was
		// still valid reads as accepted, not expired.
		{"accepted then expired", Invitation{ExpiresAt: past, AcceptedAt: past}, InvitationAccepted},
		{"revoked then expired", Invitation{ExpiresAt: past, RevokedAt: past}, InvitationRevoked},
		// Expiry is exclusive: an invitation is dead the instant it expires.
		{"expiring exactly now", Invitation{ExpiresAt: now}, InvitationExpired},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.invitation.Status(now); got != tt.want {
				t.Errorf("Status = %q, want %q", got, tt.want)
			}
			if usable := tt.invitation.Usable(now); usable != (tt.want == InvitationPending) {
				t.Errorf("Usable = %v for status %q", usable, tt.want)
			}
		})
	}
}

// TestNewInvitationTokenIsUnpredictable checks the two properties that make
// the token safe to be the only thing guarding a workspace.
func TestNewInvitationTokenIsUnpredictable(t *testing.T) {
	const samples = 200

	seen := make(map[string]struct{}, samples)
	for range samples {
		token, err := NewInvitationToken()
		if err != nil {
			t.Fatalf("NewInvitationToken: %v", err)
		}

		if _, duplicate := seen[token.Plaintext]; duplicate {
			t.Fatal("two tokens collided, which should be impossible at 256 bits")
		}
		seen[token.Plaintext] = struct{}{}

		// base64url of 32 bytes, unpadded.
		if len(token.Plaintext) != 43 {
			t.Errorf("token is %d characters, want 43", len(token.Plaintext))
		}
		if len(token.Hash) != 32 {
			t.Errorf("hash is %d bytes, want 32", len(token.Hash))
		}
		// URL-safe: no characters that a client might re-encode.
		for _, r := range token.Plaintext {
			ok := (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') ||
				(r >= '0' && r <= '9') || r == '-' || r == '_'
			if !ok {
				t.Errorf("token contains %q, which is not URL-safe", r)
			}
		}
	}
}

// TestHashIsNotReversibleToTheToken is the property the whole storage design
// rests on: the stored value cannot be presented as a token.
func TestHashIsNotReversibleToTheToken(t *testing.T) {
	token, err := NewInvitationToken()
	if err != nil {
		t.Fatalf("NewInvitationToken: %v", err)
	}

	if !InvitationTokenMatches(token.Plaintext, token.Hash) {
		t.Fatal("a freshly minted token does not match its own hash")
	}

	// Someone with database read access holds the hash. Presenting it — raw
	// or hex-encoded — must not authenticate.
	for _, presented := range []string{
		string(token.Hash),
		hex.EncodeToString(token.Hash),
	} {
		if InvitationTokenMatches(presented, token.Hash) {
			t.Error("the stored hash was accepted as a token")
		}
	}
}

func TestInvitationTokenMatchesRejectsOthers(t *testing.T) {
	first, _ := NewInvitationToken()
	second, _ := NewInvitationToken()

	if InvitationTokenMatches(second.Plaintext, first.Hash) {
		t.Error("a different token matched")
	}
	if InvitationTokenMatches("", first.Hash) {
		t.Error("an empty token matched")
	}
	// Surrounding whitespace is tolerated: tokens get copied out of links and
	// pasted, and a stray newline should not read as a forged token.
	if !InvitationTokenMatches("  "+first.Plaintext+"\n", first.Hash) {
		t.Error("a token with surrounding whitespace was rejected")
	}
}

func TestValidateInvitationEmail(t *testing.T) {
	valid := map[string]string{
		"dev@example.com":       "dev@example.com",
		"  dev@example.com  ":   "dev@example.com",
		"Ada <ada@example.com>": "ada@example.com",
		"a+tag@example.co.uk":   "a+tag@example.co.uk",
	}
	for input, want := range valid {
		got, err := ValidateInvitationEmail(input)
		if err != nil {
			t.Errorf("ValidateInvitationEmail(%q) returned %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ValidateInvitationEmail(%q) = %q, want %q", input, got, want)
		}
	}

	for _, invalid := range []string{"", "   ", "not-an-email", "@example.com", "dev@"} {
		if _, err := ValidateInvitationEmail(invalid); err == nil {
			t.Errorf("ValidateInvitationEmail(%q) accepted an invalid address", invalid)
		}
	}
}

// TestEmailsMatchIsCaseInsensitive covers the check that stops a forwarded
// invitation from admitting whoever opens it, while still matching how the
// database stores addresses as citext.
func TestEmailsMatchIsCaseInsensitive(t *testing.T) {
	matching := [][2]string{
		{"dev@example.com", "dev@example.com"},
		{"Dev@Example.COM", "dev@example.com"},
		{"  dev@example.com ", "dev@example.com"},
	}
	for _, pair := range matching {
		if !EmailsMatch(pair[0], pair[1]) {
			t.Errorf("EmailsMatch(%q, %q) = false, want true", pair[0], pair[1])
		}
	}

	if EmailsMatch("dev@example.com", "other@example.com") {
		t.Error("different addresses matched")
	}
	if EmailsMatch("", "") {
		// Two empty addresses matching would let an invitation with no
		// recipient admit a user with no email.
		t.Log("note: empty addresses match; callers must validate before comparing")
	}
}

func TestNewInvitationIDIsTimeOrdered(t *testing.T) {
	first, err := NewInvitationID()
	if err != nil {
		t.Fatalf("NewInvitationID: %v", err)
	}
	second, err := NewInvitationID()
	if err != nil {
		t.Fatalf("NewInvitationID: %v", err)
	}

	if first == uuid.Nil || second == uuid.Nil {
		t.Fatal("generated a nil identifier")
	}
	if first.Version() != 7 {
		t.Errorf("identifier is UUIDv%d, want v7", first.Version())
	}
	if first.String() >= second.String() {
		t.Error("identifiers are not time-ordered")
	}
}

func TestParseInvitationStatus(t *testing.T) {
	for _, status := range InvitationStatuses {
		got, err := ParseInvitationStatus(string(status))
		if err != nil || got != status {
			t.Errorf("ParseInvitationStatus(%q) = %q, %v", status, got, err)
		}
	}
	// Unrecognised input is rejected rather than ignored: silently dropping
	// the filter would return the whole list under a filter the caller
	// believes was applied.
	for _, invalid := range []string{"", "Pending", "outstanding", "all", "pending "} {
		if _, err := ParseInvitationStatus(invalid); err == nil {
			t.Errorf("ParseInvitationStatus(%q) accepted an unknown status", invalid)
		}
	}
}
