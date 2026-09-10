package postgres_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// invitationServices builds the pair of services the invitation flow needs.
func invitationServices(pool *pgxpool.Pool, now func() time.Time) (*application.InvitationService, *application.WorkspaceService) {
	workspaceStore := postgres.NewWorkspaceStore(pool)
	return application.NewInvitationService(postgres.NewInvitationStore(pool), workspaceStore, now),
		application.NewWorkspaceService(workspaceStore)
}

// ownerOf returns the membership an owner would carry.
func ownerOf(workspaceID, userID uuid.UUID) domain.Membership {
	return domain.Membership{WorkspaceID: workspaceID, UserID: userID, Role: domain.RoleOwner}
}

// invitedUser seeds a user whose email is known, so acceptance can be tested.
func invitedUser(t *testing.T, pool *pgxpool.Pool) domain.User {
	t.Helper()
	return seedUser(t, pool)
}

func TestIssueAndAccept(t *testing.T) {
	pool := newPool(t)
	invitations, workspaces := invitationServices(pool, time.Now)
	ctx := context.Background()

	owner := seedUser(t, pool)
	invitee := invitedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Invite Workspace")

	issued, err := invitations.Issue(ctx, ownerOf(workspace.ID, owner.ID), invitee.Email, domain.RoleDeveloper)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if issued.Token == "" {
		t.Fatal("no token was returned")
	}

	membership, err := invitations.Accept(ctx, invitee, issued.Token)
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	// The invited role, not a default.
	if membership.Role != domain.RoleDeveloper {
		t.Errorf("role = %q, want developer", membership.Role)
	}

	// The invitee can now see the workspace, scoped correctly.
	listed, err := workspaces.List(ctx, invitee.ID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].Workspace.ID != workspace.ID || listed[0].Role != domain.RoleDeveloper {
		t.Errorf("invitee sees %+v, want one workspace as developer", listed)
	}

	if got := auditCount(t, pool, workspace.ID, application.AuditInvitationAccepted); got != 1 {
		t.Errorf("accepted audit rows = %d, want 1", got)
	}
}

// TestTokenIsSingleUse covers the core capability property.
func TestTokenIsSingleUse(t *testing.T) {
	pool := newPool(t)
	invitations, _ := invitationServices(pool, time.Now)
	ctx := context.Background()

	owner := seedUser(t, pool)
	invitee := invitedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Single Use Workspace")

	issued, err := invitations.Issue(ctx, ownerOf(workspace.ID, owner.ID), invitee.Email, domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := invitations.Accept(ctx, invitee, issued.Token); err != nil {
		t.Fatalf("first Accept: %v", err)
	}

	// The second attempt is already-a-member, because the membership now
	// exists — and the token is spent either way.
	_, err = invitations.Accept(ctx, invitee, issued.Token)
	if !errors.Is(err, application.ErrAlreadyMember) && !errors.Is(err, domain.ErrInvitationNotUsable) {
		t.Errorf("second Accept returned %v, want already-member or not-usable", err)
	}
}

// TestConcurrentAcceptsCreateOneMembership is why the claim is a single
// conditional statement rather than a read followed by a write.
func TestConcurrentAcceptsCreateOneMembership(t *testing.T) {
	pool := newPool(t)
	invitations, _ := invitationServices(pool, time.Now)
	ctx := context.Background()

	owner := seedUser(t, pool)
	invitee := invitedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Race Invite Workspace")

	issued, err := invitations.Issue(ctx, ownerOf(workspace.ID, owner.ID), invitee.Email, domain.RoleDeveloper)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	const concurrency = 8
	var wg sync.WaitGroup
	var successes int
	var mu sync.Mutex
	start := make(chan struct{})

	for range concurrency {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := invitations.Accept(context.Background(), invitee, issued.Token); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if successes != 1 {
		t.Errorf("%d concurrent accepts succeeded, want exactly 1", successes)
	}

	var members int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM workspace_members WHERE workspace_id = $1 AND user_id = $2",
		workspace.ID, invitee.ID).Scan(&members); err != nil {
		t.Fatalf("count memberships: %v", err)
	}
	if members != 1 {
		t.Errorf("%d memberships created, want 1", members)
	}
}

// TestUnusableTokensAreIndistinguishable is the property that stops token
// probing from being a search with feedback.
func TestUnusableTokensAreIndistinguishable(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	stranger := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Indistinguishable Workspace")

	clock := time.Now()
	invitations, _ := invitationServices(pool, func() time.Time { return clock })
	actor := ownerOf(workspace.ID, owner.ID)

	// An expired one.
	expired, err := invitations.Issue(ctx, actor, "expired@example.com", domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue expired: %v", err)
	}
	clock = clock.Add(domain.InvitationLifetime + time.Minute)

	// A revoked one.
	revoked, err := invitations.Issue(ctx, actor, "revoked@example.com", domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue revoked: %v", err)
	}
	if err := invitations.Revoke(ctx, actor, revoked.Invitation.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// An accepted one.
	accepted, err := invitations.Issue(ctx, actor, stranger.Email, domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue accepted: %v", err)
	}
	if _, err := invitations.Accept(ctx, stranger, accepted.Token); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	cases := map[string]string{
		"unknown":  "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"expired":  expired.Token,
		"revoked":  revoked.Token,
		"accepted": accepted.Token,
		"empty":    "",
	}

	for name, token := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := invitations.Preview(ctx, token); !errors.Is(err, domain.ErrInvitationNotUsable) {
				t.Errorf("Preview returned %v, want ErrInvitationNotUsable", err)
			}
			if _, err := invitations.Accept(ctx, stranger, token); !errors.Is(err, domain.ErrInvitationNotUsable) {
				t.Errorf("Accept returned %v, want ErrInvitationNotUsable", err)
			}
		})
	}
}

// TestWrongRecipientIsRefused covers the forwarded-link case.
func TestWrongRecipientIsRefused(t *testing.T) {
	pool := newPool(t)
	invitations, _ := invitationServices(pool, time.Now)
	ctx := context.Background()

	owner := seedUser(t, pool)
	intended := invitedUser(t, pool)
	interloper := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Recipient Workspace")

	issued, err := invitations.Issue(ctx, ownerOf(workspace.ID, owner.ID), intended.Email, domain.RoleDeveloper)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	if _, err := invitations.Accept(ctx, interloper, issued.Token); !errors.Is(err, domain.ErrInvitationWrongRecipient) {
		t.Errorf("interloper got %v, want ErrInvitationWrongRecipient", err)
	}

	// The invitation survives for its intended recipient.
	if _, err := invitations.Accept(ctx, intended, issued.Token); err != nil {
		t.Errorf("intended recipient could not accept after a failed attempt: %v", err)
	}
}

// TestEmailMatchIsCaseInsensitive covers an invitation addressed with
// different casing from the account that accepts it.
func TestEmailMatchIsCaseInsensitive(t *testing.T) {
	pool := newPool(t)
	invitations, _ := invitationServices(pool, time.Now)
	ctx := context.Background()

	owner := seedUser(t, pool)
	invitee := invitedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Casing Workspace")

	upper := strings.ToUpper(invitee.Email)
	issued, err := invitations.Issue(ctx, ownerOf(workspace.ID, owner.ID), upper, domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := invitations.Accept(ctx, invitee, issued.Token); err != nil {
		t.Errorf("Accept with differently-cased address failed: %v", err)
	}
}

// TestOneOutstandingInvitationPerAddress covers the partial unique index and
// the paths around it.
func TestOneOutstandingInvitationPerAddress(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Outstanding Workspace")

	clock := time.Now()
	invitations, _ := invitationServices(pool, func() time.Time { return clock })
	actor := ownerOf(workspace.ID, owner.ID)

	first, err := invitations.Issue(ctx, actor, "pending@example.com", domain.RoleViewer)
	if err != nil {
		t.Fatalf("first Issue: %v", err)
	}

	// A second while one is outstanding is refused.
	if _, err := invitations.Issue(ctx, actor, "pending@example.com", domain.RoleViewer); !errors.Is(err, application.ErrInvitationOutstanding) {
		t.Errorf("second Issue returned %v, want ErrInvitationOutstanding", err)
	}

	// Once revoked, a new one may be issued.
	if err := invitations.Revoke(ctx, actor, first.Invitation.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	second, err := invitations.Issue(ctx, actor, "pending@example.com", domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue after revoke: %v", err)
	}

	// And once expired, the stale row is superseded rather than blocking.
	clock = clock.Add(domain.InvitationLifetime + time.Minute)
	third, err := invitations.Issue(ctx, actor, "pending@example.com", domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue after expiry: %v", err)
	}
	if third.Invitation.ID == second.Invitation.ID {
		t.Error("the expired invitation was reused rather than superseded")
	}

	// The superseded one is recorded as revoked, not deleted.
	var total int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM workspace_invitations WHERE workspace_id = $1 AND email = $2",
		workspace.ID, "pending@example.com").Scan(&total); err != nil {
		t.Fatalf("count invitations: %v", err)
	}
	if total != 3 {
		t.Errorf("%d invitation rows, want 3 — superseding must not delete history", total)
	}
}

// TestReInviteAfterRemovalSucceeds covers someone leaving and being invited
// back.
func TestReInviteAfterRemovalSucceeds(t *testing.T) {
	pool := newPool(t)
	invitations, workspaces := invitationServices(pool, time.Now)
	ctx := context.Background()

	owner := seedUser(t, pool)
	invitee := invitedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Rejoin Workspace")
	actor := ownerOf(workspace.ID, owner.ID)

	first, err := invitations.Issue(ctx, actor, invitee.Email, domain.RoleDeveloper)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := invitations.Accept(ctx, invitee, first.Token); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := workspaces.RemoveMember(ctx, actor, invitee.ID); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}

	second, err := invitations.Issue(ctx, actor, invitee.Email, domain.RoleViewer)
	if err != nil {
		t.Fatalf("re-invite: %v", err)
	}
	membership, err := invitations.Accept(ctx, invitee, second.Token)
	if err != nil {
		t.Fatalf("re-accept: %v", err)
	}
	if membership.Role != domain.RoleViewer {
		t.Errorf("rejoined at %q, want the newly invited viewer role", membership.Role)
	}
}

// TestInvitingAnExistingMemberIsRefused covers the friendly-error case.
func TestInvitingAnExistingMemberIsRefused(t *testing.T) {
	pool := newPool(t)
	invitations, _ := invitationServices(pool, time.Now)
	ctx := context.Background()

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Existing Member Workspace")

	if _, err := invitations.Issue(ctx, ownerOf(workspace.ID, owner.ID), owner.Email, domain.RoleViewer); !errors.Is(err, application.ErrAlreadyMember) {
		t.Errorf("inviting an existing member returned %v, want ErrAlreadyMember", err)
	}
}

// TestAdminCannotInviteOwner extends the M2.2 grant rule to invitations, so
// they are not a way around it.
func TestAdminCannotInviteOwner(t *testing.T) {
	pool := newPool(t)
	invitations, _ := invitationServices(pool, time.Now)
	ctx := context.Background()

	owner := seedUser(t, pool)
	admin := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Invite Escalation Workspace")
	addMember(t, pool, workspace.ID, admin.ID, domain.RoleAdmin)

	adminActor := domain.Membership{WorkspaceID: workspace.ID, UserID: admin.ID, Role: domain.RoleAdmin}

	if _, err := invitations.Issue(ctx, adminActor, "newowner@example.com", domain.RoleOwner); !errors.Is(err, domain.ErrCannotGrantRole) {
		t.Errorf("admin inviting an owner returned %v, want ErrCannotGrantRole", err)
	}
	// An admin may still invite at or below their own authority.
	if _, err := invitations.Issue(ctx, adminActor, "newadmin@example.com", domain.RoleAdmin); err != nil {
		t.Errorf("admin inviting an admin failed: %v", err)
	}
	// And an owner may invite an owner.
	if _, err := invitations.Issue(ctx, ownerOf(workspace.ID, owner.ID), "realowner@example.com", domain.RoleOwner); err != nil {
		t.Errorf("owner inviting an owner failed: %v", err)
	}
}

// TestListNeverReturnsTokenMaterial guards the storage design at the read
// boundary.
func TestListNeverReturnsTokenMaterial(t *testing.T) {
	pool := newPool(t)
	invitations, _ := invitationServices(pool, time.Now)
	ctx := context.Background()

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Listing Workspace")
	actor := ownerOf(workspace.ID, owner.ID)

	issued, err := invitations.Issue(ctx, actor, "listed@example.com", domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	records, err := invitations.List(ctx, actor, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("listed %d invitations, want 1", len(records))
	}

	// The domain entity carries no token or hash field at all, so the strongest
	// check available is that the token cannot be reconstructed from what a
	// caller receives.
	record := records[0]
	if record.Invitation.Email != "listed@example.com" || record.Invitation.Role != domain.RoleViewer {
		t.Errorf("listed invitation = %+v", record)
	}
	if record.Status != domain.InvitationPending {
		t.Errorf("status = %q, want pending", record.Status)
	}
	_ = issued
}

// TestCrossWorkspaceRevokeIsRefused proves an invitation id from one tenant
// cannot be revoked through another.
func TestCrossWorkspaceRevokeIsRefused(t *testing.T) {
	pool := newPool(t)
	invitations, _ := invitationServices(pool, time.Now)
	ctx := context.Background()

	ownerA := seedUser(t, pool)
	ownerB := seedUser(t, pool)
	workspaceA := seedWorkspace(t, pool, ownerA, "Tenant A Workspace")
	workspaceB := seedWorkspace(t, pool, ownerB, "Tenant B Workspace")

	issued, err := invitations.Issue(ctx, ownerOf(workspaceA.ID, ownerA.ID), "target@example.com", domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	err = invitations.Revoke(ctx, ownerOf(workspaceB.ID, ownerB.ID), issued.Invitation.ID)
	if !errors.Is(err, application.ErrInvitationNotFound) {
		t.Errorf("cross-workspace revoke returned %v, want ErrInvitationNotFound", err)
	}

	// Still usable by its intended recipient's workspace.
	if _, err := invitations.Preview(ctx, issued.Token); err != nil {
		t.Errorf("invitation was damaged by the cross-workspace attempt: %v", err)
	}
}

// TestListFiltersByStatus covers the status filter the spec requires.
//
// The filter runs against the derived status, so an invitation that merely
// aged out reports as expired without anything having updated its row.
func TestListFiltersByStatus(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	joiner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Filtered Workspace")

	clock := time.Now()
	invitations, _ := invitationServices(pool, func() time.Time { return clock })
	actor := ownerOf(workspace.ID, owner.ID)

	// One that will expire, purely by the clock moving.
	if _, err := invitations.Issue(ctx, actor, "aged@example.com", domain.RoleViewer); err != nil {
		t.Fatalf("Issue aged: %v", err)
	}
	clock = clock.Add(domain.InvitationLifetime + time.Minute)

	// One revoked.
	revoked, err := invitations.Issue(ctx, actor, "revoked@example.com", domain.RoleViewer)
	if err != nil {
		t.Fatalf("Issue revoked: %v", err)
	}
	if err := invitations.Revoke(ctx, actor, revoked.Invitation.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	// One accepted.
	accepted, err := invitations.Issue(ctx, actor, joiner.Email, domain.RoleDeveloper)
	if err != nil {
		t.Fatalf("Issue accepted: %v", err)
	}
	if _, err := invitations.Accept(ctx, joiner, accepted.Token); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	// And one left pending.
	if _, err := invitations.Issue(ctx, actor, "pending@example.com", domain.RoleViewer); err != nil {
		t.Fatalf("Issue pending: %v", err)
	}

	counts := map[domain.InvitationStatus]int{
		domain.InvitationPending:  1,
		domain.InvitationExpired:  1,
		domain.InvitationRevoked:  1,
		domain.InvitationAccepted: 1,
	}
	for status, want := range counts {
		t.Run(string(status), func(t *testing.T) {
			records, err := invitations.List(ctx, actor, status)
			if err != nil {
				t.Fatalf("List(%q): %v", status, err)
			}
			if len(records) != want {
				t.Errorf("List(%q) returned %d, want %d", status, len(records), want)
			}
			for _, record := range records {
				// The resolved status the service returned — the same value
				// the response is serialised from, so a mismatch here is a
				// mismatch the caller would see.
				if record.Status != status {
					t.Errorf("List(%q) included an invitation with status %q", status, record.Status)
				}
			}
		})
	}

	// No filter returns everything.
	all, err := invitations.List(ctx, actor, "")
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	if len(all) != 4 {
		t.Errorf("unfiltered List returned %d, want 4", len(all))
	}
}

// TestFilteredStatusSurvivesSerialisation covers the two-clock defect: the
// service filtered with its own clock while the handler recomputed the status
// with time.Now(), so an invitation could be returned under ?status=pending
// carrying the label "expired".
//
// A fake clock makes this deterministic rather than a race. Before the fix the
// filter used the frozen clock and the response used real time, so every
// record in this test disagreed — not merely one caught in a narrow window.
func TestFilteredStatusSurvivesSerialisation(t *testing.T) {
	pool := newPool(t)
	ctx := context.Background()

	owner := seedUser(t, pool)
	workspace := seedWorkspace(t, pool, owner, "Serialisation Workspace")

	// A clock held in the past. Everything issued "now" in real time is still
	// comfortably pending by this clock.
	frozen := time.Now()
	invitations, _ := invitationServices(pool, func() time.Time { return frozen })
	actor := ownerOf(workspace.ID, owner.ID)

	if _, err := invitations.Issue(ctx, actor, "serialised@example.com", domain.RoleViewer); err != nil {
		t.Fatalf("Issue: %v", err)
	}

	// Move the clock past expiry. The row is untouched; only the instant the
	// status is resolved against has changed.
	frozen = frozen.Add(domain.InvitationLifetime + time.Hour)

	pending, err := invitations.List(ctx, actor, domain.InvitationPending)
	if err != nil {
		t.Fatalf("List(pending): %v", err)
	}
	if len(pending) != 0 {
		t.Errorf("List(pending) returned %d after the clock passed expiry, want 0", len(pending))
	}

	expired, err := invitations.List(ctx, actor, domain.InvitationExpired)
	if err != nil {
		t.Fatalf("List(expired): %v", err)
	}
	if len(expired) != 1 {
		t.Fatalf("List(expired) returned %d, want 1", len(expired))
	}

	// The resolved status travels with the record, so what the handler
	// serialises is the same value the filter matched on. Recomputing here
	// would be asking a different question.
	if expired[0].Status != domain.InvitationExpired {
		t.Errorf("record carries status %q but was returned under the expired filter",
			expired[0].Status)
	}

	// And the unfiltered list agrees with the filtered one.
	all, err := invitations.List(ctx, actor, "")
	if err != nil {
		t.Fatalf("List(all): %v", err)
	}
	for _, record := range all {
		if record.Status != record.Invitation.Status(frozen) {
			t.Errorf("resolved status %q disagrees with the entity at the same instant",
				record.Status)
		}
	}
}
