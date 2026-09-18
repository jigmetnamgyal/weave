package application

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ErrOutboxClaimLost is returned when a settling update finds its claim gone.
//
// The publisher paused past its lease and another claimed the row. Reported
// rather than swallowed: a publisher that discovers this has been working on
// someone else's row and must stop, not retry.
var ErrOutboxClaimLost = errors.New("this publisher's outbox claim expired and was taken over")

// Outbox tuning.
//
// The lease is the window a publisher has to deliver before another may take
// over. Long enough that a slow Temporal call does not lose the row, short
// enough that a dead publisher is not waited on — and the claim function
// refuses anything outside ten seconds to ten minutes, so a mistake here is a
// loud error rather than a stranded queue.
const (
	OutboxLease = 60 * time.Second
	// OutboxLeaseSafetyMargin is how much lease a publisher keeps in hand.
	//
	// A delivery started with seconds left finishes after the lease has gone,
	// and another publisher is already holding the row — so the work is done
	// twice and the fence rejects the settle. Stopping early wastes a poll
	// interval; not stopping wastes an attempt on every remaining row.
	OutboxLeaseSafetyMargin = 15 * time.Second
	// OutboxBatchSize is small enough that one lease comfortably covers a
	// serial pass at the activity timeout, and large enough that a backlog
	// drains in a few polls rather than a few hundred.
	OutboxBatchSize = 10
	// OutboxPollInterval is how often an idle publisher looks again. Short
	// enough that a session starts promptly; long enough that an idle
	// deployment is not a busy loop against the database.
	OutboxPollInterval = 2 * time.Second

	// OutboxMaxAttempts is what bounds the duplicate guarantee.
	//
	// The workflow id stops a duplicate publish, but only while Temporal still
	// remembers the closed execution — a promise with an expiry date. A row
	// that could retry forever would outlive it, and the duplicate would
	// return on the row least likely to be looked at. The ceiling means a row
	// becomes terminal long before the protection lapses.
	OutboxMaxAttempts = 10
)

// OutboxBackoff returns how long to wait before trying a row again.
//
// Exponential with a ceiling: a transient failure clears quickly, and a
// persistent one stops hammering whatever is failing. Capped well below the
// point where attempts run out, so the ceiling is reached by failing rather
// than by waiting.
func OutboxBackoff(attempts int32) time.Duration {
	const base = 2 * time.Second
	const max = 2 * time.Minute

	backoff := base
	for range attempts - 1 {
		backoff *= 2
		if backoff >= max {
			return max
		}
	}
	return backoff
}

// ClaimedOutboxEvent is a row this publisher currently holds.
type ClaimedOutboxEvent struct {
	ID uuid.UUID
	// WorkspaceID comes from the row, and is what every write after the claim
	// is scoped by. It is never taken from a caller: a privileged claim that
	// accepted one would be an authorization bypass through a user-controlled
	// key.
	WorkspaceID uuid.UUID
	Topic       string
	SubjectID   uuid.UUID
	Payload     []byte
	// Attempts includes this one — the claim increments it — so a publisher
	// can tell how close the row is to its ceiling without a second read.
	Attempts int32
	// Claimant is the fence. Every settling update must carry it.
	Claimant uuid.UUID
}

// OutboxRepository is the persistence port for the publisher.
type OutboxRepository interface {
	// Claim takes a batch of due rows across every workspace. It is the only
	// privileged read in the publisher's path; everything after is scoped by
	// the workspace on the claimed row.
	Claim(ctx context.Context, batchSize int, lease time.Duration, maxAttempts int) ([]ClaimedOutboxEvent, error)
	Complete(ctx context.Context, event ClaimedOutboxEvent) error
	Retry(ctx context.Context, event ClaimedOutboxEvent, backoff time.Duration, reason string) error
	Terminate(ctx context.Context, event ClaimedOutboxEvent, reason string) error
}
