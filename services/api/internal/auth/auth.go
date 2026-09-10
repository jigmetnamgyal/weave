// Package auth turns a verified token into an authenticated user on the
// request context.
//
// Authentication only. It answers "who is this?" and never "may they do
// this?" — authorization is evaluated per operation against workspace
// membership in PostgreSQL, and arrives with the workspace model.
package auth

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

type contextKey int

const userKey contextKey = iota

// unauthenticatedMessage is returned for every rejection reason.
//
// One message for all cases is deliberate: distinguishing "expired" from
// "bad signature" from "unknown subject" tells an attacker which half of a
// forgery attempt succeeded, and no legitimate client needs the difference.
const unauthenticatedMessage = "Authentication required. Present a valid bearer token."

// Provisioner resolves a verified identity to a persisted user.
type Provisioner interface {
	FromIdentity(ctx context.Context, identity application.Identity) (domain.User, error)
}

// Middleware authenticates requests.
type Middleware struct {
	verifier    application.TokenVerifier
	provisioner Provisioner
	logger      *slog.Logger
}

// NewMiddleware constructs the authentication middleware.
func NewMiddleware(verifier application.TokenVerifier, provisioner Provisioner, logger *slog.Logger) *Middleware {
	return &Middleware{verifier: verifier, provisioner: provisioner, logger: logger}
}

// Require wraps a handler so it runs only for an authenticated caller.
//
// It is applied to a whole route subtree rather than to individual handlers,
// so a new route is protected by virtue of where it is registered. Making
// protection the default removes the chance to forget it.
func (m *Middleware) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		rawToken, err := bearerToken(r)
		if err != nil {
			m.reject(ctx, w, "no bearer token presented", err)
			return
		}

		identity, err := m.verifier.Verify(ctx, rawToken)
		if err != nil {
			if errors.Is(err, application.ErrVerifierUnavailable) {
				// The credential may well be valid; we simply cannot tell.
				// Reporting 503 keeps an identity-provider outage from looking
				// like a wave of failed logins.
				m.logger.ErrorContext(ctx, "token verifier unavailable",
					slog.String("error", err.Error()),
					slog.String("request_id", httpx.RequestID(ctx)),
				)
				httpx.WriteError(ctx, w, http.StatusServiceUnavailable, httpx.CodeInternal,
					"Authentication is temporarily unavailable. Retry shortly.")
				return
			}
			m.reject(ctx, w, "token rejected", err)
			return
		}

		user, err := m.provisioner.FromIdentity(ctx, identity)
		if err != nil {
			// The caller authenticated; we failed to record them. That is our
			// fault, not theirs, and must not read as an auth failure.
			m.logger.ErrorContext(ctx, "failed to resolve authenticated user",
				slog.String("error", err.Error()),
				slog.String("request_id", httpx.RequestID(ctx)),
			)
			httpx.WriteError(ctx, w, http.StatusInternalServerError, httpx.CodeInternal,
				"Something went wrong. The request was not completed.")
			return
		}

		// The tenant context travels with the request from here. Row-level
		// security reads app.user_id from it, and a request that reached a
		// store without it would read nothing rather than another tenant's
		// rows — but establishing it once, at the edge, is what keeps that
		// from happening at all.
		ctx = context.WithValue(ctx, userKey, user)
		ctx = postgres.WithTenant(ctx, postgres.TenantContext{UserID: user.ID})

		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// reject logs the real reason and returns the uniform 401.
func (m *Middleware) reject(ctx context.Context, w http.ResponseWriter, reason string, err error) {
	m.logger.InfoContext(ctx, "authentication failed",
		slog.String("reason", reason),
		slog.String("error", err.Error()),
		slog.String("request_id", httpx.RequestID(ctx)),
	)
	httpx.WriteError(ctx, w, http.StatusUnauthorized, httpx.CodeUnauthenticated, unauthenticatedMessage)
}

// UserFrom returns the authenticated user carried by the context.
func UserFrom(ctx context.Context) (domain.User, bool) {
	user, ok := ctx.Value(userKey).(domain.User)
	return user, ok
}

// ContextWithUser attaches an authenticated user to a context.
//
// Middleware does this itself; this exists so that packages downstream of
// authentication can be tested at their own boundary, standing in for the
// middleware rather than reconstructing a signed token to get past it.
func ContextWithUser(ctx context.Context, user domain.User) context.Context {
	ctx = context.WithValue(ctx, userKey, user)
	return postgres.WithTenant(ctx, postgres.TenantContext{UserID: user.ID})
}

// bearerToken extracts the credential from the Authorization header.
func bearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	if strings.TrimSpace(header) == "" {
		return "", application.ErrTokenMissing
	}

	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", application.ErrTokenInvalid
	}

	token = strings.TrimSpace(token)
	if token == "" {
		return "", application.ErrTokenMissing
	}
	return token, nil
}
