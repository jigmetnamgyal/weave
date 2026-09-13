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
	// Name is required. A caller that may retry needs the same request to
	// name the same branch; a generated name would make each attempt create
	// another. Validated strictly, because it reaches a ref.
	Name string
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

	// The name is required, and that is what makes the operation retryable.
	//
	// An earlier revision generated one when it was omitted. Each attempt then
	// produced a different random name, so a retry after a lost response
	// created a *second* branch rather than finding the first — the operation
	// was idempotent by name while quietly making the name non-deterministic.
	//
	// Generation still exists in the domain, for the session workflow in M4
	// where a session id supplies the stable identity a name can be derived
	// from. It does not belong on an endpoint whose caller may retry.
	if command.Name == "" {
		return domain.Branch{}, fmt.Errorf(
			"%w: a branch name is required, so that retrying this request finds the same branch "+
				"rather than creating another", domain.ErrInvalidBranchName)
	}
	name := command.Name
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
		return s.resolveExisting(existing, baseBranch, base)
	case errors.Is(err, ErrRemoteNotFound):
		// The ordinary path: nothing there yet. A branch that does not exist
		// still has no `protected` flag to read, and a ruleset matching
		// `weave/*` would govern every branch we create — so the absence of a
		// branch is not the absence of protection, and asking about the name
		// is the only way to find out.
		if err := s.requireUnrestrictedName(ctx, installation, repository, name); err != nil {
			return domain.Branch{}, err
		}
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
			return s.resolveExisting(raced, baseBranch, base)
		}
		return domain.Branch{}, fmt.Errorf("create branch: %w", err)
	}

	branch := domain.Branch{Name: created.Name, SHA: created.SHA, Base: base, Created: true}
	if err := s.auditBranch(ctx, membership, repository, branch); err != nil {
		// The branch exists and the record does not. Both facts go to the
		// caller: swallowing the error would hide an unaudited write, and
		// reporting a bare failure would suggest nothing happened when
		// something irreversible did.
		return branch, fmt.Errorf(
			"branch %s was created at %s but could not be recorded in the audit trail: %w",
			branch.Name, short(branch.SHA), err)
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

	// No audit row, and deliberately none.
	//
	// An earlier revision recorded one here when none existed, to recover an
	// audit lost between creating the branch and writing its row. That was
	// wrong: nothing distinguishes our own interrupted attempt from a branch
	// someone created by hand that happens to point at the same commit. The
	// repair would then assert, permanently and in an append-only trail, that
	// a member created something they did not.
	//
	// A missing audit row is a gap. A false one is a false statement about a
	// person that cannot be retracted, so the gap is the better failure — and
	// the creation path below reports loudly rather than silently when it
	// cannot record what it did.
	return domain.Branch{Name: existing.Name, SHA: existing.SHA, Base: baseName, Created: false}, nil
}

// auditTarget identifies a branch creation in the audit trail.
//
// Repository and branch together, because a branch is created once: that makes
// the target unique per effect, which is what lets a retry ask whether the
// effect was already recorded.
func auditTarget(repository domain.Repository, branch domain.Branch) string {
	return repository.FullName() + "#" + branch.Name
}

// requireUnrestrictedName refuses a name a ruleset governs.
//
// Checking an existing branch's `protected` flag covers only branches that
// exist. Rulesets match patterns, so one covering `weave/*` applies to every
// branch this unit creates and none of them would be found by that check —
// which is how invariant 11 could be satisfied everywhere it was tested and
// violated everywhere it mattered.
//
// **Fails closed.** An error here refuses the write rather than proceeding,
// for the same reason the row-level security policies match no row when
// context is absent: a protection check that fails open looks like protection
// and is not. The cost is that a GitHub outage blocks branch creation, which
// is the right way round for a control that exists to stop writes.
func (s *InstallationService) requireUnrestrictedName(
	ctx context.Context,
	installation domain.Installation,
	repository domain.Repository,
	name string,
) error {
	rule, err := s.api.BranchRules(ctx, installation.GitHubID, repository.Owner, repository.Name, name)
	if err != nil {
		return fmt.Errorf("read branch rules: %w", err)
	}
	if rule.Restricted {
		return fmt.Errorf("%w: a %q rule governs %s", domain.ErrBranchProtected, rule.Rule, name)
	}
	return nil
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
		Target:      auditTarget(repository, branch),
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
