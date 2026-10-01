package domain

import (
	"errors"
	"fmt"
	"io"

	"github.com/google/uuid"
)

// ErrInvalidSecretApproval is a fixed category that never includes evidence.
var ErrInvalidSecretApproval = errors.New("invalid secret approval metadata")

// SecretCreationTime preserves a post-Unix-epoch creation observation without
// timezone or microsecond rounding. It is metadata, not cloud authenticity proof.
type SecretCreationTime struct {
	seconds int64
	nanos   int32
}

// NewSecretCreationTime refuses absent/noncanonical observations. The upper
// bound is year 9999; actual vendor semantics must be established separately.
func NewSecretCreationTime(seconds int64, nanos int32) (SecretCreationTime, error) {
	value := SecretCreationTime{seconds: seconds, nanos: nanos}
	if err := value.Validate(); err != nil {
		return SecretCreationTime{}, err
	}
	return value, nil
}

// Validate rejects zero/missing, overflow and noncanonical nanosecond tuples.
func (t SecretCreationTime) Validate() error {
	if t.seconds <= 0 || t.seconds > 253402300799 || t.nanos < 0 || t.nanos > 999999999 {
		return ErrInvalidSecretApproval
	}
	return nil
}

// Tuple gives trusted adapters the exact stored observation, never rounded time.
func (t SecretCreationTime) Tuple() (int64, int32) { return t.seconds, t.nanos }

// Before compares exact observations; validate both values before using it.
func (t SecretCreationTime) Before(other SecretCreationTime) bool {
	return t.seconds < other.seconds || (t.seconds == other.seconds && t.nanos < other.nanos)
}

// Format prevents accidental debug disclosure of creation observations.
func (SecretCreationTime) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "[restricted creation observation]")
}

// MarshalJSON refuses putting restricted evidence into public/durable payloads.
func (SecretCreationTime) MarshalJSON() ([]byte, error) { return nil, ErrRestrictedCredentialMetadata }

// SecretApprovalScope is exact tenant/provider/reference input from trusted
// records. Shape validation is not principal authorization or ownership proof.
type SecretApprovalScope struct {
	WorkspaceID uuid.UUID
	Provider    Provider
	Reference   ProviderSecretReference
}

// Validate refuses missing scope, unsupported providers and malformed references.
func (s SecretApprovalScope) Validate() error {
	if s.WorkspaceID == uuid.Nil || s.Provider != ProviderClaudeCode {
		return ErrInvalidSecretApproval
	}
	if s.Reference.Validate() != nil {
		return ErrInvalidSecretApproval
	}
	return nil
}

// Format prevents a scope's restricted reference from entering debug output.
func (SecretApprovalScope) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "[restricted approval scope]")
}

// MarshalJSON refuses accidental durable/public serialization of lookup scope.
func (SecretApprovalScope) MarshalJSON() ([]byte, error) { return nil, ErrRestrictedCredentialMetadata }

// SecretApprovalRecord is immutable-by-convention input for trusted storage
// mapping. It has no key bytes or requester-supplied ownership attestation.
type SecretApprovalRecord struct {
	ID             uuid.UUID
	SchemaVersion  int
	Scope          SecretApprovalScope
	ResourceFamily string
	CredentialKind string
	SecretCreated  SecretCreationTime
	VersionCreated SecretCreationTime
}

// Format masks all evidence when a mapper's record is debug-formatted.
func (SecretApprovalRecord) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "[restricted approval record]")
}

// MarshalJSON prevents records from becoming API, activity or broker payloads.
func (SecretApprovalRecord) MarshalJSON() ([]byte, error) {
	return nil, ErrRestrictedCredentialMetadata
}

// ProviderSecretApproval is a read-only scoped evidence value. A valid value
// alone grants no authority: only a trusted ledger can establish current approval.
type ProviderSecretApproval struct{ record SecretApprovalRecord }

// NewProviderSecretApproval checks closed schema/family/kind and chronological
// consistency. It neither verifies cloud metadata nor proves approval provenance.
func NewProviderSecretApproval(record SecretApprovalRecord) (ProviderSecretApproval, error) {
	approval := ProviderSecretApproval{record: record}
	if err := approval.Validate(); err != nil {
		return ProviderSecretApproval{}, err
	}
	return approval, nil
}

// Validate refuses incomplete, unsupported or internally inconsistent evidence.
func (a ProviderSecretApproval) Validate() error {
	r := a.record
	if r.ID == uuid.Nil || r.SchemaVersion != 1 || r.ResourceFamily != "gcp_global" || r.CredentialKind != "api_key" || r.Scope.Validate() != nil || r.SecretCreated.Validate() != nil || r.VersionCreated.Validate() != nil || r.VersionCreated.Before(r.SecretCreated) {
		return ErrInvalidSecretApproval
	}
	return nil
}

// ID returns a product approval identifier, not a bearer token.
func (a ProviderSecretApproval) ID() uuid.UUID { return a.record.ID }

// Scope returns exact immutable scope by value, never a mutable ledger pointer.
func (a ProviderSecretApproval) Scope() SecretApprovalScope { return a.record.Scope }

// Record returns restricted evidence by value for trusted metadata comparison.
// Callers must not log/serialize its fields individually.
func (a ProviderSecretApproval) Record() SecretApprovalRecord { return a.record }

// Format refuses debug disclosure even for Go-syntax formatting.
func (ProviderSecretApproval) Format(s fmt.State, _ rune) {
	_, _ = io.WriteString(s, "[restricted secret approval]")
}

// MarshalJSON prevents approval evidence leaving the trusted metadata boundary.
func (ProviderSecretApproval) MarshalJSON() ([]byte, error) {
	return nil, ErrRestrictedCredentialMetadata
}
