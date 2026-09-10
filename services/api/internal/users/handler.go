// Package users serves the authenticated user's own profile.
package users

import (
	"net/http"

	"github.com/jigmetnamgyal/weave/services/api/internal/auth"
	"github.com/jigmetnamgyal/weave/services/api/internal/httpx"
)

// meResponse is the body of GET /v1/me.
//
// It carries the internal user ID and nothing from the identity provider.
// The Clerk subject is an implementation detail of how someone signed in;
// exposing it would let clients start keying on it, which is exactly the
// coupling the internal ID exists to prevent.
type meResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name,omitempty"`
	AvatarURL   string `json:"avatar_url,omitempty"`
}

// Me returns the handler for GET /v1/me.
func Me() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		user, ok := auth.UserFrom(ctx)
		if !ok {
			// Unreachable through the router, since this handler is only
			// mounted behind the authentication middleware. Handled anyway so
			// a future mounting mistake fails closed instead of panicking or
			// serving an empty profile.
			httpx.WriteError(ctx, w, http.StatusUnauthorized, httpx.CodeUnauthenticated,
				"Authentication required. Present a valid bearer token.")
			return
		}

		httpx.WriteJSON(ctx, w, http.StatusOK, meResponse{
			ID:          user.ID.String(),
			Email:       user.Email,
			DisplayName: user.DisplayName,
			AvatarURL:   user.AvatarURL,
		})
	})
}
