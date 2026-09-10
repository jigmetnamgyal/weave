package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

const (
	validToken  = "valid-token"
	testSubject = "user_2abcXYZ"
	testEmail   = "dev@example.com"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubVerifier accepts exactly one token and reports a configurable failure
// for everything else.
type stubVerifier struct {
	err error
}

func (s stubVerifier) Verify(_ context.Context, raw string) (application.Identity, error) {
	if s.err != nil {
		return application.Identity{}, s.err
	}
	if raw != validToken {
		return application.Identity{}, application.ErrTokenInvalid
	}
	return application.Identity{
		Subject:   testSubject,
		Email:     testEmail,
		ExpiresAt: time.Now().Add(time.Hour),
	}, nil
}

// stubProvisioner returns a fixed user, or an error.
type stubProvisioner struct {
	err error
}

func (s stubProvisioner) FromIdentity(_ context.Context, identity application.Identity) (domain.User, error) {
	if s.err != nil {
		return domain.User{}, s.err
	}
	return domain.User{
		ID:         uuid.MustParse("018f0000-0000-7000-8000-000000000001"),
		ExternalID: identity.Subject,
		Email:      identity.Email,
	}, nil
}

// protectedHandler records whether the request reached past the middleware.
func protectedHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		if _, ok := UserFrom(r.Context()); !ok {
			t := w.Header()
			t.Set("X-Missing-User", "true")
		}
		w.WriteHeader(http.StatusOK)
	})
}

// serve runs a request through request-ID and auth middleware.
func serve(t *testing.T, m *Middleware, req *http.Request, reached *bool) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	httpx.WithRequestID(m.Require(protectedHandler(reached))).ServeHTTP(rec, req)
	return rec
}

func TestRequireRejects(t *testing.T) {
	tests := []struct {
		name       string
		authHeader string
		verifier   application.TokenVerifier
	}{
		{name: "no authorization header", authHeader: ""},
		{name: "empty bearer token", authHeader: "Bearer "},
		{name: "wrong scheme", authHeader: "Basic dXNlcjpwYXNz"},
		{name: "scheme without token", authHeader: "Bearer"},
		{name: "token the verifier rejects", authHeader: "Bearer forged-token"},
		{
			name:       "expired token",
			authHeader: "Bearer expired",
			verifier:   stubVerifier{err: application.ErrTokenInvalid},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verifier := tt.verifier
			if verifier == nil {
				verifier = stubVerifier{}
			}
			m := NewMiddleware(verifier, stubProvisioner{}, discardLogger())

			req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
			if tt.authHeader != "" {
				req.Header.Set("Authorization", tt.authHeader)
			}

			reached := false
			rec := serve(t, m, req, &reached)

			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusUnauthorized)
			}
			if reached {
				t.Error("request reached the protected handler")
			}

			var body httpx.ErrorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if body.Code != httpx.CodeUnauthenticated {
				t.Errorf("code = %q, want %q", body.Code, httpx.CodeUnauthenticated)
			}
			if body.RequestID == "" {
				t.Error("error body has no request_id")
			}
		})
	}
}

// TestRequireGivesUniformRejectionMessage guards against leaking which part of
// a credential failed, which would tell an attacker how close they got.
func TestRequireGivesUniformRejectionMessage(t *testing.T) {
	m := NewMiddleware(stubVerifier{}, stubProvisioner{}, discardLogger())

	messages := make(map[string]struct{})
	for _, header := range []string{"", "Bearer forged", "Basic abc", "Bearer "} {
		req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
		if header != "" {
			req.Header.Set("Authorization", header)
		}
		reached := false
		rec := serve(t, m, req, &reached)

		var body httpx.ErrorBody
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode error body: %v", err)
		}
		messages[body.Message] = struct{}{}
	}

	if len(messages) != 1 {
		t.Errorf("rejection messages differ between failure modes: %v", messages)
	}
}

func TestRequireAcceptsValidToken(t *testing.T) {
	m := NewMiddleware(stubVerifier{}, stubProvisioner{}, discardLogger())

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+validToken)

	reached := false
	rec := serve(t, m, req, &reached)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if !reached {
		t.Error("request did not reach the protected handler")
	}
	if rec.Header().Get("X-Missing-User") == "true" {
		t.Error("authenticated user was not placed on the request context")
	}
}

// TestRequireAcceptsSchemeCaseInsensitively covers RFC 7235, which defines the
// auth scheme as case-insensitive.
func TestRequireAcceptsSchemeCaseInsensitively(t *testing.T) {
	m := NewMiddleware(stubVerifier{}, stubProvisioner{}, discardLogger())

	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
		req.Header.Set("Authorization", scheme+" "+validToken)

		reached := false
		rec := serve(t, m, req, &reached)

		if rec.Code != http.StatusOK {
			t.Errorf("scheme %q: status = %d, want %d", scheme, rec.Code, http.StatusOK)
		}
	}
}

// TestRequireReportsVerifierOutageAsUnavailable proves an identity-provider
// outage is not misreported as a wave of failed logins.
func TestRequireReportsVerifierOutageAsUnavailable(t *testing.T) {
	m := NewMiddleware(
		stubVerifier{err: application.ErrVerifierUnavailable},
		stubProvisioner{},
		discardLogger(),
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+validToken)

	reached := false
	rec := serve(t, m, req, &reached)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if reached {
		t.Error("request reached the protected handler during a verifier outage")
	}
}

// TestRequireReportsProvisioningFailureAsServerError proves a storage failure
// after successful authentication is not reported as an auth failure.
func TestRequireReportsProvisioningFailureAsServerError(t *testing.T) {
	m := NewMiddleware(
		stubVerifier{},
		stubProvisioner{err: errors.New("connection refused")},
		discardLogger(),
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+validToken)

	reached := false
	rec := serve(t, m, req, &reached)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("response leaked the underlying error: %s", rec.Body.String())
	}
}

// TestRoutingDeniesByDefault proves protection comes from where a route is
// mounted, so a newly added /v1 route is authenticated without the author
// having to remember.
func TestRoutingDeniesByDefault(t *testing.T) {
	m := NewMiddleware(stubVerifier{}, stubProvisioner{}, discardLogger())

	protected := http.NewServeMux()
	protected.Handle("GET /v1/me", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// A route added later by someone who never thought about authentication.
	protected.Handle("GET /v1/brand-new-route", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	mux := http.NewServeMux()
	mux.Handle("GET /health/live", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mux.Handle("/v1/", m.Require(protected))

	handler := httpx.WithRequestID(mux)

	tests := []struct {
		path     string
		token    bool
		wantCode int
	}{
		{path: "/health/live", token: false, wantCode: http.StatusOK},
		{path: "/v1/me", token: false, wantCode: http.StatusUnauthorized},
		{path: "/v1/me", token: true, wantCode: http.StatusOK},
		{path: "/v1/brand-new-route", token: false, wantCode: http.StatusUnauthorized},
		{path: "/v1/brand-new-route", token: true, wantCode: http.StatusOK},
	}

	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)
		if tt.token {
			req.Header.Set("Authorization", "Bearer "+validToken)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != tt.wantCode {
			t.Errorf("%s (token=%v): status = %d, want %d", tt.path, tt.token, rec.Code, tt.wantCode)
		}
	}
}
