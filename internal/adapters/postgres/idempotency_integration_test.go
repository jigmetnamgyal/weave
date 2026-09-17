package postgres_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// scopeFor builds a scope for a seeded workspace.
func scopeFor(fixture sessionFixture) domain.IdempotencyScope {
	return domain.IdempotencyScope{
		WorkspaceID: fixture.workspace.ID,
		UserID:      fixture.owner.ID,
		Endpoint:    "createSession",
	}
}

func recordFor(fixture sessionFixture, key string, fields ...string) domain.IdempotencyRecord {
	return domain.IdempotencyRecord{
		Scope:       scopeFor(fixture),
		Key:         key,
		Fingerprint: domain.FingerprintRequest(fields...),
	}
}

// warmPool forces the connections to exist before a concurrency test starts.
//
// M4.2's first concurrency test passed against unfixed code because pgxpool
// creates its second connection lazily and the handshake outlasted the overlap
// window — the two requests never actually raced. A concurrency test nobody
// has watched fail is not evidence, and this is what makes it possible to.
func warmPool(t *testing.T, pool *pgxpool.Pool, count int) {
	t.Helper()
	held := make([]*pgxpool.Conn, 0, count)
	for range count {
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("warm pool: %v", err)
		}
		held = append(held, conn)
	}
	for _, conn := range held {
		conn.Release()
	}
}

// TestTwoConcurrentClaimsProduceOneWinnerIntegration is the test the unique
// constraint exists for, and the one a check-then-insert implementation fails.
//
// Two callers arrive with the same key at the same moment. Exactly one must be
// told to proceed; the other must be told the work is in flight — not that it
// is complete, because it is not, and not blocked until it finishes.
func TestTwoConcurrentClaimsProduceOneWinnerIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewIdempotencyStore(pool)
	fixture := seedSessionFixture(t, pool, "Idempotency Race Workspace")
	warmPool(t, pool, 2)

	const key = "same-key-both-callers"
	outcomes := make(chan application.IdempotencyOutcome, 2)
	errs := make(chan error, 2)

	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	for range 2 {
		go func() {
			defer wait.Done()
			<-start
			outcome, _, err := store.Claim(fixture.ctx,
				recordFor(fixture, key, "task", "version", ""), application.IdempotencyLease)
			outcomes <- outcome
			errs <- err
		}()
	}
	close(start)
	wait.Wait()
	close(outcomes)
	close(errs)

	for err := range errs {
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
	}

	counts := map[application.IdempotencyOutcome]int{}
	for outcome := range outcomes {
		counts[outcome]++
	}
	if counts[application.IdempotencyClaimed] != 1 {
		t.Errorf("%d callers were told to proceed, want exactly 1 — both doing the work is "+
			"the duplicate this table exists to prevent", counts[application.IdempotencyClaimed])
	}
	if counts[application.IdempotencyInFlight] != 1 {
		t.Errorf("%d callers were told the work is in flight, want exactly 1 (outcomes: %v)",
			counts[application.IdempotencyInFlight], counts)
	}
}

// TestAnExpiredLeaseIsClaimableAgainIntegration is why the claim is a lease
// and not a flag.
//
// A holder that dies mid-request leaves a claim nothing will ever clear. A
// boolean would strand the key until an operator noticed; an expiry recovers
// it without anyone intervening.
func TestAnExpiredLeaseIsClaimableAgainIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewIdempotencyStore(pool)
	fixture := seedSessionFixture(t, pool, "Idempotency Lease Workspace")

	const key = "holder-that-died"
	record := recordFor(fixture, key, "task", "version", "")

	// A lease so short it is already expiring, standing in for a process that
	// claimed and never returned.
	outcome, _, err := store.Claim(fixture.ctx, record, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if outcome != application.IdempotencyClaimed {
		t.Fatalf("first claim = %v, want claimed", outcome)
	}

	// Before it expires, a second caller is refused.
	if outcome, _, err := store.Claim(fixture.ctx, record, application.IdempotencyLease); err != nil {
		t.Fatalf("second claim: %v", err)
	} else if outcome != application.IdempotencyInFlight {
		t.Fatalf("claim while leased = %v, want in flight", outcome)
	}

	time.Sleep(60 * time.Millisecond)

	// After it expires, the key is usable again — without anyone clearing it.
	if outcome, _, err := store.Claim(fixture.ctx, record, application.IdempotencyLease); err != nil {
		t.Fatalf("claim after expiry: %v", err)
	} else if outcome != application.IdempotencyClaimed {
		t.Errorf("claim after the lease expired = %v, want claimed — a dead holder "+
			"stranded the key", outcome)
	}
}

// TestAKeyReusedForADifferentRequestIsRefusedIntegration proves the
// fingerprint is actually compared, not merely stored.
func TestAKeyReusedForADifferentRequestIsRefusedIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewIdempotencyStore(pool)
	fixture := seedSessionFixture(t, pool, "Idempotency Mismatch Workspace")

	const key = "one-key"
	if _, _, err := store.Claim(fixture.ctx,
		recordFor(fixture, key, "task-a", "version", ""), application.IdempotencyLease); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	outcome, _, err := store.Claim(fixture.ctx,
		recordFor(fixture, key, "task-b", "version", ""), application.IdempotencyLease)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if outcome != application.IdempotencyMismatch {
		t.Errorf("a key reused for a different request = %v, want a mismatch — answering "+
			"with the first result would return work that was not asked for", outcome)
	}
}

// TestAKeyIsScopedToItsWorkspaceIntegration checks the scope is real: the same
// string from two tenants is two keys.
func TestAKeyIsScopedToItsWorkspaceIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewIdempotencyStore(pool)
	mine := seedSessionFixture(t, pool, "Idempotency Scope Mine")
	theirs := seedSessionFixture(t, pool, "Idempotency Scope Theirs")

	const key = "a-common-string"
	if outcome, _, err := store.Claim(mine.ctx,
		recordFor(mine, key, "task", "version", ""), application.IdempotencyLease); err != nil {
		t.Fatalf("claim in the first workspace: %v", err)
	} else if outcome != application.IdempotencyClaimed {
		t.Fatalf("first claim = %v, want claimed", outcome)
	}

	if outcome, _, err := store.Claim(theirs.ctx,
		recordFor(theirs, key, "task", "version", ""), application.IdempotencyLease); err != nil {
		t.Fatalf("claim in the second workspace: %v", err)
	} else if outcome != application.IdempotencyClaimed {
		t.Errorf("the same key in another workspace = %v, want claimed — one tenant's key "+
			"must not refuse another's first attempt", outcome)
	}
}

// TestReleasingAKeyLetsARetryProceedIntegration covers the fourth outcome: the
// work failed, so the key goes back at once rather than making a legitimate
// retry wait out the lease.
func TestReleasingAKeyLetsARetryProceedIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewIdempotencyStore(pool)
	fixture := seedSessionFixture(t, pool, "Idempotency Release Workspace")

	const key = "work-that-failed"
	record := recordFor(fixture, key, "task", "version", "")

	if _, _, err := store.Claim(fixture.ctx, record, application.IdempotencyLease); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.Release(fixture.ctx, record.Scope, key); err != nil {
		t.Fatalf("release: %v", err)
	}

	outcome, _, err := store.Claim(fixture.ctx, record, application.IdempotencyLease)
	if err != nil {
		t.Fatalf("claim after release: %v", err)
	}
	if outcome != application.IdempotencyClaimed {
		t.Errorf("claim after release = %v, want claimed — a failed request held its key", outcome)
	}
}

// TestIdempotencyKeysAreClosedWithoutContextIntegration connects as weave_app,
// which is the only way this proves anything: the owner is a superuser locally
// and bypasses every policy.
func TestIdempotencyKeysAreClosedWithoutContextIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	ctx := context.Background()

	store := postgres.NewIdempotencyStore(ownerPool)
	fixture := seedSessionFixture(t, ownerPool, "Idempotency RLS Workspace")
	if _, _, err := store.Claim(fixture.ctx,
		recordFor(fixture, "hidden", "task", "version", ""), application.IdempotencyLease); err != nil {
		t.Fatalf("claim: %v", err)
	}

	var count int
	if err := appPool.QueryRow(ctx, "SELECT count(*) FROM idempotency_keys").Scan(&count); err != nil {
		t.Fatalf("select: %v", err)
	}
	if count != 0 {
		t.Errorf("idempotency_keys returned %d rows with no tenant context, want 0", count)
	}

	other := seedUser(t, ownerPool)
	otherWorkspace := seedWorkspace(t, ownerPool, other, "Other Idempotency RLS Workspace")

	tx, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	setTenant(t, ctx, tx, other.ID.String(), otherWorkspace.ID.String())

	if err := tx.QueryRow(ctx,
		"SELECT count(*) FROM idempotency_keys WHERE workspace_id = $1",
		fixture.workspace.ID).Scan(&count); err != nil {
		t.Fatalf("cross-tenant select: %v", err)
	}
	if count != 0 {
		t.Errorf("idempotency_keys returned %d rows for another tenant's workspace, want 0", count)
	}
}
