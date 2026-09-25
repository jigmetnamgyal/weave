package application_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// sessionBranchWorld is a provisioning session over the branch world: a real
// InstallationService against a GitHub the test controls, and a session store
// that behaves like the real one — the SHA and its audit written together, or
// not at all, and never twice.
type sessionBranchWorld struct {
	*branchWorld
	sessions *fakeSessionBranches
	service  *application.SessionBranchService
	session  domain.Session
}

func newSessionBranchWorld(t *testing.T) *sessionBranchWorld {
	t.Helper()
	world := newBranchWorld(t)

	session := domain.Session{
		ID:           uuid.New(),
		WorkspaceID:  world.membership.WorkspaceID,
		State:        domain.SessionProvisioning,
		Version:      2,
		RepositoryID: world.repository.ID,
		BranchName:   "weave/add-rate-limiting-abcd1234",
		CreatedBy:    world.membership.UserID,
	}
	sessions := &fakeSessionBranches{session: session}

	return &sessionBranchWorld{
		branchWorld: world,
		sessions:    sessions,
		service:     application.NewSessionBranchService(sessions, world.service),
		session:     session,
	}
}

func (w *sessionBranchWorld) ensure(t *testing.T) (domain.Session, error) {
	t.Helper()
	return w.service.EnsureBranch(context.Background(), w.session.WorkspaceID, w.session.ID)
}

// TestEnsureBranchCutsAndRecordsAsTheSystem is the ordinary path.
//
// The branch is made at the base's commit, the SHA lands on the session, and
// the audit row that goes with it names nobody: the system cut this branch,
// not the member who filed the task hours before.
func TestEnsureBranchCutsAndRecordsAsTheSystem(t *testing.T) {
	world := newSessionBranchWorld(t)

	session, err := world.ensure(t)
	if err != nil {
		t.Fatalf("EnsureBranch: %v", err)
	}
	if session.BranchSHA != "basesha" {
		t.Errorf("recorded SHA = %q, want the base's commit", session.BranchSHA)
	}
	if world.api.created != 1 {
		t.Errorf("GitHub was asked to create %d refs, want 1", world.api.created)
	}

	if len(world.sessions.audits) != 1 {
		t.Fatalf("wrote %d session audit rows, want 1", len(world.sessions.audits))
	}
	audit := world.sessions.audits[0]
	if audit.ActorUserID != uuid.Nil {
		t.Errorf("the audit names %s; the system made this branch, and nobody else did", audit.ActorUserID)
	}
	if audit.Action != application.AuditBranchCreated {
		t.Errorf("audit action = %q, want %q", audit.Action, application.AuditBranchCreated)
	}
	if audit.Detail["by"] != "system" || audit.Detail["created"] != true {
		t.Errorf("audit detail = %v, want by=system and created=true", audit.Detail)
	}

	// The installation's own audit path is not used: that would be a second,
	// separately losable record of the same fact.
	if len(*world.audits) != 0 {
		t.Errorf("wrote %d installation audit rows alongside the session's, want 0", len(*world.audits))
	}
}

// TestEnsureBranchIsSafeToRedeliver composes the three idempotency layers.
//
// Each is tested on its own elsewhere; what matters here is that together they
// leave one branch, one SHA and one audit when the activity runs twice — and
// that the second run does not even ask GitHub, because the answer is already
// durable.
func TestEnsureBranchIsSafeToRedeliver(t *testing.T) {
	world := newSessionBranchWorld(t)

	first, err := world.ensure(t)
	if err != nil {
		t.Fatalf("first EnsureBranch: %v", err)
	}
	callsAfterFirst := world.api.calls

	second, err := world.ensure(t)
	if err != nil {
		t.Fatalf("redelivered EnsureBranch: %v", err)
	}

	if second.BranchSHA != first.BranchSHA {
		t.Errorf("redelivery recorded %q, the first run %q", second.BranchSHA, first.BranchSHA)
	}
	if world.api.created != 1 {
		t.Errorf("GitHub was asked to create %d refs across two deliveries, want 1", world.api.created)
	}
	if world.api.calls != callsAfterFirst {
		t.Errorf("the redelivery made %d GitHub calls, want none — the SHA was already recorded",
			world.api.calls-callsAfterFirst)
	}
	if len(world.sessions.audits) != 1 {
		t.Errorf("wrote %d audit rows across two deliveries, want 1", len(world.sessions.audits))
	}
}

// TestEnsureBranchRecordsAFoundBranchAsFound is the seam M3.3 shipped three
// regressions in: the external write succeeded and the local record did not.
//
// The retry finds the branch it made at the requested base. It records it —
// the SHA is not lost — but as *found*, because nothing proves this attempt
// created it, and M3.3's rule is that a false audit is worse than a missing
// one.
func TestEnsureBranchRecordsAFoundBranchAsFound(t *testing.T) {
	world := newSessionBranchWorld(t)
	world.sessions.recordErr = errors.New("the connection dropped before commit")

	if _, err := world.ensure(t); err == nil {
		t.Fatal("EnsureBranch reported success with the record unwritten")
	}
	if world.api.created != 1 {
		t.Fatalf("setup: the branch was not made on GitHub (created = %d)", world.api.created)
	}
	if len(world.sessions.audits) != 0 {
		t.Fatal("a failed transaction left an audit row behind")
	}

	world.sessions.recordErr = nil
	session, err := world.ensure(t)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if session.BranchSHA != "basesha" {
		t.Errorf("retry recorded %q, want the branch's commit", session.BranchSHA)
	}
	if len(world.sessions.audits) != 1 {
		t.Fatalf("wrote %d audit rows, want 1", len(world.sessions.audits))
	}
	audit := world.sessions.audits[0]
	if audit.Action != application.AuditSessionBranchFound {
		t.Errorf("audit action = %q, want %q — the retry cannot prove it created the branch",
			audit.Action, application.AuditSessionBranchFound)
	}
	if audit.Detail["created"] != false {
		t.Errorf("audit detail created = %v, want false", audit.Detail["created"])
	}
}

// TestEnsureBranchLeavesASessionThatMovedOnAlone covers a member cancelling
// between the two activities. A branch for a session that will not run is a
// ref nobody asked for.
func TestEnsureBranchLeavesASessionThatMovedOnAlone(t *testing.T) {
	world := newSessionBranchWorld(t)
	world.sessions.session.State = domain.SessionCancelling

	_, err := world.ensure(t)
	if !errors.Is(err, application.ErrSessionNotProvisioning) {
		t.Errorf("EnsureBranch = %v, want ErrSessionNotProvisioning", err)
	}
	if world.api.calls != 0 {
		t.Errorf("made %d GitHub calls for a session that is not provisioning", world.api.calls)
	}
}

// TestEnsureBranchReadsEverythingFromTheSessionRow is the ownership rule.
//
// A session id paired with the wrong workspace is not found, and nothing is
// asked of GitHub — so the workflow input cannot be used to reach a repository
// through a session that is not the workspace's.
func TestEnsureBranchReadsEverythingFromTheSessionRow(t *testing.T) {
	world := newSessionBranchWorld(t)

	_, err := world.service.EnsureBranch(context.Background(), uuid.New(), world.session.ID)
	if !errors.Is(err, application.ErrSessionNotFound) {
		t.Errorf("EnsureBranch with another workspace = %v, want ErrSessionNotFound", err)
	}
	if world.api.calls != 0 {
		t.Errorf("made %d GitHub calls for a mismatched pair", world.api.calls)
	}
}

// TestBranchRefusalsAreTerminalAndNamed checks each refusal a person has to
// fix is classified as terminal, with a reason that says which.
//
// Driven through the real service against the fake GitHub, so the errors
// classified are the ones CreateBranchAsSystem actually raises rather than
// ones constructed to match the classifier.
func TestBranchRefusalsAreTerminalAndNamed(t *testing.T) {
	cases := []struct {
		name    string
		arrange func(*sessionBranchWorld)
		want    application.BranchFailure
		mention string
	}{
		{
			name:    "the grant was withdrawn on GitHub",
			arrange: func(w *sessionBranchWorld) { w.api.withdrawn = true },
			want:    application.BranchFailureRepositoryUnavailable,
			mention: "no longer granted",
		},
		{
			name: "the installation lacks contents:write",
			arrange: func(w *sessionBranchWorld) {
				w.api.permissions["contents"] = "read"
			},
			want:    application.BranchFailureNoWriteAccess,
			mention: "contents:write",
		},
		{
			name: "the base does not exist",
			arrange: func(w *sessionBranchWorld) {
				w.sessions.session.BaseBranch = "release/gone"
			},
			want:    application.BranchFailureBaseMissing,
			mention: "base branch",
		},
		{
			name: "the target is protected",
			arrange: func(w *sessionBranchWorld) {
				w.api.branches[w.session.BranchName] = application.RemoteBranch{
					Name: w.session.BranchName, SHA: "basesha", Protected: true,
				}
			},
			want:    application.BranchFailureProtected,
			mention: "protection",
		},
		{
			name: "a ruleset governs the name",
			arrange: func(w *sessionBranchWorld) {
				w.api.restrictedNames[w.session.BranchName] = "creation"
			},
			want:    application.BranchFailureProtected,
			mention: "ruleset",
		},
		{
			name: "the name already points at other work",
			arrange: func(w *sessionBranchWorld) {
				w.api.branches[w.session.BranchName] = application.RemoteBranch{
					Name: w.session.BranchName, SHA: "unrelated",
				}
			},
			want:    application.BranchFailureConflict,
			mention: "other work",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			world := newSessionBranchWorld(t)
			tc.arrange(world)

			_, err := world.ensure(t)
			if err == nil {
				t.Fatal("EnsureBranch succeeded")
			}
			got, terminal := application.ClassifyBranchFailure(err)
			if !terminal {
				t.Fatalf("%v was classified as transient; retrying it cannot help", err)
			}
			if got != tc.want {
				t.Errorf("classified as %q, want %q", got, tc.want)
			}
			if !strings.Contains(got.Reason(), tc.mention) {
				t.Errorf("reason %q does not name the cause (%q)", got.Reason(), tc.mention)
			}
			if world.sessions.session.BranchSHA != "" {
				t.Error("a refused branch recorded a SHA")
			}
		})
	}
}

// TestTransientBranchFailuresAreRetried: a GitHub that is briefly down is not
// a reason to fail a session, so it is left for the retry policy.
func TestTransientBranchFailuresAreRetried(t *testing.T) {
	err := fmt.Errorf("create branch: %w", errors.New("github: POST /repos/acme/app/git/refs returned 502"))
	if cause, terminal := application.ClassifyBranchFailure(err); terminal {
		t.Errorf("a 502 was classified as terminal (%q); it should be retried", cause)
	}
}

// TestBranchFailureReasonsFitTheTransitionTrail: every reason must be one the
// transition validator accepts, or the failure path would itself fail.
func TestBranchFailureReasonsFitTheTransitionTrail(t *testing.T) {
	for _, failure := range []application.BranchFailure{
		application.BranchFailureRepositoryUnavailable,
		application.BranchFailureInstallationSuspended,
		application.BranchFailureNoWriteAccess,
		application.BranchFailureBaseMissing,
		application.BranchFailureProtected,
		application.BranchFailureConflict,
		application.BranchFailureInvalidName,
		application.BranchFailureUnreachable,
		application.BranchFailure("something unrecognised"),
	} {
		if _, err := domain.ValidateTransitionReason(failure.Reason()); err != nil {
			t.Errorf("%q: reason rejected by the trail: %v", failure, err)
		}
	}
}

// --- fakes -----------------------------------------------------------------

// fakeSessionBranches mirrors SessionStore.RecordBranch: the SHA and the audit
// commit together or not at all, a matching SHA is success without a write, and
// a different one is a conflict.
type fakeSessionBranches struct {
	session   domain.Session
	audits    []application.AuditEvent
	recordErr error
}

func (f *fakeSessionBranches) Get(_ context.Context, sessionID, workspaceID uuid.UUID) (domain.Session, error) {
	if sessionID != f.session.ID || workspaceID != f.session.WorkspaceID {
		return domain.Session{}, application.ErrSessionNotFound
	}
	return f.session, nil
}

func (f *fakeSessionBranches) RecordBranch(
	_ context.Context,
	sessionID, workspaceID uuid.UUID,
	sha string,
	actor application.Actor,
	audit func(domain.Session) application.AuditEvent,
) (domain.Session, bool, error) {
	if !actor.System {
		return domain.Session{}, false, errors.New("fake: the branch was recorded by something other than the system")
	}
	if sessionID != f.session.ID || workspaceID != f.session.WorkspaceID {
		return domain.Session{}, false, application.ErrSessionNotFound
	}
	if f.recordErr != nil {
		return domain.Session{}, false, f.recordErr
	}
	if f.session.BranchSHA != "" {
		if f.session.BranchSHA == sha {
			return f.session, false, nil
		}
		return domain.Session{}, false, domain.ErrBranchConflict
	}
	after := f.session
	after.BranchSHA = sha
	f.audits = append(f.audits, audit(after))
	f.session = after
	return after, true, nil
}
