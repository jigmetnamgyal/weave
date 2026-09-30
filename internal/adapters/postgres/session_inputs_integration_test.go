package postgres_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	weavedb "github.com/jigmetnamgyal/weave/db"
	"github.com/pressly/goose/v3"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// TestSessionInputsCaptureAndPinIntegration tests frozen text and the original
// agent-version pin after mutable task and current-agent edits.
func TestSessionInputsCaptureAndPinIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "Input Capture")
	body := "\nUnicode ☃ and literal $(not-a-command)\n"
	if err := egressSQL(app, f.workspace.ID, "UPDATE tasks SET body=$1 WHERE id=$2", body, f.task.ID); err != nil {
		t.Fatal(err)
	}
	session, err := f.createSession(t, postgres.NewSessionStore(app), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := egressSQL(app, f.workspace.ID, "UPDATE tasks SET title='new title',body='new body',status='draft' WHERE id=$1", f.task.ID); err != nil {
		t.Fatal(err)
	}
	nextVersion := uuid.New()
	if err := egressSQL(app, f.workspace.ID, `INSERT INTO agent_versions(id,agent_id,workspace_id,version,provider,model,capabilities,tool_policy,created_by)
 VALUES($1,$2,$3,2,'codex','new-model','{pause}','{"revision":2}',$4)`, nextVersion, f.version.AgentID, f.workspace.ID, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	if err := egressSQL(app, f.workspace.ID, "UPDATE agents SET current_version_id=$1 WHERE id=$2", nextVersion, f.version.AgentID); err != nil {
		t.Fatal(err)
	}
	input, err := postgres.NewSessionStore(app).GetInputs(f.ctx, session.ID, f.workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	if input.InputVersion != 1 || input.SessionID != session.ID || input.WorkspaceID != f.workspace.ID || input.TaskID != f.task.ID || input.TaskTitle != f.task.Title || input.TaskBody != body {
		t.Fatal("task input changed or content was transformed")
	}
	if input.AgentVersionID != f.version.ID || input.Provider != f.version.Provider || input.Model != f.version.Model || len(input.Capabilities) != 0 || string(input.ToolPolicy) != "{}" {
		t.Fatal("read mutable agent settings instead of pinned version")
	}
}

// TestSessionInputsRLSAndMissingIntegration proves RLS without a SQL workspace
// predicate, separately from the store's explicit workspace filter and no fallback.
func TestSessionInputsRLSAndMissingIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "Input RLS")
	session, err := f.createSession(t, postgres.NewSessionStore(app), nil)
	if err != nil {
		t.Fatal(err)
	}
	other := seedSessionFixture(t, owner, "Input Other")
	for _, ws := range []uuid.UUID{other.workspace.ID, uuid.Nil} {
		tx, err := app.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if ws != uuid.Nil {
			_, err = tx.Exec(context.Background(), "SELECT set_config('app.workspace_id',$1,true)", ws.String())
			if err != nil {
				t.Fatal(err)
			}
		}
		var id uuid.UUID
		err = tx.QueryRow(context.Background(), "SELECT session_id FROM session_input_snapshots WHERE session_id=$1", session.ID).Scan(&id)
		_ = tx.Rollback(context.Background())
		if !errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("snapshot visible without correct tenant: %v", err)
		}
	}
	store := postgres.NewSessionStore(app)
	for _, c := range []struct {
		ctx    context.Context
		id, ws uuid.UUID
	}{
		{other.ctx, session.ID, f.workspace.ID},
		{f.ctx, session.ID, other.workspace.ID},
		{context.Background(), session.ID, f.workspace.ID},
		{f.ctx, uuid.New(), f.workspace.ID},
	} {
		input, err := store.GetInputs(c.ctx, c.id, c.ws)
		if !errors.Is(err, application.ErrSessionInputUnavailable) || input.SessionID != uuid.Nil {
			t.Fatalf("missing/wrong tenant must refuse: %v", err)
		}
	}
	// Privileged fixture removal simulates a legacy session with no original
	// snapshot. Mutation trigger is disabled only within this rolled-back-safe
	// transaction; the owner can bypass guards and is outside the app boundary.
	tx, err := owner.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err = tx.Exec(context.Background(), "ALTER TABLE session_input_snapshots DISABLE TRIGGER session_input_snapshots_immutable"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(context.Background(), "DELETE FROM session_input_snapshots WHERE session_id=$1", session.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(context.Background(), "ALTER TABLE session_input_snapshots ENABLE TRIGGER session_input_snapshots_immutable"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = store.GetInputs(f.ctx, session.ID, f.workspace.ID); !errors.Is(err, application.ErrSessionInputUnavailable) {
		t.Fatal("legacy input silently recovered from mutable task")
	}
}

// TestSessionInputsGuardsIntegration verifies grants, immutable rows/pins and
// old INSERT compatibility; parent deletion is the only allowed row removal.
func TestSessionInputsGuardsIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "Input Guards")
	id := uuid.New()
	insert := `INSERT INTO sessions(id,workspace_id,task_id,agent_version_id,repository_id,branch_name,created_by)
 VALUES($1,$2,$3,$4,$5,'weave/input-test',$6)`
	if err := egressSQL(app, f.workspace.ID, insert, id, f.workspace.ID, f.task.ID, f.version.ID, f.repository.ID, f.owner.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.NewSessionStore(app).GetInputs(f.ctx, id, f.workspace.ID); err != nil {
		t.Fatal("old INSERT did not capture input", err)
	}
	for _, statement := range []string{
		"INSERT INTO session_input_snapshots(session_id,workspace_id,task_title,task_body) VALUES($1,$2,'x','x')",
		"UPDATE session_input_snapshots SET task_body='x' WHERE session_id=$1 AND workspace_id=$2",
		"DELETE FROM session_input_snapshots WHERE session_id=$1 AND workspace_id=$2",
	} {
		if err := egressSQL(app, f.workspace.ID, statement, id, f.workspace.ID); err == nil {
			t.Fatal("app snapshot write permitted")
		}
	}
	for _, statement := range []string{
		"UPDATE session_input_snapshots SET task_body='x' WHERE session_id=$1",
		"DELETE FROM session_input_snapshots WHERE session_id=$1",
	} {
		if _, err := owner.Exec(context.Background(), statement, id); err == nil {
			t.Fatal("snapshot mutation guard absent")
		}
	}
	if _, err := owner.Exec(context.Background(), "TRUNCATE session_input_snapshots"); err == nil {
		t.Fatal("snapshot truncate allowed")
	}
	for _, assignment := range []string{
		"agent_version_id=gen_random_uuid()", "task_id=gen_random_uuid()", "repository_id=gen_random_uuid()",
		"workspace_id=gen_random_uuid()", "id=gen_random_uuid()", "branch_name='changed'", "base_branch='changed'",
		"created_at=created_at+interval '1 second'", "created_by=gen_random_uuid()", "continues_id=gen_random_uuid()",
	} {
		err := egressSQL(app, f.workspace.ID, "UPDATE sessions SET "+assignment+" WHERE id=$1", id)
		var pgErr interface{ SQLState() string }
		if !errors.As(err, &pgErr) || pgErr.SQLState() != "23001" {
			t.Fatalf("identity guard bypassed for %s: %v", assignment, err)
		}
	}
	if err := egressSQL(app, f.workspace.ID, "UPDATE sessions SET state='provisioning',version=version+1 WHERE id=$1", id); err != nil {
		t.Fatal("old state UPDATE broken", err)
	}
	if err := egressSQL(app, f.workspace.ID, "UPDATE sessions SET branch_sha=repeat('a',40) WHERE id=$1", id); err != nil {
		t.Fatal("branch recording broken", err)
	}
	if _, err := owner.Exec(context.Background(), "DELETE FROM sessions WHERE id=$1", id); err != nil {
		t.Fatal("parent cascade failed", err)
	}
	var count int
	if err := owner.QueryRow(context.Background(), "SELECT count(*) FROM session_input_snapshots WHERE session_id=$1", id).Scan(&count); err != nil || count != 0 {
		t.Fatal("snapshot survived cascade", err)
	}
}

// TestSessionInputsRejectInvalidCaptureIntegration validates the exact locked
// task even for old-style app INSERT, with no orphan session/snapshot on failure.
func TestSessionInputsRejectInvalidCaptureIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "Input Refusal")
	insert := `INSERT INTO sessions(id,workspace_id,task_id,agent_version_id,repository_id,branch_name,created_by)
 VALUES($1,$2,$3,$4,$5,'weave/refusal',$6)`
	if err := egressSQL(app, f.workspace.ID, "UPDATE tasks SET status='draft' WHERE id=$1", f.task.ID); err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	err := egressSQL(app, f.workspace.ID, insert, id, f.workspace.ID, f.task.ID, f.version.ID, f.repository.ID, f.owner.ID)
	var pgErr interface{ SQLState() string }
	if !errors.As(err, &pgErr) || pgErr.SQLState() != "23514" {
		t.Fatal("draft capture did not refuse", err)
	}
	if err := egressSQL(app, f.workspace.ID, "UPDATE tasks SET status='ready' WHERE id=$1", f.task.ID); err != nil {
		t.Fatal(err)
	}
	err = egressSQL(app, f.workspace.ID, insert, id, f.workspace.ID, f.task.ID, f.version.ID, uuid.New(), f.owner.ID)
	if !errors.As(err, &pgErr) || pgErr.SQLState() != "23503" {
		t.Fatal("missing repository did not refuse with not-found constraint", err)
	}
	installations := postgres.NewInstallationStore(owner)
	repoID := nextGitHubID()
	if err := installations.Reconcile(f.ctx, f.repository.InstallationID, f.workspace.ID, domain.SelectionSelected, []domain.Repository{f.repository, {GitHubID: repoID, Owner: "acme", Name: "other", DefaultBranch: "main"}}, domain.RequiredPermissions); err != nil {
		t.Fatal(err)
	}
	repos, err := installations.ListRepositories(f.ctx, f.workspace.ID)
	if err != nil {
		t.Fatal(err)
	}
	var mismatched uuid.UUID
	for _, repo := range repos {
		if repo.GitHubID == repoID {
			mismatched = repo.ID
		}
	}
	if mismatched == uuid.Nil {
		t.Fatal("mismatch fixture missing")
	}
	err = egressSQL(app, f.workspace.ID, insert, id, f.workspace.ID, f.task.ID, f.version.ID, mismatched, f.owner.ID)
	if !errors.As(err, &pgErr) || pgErr.SQLState() != "23514" {
		t.Fatal("existing mismatched repository accepted", err)
	}
	other := seedSessionFixture(t, owner, "Input Foreign Pin")
	err = egressSQL(app, f.workspace.ID, insert, id, f.workspace.ID, f.task.ID, other.version.ID, f.repository.ID, f.owner.ID)
	if !errors.As(err, &pgErr) || pgErr.SQLState() != "23503" {
		t.Fatal("foreign version pin permitted", err)
	}
	for _, table := range []string{"sessions", "session_input_snapshots"} {
		var count int
		if err := owner.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE workspace_id=$1", f.workspace.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal("failed capture left data", err)
		}
	}
}

// TestSessionInputsSerializeWithTaskEditsIntegration tests the lock rather than
// relying on a race's luck: INSERT must be observed blocked on the task editor.
func TestSessionInputsSerializeWithTaskEditsIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "Input Lock")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	editor, err := owner.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = editor.Rollback(context.Background()) }()
	if _, err = editor.Exec(ctx, "UPDATE tasks SET body='edited before capture' WHERE id=$1", f.task.ID); err != nil {
		t.Fatal(err)
	}
	conn, err := app.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	creator, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = creator.Rollback(context.Background()) }()
	if _, err = creator.Exec(ctx, "SELECT set_config('app.workspace_id',$1,true)", f.workspace.ID.String()); err != nil {
		t.Fatal(err)
	}
	pid := conn.Conn().PgConn().PID()
	id := uuid.New()
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := creator.Exec(ctx, `INSERT INTO sessions(id,workspace_id,task_id,agent_version_id,repository_id,branch_name,created_by)
 VALUES($1,$2,$3,$4,$5,'weave/input-lock',$6)`, id, f.workspace.ID, f.task.ID, f.version.ID, f.repository.ID, f.owner.ID)
		result <- err
	}()
	defer func() {
		cancel()
		_ = editor.Rollback(context.Background())
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("capture goroutine did not stop")
		}
	}()
	for {
		var blocked bool
		if err := owner.QueryRow(ctx, "SELECT cardinality(pg_blocking_pids($1))>0", pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("capture did not serialize with task edit: %v", err)
		case <-ctx.Done():
			t.Fatal("capture never blocked on editor")
		case <-time.After(5 * time.Millisecond):
		}
	}
	if err = editor.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-result; err != nil {
		t.Fatal(err)
	}
	if err = creator.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	input, err := postgres.NewSessionStore(app).GetInputs(f.ctx, id, f.workspace.ID)
	if err != nil || input.TaskBody != "edited before capture" {
		t.Fatal("capture did not use serialized text", err)
	}
}

// TestSessionInputsMigrationLegacyIntegration uses a disposable database, never
// rolling back a shared test/dev database. It proves no fabricated historical
// inputs and recovery after the documented destructive snapshot rollback.
func TestSessionInputsMigrationLegacyIntegration(t *testing.T) {
	admin := newPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := "weave_input_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatal("create disposable migration database", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+identifier); err != nil {
			t.Error("drop owned migration database", err)
		}
	})
	config, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("invalid test database configuration")
	}
	config.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal("open migration fixture database")
	}
	t.Cleanup(pool.Close)
	sqlDB := stdlib.OpenDB(*config.ConnConfig)
	t.Cleanup(func() { _ = sqlDB.Close() })
	migrations, err := fs.Sub(weavedb.MigrationsFS, weavedb.MigrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 15); err != nil {
		t.Fatal("prepare previous schema", err)
	}
	f := seedSessionFixture(t, pool, "Legacy Input Migration")
	store := postgres.NewSessionStore(pool)
	legacy, err := f.createSession(t, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := egressSQL(pool, f.workspace.ID, "UPDATE tasks SET body='current mutable body' WHERE id=$1", f.task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 16); err != nil {
		t.Fatal("expand migration", err)
	}
	if _, err := store.GetInputs(f.ctx, legacy.ID, f.workspace.ID); !errors.Is(err, application.ErrSessionInputUnavailable) {
		t.Fatal("migration fabricated legacy input", err)
	}
	modern, err := f.createSession(t, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	input, err := store.GetInputs(f.ctx, modern.ID, f.workspace.ID)
	if err != nil || input.TaskBody != "current mutable body" {
		t.Fatal("new capture after expansion failed", err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatal("rollback migration with data", err)
	}
	if _, err := provider.UpTo(ctx, 16); err != nil {
		t.Fatal("restore migration", err)
	}
	if _, err := store.GetInputs(f.ctx, modern.ID, f.workspace.ID); !errors.Is(err, application.ErrSessionInputUnavailable) {
		t.Fatal("rollback data loss was hidden by reconstruction", err)
	}
	restored, err := f.createSession(t, store, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetInputs(f.ctx, restored.ID, f.workspace.ID); err != nil {
		t.Fatal("restored capture failed", err)
	}
}
