package application

import (
	"context"
	"time"

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
	Claim(ctx context.Context, record domain.IdempotencyRecord, lease time.Duration) (IdempotencyOutcome, domain.IdempotencyRecord, error)
	// Release gives the key back when the work failed, so a legitimate retry
	// is not refused for the length of the lease.
	Release(ctx context.Context, scope domain.IdempotencyScope, key string) error
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
	// OriginRequestID is the request id of this attempt. Stored, not replayed
	// — see IdempotentResponse.
	OriginRequestID string
	// Render turns the result into the response to store. Called inside the
	// transaction, with the thing that was just written.
	Render func(domain.Session) (status int, body []byte, err error)
}
