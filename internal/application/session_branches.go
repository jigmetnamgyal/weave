package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// ErrSessionNotProvisioning is returned when the workflow asks for a branch
// for a session that has left `provisioning` without one — cancelled by a
// member between the two activities, say. Cutting a branch for a session that
// is no longer going to run would leave a ref nobody asked for.
var ErrSessionNotProvisioning = errors.New("session is not provisioning")

// Audit actions for a session's branch. Stable strings.
const (
	// AuditSessionBranchFound records a branch the workflow did not create on
	// this attempt but found already at the requested base.
	//
	// Distinct from AuditBranchCreated on purpose. The usual cause is our own
	// earlier attempt whose record was lost, but nothing proves that — and
	// M3.3's rule is that a false audit is worse than a missing one. "Found"
	// is true in every case; "created" is true in only some.
	AuditSessionBranchFound = "session.branch_found"
)

// SessionBranchCutter creates a branch on the product's behalf.
//
// Implemented by InstallationService.CreateBranchAsSystem. Narrow so that this
// service can cut a branch and do nothing else to an installation.
type SessionBranchCutter interface {
	CreateBranchAsSystem(ctx context.Context, workspaceID uuid.UUID, command CreateBranchCommand) (domain.Branch, error)
}

// SessionBranchStore reads a session and records its branch.
type SessionBranchStore interface {
	Get(ctx context.Context, sessionID, workspaceID uuid.UUID) (domain.Session, error)
	// RecordBranch writes the SHA and the audit row in one transaction, once.
	// The bool reports whether this call wrote them; false with no error
	// means the same SHA was already recorded.
	RecordBranch(
		ctx context.Context,
		sessionID, workspaceID uuid.UUID,
		sha string,
		actor Actor,
		audit func(domain.Session) AuditEvent,
	) (domain.Session, bool, error)
}

// SessionBranchService makes a session's branch exist.
type SessionBranchService struct {
	sessions SessionBranchStore
	cutter   SessionBranchCutter
}

// NewSessionBranchService wires the service.
func NewSessionBranchService(sessions SessionBranchStore, cutter SessionBranchCutter) *SessionBranchService {
	return &SessionBranchService{sessions: sessions, cutter: cutter}
}

// CutBranch makes the session's branch exist on GitHub, and records nothing.
//
// The first of two steps, split so that a branch GitHub made is never lost to
// a failure writing it down. The workflow runs this as one activity; once it
// completes, the branch and its SHA are in the workflow's history, which is
// durable. RecordBranch then runs as a second activity that touches only the
// database and can retry patiently, rather than re-asking GitHub and — if the
// database stays down — ending in a failure that calls GitHub unreachable while
// a real ref sits unrecorded.
//
// Called more than once for one session when Temporal redelivers. Three layers
// make the repeat harmless:
//
//  1. **The name is stable.** M4.2 derived it from the session id and stored
//     it; this reads it rather than generating one.
//  2. **Cutting is idempotent by name.** A branch already at the requested
//     base comes back as success with Created false.
//  3. **A recorded SHA short-circuits.** If the branch was already recorded,
//     that record is returned and GitHub is not asked anything.
//
// **Everything comes from the session row.** The workflow input names the
// workspace and the session; the repository, branch name and base are read
// here, under that workspace's scope. A session id from another workspace is
// not found, so the pair cannot be mismatched into reaching someone else's
// repository.
func (s *SessionBranchService) CutBranch(
	ctx context.Context,
	workspaceID, sessionID uuid.UUID,
) (domain.Branch, error) {
	session, err := s.sessions.Get(ctx, sessionID, workspaceID)
	if err != nil {
		return domain.Branch{}, err
	}
	if session.BranchSHA != "" {
		return domain.Branch{
			Name: session.BranchName, SHA: session.BranchSHA, Base: session.BaseBranch,
		}, nil
	}
	if session.State != domain.SessionProvisioning {
		return domain.Branch{}, fmt.Errorf("%w: it is %s", ErrSessionNotProvisioning, session.State)
	}

	return s.cutter.CreateBranchAsSystem(ctx, workspaceID, CreateBranchCommand{
		RepositoryID: session.RepositoryID,
		Name:         session.BranchName,
		Base:         session.BaseBranch,
	})
}

// RecordBranch writes the cut branch's SHA and its audit row, together, once.
//
// **The record is the SHA, not a separate audit.** Both are written in one
// transaction, so neither can exist without the other. A branch found rather
// than created — the usual cause is our own attempt whose completion was lost
// — is audited as *found*, which is true, rather than as created, which this
// cannot prove.
//
// Deliberately **not** conditional on the session's state. A ref that exists
// is a fact whether or not the session is still provisioning, and refusing to
// record it would leave a branch nobody knows about. Removing a branch for a
// session that will not run belongs to cancellation, which nothing routes to
// yet — see the tracker.
func (s *SessionBranchService) RecordBranch(
	ctx context.Context,
	workspaceID, sessionID uuid.UUID,
	branch domain.Branch,
) (domain.Session, error) {
	recorded, _, err := s.sessions.RecordBranch(ctx, sessionID, workspaceID, branch.SHA,
		SystemActor(),
		func(after domain.Session) AuditEvent {
			action := AuditBranchCreated
			if !branch.Created {
				action = AuditSessionBranchFound
			}
			return AuditEvent{
				WorkspaceID: workspaceID,
				// No actor. The system cut this, and naming the member who
				// created the session would put their name on a write they
				// did not make, at a time they did not choose.
				Action: action,
				Target: sessionID.String(),
				Detail: map[string]any{
					"session_id":    sessionID.String(),
					"repository_id": after.RepositoryID.String(),
					"branch":        branch.Name,
					"base":          branch.Base,
					"sha":           branch.SHA,
					"created":       branch.Created,
					"by":            "system",
				},
			}
		})
	if err != nil {
		return domain.Session{}, err
	}
	return recorded, nil
}

// BranchFailure is why a session's branch could not be made, as a stable code.
//
// A code rather than an error message, because it travels through workflow
// history into a transition reason that people read. Only this package's own
// strings reach that reason; nothing from GitHub's response does.
type BranchFailure string

// The reasons a branch can be refused for good. Each is a condition retrying
// cannot change: a person has to act — re-grant the repository, approve a
// permission, pick another base — before the session could succeed.
const (
	BranchFailureRepositoryUnavailable BranchFailure = "repository_unavailable"
	BranchFailureInstallationSuspended BranchFailure = "installation_suspended"
	BranchFailureNoWriteAccess         BranchFailure = "no_write_access"
	BranchFailureBaseMissing           BranchFailure = "base_missing"
	BranchFailureProtected             BranchFailure = "branch_protected"
	BranchFailureConflict              BranchFailure = "branch_conflict"
	BranchFailureInvalidName           BranchFailure = "invalid_branch_name"
	// BranchFailureRefused is GitHub itself saying no to a request the
	// prechecks passed — a permission or rule they could not see.
	BranchFailureRefused BranchFailure = "github_refused"
	// BranchFailureUnreachable is not terminal when raised: it is what the
	// workflow records once the bounded retries for a transient failure are
	// spent, so the session does not sit in `provisioning` for someone to find.
	BranchFailureUnreachable BranchFailure = "github_unreachable"
	// BranchFailureUnrecorded is the case that must never read as an outage:
	// GitHub made the branch and the database would not take its SHA, even
	// after an hour of retries. The ref exists; its commit is in the workflow
	// history and the worker's log.
	BranchFailureUnrecorded BranchFailure = "branch_unrecorded"
)

// ClassifyBranchFailure reports whether err is a refusal retrying cannot fix,
// and which.
//
// Classified here, where the errors are defined, rather than by matching
// strings in the workflow. Anything not listed is treated as transient, and
// the activity's retry policy bounds how long that assumption holds.
func ClassifyBranchFailure(err error) (BranchFailure, bool) {
	switch {
	case errors.Is(err, domain.ErrRepositoryNotGranted),
		errors.Is(err, ErrRepositoryNotFound),
		errors.Is(err, ErrInstallationNotFound),
		// GitHub no longer knows the installation: uninstalled, with the
		// webhook not yet processed. Reconciliation reports it this way.
		errors.Is(err, ErrRemoteNotFound):
		return BranchFailureRepositoryUnavailable, true
	case errors.Is(err, domain.ErrInstallationSuspended):
		return BranchFailureInstallationSuspended, true
	case errors.Is(err, ErrPermissionDenied):
		// The only permission refusal on the system path is the missing
		// `contents: write`; there is no membership to be refused.
		return BranchFailureNoWriteAccess, true
	case errors.Is(err, domain.ErrBaseNotFound):
		return BranchFailureBaseMissing, true
	case errors.Is(err, domain.ErrBranchProtected):
		return BranchFailureProtected, true
	case errors.Is(err, domain.ErrBranchConflict):
		return BranchFailureConflict, true
	case errors.Is(err, domain.ErrInvalidBranchName):
		return BranchFailureInvalidName, true
	case errors.Is(err, ErrRemoteRefused):
		return BranchFailureRefused, true
	default:
		return "", false
	}
}

// Reason is what the session's history says, naming the cause and, where
// there is one, what a person can do about it.
func (f BranchFailure) Reason() string {
	switch f {
	case BranchFailureRepositoryUnavailable:
		return "the session branch could not be created: the repository is no longer granted " +
			"to the GitHub App installation"
	case BranchFailureInstallationSuspended:
		return "the session branch could not be created: the GitHub App installation is suspended"
	case BranchFailureNoWriteAccess:
		return "the session branch could not be created: the installation does not hold " +
			"contents:write — approve the updated permissions on GitHub"
	case BranchFailureBaseMissing:
		return "the session branch could not be created: the base branch does not exist"
	case BranchFailureProtected:
		return "the session branch could not be created: a branch protection or ruleset " +
			"governs its name"
	case BranchFailureConflict:
		return "the session branch could not be created: a branch with its name already " +
			"points at other work"
	case BranchFailureInvalidName:
		return "the session branch could not be created: its name is not a valid branch name"
	case BranchFailureRefused:
		return "the session branch could not be created: GitHub refused the request — check the " +
			"App's permissions and the repository's rules"
	case BranchFailureUnrecorded:
		return "the session branch was created on GitHub but could not be recorded here; " +
			"its commit is in the workflow history"
	case BranchFailureUnreachable:
		return "the session branch could not be created: GitHub could not be reached " +
			"after repeated attempts"
	default:
		return "the session branch could not be created"
	}
}
