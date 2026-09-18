package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
)

// claimFor takes rows until it finds the one belonging to this workspace.
//
// **The claim is cross-workspace by design** — the publisher polls every
// tenant's queue — so a test cannot assert anything about the global result
// without depending on which other tests are running. Every assertion here is
// about this workspace's row, which is also how the publisher behaves: it
// claims whatever is due and settles each row under its own tenant.
func claimFor(
	t *testing.T,
	pool *pgxpool.Pool,
	store *postgres.OutboxStore,
	workspaceID uuid.UUID,
) (application.ClaimedOutboxEvent, bool) {
	t.Helper()
	claimed, err := store.Claim(context.Background(), application.OutboxBatchSize, application.OutboxLease, application.OutboxMaxAttempts)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	var mine application.ClaimedOutboxEvent
	found := false
	for _, event := range claimed {
		if event.WorkspaceID == workspaceID {
			mine, found = event, true
			continue
		}
		// Put back what was not ours, completely.
		//
		// The claim is global — the publisher polls every tenant — so a test
		// running against a shared database takes rows belonging to real work
		// and to other tests. Leaving them leased delays them; leaving the
		// attempt spent is worse, because attempts are finite and a row that
		// exhausts them is terminated. Measured: before this, one test run
		// took an unrelated row from 0 attempts to 1 and left it leased.
		releaseForeign(t, pool, event.ID, event.Claimant)
	}
	return mine, found
}

// releaseForeign undoes a claim this test had no business making.
//
// Through the owner pool, restoring the row exactly: no lease, no claimant,
// and the attempt given back.
//
// **Fenced on the claimant, like every other write to a claimed row.** Without
// it this filters on id alone, and a publisher that reclaimed the row between
// the claim and this cleanup would have its lease cleared and its attempt
// decremented by a test tidying up after itself. That is the same fencing hole
// this PR fixed in the outbox and the settling updates — and it was sitting in
// the helper written to stop tests damaging other tenants' rows, which would
// have made the protection the thing doing the damage.
func releaseForeign(t *testing.T, pool *pgxpool.Pool, id, claimant uuid.UUID) {
	t.Helper()
	tag, err := pool.Exec(context.Background(),
		`UPDATE outbox_events
		 SET leased_until = NULL, claimant = NULL, attempts = GREATEST(attempts - 1, 0)
		 WHERE id = $1 AND claimant = $2`, id, claimant)
	if err != nil {
		t.Fatalf("release a row this test should not have claimed: %v", err)
	}
	if tag.RowsAffected() == 0 {
		// Somebody else owns the row now: this claim's lease expired and a
		// real publisher took it between the claim and this cleanup.
		//
		// Leaving it alone is the only correct move — decrementing a count
		// that now belongs to another claim is the corruption this helper
		// exists to avoid. The honest cost is that the attempt this test spent
		// stays spent, which is rare and preferable to the alternative. Not a
		// failure, which is why it is recorded here rather than raised.
		t.Logf("outbox row %s was reclaimed before cleanup; its attempt stays spent", id)
	}
}

// mustClaimFor fails when this workspace's row is not claimable.
func mustClaimFor(
	t *testing.T,
	pool *pgxpool.Pool,
	store *postgres.OutboxStore,
	workspaceID uuid.UUID,
) application.ClaimedOutboxEvent {
	t.Helper()
	event, found := claimFor(t, pool, store, workspaceID)
	if !found {
		t.Fatal("this workspace's outbox row was not claimable")
	}
	return event
}

// TestAnExpiredOutboxLeaseIsClaimableAgainIntegration is the first of the two
// tests M4.2 designed the protocol for and could not write, because nothing
// claimed a row.
//
// A publisher that dies holding a row must not strand it. This is why
// `leased_until` is a timestamp rather than a boolean: a flag set by a process
// that never returns is a row nothing will ever clear, and no operator would
// know to look.
func TestAnExpiredOutboxLeaseIsClaimableAgainIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewOutboxStore(pool)
	fixture := seedSessionFixture(t, pool, "Outbox Lease Workspace")
	if _, err := fixture.createSession(t, postgres.NewSessionStore(pool), nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Stands in for a publisher that claimed and never came back.
	claimed := mustClaimFor(t, pool, store, fixture.workspace.ID)

	// While the lease is live the row is invisible to the next publisher.
	if _, found := claimFor(t, pool, store, fixture.workspace.ID); found {
		t.Fatal("the row was claimed twice while its lease was live")
	}

	// Expire it the way time would, without waiting out the lease.
	if _, err := pool.Exec(context.Background(),
		"UPDATE outbox_events SET leased_until = now() - interval '1 second' WHERE id = $1",
		claimed.ID); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}

	recovered, found := claimFor(t, pool, store, fixture.workspace.ID)
	if !found {
		t.Fatal("the row was not claimable after its lease expired — a dead publisher stranded it")
	}
	if recovered.Claimant == claimed.Claimant {
		t.Error("the reclaim reused the dead publisher's token, so there is no fence")
	}
	if recovered.Attempts != 2 {
		t.Errorf("attempts = %d after a reclaim, want 2 — a row that keeps being "+
			"reclaimed must approach its ceiling", recovered.Attempts)
	}
}

// TestTwoPublishersCannotClaimTheSameRowIntegration is the second owed test.
//
// Two publishers poll at once. Each row must go to exactly one of them: both
// claiming it means both deliver it, which for this queue means two workflows
// for one session.
func TestTwoPublishersCannotClaimTheSameRowIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewOutboxStore(pool)
	fixture := seedSessionFixture(t, pool, "Outbox Race Workspace")
	if _, err := fixture.createSession(t, postgres.NewSessionStore(pool), nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	warmPool(t, pool, 2)

	type result struct {
		events []application.ClaimedOutboxEvent
		err    error
	}
	results := make(chan result, 2)
	start := make(chan struct{})
	var wait sync.WaitGroup
	wait.Add(2)
	for range 2 {
		go func() {
			defer wait.Done()
			<-start
			events, err := store.Claim(context.Background(), application.OutboxBatchSize, application.OutboxLease, application.OutboxMaxAttempts)
			results <- result{events: events, err: err}
		}()
	}
	close(start)
	wait.Wait()
	close(results)

	mine := 0
	for r := range results {
		if r.err != nil {
			t.Fatalf("claim: %v", r.err)
		}
		for _, event := range r.events {
			if event.WorkspaceID == fixture.workspace.ID {
				mine++
				continue
			}
			releaseForeign(t, pool, event.ID, event.Claimant)
		}
	}
	if mine != 1 {
		t.Errorf("%d claims across two publishers for one row, want 1 — both would "+
			"deliver it, which is two workflows for one session", mine)
	}
}

// TestAStalePublisherCannotSettleTheRowThatReplacedItIntegration is the fence,
// which the M5.1 spec's first draft applied to two of the three settling
// operations and left off the third.
func TestAStalePublisherCannotSettleTheRowThatReplacedItIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewOutboxStore(pool)
	fixture := seedSessionFixture(t, pool, "Outbox Fence Workspace")
	if _, err := fixture.createSession(t, postgres.NewSessionStore(pool), nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	stale := mustClaimFor(t, pool, store, fixture.workspace.ID)
	if _, err := pool.Exec(context.Background(),
		"UPDATE outbox_events SET leased_until = now() - interval '1 second' WHERE id = $1",
		stale.ID); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}
	current := mustClaimFor(t, pool, store, fixture.workspace.ID)
	if current.Claimant == stale.Claimant {
		t.Fatal("the reclaim reused the token, so there is no fence to test")
	}

	// Every settling operation, not an enumerated two. The terminal one is the
	// case a reviewer found missing from the spec: a stale publisher
	// quarantining the row that replaced it would stop work nobody asked to
	// stop.
	settles := map[string]func() error{
		"complete":  func() error { return store.Complete(context.Background(), stale) },
		"retry":     func() error { return store.Retry(context.Background(), stale, time.Second, "stale") },
		"terminate": func() error { return store.Terminate(context.Background(), stale, "stale") },
	}
	for name, settle := range settles {
		t.Run(name, func(t *testing.T) {
			if err := settle(); !errors.Is(err, application.ErrOutboxClaimLost) {
				t.Errorf("a stale publisher's %s returned %v, want the claim reported lost", name, err)
			}
		})
	}

	// And the current holder's row is untouched by any of it.
	var completedAt, terminatedAt, leasedUntil *time.Time
	if err := pool.QueryRow(context.Background(),
		"SELECT completed_at, terminated_at, leased_until FROM outbox_events WHERE id = $1",
		current.ID).Scan(&completedAt, &terminatedAt, &leasedUntil); err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if completedAt != nil || terminatedAt != nil {
		t.Errorf("the stale publisher settled the current holder's row: completed=%v terminated=%v",
			completedAt, terminatedAt)
	}
	if leasedUntil == nil {
		t.Error("the stale publisher cleared the current holder's lease")
	}
}

// TestSettlingAnOutboxRowIsFencedAndScopedIntegration checks the ordinary path
// works, so the fence tests above cannot pass by everything failing.
func TestSettlingAnOutboxRowIsFencedAndScopedIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewOutboxStore(pool)
	fixture := seedSessionFixture(t, pool, "Outbox Settle Workspace")
	if _, err := fixture.createSession(t, postgres.NewSessionStore(pool), nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	claimed := mustClaimFor(t, pool, store, fixture.workspace.ID)
	if err := store.Retry(context.Background(), claimed, time.Millisecond, "a transient failure"); err != nil {
		t.Fatalf("retry: %v", err)
	}

	again := mustClaimFor(t, pool, store, fixture.workspace.ID)
	if again.Attempts != 2 {
		t.Errorf("attempts = %d after one retry, want 2", again.Attempts)
	}
	if err := store.Complete(context.Background(), again); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// A completed row is never claimed again.
	if _, found := claimFor(t, pool, store, fixture.workspace.ID); found {
		t.Error("a completed row was claimed again")
	}
}

// TestATerminatedRowIsNeverClaimedAgainIntegration pins the outcome the schema
// had nowhere to put.
//
// Marking a dead row complete would lie to anyone counting deliveries; leaving
// it pending would retry it forever. It keeps its reason, because a dead row
// nobody can explain is one nobody can act on.
func TestATerminatedRowIsNeverClaimedAgainIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewOutboxStore(pool)
	fixture := seedSessionFixture(t, pool, "Outbox Terminal Workspace")
	if _, err := fixture.createSession(t, postgres.NewSessionStore(pool), nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	claimed := mustClaimFor(t, pool, store, fixture.workspace.ID)
	if err := store.Terminate(context.Background(), claimed, "the task was archived"); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	if _, found := claimFor(t, pool, store, fixture.workspace.ID); found {
		t.Error("a terminated row was claimed again")
	}

	var completedAt *time.Time
	var lastError *string
	if err := pool.QueryRow(context.Background(),
		"SELECT completed_at, last_error FROM outbox_events WHERE id = $1",
		claimed.ID).Scan(&completedAt, &lastError); err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if completedAt != nil {
		t.Error("a terminated row is marked complete, which claims work that never happened")
	}
	if lastError == nil || *lastError != "the task was archived" {
		t.Errorf("last_error = %v, want the reason kept", lastError)
	}
}

// TestAnExhaustedRowIsTerminatedByTheClaimIntegration covers the path the
// publisher's own ceiling cannot reach.
//
// `Publisher.settleRetry` gives up after enough attempts, but it only runs
// when a publisher reaches settlement. A publisher that dies mid-delivery
// never does: its lease expires, the row is claimable again, and that repeats
// forever — so the ceiling that bounds the duplicate guarantee would never
// apply on the one path most likely to need it, which is the publisher dying.
//
// Enforcing it inside the claim is what closes that, and it is terminated
// rather than skipped so the row leaves the queue with its reason instead of
// being walked past on every poll.
func TestAnExhaustedRowIsTerminatedByTheClaimIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewOutboxStore(pool)
	fixture := seedSessionFixture(t, pool, "Outbox Exhausted Workspace")
	if _, err := fixture.createSession(t, postgres.NewSessionStore(pool), nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// A publisher that claimed and died, over and over, until the attempts ran
	// out — set directly, because reproducing it honestly means killing a
	// process ten times.
	if _, err := pool.Exec(context.Background(),
		`UPDATE outbox_events SET attempts = $1, leased_until = NULL, claimant = NULL
		 WHERE workspace_id = $2`, application.OutboxMaxAttempts, fixture.workspace.ID); err != nil {
		t.Fatalf("exhaust the row: %v", err)
	}

	if _, found := claimFor(t, pool, store, fixture.workspace.ID); found {
		t.Error("a row past its attempt ceiling was claimed again, so it can outlive " +
			"the workflow deduplication that protects it")
	}

	var terminated bool
	var lastError *string
	if err := pool.QueryRow(context.Background(),
		"SELECT terminated_at IS NOT NULL, last_error FROM outbox_events WHERE workspace_id = $1",
		fixture.workspace.ID).Scan(&terminated, &lastError); err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if !terminated {
		t.Error("the exhausted row was left pending, so every poll will walk past it forever")
	}
	if lastError == nil || *lastError == "" {
		t.Error("the exhausted row kept no reason, so nobody can tell why it stopped")
	}
}

// TestATerminatedRowIsNotReportedAsPendingIntegration covers a query that
// predates the terminal column.
//
// `ListPendingOutboxEvents` was written in M4.2, when the only outcome was
// completion. Left alone it reports a row that will never be delivered as
// still waiting — which is the opposite of what terminating one means, and
// would tell an operator the queue is backed up when it is not.
func TestATerminatedRowIsNotReportedAsPendingIntegration(t *testing.T) {
	pool := newPool(t)
	sessions := postgres.NewSessionStore(pool)
	store := postgres.NewOutboxStore(pool)
	fixture := seedSessionFixture(t, pool, "Outbox Pending Report Workspace")
	if _, err := fixture.createSession(t, sessions, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	pending, err := sessions.ListPendingOutbox(fixture.ctx, fixture.workspace.ID)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending rows before terminating, want 1", len(pending))
	}

	claimed := mustClaimFor(t, pool, store, fixture.workspace.ID)
	if err := store.Terminate(context.Background(), claimed, "the task was archived"); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	after, err := sessions.ListPendingOutbox(fixture.ctx, fixture.workspace.ID)
	if err != nil {
		t.Fatalf("list pending after terminating: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("%d rows still reported as pending after termination — work that will "+
			"never be delivered reads as waiting", len(after))
	}
}

// TestReleasingAnUnstartedRowGivesBackItsAttemptIntegration is the other half
// of the lease deadline.
//
// The claim spends an attempt on every row in the batch — deliberately, so a
// publisher that dies mid-delivery still burns one and the ceiling applies to
// the case it exists for. But a row the publisher never reached has not been
// attempted, and letting that attempt stand would march it toward termination
// for no reason but our own slowness. A run of slow batches would then
// terminate work that was never once tried.
func TestReleasingAnUnstartedRowGivesBackItsAttemptIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewOutboxStore(pool)
	fixture := seedSessionFixture(t, pool, "Outbox Release Unstarted Workspace")
	if _, err := fixture.createSession(t, postgres.NewSessionStore(pool), nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	claimed := mustClaimFor(t, pool, store, fixture.workspace.ID)
	if claimed.Attempts != 1 {
		t.Fatalf("attempts = %d after one claim, want 1", claimed.Attempts)
	}

	if err := store.ReleaseUnstarted(context.Background(), claimed); err != nil {
		t.Fatalf("release: %v", err)
	}

	// Claimable at once rather than pushed out: nothing failed, so there is
	// nothing to back off from.
	again, found := claimFor(t, pool, store, fixture.workspace.ID)
	if !found {
		t.Fatal("a released row was not immediately claimable")
	}
	if again.Attempts != 1 {
		t.Errorf("attempts = %d after release and reclaim, want 1 — the unattempted "+
			"row spent an attempt it never used", again.Attempts)
	}
}

// TestTerminatingAMaxedOutErrorDoesNotJamTheQueueIntegration is why the
// termination reason is truncated.
//
// `last_error` is capped at 2000 characters and a retry can leave it exactly
// there. Appending to it then violates the CHECK — which aborts the claim
// function, so **one poison row stops every other row in every workspace from
// being claimed**. A queue that stops draining because one row failed loudly
// enough is the worst shape this can take.
func TestTerminatingAMaxedOutErrorDoesNotJamTheQueueIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewOutboxStore(pool)
	fixture := seedSessionFixture(t, pool, "Outbox Jam Workspace")
	if _, err := fixture.createSession(t, postgres.NewSessionStore(pool), nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Exhausted, with an error already at the column's limit.
	if _, err := pool.Exec(context.Background(),
		`UPDATE outbox_events SET attempts = $1, last_error = repeat('x', 2000),
		        leased_until = NULL, claimant = NULL
		 WHERE workspace_id = $2`, application.OutboxMaxAttempts, fixture.workspace.ID); err != nil {
		t.Fatalf("exhaust the row: %v", err)
	}

	// The claim must succeed. Before the fix it raised, and every workspace's
	// queue stopped with it.
	if _, err := store.Claim(context.Background(),
		application.OutboxBatchSize, application.OutboxLease, application.OutboxMaxAttempts); err != nil {
		t.Fatalf("the claim failed on a row whose error was at the column limit, "+
			"which stops every other row too: %v", err)
	}

	var length int
	var terminated bool
	if err := pool.QueryRow(context.Background(),
		"SELECT length(last_error), terminated_at IS NOT NULL FROM outbox_events WHERE workspace_id = $1",
		fixture.workspace.ID).Scan(&length, &terminated); err != nil {
		t.Fatalf("read the row: %v", err)
	}
	if !terminated {
		t.Error("the exhausted row was not terminated")
	}
	if length > 2000 {
		t.Errorf("last_error is %d characters, above the column's limit", length)
	}
}
