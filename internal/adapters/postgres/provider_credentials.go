package postgres

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// ProviderCredentialStore writes verified metadata, never secret values. No
// cloud verifier/accessor or runner wiring is constructed by this adapter.
type ProviderCredentialStore struct{ pool *pgxpool.Pool }

var _ application.ProviderCredentialRepository = (*ProviderCredentialStore)(nil)

// NewProviderCredentialStore expects a tenant-enforcing application-role pool.
func NewProviderCredentialStore(pool *pgxpool.Pool) *ProviderCredentialStore {
	return &ProviderCredentialStore{pool: pool}
}

func credentialStoreError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	for _, safe := range []error{application.ErrPermissionDenied, application.ErrWorkspaceNotFound, application.ErrCredentialNotFound, application.ErrCredentialConflict, application.ErrCredentialReferenceRejected, application.ErrCredentialAlreadyDisabled, application.ErrIdempotencyFenced, domain.ErrInvalidCredentialEpoch, domain.ErrInvalidCredentialReference} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return application.ErrCredentialReferenceRejected
	}
	// PostgreSQL Details may include restricted resource names. Never wrap them.
	return application.ErrCredentialStoreUnavailable
}

func authorizeCredentialActor(ctx context.Context, q *postgresdb.Queries, ws uuid.UUID, actor application.Actor) error {
	if actor.System || actor.UserID == uuid.Nil || actor.Required != domain.PermissionWorkspaceManage {
		return application.ErrPermissionDenied
	}
	return authorizeActor(ctx, q, ws, actor)
}

// Authorize checks current membership under the shared workspace lock.
func (s *ProviderCredentialStore) Authorize(ctx context.Context, ws uuid.UUID, actor application.Actor) error {
	return credentialStoreError(ctx, inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error { return authorizeCredentialActor(ctx, q, ws, actor) }))
}

// Get reads status only after fresh authorization; refs never enter this DTO.
func (s *ProviderCredentialStore) Get(ctx context.Context, ws uuid.UUID, provider domain.Provider, actor application.Actor) (domain.ProviderCredentialBinding, error) {
	var out domain.ProviderCredentialBinding
	err := inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		if err := authorizeCredentialActor(ctx, q, ws, actor); err != nil {
			return err
		}
		row, err := q.GetProviderCredentialBinding(ctx, postgresdb.GetProviderCredentialBindingParams{WorkspaceID: ws, Provider: string(provider)})
		if errors.Is(err, pgx.ErrNoRows) {
			return application.ErrCredentialNotFound
		}
		if err != nil {
			return err
		}
		out = credentialBinding(row)
		return nil
	})
	if err != nil {
		return domain.ProviderCredentialBinding{}, credentialStoreError(ctx, err)
	}
	return out, nil
}

// Register rechecks permission and expected epoch before writing any approved
// metadata, and rejects shared resource ownership even across numeric versions.
func (s *ProviderCredentialStore) Register(ctx context.Context, r application.CredentialRegistration, actor application.Actor, c *application.CredentialCompletion) (domain.ProviderCredentialBinding, error) {
	var out domain.ProviderCredentialBinding
	err := inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		if err := authorizeCredentialActor(ctx, q, r.WorkspaceID, actor); err != nil {
			return err
		}
		if err := domain.ValidateCredentialEpoch(r.ExpectedEpoch); err != nil {
			return err
		}
		if r.Provider != domain.ProviderClaudeCode || r.Verified.WorkspaceID != r.WorkspaceID || r.Verified.Provider != r.Provider || r.Verified.VerificationID == uuid.Nil || r.BindingID == uuid.Nil || r.ResourceID == uuid.Nil || r.VersionID == uuid.Nil {
			return application.ErrCredentialReferenceRejected
		}
		ref := r.Verified.Reference
		if err := ref.Validate(); err != nil {
			return err
		}
		binding, err := q.GetProviderCredentialBinding(ctx, postgresdb.GetProviderCredentialBindingParams{WorkspaceID: r.WorkspaceID, Provider: string(r.Provider)})
		if errors.Is(err, pgx.ErrNoRows) {
			if r.ExpectedEpoch != 0 {
				return application.ErrCredentialConflict
			}
			binding, err = q.InsertProviderCredentialBinding(ctx, postgresdb.InsertProviderCredentialBindingParams{ID: r.BindingID, WorkspaceID: r.WorkspaceID, Provider: string(r.Provider), CreatedBy: actor.UserID})
		}
		if err != nil {
			return err
		}
		if binding.Epoch != r.ExpectedEpoch {
			return application.ErrCredentialConflict
		}
		resource, err := q.InsertProviderCredentialResource(ctx, postgresdb.InsertProviderCredentialResourceParams{ID: r.ResourceID, WorkspaceID: r.WorkspaceID, CredentialID: binding.ID, Environment: ref.Environment, ProjectNumber: ref.ProjectNumber, SecretID: ref.SecretID})
		if errors.Is(err, pgx.ErrNoRows) {
			resource, err = q.GetOwnedCredentialResource(ctx, postgresdb.GetOwnedCredentialResourceParams{ProjectNumber: ref.ProjectNumber, SecretID: ref.SecretID, WorkspaceID: r.WorkspaceID, CredentialID: binding.ID})
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrCredentialReferenceRejected
			}
		}
		if err != nil {
			return err
		}
		if resource.Environment != ref.Environment {
			return application.ErrCredentialReferenceRejected
		}
		if err = q.InsertProviderCredentialVersion(ctx, postgresdb.InsertProviderCredentialVersionParams{ID: r.VersionID, WorkspaceID: r.WorkspaceID, CredentialID: binding.ID, ResourceID: resource.ID, SecretVersion: ref.Version, RegistrationEpoch: r.ExpectedEpoch + 1, VerificationID: r.Verified.VerificationID, CreatedBy: actor.UserID}); err != nil {
			return err
		}
		binding, err = q.ActivateProviderCredential(ctx, postgresdb.ActivateProviderCredentialParams{WorkspaceID: r.WorkspaceID, Provider: string(r.Provider), Epoch: r.ExpectedEpoch, CurrentVersionID: nullableUUID(&r.VersionID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return application.ErrCredentialConflict
		}
		if err != nil {
			return err
		}
		out = credentialBinding(binding)
		if err := credentialAudit(ctx, q, out, actor, "workspace.provider_credential.registered"); err != nil {
			return err
		}
		return completeCredential(ctx, q, out, actor, "registerProviderCredential", c)
	})
	if err != nil {
		return domain.ProviderCredentialBinding{}, credentialStoreError(ctx, err)
	}
	return out, nil
}

// Disable advances metadata epoch only. No delivered key or runner is recalled.
func (s *ProviderCredentialStore) Disable(ctx context.Context, ws uuid.UUID, provider domain.Provider, expected int64, actor application.Actor, c *application.CredentialCompletion) (domain.ProviderCredentialBinding, error) {
	var out domain.ProviderCredentialBinding
	err := inTenantTx(ctx, s.pool, func(q *postgresdb.Queries) error {
		if err := authorizeCredentialActor(ctx, q, ws, actor); err != nil {
			return err
		}
		if err := domain.ValidateCredentialEpoch(expected); err != nil {
			return err
		}
		row, err := q.GetProviderCredentialBinding(ctx, postgresdb.GetProviderCredentialBindingParams{WorkspaceID: ws, Provider: string(provider)})
		if errors.Is(err, pgx.ErrNoRows) {
			return application.ErrCredentialNotFound
		}
		if err != nil {
			return err
		}
		if row.Epoch != expected {
			return application.ErrCredentialConflict
		}
		if row.State != "active" {
			return application.ErrCredentialAlreadyDisabled
		}
		row, err = q.DisableProviderCredential(ctx, postgresdb.DisableProviderCredentialParams{WorkspaceID: ws, Provider: string(provider), Epoch: expected})
		if errors.Is(err, pgx.ErrNoRows) {
			return application.ErrCredentialConflict
		}
		if err != nil {
			return err
		}
		out = credentialBinding(row)
		if err := credentialAudit(ctx, q, out, actor, "workspace.provider_credential.disabled"); err != nil {
			return err
		}
		return completeCredential(ctx, q, out, actor, "disableProviderCredential", c)
	})
	if err != nil {
		return domain.ProviderCredentialBinding{}, credentialStoreError(ctx, err)
	}
	return out, nil
}

func credentialBinding(row postgresdb.WorkspaceProviderCredential) domain.ProviderCredentialBinding {
	var version *uuid.UUID
	if row.CurrentVersionID.Valid {
		v := uuid.UUID(row.CurrentVersionID.Bytes)
		version = &v
	}
	return domain.ProviderCredentialBinding{ID: row.ID, WorkspaceID: row.WorkspaceID, Provider: domain.Provider(row.Provider), State: row.State, Epoch: row.Epoch, CurrentVersionID: version, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt.Time, UpdatedAt: row.UpdatedAt.Time}
}
func credentialAudit(ctx context.Context, q *postgresdb.Queries, b domain.ProviderCredentialBinding, actor application.Actor, action string) error {
	return appendAudit(ctx, q, application.AuditEvent{WorkspaceID: b.WorkspaceID, ActorUserID: actor.UserID, Action: action, Target: b.ID.String(), Detail: map[string]any{"provider": string(b.Provider), "epoch": b.Epoch, "state": b.State, "version_id": b.CurrentVersionID}})
}
func completeCredential(ctx context.Context, q *postgresdb.Queries, b domain.ProviderCredentialBinding, actor application.Actor, endpoint string, c *application.CredentialCompletion) error {
	if c == nil {
		return nil
	}
	if c.Claim.Scope.WorkspaceID != b.WorkspaceID || c.Claim.Scope.UserID != actor.UserID || c.Claim.Scope.Endpoint != endpoint || c.Render == nil {
		return application.ErrCredentialStoreUnavailable
	}
	status, body, err := c.Render(b)
	if err != nil {
		return application.ErrCredentialStoreUnavailable
	}
	if body == nil {
		body = []byte{}
	}
	return completeIdempotency(ctx, q, c.Claim, status, body)
}
