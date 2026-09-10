package application

import (
	"context"
	"errors"
	"time"
)

// Errors a TokenVerifier may return. Callers distinguish them with errors.Is
// so that transport layers can map them to responses without inspecting
// provider-specific error text.
var (
	// ErrTokenMissing is returned when no credential was presented.
	ErrTokenMissing = errors.New("no authentication token presented")
	// ErrTokenInvalid is returned when a credential was presented but is not
	// trustworthy: malformed, badly signed, wrong issuer or audience, wrong
	// algorithm, or expired.
	//
	// The distinction between "expired" and "forged" is deliberately not
	// exposed. It tells an attacker which of the two they achieved, and every
	// caller treats them identically.
	ErrTokenInvalid = errors.New("authentication token is not valid")
	// ErrVerifierUnavailable is returned when the verifier cannot reach the
	// key material it needs. It is a dependency failure, not a credential
	// failure, and must never be treated as "not authenticated".
	ErrVerifierUnavailable = errors.New("token verifier is unavailable")
)

// Identity is the subset of a verified token the product acts on.
//
// It is provider-neutral by construction: nothing here names Clerk, and an
// adapter for a different OIDC provider populates the same fields.
type Identity struct {
	// Subject is the identity provider's stable identifier for this person.
	Subject string
	// Email is the verified primary email address.
	Email string
	// DisplayName and AvatarURL are optional profile details.
	DisplayName string
	AvatarURL   string
	// ExpiresAt is when the presented token stops being valid.
	ExpiresAt time.Time
}

// TokenVerifier turns a raw bearer token into a verified Identity.
//
// Implementations must validate signature, issuer, audience, expiry and
// signing algorithm before returning. An implementation that cannot reach its
// key material must return ErrVerifierUnavailable rather than failing open.
type TokenVerifier interface {
	Verify(ctx context.Context, rawToken string) (Identity, error)
}
