package postgres_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/activity"
	temporalclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/jigmetnamgyal/weave/internal/adapters/devdocker"
	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	weavetemporal "github.com/jigmetnamgyal/weave/internal/adapters/temporal"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// newHarness brings up a real Temporal worker and publisher for one test.
//
// Real rather than mocked, deliberately: the guarantee under test is
// Temporal's workflow-id reuse policy, and a mock would only assert our belief
// about how it behaves. That belief was wrong once already — the first draft
// of the M5.1 spec assumed uniqueness among running workflows covered every
// duplicate, and it does not cover a closed one.
func newHarness(t *testing.T) (*pgxpool.Pool, *application.Publisher, func()) {
	t.Helper()
	h := startHarness(t)
	return h.pool, h.publisher, h.stop
}

// harness is a running worker and publisher, with the fake GitHub they reach.
type harness struct {
	pool      *pgxpool.Pool
	publisher *application.Publisher
	github    *fakeGitHubServer
	// commit is the real commit the test git server's repository holds, and
	// the one the fake GitHub cuts branches at — so a runner can clone it.
	commit  string
	backend *devdocker.Backend
	stop    func()
}

// startHarness is newHarness, keeping hold of the fake GitHub so a test can
// grant repositories and count what was created.
func startHarness(t *testing.T) harness {
	t.Helper()

	hostPort := os.Getenv("TEST_TEMPORAL_HOST_PORT")
	if hostPort == "" {
		t.Skip("TEST_TEMPORAL_HOST_PORT is not set; run `make test-integration`")
	}
	pool := newPool(t)

	client, err := temporalclient.Dial(temporalclient.Options{HostPort: hostPort})
	if err != nil {
		t.Fatalf("connect to temporal at %s: %v", hostPort, err)
	}

	sessionStore := postgres.NewSessionStore(pool)
	sessions := application.NewSessionService(
		sessionStore, postgres.NewTaskStore(pool), postgres.NewAgentStore(pool))
	// Every session workflow now provisions a runner (M5.4a), so the harness
	// runs a runner manager too: the dev Docker backend on a scope of its own,
	// cloning from a real git server.
	backend := dockerBackend(t)
	gitBase, commit := gitServer(t, "acme", "service")
	github := newFakeGitHubServer(t)
	github.baseCommit = commit
	installations := installationServiceFor(t, pool, github)
	branches := application.NewSessionBranchService(sessionStore, installations)
	outcomes := application.NewSessionOutcomeService(sessions, sessionStore,
		postgres.NewAgentStore(pool), postgres.NewEventStore(pool))
	activities := weavetemporal.NewSessionActivities(sessions, branches, outcomes)

	// A task queue per test, so two tests never race for each other's work.
	queue := "weave-sessions-test-" + uuid.NewString()
	w := worker.New(client, queue, worker.Options{})
	w.RegisterWorkflowWithOptions(weavetemporal.SessionWorkflow,
		workflow.RegisterOptions{Name: weavetemporal.SessionWorkflowName})
	w.RegisterActivityWithOptions(activities.MarkProvisioning,
		activity.RegisterOptions{Name: weavetemporal.ActivityMarkProvisioning})
	w.RegisterActivityWithOptions(activities.FailUnprovisionable,
		activity.RegisterOptions{Name: weavetemporal.ActivityFailUnprovisionable})
	w.RegisterActivityWithOptions(activities.CreateBranch,
		activity.RegisterOptions{Name: weavetemporal.ActivityCreateBranch})
	w.RegisterActivityWithOptions(activities.RecordBranch,
		activity.RegisterOptions{Name: weavetemporal.ActivityRecordBranch})
	w.RegisterActivityWithOptions(activities.FailSession,
		activity.RegisterOptions{Name: weavetemporal.ActivityFailSession})
	w.RegisterActivityWithOptions(activities.MarkRunning,
		activity.RegisterOptions{Name: weavetemporal.ActivityMarkRunning})
	w.RegisterActivityWithOptions(activities.CompleteSession,
		activity.RegisterOptions{Name: weavetemporal.ActivityCompleteSession})
	if err := w.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}

	broker := runnerBroker(t)
	// A real ingestor over the runner's stream, so a session's events are
	// stored and the drain has a count to watch fall. Slowed, so ingestion
	// lags the runner's exit and a workflow that did not wait for the drain
	// would lose events — which is what the drain exists to prevent.
	broker.ingest(t, slowEventStore{EventStore: postgres.NewEventStore(pool), delay: 250 * time.Millisecond})
	runners := application.NewRunnerService(postgres.NewRunnerStore(pool), sessionStore, backend, installations,
		broker.issuer, broker.natsURL, postgres.NewAgentStore(pool), broker.drain(),
		postgres.WithTenantWorkspace, gitBase, slog.New(slog.NewTextHandler(io.Discard, nil))).
		WithEgress(postgres.NewEgressStore(pool), "", nil)
	runnerActivities := weavetemporal.NewRunnerActivities(runners)
	rw := worker.New(client, weavetemporal.RunnerTaskQueue(queue), worker.Options{})
	rw.RegisterActivityWithOptions(runnerActivities.ProvisionRunner,
		activity.RegisterOptions{Name: weavetemporal.ActivityProvisionRunner})
	rw.RegisterActivityWithOptions(runnerActivities.TeardownRunner,
		activity.RegisterOptions{Name: weavetemporal.ActivityTeardownRunner})
	rw.RegisterActivityWithOptions(runnerActivities.AwaitRunnerExit,
		activity.RegisterOptions{Name: weavetemporal.ActivityAwaitRunnerExit})
	rw.RegisterActivityWithOptions(runnerActivities.HaltRunner,
		activity.RegisterOptions{Name: weavetemporal.ActivityHaltRunner})
	rw.RegisterActivityWithOptions(runnerActivities.DrainRunnerEvents,
		activity.RegisterOptions{Name: weavetemporal.ActivityDrainRunnerEvents})
	if err := rw.Start(); err != nil {
		t.Fatalf("start runner worker: %v", err)
	}

	publisher := application.NewPublisher(
		postgres.NewOutboxStore(pool),
		weavetemporal.NewStarterOn(client, queue),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		publisher.Run(ctx)
	}()

	return harness{pool: pool, publisher: publisher, github: github, commit: commit, backend: backend, stop: func() {
		cancel()
		<-done
		w.Stop()
		rw.Stop()
		client.Close()
	}}
}

// slowEventStore delays every append, so ingestion lags the runner.
type slowEventStore struct {
	application.EventStore
	delay time.Duration
}

func (s slowEventStore) AppendEvent(ctx context.Context, event domain.SessionEvent) (int64, bool, error) {
	time.Sleep(s.delay)
	return s.EventStore.AppendEvent(ctx, event)
}

// waitForState polls until a session reaches the state, or gives up.
func waitForState(t *testing.T, pool *pgxpool.Pool, tenant context.Context, sessionID, workspaceID uuid.UUID, want string) int32 {
	t.Helper()
	store := postgres.NewSessionStore(pool)
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		current, err := store.Get(tenant, sessionID, workspaceID)
		if err != nil {
			t.Fatalf("get session: %v", err)
		}
		if string(current.State) == want {
			return current.Version
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the session never reached %s", want)
	return 0
}

// TestARepublishStartsNoSecondWorkflowIntegration is the duplicate-delivery
// guarantee, and specifically the case running-id uniqueness does not cover.
//
// The publisher is at-least-once: a crash between starting the workflow and
// completing the outbox row leaves the row to be delivered again. Here the
// first execution has already **closed** before the second delivery — which
// the default reuse policy would permit, starting a second workflow for a
// session that already ran. Rejecting duplicates is what closes it, and this
// is the test that would have caught the first draft's assumption.
func TestARepublishStartsNoSecondWorkflowIntegration(t *testing.T) {
	pool, _, stop := newHarness(t)
	defer stop()

	sessions := postgres.NewSessionStore(pool)
	fixture := seedSessionFixture(t, pool, "Republish Workspace")
	session, err := fixture.createSession(t, sessions, nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	// The workflow runs to completion on its own.
	version := waitForState(t, pool, fixture.ctx, session.ID, fixture.workspace.ID, "failed")

	// Put the row back exactly as a publisher that crashed after starting the
	// workflow would have left it.
	if _, err := pool.Exec(context.Background(),
		`UPDATE outbox_events
		 SET completed_at = NULL, leased_until = NULL, claimant = NULL, available_at = now()
		 WHERE subject_id = $1`, session.ID); err != nil {
		t.Fatalf("republish: %v", err)
	}

	waitForSettled(t, pool, session.ID)

	after, err := sessions.Get(fixture.ctx, session.ID, fixture.workspace.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if after.Version != version {
		t.Errorf("version moved from %d to %d after a republish — the session ran twice",
			version, after.Version)
	}

	var transitions int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM session_state_transitions WHERE session_id = $1",
		session.ID).Scan(&transitions); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	if transitions != 3 {
		t.Errorf("%d transitions after a republish, want 3 — the history recorded a second run",
			transitions)
	}
}

// TestTheWorkflowMovesASessionWithoutBorrowingAnIdentityIntegration is the
// system actor, observed rather than asserted in a unit test.
//
// `authorizeActor` refuses a nil user, so the shortcut would have been to
// reuse the member who created the session. Attributing an automated move to a
// person is a false statement in a trail that cannot be corrected by editing.
func TestTheWorkflowMovesASessionWithoutBorrowingAnIdentityIntegration(t *testing.T) {
	pool, _, stop := newHarness(t)
	defer stop()

	fixture := seedSessionFixture(t, pool, "System Actor Workspace")
	session, err := fixture.createSession(t, postgres.NewSessionStore(pool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	waitForState(t, pool, fixture.ctx, session.ID, fixture.workspace.ID, "failed")

	rows, err := pool.Query(context.Background(),
		`SELECT next_state, actor_user_id IS NULL
		 FROM session_state_transitions WHERE session_id = $1 ORDER BY created_at`, session.ID)
	if err != nil {
		t.Fatalf("read transitions: %v", err)
	}
	defer rows.Close()

	var seen []string
	systemMoves := 0
	for rows.Next() {
		var state string
		var bySystem bool
		if err := rows.Scan(&state, &bySystem); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen = append(seen, state)
		if bySystem {
			systemMoves++
		}
	}
	if len(seen) != 3 {
		t.Fatalf("transitions = %v, want three: queued, provisioning, failed", seen)
	}
	// The first is the person who created it; the two the workflow made are
	// attributed to nobody.
	if systemMoves != 2 {
		t.Errorf("%d transitions carry no actor, want 2 — the workflow borrowed a person's name",
			systemMoves)
	}
}

// waitForSettled polls until the outbox row is completed or terminated.
func waitForSettled(t *testing.T, pool *pgxpool.Pool, subjectID uuid.UUID) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var settled bool
		if err := pool.QueryRow(context.Background(),
			`SELECT completed_at IS NOT NULL OR terminated_at IS NOT NULL
			 FROM outbox_events WHERE subject_id = $1`, subjectID).Scan(&settled); err != nil {
			t.Fatalf("read the row: %v", err)
		}
		if settled {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the outbox row was never settled — a duplicate reported as an error retries forever")
}

// sessionEvents returns a session's stored events in sequence order.
func sessionEvents(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID) []domain.SessionEvent {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT sequence, runner_id, type, payload FROM session_events WHERE session_id = $1 ORDER BY sequence`, sessionID)
	if err != nil {
		t.Fatalf("read events: %v", err)
	}
	defer rows.Close()
	var out []domain.SessionEvent
	for rows.Next() {
		var e domain.SessionEvent
		var typ string
		if err := rows.Scan(&e.Sequence, &e.RunnerID, &typ, &e.Payload); err != nil {
			t.Fatal(err)
		}
		e.Type = domain.EventType(typ)
		out = append(out, e)
	}
	return out
}

// routeOf returns a session's states in order, and its last reason.
func routeOf(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID) (string, string) {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT next_state, reason FROM session_state_transitions WHERE session_id = $1 ORDER BY created_at, id`, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var route []string
	var last string
	for rows.Next() {
		var state, reason string
		_ = rows.Scan(&state, &reason)
		route, last = append(route, state), reason
	}
	return strings.Join(route, " → "), last
}

// failingFixture is the fixture with its agent version replaced by one on the
// fake's failing model — chosen by the model, as a real session's would be.
func failingFixture(t *testing.T, pool *pgxpool.Pool, fixture sessionFixture) sessionFixture {
	t.Helper()
	agents := postgres.NewAgentStore(pool)
	agentID, _ := domain.NewAgentID()
	versionID, _ := domain.NewAgentVersionID()
	_, version, err := agents.Create(fixture.ctx,
		domain.Agent{ID: agentID, WorkspaceID: fixture.workspace.ID, Name: "failer", CreatedBy: fixture.owner.ID},
		domain.AgentVersion{ID: versionID, AgentID: agentID, WorkspaceID: fixture.workspace.ID, Version: 1,
			Provider: domain.ProviderFake, Model: "deterministic-fail-v1", ToolPolicy: []byte(`{}`), CreatedBy: fixture.owner.ID},
		application.Actor{UserID: fixture.owner.ID, Required: domain.PermissionWorkspaceManage},
		application.AuditEvent{WorkspaceID: fixture.workspace.ID, ActorUserID: fixture.owner.ID,
			Action: application.AuditAgentCreated, Target: agentID.String()})
	if err != nil {
		t.Fatalf("create failing agent: %v", err)
	}
	fixture.version = version
	return fixture
}

// TestASessionRunsItsProviderAndIsReadyForReviewIntegration is M5.5b's check,
// end to end: a session created the way the API creates one is provisioned a
// runner, runs the fake provider, and ends in review_ready on its own — its
// history holding the fake's messages in order, from its bound runner, with
// nothing quarantined, and the runner gone.
//
// Ingestion is deliberately slowed (see slowEventStore), so the runner exits
// long before its events are stored; they are all present because the
// workflow halts and drains before it ends anything.
func TestASessionRunsItsProviderAndIsReadyForReviewIntegration(t *testing.T) {
	h := startHarness(t)
	defer h.stop()

	fixture := seedSessionFixture(t, h.pool, "Provider Run Workspace")
	h.github.grant(t, h.pool, fixture)
	session, err := fixture.createSession(t, postgres.NewSessionStore(h.pool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	waitForState(t, h.pool, fixture.ctx, session.ID, fixture.workspace.ID, "review_ready")

	route, reason := routeOf(t, h.pool, session.ID)
	if route != "queued → provisioning → running → review_ready" {
		t.Errorf("route = %s", route)
	}
	if !strings.Contains(reason, "fake provider finished") {
		t.Errorf("final reason = %q; a fake-run session must say so", reason)
	}

	var runnerID uuid.UUID
	var state string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT id, state FROM runners WHERE session_id = $1`, session.ID).Scan(&runnerID, &state); err != nil {
		t.Fatalf("read runner: %v", err)
	}
	if state != "terminated" {
		t.Errorf("runner = %s, want terminated", state)
	}

	events := sessionEvents(t, h.pool, session.ID)
	if len(events) != 5 {
		t.Fatalf("%d events stored, want the fake's 5", len(events))
	}
	for i, e := range events {
		if e.Sequence != int64(i+1) || e.RunnerID != runnerID || e.Type != domain.EventMessageCreated {
			t.Errorf("event %d = seq %d from %s (%s); want sequence %d from the session's runner", i, e.Sequence, e.RunnerID, e.Type, i+1)
		}
	}
	var quarantined int
	_ = h.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM session_event_quarantine WHERE session_id = $1`, session.ID).Scan(&quarantined)
	if quarantined != 0 {
		t.Errorf("%d of the session's events were quarantined; the drain must have them all stored", quarantined)
	}
	if environments, _ := h.backend.List(context.Background()); len(environments) != 0 {
		t.Errorf("%d runner environments left behind", len(environments))
	}
}

// TestAFailingProviderFailsTheSessionNamingItsCodeIntegration.
func TestAFailingProviderFailsTheSessionNamingItsCodeIntegration(t *testing.T) {
	h := startHarness(t)
	defer h.stop()

	fixture := failingFixture(t, h.pool, seedSessionFixture(t, h.pool, "Failing Provider Workspace"))
	h.github.grant(t, h.pool, fixture)
	session, err := fixture.createSession(t, postgres.NewSessionStore(h.pool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	waitForState(t, h.pool, fixture.ctx, session.ID, fixture.workspace.ID, "failed")

	route, reason := routeOf(t, h.pool, session.ID)
	if route != "queued → provisioning → running → failed" {
		t.Errorf("route = %s", route)
	}
	if !strings.Contains(reason, "fake provider reported failure: fake_failure") {
		t.Errorf("final reason = %q, want the provider's code named", reason)
	}
	events := sessionEvents(t, h.pool, session.ID)
	if len(events) == 0 || events[len(events)-1].Type != domain.EventProviderFailed {
		t.Errorf("events = %v, want the history to end in provider.failed", events)
	}
}

// TestTheFakeProducesTheSameHistoryTwiceIntegration: determinism, observed
// through the whole chain. Message ids are derived from the session, so they
// are compared with the session factored out; the text must match exactly.
func TestTheFakeProducesTheSameHistoryTwiceIntegration(t *testing.T) {
	h := startHarness(t)
	defer h.stop()

	fixture := seedSessionFixture(t, h.pool, "Determinism Workspace")
	h.github.grant(t, h.pool, fixture)
	store := postgres.NewSessionStore(h.pool)
	var histories [2][]string
	for i := range histories {
		session, err := fixture.createSession(t, store, nil)
		if err != nil {
			t.Fatal(err)
		}
		waitForState(t, h.pool, fixture.ctx, session.ID, fixture.workspace.ID, "review_ready")
		for _, e := range sessionEvents(t, h.pool, session.ID) {
			var msg domain.MessageCreated
			_ = json.Unmarshal(e.Payload, &msg)
			histories[i] = append(histories[i], string(e.Type)+"|"+msg.Role+"|"+msg.Text)
		}
	}
	if strings.Join(histories[0], "\n") != strings.Join(histories[1], "\n") {
		t.Errorf("two sessions on the same model produced different histories:\n%v\n%v", histories[0], histories[1])
	}
}
