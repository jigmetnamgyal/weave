package application

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Egress configuration errors are distinct from transport/DNS failures.
var (
	ErrEgressHostNotFound    = errors.New("egress host not found")
	ErrEgressHostExists      = errors.New("egress host already exists")
	ErrEgressHostLimit       = errors.New("egress host limit reached")
	ErrEgressSnapshotMissing = errors.New("runner egress snapshot missing")
)

// EgressCompletion renders the entry inside the configuration transaction.
// Its claimant is completed atomically with the mutation and audit row.
type EgressCompletion struct {
	Claim  IdempotentCompletion
	Render func(domain.EgressHost) (int, []byte, error)
}

// EgressStore must recheck the actor under the workspace lock on every operation.
// Mutations, audit and optional completion commit together or not at all.
type EgressStore interface {
	List(context.Context, uuid.UUID, Actor) ([]domain.EgressHost, error)
	Add(context.Context, domain.EgressHost, Actor, AuditEvent, *EgressCompletion) (domain.EgressHost, error)
	Remove(context.Context, uuid.UUID, uuid.UUID, Actor, AuditEvent, *EgressCompletion) error
	Authorize(context.Context, uuid.UUID, Actor) error
	Snapshot(context.Context, uuid.UUID, uuid.UUID) (domain.RunnerEgressSnapshot, error)
}

// EgressService manages configuration, not runtime network access.
type EgressService struct {
	store    EgressStore
	reserved []string
}

// NewEgressService validates deployment namespaces and reserves built-in hosts.
// No DNS lookup occurs during construction or configuration changes.
func NewEgressService(store EgressStore, reserved []string) (*EgressService, error) {
	names := append(append([]string(nil), GitHubEgressHosts...), RegistryHosts...)
	for _, raw := range reserved {
		host, err := domain.ValidateEgressHostname(raw)
		if err != nil {
			return nil, err
		}
		names = append(names, host)
	}
	return &EgressService{store: store, reserved: names}, nil
}

// Validate canonicalizes input and refuses built-in/deployment namespaces.
func (s *EgressService) Validate(raw string) (string, error) {
	host, err := domain.ValidateEgressHostname(raw)
	if err != nil {
		return "", err
	}
	if domain.EgressHostReserved(host, s.reserved) {
		return "", domain.ErrReservedEgressHost
	}
	return host, nil
}

func egressActor(m domain.Membership) Actor {
	return Actor{UserID: m.UserID, Required: domain.PermissionWorkspaceManage}
}

// Authorize rechecks current membership for completed-response replay.
func (s *EgressService) Authorize(ctx context.Context, m domain.Membership) error {
	if err := require(m, domain.PermissionWorkspaceManage); err != nil {
		return err
	}
	return s.store.Authorize(ctx, m.WorkspaceID, egressActor(m))
}

// List returns currently authorized configuration, never runtime policy.
func (s *EgressService) List(ctx context.Context, m domain.Membership) ([]domain.EgressHost, error) {
	if err := require(m, domain.PermissionWorkspaceManage); err != nil {
		return nil, err
	}
	return s.store.List(ctx, m.WorkspaceID, egressActor(m))
}

// Add records one hostname with audit and optional fenced completion.
func (s *EgressService) Add(ctx context.Context, m domain.Membership, raw string, completion *EgressCompletion) (domain.EgressHost, error) {
	if err := require(m, domain.PermissionWorkspaceManage); err != nil {
		return domain.EgressHost{}, err
	}
	host, err := s.Validate(raw)
	if err != nil {
		return domain.EgressHost{}, err
	}
	id, err := domain.NewEgressHostID()
	if err != nil {
		return domain.EgressHost{}, err
	}
	return s.store.Add(ctx, domain.EgressHost{ID: id, WorkspaceID: m.WorkspaceID, Hostname: host, CreatedBy: m.UserID}, egressActor(m), AuditEvent{WorkspaceID: m.WorkspaceID, ActorUserID: m.UserID, Action: "workspace.egress_host.added", Target: id.String(), Detail: map[string]any{"hostname": host}}, completion)
}

// Remove affects future snapshots only; audit names the stored hostname.
func (s *EgressService) Remove(ctx context.Context, m domain.Membership, id uuid.UUID, completion *EgressCompletion) error {
	if err := require(m, domain.PermissionWorkspaceManage); err != nil {
		return err
	}
	return s.store.Remove(ctx, m.WorkspaceID, id, egressActor(m), AuditEvent{WorkspaceID: m.WorkspaceID, ActorUserID: m.UserID, Action: "workspace.egress_host.removed", Target: id.String()}, completion)
}
