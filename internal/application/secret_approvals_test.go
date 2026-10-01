package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// approvalReaderFake is scoped synthetic authority, never a production verifier.
type approvalReaderFake struct {
	calls         int
	selectedCalls int
	result        domain.ProviderSecretApproval
	err           error
	before        func()
}

// LookupApproval simulates the current approval projection without cloud calls.
func (f *approvalReaderFake) LookupApproval(context.Context, domain.SecretApprovalScope) (domain.ProviderSecretApproval, error) {
	f.calls++
	if f.before != nil {
		f.before()
	}
	return f.result, f.err
}

// LookupSelectedApproval counts exact-selection reads separately from registration.
func (f *approvalReaderFake) LookupSelectedApproval(context.Context, uuid.UUID, domain.SecretApprovalScope) (domain.ProviderSecretApproval, error) {
	f.selectedCalls++
	if f.before != nil {
		f.before()
	}
	return f.result, f.err
}

// resolverFixture constructs synthetic exact scope and nanosecond evidence.
func resolverFixture(t *testing.T) (domain.SecretApprovalScope, domain.ProviderSecretApproval) {
	t.Helper()
	scope := domain.SecretApprovalScope{WorkspaceID: uuid.New(), Provider: domain.ProviderClaudeCode, Reference: domain.ProviderSecretReference{Environment: "test", ProjectNumber: "918273645", SecretID: uuid.New(), Version: 1}}
	created, err := domain.NewSecretCreationTime(1700000000, 123456789)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := domain.NewProviderSecretApproval(domain.SecretApprovalRecord{ID: uuid.New(), SchemaVersion: 1, Scope: scope, ResourceFamily: "gcp_global", CredentialKind: "api_key", SecretCreated: created, VersionCreated: created})
	if err != nil {
		t.Fatal(err)
	}
	return scope, approved
}

// resolverForTest requires a valid fake dependency and performs no discovery.
func resolverForTest(t *testing.T, f *approvalReaderFake) *SecretApprovalResolver {
	t.Helper()
	r, err := NewSecretApprovalResolver(f)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestSecretApprovalResolverExactSelectionAndWithdrawal prevents mutable fallback
// and proves each call consults current authority rather than cached evidence.
func TestSecretApprovalResolverExactSelectionAndWithdrawal(t *testing.T) {
	scope, approval := resolverFixture(t)
	f := &approvalReaderFake{result: approval}
	r := resolverForTest(t, f)
	got, err := r.Resolve(context.Background(), scope)
	if err != nil || got != approval || f.calls != 1 {
		t.Fatal("scoped lookup failed", err)
	}
	got, err = r.ResolveSelected(context.Background(), approval.ID(), scope)
	if err != nil || got != approval || f.selectedCalls != 1 {
		t.Fatal("selected lookup failed", err)
	}
	_, replacement := resolverFixture(t)
	record := replacement.Record()
	record.Scope = scope
	f.result, err = domain.NewProviderSecretApproval(record)
	if err != nil {
		t.Fatal(err)
	}
	if got, err = r.ResolveSelected(context.Background(), approval.ID(), scope); !errors.Is(err, ErrCredentialReferenceRejected) || got != (domain.ProviderSecretApproval{}) {
		t.Fatal("selected ID switched to current approval", err)
	}
	f.err = ErrCredentialReferenceRejected
	if got, err = r.Resolve(context.Background(), scope); !errors.Is(err, ErrCredentialReferenceRejected) || got != (domain.ProviderSecretApproval{}) || f.calls != 2 {
		t.Fatal("withdrawn evidence cached", err)
	}
	if f.selectedCalls != 2 {
		t.Fatal("selected lookup fell back to registration")
	}
}

// TestSecretApprovalResolverRejectsScopeBeforeIO refuses malformed lookup intent.
func TestSecretApprovalResolverRejectsScopeBeforeIO(t *testing.T) {
	scope, approval := resolverFixture(t)
	for name, mutate := range map[string]func(*domain.SecretApprovalScope){
		"workspace": func(s *domain.SecretApprovalScope) { s.WorkspaceID = uuid.Nil },
		"provider":  func(s *domain.SecretApprovalScope) { s.Provider = domain.ProviderFake },
		"project":   func(s *domain.SecretApprovalScope) { s.Reference.ProjectNumber = "SYNTHETIC_PRIVATE_MARKER" },
		"version":   func(s *domain.SecretApprovalScope) { s.Reference.Version = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			f := &approvalReaderFake{result: approval}
			r := resolverForTest(t, f)
			bad := scope
			mutate(&bad)
			for _, selected := range []bool{false, true} {
				var err error
				if selected {
					_, err = r.ResolveSelected(context.Background(), approval.ID(), bad)
				} else {
					_, err = r.Resolve(context.Background(), bad)
				}
				if !errors.Is(err, ErrCredentialReferenceRejected) || f.calls+f.selectedCalls != 0 {
					t.Fatal("bad scope reached authority", err)
				}
			}
		})
	}
	f := &approvalReaderFake{result: approval}
	r := resolverForTest(t, f)
	if _, err := r.ResolveSelected(context.Background(), uuid.Nil, scope); !errors.Is(err, ErrCredentialReferenceRejected) || f.selectedCalls != 0 {
		t.Fatal("missing selection reached authority", err)
	}
}

// TestSecretApprovalResolverRejectsForgedResult checks every scope dimension,
// including matching valid evidence that the authority returned for another owner.
func TestSecretApprovalResolverRejectsForgedResult(t *testing.T) {
	scope, approval := resolverFixture(t)
	for name, mutate := range map[string]func(*domain.SecretApprovalRecord){
		"workspace":   func(r *domain.SecretApprovalRecord) { r.Scope.WorkspaceID = uuid.New() },
		"environment": func(r *domain.SecretApprovalRecord) { r.Scope.Reference.Environment = "production" },
		"project":     func(r *domain.SecretApprovalRecord) { r.Scope.Reference.ProjectNumber = "123456789" },
		"resource":    func(r *domain.SecretApprovalRecord) { r.Scope.Reference.SecretID = uuid.New() },
		"version":     func(r *domain.SecretApprovalRecord) { r.Scope.Reference.Version++ },
	} {
		t.Run(name, func(t *testing.T) {
			record := approval.Record()
			mutate(&record)
			bad, err := domain.NewProviderSecretApproval(record)
			if err != nil {
				t.Fatal(err)
			}
			f := &approvalReaderFake{result: bad}
			r := resolverForTest(t, f)
			got, err := r.Resolve(context.Background(), scope)
			if !errors.Is(err, ErrCredentialReferenceRejected) || got != (domain.ProviderSecretApproval{}) {
				t.Fatal("forged authority scope accepted", err)
			}
		})
	}
	f := &approvalReaderFake{}
	if _, err := resolverForTest(t, f).Resolve(context.Background(), scope); !errors.Is(err, ErrCredentialReferenceRejected) {
		t.Fatal("zero approval accepted", err)
	}
}

// TestSecretApprovalResolverSafeDiagnosticsAndCancellation prevents unknown
// storage errors/context causes or late success from escaping as usable evidence.
func TestSecretApprovalResolverSafeDiagnosticsAndCancellation(t *testing.T) {
	scope, approval := resolverFixture(t)
	for _, driverErr := range []error{errors.New("SYNTHETIC_PRIVATE_MARKER"), fmt.Errorf("SYNTHETIC_PRIVATE_MARKER: %w", ErrCredentialReferenceRejected)} {
		f := &approvalReaderFake{result: approval, err: driverErr}
		got, err := resolverForTest(t, f).Resolve(context.Background(), scope)
		if err == nil || strings.Contains(err.Error(), "SYNTHETIC_PRIVATE_MARKER") || got != (domain.ProviderSecretApproval{}) {
			t.Fatal("diagnostic leaked or error evidence accepted")
		}
	}
	f := &approvalReaderFake{result: approval}
	r := resolverForTest(t, f)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(errors.New("SYNTHETIC_PRIVATE_MARKER"))
	if _, err := r.Resolve(ctx, scope); !errors.Is(err, context.Canceled) || f.calls != 0 || strings.Contains(err.Error(), "SYNTHETIC_PRIVATE_MARKER") {
		t.Fatal("cancelled lookup performed I/O or leaked cause", err)
	}
	ctx, cancel = context.WithCancelCause(context.Background())
	defer cancel(nil)
	f.before = func() { cancel(errors.New("SYNTHETIC_PRIVATE_MARKER")) }
	if got, err := r.ResolveSelected(ctx, approval.ID(), scope); !errors.Is(err, context.Canceled) || got != (domain.ProviderSecretApproval{}) {
		t.Fatal("late cancelled success accepted", err)
	}
}

// TestSecretApprovalResolverRequiresAuthority proves nil/typed-nil construction
// fails closed without any cloud/credential discovery or default authority.
func TestSecretApprovalResolverRequiresAuthority(t *testing.T) {
	var typedNil *approvalReaderFake
	for _, reader := range []SecretApprovalReader{nil, typedNil} {
		if r, err := NewSecretApprovalResolver(reader); r != nil || !errors.Is(err, ErrCredentialConfiguration) {
			t.Fatal("unwired authority accepted")
		}
	}
}
