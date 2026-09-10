package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Errors the invitation use cases raise.
var (
	// ErrInvitationNotFound is returned when no invitation matches within the
	// caller's workspace.
	ErrInvitationNotFound = errors.New("invitation not found")
	// ErrInvitationOutstanding is returned when an address already has a
	// usable invitation to this workspace.
	ErrInvitationOutstanding = errors.New("an invitation to this address is already outstanding")
)

// Audit action names for invitations.
const (
	AuditInvitationCreated  = "workspace.invitation.created"
	AuditInvitationRevoked  = "workspace.invitation.revoked"
	AuditInvitationAccepted = "workspace.invitation.accepted"
)

// InvitationRecord is an invitation together with who issued it, which a
// member list needs and a second lookup would otherwise cost.
//
// Status is a resolved value rather than the embedded Invitation.Status
// method, and the distinction matters: status depends on the clock, so a
// caller that recomputed it would be answering a slightly later question than
// the one the list was filtered by. A pending invitation that expires between
// the two would come back labelled `expired` under `?status=pending`.
// Resolving it once, here, is what keeps the filter and the label the same
// answer.
type InvitationRecord struct {
	Invitation           domain.Invitation
	Status               domain.InvitationStatus
	InvitedByEmail       string
	InvitedByDisplayName string
}

// InvitationPreview is what someone holding a token may learn before
// accepting: enough to recognise the offer, and nothing else.
//
// It deliberately omits the invited address. A token can be forwarded, and
// whoever ends up holding it should not learn who it was meant for — that is
// somebody else's email address, disclosed to a party the inviter never chose.
// The recipient does not need to be told their own address either; the accept
// flow shows them which account they are signed in as instead.
type InvitationPreview struct {
	WorkspaceName        string
	Role                 domain.Role
	InvitedByEmail       string
	InvitedByDisplayName string
	ExpiresAt            time.Time
}

// InvitationStore persists invitations.
//
// As with WorkspaceStore, the methods that change state and write an audit row
// are atomic by contract; the transaction lives in the adapter.
type InvitationStore interface {
	// Create records a new invitation, re-checking the actor under the
	// workspace lock, and appends the audit event atomically.
	//
	// supersede, when set, is an expired invitation occupying the
	// one-outstanding slot; the implementation revokes it in the same
	// transaction rather than deleting it, so the record of it survives.
	Create(ctx context.Context, invitation domain.Invitation, tokenHash []byte, actor Actor, supersede uuid.UUID, event AuditEvent) (domain.Invitation, error)

	// Outstanding returns the invitation occupying the one-outstanding slot
	// for an address, or ErrInvitationNotFound. It may be expired.
	Outstanding(ctx context.Context, workspaceID uuid.UUID, email string) (domain.Invitation, error)

	// ListForWorkspace returns every invitation for a workspace, newest first.
	ListForWorkspace(ctx context.Context, workspaceID uuid.UUID) ([]InvitationRecord, error)

	// Revoke marks an outstanding invitation revoked and appends the audit
	// event atomically. Returns ErrInvitationNotFound when nothing outstanding
	// matches, which covers "wrong workspace" as well as "already terminal".
	Revoke(ctx context.Context, workspaceID, invitationID uuid.UUID, actor Actor, event AuditEvent) error

	// ByTokenHash looks an invitation up by the hash of its token. Unscoped by
	// workspace, because the acceptor is not yet a member of one.
	ByTokenHash(ctx context.Context, tokenHash []byte) (domain.Invitation, error)

	// Context returns the workspace name and inviter for a preview.
	Context(ctx context.Context, invitationID uuid.UUID) (InvitationPreview, error)

	// Accept claims the invitation and creates the membership atomically.
	//
	// The claim must be conditional on the invitation still being usable, so
	// that two concurrent accepts of one token produce exactly one membership.
	Accept(ctx context.Context, invitationID, userID uuid.UUID, role domain.Role, event AuditEvent) (domain.Membership, error)
}

// InvitationService holds the invitation use cases.
type InvitationService struct {
	invitations InvitationStore
	workspaces  WorkspaceStore
	now         func() time.Time
}

// NewInvitationService constructs the service.
//
// now is injected so expiry can be tested without sleeping.
func NewInvitationService(invitations InvitationStore, workspaces WorkspaceStore, now func() time.Time) *InvitationService {
	if now == nil {
		now = time.Now
	}
	return &InvitationService{invitations: invitations, workspaces: workspaces, now: now}
}

// IssuedInvitation is the result of issuing: the record, plus the token, which
// the caller must surface immediately because it is never recoverable.
type IssuedInvitation struct {
	Invitation domain.Invitation
	// Status resolved against the service's clock, for the same reason
	// InvitationRecord carries one.
	Status domain.InvitationStatus
	Token  string
}

// Issue creates an invitation to a workspace.
func (s *InvitationService) Issue(ctx context.Context, actor domain.Membership, email string, role domain.Role) (IssuedInvitation, error) {
	if err := require(actor, domain.PermissionMemberInvite); err != nil {
		return IssuedInvitation{}, err
	}
	if !role.Valid() {
		return IssuedInvitation{}, fmt.Errorf("%w: unknown role %q", domain.ErrInvalidInvitation, role)
	}
	// The same rule as changing a role: an admin cannot invite an owner,
	// because an admin cannot make one. Without this, invitations would be a
	// way around the escalation guard rather than a use of it.
	if !actor.Role.CanGrant(role) {
		return IssuedInvitation{}, fmt.Errorf("%w: %s cannot invite at %s",
			domain.ErrCannotGrantRole, actor.Role, role)
	}

	address, err := domain.ValidateInvitationEmail(email)
	if err != nil {
		return IssuedInvitation{}, err
	}

	// An address that already belongs here needs no invitation, and saying so
	// plainly is more useful than a generic refusal.
	members, err := s.workspaces.ListMembers(ctx, actor.WorkspaceID)
	if err != nil {
		return IssuedInvitation{}, fmt.Errorf("check existing membership: %w", err)
	}
	for _, member := range members {
		if domain.EmailsMatch(member.Email, address) {
			return IssuedInvitation{}, ErrAlreadyMember
		}
	}

	// The one-outstanding index cannot exclude expired rows, so an expired
	// invitation still holds the slot. Supersede it rather than refuse.
	var supersede uuid.UUID
	existing, err := s.invitations.Outstanding(ctx, actor.WorkspaceID, address)
	switch {
	case err == nil && existing.Usable(s.now()):
		return IssuedInvitation{}, ErrInvitationOutstanding
	case err == nil:
		supersede = existing.ID
	case !errors.Is(err, ErrInvitationNotFound):
		return IssuedInvitation{}, fmt.Errorf("check outstanding invitation: %w", err)
	}

	id, err := domain.NewInvitationID()
	if err != nil {
		return IssuedInvitation{}, err
	}
	token, err := domain.NewInvitationToken()
	if err != nil {
		return IssuedInvitation{}, err
	}

	invitation := domain.Invitation{
		ID:          id,
		WorkspaceID: actor.WorkspaceID,
		Email:       address,
		Role:        role,
		InvitedBy:   actor.UserID,
		ExpiresAt:   s.now().Add(domain.InvitationLifetime),
	}

	created, err := s.invitations.Create(ctx, invitation, token.Hash, Actor{
		UserID:   actor.UserID,
		Required: domain.PermissionMemberInvite,
	}, supersede, AuditEvent{
		WorkspaceID: actor.WorkspaceID,
		ActorUserID: actor.UserID,
		Action:      AuditInvitationCreated,
		Target:      id.String(),
		// The address and role are recorded; the token never is.
		Detail: map[string]any{"email": address, "role": role.String()},
	})
	if err != nil {
		return IssuedInvitation{}, err
	}

	return IssuedInvitation{
		Invitation: created,
		Status:     created.Status(s.now()),
		Token:      token.Plaintext,
	}, nil
}

// List returns a workspace's invitations, optionally narrowed to one status.
//
// The filter is applied here rather than in SQL because status is derived from
// timestamps, not stored. Expressing the same derivation a second time as a
// CASE expression would create exactly the disagreement that deriving it
// avoids — and an invitation list is bounded by a workspace's team, so there
// is nothing to gain from pushing it down.
func (s *InvitationService) List(ctx context.Context, actor domain.Membership, status domain.InvitationStatus) ([]InvitationRecord, error) {
	if err := require(actor, domain.PermissionMemberInvite); err != nil {
		return nil, err
	}

	records, err := s.invitations.ListForWorkspace(ctx, actor.WorkspaceID)
	if err != nil {
		return nil, fmt.Errorf("list invitations: %w", err)
	}

	// Resolve every status against one instant, before filtering. Everything
	// downstream — the filter here, the response the handler writes — reads
	// this value rather than asking the clock again.
	now := s.now()
	for i := range records {
		records[i].Status = records[i].Invitation.Status(now)
	}

	if status == "" {
		return records, nil
	}

	filtered := make([]InvitationRecord, 0, len(records))
	for _, record := range records {
		if record.Status == status {
			filtered = append(filtered, record)
		}
	}
	return filtered, nil
}

// Revoke withdraws an outstanding invitation.
func (s *InvitationService) Revoke(ctx context.Context, actor domain.Membership, invitationID uuid.UUID) error {
	if err := require(actor, domain.PermissionMemberInvite); err != nil {
		return err
	}

	return s.invitations.Revoke(ctx, actor.WorkspaceID, invitationID, Actor{
		UserID:   actor.UserID,
		Required: domain.PermissionMemberInvite,
	}, AuditEvent{
		WorkspaceID: actor.WorkspaceID,
		ActorUserID: actor.UserID,
		Action:      AuditInvitationRevoked,
		Target:      invitationID.String(),
	})
}

// Preview describes an invitation to someone holding its token.
//
// Every unusable state — unknown, expired, revoked, accepted — returns
// ErrInvitationNotUsable, so a caller probing tokens learns nothing about
// which ones ever existed.
func (s *InvitationService) Preview(ctx context.Context, token string) (InvitationPreview, error) {
	invitation, err := s.usableInvitation(ctx, token)
	if err != nil {
		return InvitationPreview{}, err
	}

	preview, err := s.invitations.Context(ctx, invitation.ID)
	if err != nil {
		return InvitationPreview{}, fmt.Errorf("load invitation context: %w", err)
	}

	preview.Role = invitation.Role
	preview.ExpiresAt = invitation.ExpiresAt
	return preview, nil
}

// Accept turns an invitation into a membership.
func (s *InvitationService) Accept(ctx context.Context, user domain.User, token string) (domain.Membership, error) {
	invitation, err := s.usableInvitation(ctx, token)
	if err != nil {
		return domain.Membership{}, err
	}

	// The token alone is not enough. A link that admits whoever opens it turns
	// a forwarded message into a workspace breach.
	if !domain.EmailsMatch(invitation.Email, user.Email) {
		return domain.Membership{}, domain.ErrInvitationWrongRecipient
	}

	// Already a member: a mistake rather than an attack, and they can already
	// see the workspace, so say so plainly.
	if _, err := s.workspaces.GetMembership(ctx, invitation.WorkspaceID, user.ID); err == nil {
		return domain.Membership{}, ErrAlreadyMember
	} else if !errors.Is(err, ErrMemberNotFound) {
		return domain.Membership{}, fmt.Errorf("check membership: %w", err)
	}

	membership, err := s.invitations.Accept(ctx, invitation.ID, user.ID, invitation.Role, AuditEvent{
		WorkspaceID: invitation.WorkspaceID,
		ActorUserID: user.ID,
		Action:      AuditInvitationAccepted,
		Target:      invitation.ID.String(),
		Detail:      map[string]any{"role": invitation.Role.String()},
	})
	if err != nil {
		return domain.Membership{}, err
	}
	return membership, nil
}

// usableInvitation resolves a token to an invitation that can still be acted
// on, collapsing every failure into one error.
func (s *InvitationService) usableInvitation(ctx context.Context, token string) (domain.Invitation, error) {
	if token == "" {
		return domain.Invitation{}, domain.ErrInvitationNotUsable
	}

	invitation, err := s.invitations.ByTokenHash(ctx, domain.HashInvitationToken(token))
	if err != nil {
		if errors.Is(err, ErrInvitationNotFound) {
			return domain.Invitation{}, domain.ErrInvitationNotUsable
		}
		return domain.Invitation{}, fmt.Errorf("look up invitation: %w", err)
	}

	if !invitation.Usable(s.now()) {
		return domain.Invitation{}, domain.ErrInvitationNotUsable
	}
	return invitation, nil
}
