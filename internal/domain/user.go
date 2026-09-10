package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrInvalidUser is returned when a user fails its invariants.
var ErrInvalidUser = errors.New("invalid user")

// User is a person who can sign in to Weave.
//
// ExternalID is the identity-provider subject. It is stored so a returning
// sign-in resolves to the same row, but it is never an identifier the rest of
// the product uses: only ID travels through the domain, the API and the
// database's foreign keys. That keeps the identity provider replaceable and
// stops an external system's identifier leaking into product surfaces.
type User struct {
	ID          uuid.UUID
	ExternalID  string
	Email       string
	DisplayName string
	AvatarURL   string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// NewUserID returns an identifier for a new user.
//
// UUIDv7 is time-ordered, which keeps primary-key inserts append-friendly
// instead of scattering them through the index the way UUIDv4 does.
func NewUserID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate user id: %w", err)
	}
	return id, nil
}

// Validate reports whether the user satisfies its invariants.
func (u User) Validate() error {
	if u.ID == uuid.Nil {
		return fmt.Errorf("%w: id is required", ErrInvalidUser)
	}
	if strings.TrimSpace(u.ExternalID) == "" {
		return fmt.Errorf("%w: external id is required", ErrInvalidUser)
	}
	if strings.TrimSpace(u.Email) == "" {
		return fmt.Errorf("%w: email is required", ErrInvalidUser)
	}
	return nil
}
