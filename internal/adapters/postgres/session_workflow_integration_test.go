package postgres_test

import (
	"context"
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
	activities := weavetemporal.NewSessionActivities(sessions, branches)

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
	if err := w.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
	}

	runners := application.NewRunnerService(postgres.NewRunnerStore(pool), sessionStore, backend, installations,
		postgres.WithTenantWorkspace, gitBase, slog.New(slog.NewTextHandler(io.Discard, nil)))
	runnerActivities := weavetemporal.NewRunnerActivities(runners)
	rw := worker.New(client, weavetemporal.RunnerTaskQueue(queue), worker.Options{})
	rw.RegisterActivityWithOptions(runnerActivities.ProvisionRunner,
		activity.RegisterOptions{Name: weavetemporal.ActivityProvisionRunner})
	rw.RegisterActivityWithOptions(runnerActivities.TeardownRunner,
		activity.RegisterOptions{Name: weavetemporal.ActivityTeardownRunner})
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

// TestASessionReachesRunningWithAVerifiedCheckoutIntegration is M5.4a's check,
// end to end: a session created the way the API creates one is provisioned a
// real runner, reaches `running` for the first time any session has, and —
// with no provider yet — is torn down and ends `failed` saying so. Nothing is
// left behind: no container, no volume.
func TestASessionReachesRunningWithAVerifiedCheckoutIntegration(t *testing.T) {
	h := startHarness(t)
	defer h.stop()

	fixture := seedSessionFixture(t, h.pool, "Running Session Workspace")
	h.github.grant(t, h.pool, fixture)
	session, err := fixture.createSession(t, postgres.NewSessionStore(h.pool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	waitForState(t, h.pool, fixture.ctx, session.ID, fixture.workspace.ID, "failed")

	rows, err := h.pool.Query(context.Background(),
		`SELECT next_state, reason FROM session_state_transitions WHERE session_id = $1 ORDER BY created_at, id`, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	var route []string
	var lastReason string
	for rows.Next() {
		var state, reason string
		_ = rows.Scan(&state, &reason)
		route = append(route, state)
		lastReason = reason
	}
	rows.Close()
	if got := strings.Join(route, " → "); got != "queued → provisioning → running → failed" {
		t.Errorf("route = %s, want queued → provisioning → running → failed", got)
	}
	if !strings.Contains(lastReason, "no provider adapter") {
		t.Errorf("final reason = %q, want the missing provider named", lastReason)
	}

	var state string
	var ready bool
	if err := h.pool.QueryRow(context.Background(),
		`SELECT state, ready_at IS NOT NULL FROM runners WHERE session_id = $1`, session.ID).Scan(&state, &ready); err != nil {
		t.Fatalf("read runner: %v", err)
	}
	if state != "terminated" || !ready {
		t.Errorf("runner = %s (ready %v), want terminated after having been ready", state, ready)
	}
	if environments, _ := h.backend.List(context.Background()); len(environments) != 0 {
		t.Errorf("%d runner environments left behind; ADR-013 retains no source", len(environments))
	}
}
