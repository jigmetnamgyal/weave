package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// referenceVerifier is a metadata-only test fake, not ownership attestation.
type referenceVerifier struct {
	before func()
	calls  int
}

// Verify runs a race hook before returning synthetic scoped evidence, not cloud proof.
func (v *referenceVerifier) Verify(_ context.Context, ws uuid.UUID, p domain.Provider, ref domain.ProviderSecretReference) (domain.VerifiedProviderReference, error) {
	v.calls++
	if v.before != nil {
		v.before()
	}
	return domain.VerifiedProviderReference{WorkspaceID: ws, Provider: p, Reference: ref, VerificationID: uuid.New()}, nil
}

// bindingMember supplies the fixture owner snapshot for authorization rechecks.
func bindingMember(f sessionFixture) domain.Membership {
	return domain.Membership{UserID: f.owner.ID, WorkspaceID: f.workspace.ID, Role: domain.RoleOwner}
}

// bindingRef creates an opaque synthetic reference without accessing a cloud resource.
func bindingRef() domain.ProviderSecretReference {
	return domain.ProviderSecretReference{Environment: "test", ProjectNumber: "918273645", SecretID: uuid.New(), Version: 1}
}

// bindingService uses the supplied database role and a test-only metadata verifier.
func bindingService(t *testing.T, pool *pgxpool.Pool, v *referenceVerifier) *application.ProviderCredentialService {
	t.Helper()
	store, err := postgres.NewProviderCredentialStore(pool)
	if err != nil {
		t.Fatal(err)
	}
	s, err := application.NewProviderCredentialService(store, v, "test", "918273645")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// bindingCounts checks atomic persistence using the fixture owner connection.
func bindingCounts(t *testing.T, pool *pgxpool.Pool, ws uuid.UUID, wantBinding, wantResource, wantVersion int) {
	t.Helper()
	for table, want := range map[string]int{"workspace_provider_credentials": wantBinding, "provider_credential_resources": wantResource, "provider_credential_versions": wantVersion} {
		var got int
		if err := pool.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE workspace_id=$1", ws).Scan(&got); err != nil || got != want {
			t.Fatalf("metadata row count for %s=%d want %d: %v", table, got, want, err)
		}
	}
}

// TestProviderCredentialLifecycleIntegration proves immutable rotation and status,
// stale intent rejection, disable, reactivation and missing fake-provider binding.
func TestProviderCredentialLifecycleIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "BYOK Lifecycle")
	v := &referenceVerifier{}
	s := bindingService(t, app, v)
	m, ref := bindingMember(f), bindingRef()
	b, err := s.Register(f.ctx, m, domain.ProviderClaudeCode, 0, ref, nil)
	if err != nil || b.State != "active" || b.Epoch != 1 || b.CurrentVersionID == nil {
		t.Fatal("create failed", err)
	}
	first := *b.CurrentVersionID
	ref.Version = 2
	b, err = s.Register(f.ctx, m, domain.ProviderClaudeCode, 1, ref, nil)
	if err != nil || b.Epoch != 2 || *b.CurrentVersionID == first {
		t.Fatal("rotation failed", err)
	}
	bindingCounts(t, owner, m.WorkspaceID, 1, 1, 2)
	calls := v.calls
	if _, err = s.Register(f.ctx, m, domain.ProviderClaudeCode, 1, ref, nil); !errors.Is(err, application.ErrCredentialConflict) || v.calls != calls {
		t.Fatal("stale epoch verified", err)
	}
	if _, err = s.Register(f.ctx, m, domain.ProviderClaudeCode, 2, ref, nil); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("same version reused", err)
	}
	b, err = s.Disable(f.ctx, m, domain.ProviderClaudeCode, 2, nil)
	if err != nil || b.State != "revoked" || b.Epoch != 3 {
		t.Fatal("disable failed", err)
	}
	if _, err = s.Disable(f.ctx, m, domain.ProviderClaudeCode, 3, nil); !errors.Is(err, application.ErrCredentialAlreadyDisabled) {
		t.Fatal("duplicate disable advanced epoch", err)
	}
	ref.Version = 3
	b, err = s.Register(f.ctx, m, domain.ProviderClaudeCode, 3, ref, nil)
	if err != nil || b.State != "active" || b.Epoch != 4 {
		t.Fatal("new approved reactivation failed", err)
	}
	got, err := s.Get(f.ctx, m, domain.ProviderClaudeCode)
	if err != nil || got.Epoch != 4 {
		t.Fatal("status read failed", err)
	}
	body, _ := json.Marshal(got)
	if strings.Contains(string(body), ref.ProjectNumber) || strings.Contains(string(body), ref.SecretID.String()) {
		t.Fatal("status leaked reference")
	}
	if _, err = s.Get(f.ctx, m, domain.ProviderFake); !errors.Is(err, application.ErrCredentialProviderUnsupported) {
		t.Fatal("fake unexpectedly has binding", err)
	}
}

// TestProviderCredentialPermissionAndRecheckIntegration covers every role and a
// membership removal while verification is outside the transaction.
func TestProviderCredentialPermissionAndRecheckIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "BYOK Roles")
	for _, role := range domain.Roles {
		t.Run(string(role), func(t *testing.T) {
			u := seedUser(t, owner)
			addMember(t, owner, f.workspace.ID, u.ID, role)
			ctx := postgres.WithTenant(context.Background(), postgres.TenantContext{UserID: u.ID, WorkspaceID: f.workspace.ID})
			v := &referenceVerifier{}
			s := bindingService(t, app, v)
			m := domain.Membership{WorkspaceID: f.workspace.ID, UserID: u.ID, Role: role}
			if role == domain.RoleOwner || role == domain.RoleAdmin {
				if err := s.Authorize(ctx, m); err != nil {
					t.Fatal(err)
				}
				roleFixture := seedSessionFixture(t, owner, "BYOK "+string(role)+" Write")
				addMember(t, owner, roleFixture.workspace.ID, u.ID, role)
				roleCtx := postgres.WithTenant(context.Background(), postgres.TenantContext{UserID: u.ID, WorkspaceID: roleFixture.workspace.ID})
				roleMember := domain.Membership{UserID: u.ID, WorkspaceID: roleFixture.workspace.ID, Role: role}
				created, err := s.Register(roleCtx, roleMember, domain.ProviderClaudeCode, 0, bindingRef(), nil)
				if err != nil || created.CreatedBy != u.ID {
					t.Fatal("owner/admin store registration denied", err)
				}
				if _, err := s.Disable(roleCtx, roleMember, domain.ProviderClaudeCode, 1, nil); err != nil {
					t.Fatal("owner/admin disable denied", err)
				}
			} else {
				if _, err := s.Register(ctx, m, domain.ProviderClaudeCode, 0, bindingRef(), nil); !errors.Is(err, application.ErrPermissionDenied) || v.calls != 0 {
					t.Fatal("denied role verified", err)
				}
			}
		})
	}
	v := &referenceVerifier{before: func() {
		if err := egressSQL(owner, f.workspace.ID, "DELETE FROM workspace_members WHERE workspace_id=$1 AND user_id=$2", f.workspace.ID, f.owner.ID); err != nil {
			t.Fatal(err)
		}
	}}
	s := bindingService(t, app, v)
	if _, err := s.Register(f.ctx, bindingMember(f), domain.ProviderClaudeCode, 0, bindingRef(), nil); err == nil {
		t.Fatal("removed actor committed")
	}
	bindingCounts(t, owner, f.workspace.ID, 0, 0, 0)
	if err := s.Authorize(f.ctx, bindingMember(f)); err == nil {
		t.Fatal("removed actor could replay")
	}
	store, err := postgres.NewProviderCredentialStore(app)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Authorize(f.ctx, f.workspace.ID, application.SystemActor()); !errors.Is(err, application.ErrPermissionDenied) {
		t.Fatal("system metadata mutation allowed", err)
	}
}

// TestProviderCredentialLateEpochAndCancellationIntegration forces the commit
// race after preflight, rather than relying on concurrent scheduling luck.
func TestProviderCredentialLateEpochAndCancellationIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "BYOK Late Fence")
	fast := bindingService(t, app, &referenceVerifier{})
	m, ref := bindingMember(f), bindingRef()
	if _, err := fast.Register(f.ctx, m, domain.ProviderClaudeCode, 0, ref, nil); err != nil {
		t.Fatal(err)
	}
	winner := ref
	winner.Version = 2
	slow := bindingService(t, app, &referenceVerifier{before: func() {
		if _, err := fast.Register(f.ctx, m, domain.ProviderClaudeCode, 1, winner, nil); err != nil {
			t.Fatal(err)
		}
	}})
	loser := ref
	loser.Version = 3
	if _, err := slow.Register(f.ctx, m, domain.ProviderClaudeCode, 1, loser, nil); !errors.Is(err, application.ErrCredentialConflict) {
		t.Fatal("late stale epoch committed", err)
	}
	bindingCounts(t, owner, m.WorkspaceID, 1, 1, 2)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	cancelled := bindingService(t, app, &referenceVerifier{before: cancel})
	if _, err := cancelled.Register(ctx, m, domain.ProviderClaudeCode, 2, loser, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled verification committed", err)
	}
	bindingCounts(t, owner, m.WorkspaceID, 1, 1, 2)
}

// TestProviderCredentialResourceOwnershipIntegration refuses cross-tenant reuse
// even at another numeric version, with no metadata partially committed.
func TestProviderCredentialResourceOwnershipIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	a := seedSessionFixture(t, owner, "BYOK Owner A")
	b := seedSessionFixture(t, owner, "BYOK Owner B")
	s := bindingService(t, app, &referenceVerifier{})
	ref := bindingRef()
	if _, err := s.Register(a.ctx, bindingMember(a), domain.ProviderClaudeCode, 0, ref, nil); err != nil {
		t.Fatal(err)
	}
	ref.Version = 2
	if _, err := s.Register(b.ctx, bindingMember(b), domain.ProviderClaudeCode, 0, ref, nil); !errors.Is(err, application.ErrCredentialReferenceRejected) {
		t.Fatal("foreign resource reused at another version", err)
	}
	bindingCounts(t, owner, b.workspace.ID, 0, 0, 0)
}

// TestProviderCredentialRLSAndGuardsIntegration independently tests raw app-role
// RLS and owner-bypass immutability/epoch/composite reference guards.
func TestProviderCredentialRLSAndGuardsIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	a := seedSessionFixture(t, owner, "BYOK Guards")
	b := seedSessionFixture(t, owner, "BYOK Foreign")
	s := bindingService(t, app, &referenceVerifier{})
	ref := bindingRef()
	row, err := s.Register(a.ctx, bindingMember(a), domain.ProviderClaudeCode, 0, ref, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ws := range []uuid.UUID{b.workspace.ID, uuid.Nil} {
		for _, table := range []string{"workspace_provider_credentials", "provider_credential_resources", "provider_credential_versions"} {
			tx, err := app.Begin(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if ws != uuid.Nil {
				if _, err = tx.Exec(context.Background(), "SELECT set_config('app.workspace_id',$1,true)", ws.String()); err != nil {
					t.Fatal(err)
				}
			}
			var count int
			err = tx.QueryRow(context.Background(), "SELECT count(*) FROM "+table+" WHERE workspace_id=$1", a.workspace.ID).Scan(&count)
			_ = tx.Rollback(context.Background())
			if err != nil || count != 0 {
				t.Fatal("RLS exposed credential metadata", err)
			}
		}
	}
	// Context and explicit workspace are independently enforced.
	if _, err = s.Get(b.ctx, bindingMember(a), domain.ProviderClaudeCode); err == nil {
		t.Fatal("wrong context read metadata")
	}
	for _, stmt := range []string{
		"DELETE FROM workspace_provider_credentials WHERE id=$1",
		"UPDATE workspace_provider_credentials SET epoch=epoch+2 WHERE id=$1",
		"UPDATE workspace_provider_credentials SET state='revoked',epoch=epoch+2 WHERE id=$1",
		"UPDATE workspace_provider_credentials SET provider='fake',epoch=epoch+1 WHERE id=$1",
		"UPDATE workspace_provider_credentials SET current_version_id=NULL,epoch=epoch+1 WHERE id=$1",
		"UPDATE workspace_provider_credentials SET state='pending',epoch=epoch+1 WHERE id=$1",
		"UPDATE provider_credential_resources SET environment='production' WHERE credential_id=$1",
		"DELETE FROM provider_credential_resources WHERE credential_id=$1",
		"UPDATE provider_credential_versions SET secret_version=999 WHERE credential_id=$1",
		"DELETE FROM provider_credential_versions WHERE credential_id=$1",
	} {
		if _, err := owner.Exec(context.Background(), stmt, row.ID); err == nil {
			t.Fatal("metadata guard bypassed")
		}
	}
	if _, err := owner.Exec(context.Background(), "TRUNCATE provider_credential_versions CASCADE"); err == nil {
		t.Fatal("truncate guard absent")
	}
	var resourceID uuid.UUID
	if err := owner.QueryRow(context.Background(), "SELECT id FROM provider_credential_resources WHERE credential_id=$1", row.ID).Scan(&resourceID); err != nil {
		t.Fatal(err)
	}
	// Owner connection bypasses RLS: the FK must still refuse another workspace.
	if _, err := owner.Exec(context.Background(), `INSERT INTO provider_credential_versions(id,workspace_id,credential_id,resource_id,secret_version,registration_epoch,verification_id,created_by) VALUES($1,$2,$3,$4,2,2,$5,$6)`, uuid.New(), b.workspace.ID, row.ID, resourceID, uuid.New(), b.owner.ID); err == nil {
		t.Fatal("foreign resource FK bypassed")
	}
	if err := egressSQL(app, a.workspace.ID, "DELETE FROM workspace_provider_credentials WHERE id=$1", row.ID); err == nil {
		t.Fatal("app could erase binding")
	}
}

// TestProviderCredentialFencingAndAtomicCompletionIntegration verifies response
// replay, stale claim rollback, render rollback, scope and safe response/audit.
func TestProviderCredentialFencingAndAtomicCompletionIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "BYOK Idempotency")
	s := bindingService(t, app, &referenceVerifier{})
	m, ref := bindingMember(f), bindingRef()
	keys := postgres.NewIdempotencyStore(app)
	record := domain.IdempotencyRecord{Scope: domain.IdempotencyScope{WorkspaceID: m.WorkspaceID, UserID: m.UserID, Endpoint: "registerProviderCredential"}, Key: "byok-register", Fingerprint: domain.FingerprintRequest("synthetic-reference-intent")}
	outcome, _, token, err := keys.Claim(f.ctx, record, application.IdempotencyLease)
	if err != nil || outcome != application.IdempotencyClaimed {
		t.Fatal(err)
	}
	c := &application.CredentialCompletion{Claim: application.IdempotentCompletion{Scope: record.Scope, Key: record.Key, Claimant: token, OriginRequestID: "byok-test"}, Render: func(b domain.ProviderCredentialBinding) (int, []byte, error) {
		body, err := json.Marshal(b)
		return 201, body, err
	}}
	b, err := s.Register(f.ctx, m, domain.ProviderClaudeCode, 0, ref, c)
	if err != nil {
		t.Fatal(err)
	}
	outcome, saved, _, err := keys.Claim(f.ctx, record, application.IdempotencyLease)
	if err != nil || outcome != application.IdempotencyComplete || saved.Response == nil {
		t.Fatal("replay not recorded", err)
	}
	if strings.Contains(string(saved.Response.Body), ref.ProjectNumber) || strings.Contains(string(saved.Response.Body), ref.SecretID.String()) {
		t.Fatal("cache exposed reference")
	}
	if err := s.Authorize(f.ctx, m); err != nil {
		t.Fatal("replay reauth", err)
	}
	ref.Version = 2
	c.Claim.Claimant = uuid.New()
	if _, err := s.Register(f.ctx, m, domain.ProviderClaudeCode, 1, ref, c); !errors.Is(err, application.ErrIdempotencyFenced) {
		t.Fatal("invalid claimant committed", err)
	}
	bindingCounts(t, owner, m.WorkspaceID, 1, 1, 1)
	c.Render = func(domain.ProviderCredentialBinding) (int, []byte, error) {
		return 0, nil, errors.New("SYNTHETIC_PRIVATE_MARKER")
	}
	if _, err := s.Register(f.ctx, m, domain.ProviderClaudeCode, 1, ref, c); !errors.Is(err, application.ErrCredentialStoreUnavailable) || strings.Contains(err.Error(), "SYNTHETIC_PRIVATE_MARKER") {
		t.Fatal("render committed/leaked", err)
	}
	bindingCounts(t, owner, m.WorkspaceID, 1, 1, 1)
	got, err := s.Get(f.ctx, m, domain.ProviderClaudeCode)
	if err != nil || got.Epoch != b.Epoch {
		t.Fatal("failed mutation advanced epoch", err)
	}
	var count int
	var detail string
	if err := owner.QueryRow(context.Background(), "SELECT count(*),coalesce(string_agg(detail::text,''),'') FROM audit_events WHERE workspace_id=$1 AND action='workspace.provider_credential.registered'", m.WorkspaceID).Scan(&count, &detail); err != nil || count != 1 || strings.Contains(detail, ref.ProjectNumber) || strings.Contains(detail, ref.SecretID.String()) {
		t.Fatal("audit duplicated or exposed reference", err)
	}
}

// TestProviderCredentialAuditRollbackIntegration causes a scoped synthetic
// audit failure and proves all metadata rolls back, without logging diagnostics.
func TestProviderCredentialAuditRollbackIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "BYOK Audit Rollback")
	name := "test_byok_audit_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	function := pgx.Identifier{name}.Sanitize()
	stmt := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.workspace_id='%s' AND NEW.action='workspace.provider_credential.registered' THEN RAISE EXCEPTION 'SYNTHETIC_PRIVATE_MARKER'; END IF; RETURN NEW; END; $$; CREATE TRIGGER %s BEFORE INSERT ON audit_events FOR EACH ROW EXECUTE FUNCTION %s();`, function, f.workspace.ID.String(), function, function)
	if _, err := owner.Exec(context.Background(), stmt); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = owner.Exec(context.Background(), "DROP TRIGGER "+function+" ON audit_events; DROP FUNCTION "+function+"();")
	}()
	_, err := bindingService(t, app, &referenceVerifier{}).Register(f.ctx, bindingMember(f), domain.ProviderClaudeCode, 0, bindingRef(), nil)
	if !errors.Is(err, application.ErrCredentialStoreUnavailable) || strings.Contains(err.Error(), "SYNTHETIC_PRIVATE_MARKER") {
		t.Fatal("audit failure leaked or committed", err)
	}
	bindingCounts(t, owner, f.workspace.ID, 0, 0, 0)
}

// TestProviderCredentialConcurrentRegistrationIntegration enforces one winner
// at epoch zero; the loser cannot borrow current state and append a rotation.
func TestProviderCredentialConcurrentRegistrationIntegration(t *testing.T) {
	owner, app := newPool(t), newAppPool(t)
	f := seedSessionFixture(t, owner, "BYOK Concurrency")
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, err := bindingService(t, app, &referenceVerifier{}).Register(f.ctx, bindingMember(f), domain.ProviderClaudeCode, 0, bindingRef(), nil)
			results <- err
		})
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, application.ErrCredentialConflict) {
			conflict++
		} else {
			t.Fatal("unexpected conflict", err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatal("registration was not fenced")
	}
	bindingCounts(t, owner, f.workspace.ID, 1, 1, 1)
}

// TestProviderCredentialMigrationRecoveryIntegration never rolls back a shared
// dev database. Down is destructive metadata rollback and Up must not invent it.
func TestProviderCredentialMigrationRecoveryIntegration(t *testing.T) {
	admin := newPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	name := "weave_byok_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{name}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+identifier); err != nil {
			t.Error("cleanup owned migration DB", err)
		}
	})
	cfg, err := pgxpool.ParseConfig(os.Getenv("TEST_DATABASE_URL"))
	if err != nil {
		t.Fatal("invalid fixture configuration")
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
	if _, err := provider.UpTo(ctx, 16); err != nil {
		t.Fatal(err)
	}
	f := seedSessionFixture(t, pool, "BYOK Migration")
	s := bindingService(t, pool, &referenceVerifier{})
	if _, err := provider.UpTo(ctx, 17); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Register(f.ctx, bindingMember(f), domain.ProviderClaudeCode, 0, bindingRef(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(ctx); err != nil {
		t.Fatal("Down with cyclic metadata FK", err)
	}
	if _, err := provider.UpTo(ctx, 17); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(f.ctx, bindingMember(f), domain.ProviderClaudeCode); !errors.Is(err, application.ErrCredentialNotFound) {
		t.Fatal("fabricated restored binding", err)
	}
	if _, err := s.Register(f.ctx, bindingMember(f), domain.ProviderClaudeCode, 0, bindingRef(), nil); err != nil {
		t.Fatal("reapply broken", err)
	}
}
