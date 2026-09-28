package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// eventFor builds a valid stored-form event for a session.
func eventFor(session domain.Session, runner uuid.UUID) domain.SessionEvent {
	payload, _ := json.Marshal(domain.MessageCreated{MessageID: uuid.New(), Role: "assistant", Text: "hello"})
	return domain.SessionEvent{
		EventID: uuid.New(), SessionID: session.ID, RunnerID: runner, CorrelationID: uuid.New(),
		Type: domain.EventMessageCreated, SchemaVersion: "1.0",
		OccurredAt: time.Now().UTC(), ReceivedAt: time.Now().UTC(), Payload: payload,
	}
}

// warm opens connections up front, so concurrent writers really overlap.
//
// M4.2's concurrency test passed against unfixed code because pgxpool opened
// its second connection lazily and the handshake outlasted the overlap. A
// race test that does not race is decoration.
func warm(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	var conns []*pgxpool.Conn
	for range n {
		conn, err := pool.Acquire(context.Background())
		if err != nil {
			t.Fatalf("warm pool: %v", err)
		}
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		conn.Release()
	}
}

// TestEventsAreSequencedGaplessUnderConcurrencyIntegration: several writers,
// different events, one session. Every number 1..N exactly once.
//
// Watched failing against an unlocked `max(sequence) + 1`: writers read the
// same maximum and collide on the primary key.
func TestEventsAreSequencedGaplessUnderConcurrencyIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	fixture := seedSessionFixture(t, ownerPool, "Sequencing Workspace")
	session, err := fixture.createSession(t, postgres.NewSessionStore(ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	store := postgres.NewEventStore(appPool)
	tenant := postgres.WithTenantWorkspace(context.Background(), fixture.workspace.ID)

	const writers, each = 8, 10
	warm(t, appPool, writers)
	runner := uuid.New()

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		numbers []int64
		failed  []error
	)
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range each {
				sequence, appended, err := store.AppendEvent(tenant, eventFor(session, runner))
				mu.Lock()
				if err != nil {
					failed = append(failed, err)
				} else if appended {
					numbers = append(numbers, sequence)
				}
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(failed) > 0 {
		t.Fatalf("%d appends failed, first: %v", len(failed), failed[0])
	}
	sort.Slice(numbers, func(i, j int) bool { return numbers[i] < numbers[j] })
	if len(numbers) != writers*each {
		t.Fatalf("%d events stored, want %d", len(numbers), writers*each)
	}
	for i, n := range numbers {
		if n != int64(i+1) {
			t.Fatalf("sequence %d is %d: numbers are not gapless and unique from 1", i+1, n)
		}
	}
}

// TestARacedDuplicateIsStoredOnceAndConsumesNoNumberIntegration: the same
// event, delivered to several ingestors at once.
//
// Two properties, and a naive design gets one of them: exactly one is stored,
// and the next *different* event is numbered 2, not 2 plus the number of
// losers. Watched failing against a read-then-insert, which stored it twice.
func TestARacedDuplicateIsStoredOnceAndConsumesNoNumberIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	fixture := seedSessionFixture(t, ownerPool, "Duplicate Workspace")
	session, err := fixture.createSession(t, postgres.NewSessionStore(ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	store := postgres.NewEventStore(appPool)
	tenant := postgres.WithTenantWorkspace(context.Background(), fixture.workspace.ID)

	const racers = 8
	warm(t, appPool, racers)
	event := eventFor(session, uuid.New())

	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		mu       sync.Mutex
		appended int
		failed   []error
	)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, ok, err := store.AppendEvent(tenant, event)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failed = append(failed, err)
			} else if ok {
				appended++
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(failed) > 0 {
		t.Fatalf("%d racers failed rather than seeing a duplicate, first: %v", len(failed), failed[0])
	}
	if appended != 1 {
		t.Fatalf("%d racers stored the same event, want exactly 1", appended)
	}
	next, ok, err := store.AppendEvent(tenant, eventFor(session, event.RunnerID))
	if err != nil || !ok {
		t.Fatalf("next event: %v %v", ok, err)
	}
	if next != 2 {
		t.Errorf("the next event got sequence %d, want 2 — the %d losing duplicates consumed numbers", next, racers-1)
	}
}

// TestSessionEventsAreAppendOnlyIntegration: invariant 9, enforced by the
// database for every path, including the owner's.
func TestSessionEventsAreAppendOnlyIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	fixture := seedSessionFixture(t, ownerPool, "Append Only Workspace")
	session, err := fixture.createSession(t, postgres.NewSessionStore(ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	tenant := postgres.WithTenantWorkspace(context.Background(), fixture.workspace.ID)
	if _, _, err := postgres.NewEventStore(appPool).AppendEvent(tenant, eventFor(session, uuid.New())); err != nil {
		t.Fatalf("append: %v", err)
	}

	for name, statement := range map[string]string{
		"update":   `UPDATE session_events SET type = 'provider.failed' WHERE session_id = $1`,
		"delete":   `DELETE FROM session_events WHERE session_id = $1`,
		"truncate": `TRUNCATE session_events`,
	} {
		args := []any{session.ID}
		if name == "truncate" {
			args = nil
		}
		if _, err := ownerPool.Exec(context.Background(), statement, args...); err == nil {
			t.Errorf("%s on session_events succeeded; history must be append-only", name)
		} else if !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%s refused for the wrong reason: %v", name, err)
		}
	}
}

// TestEventsAndQuarantineAreNotReadableAcrossTenantsIntegration.
func TestEventsAndQuarantineAreNotReadableAcrossTenantsIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	a := seedSessionFixture(t, ownerPool, "Tenant A Events")
	b := seedSessionFixture(t, ownerPool, "Tenant B Events")
	session, err := a.createSession(t, postgres.NewSessionStore(ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	store := postgres.NewEventStore(appPool)
	if _, _, err := store.AppendEvent(postgres.WithTenantWorkspace(context.Background(), a.workspace.ID),
		eventFor(session, uuid.New())); err != nil {
		t.Fatalf("append: %v", err)
	}

	// B's context, asking for A's events by A's own identifiers.
	events, err := store.ListEvents(postgres.WithTenantWorkspace(context.Background(), b.workspace.ID),
		session.ID, a.workspace.ID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("workspace B read %d of workspace A's events", len(events))
	}

	// A writer claiming another tenant's workspace is refused by the policy,
	// behind the ingestor's own check.
	if _, _, err := store.AppendEvent(postgres.WithTenantWorkspace(context.Background(), b.workspace.ID),
		eventFor(session, uuid.New())); err == nil {
		t.Error("an event for A's session was written under B's tenant context")
	}

	// The quarantine is INSERT-only for the application role: no tenant can
	// read any of it, its own included.
	if _, err := appPool.Exec(context.Background(), `SELECT count(*) FROM session_event_quarantine`); err == nil {
		t.Error("weave_app could read the quarantine")
	}
}

// TestTheSessionLookupCannotEnumerateIntegration: the privileged function
// answers only for an id already held.
func TestTheSessionLookupCannotEnumerateIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	fixture := seedSessionFixture(t, ownerPool, "Lookup Workspace")
	session, err := fixture.createSession(t, postgres.NewSessionStore(ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	store := postgres.NewEventStore(appPool)

	// The positive case, as weave_app with no ambient tenant — the condition
	// under which a direct read matches nothing and reports success.
	found, err := store.ResolveSession(context.Background(), session.ID)
	if err != nil || found.WorkspaceID != fixture.workspace.ID {
		t.Fatalf("ResolveSession = %+v, %v; want the session's workspace", found, err)
	}
	var direct int
	if err := appPool.QueryRow(context.Background(),
		`SELECT count(*) FROM sessions WHERE id = $1`, session.ID).Scan(&direct); err != nil {
		t.Fatalf("direct read: %v", err)
	}
	if direct != 0 {
		t.Fatal("setup: weave_app read a session with no tenant; the lookup proves nothing")
	}

	var rows int
	if err := appPool.QueryRow(context.Background(),
		`SELECT count(*) FROM weave_session_for_event($1)`, uuid.New()).Scan(&rows); err != nil {
		t.Fatalf("unknown id: %v", err)
	}
	if rows != 0 {
		t.Errorf("an unknown id returned %d rows", rows)
	}
	if _, err := appPool.Exec(context.Background(), `SELECT * FROM weave_session_for_event(NULL)`); err == nil {
		t.Error("the lookup accepted NULL")
	}
}

// TestATerminalTransitionInFlightBlocksTheAppendIntegration is a review
// finding on PR #18: terminality read before the append transaction could be
// overtaken by a terminal transition committing in between.
//
// The transaction below does what SessionStore.Transition does into a
// terminal state — NO KEY UPDATE on the session, then the event counter, then
// the state — and holds it. The append must wait for it and then refuse.
// Watched failing with the counter lock removed from both sides: the append
// did not wait, read the old state and stored the event.
func TestATerminalTransitionInFlightBlocksTheAppendIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	fixture := seedSessionFixture(t, ownerPool, "In Flight Workspace")
	session, err := fixture.createSession(t, postgres.NewSessionStore(ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	tx, err := ownerPool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for _, statement := range []string{
		`SELECT 1 FROM sessions WHERE id = $1 FOR NO KEY UPDATE`,
		`INSERT INTO session_event_sequences (session_id, workspace_id, last_sequence)
		 SELECT id, workspace_id, 0 FROM sessions WHERE id = $1
		 ON CONFLICT (session_id) DO UPDATE SET last_sequence = session_event_sequences.last_sequence`,
		`UPDATE sessions SET state = 'failed' WHERE id = $1`,
	} {
		if _, err := tx.Exec(context.Background(), statement, session.ID); err != nil {
			t.Fatalf("transition: %v", err)
		}
	}

	type result struct {
		appended bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		_, appended, err := postgres.NewEventStore(appPool).AppendEvent(
			postgres.WithTenantWorkspace(context.Background(), fixture.workspace.ID), eventFor(session, uuid.New()))
		done <- result{appended, err}
	}()

	select {
	case r := <-done:
		t.Fatalf("the append did not wait for the transition in flight (appended=%v, err=%v)", r.appended, r.err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}

	r := <-done
	if !errors.Is(r.err, application.ErrEventSessionTerminal) {
		t.Errorf("append after the terminal commit = (%v, %v), want ErrEventSessionTerminal", r.appended, r.err)
	}
}

// TestATerminalTransitionIsNotStarvedByOutputIntegration is the second review
// round's finding on PR #18, measured before it was fixed.
//
// The first fix took FOR SHARE on the session for every append. Under eight
// writers appending as fast as they could, a transition's lock waited 2.98s —
// the whole run: new share lockers kept jumping the waiting FOR UPDATE, and
// every append's foreign-key check took FOR KEY SHARE, which FOR UPDATE also
// conflicts with. A workflow could not reliably fail a session that was busy
// producing output. Here the real terminal transition, through the service,
// must finish promptly under the same load — and not one event may land after.
func TestATerminalTransitionIsNotStarvedByOutputIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	fixture := seedSessionFixture(t, ownerPool, "Busy Session Workspace")
	sessionStore := postgres.NewSessionStore(appPool)
	session, err := fixture.createSession(t, postgres.NewSessionStore(ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	events := postgres.NewEventStore(appPool)
	tenant := postgres.WithTenantWorkspace(context.Background(), fixture.workspace.ID)
	warm(t, appPool, 10)

	ctx, cancel := context.WithCancel(context.Background())
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		refused  int
		failures []error
	)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_, _, err := events.AppendEvent(tenant, eventFor(session, uuid.New()))
				mu.Lock()
				switch {
				case errors.Is(err, application.ErrEventSessionTerminal):
					refused++
				case err != nil:
					failures = append(failures, err)
				}
				mu.Unlock()
			}
		}()
	}
	time.Sleep(time.Second) // let the output build up

	sessions := application.NewSessionService(sessionStore, postgres.NewTaskStore(appPool), postgres.NewAgentStore(appPool))
	start := time.Now()
	moved, err := sessions.TransitionAsSystem(tenant, fixture.workspace.ID, session.ID,
		domain.SessionFailed, "test: failed under load")
	waited := time.Since(start)
	if err != nil {
		cancel()
		wg.Wait()
		t.Fatalf("terminal transition under load: %v", err)
	}
	var lastBefore int64
	if err := ownerPool.QueryRow(context.Background(),
		`SELECT coalesce(max(sequence), 0) FROM session_events WHERE session_id = $1`, session.ID).Scan(&lastBefore); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // appends keep arriving after the end
	cancel()
	wg.Wait()

	if len(failures) > 0 {
		t.Fatalf("%d appends failed (deadlock?), first: %v", len(failures), failures[0])
	}
	if waited > time.Second {
		t.Errorf("the terminal transition took %v under load; it must not queue behind the output", waited)
	}
	var after int
	if err := ownerPool.QueryRow(context.Background(),
		`SELECT count(*) FROM session_events WHERE session_id = $1 AND sequence > $2`, session.ID, lastBefore).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != 0 {
		t.Errorf("%d events landed after the session became %s", after, moved.State)
	}
	if refused == 0 {
		t.Error("no append was refused after the end; the load stopped too early to prove anything")
	}
	t.Logf("terminal transition under load took %v; %d later appends refused", waited, refused)
}

// TestContentTheDatabaseRejectsIsNotTransientIntegration: bypassing the
// decoder, a payload jsonb refuses must come back as a rejection — not a
// failure the ingestor would retry to exhaustion.
func TestContentTheDatabaseRejectsIsNotTransientIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	fixture := seedSessionFixture(t, ownerPool, "Rejected Content Workspace")
	session, err := fixture.createSession(t, postgres.NewSessionStore(ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	event := eventFor(session, uuid.New())
	event.Payload = []byte(`{"message_id":"` + uuid.NewString() + `","role":"assistant","text":"a\u0000b"}`)

	_, _, err = postgres.NewEventStore(appPool).AppendEvent(
		postgres.WithTenantWorkspace(context.Background(), fixture.workspace.ID), event)
	if !errors.Is(err, application.ErrEventRejectedByStore) {
		t.Errorf("AppendEvent with a NUL escape = %v, want ErrEventRejectedByStore", err)
	}
}

// TestTheTableAgreesWithTheVersionContractIntegration: the database's CHECK is
// the third enforcer of domain.SchemaVersionPattern, and must accept and
// refuse what the other two do.
func TestTheTableAgreesWithTheVersionContractIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	fixture := seedSessionFixture(t, ownerPool, "Version Contract Workspace")
	session, err := fixture.createSession(t, postgres.NewSessionStore(ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	store := postgres.NewEventStore(appPool)
	tenant := postgres.WithTenantWorkspace(context.Background(), fixture.workspace.ID)

	for version, accepted := range map[string]bool{"1.0": true, "1.10": true, "1.01": false, "1.1234567890": false} {
		event := eventFor(session, uuid.New())
		event.SchemaVersion = version
		_, _, err := store.AppendEvent(tenant, event)
		switch {
		case accepted && err != nil:
			t.Errorf("the table refused %q: %v", version, err)
		case !accepted && !errors.Is(err, application.ErrEventRejectedByStore):
			t.Errorf("the table took %q (err %v); it must refuse what the decoder refuses", version, err)
		}
	}
}

// TestATerminalTransitionWaitsForTheAppendInFlightIntegration is the other
// direction, through the real transition code.
//
// An append holds the session's event counter — here, a transaction that has
// taken it and not yet committed. A terminal transition through
// SessionService must wait for it, so the event lands before the end rather
// than after. The load test alone did not catch the transition's counter lock
// being removed — the window is too narrow to hit by chance — so this pins it
// deterministically. Watched failing with that lock removed: the transition
// returned at once while the append was still in flight.
func TestATerminalTransitionWaitsForTheAppendInFlightIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	fixture := seedSessionFixture(t, ownerPool, "Append In Flight Workspace")
	session, err := fixture.createSession(t, postgres.NewSessionStore(ownerPool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	appending, err := ownerPool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = appending.Rollback(context.Background()) }()
	if _, err := appending.Exec(context.Background(),
		`INSERT INTO session_event_sequences (session_id, workspace_id, last_sequence)
		 SELECT id, workspace_id, 1 FROM sessions WHERE id = $1`, session.ID); err != nil {
		t.Fatalf("hold the counter: %v", err)
	}

	sessions := application.NewSessionService(postgres.NewSessionStore(appPool),
		postgres.NewTaskStore(appPool), postgres.NewAgentStore(appPool))
	done := make(chan error, 1)
	go func() {
		_, err := sessions.TransitionAsSystem(postgres.WithTenantWorkspace(context.Background(), fixture.workspace.ID),
			fixture.workspace.ID, session.ID, domain.SessionFailed, "test: failed while an append is in flight")
		done <- err
	}()

	select {
	case err := <-done:
		t.Fatalf("the terminal transition did not wait for the append in flight (err=%v)", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := appending.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Errorf("the transition failed once the append committed: %v", err)
	}
}
