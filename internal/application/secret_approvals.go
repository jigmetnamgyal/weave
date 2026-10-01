package application

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// ErrSecretApprovalUnavailable hides storage/driver diagnostics and evidence.
var ErrSecretApprovalUnavailable = errors.New("secret approval authority unavailable")

// SecretApprovalReader is the protected authority boundary for future metadata
// verification. Each read must check exact scope and current withdrawal/deletion
// state; there is no lookup-by-ID alone, payload capability or production adapter.
// Missing, foreign, unknown or withdrawn approvals return ErrCredentialReferenceRejected.
// Implementations must honor cancellation and never return raw diagnostics.
type SecretApprovalReader interface {
	LookupApproval(context.Context, domain.SecretApprovalScope) (domain.ProviderSecretApproval, error)
	LookupSelectedApproval(context.Context, uuid.UUID, domain.SecretApprovalScope) (domain.ProviderSecretApproval, error)
}

// SecretApprovalResolver validates authority responses without caching or turning
// shape checks into ownership proof. Callers must authenticate/authorize first;
// runtime allocation/binding authorization remains outside this unwired resolver.
type SecretApprovalResolver struct{ reader SecretApprovalReader }

// NewSecretApprovalResolver requires an explicitly supplied nonnil authority,
// including refusal of interface-typed nil implementations. It does no I/O.
func NewSecretApprovalResolver(reader SecretApprovalReader) (*SecretApprovalResolver, error) {
	if nilCredentialDependency(reader) {
		return nil, ErrCredentialConfiguration
	}
	return &SecretApprovalResolver{reader: reader}, nil
}

// Resolve obtains current evidence for an exact reference, for future registration
// verification. It performs no cloud call or verification UUID generation.
func (r *SecretApprovalResolver) Resolve(ctx context.Context, scope domain.SecretApprovalScope) (domain.ProviderSecretApproval, error) {
	return r.resolve(ctx, uuid.Nil, scope, false)
}

// ResolveSelected requires the exact immutable approval ID and scope; it cannot
// switch to another approval after withdrawal or follow a mutable current binding.
func (r *SecretApprovalResolver) ResolveSelected(ctx context.Context, id uuid.UUID, scope domain.SecretApprovalScope) (domain.ProviderSecretApproval, error) {
	return r.resolve(ctx, id, scope, true)
}

// resolve rejects malformed intent before authority I/O and normalizes failures.
func (r *SecretApprovalResolver) resolve(ctx context.Context, id uuid.UUID, scope domain.SecretApprovalScope, selected bool) (domain.ProviderSecretApproval, error) {
	zero := domain.ProviderSecretApproval{}
	if scope.Validate() != nil || (selected && id == uuid.Nil) {
		return zero, ErrCredentialReferenceRejected
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	var approval domain.ProviderSecretApproval
	var err error
	if selected {
		approval, err = r.reader.LookupSelectedApproval(ctx, id, scope)
	} else {
		approval, err = r.reader.LookupApproval(ctx, scope)
	}
	if ctx.Err() != nil {
		return zero, ctx.Err()
	}
	if err != nil {
		if errors.Is(err, ErrCredentialReferenceRejected) {
			return zero, ErrCredentialReferenceRejected
		}
		return zero, ErrSecretApprovalUnavailable
	}
	if approval.Validate() != nil || approval.Scope() != scope || (selected && approval.ID() != id) {
		return zero, ErrCredentialReferenceRejected
	}
	return approval, nil
}
