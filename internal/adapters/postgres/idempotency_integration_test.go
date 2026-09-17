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
			outcome, _, _, err := store.Claim(fixture.ctx,
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
	outcome, _, _, err := store.Claim(fixture.ctx, record, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	if outcome != application.IdempotencyClaimed {
		t.Fatalf("first claim = %v, want claimed", outcome)
	}

	// Before it expires, a second caller is refused.
	if outcome, _, _, err := store.Claim(fixture.ctx, record, application.IdempotencyLease); err != nil {
		t.Fatalf("second claim: %v", err)
	} else if outcome != application.IdempotencyInFlight {
		t.Fatalf("claim while leased = %v, want in flight", outcome)
	}

	time.Sleep(60 * time.Millisecond)

	// After it expires, the key is usable again — without anyone clearing it.
	if outcome, _, _, err := store.Claim(fixture.ctx, record, application.IdempotencyLease); err != nil {
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
	if _, _, _, err := store.Claim(fixture.ctx,
		recordFor(fixture, key, "task-a", "version", ""), application.IdempotencyLease); err != nil {
		t.Fatalf("first claim: %v", err)
	}

	outcome, _, _, err := store.Claim(fixture.ctx,
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
	if outcome, _, _, err := store.Claim(mine.ctx,
		recordFor(mine, key, "task", "version", ""), application.IdempotencyLease); err != nil {
		t.Fatalf("claim in the first workspace: %v", err)
	} else if outcome != application.IdempotencyClaimed {
		t.Fatalf("first claim = %v, want claimed", outcome)
	}

	if outcome, _, _, err := store.Claim(theirs.ctx,
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

	_, _, claimant, err := store.Claim(fixture.ctx, record, application.IdempotencyLease)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := store.Release(fixture.ctx, record.Scope, key, claimant); err != nil {
		t.Fatalf("release: %v", err)
	}

	outcome, _, _, err2 := store.Claim(fixture.ctx, record, application.IdempotencyLease)
	if err2 != nil {
		t.Fatalf("claim after release: %v", err2)
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
	if _, _, _, err := store.Claim(fixture.ctx,
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

// TestPruneRemovesExpiredRecordsIntegration is the retention promise, and it
// needs the application role to mean anything.
//
// The sweep runs from a background goroutine with no tenant context. Under
// FORCE row-level security that makes every policy match nothing, so a DELETE
// succeeds having removed no rows and reports no error — retention silently
// never happening, while these rows hold a response body carrying identifiers.
func TestPruneRemovesExpiredRecordsIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	fixture := seedSessionFixture(t, ownerPool, "Idempotency Prune Workspace")

	if _, _, _, err := postgres.NewIdempotencyStore(ownerPool).Claim(fixture.ctx,
		recordFor(fixture, "old-key", "task", "version", ""), application.IdempotencyLease); err != nil {
		t.Fatalf("claim: %v", err)
	}
	// Age it past any plausible retention window.
	if _, err := ownerPool.Exec(context.Background(),
		"UPDATE idempotency_keys SET created_at = now() - interval '30 days' WHERE workspace_id = $1",
		fixture.workspace.ID); err != nil {
		t.Fatalf("age the record: %v", err)
	}

	// The application pool and a bare context, exactly as the sweep runs.
	removed, err := postgres.NewIdempotencyStore(appPool).Prune(context.Background(), 24*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed == 0 {
		t.Fatal("the sweep removed nothing: retention is not happening, and it reports success")
	}

	var remaining int
	if err := ownerPool.QueryRow(context.Background(),
		"SELECT count(*) FROM idempotency_keys WHERE workspace_id = $1",
		fixture.workspace.ID).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d expired records survived the sweep", remaining)
	}
}

// TestALateHolderCannotReleaseTheClaimThatReplacedItIntegration is the fencing
// the lease needs to be safe rather than merely optimistic.
//
// A holder that is *slow* rather than dead comes back after its lease expired
// and someone else reclaimed the key. Identified only by scope and key, its
// release would delete the new holder's claim — and both would then be free to
// create a session, which is the duplicate the whole mechanism exists to
// prevent, arriving through the recovery path.
func TestALateHolderCannotReleaseTheClaimThatReplacedItIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewIdempotencyStore(pool)
	fixture := seedSessionFixture(t, pool, "Idempotency Fencing Workspace")

	const key = "slow-not-dead"
	record := recordFor(fixture, key, "task", "version", "")

	// The first holder takes a lease so short it expires while it is still
	// working.
	_, _, slow, err := store.Claim(fixture.ctx, record, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	time.Sleep(60 * time.Millisecond)

	// A second caller reclaims the key, believing the first dead.
	outcome, _, current, err := store.Claim(fixture.ctx, record, application.IdempotencyLease)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if outcome != application.IdempotencyClaimed {
		t.Fatalf("reclaim = %v, want claimed", outcome)
	}
	if slow == current {
		t.Fatal("the reclaim reused the first holder's token, so there is no fence at all")
	}

	// The first holder finishes and tries to tidy up.
	if err := store.Release(fixture.ctx, record.Scope, key, slow); err != nil {
		t.Fatalf("late release: %v", err)
	}

	// The current holder's claim must still be there. If the late release
	// deleted it, this claim succeeds — and two callers are doing the work.
	after, _, _, err := store.Claim(fixture.ctx, record, application.IdempotencyLease)
	if err != nil {
		t.Fatalf("claim after the late release: %v", err)
	}
	if after != application.IdempotencyInFlight {
		t.Errorf("after a late release the key is %v, want still in flight — the slow holder "+
			"deleted the claim that replaced it", after)
	}
}

// TestAReclaimWithADifferentRequestIsRefusedIntegration covers the other half
// of the expired-lease path.
//
// Reclaiming is for a holder that died doing *this* request. A different body
// arriving on the same key is still a caller bug, and quietly overwriting the
// stored fingerprint would hide it — after which the caller's two different
// requests share one key and neither is what they asked for.
func TestAReclaimWithADifferentRequestIsRefusedIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewIdempotencyStore(pool)
	fixture := seedSessionFixture(t, pool, "Idempotency Reclaim Workspace")

	const key = "reused-after-expiry"
	if _, _, _, err := store.Claim(fixture.ctx,
		recordFor(fixture, key, "task-a", "version", ""), 10*time.Millisecond); err != nil {
		t.Fatalf("first claim: %v", err)
	}
	time.Sleep(60 * time.Millisecond)

	outcome, _, _, err := store.Claim(fixture.ctx,
		recordFor(fixture, key, "task-b", "version", ""), application.IdempotencyLease)
	if err != nil {
		t.Fatalf("reclaim with a different body: %v", err)
	}
	if outcome != application.IdempotencyMismatch {
		t.Errorf("reclaim with a different request = %v, want a mismatch — the expired "+
			"lease let a different body overwrite the fingerprint", outcome)
	}
}

// TestTheSweepRefusesToDeleteUnexpiredRecordsIntegration guards the privilege
// the prune function holds.
//
// It deletes across every workspace and runs as a role that looks past the
// policies, so an arbitrary interval is an arbitrary amount of deletion: a
// zero or negative one would empty the table for every tenant, and the next
// retry of anything would then create a duplicate. Being callable only by
// weave_app is not the protection — a compromised credential or an injection
// path is exactly what a privileged function has to survive.
func TestTheSweepRefusesToDeleteUnexpiredRecordsIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	store := postgres.NewIdempotencyStore(appPool)
	fixture := seedSessionFixture(t, ownerPool, "Idempotency Floor Workspace")

	if _, _, _, err := postgres.NewIdempotencyStore(ownerPool).Claim(fixture.ctx,
		recordFor(fixture, "fresh-key", "task", "version", ""), application.IdempotencyLease); err != nil {
		t.Fatalf("claim: %v", err)
	}

	for name, retention := range map[string]time.Duration{
		"zero":     0,
		"negative": -24 * time.Hour,
		"an hour":  time.Hour,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.Prune(context.Background(), retention); err == nil {
				t.Errorf("a %s retention was accepted; it would delete unexpired records "+
					"across every workspace", name)
			}
		})
	}

	// The record is still there, which is the thing that matters.
	var remaining int
	if err := ownerPool.QueryRow(context.Background(),
		"SELECT count(*) FROM idempotency_keys WHERE workspace_id = $1",
		fixture.workspace.ID).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Errorf("%d records remain, want 1 — a refused sweep deleted something", remaining)
	}
}
