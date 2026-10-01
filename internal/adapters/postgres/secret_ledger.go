package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

type secretLedgerConfig struct {
	environment, project string
	principal            uuid.UUID
}

// SecretApprovalStore reads protected evidence under a dedicated capability pool.
// It grants no cloud ownership proof or runtime authorization and fetches no keys.
type SecretApprovalStore struct {
	pool   *pgxpool.Pool
	config secretLedgerConfig
}

var _ application.SecretApprovalReader = (*SecretApprovalStore)(nil)

// SecretApprovalWriter records trusted onboarding decisions only. Ordinary app
// pools cannot construct it; no service identity is granted the DB role here.
type SecretApprovalWriter struct {
	pool   *pgxpool.Pool
	config secretLedgerConfig
}

var _ application.SecretApprovalLedgerWriter = (*SecretApprovalWriter)(nil)

// NewSecretApprovalStore validates explicit config and the actual non-bypass
// reader role. Constructor I/O is limited to the supplied DB under five seconds.
func NewSecretApprovalStore(ctx context.Context, pool *pgxpool.Pool, environment, project string) (*SecretApprovalStore, error) {
	cfg := secretLedgerConfig{environment: environment, project: project}
	if err := validateSecretLedgerPool(ctx, pool, cfg, "weave_secret_approval_reader"); err != nil {
		return nil, err
	}
	return &SecretApprovalStore{pool: pool, config: cfg}, nil
}

// NewSecretApprovalWriter requires a dedicated non-bypass writer pool and a
// nonnil trusted principal attribution; neither role nor actor is inferred.
func NewSecretApprovalWriter(ctx context.Context, pool *pgxpool.Pool, environment, project string, principal uuid.UUID) (*SecretApprovalWriter, error) {
	cfg := secretLedgerConfig{environment: environment, project: project, principal: principal}
	if principal == uuid.Nil {
		return nil, application.ErrCredentialConfiguration
	}
	if err := validateSecretLedgerPool(ctx, pool, cfg, "weave_secret_approval_writer"); err != nil {
		return nil, err
	}
	return &SecretApprovalWriter{pool: pool, config: cfg}, nil
}

// validateSecretLedgerPool uses fixed catalog SQL, not dynamic/tenant SQL. A
// supplied pool is not evidence of privilege separation unless its role matches.
func validateSecretLedgerPool(ctx context.Context, pool *pgxpool.Pool, cfg secretLedgerConfig, want string) error {
	if pool == nil || (domain.ProviderSecretReference{Environment: cfg.environment, ProjectNumber: cfg.project, SecretID: uuid.UUID{15: 1}, Version: 1}).Validate() != nil {
		return application.ErrCredentialConfiguration
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var role string
	var unsafe bool
	err := pool.QueryRow(ctx, "SELECT current_user::text, (rolsuper OR rolbypassrls OR rolcanlogin OR rolinherit OR rolcreatedb OR rolcreaterole OR rolreplication) FROM pg_roles WHERE rolname=current_user").Scan(&role, &unsafe)
	if err != nil || unsafe || role != want {
		return application.ErrCredentialConfiguration
	}
	var allowed bool
	if want == "weave_secret_approval_reader" {
		err = pool.QueryRow(ctx, "SELECT has_schema_privilege(current_user,'public','USAGE') AND has_table_privilege(current_user,'provider_secret_approvals','SELECT') AND has_function_privilege(current_user,'weave_current_workspace_id()','EXECUTE')").Scan(&allowed)
	} else {
		err = pool.QueryRow(ctx, "SELECT has_schema_privilege(current_user,'public','USAGE') AND has_table_privilege(current_user,'provider_secret_intents','INSERT') AND has_column_privilege(current_user,'provider_secret_reservations','state','UPDATE') AND has_function_privilege(current_user,'weave_current_workspace_id()','EXECUTE')").Scan(&allowed)
	}
	if err != nil || !allowed {
		return application.ErrCredentialConfiguration
	}
	return nil
}

// secretLedgerTx preserves caller tenant scope and resets project settings at
// transaction end. No external I/O or callback runs inside its short transaction.
func secretLedgerTx(ctx context.Context, pool *pgxpool.Pool, cfg secretLedgerConfig, ws uuid.UUID, fn func(context.Context, *postgresdb.Queries) error) error {
	if ws == uuid.Nil || TenantFrom(ctx).WorkspaceID != ws {
		return application.ErrCredentialReferenceRejected
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return secretLedgerError(ctx, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err = applyTenantContext(ctx, tx, TenantFrom(ctx)); err != nil {
		return secretLedgerError(ctx, err)
	}
	if _, err = tx.Exec(ctx, "SELECT set_config('app.secret_environment',$1,true),set_config('app.secret_project',$2,true)", cfg.environment, cfg.project); err != nil {
		return secretLedgerError(ctx, err)
	}
	if err = fn(ctx, postgresdb.New(tx)); err != nil {
		return secretLedgerError(ctx, err)
	}
	return secretLedgerError(ctx, tx.Commit(ctx))
}

// secretLedgerError never wraps private SQL details or context causes.
func secretLedgerError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err == nil {
		return nil
	}
	if errors.Is(err, application.ErrCredentialReferenceRejected) || errors.Is(err, pgx.ErrNoRows) || errors.Is(err, domain.ErrInvalidSecretApproval) {
		return application.ErrCredentialReferenceRejected
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && (pgErr.Code == "23505" || pgErr.Code == "23503" || pgErr.Code == "23514") {
		return application.ErrCredentialReferenceRejected
	}
	return application.ErrSecretApprovalUnavailable
}

// ledgerScope requires exact configured project/environment, not a path alias.
func ledgerScope(cfg secretLedgerConfig, s domain.SecretApprovalScope) error {
	if s.Validate() != nil || s.Reference.Environment != cfg.environment || s.Reference.ProjectNumber != cfg.project {
		return application.ErrCredentialReferenceRejected
	}
	return nil
}

// LookupApproval reads current exact evidence, excluding resource/version withdrawal.
func (s *SecretApprovalStore) LookupApproval(ctx context.Context, scope domain.SecretApprovalScope) (domain.ProviderSecretApproval, error) {
	return s.lookup(ctx, uuid.Nil, scope, false)
}

// LookupSelectedApproval cannot follow a mutable current version or approval ID.
func (s *SecretApprovalStore) LookupSelectedApproval(ctx context.Context, id uuid.UUID, scope domain.SecretApprovalScope) (domain.ProviderSecretApproval, error) {
	return s.lookup(ctx, id, scope, true)
}

// lookup maps only restricted evidence, validates exact tuples and returns no data
// on transaction/commit failure. Generated rows must never be logged/serialized.
func (s *SecretApprovalStore) lookup(ctx context.Context, id uuid.UUID, scope domain.SecretApprovalScope, selected bool) (domain.ProviderSecretApproval, error) {
	zero := domain.ProviderSecretApproval{}
	if ledgerScope(s.config, scope) != nil || (selected && id == uuid.Nil) {
		return zero, application.ErrCredentialReferenceRejected
	}
	var out domain.ProviderSecretApproval
	err := secretLedgerTx(ctx, s.pool, s.config, scope.WorkspaceID, func(ctx context.Context, q *postgresdb.Queries) error {
		p := postgresdb.LookupSecretApprovalParams{WorkspaceID: scope.WorkspaceID, Provider: string(scope.Provider), Environment: scope.Reference.Environment, ProjectNumber: scope.Reference.ProjectNumber, SecretID: scope.Reference.SecretID, SecretVersion: scope.Reference.Version}
		var row postgresdb.LookupSecretApprovalRow
		var err error
		if selected {
			var r postgresdb.LookupSelectedSecretApprovalRow
			r, err = q.LookupSelectedSecretApproval(ctx, postgresdb.LookupSelectedSecretApprovalParams{ID: id, WorkspaceID: p.WorkspaceID, Provider: p.Provider, Environment: p.Environment, ProjectNumber: p.ProjectNumber, SecretID: p.SecretID, SecretVersion: p.SecretVersion})
			row = postgresdb.LookupSecretApprovalRow(r)
		} else {
			row, err = q.LookupSecretApproval(ctx, p)
		}
		if err != nil {
			return err
		}
		secret, err := domain.NewSecretCreationTime(row.SecretSeconds, row.SecretNanos)
		if err != nil {
			return err
		}
		version, err := domain.NewSecretCreationTime(row.VersionSeconds, row.VersionNanos)
		if err != nil {
			return err
		}
		out, err = domain.NewProviderSecretApproval(domain.SecretApprovalRecord{ID: row.ID, SchemaVersion: int(row.SchemaVersion), Scope: scope, ResourceFamily: row.ResourceFamily, CredentialKind: row.CredentialKind, SecretCreated: secret, VersionCreated: version})
		return err
	})
	if err != nil {
		return zero, err
	}
	return out, nil
}

// ReserveIntent commits a nonreusable name and tenant intent atomically before
// any cloud creation. Identical retries are safe; foreign/reused names refuse.
func (s *SecretApprovalWriter) ReserveIntent(ctx context.Context, r application.SecretReservationIntent) error {
	if r.ID == uuid.Nil || ledgerScope(s.config, r.Scope) != nil {
		return application.ErrCredentialReferenceRejected
	}
	return secretLedgerTx(ctx, s.pool, s.config, r.Scope.WorkspaceID, func(ctx context.Context, q *postgresdb.Queries) error {
		old, err := q.GetSecretIntent(ctx, postgresdb.GetSecretIntentParams{ID: r.ID, WorkspaceID: r.Scope.WorkspaceID})
		if err == nil {
			if old.Provider != string(r.Scope.Provider) || old.Environment != r.Scope.Reference.Environment || old.ProjectNumber != r.Scope.Reference.ProjectNumber || old.SecretID != r.Scope.Reference.SecretID || old.InitialVersion != r.Scope.Reference.Version || old.PrincipalID != s.config.principal {
				return application.ErrCredentialReferenceRejected
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err = q.InsertSecretReservation(ctx, postgresdb.InsertSecretReservationParams{ProjectNumber: r.Scope.Reference.ProjectNumber, SecretID: r.Scope.Reference.SecretID, Environment: r.Scope.Reference.Environment}); err != nil {
			return err
		}
		return q.InsertSecretIntent(ctx, postgresdb.InsertSecretIntentParams{ID: r.ID, WorkspaceID: r.Scope.WorkspaceID, Provider: string(r.Scope.Provider), Environment: r.Scope.Reference.Environment, ProjectNumber: r.Scope.Reference.ProjectNumber, SecretID: r.Scope.Reference.SecretID, InitialVersion: r.Scope.Reference.Version, PrincipalID: s.config.principal})
	})
}

// AssignResource records trusted exact creation evidence, never placeholder proof.
func (s *SecretApprovalWriter) AssignResource(ctx context.Context, ws, intent uuid.UUID, created domain.SecretCreationTime) error {
	if intent == uuid.Nil || created.Validate() != nil {
		return application.ErrCredentialReferenceRejected
	}
	return secretLedgerTx(ctx, s.pool, s.config, ws, func(ctx context.Context, q *postgresdb.Queries) error {
		if _, err := q.LockSecretIntentRoot(ctx, postgresdb.LockSecretIntentRootParams{ID: intent, WorkspaceID: ws}); err != nil {
			return err
		}
		sec, ns := created.Tuple()
		old, err := q.GetSecretAssignment(ctx, postgresdb.GetSecretAssignmentParams{IntentID: intent, WorkspaceID: ws})
		if err == nil {
			if old.SecretSeconds != sec || old.SecretNanos != ns || old.PrincipalID != s.config.principal {
				return application.ErrCredentialReferenceRejected
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return q.InsertSecretAssignment(ctx, postgresdb.InsertSecretAssignmentParams{IntentID: intent, WorkspaceID: ws, SecretSeconds: sec, SecretNanos: ns, PrincipalID: s.config.principal})
	})
}

// PublishApproval checks resource incarnation/scope under the shared root lock.
// Stable-ID retries compare immutable decision content, not current binding data.
func (s *SecretApprovalWriter) PublishApproval(ctx context.Context, intent uuid.UUID, a domain.ProviderSecretApproval) error {
	if intent == uuid.Nil || a.Validate() != nil || ledgerScope(s.config, a.Scope()) != nil {
		return application.ErrCredentialReferenceRejected
	}
	r := a.Record()
	ws := r.Scope.WorkspaceID
	return secretLedgerTx(ctx, s.pool, s.config, ws, func(ctx context.Context, q *postgresdb.Queries) error {
		if _, err := q.LockSecretIntentRoot(ctx, postgresdb.LockSecretIntentRootParams{ID: intent, WorkspaceID: ws}); err != nil {
			return err
		}
		i, err := q.GetSecretIntent(ctx, postgresdb.GetSecretIntentParams{ID: intent, WorkspaceID: ws})
		if err != nil {
			return err
		}
		if i.Provider != string(r.Scope.Provider) || i.Environment != r.Scope.Reference.Environment || i.ProjectNumber != r.Scope.Reference.ProjectNumber || i.SecretID != r.Scope.Reference.SecretID || i.ResourceFamily != r.ResourceFamily {
			return application.ErrCredentialReferenceRejected
		}
		assigned, err := q.GetSecretAssignment(ctx, postgresdb.GetSecretAssignmentParams{IntentID: intent, WorkspaceID: ws})
		if err != nil {
			return err
		}
		sec, ns := r.SecretCreated.Tuple()
		if assigned.SecretSeconds != sec || assigned.SecretNanos != ns {
			return application.ErrCredentialReferenceRejected
		}
		vsec, vns := r.VersionCreated.Tuple()
		old, err := q.GetSecretApprovalDecision(ctx, postgresdb.GetSecretApprovalDecisionParams{ID: r.ID, WorkspaceID: ws})
		if err == nil {
			if old.IntentID != intent || old.SecretVersion != r.Scope.Reference.Version || old.VersionSeconds != vsec || old.VersionNanos != vns || old.PrincipalID != s.config.principal {
				return application.ErrCredentialReferenceRejected
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return q.InsertSecretApproval(ctx, postgresdb.InsertSecretApprovalParams{ID: r.ID, IntentID: intent, WorkspaceID: ws, SecretVersion: r.Scope.Reference.Version, VersionSeconds: vsec, VersionNanos: vns, PrincipalID: s.config.principal})
	})
}

// Withdraw appends an immutable version-specific or whole-resource decision.
// Whole-resource withdrawal retires the root in that same transaction.
func (s *SecretApprovalWriter) Withdraw(ctx context.Context, ws, intent, decision uuid.UUID, approval *uuid.UUID) error {
	if intent == uuid.Nil || decision == uuid.Nil || (approval != nil && *approval == uuid.Nil) {
		return application.ErrCredentialReferenceRejected
	}
	return secretLedgerTx(ctx, s.pool, s.config, ws, func(ctx context.Context, q *postgresdb.Queries) error {
		if _, err := q.LockSecretIntentRoot(ctx, postgresdb.LockSecretIntentRootParams{ID: intent, WorkspaceID: ws}); err != nil {
			return err
		}
		id := nullableUUID(approval)
		old, err := q.GetSecretWithdrawal(ctx, postgresdb.GetSecretWithdrawalParams{ID: decision, WorkspaceID: ws})
		if err == nil {
			if old.IntentID != intent || old.ApprovalID != id || old.PrincipalID != s.config.principal {
				return application.ErrCredentialReferenceRejected
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return q.InsertSecretWithdrawal(ctx, postgresdb.InsertSecretWithdrawalParams{ID: decision, IntentID: intent, WorkspaceID: ws, ApprovalID: id, PrincipalID: s.config.principal})
	})
}
