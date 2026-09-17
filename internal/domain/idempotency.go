package domain

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Errors the idempotency domain raises.
var (
	// ErrInvalidIdempotencyKey is returned when a caller-supplied key is not
	// usable.
	ErrInvalidIdempotencyKey = errors.New("invalid idempotency key")
	// ErrIdempotencyKeyReused is returned when a key arrives again with a
	// different request. That is a caller bug, and answering it with the first
	// result would hand back a session for work they did not ask for.
	ErrIdempotencyKeyReused = errors.New("idempotency key already used for a different request")
	// ErrIdempotentRequestInFlight is returned when an earlier attempt holds
	// the key and has not finished. A refusal the caller can retry, rather
	// than a fabricated success.
	ErrIdempotentRequestInFlight = errors.New("an earlier request with this idempotency key is still running")
)

// idempotencyKeyMaxLen matches the CHECK constraint in the migration, counted
// in characters for the reason every other bound in this package is.
const idempotencyKeyMaxLen = 255

// ValidateIdempotencyKey checks a caller-supplied key.
//
// Trimmed, because surrounding whitespace is never meant and a key that
// differs from its own retry by a stray space is worse than no key at all.
// Not otherwise constrained: the key is the caller's to choose, and refusing
// anything that is not a UUID would reject a perfectly good hash of their own
// request.
func ValidateIdempotencyKey(key string) (string, error) {
	trimmed := strings.TrimSpace(key)
	switch {
	case trimmed == "":
		return "", fmt.Errorf("%w: it is empty", ErrInvalidIdempotencyKey)
	case utf8.RuneCountInString(trimmed) > idempotencyKeyMaxLen:
		return "", fmt.Errorf("%w: it may be at most %d characters",
			ErrInvalidIdempotencyKey, idempotencyKeyMaxLen)
	}
	// Control characters would travel into a log line and a database row, and
	// nothing about a key needs them.
	for _, r := range trimmed {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("%w: it contains a control character", ErrInvalidIdempotencyKey)
		}
	}
	return trimmed, nil
}

// IdempotencyScope is what makes two identical key strings different keys.
//
// The workspace and the user are in it because a key is a caller's own string
// and two callers may pick the same one. The endpoint is in it because the
// same string against two different mutations is two unrelated requests.
type IdempotencyScope struct {
	WorkspaceID uuid.UUID
	UserID      uuid.UUID
	// Endpoint is a stable identifier for the operation, not a URL path — a
	// path carries identifiers that vary between requests that should share a
	// scope. The operation ids from the contract are the natural choice.
	Endpoint string
}

// IdempotencyRecord is what is known about a key.
type IdempotencyRecord struct {
	Scope IdempotencyScope
	Key   string
	// Fingerprint is the canonical hash of the request, so a key replayed with
	// a different body is caught rather than answered.
	Fingerprint []byte
	// Response is present only once the work has finished.
	Response *IdempotentResponse
}

// IdempotentResponse is the answer to replay.
//
// The status and the body bytes as they were first sent — not a re-encoding,
// which would let a field order or a formatting change slip in between the two
// answers to what the caller must be able to treat as one request.
type IdempotentResponse struct {
	Status int
	Body   []byte
	// OriginRequestID is the request id of the attempt that did the work. It
	// is not sent back: the response carries the retry's own id, so it matches
	// the log line for the request actually made. This is recorded so the
	// replay's log line can name the original and an operator can get from one
	// to the other.
	OriginRequestID string
}

// FingerprintRequest reduces a request to the bytes that decide whether two
// attempts are the same request.
//
// **Canonical, not raw.** A byte comparison rejects retries that are
// equivalent: a client that reorders JSON members, adds whitespace, or sends a
// field empty where it omitted it before has not changed what it asked for.
// The caller passes the request's meaningful fields already normalised, in a
// fixed order, and this hashes them with a separator that cannot appear in
// them — without one, ("ab", "c") and ("a", "bc") would fingerprint alike.
func FingerprintRequest(fields ...string) []byte {
	hash := sha256.New()
	for _, field := range fields {
		// The length prefix is the separator. A delimiter byte would only move
		// the problem to inputs containing that byte.
		fmt.Fprintf(hash, "%d:", len(field))
		hash.Write([]byte(field))
	}
	return hash.Sum(nil)
}

// NewIdempotencyRecordID returns an identifier for a new record.
func NewIdempotencyRecordID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate idempotency record id: %w", err)
	}
	return id, nil
}
