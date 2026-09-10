package application

import (
	"context"
	"errors"
	"fmt"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// ErrUserNotFound is returned by a UserStore when no row matches.
var ErrUserNotFound = errors.New("user not found")

// UserStore persists users. It is declared here, at the consumer, and
// implemented in internal/adapters.
type UserStore interface {
	// FindByExternalID returns the user for an identity-provider subject, or
	// ErrUserNotFound.
	FindByExternalID(ctx context.Context, externalID string) (domain.User, error)
	// UpsertByExternalID creates the user, or returns the existing row when
	// the external ID is already present.
	//
	// This must be atomic: two concurrent calls for the same external ID have
	// to resolve to one row, not two. The implementation relies on the unique
	// constraint rather than a read-then-write.
	UpsertByExternalID(ctx context.Context, user domain.User) (domain.User, error)
}

// UserProvisioner resolves a verified identity to a persisted user, creating
// the user on first sign-in.
type UserProvisioner struct {
	store UserStore
}

// NewUserProvisioner constructs a UserProvisioner.
func NewUserProvisioner(store UserStore) *UserProvisioner {
	return &UserProvisioner{store: store}
}

// FromIdentity returns the user for a verified identity, creating it if this
// is the first time the subject has been seen.
//
// The read comes first so the common case — an established user making a
// request — costs one SELECT and no write. Only a miss falls through to the
// upsert, where the unique constraint settles any race between concurrent
// first requests.
func (p *UserProvisioner) FromIdentity(ctx context.Context, identity Identity) (domain.User, error) {
	existing, err := p.store.FindByExternalID(ctx, identity.Subject)
	switch {
	case err == nil:
		return existing, nil
	case !errors.Is(err, ErrUserNotFound):
		return domain.User{}, fmt.Errorf("look up user: %w", err)
	}

	id, err := domain.NewUserID()
	if err != nil {
		return domain.User{}, err
	}

	user := domain.User{
		ID:          id,
		ExternalID:  identity.Subject,
		Email:       identity.Email,
		DisplayName: identity.DisplayName,
		AvatarURL:   identity.AvatarURL,
	}
	if err := user.Validate(); err != nil {
		return domain.User{}, err
	}

	created, err := p.store.UpsertByExternalID(ctx, user)
	if err != nil {
		return domain.User{}, fmt.Errorf("provision user: %w", err)
	}
	return created, nil
}
