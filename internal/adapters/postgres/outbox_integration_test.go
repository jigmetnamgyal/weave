package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

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
func claimFor(t *testing.T, store *postgres.OutboxStore, workspaceID uuid.UUID) (application.ClaimedOutboxEvent, bool) {
	t.Helper()
	claimed, err := store.Claim(context.Background(), application.OutboxBatchSize, application.OutboxLease)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	for _, event := range claimed {
		if event.WorkspaceID == workspaceID {
			return event, true
		}
	}
	return application.ClaimedOutboxEvent{}, false
}

// mustClaimFor fails when this workspace's row is not claimable.
func mustClaimFor(t *testing.T, store *postgres.OutboxStore, workspaceID uuid.UUID) application.ClaimedOutboxEvent {
	t.Helper()
	event, found := claimFor(t, store, workspaceID)
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
	claimed := mustClaimFor(t, store, fixture.workspace.ID)

	// While the lease is live the row is invisible to the next publisher.
	if _, found := claimFor(t, store, fixture.workspace.ID); found {
		t.Fatal("the row was claimed twice while its lease was live")
	}

	// Expire it the way time would, without waiting out the lease.
	if _, err := pool.Exec(context.Background(),
		"UPDATE outbox_events SET leased_until = now() - interval '1 second' WHERE id = $1",
		claimed.ID); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}

	recovered, found := claimFor(t, store, fixture.workspace.ID)
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
			events, err := store.Claim(context.Background(), 10, application.OutboxLease)
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
			}
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

	stale := mustClaimFor(t, store, fixture.workspace.ID)
	if _, err := pool.Exec(context.Background(),
		"UPDATE outbox_events SET leased_until = now() - interval '1 second' WHERE id = $1",
		stale.ID); err != nil {
		t.Fatalf("expire the lease: %v", err)
	}
	current := mustClaimFor(t, store, fixture.workspace.ID)
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

	claimed := mustClaimFor(t, store, fixture.workspace.ID)
	if err := store.Retry(context.Background(), claimed, time.Millisecond, "a transient failure"); err != nil {
		t.Fatalf("retry: %v", err)
	}

	again := mustClaimFor(t, store, fixture.workspace.ID)
	if again.Attempts != 2 {
		t.Errorf("attempts = %d after one retry, want 2", again.Attempts)
	}
	if err := store.Complete(context.Background(), again); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// A completed row is never claimed again.
	if _, found := claimFor(t, store, fixture.workspace.ID); found {
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

	claimed := mustClaimFor(t, store, fixture.workspace.ID)
	if err := store.Terminate(context.Background(), claimed, "the task was archived"); err != nil {
		t.Fatalf("terminate: %v", err)
	}

	if _, found := claimFor(t, store, fixture.workspace.ID); found {
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
