package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// CreateBranchCommand is a request to create a branch.
type CreateBranchCommand struct {
	RepositoryID uuid.UUID
	// Name is optional. When empty one is generated from Slug, which is the
	// normal path: a name the product chose cannot smuggle anything into a
	// ref. A supplied name is validated just as strictly, and exists so a
	// retry can name the branch its first attempt created.
	Name string
	// Slug describes the branch's purpose and seeds a generated name.
	Slug string
	// Base is the branch to create from. Empty means the repository's default.
	Base string
}

// CreateBranch creates a branch in a repository the workspace may reach.
//
// The order is the safety of this operation, so it is worth stating:
//
//  1. Validate the name before anything leaves the process. `..`, a `refs/`
//     prefix and control characters are rejected here, not by GitHub.
//  2. Resolve the repository through RepositoryForUse, which reconciles with
//     GitHub — a grant withdrawn since the page loaded is caught, because a
//     stored `granted` flag is a belief and this is a write.
//  3. Check the installation holds `contents: write`, and name it if not,
//     rather than relaying a 403 that says nothing actionable.
//  4. Resolve the base and refuse if it does not exist.
//  5. Refuse if the target already exists and is protected — invariant 11.
//  6. Create, and treat "already exists at the requested base" as success.
func (s *InstallationService) CreateBranch(
	ctx context.Context,
	membership domain.Membership,
	command CreateBranchCommand,
) (domain.Branch, error) {
	if !membership.Can(domain.PermissionRepositoryManage) {
		return domain.Branch{}, fmt.Errorf("%w: %s requires %s",
			ErrPermissionDenied, membership.Role, domain.PermissionRepositoryManage)
	}

	name := command.Name
	if name == "" {
		generated, err := domain.NewBranchName(command.Slug)
		if err != nil {
			return domain.Branch{}, err
		}
		name = generated
	}
	if err := domain.ValidateBranchName(name); err != nil {
		return domain.Branch{}, err
	}

	// Reconciles before answering, so a grant withdrawn on GitHub since this
	// page was rendered stops the write here rather than at GitHub.
	repository, err := s.RepositoryForUse(ctx, command.RepositoryID, membership.WorkspaceID)
	if err != nil {
		return domain.Branch{}, err
	}

	installation, err := s.installations.Get(ctx, repository.InstallationID, membership.WorkspaceID)
	if err != nil {
		return domain.Branch{}, err
	}
	if err := s.requireWritePermission(ctx, installation, membership.WorkspaceID); err != nil {
		return domain.Branch{}, err
	}

	base := command.Base
	if base == "" {
		base = repository.DefaultBranch
	}
	baseBranch, err := s.api.Branch(ctx, installation.GitHubID, repository.Owner, repository.Name, base)
	if err != nil {
		if errors.Is(err, ErrRemoteNotFound) {
			return domain.Branch{}, fmt.Errorf("%w: %s", domain.ErrBaseNotFound, base)
		}
		return domain.Branch{}, fmt.Errorf("read base branch: %w", err)
	}

	// Ask about the target before writing. A branch that already exists may be
	// protected, and the answer decides whether this is idempotent success, a
	// conflict, or a refusal.
	existing, err := s.api.Branch(ctx, installation.GitHubID, repository.Owner, repository.Name, name)
	switch {
	case err == nil:
		return s.resolveExisting(ctx, membership, repository, existing, baseBranch, base)
	case errors.Is(err, ErrRemoteNotFound):
		// The ordinary path: nothing there yet.
	default:
		return domain.Branch{}, fmt.Errorf("read target branch: %w", err)
	}

	created, err := s.api.CreateBranch(ctx, installation.GitHubID,
		repository.Owner, repository.Name, name, baseBranch.SHA)
	if err != nil {
		if errors.Is(err, ErrRemoteRefExists) {
			// Created between the read above and this write. The same three
			// outcomes apply, so re-read rather than guessing which.
			raced, readErr := s.api.Branch(ctx, installation.GitHubID, repository.Owner, repository.Name, name)
			if readErr != nil {
				return domain.Branch{}, fmt.Errorf("read branch after conflict: %w", readErr)
			}
			return s.resolveExisting(ctx, membership, repository, raced, baseBranch, base)
		}
		return domain.Branch{}, fmt.Errorf("create branch: %w", err)
	}

	branch := domain.Branch{Name: created.Name, SHA: created.SHA, Base: base, Created: true}
	if err := s.auditBranch(ctx, membership, repository, branch); err != nil {
		return domain.Branch{}, err
	}
	return branch, nil
}

// resolveExisting decides what an already-present branch means.
//
// Three outcomes, and collapsing any two of them loses something. Protected is
// refused outright. Pointing at the requested base is the idempotent success
// invariant 5 requires — a retry finding its own earlier work. Pointing
// anywhere else is a conflict, because handing back a branch at unknown work
// as though it were what was asked for is worse than an error.
func (s *InstallationService) resolveExisting(
	ctx context.Context,
	membership domain.Membership,
	repository domain.Repository,
	existing RemoteBranch,
	base RemoteBranch,
	baseName string,
) (domain.Branch, error) {
	if existing.Protected {
		return domain.Branch{}, fmt.Errorf("%w: %s", domain.ErrBranchProtected, existing.Name)
	}
	if existing.SHA != base.SHA {
		return domain.Branch{}, fmt.Errorf("%w: %s points at %s, not %s",
			domain.ErrBranchConflict, existing.Name, short(existing.SHA), short(base.SHA))
	}

	// No audit row. Nothing changed on GitHub, and an audit trail that records
	// attempts rather than effects stops being a record of what happened.
	_ = ctx
	_ = membership
	_ = repository
	return domain.Branch{Name: existing.Name, SHA: existing.SHA, Base: baseName, Created: false}, nil
}

// requireWritePermission refuses when the installation cannot write contents.
//
// Named rather than relayed. An App's permission set can be widened after an
// installation accepted the old one, and the installation keeps what it
// accepted until a human approves the change — so this is not a transient
// failure and no amount of retrying fixes it. GitHub's 403 says none of that.
func (s *InstallationService) requireWritePermission(
	ctx context.Context,
	installation domain.Installation,
	workspaceID uuid.UUID,
) error {
	remote, err := s.api.Installation(ctx, installation.GitHubID)
	granted := remote.Permissions
	if err != nil {
		// Fall back to what was recorded at the last reconciliation. Stale,
		// but refusing every write because GitHub is briefly unreachable would
		// be worse than acting on the last known answer.
		stored, storedErr := s.installations.ListPermissions(ctx, installation.ID, workspaceID)
		if storedErr != nil {
			return fmt.Errorf("read installation permissions: %w", err)
		}
		granted = stored
	}

	if access, ok := granted["contents"]; !ok || access == "read" {
		return fmt.Errorf(
			"%w: this installation does not hold contents:write, so it cannot create a branch. "+
				"Approve the updated permissions for %s on GitHub",
			ErrPermissionDenied, installation.AccountLogin)
	}
	return nil
}

// auditBranch records a creation against the member who caused it.
func (s *InstallationService) auditBranch(
	ctx context.Context,
	membership domain.Membership,
	repository domain.Repository,
	branch domain.Branch,
) error {
	return s.installations.AppendAudit(ctx, AuditEvent{
		WorkspaceID: membership.WorkspaceID,
		ActorUserID: membership.UserID,
		Action:      AuditBranchCreated,
		Target:      repository.FullName() + "#" + branch.Name,
		Detail: map[string]any{
			"repository": repository.FullName(),
			"branch":     branch.Name,
			"base":       branch.Base,
			"sha":        branch.SHA,
		},
	})
}

// short truncates a SHA for a message. Seven characters is what Git itself
// shows, and the full value is in the audit detail.
func short(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}
