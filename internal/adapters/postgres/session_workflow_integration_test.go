package postgres_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/activity"
	temporalclient "go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

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
	stop      func()
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
	github := newFakeGitHubServer(t)
	branches := application.NewSessionBranchService(sessionStore, installationServiceFor(t, pool, github))
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
	if err := w.Start(); err != nil {
		t.Fatalf("start worker: %v", err)
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

	return harness{pool: pool, publisher: publisher, github: github, stop: func() {
		cancel()
		<-done
		w.Stop()
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
