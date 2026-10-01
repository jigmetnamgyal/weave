package application

import (
	"context"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"strings"
	"testing"
)

type credentialRepoFake struct {
	authErr      error
	current      *domain.ProviderCredentialBinding
	writes       int
	registration CredentialRegistration
}

func (f *credentialRepoFake) Authorize(context.Context, uuid.UUID, Actor) error { return f.authErr }
func (f *credentialRepoFake) Get(context.Context, uuid.UUID, domain.Provider, Actor) (domain.ProviderCredentialBinding, error) {
	if f.authErr != nil {
		return domain.ProviderCredentialBinding{}, f.authErr
	}
	if f.current == nil {
		return domain.ProviderCredentialBinding{}, ErrCredentialNotFound
	}
	return *f.current, nil
}
func (f *credentialRepoFake) Register(_ context.Context, r CredentialRegistration, _ Actor, _ *CredentialCompletion) (domain.ProviderCredentialBinding, error) {
	f.writes++
	f.registration = r
	return domain.ProviderCredentialBinding{ID: r.BindingID, Epoch: r.ExpectedEpoch + 1}, nil
}
func (f *credentialRepoFake) Disable(context.Context, uuid.UUID, domain.Provider, int64, Actor, *CredentialCompletion) (domain.ProviderCredentialBinding, error) {
	f.writes++
	return domain.ProviderCredentialBinding{}, nil
}

type credentialVerifierFake struct {
	calls int
	fn    func(context.Context, uuid.UUID, domain.Provider, domain.ProviderSecretReference) (domain.VerifiedProviderReference, error)
}

func (f *credentialVerifierFake) Verify(ctx context.Context, ws uuid.UUID, p domain.Provider, r domain.ProviderSecretReference) (domain.VerifiedProviderReference, error) {
	f.calls++
	if f.fn != nil {
		return f.fn(ctx, ws, p, r)
	}
	return domain.VerifiedProviderReference{WorkspaceID: ws, Provider: p, Reference: r, VerificationID: uuid.New()}, nil
}
func credentialTestMember() domain.Membership {
	return domain.Membership{UserID: uuid.New(), WorkspaceID: uuid.New(), Role: domain.RoleOwner}
}
func credentialTestRef() domain.ProviderSecretReference {
	return domain.ProviderSecretReference{Environment: "test", ProjectNumber: "918273645", SecretID: uuid.New(), Version: 1}
}
func credentialTestService(t *testing.T, r *credentialRepoFake, v *credentialVerifierFake) *ProviderCredentialService {
	t.Helper()
	s, e := NewProviderCredentialService(r, v, "test", "918273645")
	if e != nil {
		t.Fatal(e)
	}
	return s
}

// TestCredentialPermissionBeforeVerification covers the matrix and live denial.
func TestCredentialPermissionBeforeVerification(t *testing.T) {
	for _, role := range domain.Roles {
		t.Run(string(role), func(t *testing.T) {
			r, v := &credentialRepoFake{}, &credentialVerifierFake{}
			s := credentialTestService(t, r, v)
			m := credentialTestMember()
			m.Role = role
			_, err := s.Register(context.Background(), m, domain.ProviderClaudeCode, 0, credentialTestRef(), nil)
			allowed := role == domain.RoleOwner || role == domain.RoleAdmin
			if allowed {
				if err != nil || v.calls != 1 || r.writes != 1 {
					t.Fatal("admin registration failed")
				}
			} else if !errors.Is(err, ErrPermissionDenied) || v.calls != 0 || r.writes != 0 {
				t.Fatal("denial did external verification or mutation")
			}
		})
	}
	for _, m := range []domain.Membership{{}, {Role: domain.RoleOwner, WorkspaceID: uuid.New()}, {Role: domain.RoleOwner, UserID: uuid.New()}} {
		r, v := &credentialRepoFake{}, &credentialVerifierFake{}
		s := credentialTestService(t, r, v)
		if _, err := s.Register(context.Background(), m, domain.ProviderClaudeCode, 0, credentialTestRef(), nil); !errors.Is(err, ErrPermissionDenied) || v.calls != 0 {
			t.Fatal("anonymous forged role verified")
		}
	}
	r, v := &credentialRepoFake{authErr: ErrPermissionDenied}, &credentialVerifierFake{}
	if _, err := credentialTestService(t, r, v).Register(context.Background(), credentialTestMember(), domain.ProviderClaudeCode, 0, credentialTestRef(), nil); !errors.Is(err, ErrPermissionDenied) || v.calls != 0 {
		t.Fatal("stale role verified")
	}
}

// TestCredentialScopeAndEpochBeforeVerification rejects unsafe intent cheaply.
func TestCredentialScopeAndEpochBeforeVerification(t *testing.T) {
	for _, tc := range []struct {
		p     domain.Provider
		epoch int64
		ref   domain.ProviderSecretReference
	}{
		{domain.ProviderFake, 0, credentialTestRef()}, {domain.ProviderCodex, 0, credentialTestRef()}, {domain.ProviderClaudeCode, -1, credentialTestRef()}, {domain.ProviderClaudeCode, 2, credentialTestRef()}, {domain.ProviderClaudeCode, 0, domain.ProviderSecretReference{Environment: "production", ProjectNumber: "918273645", SecretID: uuid.New(), Version: 1}}, {domain.ProviderClaudeCode, 0, domain.ProviderSecretReference{Environment: "test", ProjectNumber: "123456789", SecretID: uuid.New(), Version: 1}},
	} {
		r, v := &credentialRepoFake{}, &credentialVerifierFake{}
		_, err := credentialTestService(t, r, v).Register(context.Background(), credentialTestMember(), tc.p, tc.epoch, tc.ref, nil)
		if err == nil || v.calls != 0 || r.writes != 0 {
			t.Fatal("invalid intent verified or written")
		}
	}
	r, v := &credentialRepoFake{current: &domain.ProviderCredentialBinding{Epoch: 2}}, &credentialVerifierFake{}
	_, err := credentialTestService(t, r, v).Register(context.Background(), credentialTestMember(), domain.ProviderClaudeCode, 1, credentialTestRef(), nil)
	if !errors.Is(err, ErrCredentialConflict) || v.calls != 0 {
		t.Fatal("stale epoch verified")
	}
}

// TestCredentialVerifierScopeAndSafeErrors rejects forged results/raw diagnostics.
func TestCredentialVerifierScopeAndSafeErrors(t *testing.T) {
	for _, kind := range []string{"workspace", "provider", "reference", "proof", "error", "rejected"} {
		t.Run(kind, func(t *testing.T) {
			r, v := &credentialRepoFake{}, &credentialVerifierFake{}
			v.fn = func(_ context.Context, ws uuid.UUID, p domain.Provider, ref domain.ProviderSecretReference) (domain.VerifiedProviderReference, error) {
				result := domain.VerifiedProviderReference{WorkspaceID: ws, Provider: p, Reference: ref, VerificationID: uuid.New()}
				switch kind {
				case "workspace":
					result.WorkspaceID = uuid.New()
				case "provider":
					result.Provider = domain.ProviderCodex
				case "reference":
					result.Reference.Version++
				case "proof":
					result.VerificationID = uuid.Nil
				case "error":
					return result, errors.New("SYNTHETIC_PRIVATE_MARKER")
				case "rejected":
					return result, fmt.Errorf("SYNTHETIC_PRIVATE_MARKER: %w", ErrCredentialReferenceRejected)
				}
				return result, nil
			}
			_, err := credentialTestService(t, r, v).Register(context.Background(), credentialTestMember(), domain.ProviderClaudeCode, 0, credentialTestRef(), nil)
			if err == nil || strings.Contains(err.Error(), "SYNTHETIC_PRIVATE_MARKER") || r.writes != 0 {
				t.Fatal("forged verification accepted or diagnostics leaked")
			}
		})
	}
}

// TestCredentialCancellationBeforeCommit prevents late verification success writes.
func TestCredentialCancellationBeforeCommit(t *testing.T) {
	r, v := &credentialRepoFake{}, &credentialVerifierFake{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := credentialTestService(t, r, v).Register(ctx, credentialTestMember(), domain.ProviderClaudeCode, 0, credentialTestRef(), nil)
	if !errors.Is(err, context.Canceled) || v.calls != 0 || r.writes != 0 {
		t.Fatal("cancelled intent verified")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	v.fn = func(_ context.Context, ws uuid.UUID, p domain.Provider, ref domain.ProviderSecretReference) (domain.VerifiedProviderReference, error) {
		cancel()
		return domain.VerifiedProviderReference{WorkspaceID: ws, Provider: p, Reference: ref, VerificationID: uuid.New()}, nil
	}
	_, err = credentialTestService(t, r, v).Register(ctx, credentialTestMember(), domain.ProviderClaudeCode, 0, credentialTestRef(), nil)
	if !errors.Is(err, context.Canceled) || r.writes != 0 {
		t.Fatal("late success committed")
	}
}

// TestCredentialVerifierDeadline and constructor keep unwired/late ports closed.
func TestCredentialVerifierDeadline(t *testing.T) {
	r, v := &credentialRepoFake{}, &credentialVerifierFake{}
	v.fn = func(ctx context.Context, ws uuid.UUID, p domain.Provider, ref domain.ProviderSecretReference) (domain.VerifiedProviderReference, error) {
		<-ctx.Done()
		return domain.VerifiedProviderReference{WorkspaceID: ws, Provider: p, Reference: ref, VerificationID: uuid.New()}, nil
	}
	_, err := credentialTestService(t, r, v).Register(context.Background(), credentialTestMember(), domain.ProviderClaudeCode, 0, credentialTestRef(), nil)
	if !errors.Is(err, ErrCredentialVerificationUnavailable) || r.writes != 0 {
		t.Fatal("deadline ignored")
	}
	if _, err := NewProviderCredentialService(r, nil, "test", "918273645"); !errors.Is(err, ErrCredentialConfiguration) {
		t.Fatal("unwired verifier accepted")
	}
	if _, err := NewProviderCredentialService(nil, v, "test", "918273645"); !errors.Is(err, ErrCredentialConfiguration) {
		t.Fatal("unwired store accepted")
	}
	if _, err := NewProviderCredentialService(r, v, "bad", "private-marker"); !errors.Is(err, ErrCredentialConfiguration) {
		t.Fatal("invalid environment accepted")
	}
}

// TestCredentialStatusDisableAndReplayAuthorization keeps non-registration
// paths authorized and key-free, including stale role and invalid intent.
func TestCredentialStatusDisableAndReplayAuthorization(t *testing.T) {
	for _, role := range domain.Roles {
		r, v := &credentialRepoFake{current: &domain.ProviderCredentialBinding{Epoch: 9}}, &credentialVerifierFake{}
		s := credentialTestService(t, r, v)
		m := credentialTestMember()
		m.Role = role
		authErr := s.Authorize(context.Background(), m)
		got, getErr := s.Get(context.Background(), m, domain.ProviderClaudeCode)
		_, disableErr := s.Disable(context.Background(), m, domain.ProviderClaudeCode, 9, nil)
		if role == domain.RoleOwner || role == domain.RoleAdmin {
			if authErr != nil || getErr != nil || disableErr != nil || got.Epoch != 9 || r.writes != 1 {
				t.Fatal("authorized status/disable failed")
			}
		} else {
			if !errors.Is(authErr, ErrPermissionDenied) || !errors.Is(getErr, ErrPermissionDenied) || !errors.Is(disableErr, ErrPermissionDenied) || r.writes != 0 {
				t.Fatal("unauthorized status/disable/replay allowed")
			}
		}
		if v.calls != 0 {
			t.Fatal("status/disable verified or accessed credentials")
		}
	}
	r, v := &credentialRepoFake{}, &credentialVerifierFake{}
	s := credentialTestService(t, r, v)
	m := credentialTestMember()
	if _, err := s.Get(context.Background(), m, domain.ProviderCodex); !errors.Is(err, ErrCredentialProviderUnsupported) {
		t.Fatal("unsupported status provider accepted")
	}
	if _, err := s.Disable(context.Background(), m, domain.ProviderCodex, 0, nil); !errors.Is(err, ErrCredentialProviderUnsupported) {
		t.Fatal("unsupported disable provider accepted")
	}
	if _, err := s.Disable(context.Background(), m, domain.ProviderClaudeCode, -1, nil); !errors.Is(err, domain.ErrInvalidCredentialEpoch) || r.writes != 0 {
		t.Fatal("invalid disable epoch accepted")
	}
	r.authErr = ErrPermissionDenied
	if err := s.Authorize(context.Background(), m); !errors.Is(err, ErrPermissionDenied) {
		t.Fatal("stale replay role accepted")
	}
}
