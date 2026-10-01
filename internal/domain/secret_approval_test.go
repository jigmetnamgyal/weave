package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// approvalFixture uses synthetic metadata, never an actual cloud resource/key.
func approvalFixture(t *testing.T) SecretApprovalRecord {
	t.Helper()
	created, err := NewSecretCreationTime(1700000000, 123456789)
	if err != nil {
		t.Fatal(err)
	}
	version, err := NewSecretCreationTime(1700000000, 123456790)
	if err != nil {
		t.Fatal(err)
	}
	return SecretApprovalRecord{ID: uuid.New(), SchemaVersion: 1, Scope: SecretApprovalScope{WorkspaceID: uuid.New(), Provider: ProviderClaudeCode, Reference: ProviderSecretReference{Environment: "test", ProjectNumber: "918273645", SecretID: uuid.New(), Version: 1}}, ResourceFamily: "gcp_global", CredentialKind: "api_key", SecretCreated: created, VersionCreated: version}
}

// TestSecretCreationTimePreservesPrecision proves one-nanosecond distinctions
// cannot disappear through database-style microsecond rounding or conversion.
func TestSecretCreationTimePreservesPrecision(t *testing.T) {
	for _, tuple := range []struct {
		seconds int64
		nanos   int32
	}{{1, 0}, {1700000000, 123456789}, {253402300799, 999999999}} {
		value, err := NewSecretCreationTime(tuple.seconds, tuple.nanos)
		if err != nil {
			t.Fatal(err)
		}
		seconds, nanos := value.Tuple()
		if seconds != tuple.seconds || nanos != tuple.nanos {
			t.Fatal("creation observation rounded")
		}
	}
	a, _ := NewSecretCreationTime(1700000000, 123456789)
	b, _ := NewSecretCreationTime(1700000000, 123456790)
	if a == b || !a.Before(b) || b.Before(a) || a.Before(a) {
		t.Fatal("nanosecond ordering lost")
	}
	c, _ := NewSecretCreationTime(1700000001, 0)
	if !b.Before(c) {
		t.Fatal("seconds ordering lost")
	}
	for _, tuple := range []struct {
		seconds int64
		nanos   int32
	}{{0, 0}, {-1, 0}, {253402300800, 0}, {math.MaxInt64, 0}, {1, -1}, {1, 1000000000}} {
		value, err := NewSecretCreationTime(tuple.seconds, tuple.nanos)
		if !errors.Is(err, ErrInvalidSecretApproval) || value != (SecretCreationTime{}) {
			t.Fatal("invalid creation observation accepted")
		}
	}
}

// TestSecretApprovalValidation checks closed schema/kind/scope and chronology.
func TestSecretApprovalValidation(t *testing.T) {
	record := approvalFixture(t)
	approval, err := NewProviderSecretApproval(record)
	if err != nil || approval.ID() != record.ID || approval.Scope() != record.Scope || approval.Record() != record {
		t.Fatal("valid evidence changed", err)
	}
	// Getter copies cannot retarget an existing value.
	copy := approval.Record()
	copy.Scope.Reference.Version++
	if approval.Record() != record {
		t.Fatal("mutable record alias")
	}
	equal := record
	equal.VersionCreated = equal.SecretCreated
	if _, err := NewProviderSecretApproval(equal); err != nil {
		t.Fatal("equal creation observations refused", err)
	}
	for name, mutate := range map[string]func(*SecretApprovalRecord){
		"id":               func(r *SecretApprovalRecord) { r.ID = uuid.Nil },
		"schema":           func(r *SecretApprovalRecord) { r.SchemaVersion = 2 },
		"family":           func(r *SecretApprovalRecord) { r.ResourceFamily = "SYNTHETIC_PRIVATE_MARKER" },
		"kind":             func(r *SecretApprovalRecord) { r.CredentialKind = "oauth" },
		"workspace":        func(r *SecretApprovalRecord) { r.Scope.WorkspaceID = uuid.Nil },
		"provider":         func(r *SecretApprovalRecord) { r.Scope.Provider = ProviderFake },
		"reference":        func(r *SecretApprovalRecord) { r.Scope.Reference.Version = 0 },
		"resource_created": func(r *SecretApprovalRecord) { r.SecretCreated = SecretCreationTime{} },
		"version_created":  func(r *SecretApprovalRecord) { r.VersionCreated = SecretCreationTime{} },
		"chronology":       func(r *SecretApprovalRecord) { r.SecretCreated, r.VersionCreated = r.VersionCreated, r.SecretCreated },
	} {
		t.Run(name, func(t *testing.T) {
			bad := record
			mutate(&bad)
			value, err := NewProviderSecretApproval(bad)
			if !errors.Is(err, ErrInvalidSecretApproval) || value != (ProviderSecretApproval{}) || strings.Contains(err.Error(), "SYNTHETIC_PRIVATE_MARKER") {
				t.Fatal("invalid evidence accepted/disclosed")
			}
		})
	}
	if !errors.Is((ProviderSecretApproval{}).Validate(), ErrInvalidSecretApproval) {
		t.Fatal("zero evidence accepted")
	}
}

// TestSecretApprovalEvidenceRedaction covers fmt and JSON for values/pointers
// and nested records. Explicit field access remains a trusted-code responsibility.
func TestSecretApprovalEvidenceRedaction(t *testing.T) {
	r := approvalFixture(t)
	approval, _ := NewProviderSecretApproval(r)
	for _, value := range []any{r, &r, r.Scope, &r.Scope, r.SecretCreated, &r.SecretCreated, approval, &approval, struct{ Evidence ProviderSecretApproval }{approval}} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
			got := fmt.Sprintf(verb, value)
			for _, marker := range []string{r.Scope.Reference.ProjectNumber, r.Scope.Reference.SecretID.String(), "1700000000", "123456789"} {
				if strings.Contains(got, marker) {
					t.Fatal("creation/reference evidence leaked")
				}
			}
		}
		if _, err := json.Marshal(value); !errors.Is(err, ErrRestrictedCredentialMetadata) {
			t.Fatal("evidence serialized", err)
		}
	}
}

// FuzzSecretCreationTimeRoundTrip exercises exact tuples and safe refusal.
func FuzzSecretCreationTimeRoundTrip(f *testing.F) {
	for _, tuple := range []struct {
		seconds int64
		nanos   int32
	}{{1, 0}, {1700000000, 123456789}, {0, 0}, {253402300799, 999999999}, {math.MaxInt64, -1}} {
		f.Add(tuple.seconds, tuple.nanos)
	}
	f.Fuzz(func(t *testing.T, seconds int64, nanos int32) {
		value, err := NewSecretCreationTime(seconds, nanos)
		valid := seconds > 0 && seconds <= 253402300799 && nanos >= 0 && nanos <= 999999999
		if !valid {
			if !errors.Is(err, ErrInvalidSecretApproval) || value != (SecretCreationTime{}) {
				t.Fatal("invalid tuple accepted")
			}
			return
		}
		gotSeconds, gotNanos := value.Tuple()
		if err != nil || gotSeconds != seconds || gotNanos != nanos {
			t.Fatal("valid tuple changed")
		}
	})
}
