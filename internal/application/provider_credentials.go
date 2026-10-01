package application

import (
	"context"
	"errors"
	"reflect"
	"time"

	"github.com/google/uuid"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Safe configuration categories: no SDK/database diagnostics or references.
var (
	ErrCredentialConfiguration           = errors.New("provider credential configuration unavailable")
	ErrCredentialNotFound                = errors.New("provider credential binding not found")
	ErrCredentialConflict                = errors.New("provider credential epoch conflict")
	ErrCredentialReferenceRejected       = errors.New("provider credential reference rejected")
	ErrCredentialVerificationUnavailable = errors.New("provider credential verification unavailable")
	ErrCredentialStoreUnavailable        = errors.New("provider credential storage unavailable")
	ErrCredentialAlreadyDisabled         = errors.New("provider credential binding already disabled")
	ErrCredentialProviderUnsupported     = errors.New("provider credential provider unsupported")
)

// CredentialVerificationTimeout bounds cooperative metadata verification.
const CredentialVerificationTimeout = 5 * time.Second

// ProviderReferenceVerifier verifies a tenant's exact approved metadata reference;
// it never fetches key bytes. Implementations must honor cancellation. This slice
// wires no production verifier and cannot use labels/names as ownership proof.
type ProviderReferenceVerifier interface {
	Verify(context.Context, uuid.UUID, domain.Provider, domain.ProviderSecretReference) (domain.VerifiedProviderReference, error)
}

// CredentialCompletion records status-only bytes with work/audit, claimant-fenced.
type CredentialCompletion struct {
	Claim  IdempotentCompletion
	Render func(domain.ProviderCredentialBinding) (int, []byte, error)
}

// CredentialRegistration is internal approved metadata, not a transport body.
type CredentialRegistration struct {
	BindingID     uuid.UUID
	ResourceID    uuid.UUID
	VersionID     uuid.UUID
	WorkspaceID   uuid.UUID
	Provider      domain.Provider
	ExpectedEpoch int64
	Verified      domain.VerifiedProviderReference
}

// ProviderCredentialRepository serializes mutations with membership changes,
// rechecks permission/epoch and commits version/pointer/audit/completion atomically.
type ProviderCredentialRepository interface {
	Authorize(context.Context, uuid.UUID, Actor) error
	Get(context.Context, uuid.UUID, domain.Provider, Actor) (domain.ProviderCredentialBinding, error)
	Register(context.Context, CredentialRegistration, Actor, *CredentialCompletion) (domain.ProviderCredentialBinding, error)
	Disable(context.Context, uuid.UUID, domain.Provider, int64, Actor, *CredentialCompletion) (domain.ProviderCredentialBinding, error)
}

// ProviderCredentialService manages metadata only. There is no secret access,
// backend delivery, public route or runtime/cleanup authorization in this slice.
type ProviderCredentialService struct {
	store       ProviderCredentialRepository
	verifier    ProviderReferenceVerifier
	environment string
	project     string
}

// NewProviderCredentialService requires explicit trusted verification/config.
// Neither construction nor any operation discovers ambient/local credentials.
func NewProviderCredentialService(store ProviderCredentialRepository, verifier ProviderReferenceVerifier, environment, project string) (*ProviderCredentialService, error) {
	if nilCredentialDependency(store) || nilCredentialDependency(verifier) {
		return nil, ErrCredentialConfiguration
	}
	if err := (domain.ProviderSecretReference{Environment: environment, ProjectNumber: project, SecretID: uuid.UUID{15: 1}, Version: 1}).Validate(); err != nil {
		return nil, ErrCredentialConfiguration
	}
	return &ProviderCredentialService{store: store, verifier: verifier, environment: environment, project: project}, nil
}

// Interface equality alone misses nil concrete pointers (and other nilable
// implementations). Refuse them before a service can reach a method call.
func nilCredentialDependency(dependency any) bool {
	if dependency == nil {
		return true
	}
	value := reflect.ValueOf(dependency)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return value.IsNil()
	default:
		return false
	}
}

// credentialActor pins the caller to workspace-management authority at commit.
func credentialActor(m domain.Membership) Actor {
	return Actor{UserID: m.UserID, Required: domain.PermissionWorkspaceManage}
}

// credentialPermission rejects missing identity/scope before checking the role.
func credentialPermission(m domain.Membership) error {
	if m.UserID == uuid.Nil || m.WorkspaceID == uuid.Nil {
		return ErrPermissionDenied
	}
	return require(m, domain.PermissionWorkspaceManage)
}

// Authorize rechecks current authority before a caller replays a stored response.
func (s *ProviderCredentialService) Authorize(ctx context.Context, m domain.Membership) error {
	if err := credentialPermission(m); err != nil {
		return err
	}
	return s.store.Authorize(ctx, m.WorkspaceID, credentialActor(m))
}

// Get returns authorized status only; missing and foreign bindings are indistinguishable.
func (s *ProviderCredentialService) Get(ctx context.Context, m domain.Membership, provider domain.Provider) (domain.ProviderCredentialBinding, error) {
	if err := credentialPermission(m); err != nil {
		return domain.ProviderCredentialBinding{}, err
	}
	if provider != domain.ProviderClaudeCode {
		return domain.ProviderCredentialBinding{}, ErrCredentialProviderUnsupported
	}
	return s.store.Get(ctx, m.WorkspaceID, provider, credentialActor(m))
}

// Register verifies metadata outside the transaction, then atomically registers
// or rotates it at the expected epoch. Zero means create; no automatic retry
// against a newer epoch/reference. Context and verifier failures never commit.
func (s *ProviderCredentialService) Register(ctx context.Context, m domain.Membership, provider domain.Provider, expected int64, ref domain.ProviderSecretReference, c *CredentialCompletion) (domain.ProviderCredentialBinding, error) {
	zero := domain.ProviderCredentialBinding{}
	if err := credentialPermission(m); err != nil {
		return zero, err
	}
	if provider != domain.ProviderClaudeCode {
		return zero, ErrCredentialProviderUnsupported
	}
	if err := domain.ValidateCredentialEpoch(expected); err != nil {
		return zero, err
	}
	if err := ref.Validate(); err != nil {
		return zero, err
	}
	if ref.Environment != s.environment || ref.ProjectNumber != s.project {
		return zero, ErrCredentialReferenceRejected
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	if err := s.store.Authorize(ctx, m.WorkspaceID, credentialActor(m)); err != nil {
		return zero, err
	}
	// Reject stale intent before external I/O as well as at commit.
	current, err := s.store.Get(ctx, m.WorkspaceID, provider, credentialActor(m))
	if errors.Is(err, ErrCredentialNotFound) {
		if expected != 0 {
			return zero, ErrCredentialConflict
		}
	} else if err != nil {
		return zero, err
	} else if current.Epoch != expected {
		return zero, ErrCredentialConflict
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	vctx, cancel := context.WithTimeout(ctx, CredentialVerificationTimeout)
	verified, verr := s.verifier.Verify(vctx, m.WorkspaceID, provider, ref)
	deadlineErr := vctx.Err()
	cancel()
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	if deadlineErr != nil {
		return zero, ErrCredentialVerificationUnavailable
	}
	if verr != nil {
		if errors.Is(verr, ErrCredentialReferenceRejected) {
			return zero, ErrCredentialReferenceRejected
		}
		return zero, ErrCredentialVerificationUnavailable
	}
	if verified.WorkspaceID != m.WorkspaceID || verified.Provider != provider || verified.Reference != ref || verified.VerificationID == uuid.Nil {
		return zero, ErrCredentialReferenceRejected
	}
	binding, err := uuid.NewV7()
	if err != nil {
		return zero, ErrCredentialStoreUnavailable
	}
	resource, err := uuid.NewV7()
	if err != nil {
		return zero, ErrCredentialStoreUnavailable
	}
	version, err := uuid.NewV7()
	if err != nil {
		return zero, ErrCredentialStoreUnavailable
	}
	return s.store.Register(ctx, CredentialRegistration{BindingID: binding, ResourceID: resource, VersionID: version, WorkspaceID: m.WorkspaceID, Provider: provider, ExpectedEpoch: expected, Verified: verified}, credentialActor(m), c)
}

// Disable fences binding metadata only, not already-delivered keys or runners.
// Runtime cleanup must be implemented before real adapter activation.
func (s *ProviderCredentialService) Disable(ctx context.Context, m domain.Membership, provider domain.Provider, expected int64, c *CredentialCompletion) (domain.ProviderCredentialBinding, error) {
	if err := credentialPermission(m); err != nil {
		return domain.ProviderCredentialBinding{}, err
	}
	if provider != domain.ProviderClaudeCode {
		return domain.ProviderCredentialBinding{}, ErrCredentialProviderUnsupported
	}
	if err := domain.ValidateCredentialEpoch(expected); err != nil {
		return domain.ProviderCredentialBinding{}, err
	}
	return s.store.Disable(ctx, m.WorkspaceID, provider, expected, credentialActor(m), c)
}
