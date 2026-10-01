package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"time"

	"github.com/google/uuid"
)

// Credential reference errors never echo restricted input.
var (
	ErrInvalidCredentialReference   = errors.New("invalid provider credential reference")
	ErrRestrictedCredentialMetadata = errors.New("provider credential reference cannot be serialized")
	ErrInvalidCredentialEpoch       = errors.New("invalid provider credential epoch")
)

var credentialProjectNumber = regexp.MustCompile(`^[1-9][0-9]{5,19}$`)

// ProviderSecretReference identifies metadata only, never a key value. Secret
// resource IDs are opaque UUIDs, rendered by the future GCP adapter under its
// trusted namespace. Numeric versions exclude latest/aliases. Naming is not auth.
type ProviderSecretReference struct {
	Environment   string
	ProjectNumber string
	SecretID      uuid.UUID
	Version       int64
}

// Validate bounds the canonical reference; only a trusted verifier proves ownership.
func (r ProviderSecretReference) Validate() error {
	switch r.Environment {
	case "test", "development", "staging", "production":
	default:
		return ErrInvalidCredentialReference
	}
	if !credentialProjectNumber.MatchString(r.ProjectNumber) || r.SecretID == uuid.Nil || r.Version <= 0 {
		return ErrInvalidCredentialReference
	}
	return nil
}

// Format redacts every fmt verb, including debug/Go-syntax formatting.
func (ProviderSecretReference) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "[restricted provider reference]")
}

// MarshalJSON prevents accidentally placing a reference in a response/history.
// Trusted storage adapters read the explicit fields, not this serialization.
func (ProviderSecretReference) MarshalJSON() ([]byte, error) {
	return nil, ErrRestrictedCredentialMetadata
}

var _ json.Marshaler = ProviderSecretReference{}

// ValidateCredentialEpoch reserves room for the next fenced mutation.
func ValidateCredentialEpoch(epoch int64) error {
	if epoch < 0 || epoch == math.MaxInt64 {
		return ErrInvalidCredentialEpoch
	}
	return nil
}

// ProviderCredentialBinding is safe configuration status: no resource/evidence
// fields and no key values. Active means metadata approved, not runtime enabled.
type ProviderCredentialBinding struct {
	ID               uuid.UUID
	WorkspaceID      uuid.UUID
	Provider         Provider
	State            string
	Epoch            int64
	CurrentVersionID *uuid.UUID
	CreatedBy        uuid.UUID
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// VerifiedProviderReference is a trusted verifier's scoped result, not a public
// request type or cryptographic proof. A fake is permitted only in tests.
type VerifiedProviderReference struct {
	WorkspaceID    uuid.UUID
	Provider       Provider
	Reference      ProviderSecretReference
	VerificationID uuid.UUID
}

// Format prevents recursive debug logging of restricted verification metadata.
func (VerifiedProviderReference) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "[restricted provider verification]")
}

// MarshalJSON refuses accidental durable serialization of verification metadata.
func (VerifiedProviderReference) MarshalJSON() ([]byte, error) {
	return nil, ErrRestrictedCredentialMetadata
}
