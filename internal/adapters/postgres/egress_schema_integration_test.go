package postgres_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// egressSQL executes an isolated statement under verified synthetic tenant scope.
func egressSQL(pool *pgxpool.Pool, workspace uuid.UUID, query string, args ...any) error {
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "SELECT set_config('app.workspace_id',$1,true)", workspace.String()); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, query, args...); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// egressSnapshot reads raw rows with no application filter beyond the runner;
// RLS must independently prevent a foreign tenant from seeing that snapshot.
func egressSnapshot(t *testing.T, pool *pgxpool.Pool, workspace, runner uuid.UUID) ([]string, error) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "SELECT set_config('app.workspace_id',$1,true)", workspace.String()); err != nil {
		return nil, err
	}
	var hosts []string
	err = tx.QueryRow(ctx, "SELECT hosts FROM runner_egress_snapshots WHERE runner_id=$1", runner).Scan(&hosts)
	return hosts, err
}

// TestEgressSnapshotOldInsertAndImmutabilityIntegration exercises unchanged
// runner INSERT syntax, explicit empty snapshots, future-only edits and RLS.
func TestEgressSnapshotOldInsertAndImmutabilityIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "Egress Snapshots")
	session, err := f.createSession(t, postgres.NewSessionStore(owner), nil)
	if err != nil {
		t.Fatal(err)
	}
	runner := uuid.New()
	if err := egressSQL(app, f.workspace.ID, "INSERT INTO runners(id,session_id,workspace_id,backend) VALUES($1,$2,$3,'test')", runner, session.ID, f.workspace.ID); err != nil {
		t.Fatal(err)
	}
	hosts, err := egressSnapshot(t, app, f.workspace.ID, runner)
	if err != nil || hosts == nil || len(hosts) != 0 {
		t.Fatalf("explicit empty snapshot: %v %v", hosts, err)
	}
	insert := `INSERT INTO workspace_egress_hosts(id,workspace_id,hostname,created_by) VALUES($1,$2,$3,$4)`
	if err := egressSQL(app, f.workspace.ID, insert, uuid.New(), f.workspace.ID, "docs.example.com", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	hosts, err = egressSnapshot(t, app, f.workspace.ID, runner)
	if err != nil || len(hosts) != 0 {
		t.Fatalf("old snapshot changed: %v %v", hosts, err)
	}
	store := postgres.NewRunnerStore(app)
	ctx := postgres.WithTenantWorkspace(context.Background(), f.workspace.ID)
	if _, err := store.End(ctx, runner, f.workspace.ID, domain.RunnerTerminated, ""); err != nil {
		t.Fatal(err)
	}
	next, err := store.Create(ctx, domain.Runner{ID: uuid.New(), WorkspaceID: f.workspace.ID, SessionID: session.ID, Backend: "test"})
	if err != nil {
		t.Fatal(err)
	}
	hosts, err = egressSnapshot(t, app, f.workspace.ID, next.ID)
	if err != nil || len(hosts) != 1 || hosts[0] != "docs.example.com" {
		t.Fatalf("new snapshot: %v %v", hosts, err)
	}
	if _, err := egressSnapshot(t, app, uuid.New(), next.ID); err != pgx.ErrNoRows {
		t.Fatalf("foreign snapshot read: %v", err)
	}
	for _, sql := range []string{"UPDATE runner_egress_snapshots SET hosts=ARRAY[]::text[] WHERE runner_id=$1", "DELETE FROM runner_egress_snapshots WHERE runner_id=$1"} {
		if err := egressSQL(owner, f.workspace.ID, sql, next.ID); err == nil {
			t.Fatalf("owner mutation accepted: %s", sql)
		}
	}
	if err := egressSQL(app, f.workspace.ID, "INSERT INTO runner_egress_snapshots(runner_id,workspace_id,session_id,hosts) VALUES($1,$2,$3,ARRAY[]::text[])", uuid.New(), f.workspace.ID, session.ID); err == nil {
		t.Fatal("app directly wrote snapshot")
	}
	if err := egressSQL(app, uuid.New(), "INSERT INTO runners(id,session_id,workspace_id,backend) VALUES($1,$2,$3,'test')", uuid.New(), session.ID, f.workspace.ID); err == nil {
		t.Fatal("foreign runner inserted")
	}
	// Parent cleanup is permitted without giving clients snapshot DELETE access.
	if _, err := owner.Exec(context.Background(), "DELETE FROM workspaces WHERE id=$1", f.workspace.ID); err != nil {
		t.Fatalf("workspace cascade: %v", err)
	}
}

// TestEgressCapIsAtomicIntegration races direct application-role inserts at the
// boundary: removing the workspace lock lets concurrent inserts exceed the cap.
func TestEgressCapIsAtomicIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "Egress Cap")
	sql := `INSERT INTO workspace_egress_hosts(id,workspace_id,hostname,created_by) VALUES($1,$2,$3,$4)`
	for i := 0; i < 19; i++ {
		if err := egressSQL(app, f.workspace.ID, sql, uuid.New(), f.workspace.ID, fmt.Sprintf("h%d.example.com", i), f.owner.ID); err != nil {
			t.Fatal(err)
		}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	out := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			out <- egressSQL(app, f.workspace.ID, sql, uuid.New(), f.workspace.ID, fmt.Sprintf("race%d.example.com", i), f.owner.ID)
		}(i)
	}
	close(start)
	wg.Wait()
	close(out)
	success := 0
	for err := range out {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("%d additions succeeded; want exactly one", success)
	}
	var count int
	if err := owner.QueryRow(context.Background(), "SELECT count(*) FROM workspace_egress_hosts WHERE workspace_id=$1", f.workspace.ID).Scan(&count); err != nil || count != 20 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

// TestEgressPersistedShapeAndIsolationIntegration pins SQL validation and forced
// RLS; an application role cannot write another workspace or change canonical data.
func TestEgressPersistedShapeAndIsolationIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "Egress Shape")
	sql := `INSERT INTO workspace_egress_hosts(id,workspace_id,hostname,created_by) VALUES($1,$2,$3,$4)`
	for _, host := range []string{"UPPER.example.com", "127.0.0.1", "xn--bcher-kva.example", "foo.internal", "foo..example.com", "foo.example.com.", "*.example.com"} {
		if err := egressSQL(app, f.workspace.ID, sql, uuid.New(), f.workspace.ID, host, f.owner.ID); err == nil {
			t.Errorf("invalid persisted hostname accepted: %s", host)
		}
	}
	if err := egressSQL(app, uuid.New(), sql, uuid.New(), f.workspace.ID, "docs.example.com", f.owner.ID); err == nil {
		t.Fatal("cross-tenant insert accepted")
	}
	if err := egressSQL(app, f.workspace.ID, sql, uuid.New(), f.workspace.ID, "docs.example.com", f.owner.ID); err != nil {
		t.Fatal(err)
	}
	var forced int
	if err := owner.QueryRow(context.Background(), "SELECT count(*) FROM pg_class WHERE relname IN ('workspace_egress_hosts','runner_egress_snapshots') AND relrowsecurity AND relforcerowsecurity").Scan(&forced); err != nil || forced != 2 {
		t.Fatalf("forced RLS: %d %v", forced, err)
	}
}

// TestEgressSnapshotSerializesWithHostChangesIntegration is review feedback on
// PR #28: a runner created while a host change is in flight must wait for it,
// so its snapshot reflects one committed policy state. Without the runner-insert
// workspace lock, the snapshot would read the pre-change list mid-transaction.
func TestEgressSnapshotSerializesWithHostChangesIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "Egress Snapshot Race")
	session, err := f.createSession(t, postgres.NewSessionStore(owner), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	store := postgres.NewRunnerStore(app)
	tenant := postgres.WithTenantWorkspace(ctx, f.workspace.ID)

	// change runs one host mutation in an open transaction the way the store
	// does — workspace lock first — then races a runner creation against it.
	race := func(t *testing.T, mutate string, args ...any) []string {
		t.Helper()
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SELECT set_config('app.workspace_id',$1,true)", f.workspace.ID.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, "SELECT id FROM workspaces WHERE id=$1 FOR UPDATE", f.workspace.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, mutate, args...); err != nil {
			t.Fatal(err)
		}

		runner := uuid.New()
		done := make(chan error, 1)
		go func() {
			_, err := store.Create(tenant, domain.Runner{ID: runner, SessionID: session.ID, WorkspaceID: f.workspace.ID, Backend: "test"})
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("runner creation did not wait for the in-flight host change (err=%v)", err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatalf("runner after commit: %v", err)
		}
		hosts, err := egressSnapshot(t, app, f.workspace.ID, runner)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.End(tenant, runner, f.workspace.ID, domain.RunnerTerminated, ""); err != nil {
			t.Fatal(err)
		}
		return hosts
	}

	id := uuid.New()
	if hosts := race(t, `INSERT INTO workspace_egress_hosts(id,workspace_id,hostname,created_by) VALUES($1,$2,$3,$4)`,
		id, f.workspace.ID, "docs.example.com", f.owner.ID); len(hosts) != 1 || hosts[0] != "docs.example.com" {
		t.Errorf("snapshot during an add = %v, want the committed addition", hosts)
	}
	if hosts := race(t, `DELETE FROM workspace_egress_hosts WHERE id=$1 AND workspace_id=$2`, id, f.workspace.ID); len(hosts) != 0 {
		t.Errorf("snapshot during a removal = %v, want the committed removal", hosts)
	}
}
