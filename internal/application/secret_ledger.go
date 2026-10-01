package application

import (
	"context"

	"github.com/google/uuid"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// SecretReservationIntent binds a stable onboarding decision to exact initial
// scope. Retries must retain its ID and parameters; there is no cloud side effect.
type SecretReservationIntent struct {
	ID    uuid.UUID
	Scope domain.SecretApprovalScope
}

// SecretApprovalLedgerWriter is a privileged onboarding capability, not a user
// service or SystemActor privilege. Stable IDs fence exact command retries; actor
// attribution comes from explicit trusted writer configuration, not request JSON.
// No implementation is composed into production by this metadata-only slice.
type SecretApprovalLedgerWriter interface {
	ReserveIntent(context.Context, SecretReservationIntent) error
	AssignResource(context.Context, uuid.UUID, uuid.UUID, domain.SecretCreationTime) error
	PublishApproval(context.Context, uuid.UUID, domain.ProviderSecretApproval) error
	Withdraw(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, *uuid.UUID) error
}
