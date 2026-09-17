package application

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// IdempotencyOutcome is what trying to claim a key produced.
//
// Four states rather than "seen" and "not seen", because the naive two answer
// "already done" to a caller whose first attempt has not finished — which is
// how the M3.1 delivery path acknowledged work GitHub then never retried.
type IdempotencyOutcome int

const (
	// IdempotencyClaimed means this attempt holds the key and should proceed.
	// Either it is the first, or the previous holder's lease expired.
	IdempotencyClaimed IdempotencyOutcome = iota
	// IdempotencyInFlight means an earlier attempt holds the key and has not
	// finished. The caller is refused and can retry.
	IdempotencyInFlight
	// IdempotencyComplete means the work is done and the stored response
	// should be replayed.
	IdempotencyComplete
	// IdempotencyMismatch means the key arrived with a different request. A
	// caller bug: answering with the first result would hand back work they
	// did not ask for.
	IdempotencyMismatch
)

// ErrIdempotencyFenced is returned when a completion finds its claim has been
// taken over.
//
// The attempt was slow rather than dead: its lease expired, another caller
// reclaimed the key, and both were doing the work. Failing here stops this one
// committing under a record that now belongs to the other — a caller replaying
// it would otherwise be handed the wrong session.
var ErrIdempotencyFenced = errors.New("this attempt's idempotency claim was taken over after its lease expired")

// IdempotencyRetention is how long a record is kept.
//
// A retry is recovery from a lost response, which happens inside a
// request-timeout window rather than days later — so the seven days the
// webhook delivery log keeps, sized to GitHub's redelivery window, would be
// wrong here for a reason rather than merely generous. These rows also hold a
// response body carrying identifiers, so they are customer data and shorter is
// better. Past this, a retry is a new request and creates a new session.
const IdempotencyRetention = 24 * time.Hour

// IdempotencyLease is how long a claim survives its holder.
//
// A session create is three transactions and no network calls, so a holder
// still running after a minute has died or hung. Being wrong costs one
// duplicate; being too generous costs a key nothing can use.
const IdempotencyLease = 60 * time.Second

// IdempotencyRepository is the persistence port for keys.
type IdempotencyRepository interface {
	// Claim takes the key in its own transaction, which is what lets a
	// concurrent caller be told the work is in flight rather than blocking on
	// it. The returned record carries the stored response when the outcome is
	// IdempotencyComplete.
	// The returned claimant is the fencing token for this attempt: a holder
	// whose lease expired while it was still running must not be able to
	// complete or release the claim that replaced it.
	Claim(ctx context.Context, record domain.IdempotencyRecord, lease time.Duration) (IdempotencyOutcome, domain.IdempotencyRecord, uuid.UUID, error)
	// Release gives the key back when the work failed, so a legitimate retry
	// is not refused for the length of the lease. Fenced on the claimant.
	Release(ctx context.Context, scope domain.IdempotencyScope, key string, claimant uuid.UUID) error
}

// IdempotentCompletion is the instruction to record a response alongside the
// work that produced it.
//
// It is passed into the store that does the work, and written inside that
// store's transaction. Recording it afterwards would leave a window where the
// session committed and the response did not: the lease would expire and the
// retry would create a second session, which is the failure this whole
// mechanism exists to prevent, reappearing at the last step.
type IdempotentCompletion struct {
	Scope domain.IdempotencyScope
	Key   string
	// Claimant fences the completion. Without it a slow holder could write its
	// response over the record belonging to whoever reclaimed the key after
	// its lease expired, while both were creating sessions.
	Claimant uuid.UUID
	// OriginRequestID is the request id of this attempt. Stored, not replayed
	// — see IdempotentResponse.
	OriginRequestID string
	// Render turns the result into the response to store. Called inside the
	// transaction, with the thing that was just written.
	Render func(domain.Session) (status int, body []byte, err error)
}
