package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"math"
	"strings"
	"testing"
)

// TestProviderReferenceIsRestricted proves arbitrary fmt/JSON cannot spill refs.
func TestProviderReferenceIsRestricted(t *testing.T) {
	ref := ProviderSecretReference{Environment: "test", ProjectNumber: "918273645", SecretID: uuid.New(), Version: 1}
	proof := VerifiedProviderReference{WorkspaceID: uuid.New(), Provider: ProviderClaudeCode, Reference: ref, VerificationID: uuid.New()}
	for _, value := range []any{ref, &ref, proof, &proof, struct{ Ref ProviderSecretReference }{ref}} {
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q"} {
			got := fmt.Sprintf(verb, value)
			if strings.Contains(got, ref.ProjectNumber) || strings.Contains(got, ref.SecretID.String()) {
				t.Fatal("format exposed restricted reference")
			}
		}
		if _, err := json.Marshal(value); !errors.Is(err, ErrRestrictedCredentialMetadata) {
			t.Fatal("JSON reference not refused", err)
		}
	}
}

// TestProviderReferenceValidation checks canonical metadata, never aliases/URLs.
func TestProviderReferenceValidation(t *testing.T) {
	good := ProviderSecretReference{Environment: "test", ProjectNumber: "918273645", SecretID: uuid.New(), Version: 1}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []ProviderSecretReference{{}, {Environment: "private-marker", ProjectNumber: good.ProjectNumber, SecretID: good.SecretID, Version: 1}, {Environment: "test", ProjectNumber: "00123456", SecretID: good.SecretID, Version: 1}, {Environment: "test", ProjectNumber: "https://private-marker", SecretID: good.SecretID, Version: 1}, {Environment: "test", ProjectNumber: good.ProjectNumber, Version: 1}, {Environment: "test", ProjectNumber: good.ProjectNumber, SecretID: good.SecretID, Version: 0}, {Environment: "test", ProjectNumber: good.ProjectNumber, SecretID: good.SecretID, Version: -1}} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidCredentialReference) || strings.Contains(err.Error(), "private-marker") {
			t.Fatal("invalid reference accepted or exposed")
		}
	}
	for _, epoch := range []int64{-1, math.MaxInt64} {
		if !errors.Is(ValidateCredentialEpoch(epoch), ErrInvalidCredentialEpoch) {
			t.Fatal("unsafe epoch accepted")
		}
	}
	if ValidateCredentialEpoch(0) != nil || ValidateCredentialEpoch(math.MaxInt64-1) != nil {
		t.Fatal("valid epoch refused")
	}
}
