package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/temporal"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	weavetemporal "github.com/jigmetnamgyal/weave/internal/adapters/temporal"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// TestTheWorkflowCutsTheSessionBranchIntegration is the unit's check, end to
// end: a session created the way the API creates one reaches `failed` on its
// own, and leaves a real branch behind with its commit recorded.
func TestTheWorkflowCutsTheSessionBranchIntegration(t *testing.T) {
	h := startHarness(t)
	defer h.stop()

	fixture := seedSessionFixture(t, h.pool, "Branch Workflow Workspace")
	h.github.grant(t, h.pool, fixture)

	session, err := fixture.createSession(t, postgres.NewSessionStore(h.pool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	waitForState(t, h.pool, fixture.ctx, session.ID, fixture.workspace.ID, "failed")

	if got := h.github.created(session.BranchName); got != 1 {
		t.Errorf("GitHub created %s %d times, want 1", session.BranchName, got)
	}

	after, err := postgres.NewSessionStore(h.pool).Get(fixture.ctx, session.ID, fixture.workspace.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if after.BranchSHA != baseSHA {
		t.Errorf("recorded branch SHA = %q, want the base commit %q", after.BranchSHA, baseSHA)
	}

	// It still fails, because there is no runner — and says so, rather than
	// blaming the branch that was in fact made.
	reason := lastReason(t, h.pool, session.ID)
	if !strings.Contains(reason, "no runner") {
		t.Errorf("final reason = %q, want the missing runner named", reason)
	}

	// The creation is audited, and attributed to nobody.
	var audits, withActor int
	if err := h.pool.QueryRow(context.Background(),
		`SELECT count(*), count(actor_user_id) FROM audit_events
		 WHERE target = $1 AND action = $2`,
		session.ID.String(), application.AuditBranchCreated).Scan(&audits, &withActor); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if audits != 1 {
		t.Errorf("%d branch-created audit rows, want 1", audits)
	}
	if withActor != 0 {
		t.Error("the branch audit names a person; the system cut this branch")
	}
}

// TestAWithdrawnGrantFailsTheSessionWithItsReasonIntegration: GitHub no longer
// grants the repository, and our table still says it does. The workflow must
// believe GitHub, fail the session with a reason naming the grant, and create
// nothing.
func TestAWithdrawnGrantFailsTheSessionWithItsReasonIntegration(t *testing.T) {
	h := startHarness(t)
	defer h.stop()

	fixture := seedSessionFixture(t, h.pool, "Withdrawn Grant Workspace")
	// Not granted on the fake: GitHub lists nothing for this installation,
	// while the repositories row still reads granted.

	session, err := fixture.createSession(t, postgres.NewSessionStore(h.pool), nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	waitForState(t, h.pool, fixture.ctx, session.ID, fixture.workspace.ID, "failed")

	if got := h.github.created(session.BranchName); got != 0 {
		t.Errorf("GitHub created the branch %d times for a repository no longer granted", got)
	}
	reason := lastReason(t, h.pool, session.ID)
	if !strings.Contains(reason, "no longer granted") {
		t.Errorf("final reason = %q, want the withdrawn grant named", reason)
	}

	after, err := postgres.NewSessionStore(h.pool).Get(fixture.ctx, session.ID, fixture.workspace.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if after.BranchSHA != "" {
		t.Errorf("a refused branch recorded SHA %q", after.BranchSHA)
	}
}

// branchActivities builds the activities on the application role, with the
// fake GitHub behind the real client — and no ambient tenant anywhere.
func branchActivities(t *testing.T, appPool *pgxpool.Pool, github *fakeGitHubServer) *weavetemporal.SessionActivities {
	t.Helper()
	store := postgres.NewSessionStore(appPool)
	sessions := application.NewSessionService(store, postgres.NewTaskStore(appPool), postgres.NewAgentStore(appPool))
	return weavetemporal.NewSessionActivities(sessions,
		application.NewSessionBranchService(store, installationServiceFor(t, appPool, github)))
}

// provisioningSession creates a session and moves it to provisioning, as the
// workflow's first activity would.
func provisioningSession(t *testing.T, pool *pgxpool.Pool, fixture sessionFixture) domain.Session {
	t.Helper()
	store := postgres.NewSessionStore(pool)
	session, err := fixture.createSession(t, store, nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sessions := application.NewSessionService(store, postgres.NewTaskStore(pool), postgres.NewAgentStore(pool))
	if _, err := sessions.TransitionAsSystem(fixture.ctx, fixture.workspace.ID, session.ID,
		domain.SessionProvisioning, "test: provisioning"); err != nil {
		t.Fatalf("move to provisioning: %v", err)
	}
	return session
}

// TestTheBranchActivityIsSafeToRedeliverIntegration runs the activity twice,
// as Temporal does when an acknowledgement is lost, on the application role
// with **no ambient tenant context** — the condition under which M5.0's sweep
// read nothing and reported success.
//
// So the assertions are positive: the branch **was** made, and the SHA **is**
// recorded. A tenantless read that matched no row would fail here rather than
// pass quietly.
func TestTheBranchActivityIsSafeToRedeliverIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	github := newFakeGitHubServer(t)

	fixture := seedSessionFixture(t, ownerPool, "Redelivery Workspace")
	github.grant(t, ownerPool, fixture)
	session := provisioningSession(t, ownerPool, fixture)

	activities := branchActivities(t, appPool, github)
	input := weavetemporal.SessionWorkflowInput{
		WorkspaceID: fixture.workspace.ID.String(),
		SessionID:   session.ID.String(),
	}

	// Both activities, each delivered twice, the way a lost acknowledgement
	// makes Temporal run them.
	for attempt := 1; attempt <= 2; attempt++ {
		branch, err := activities.CreateBranch(context.Background(), input)
		if err != nil {
			t.Fatalf("cut, delivery %d: %v", attempt, err)
		}
		if err := activities.RecordBranch(context.Background(), weavetemporal.RecordBranchInput{
			WorkspaceID: input.WorkspaceID, SessionID: input.SessionID, Branch: branch,
		}); err != nil {
			t.Fatalf("record, delivery %d: %v", attempt, err)
		}
	}

	if got := github.created(session.BranchName); got != 1 {
		t.Errorf("two deliveries created the branch %d times, want 1", got)
	}

	var sha *string
	var audits int
	if err := ownerPool.QueryRow(context.Background(),
		`SELECT branch_sha FROM sessions WHERE id = $1`, session.ID).Scan(&sha); err != nil {
		t.Fatalf("read session: %v", err)
	}
	if sha == nil || *sha != baseSHA {
		t.Errorf("branch_sha = %v, want %q — found and recorded, not merely returned", sha, baseSHA)
	}
	if err := ownerPool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE target = $1 AND action IN ($2, $3)`,
		session.ID.String(), application.AuditBranchCreated, application.AuditSessionBranchFound,
	).Scan(&audits); err != nil {
		t.Fatalf("read audits: %v", err)
	}
	if audits != 1 {
		t.Errorf("two deliveries wrote %d branch audit rows, want 1", audits)
	}

	// And one transition fewer than it might have been: recording the branch
	// is not a state change, so the history is queued → provisioning only.
	var transitions int
	if err := ownerPool.QueryRow(context.Background(),
		`SELECT count(*) FROM session_state_transitions WHERE session_id = $1`,
		session.ID).Scan(&transitions); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	if transitions != 2 {
		t.Errorf("%d transitions, want 2 — cutting a branch must not invent one", transitions)
	}
}

// TestTheBranchActivityRefusesAMismatchedWorkspaceIntegration is the ownership
// rule from M5.1, applied to a path that writes to GitHub.
//
// The input pairs one workspace with another workspace's session. It must be
// not found, non-retryable, and must reach GitHub not at all.
func TestTheBranchActivityRefusesAMismatchedWorkspaceIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	github := newFakeGitHubServer(t)

	victim := seedSessionFixture(t, ownerPool, "Victim Workspace")
	github.grant(t, ownerPool, victim)
	session := provisioningSession(t, ownerPool, victim)
	attacker := seedSessionFixture(t, ownerPool, "Other Workspace")

	activities := branchActivities(t, appPool, github)
	_, err := activities.CreateBranch(context.Background(), weavetemporal.SessionWorkflowInput{
		WorkspaceID: attacker.workspace.ID.String(),
		SessionID:   session.ID.String(),
	})

	var applicationErr *temporal.ApplicationError
	if !errors.As(err, &applicationErr) || applicationErr.Type() != weavetemporal.ErrorTypeNotFound {
		t.Fatalf("CreateBranch = %v, want a non-retryable %s", err, weavetemporal.ErrorTypeNotFound)
	}
	if !applicationErr.NonRetryable() {
		t.Error("a mismatched pair was marked retryable; waiting will not make it match")
	}
	if calls := github.callCount(); calls != 0 {
		t.Errorf("made %d GitHub calls for a session the workspace does not own", calls)
	}
}

// TestTheRecordedBranchIsWriteOnceIntegration covers the column's two
// guardians: the store's lock-and-compare, and the trigger behind it for any
// path that forgets.
func TestTheRecordedBranchIsWriteOnceIntegration(t *testing.T) {
	pool := newPool(t)
	fixture := seedSessionFixture(t, pool, "Write Once Workspace")
	store := postgres.NewSessionStore(pool)
	session, err := fixture.createSession(t, store, nil)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	audit := func(domain.Session) application.AuditEvent {
		return application.AuditEvent{
			WorkspaceID: fixture.workspace.ID, Action: application.AuditBranchCreated,
			Target: session.ID.String(),
		}
	}
	record := func(sha string) (bool, error) {
		_, recorded, err := store.RecordBranch(fixture.ctx, session.ID, fixture.workspace.ID,
			sha, application.SystemActor(), audit)
		return recorded, err
	}

	if recorded, err := record(baseSHA); err != nil || !recorded {
		t.Fatalf("first record = (%v, %v), want (true, nil)", recorded, err)
	}
	if recorded, err := record(baseSHA); err != nil || recorded {
		t.Errorf("same SHA again = (%v, %v), want (false, nil) — a redelivery finding its own work", recorded, err)
	}
	other := strings.Repeat("a", 40)
	if _, err := record(other); !errors.Is(err, domain.ErrBranchConflict) {
		t.Errorf("a different SHA = %v, want ErrBranchConflict", err)
	}

	var audits int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE target = $1 AND action = $2`,
		session.ID.String(), application.AuditBranchCreated).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if audits != 1 {
		t.Errorf("%d audit rows across three attempts, want 1", audits)
	}

	// The trigger: a direct UPDATE, which skips the store entirely, is refused.
	if _, err := pool.Exec(context.Background(),
		`UPDATE sessions SET branch_sha = $2 WHERE id = $1`, session.ID, other); err == nil {
		t.Error("a direct UPDATE rewrote a recorded branch SHA")
	}

	// And a value that is not a commit id never lands at all.
	fresh, err := fixture.createSession(t, store, nil)
	if err != nil {
		t.Fatalf("create second session: %v", err)
	}
	if _, _, err := store.RecordBranch(fixture.ctx, fresh.ID, fixture.workspace.ID,
		"not-a-sha", application.SystemActor(), audit); !errors.Is(err, domain.ErrInvalidSession) {
		t.Errorf("recording a malformed SHA = %v, want ErrInvalidSession", err)
	}
}

// lastReason reads the reason on a session's most recent transition.
func lastReason(t *testing.T, pool *pgxpool.Pool, sessionID uuid.UUID) string {
	t.Helper()
	var reason string
	if err := pool.QueryRow(context.Background(),
		`SELECT reason FROM session_state_transitions WHERE session_id = $1
		 ORDER BY created_at DESC, id DESC LIMIT 1`, sessionID).Scan(&reason); err != nil {
		t.Fatalf("read last reason: %v", err)
	}
	return reason
}
