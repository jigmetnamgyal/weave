package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// sessionFixture is everything a session needs to exist: a workspace, a
// granted repository, a ready task, and an agent version to pin.
type sessionFixture struct {
	owner      domain.User
	workspace  domain.Workspace
	repository domain.Repository
	task       domain.Task
	version    domain.AgentVersion
	ctx        context.Context
	actor      application.Actor
}

func seedSessionFixture(t *testing.T, pool *pgxpool.Pool, name string) sessionFixture {
	t.Helper()

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, name)
	ctx := postgres.WithTenant(context.Background(), postgres.TenantContext{
		UserID: owner.ID, WorkspaceID: workspace.ID,
	})

	installations := postgres.NewInstallationStore(pool)
	installation := connectInstallation(t, installations, workspace, owner, nextGitHubID())
	if err := installations.Reconcile(ctx, installation.ID, workspace.ID,
		domain.SelectionSelected, []domain.Repository{
			{GitHubID: nextGitHubID(), Owner: "acme", Name: "service", DefaultBranch: "main"},
		}, domain.RequiredPermissions); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	repositories, err := installations.ListRepositories(ctx, workspace.ID)
	if err != nil {
		t.Fatalf("ListRepositories: %v", err)
	}
	if len(repositories) != 1 {
		t.Fatalf("got %d repositories, want 1", len(repositories))
	}

	sessionActor := application.Actor{UserID: owner.ID, Required: domain.PermissionSessionCreate}

	tasks := postgres.NewTaskStore(pool)
	taskID, _ := domain.NewTaskID()
	task, err := tasks.Create(ctx, domain.Task{
		ID: taskID, WorkspaceID: workspace.ID, RepositoryID: &repositories[0].ID,
		Title: "add rate limiting", Body: "the description someone wrote",
		Status: domain.TaskReady, CreatedBy: owner.ID,
	}, sessionActor, application.AuditEvent{
		WorkspaceID: workspace.ID, ActorUserID: owner.ID,
		Action: application.AuditTaskCreated, Target: taskID.String(),
	})
	if err != nil {
		t.Fatalf("Create task: %v", err)
	}

	agents := postgres.NewAgentStore(pool)
	agentID, _ := domain.NewAgentID()
	versionID, _ := domain.NewAgentVersionID()
	_, version, err := agents.Create(ctx,
		domain.Agent{ID: agentID, WorkspaceID: workspace.ID, Name: "reviewer", CreatedBy: owner.ID},
		domain.AgentVersion{
			ID: versionID, AgentID: agentID, WorkspaceID: workspace.ID, Version: 1,
			Provider: domain.ProviderFake, Model: "deterministic-v1",
			ToolPolicy: []byte(`{}`), CreatedBy: owner.ID,
		}, application.Actor{UserID: owner.ID, Required: domain.PermissionWorkspaceManage},
		application.AuditEvent{
			WorkspaceID: workspace.ID, ActorUserID: owner.ID,
			Action: application.AuditAgentCreated, Target: agentID.String(),
		})
	if err != nil {
		t.Fatalf("Create agent: %v", err)
	}

	return sessionFixture{
		owner: owner, workspace: workspace, repository: repositories[0],
		task: task, version: version, ctx: ctx, actor: sessionActor,
	}
}

// createSession builds the six-row write the service would, so a test can
// vary one part of it — which is how the atomicity failure is injected.
func (f sessionFixture) createSession(
	t *testing.T,
	store *postgres.SessionStore,
	mutate func(*application.AuditEvent, *application.OutboxEvent),
) (domain.Session, error) {
	t.Helper()

	sessionID, err := domain.NewSessionID()
	if err != nil {
		t.Fatalf("NewSessionID: %v", err)
	}
	branchName, err := domain.SessionBranchName(sessionID, f.task.Title)
	if err != nil {
		t.Fatalf("SessionBranchName: %v", err)
	}
	transitionID, _ := domain.NewTransitionID()
	eventID, _ := domain.NewOutboxEventID()

	audit := application.AuditEvent{
		WorkspaceID: f.workspace.ID, ActorUserID: f.owner.ID,
		Action: application.AuditSessionCreated, Target: sessionID.String(),
	}
	event := application.OutboxEvent{
		ID: eventID, WorkspaceID: f.workspace.ID,
		Topic: application.TopicSessionCreated, SubjectID: sessionID,
		Payload: []byte(`{"session_id":"` + sessionID.String() + `"}`),
	}
	if mutate != nil {
		mutate(&audit, &event)
	}

	return store.Create(f.ctx,
		domain.Session{
			ID: sessionID, WorkspaceID: f.workspace.ID, TaskID: f.task.ID,
			AgentVersionID: f.version.ID, State: domain.SessionQueued,
			RepositoryID: f.repository.ID, BranchName: branchName,
			CreatedBy: f.owner.ID,
		},
		domain.SessionParticipant{
			SessionID: sessionID, WorkspaceID: f.workspace.ID,
			UserID: f.owner.ID, Capacity: domain.ParticipantOwner,
		},
		domain.SessionStateTransition{
			ID: transitionID, SessionID: sessionID, WorkspaceID: f.workspace.ID,
			NextState: domain.SessionQueued, ObservedVersion: 1,
			Reason: "session created", ActorUserID: &f.owner.ID,
		},
		event, verifyTaskIsReady, f.actor, audit)
}

// verifyTaskIsReady is the check the service passes into Create, kept here so
// every test exercises the same rule the production path does.
func verifyTaskIsReady(locked domain.Task) error {
	if locked.Status != domain.TaskReady {
		return domain.ErrTaskNotRunnable
	}
	return nil
}

// TestCreatingASessionWritesEverythingTogetherIntegration is the success half
// of the atomicity claim.
func TestCreatingASessionWritesEverythingTogetherIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewSessionStore(pool)
	fixture := seedSessionFixture(t, pool, "Session Creation Workspace")

	session, err := fixture.createSession(t, store, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if session.State != domain.SessionQueued {
		t.Errorf("state = %q, want queued", session.State)
	}
	if session.Version != 1 {
		t.Errorf("version = %d, want 1", session.Version)
	}

	// The branch does not exist yet — nothing in this unit talks to GitHub —
	// and the name must read back identically, because M5's workflow reads it
	// to decide what to create and a retry has to reach the same answer.
	derived, err := domain.SessionBranchName(session.ID, fixture.task.Title)
	if err != nil {
		t.Fatalf("SessionBranchName: %v", err)
	}
	if session.BranchName != derived {
		t.Errorf("branch_name = %q, want %q derived from the session id",
			session.BranchName, derived)
	}
	for i := 0; i < 3; i++ {
		read, err := store.Get(fixture.ctx, session.ID, fixture.workspace.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if read.BranchName != session.BranchName {
			t.Fatalf("branch_name changed between reads: %q then %q",
				session.BranchName, read.BranchName)
		}
		if read.State != domain.SessionQueued {
			t.Errorf("state = %q on read, want queued", read.State)
		}
	}

	participants, err := store.ListParticipants(fixture.ctx, session.ID, fixture.workspace.ID)
	if err != nil {
		t.Fatalf("ListParticipants: %v", err)
	}
	if len(participants) != 1 || participants[0].Capacity != domain.ParticipantOwner {
		t.Fatalf("participants = %+v, want one owner", participants)
	}

	transitions, err := store.ListTransitions(fixture.ctx, session.ID, fixture.workspace.ID)
	if err != nil {
		t.Fatalf("ListTransitions: %v", err)
	}
	if len(transitions) != 1 {
		t.Fatalf("got %d transitions, want 1", len(transitions))
	}
	// The first transition has no previous state. A session comes into
	// existence already queued, and recording a move it never made would be a
	// fiction in a trail that cannot be edited.
	if transitions[0].PreviousState != nil {
		t.Errorf("previous_state = %q, want absent on the first transition",
			*transitions[0].PreviousState)
	}
	if transitions[0].NextState != domain.SessionQueued {
		t.Errorf("next_state = %q, want queued", transitions[0].NextState)
	}

	pending, err := store.ListPendingOutbox(fixture.ctx, fixture.workspace.ID)
	if err != nil {
		t.Fatalf("ListPendingOutbox: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("got %d pending outbox rows, want exactly 1", len(pending))
	}
	if pending[0].SubjectID != session.ID {
		t.Errorf("outbox subject = %s, want the session", pending[0].SubjectID)
	}
	if pending[0].Attempts != 0 {
		t.Errorf("attempts = %d, want 0 — the row is enqueued unclaimed", pending[0].Attempts)
	}
	if pending[0].Topic != application.TopicSessionCreated {
		t.Errorf("topic = %q, want %q", pending[0].Topic, application.TopicSessionCreated)
	}
}

// TestAFailedSessionWriteLeavesNothingBehindIntegration is the half that
// matters, and the one a success-only test would let pass.
//
// The audit event is the last of the six writes, so making it fail — a blank
// action, which audit_events_action_not_blank refuses — exercises the case
// where everything before it succeeded. If any of those rows survived, a
// session would exist that nothing had promised to run, which is precisely the
// failure the single transaction exists to prevent.
func TestAFailedSessionWriteLeavesNothingBehindIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewSessionStore(pool)
	fixture := seedSessionFixture(t, pool, "Session Rollback Workspace")

	session, err := fixture.createSession(t, store, func(audit *application.AuditEvent, _ *application.OutboxEvent) {
		audit.Action = ""
	})
	if err == nil {
		t.Fatal("Create with a blank audit action should fail")
	}
	if session.ID != uuid.Nil {
		t.Errorf("a failed Create returned session %s", session.ID)
	}

	// Every row, checked directly rather than through the store, because the
	// store's readers are scoped and a test that asks the wrong question
	// proves nothing.
	counts := map[string]string{
		"sessions":                  "SELECT count(*) FROM sessions WHERE workspace_id = $1",
		"session_participants":      "SELECT count(*) FROM session_participants WHERE workspace_id = $1",
		"session_state_transitions": "SELECT count(*) FROM session_state_transitions WHERE workspace_id = $1",
		"outbox_events":             "SELECT count(*) FROM outbox_events WHERE workspace_id = $1",
	}
	for table, query := range counts {
		var count int
		if err := pool.QueryRow(context.Background(), query, fixture.workspace.ID).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s has %d rows after a failed create; the transaction did not roll back",
				table, count)
		}
	}
}

// TestSessionTransitionsAreAppendOnlyIntegration proves the triggers rather
// than trusting the migration to have been read correctly.
func TestSessionTransitionsAreAppendOnlyIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewSessionStore(pool)
	fixture := seedSessionFixture(t, pool, "Session Append Only Workspace")

	session, err := fixture.createSession(t, store, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	transitions, err := store.ListTransitions(fixture.ctx, session.ID, fixture.workspace.ID)
	if err != nil || len(transitions) != 1 {
		t.Fatalf("ListTransitions = %v, %v", transitions, err)
	}
	id := transitions[0].ID

	refused := map[string]string{
		"UPDATE":   "UPDATE session_state_transitions SET reason = 'rewritten' WHERE id = $1",
		"DELETE":   "DELETE FROM session_state_transitions WHERE id = $1",
		"TRUNCATE": "TRUNCATE session_state_transitions",
	}
	for operation, statement := range refused {
		t.Run(operation, func(t *testing.T) {
			var execErr error
			if operation == "TRUNCATE" {
				// TRUNCATE takes no parameters, and is a separate trigger: a
				// row trigger alone leaves the table erasable in one
				// statement, which is why both exist.
				_, execErr = pool.Exec(context.Background(), statement)
			} else {
				_, execErr = pool.Exec(context.Background(), statement, id)
			}
			if execErr == nil {
				t.Fatalf("%s was allowed; session history must be append-only", operation)
			}
			if !strings.Contains(execErr.Error(), "append-only") {
				t.Errorf("%s was refused for the wrong reason: %v", operation, execErr)
			}
		})
	}

	// Deleting the workspace still works, which is what every other test's
	// cleanup depends on. The trigger permits a delete only when it is nested
	// and the parent session is already gone — the arrangement M4.1 worked out
	// for agent_versions, reused rather than rediscovered.
	if _, err := pool.Exec(context.Background(),
		"DELETE FROM workspaces WHERE id = $1", fixture.workspace.ID); err != nil {
		t.Fatalf("a session's history made its workspace undeletable: %v", err)
	}
}

// TestSessionTablesAreClosedWithoutContextIntegration connects as weave_app,
// which is the only way this proves anything: the owner is a superuser locally
// and bypasses every policy, so the same assertions against the owner pool
// would pass against no policies at all.
func TestSessionTablesAreClosedWithoutContextIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	ctx := context.Background()

	store := postgres.NewSessionStore(ownerPool)
	fixture := seedSessionFixture(t, ownerPool, "Session Isolation Workspace")
	if _, err := fixture.createSession(t, store, nil); err != nil {
		t.Fatalf("Create: %v", err)
	}

	tables := []string{"sessions", "session_participants", "session_state_transitions", "outbox_events"}

	// No tenant context at all: every policy matches nothing. An absent
	// context narrowing access rather than widening it is the whole design —
	// RLS that fails open looks like protection and is not.
	for _, table := range tables {
		var count int
		if err := appPool.QueryRow(ctx, "SELECT count(*) FROM "+table).Scan(&count); err != nil {
			t.Fatalf("select %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s returned %d rows with no tenant context, want 0", table, count)
		}
	}

	// And with another tenant's context, naming this workspace explicitly.
	other := seedUser(t, ownerPool)
	otherWorkspace := seedWorkspace(t, ownerPool, other, "Other Session Isolation Workspace")

	tx, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	setTenant(t, ctx, tx, other.ID.String(), otherWorkspace.ID.String())

	for _, table := range tables {
		var count int
		if err := tx.QueryRow(ctx,
			"SELECT count(*) FROM "+table+" WHERE workspace_id = $1", fixture.workspace.ID).Scan(&count); err != nil {
			t.Fatalf("cross-tenant select %s: %v", table, err)
		}
		if count != 0 {
			t.Errorf("%s returned %d rows for another tenant's workspace, want 0", table, count)
		}
	}

	// The control: the owning tenant does see its rows, so the assertions
	// above cannot be passing because the query is broken or the rows were
	// never written.
	owning, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = owning.Rollback(ctx) }()
	setTenant(t, ctx, owning, fixture.owner.ID.String(), fixture.workspace.ID.String())

	for _, table := range tables {
		var count int
		if err := owning.QueryRow(ctx,
			"SELECT count(*) FROM "+table+" WHERE workspace_id = $1", fixture.workspace.ID).Scan(&count); err != nil {
			t.Fatalf("own-tenant select %s: %v", table, err)
		}
		if count == 0 {
			t.Errorf("%s returned nothing to its own tenant, so the isolation "+
				"assertions above prove nothing", table)
		}
	}
}

// TestASessionCannotPinAnotherWorkspacesRowsIntegration proves the composite
// foreign keys, which are what stop workspace_id on a session being an
// unchecked copy — and the copy is what every policy reads.
func TestASessionCannotPinAnotherWorkspacesRowsIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewSessionStore(pool)

	mine := seedSessionFixture(t, pool, "Session FK Mine")
	theirs := seedSessionFixture(t, pool, "Session FK Theirs")

	cases := map[string]func(*domain.Session){
		"task":          func(s *domain.Session) { s.TaskID = theirs.task.ID },
		"agent version": func(s *domain.Session) { s.AgentVersionID = theirs.version.ID },
		"repository":    func(s *domain.Session) { s.RepositoryID = theirs.repository.ID },
	}

	for name, borrow := range cases {
		t.Run(name, func(t *testing.T) {
			sessionID, _ := domain.NewSessionID()
			branchName, _ := domain.SessionBranchName(sessionID, "borrowed")
			transitionID, _ := domain.NewTransitionID()
			eventID, _ := domain.NewOutboxEventID()

			session := domain.Session{
				ID: sessionID, WorkspaceID: mine.workspace.ID, TaskID: mine.task.ID,
				AgentVersionID: mine.version.ID, State: domain.SessionQueued,
				RepositoryID: mine.repository.ID, BranchName: branchName,
				CreatedBy: mine.owner.ID,
			}
			borrow(&session)

			_, err := store.Create(mine.ctx, session,
				domain.SessionParticipant{
					SessionID: sessionID, WorkspaceID: mine.workspace.ID,
					UserID: mine.owner.ID, Capacity: domain.ParticipantOwner,
				},
				domain.SessionStateTransition{
					ID: transitionID, SessionID: sessionID, WorkspaceID: mine.workspace.ID,
					NextState: domain.SessionQueued, ObservedVersion: 1,
				},
				application.OutboxEvent{
					ID: eventID, WorkspaceID: mine.workspace.ID,
					Topic: application.TopicSessionCreated, SubjectID: sessionID,
				},
				verifyTaskIsReady, mine.actor,
				application.AuditEvent{
					WorkspaceID: mine.workspace.ID, ActorUserID: mine.owner.ID,
					Action: application.AuditSessionCreated, Target: sessionID.String(),
				})
			if err == nil {
				t.Fatalf("a session pinned another workspace's %s", name)
			}
			// Reported as not-found rather than as a constraint violation: the
			// row is not reachable from this workspace, which is the same
			// answer as it not existing.
			if !errors.Is(err, application.ErrTaskNotFound) &&
				!errors.Is(err, application.ErrAgentVersionNotFound) &&
				!errors.Is(err, application.ErrRepositoryNotFound) {
				t.Errorf("refusal for a borrowed %s was not translated: %v", name, err)
			}
		})
	}
}

// TestATransitionAgainstAStaleVersionIsRefusedIntegration is the optimistic
// concurrency the spec asks for, and it needs real PostgreSQL because the
// version it checks is the one in the locked row.
//
// Two callers read version 1 and both act. The first moves the session and
// takes it to version 2; the second is still holding 1 and must be refused,
// or the second move would overwrite the first with nothing recording that it
// happened — and the history row would claim a move the session never made.
func TestATransitionAgainstAStaleVersionIsRefusedIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewSessionStore(pool)
	fixture := seedSessionFixture(t, pool, "Session Transition Workspace")

	session, err := fixture.createSession(t, store, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	controller := application.Actor{UserID: fixture.owner.ID, Required: domain.PermissionSessionControl}
	audit := func(moved domain.Session) application.AuditEvent {
		return application.AuditEvent{
			WorkspaceID: fixture.workspace.ID, ActorUserID: fixture.owner.ID,
			Action: application.AuditSessionTransitioned, Target: moved.ID.String(),
		}
	}
	move := func(to domain.SessionState, observed int32) (domain.Session, error) {
		transitionID, _ := domain.NewTransitionID()
		return store.Transition(fixture.ctx, session.ID, fixture.workspace.ID,
			func(current domain.Session) (domain.SessionStateTransition, error) {
				if current.Version != observed {
					return domain.SessionStateTransition{}, domain.ErrSessionVersionConflict
				}
				if !domain.CanTransition(current.State, to) {
					return domain.SessionStateTransition{}, domain.ErrTransitionNotAllowed
				}
				return domain.SessionStateTransition{
					ID: transitionID, SessionID: session.ID, WorkspaceID: fixture.workspace.ID,
					NextState: to, ObservedVersion: observed, ActorUserID: &fixture.owner.ID,
				}, nil
			}, controller, audit)
	}

	moved, err := move(domain.SessionProvisioning, 1)
	if err != nil {
		t.Fatalf("first transition: %v", err)
	}
	if moved.State != domain.SessionProvisioning {
		t.Errorf("state = %q, want provisioning", moved.State)
	}
	if moved.Version != 2 {
		t.Errorf("version = %d, want 2 — a transition must advance it", moved.Version)
	}

	// The second caller, still holding version 1.
	if _, err := move(domain.SessionRunning, 1); !errors.Is(err, domain.ErrSessionVersionConflict) {
		t.Fatalf("stale transition = %v, want a version conflict", err)
	}

	// And nothing was written by the refused attempt: two rows would mean the
	// history recorded a move that never happened.
	transitions, err := store.ListTransitions(fixture.ctx, session.ID, fixture.workspace.ID)
	if err != nil {
		t.Fatalf("ListTransitions: %v", err)
	}
	if len(transitions) != 2 {
		t.Fatalf("got %d transitions, want 2 (creation and the one move)", len(transitions))
	}
	if transitions[1].PreviousState == nil || *transitions[1].PreviousState != domain.SessionQueued {
		t.Errorf("the move's previous_state is %v, want queued", transitions[1].PreviousState)
	}

	after, err := store.Get(fixture.ctx, session.ID, fixture.workspace.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if after.State != domain.SessionProvisioning || after.Version != 2 {
		t.Errorf("session is %s at version %d; the refused move changed it",
			after.State, after.Version)
	}
}

// TestAnIllegalTransitionIsRefusedIntegration checks the table is consulted
// against the locked row rather than against whatever the caller believed.
func TestAnIllegalTransitionIsRefusedIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewSessionStore(pool)
	fixture := seedSessionFixture(t, pool, "Session Illegal Move Workspace")

	session, err := fixture.createSession(t, store, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	transitionID, _ := domain.NewTransitionID()
	_, err = store.Transition(fixture.ctx, session.ID, fixture.workspace.ID,
		func(current domain.Session) (domain.SessionStateTransition, error) {
			// queued cannot become completed: work that never ran cannot have
			// finished, and the table says so.
			if !domain.CanTransition(current.State, domain.SessionCompleted) {
				return domain.SessionStateTransition{}, domain.ErrTransitionNotAllowed
			}
			return domain.SessionStateTransition{
				ID: transitionID, SessionID: session.ID, WorkspaceID: fixture.workspace.ID,
				NextState: domain.SessionCompleted, ObservedVersion: current.Version,
			}, nil
		},
		application.Actor{UserID: fixture.owner.ID, Required: domain.PermissionSessionControl},
		func(domain.Session) application.AuditEvent {
			return application.AuditEvent{
				WorkspaceID: fixture.workspace.ID, ActorUserID: fixture.owner.ID,
				Action: application.AuditSessionTransitioned, Target: session.ID.String(),
			}
		})
	if !errors.Is(err, domain.ErrTransitionNotAllowed) {
		t.Fatalf("queued -> completed = %v, want it refused", err)
	}

	transitions, err := store.ListTransitions(fixture.ctx, session.ID, fixture.workspace.ID)
	if err != nil {
		t.Fatalf("ListTransitions: %v", err)
	}
	if len(transitions) != 1 {
		t.Errorf("got %d transitions, want only the creation row", len(transitions))
	}
}

// TestASessionRefusesATaskPatchedOutOfReadinessIntegration is the race two
// reviewers found, exercised.
//
// The task is read in one transaction and the session written in another, so a
// PATCH can commit in between — the same shape as the lost update M4.1 fixed
// in TaskService.Update, reappearing here in a different disguise. The check
// runs against the locked row, so this fails before anything is written.
func TestASessionRefusesATaskPatchedOutOfReadinessIntegration(t *testing.T) {
	pool := newPool(t)
	store := postgres.NewSessionStore(pool)
	fixture := seedSessionFixture(t, pool, "Session Stale Task Workspace")

	// Exactly what happens if a PATCH lands between the service's read and the
	// store's transaction.
	if _, err := pool.Exec(context.Background(),
		"UPDATE tasks SET status = 'draft', repository_id = NULL WHERE id = $1",
		fixture.task.ID); err != nil {
		t.Fatalf("patch task back to draft: %v", err)
	}

	// The service read a ready task, so the caller still believes it is one.
	if _, err := fixture.createSession(t, store, nil); !errors.Is(err, domain.ErrTaskNotRunnable) {
		t.Fatalf("Create against a task patched to draft = %v, want it refused", err)
	}

	var count int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM sessions WHERE workspace_id = $1", fixture.workspace.ID).Scan(&count); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if count != 0 {
		t.Errorf("a session was created for a task that is no longer ready")
	}
}

// TestTheApplicationRoleCannotDeleteASessionIntegration is least privilege
// where it protects an invariant rather than merely tidying a grant.
//
// Deleting a session cascades to session_state_transitions, and the
// append-only trigger *permits* that cascade — deliberately, so a tenant stays
// deletable. That makes DELETE on sessions a route to erasing history the
// table exists to keep, with nothing raising an error along the way. Invariant
// 10 says terminal history is immutable and reopening creates a linked
// continuation, so the application has no reason to hold it.
//
// Asserted as weave_app, because the owner is a superuser locally and would
// pass this while holding every privilege.
func TestTheApplicationRoleCannotDeleteASessionIntegration(t *testing.T) {
	ownerPool := newPool(t)
	appPool := newAppPool(t)
	ctx := context.Background()

	store := postgres.NewSessionStore(ownerPool)
	fixture := seedSessionFixture(t, ownerPool, "Session Grant Workspace")
	session, err := fixture.createSession(t, store, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	tx, err := appPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	setTenant(t, ctx, tx, fixture.owner.ID.String(), fixture.workspace.ID.String())

	// Its own tenant context, so this is refused by the grant rather than by a
	// policy matching nothing — which would pass for the wrong reason.
	_, err = tx.Exec(ctx, "DELETE FROM sessions WHERE id = $1", session.ID)
	if err == nil {
		t.Fatal("weave_app deleted a session, which cascades to append-only history")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("the refusal was not a privilege error, so the grant may still be held: %v", err)
	}
}
