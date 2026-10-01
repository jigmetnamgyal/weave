package postgres_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	weavedb "github.com/jigmetnamgyal/weave/db"
	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/pressly/goose/v3"
)

// ledgerRolePool uses an owner connection only to enter a genuine NOLOGIN test
// role; production attachment is not granted. Current role must not bypass RLS.
func ledgerRolePool(t *testing.T, role string, database string) *pgxpool.Pool {
	t.Helper()
	if os.Getenv("TEST_DATABASE_URL") == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("invalid test DB config")
	}
	if database != "" {
		cfg.ConnConfig.Database = database
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+pgx.Identifier{role}.Sanitize())
		return err
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	var actual string
	var unsafe bool
	if err = pool.QueryRow(context.Background(), "SELECT current_user::text,rolsuper OR rolbypassrls FROM pg_roles WHERE rolname=current_user").Scan(&actual, &unsafe); err != nil || unsafe || actual != role {
		t.Fatal("not an actual restricted role", err)
	}
	return pool
}

// ledgerStores constructs separate exact capability adapters with synthetic attribution.
func ledgerStores(t *testing.T, db string) (*postgres.SecretApprovalWriter, *postgres.SecretApprovalStore, *pgxpool.Pool, *pgxpool.Pool) {
	t.Helper()
	wp := ledgerRolePool(t, "weave_secret_approval_writer", db)
	rp := ledgerRolePool(t, "weave_secret_approval_reader", db)
	w, err := postgres.NewSecretApprovalWriter(context.Background(), wp, "test", "918273645", uuid.New())
	if err != nil {
		t.Fatal(err)
	}
	r, err := postgres.NewSecretApprovalStore(context.Background(), rp, "test", "918273645")
	if err != nil {
		t.Fatal(err)
	}
	return w, r, wp, rp
}

// ledgerEvidence creates synthetic immutable observations, not cloud proof.
func ledgerEvidence(t *testing.T, ws uuid.UUID) (application.SecretReservationIntent, domain.ProviderSecretApproval) {
	t.Helper()
	scope := domain.SecretApprovalScope{WorkspaceID: ws, Provider: domain.ProviderClaudeCode, Reference: bindingRef()}
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatal(err)
	}
	instant, err := domain.NewSecretCreationTime(1700000000, 123456789)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := domain.NewProviderSecretApproval(domain.SecretApprovalRecord{ID: uuid.New(), SchemaVersion: 1, Scope: scope, ResourceFamily: "gcp_global", CredentialKind: "api_key", SecretCreated: instant, VersionCreated: instant})
	if err != nil {
		t.Fatal(err)
	}
	return application.SecretReservationIntent{ID: id, Scope: scope}, approval
}

// publishLedgerFixture exercises each privileged operation on an actual writer role.
func publishLedgerFixture(t *testing.T, w *postgres.SecretApprovalWriter, ctx context.Context, intent application.SecretReservationIntent, a domain.ProviderSecretApproval) {
	t.Helper()
	if err := w.ReserveIntent(ctx, intent); err != nil {
		t.Fatal("reserve", err)
	}
	if err := w.AssignResource(ctx, intent.Scope.WorkspaceID, intent.ID, a.Record().SecretCreated); err != nil {
		t.Fatal("assign", err)
	}
	if err := w.PublishApproval(ctx, intent.ID, a); err != nil {
		t.Fatal("publish", err)
	}
}

// ledgerSQL establishes transaction-local test scope without adapter predicates.
func ledgerSQL(pool *pgxpool.Pool, ws uuid.UUID, project string, fn func(pgx.Tx) error) error {
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err = tx.Exec(ctx, "SELECT set_config('app.workspace_id',$1,true),set_config('app.secret_environment','test',true),set_config('app.secret_project',$2,true)", func() string {
		if ws == uuid.Nil {
			return ""
		}
		return ws.String()
	}(), project); err != nil {
		return err
	}
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// TestSecretLedgerLifecycleIntegration proves exact read/retry/version withdrawal
// and resource retirement without granting authority to current binding metadata.
func TestSecretLedgerLifecycleIntegration(t *testing.T) {
	owner := newPool(t)
	f := seedSessionFixture(t, owner, "Secret Ledger Lifecycle")
	w, r, _, _ := ledgerStores(t, "")
	intent, a := ledgerEvidence(t, f.workspace.ID)
	publishLedgerFixture(t, w, f.ctx, intent, a)
	publishLedgerFixture(t, w, f.ctx, intent, a)
	got, err := r.LookupApproval(f.ctx, intent.Scope)
	if err != nil || got != a {
		t.Fatal("exact evidence read", err)
	}
	got, err = r.LookupSelectedApproval(f.ctx, a.ID(), intent.Scope)
	if err != nil || got != a {
		t.Fatal("selected read", err)
	}
	if _, err = r.LookupSelectedApproval(f.ctx, uuid.New(), intent.Scope); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("unknown approval accepted", err)
	}
	changed := intent
	changed.Scope.Reference.Version++
	if err = w.ReserveIntent(f.ctx, changed); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("changed retry accepted", err)
	}
	// One-nanosecond incarnation difference must not publish.
	bad := a.Record()
	bad.ID = uuid.New()
	bad.Scope.Reference.Version = 2
	bad.SecretCreated, _ = domain.NewSecretCreationTime(1700000000, 123456788)
	wrong, _ := domain.NewProviderSecretApproval(bad)
	if err = w.PublishApproval(f.ctx, intent.ID, wrong); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("wrong incarnation published", err)
	}
	id := a.ID()
	decision := uuid.New()
	if err = w.Withdraw(f.ctx, f.workspace.ID, intent.ID, decision, &id); err != nil {
		t.Fatal(err)
	}
	if err = w.Withdraw(f.ctx, f.workspace.ID, intent.ID, decision, &id); err != nil {
		t.Fatal("withdrawal retry", err)
	}
	if _, err = r.LookupApproval(f.ctx, intent.Scope); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("withdrawn evidence returned", err)
	}
	next := a.Record()
	next.ID = uuid.New()
	next.Scope.Reference.Version = 2
	second, _ := domain.NewProviderSecretApproval(next)
	if err = w.PublishApproval(f.ctx, intent.ID, second); err != nil {
		t.Fatal("new version", err)
	}
	whole := uuid.New()
	if err = w.Withdraw(f.ctx, f.workspace.ID, intent.ID, whole, nil); err != nil {
		t.Fatal("resource withdrawal", err)
	}
	if err = w.Withdraw(f.ctx, f.workspace.ID, intent.ID, whole, nil); err != nil {
		t.Fatal("retire retry", err)
	}
	if _, err = r.LookupApproval(f.ctx, second.Scope()); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("resource withdrawal not projected", err)
	}
	next.ID = uuid.New()
	next.Scope.Reference.Version = 3
	third, _ := domain.NewProviderSecretApproval(next)
	if err = w.PublishApproval(f.ctx, intent.ID, third); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("retired resource republished", err)
	}
	var count int
	if err = owner.QueryRow(context.Background(), "SELECT count(*) FROM provider_secret_approvals WHERE intent_id=$1", intent.ID).Scan(&count); err != nil || count != 2 {
		t.Fatal("retry/failed writes altered history", err)
	}
}

// TestSecretLedgerRolesAndRLSIntegration checks native grants, missing/wrong
// tenant/project, raw query filtering and role/config constructor failures.
func TestSecretLedgerRolesAndRLSIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	a, b, _, _ := twoTenants(t, owner)
	w, r, wp, rp := ledgerStores(t, "")
	ctx := postgres.WithTenantWorkspace(context.Background(), a.ID)
	intent, proof := ledgerEvidence(t, a.ID)
	publishLedgerFixture(t, w, ctx, intent, proof)
	for _, ws := range []uuid.UUID{b.ID, uuid.Nil} {
		for _, table := range []string{"provider_secret_intents", "provider_secret_assignments", "provider_secret_approvals", "provider_secret_withdrawals"} {
			err := ledgerSQL(rp, ws, "918273645", func(tx pgx.Tx) error {
				var count int
				err := tx.QueryRow(context.Background(), "SELECT count(*) FROM "+table).Scan(&count)
				if err == nil && count != 0 {
					t.Fatal("raw RLS exposed rows")
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := ledgerSQL(rp, a.ID, "123456789", func(tx pgx.Tx) error {
		var count int
		err := tx.QueryRow(context.Background(), "SELECT count(*) FROM provider_secret_approvals").Scan(&count)
		if count != 0 {
			t.Fatal("wrong project visible")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"provider_secret_reservations", "provider_secret_intents", "provider_secret_assignments", "provider_secret_approvals", "provider_secret_withdrawals"} {
		if _, err := app.Exec(context.Background(), "SELECT * FROM "+table); err == nil {
			t.Fatal("app ledger grant")
		}
	}
	if _, err := rp.Exec(context.Background(), "SELECT * FROM provider_secret_reservations"); err == nil {
		t.Fatal("reader can inventory global names")
	}
	if err := ledgerSQL(rp, a.ID, "918273645", func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "INSERT INTO provider_secret_withdrawals(id,intent_id,workspace_id,principal_id) VALUES($1,$2,$3,$4)", uuid.New(), intent.ID, a.ID, uuid.New())
		return err
	}); err == nil {
		t.Fatal("reader became writer")
	}
	if _, err := app.Exec(context.Background(), "SET ROLE weave_secret_approval_writer"); err == nil {
		t.Fatal("app inherited writer role")
	}
	if _, err := postgres.NewSecretApprovalStore(context.Background(), app, "test", "918273645"); !errors.Is(err, application.ErrCredentialConfiguration) {
		t.Fatal("app pool reader construction", err)
	}
	if _, err := postgres.NewSecretApprovalWriter(context.Background(), rp, "test", "918273645", uuid.New()); !errors.Is(err, application.ErrCredentialConfiguration) {
		t.Fatal("reader pool writer construction", err)
	}
	if _, err := postgres.NewSecretApprovalStore(context.Background(), nil, "test", "918273645"); !errors.Is(err, application.ErrCredentialConfiguration) {
		t.Fatal("nil reader pool", err)
	}
	if _, err := postgres.NewSecretApprovalWriter(context.Background(), wp, "test", "918273645", uuid.Nil); !errors.Is(err, application.ErrCredentialConfiguration) {
		t.Fatal("missing principal", err)
	}
	if _, err := r.LookupApproval(context.Background(), intent.Scope); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("no-tenant adapter read", err)
	}
	foreign := intent.Scope
	foreign.WorkspaceID = b.ID
	if _, err := r.LookupApproval(postgres.WithTenantWorkspace(context.Background(), b.ID), foreign); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("foreign scoped read", err)
	}
}

// TestSecretLedgerImmutabilityAndCascadeIntegration demonstrates permanent
// root retention without tenant/actor evidence and owner-bypass mutation guards.
func TestSecretLedgerImmutabilityAndCascadeIntegration(t *testing.T) {
	owner := newPool(t)
	a, b, _, _ := twoTenants(t, owner)
	w, r, wp, _ := ledgerStores(t, "")
	ctx := postgres.WithTenantWorkspace(context.Background(), a.ID)
	intent, proof := ledgerEvidence(t, a.ID)
	publishLedgerFixture(t, w, ctx, intent, proof)
	approvalID := proof.ID()
	if err := w.Withdraw(ctx, a.ID, intent.ID, uuid.New(), &approvalID); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"UPDATE provider_secret_intents SET initial_version=9 WHERE id=$1",
		"DELETE FROM provider_secret_intents WHERE id=$1",
		"UPDATE provider_secret_assignments SET secret_nanos=0 WHERE intent_id=$1",
		"DELETE FROM provider_secret_assignments WHERE intent_id=$1",
		"UPDATE provider_secret_approvals SET version_nanos=0 WHERE intent_id=$1",
		"DELETE FROM provider_secret_approvals WHERE intent_id=$1",
		"UPDATE provider_secret_withdrawals SET principal_id='00000000-0000-0000-0000-000000000001' WHERE intent_id=$1",
		"DELETE FROM provider_secret_withdrawals WHERE intent_id=$1",
	} {
		if _, err := owner.Exec(context.Background(), stmt, intent.ID); err == nil {
			t.Fatal("evidence mutable")
		}
	}
	if _, err := owner.Exec(context.Background(), "TRUNCATE provider_secret_reservations CASCADE"); err == nil {
		t.Fatal("nonreuse truncate permitted")
	}
	// Bypass pure Go values: PostgreSQL independently rejects one-nanosecond
	// version chronology inversion against immutable resource observation.
	if err := ledgerSQL(wp, a.ID, "918273645", func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "INSERT INTO provider_secret_approvals(id,intent_id,workspace_id,secret_version,version_seconds,version_nanos,principal_id) VALUES($1,$2,$3,2,1700000000,123456788,$4)", uuid.New(), intent.ID, a.ID, uuid.New())
		return err
	}); err == nil {
		t.Fatal("database accepted chronology inversion")
	}
	// Retirement without a durable withdrawal must roll back at commit.
	if err := ledgerSQL(wp, a.ID, "918273645", func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "UPDATE provider_secret_reservations SET state='retired' WHERE project_number=$1 AND secret_id=$2", intent.Scope.Reference.ProjectNumber, intent.Scope.Reference.SecretID)
		return err
	}); err == nil {
		t.Fatal("retirement missing withdrawal committed")
	}
	// Cross-workspace raw FK injection cannot bypass the scoped decision guard.
	if err := ledgerSQL(wp, b.ID, "918273645", func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "INSERT INTO provider_secret_approvals(id,intent_id,workspace_id,secret_version,version_seconds,version_nanos,principal_id) VALUES($1,$2,$3,2,1700000000,123456789,$4)", uuid.New(), intent.ID, b.ID, uuid.New())
		return err
	}); err == nil {
		t.Fatal("foreign intent reference accepted")
	}
	if _, err := owner.Exec(context.Background(), "DELETE FROM workspaces WHERE id=$1", a.ID); err != nil {
		t.Fatal("workspace cascade failed", err)
	}
	for _, table := range []string{"provider_secret_intents", "provider_secret_assignments", "provider_secret_approvals", "provider_secret_withdrawals"} {
		var count int
		if err := owner.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE workspace_id=$1", a.ID).Scan(&count); err != nil || count != 0 {
			t.Fatal("tenant evidence retained", err)
		}
	}
	var state string
	if err := owner.QueryRow(context.Background(), "SELECT state FROM provider_secret_reservations WHERE project_number=$1 AND secret_id=$2", intent.Scope.Reference.ProjectNumber, intent.Scope.Reference.SecretID).Scan(&state); err != nil || state != "consumed" {
		t.Fatal("nonreuse root lost", err)
	}
	if _, err := r.LookupApproval(ctx, intent.Scope); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("deleted workspace authority revived", err)
	}
	if _, err := owner.Exec(context.Background(), "DELETE FROM provider_secret_reservations WHERE project_number=$1 AND secret_id=$2", intent.Scope.Reference.ProjectNumber, intent.Scope.Reference.SecretID); err == nil {
		t.Fatal("root erased after tenant deletion")
	}
	reused := intent
	reused.ID = uuid.New()
	reused.Scope.WorkspaceID = b.ID
	if err := w.ReserveIntent(postgres.WithTenantWorkspace(context.Background(), b.ID), reused); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("name reassigned after cascade", err)
	}
}

// TestSecretLedgerAtomicFailureAndConcurrencyIntegration catches orphan roots
// and races publication with whole-resource withdrawal under the shared lock.
func TestSecretLedgerAtomicFailureAndConcurrencyIntegration(t *testing.T) {
	owner := newPool(t)
	f := seedSessionFixture(t, owner, "Secret Ledger Atomic")
	w, r, _, _ := ledgerStores(t, "")
	bad, _ := ledgerEvidence(t, uuid.New())
	ctx := postgres.WithTenantWorkspace(context.Background(), bad.Scope.WorkspaceID)
	if err := w.ReserveIntent(ctx, bad); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("missing workspace reserve", err)
	}
	var count int
	if err := owner.QueryRow(context.Background(), "SELECT count(*) FROM provider_secret_reservations WHERE secret_id=$1", bad.Scope.Reference.SecretID).Scan(&count); err != nil || count != 0 {
		t.Fatal("failed intent retained orphan root", err)
	}
	intent, proof := ledgerEvidence(t, f.workspace.ID)
	publishLedgerFixture(t, w, f.ctx, intent, proof)
	next := proof.Record()
	next.ID = uuid.New()
	next.Scope.Reference.Version = 2
	second, _ := domain.NewProviderSecretApproval(next)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Go(func() { results <- w.PublishApproval(f.ctx, intent.ID, second) })
	wg.Go(func() { results <- w.Withdraw(f.ctx, f.workspace.ID, intent.ID, uuid.New(), nil) })
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, application.ErrCredentialReferenceRejected) {
			t.Fatal("unexpected concurrency failure", err)
		}
	}
	if _, err := r.LookupApproval(f.ctx, next.Scope); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("withdrawal/publication race left usable evidence", err)
	}
}

// TestSecretLedgerDowngradeGuardIntegration uses a separately owned disposable
// database: empty Down succeeds, populated Down refuses without freeing names.
func TestSecretLedgerDowngradeGuardIntegration(t *testing.T) {
	admin := newPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := "weave_secret_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			t.Error("owned migration DB cleanup", err)
		}
	})
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("invalid fixture config")
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	sqlDB := stdlib.OpenDB(*cfg.ConnConfig)
	t.Cleanup(func() { _ = sqlDB.Close() })
	migrations, err := fs.Sub(weavedb.MigrationsFS, weavedb.MigrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = provider.UpTo(ctx, 18); err != nil {
		t.Fatal(err)
	}
	if _, err = provider.Down(ctx); err != nil {
		t.Fatal("empty Down", err)
	}
	downgradedReader := ledgerRolePool(t, "weave_secret_approval_reader", name)
	var helper bool
	if err = downgradedReader.QueryRow(ctx, "SELECT has_function_privilege(current_user,'weave_current_workspace_id()','EXECUTE')").Scan(&helper); err != nil || helper {
		t.Fatal("Down retained helper privilege", err)
	}
	if _, err = postgres.NewSecretApprovalStore(ctx, downgradedReader, "test", "918273645"); !errors.Is(err, application.ErrCredentialConfiguration) {
		t.Fatal("missing ledger accepted at startup", err)
	}
	if _, err = provider.UpTo(ctx, 18); err != nil {
		t.Fatal("reapply", err)
	}
	f := seedSessionFixture(t, pool, "Ledger Downgrade")
	w, _, _, _ := ledgerStores(t, name)
	intent, proof := ledgerEvidence(t, f.workspace.ID)
	publishLedgerFixture(t, w, f.ctx, intent, proof)
	if _, err = provider.Down(ctx); err == nil {
		t.Fatal("populated Down freed nonreuse names")
	}
	var state string
	if err = pool.QueryRow(ctx, "SELECT state FROM provider_secret_reservations WHERE secret_id=$1", intent.Scope.Reference.SecretID).Scan(&state); err != nil || state != "consumed" {
		t.Fatal("failed Down damaged ledger", err)
	}
}

// TestSecretLedgerPendingReservationNonreuseIntegration catches reclaim of an
// unassigned name after its tenant intent cascades; live MVCC identity fences
// first intent attachment, not an intent ID reconstructed after deletion.
func TestSecretLedgerPendingReservationNonreuseIntegration(t *testing.T) {
	owner := newPool(t)
	a, b, _, _ := twoTenants(t, owner)
	w, _, wp, _ := ledgerStores(t, "")
	intent, _ := ledgerEvidence(t, a.ID)
	ctx := postgres.WithTenantWorkspace(context.Background(), a.ID)
	if err := w.ReserveIntent(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.Exec(context.Background(), "DELETE FROM workspaces WHERE id=$1", a.ID); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := owner.QueryRow(context.Background(), "SELECT state FROM provider_secret_reservations WHERE secret_id=$1", intent.Scope.Reference.SecretID).Scan(&state); err != nil || state != "reserved" {
		t.Fatal("pending root lost", err)
	}
	if err := ledgerSQL(wp, b.ID, "918273645", func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), "INSERT INTO provider_secret_intents(id,workspace_id,provider,environment,project_number,secret_id,initial_version,principal_id) VALUES($1,$2,'claude_code','test',$3,$4,1,$5)", uuid.New(), b.ID, intent.Scope.Reference.ProjectNumber, intent.Scope.Reference.SecretID, uuid.New())
		return err
	}); err == nil {
		t.Fatal("old pending reservation attached to a different intent")
	}
	// Failed/ambiguous live onboarding retires without fabricated creation evidence.
	pending, _ := ledgerEvidence(t, b.ID)
	other := postgres.WithTenantWorkspace(context.Background(), b.ID)
	if err := w.ReserveIntent(other, pending); err != nil {
		t.Fatal(err)
	}
	if err := w.Withdraw(other, b.ID, pending.ID, uuid.New(), nil); err != nil {
		t.Fatal("pending retirement", err)
	}
	created, _ := domain.NewSecretCreationTime(1, 1)
	if err := w.AssignResource(other, b.ID, pending.ID, created); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("retired pending resource consumed", err)
	}
}

// TestSecretLedgerReservationRaceAndCancellationIntegration verifies one lifetime
// reservation across tenants and caller cancellation without partial evidence.
func TestSecretLedgerReservationRaceAndCancellationIntegration(t *testing.T) {
	owner := newPool(t)
	a, b, _, _ := twoTenants(t, owner)
	w, _, _, _ := ledgerStores(t, "")
	first, _ := ledgerEvidence(t, a.ID)
	second := first
	second.ID = uuid.New()
	second.Scope.WorkspaceID = b.ID
	var wg sync.WaitGroup
	results := make(chan error, 2)
	wg.Go(func() { results <- w.ReserveIntent(postgres.WithTenantWorkspace(context.Background(), a.ID), first) })
	wg.Go(func() { results <- w.ReserveIntent(postgres.WithTenantWorkspace(context.Background(), b.ID), second) })
	wg.Wait()
	close(results)
	success := 0
	for err := range results {
		if err == nil {
			success++
		} else if !errors.Is(err, application.ErrCredentialReferenceRejected) {
			t.Fatal("reservation race failure", err)
		}
	}
	if success != 1 {
		t.Fatal("more than one lifetime reservation")
	}
	canceled, _ := ledgerEvidence(t, a.ID)
	ctx, cancel := context.WithCancel(postgres.WithTenantWorkspace(context.Background(), a.ID))
	cancel()
	if err := w.ReserveIntent(ctx, canceled); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled reserve", err)
	}
	var count int
	if err := owner.QueryRow(context.Background(), "SELECT count(*) FROM provider_secret_reservations WHERE secret_id=$1", canceled.Scope.Reference.SecretID).Scan(&count); err != nil || count != 0 {
		t.Fatal("cancellation left a root", err)
	}
}

// TestSecretLedgerInitialVersionFenceIntegration independently checks Go and
// PostgreSQL against approval drift before the exact initial decision exists.
func TestSecretLedgerInitialVersionFenceIntegration(t *testing.T) {
	owner := newPool(t)
	f := seedSessionFixture(t, owner, "Ledger Initial Version")
	w, _, wp, _ := ledgerStores(t, "")
	intent, proof := ledgerEvidence(t, f.workspace.ID)
	intent.Scope.Reference.Version = 5
	record := proof.Record()
	record.Scope = intent.Scope
	proof, _ = domain.NewProviderSecretApproval(record)
	if err := w.ReserveIntent(f.ctx, intent); err != nil {
		t.Fatal(err)
	}
	if err := w.AssignResource(f.ctx, f.workspace.ID, intent.ID, record.SecretCreated); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int64{1, 6} {
		wrong := record
		wrong.ID = uuid.New()
		wrong.Scope.Reference.Version = version
		drift, _ := domain.NewProviderSecretApproval(wrong)
		if err := ledgerSQL(wp, f.workspace.ID, "918273645", func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), "INSERT INTO provider_secret_approvals(id,intent_id,workspace_id,secret_version,version_seconds,version_nanos,principal_id) VALUES($1,$2,$3,$4,1700000000,123456789,$5)", uuid.New(), intent.ID, f.workspace.ID, version, uuid.New())
			if err == nil {
				t.Errorf("SQL accepted initial-version drift %d", version)
				return errors.New("synthetic rollback of invalid test decision")
			}
			return err
		}); err == nil {
			t.Errorf("SQL accepted initial-version drift %d", version)
		}
		if err := w.PublishApproval(f.ctx, intent.ID, drift); !errors.Is(err, application.ErrCredentialReferenceRejected) {
			t.Errorf("Go accepted initial-version drift %d: %v", version, err)
		}
	}
	if err := w.PublishApproval(f.ctx, intent.ID, proof); err != nil {
		t.Fatal("exact initial version", err)
	}
	record.ID = uuid.New()
	record.Scope.Reference.Version = 6
	later, _ := domain.NewProviderSecretApproval(record)
	if err := w.PublishApproval(f.ctx, intent.ID, later); err != nil {
		t.Fatal("later rotation rejected", err)
	}
	record.ID = uuid.New()
	record.Scope.Reference.Version = 4
	earlier, _ := domain.NewProviderSecretApproval(record)
	if err := w.PublishApproval(f.ctx, intent.ID, earlier); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("pre-initial version accepted after rotation", err)
	}
}

// TestSecretLedgerDowngradeRLSDenialIntegration executes the actual Down guard
// under a genuine non-bypass writer with no root scope. Hidden rows must cause
// refusal, not a false empty-ledger result; no destructive statement is run.
func TestSecretLedgerDowngradeRLSDenialIntegration(t *testing.T) {
	owner := newPool(t)
	f := seedSessionFixture(t, owner, "Ledger Downgrade RLS")
	w, _, wp, _ := ledgerStores(t, "")
	intent, proof := ledgerEvidence(t, f.workspace.ID)
	publishLedgerFixture(t, w, f.ctx, intent, proof)
	migrations, err := fs.Sub(weavedb.MigrationsFS, weavedb.MigrationsDir)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := fs.ReadFile(migrations, "00018_secret_approval_ledger.sql")
	if err != nil {
		t.Fatal(err)
	}
	down := strings.SplitN(string(migration), "-- +goose Down", 2)[1]
	guard := down[strings.Index(down, "DO $$"):strings.Index(down, "DROP TABLE")]
	if _, err := wp.Exec(context.Background(), guard); err == nil {
		t.Fatal("non-bypass Down guard silently treated hidden rows as empty")
	}
}
