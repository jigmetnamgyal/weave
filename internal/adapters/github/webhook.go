package github

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
)

// Errors webhook verification raises. All three are deliberately reported to
// the caller as one outcome — the request is rejected — but kept distinct here
// so a log line can say which without the response doing so.
var (
	// ErrSignatureMissing is returned when the delivery carried no signature
	// header at all. Almost always something other than GitHub.
	ErrSignatureMissing = errors.New("github: delivery carried no signature")
	// ErrSignatureMalformed is returned when the header is present but not in
	// the documented form.
	ErrSignatureMalformed = errors.New("github: signature is malformed")
	// ErrSignatureMismatch is returned when the signature does not match the
	// body under our secret.
	ErrSignatureMismatch = errors.New("github: signature does not match")
)

// SignatureHeader is where GitHub puts the HMAC.
//
// The SHA-256 header specifically. GitHub also sends X-Hub-Signature, an
// HMAC-SHA1 kept for old integrations; it must be ignored, because accepting
// it would let a caller downgrade the verification to a broken hash.
const SignatureHeader = "X-Hub-Signature-256"

// DeliveryHeader carries GitHub's unique id for the delivery, which is what
// deduplication keys on.
const DeliveryHeader = "X-GitHub-Delivery"

// EventHeader names the event type.
const EventHeader = "X-GitHub-Event"

// VerifySignature checks a delivery against the shared secret.
//
// The body must be the exact bytes received, before any parsing. A handler
// that decodes JSON first and re-encodes to verify has already trusted the
// input, and would accept a body whose re-encoding happens to match while the
// original does not.
//
// The comparison is constant time. A byte-by-byte compare leaks, through
// timing, how much of a guessed signature was correct, which turns forging one
// into a series of cheap measurements rather than an exhaustive search.
func VerifySignature(secret string, body []byte, header string) error {
	header = strings.TrimSpace(header)
	if header == "" {
		return ErrSignatureMissing
	}

	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return ErrSignatureMalformed
	}
	presented, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return ErrSignatureMalformed
	}

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	if !hmac.Equal(presented, mac.Sum(nil)) {
		return ErrSignatureMismatch
	}
	return nil
}
