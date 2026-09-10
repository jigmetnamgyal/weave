package domain

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Errors the invitation domain raises.
var (
	// ErrInvalidInvitation is returned when input fails an invitation
	// invariant.
	ErrInvalidInvitation = errors.New("invalid invitation")
	// ErrInvitationNotUsable is the single error returned for an invitation
	// that is unknown, expired, revoked or already accepted.
	//
	// One error for four states on purpose. Distinguishing them tells someone
	// guessing tokens which guesses were once real, which turns a brute-force
	// attempt into a search with feedback.
	ErrInvitationNotUsable = errors.New("invitation is not usable")
	// ErrInvitationWrongRecipient is returned when the signed-in user's email
	// does not match the invitation's.
	ErrInvitationWrongRecipient = errors.New("invitation was issued to a different address")
)

const (
	// InvitationLifetime is how long an invitation remains usable.
	//
	// Long enough to survive a weekend and a forwarded message; short enough
	// that a link left in an inbox does not stay a way in indefinitely.
	InvitationLifetime = 7 * 24 * time.Hour

	// invitationTokenBytes is the entropy behind a token. 256 bits: the token
	// is the only thing standing between a stranger and a workspace, and it
	// travels in a URL where it may be logged or shoulder-surfed.
	invitationTokenBytes = 32

	// invitationEmailMaxLen bounds the stored address.
	invitationEmailMaxLen = 320
)

// InvitationStatus is the state of an invitation.
//
// Derived from the timestamps rather than stored, so it cannot disagree with
// them. A stored status column is a second source of truth that drifts the
// first time a row is updated without it.
type InvitationStatus string

// The states an invitation can be in.
const (
	InvitationPending  InvitationStatus = "pending"
	InvitationAccepted InvitationStatus = "accepted"
	InvitationRevoked  InvitationStatus = "revoked"
	InvitationExpired  InvitationStatus = "expired"
)

// InvitationStatuses is every status an invitation can report.
var InvitationStatuses = []InvitationStatus{
	InvitationPending, InvitationAccepted, InvitationRevoked, InvitationExpired,
}

// ParseInvitationStatus converts external input into a status, rejecting
// anything unknown.
func ParseInvitationStatus(value string) (InvitationStatus, error) {
	status := InvitationStatus(value)
	for _, known := range InvitationStatuses {
		if status == known {
			return status, nil
		}
	}
	return "", fmt.Errorf("%w: unknown status %q", ErrInvalidInvitation, value)
}

// Invitation is an offer to join a workspace at a given role.
type Invitation struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	Email       string
	Role        Role
	InvitedBy   uuid.UUID
	ExpiresAt   time.Time
	CreatedAt   time.Time
	AcceptedAt  time.Time
	AcceptedBy  uuid.UUID
	RevokedAt   time.Time
	RevokedBy   uuid.UUID
}

// Status reports the invitation's state at the given time.
//
// Terminal states win over expiry: an invitation accepted before it expired
// reads as accepted, not expired.
func (i Invitation) Status(now time.Time) InvitationStatus {
	switch {
	case !i.RevokedAt.IsZero():
		return InvitationRevoked
	case !i.AcceptedAt.IsZero():
		return InvitationAccepted
	case !i.ExpiresAt.After(now):
		return InvitationExpired
	default:
		return InvitationPending
	}
}

// Usable reports whether the invitation can still be accepted.
func (i Invitation) Usable(now time.Time) bool {
	return i.Status(now) == InvitationPending
}

// InvitationToken is a freshly minted token and its hash.
//
// The plaintext is returned to the issuer exactly once and never persisted;
// only Hash reaches the database.
type InvitationToken struct {
	Plaintext string
	Hash      []byte
}

// NewInvitationToken mints a token.
func NewInvitationToken() (InvitationToken, error) {
	raw := make([]byte, invitationTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return InvitationToken{}, fmt.Errorf("generate invitation token: %w", err)
	}

	// URL-safe and unpadded: the token travels in a link, and padding
	// characters invite mangling by clients that re-encode URLs.
	plaintext := base64.RawURLEncoding.EncodeToString(raw)

	return InvitationToken{Plaintext: plaintext, Hash: HashInvitationToken(plaintext)}, nil
}

// HashInvitationToken returns the stored form of a token.
//
// A plain SHA-256 rather than a password hash: the token is 256 bits of
// uniform randomness, so there is no dictionary to attack and no work factor
// worth paying on every acceptance. What the hash buys is that a database
// dump cannot be replayed as a set of working invitations.
func HashInvitationToken(plaintext string) []byte {
	sum := sha256.Sum256([]byte(strings.TrimSpace(plaintext)))
	return sum[:]
}

// InvitationTokenMatches compares a presented token against a stored hash in
// constant time.
func InvitationTokenMatches(plaintext string, hash []byte) bool {
	presented := HashInvitationToken(plaintext)
	return subtle.ConstantTimeCompare(presented, hash) == 1
}

// NewInvitationID returns an identifier for a new invitation.
func NewInvitationID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate invitation id: %w", err)
	}
	return id, nil
}

// ValidateInvitationEmail normalises and checks an invited address.
func ValidateInvitationEmail(email string) (string, error) {
	trimmed := strings.TrimSpace(email)
	if trimmed == "" {
		return "", fmt.Errorf("%w: an email address is required", ErrInvalidInvitation)
	}
	if len(trimmed) > invitationEmailMaxLen {
		return "", fmt.Errorf("%w: email address is too long", ErrInvalidInvitation)
	}

	// Parses RFC 5322. Not a deliverability check — nothing short of sending
	// mail is — but it rejects the obviously malformed before storing it.
	address, err := mail.ParseAddress(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: %q is not a valid email address", ErrInvalidInvitation, trimmed)
	}
	// ParseAddress accepts `Name <addr>`; keep only the address itself.
	return address.Address, nil
}

// EmailsMatch reports whether two addresses identify the same recipient.
//
// Case-insensitive, matching how the database stores them as citext. The
// comparison is what stops a forwarded invitation from admitting whoever
// opens it.
func EmailsMatch(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}
