package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// InstallationStore persists GitHub App installations and the repositories
// they grant.
type InstallationStore struct {
	pool *pgxpool.Pool
}

// NewInstallationStore returns a store backed by pool.
func NewInstallationStore(pool *pgxpool.Pool) *InstallationStore {
	return &InstallationStore{pool: pool}
}

// inTx runs fn inside a transaction carrying the caller's tenant context.
func (s *InstallationStore) inTx(ctx context.Context, fn func(*postgresdb.Queries) error) error {
	return inTenantTx(ctx, s.pool, fn)
}

// Connect binds an installation to the workspace in context.
//
// The unique index on github_installation_id does the refusing, not a prior
// read: checking first and inserting second leaves a window in which two
// requests both see nothing and both insert. Translating the constraint
// violation is what makes the refusal race-free.
func (s *InstallationStore) Connect(
	ctx context.Context,
	installation domain.Installation,
	actor application.Actor,
	event application.AuditEvent,
) (domain.Installation, error) {
	var connected domain.Installation

	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		// The same in-transaction re-check every other mutation does: the
		// handler authorized from a snapshot, and the actor may have been
		// removed or demoted since — including while their browser sat on
		// GitHub's consent screen, which here is a long time.
		if err := authorizeActor(ctx, q, installation.WorkspaceID, actor); err != nil {
			return err
		}

		row, err := q.ConnectInstallation(ctx, postgresdb.ConnectInstallationParams{
			ID:                   installation.ID,
			WorkspaceID:          installation.WorkspaceID,
			GithubInstallationID: installation.GitHubID,
			AccountLogin:         installation.AccountLogin,
			AccountType:          string(installation.AccountType),
			RepositorySelection:  string(installation.RepositorySelection),
			ConnectedBy:          installation.ConnectedBy,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
				return domain.ErrInstallationBoundElsewhere
			}
			return fmt.Errorf("connect installation: %w", err)
		}
		connected = installationToDomain(row)
		return appendAudit(ctx, q, event)
	})
	return connected, err
}

// Resolve turns a GitHub installation id into the workspace it belongs to.
//
// Deliberately outside a tenant transaction and written by hand rather than
// generated: a webhook has no user and no workspace, so this is the one lookup
// that cannot be workspace-scoped. It goes through
// weave_installation_by_github_id, a SECURITY DEFINER function returning the
// two ids and a suspension flag.
//
// The caller is what keeps this narrow. Only the webhook handler may use it,
// and only after verifying the delivery signature — an installation id is a
// small integer, so unlike an invitation token it authorizes nothing by
// itself.
func (s *InstallationStore) Resolve(ctx context.Context, githubID int64) (application.InstallationRef, error) {
	const query = `SELECT installation_id, workspace_id, suspended
	               FROM weave_installation_by_github_id($1)`

	var ref application.InstallationRef
	err := s.pool.QueryRow(ctx, query, githubID).
		Scan(&ref.InstallationID, &ref.WorkspaceID, &ref.Suspended)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return application.InstallationRef{}, application.ErrInstallationNotFound
		}
		return application.InstallationRef{}, fmt.Errorf("resolve installation: %w", err)
	}
	return ref, nil
}

// Get returns one installation, scoped to the workspace in context.
func (s *InstallationStore) Get(ctx context.Context, installationID, workspaceID uuid.UUID) (domain.Installation, error) {
	var installation domain.Installation
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		row, err := q.GetInstallationForWorkspace(ctx, postgresdb.GetInstallationForWorkspaceParams{
			ID:          installationID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrInstallationNotFound
			}
			return fmt.Errorf("select installation: %w", err)
		}
		installation = installationToDomain(row)
		return nil
	})
	return installation, err
}

// List returns every installation bound to a workspace.
func (s *InstallationStore) List(ctx context.Context, workspaceID uuid.UUID) ([]domain.Installation, error) {
	var installations []domain.Installation
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		rows, err := q.ListInstallationsForWorkspace(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("list installations: %w", err)
		}
		installations = make([]domain.Installation, 0, len(rows))
		for _, row := range rows {
			installations = append(installations, installationToDomain(row))
		}
		return nil
	})
	return installations, err
}

// SetSuspended records or clears GitHub's suspension of an installation.
func (s *InstallationStore) SetSuspended(
	ctx context.Context,
	installationID, workspaceID uuid.UUID,
	suspendedAt *time.Time,
	event application.AuditEvent,
) error {
	return s.inTx(ctx, func(q *postgresdb.Queries) error {
		var at pgtype.Timestamptz
		if suspendedAt != nil {
			at = pgtype.Timestamptz{Time: *suspendedAt, Valid: true}
		}
		if _, err := q.SetInstallationSuspended(ctx, postgresdb.SetInstallationSuspendedParams{
			ID:          installationID,
			WorkspaceID: workspaceID,
			SuspendedAt: at,
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrInstallationNotFound
			}
			return fmt.Errorf("set installation suspension: %w", err)
		}
		return appendAudit(ctx, q, event)
	})
}

// Delete removes an installation GitHub reports as deleted.
func (s *InstallationStore) Delete(
	ctx context.Context,
	installationID, workspaceID uuid.UUID,
	event application.AuditEvent,
) error {
	return s.inTx(ctx, func(q *postgresdb.Queries) error {
		if err := q.DeleteInstallation(ctx, postgresdb.DeleteInstallationParams{
			ID:          installationID,
			WorkspaceID: workspaceID,
		}); err != nil {
			return fmt.Errorf("delete installation: %w", err)
		}
		return appendAudit(ctx, q, event)
	})
}

// Reconcile replaces what we believe an installation grants with what GitHub
// says it grants now.
//
// One transaction for the whole set, because a partial reconciliation is worse
// than a stale one: a crash between "withdraw everything" and "add back what
// is granted" would leave a workspace believing it had lost access to all its
// repositories.
func (s *InstallationStore) Reconcile(
	ctx context.Context,
	installationID, workspaceID uuid.UUID,
	selection domain.RepositorySelection,
	repositories []domain.Repository,
	permissions map[string]string,
) error {
	return s.inTx(ctx, func(q *postgresdb.Queries) error {
		if _, err := q.SetInstallationSelection(ctx, postgresdb.SetInstallationSelectionParams{
			ID:                  installationID,
			WorkspaceID:         workspaceID,
			RepositorySelection: string(selection),
		}); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrInstallationNotFound
			}
			return fmt.Errorf("update repository selection: %w", err)
		}

		grantedIDs := make([]int64, 0, len(repositories))
		for _, repository := range repositories {
			id, err := domain.NewRepositoryID()
			if err != nil {
				return err
			}
			if _, err := q.UpsertRepository(ctx, postgresdb.UpsertRepositoryParams{
				ID:                 id,
				WorkspaceID:        workspaceID,
				InstallationID:     installationID,
				GithubRepositoryID: repository.GitHubID,
				Owner:              repository.Owner,
				Name:               repository.Name,
				DefaultBranch:      repository.DefaultBranch,
				Private:            repository.Private,
			}); err != nil {
				return fmt.Errorf("upsert repository: %w", err)
			}
			grantedIDs = append(grantedIDs, repository.GitHubID)
		}

		// Withdrawing by negation rather than by naming what was removed is
		// what catches a repository that disappeared without an event —
		// deleted, transferred away, or made invisible to the installation.
		if len(grantedIDs) == 0 {
			if err := q.WithdrawAllRepositories(ctx, postgresdb.WithdrawAllRepositoriesParams{
				InstallationID: installationID,
				WorkspaceID:    workspaceID,
			}); err != nil {
				return fmt.Errorf("withdraw repositories: %w", err)
			}
		} else if err := q.WithdrawRepositoriesNotIn(ctx, postgresdb.WithdrawRepositoriesNotInParams{
			InstallationID: installationID,
			WorkspaceID:    workspaceID,
			GrantedIds:     grantedIDs,
		}); err != nil {
			return fmt.Errorf("withdraw repositories: %w", err)
		}

		for permission, access := range permissions {
			if err := q.RecordInstallationPermission(ctx, postgresdb.RecordInstallationPermissionParams{
				InstallationID: installationID,
				WorkspaceID:    workspaceID,
				Permission:     permission,
				Access:         access,
			}); err != nil {
				return fmt.Errorf("record installation permission: %w", err)
			}
		}
		return nil
	})
}

// ListRepositories returns every repository recorded for a workspace,
// withdrawn ones included.
func (s *InstallationStore) ListRepositories(ctx context.Context, workspaceID uuid.UUID) ([]domain.Repository, error) {
	var repositories []domain.Repository
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		rows, err := q.ListRepositoriesForWorkspace(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("list repositories: %w", err)
		}
		repositories = make([]domain.Repository, 0, len(rows))
		for _, row := range rows {
			repositories = append(repositories, repositoryToDomain(row))
		}
		return nil
	})
	return repositories, err
}

// GetRepository returns one repository, scoped to the workspace in context.
func (s *InstallationStore) GetRepository(ctx context.Context, repositoryID, workspaceID uuid.UUID) (domain.Repository, error) {
	var repository domain.Repository
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		row, err := q.GetRepositoryForWorkspace(ctx, postgresdb.GetRepositoryForWorkspaceParams{
			ID:          repositoryID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrRepositoryNotFound
			}
			return fmt.Errorf("select repository: %w", err)
		}
		repository = repositoryToDomain(row)
		return nil
	})
	return repository, err
}

// ListPermissions returns the permissions an installation holds.
func (s *InstallationStore) ListPermissions(ctx context.Context, installationID, workspaceID uuid.UUID) (map[string]string, error) {
	permissions := map[string]string{}
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		rows, err := q.ListInstallationPermissions(ctx, postgresdb.ListInstallationPermissionsParams{
			InstallationID: installationID,
			WorkspaceID:    workspaceID,
		})
		if err != nil {
			return fmt.Errorf("list installation permissions: %w", err)
		}
		for _, row := range rows {
			permissions[row.Permission] = row.Access
		}
		return nil
	})
	return permissions, err
}

// RecordDelivery notes a webhook delivery and reports whether it is new.
//
// Outside a tenant transaction because a delivery is recorded before we know
// which workspace it concerns — and some name an installation we have no
// record of at all. The table holds an id, an event name and a timestamp, so
// there is no tenant data to scope.
//
// Returning false for a delivery already seen is the deduplication: GitHub
// retries, and a retry must not produce a second effect.
func (s *InstallationStore) RecordDelivery(ctx context.Context, deliveryID, event, action string) (bool, error) {
	const query = `INSERT INTO github_webhook_deliveries (delivery_id, event, action)
	               VALUES ($1, $2, $3)
	               ON CONFLICT (delivery_id) DO NOTHING
	               RETURNING delivery_id`

	var actionValue *string
	if action != "" {
		actionValue = &action
	}

	var recorded string
	err := s.pool.QueryRow(ctx, query, deliveryID, event, actionValue).Scan(&recorded)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("record webhook delivery: %w", err)
	default:
		return true, nil
	}
}

// installationToDomain converts a generated row.
func installationToDomain(row postgresdb.GithubInstallation) domain.Installation {
	installation := domain.Installation{
		ID:                  row.ID,
		WorkspaceID:         row.WorkspaceID,
		GitHubID:            row.GithubInstallationID,
		AccountLogin:        row.AccountLogin,
		AccountType:         domain.AccountType(row.AccountType),
		RepositorySelection: domain.RepositorySelection(row.RepositorySelection),
		ConnectedBy:         row.ConnectedBy,
		CreatedAt:           timestamp(row.CreatedAt),
		UpdatedAt:           timestamp(row.UpdatedAt),
	}
	if row.SuspendedAt.Valid {
		at := row.SuspendedAt.Time
		installation.SuspendedAt = &at
	}
	return installation
}

// repositoryToDomain converts a generated row.
func repositoryToDomain(row postgresdb.Repository) domain.Repository {
	return domain.Repository{
		ID:             row.ID,
		WorkspaceID:    row.WorkspaceID,
		InstallationID: row.InstallationID,
		GitHubID:       row.GithubRepositoryID,
		Owner:          row.Owner,
		Name:           row.Name,
		DefaultBranch:  row.DefaultBranch,
		Private:        row.Private,
		Granted:        row.Granted,
		CreatedAt:      timestamp(row.CreatedAt),
		UpdatedAt:      timestamp(row.UpdatedAt),
	}
}
