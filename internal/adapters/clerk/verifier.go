// Package clerk implements the application's TokenVerifier port against
// Clerk-issued session tokens.
//
// Nothing above this package knows Clerk exists. Swapping identity providers
// means writing a sibling package that satisfies the same port; it does not
// mean touching the API, the domain or the database.
package clerk

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/lestrrat-go/httprc/v3"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// Config configures the verifier.
type Config struct {
	// Issuer is the expected `iss` claim, e.g. https://example.clerk.accounts.dev.
	Issuer string
	// JWKSURL is where signing keys are published. Defaults to the issuer's
	// standard discovery path.
	JWKSURL string
	// Audience, when set, is required to appear in the token's `aud` claim.
	//
	// Clerk's default session token carries no audience; one appears when a
	// JWT template sets it. Enforcement is therefore opt-in, and skipping it
	// is safe only because the issuer check already binds the token to this
	// Clerk instance.
	Audience string
	// Leeway absorbs clock skew between Clerk and this service when checking
	// time-based claims.
	Leeway time.Duration
	// RefreshInterval bounds how often signing keys are re-fetched.
	RefreshInterval time.Duration
	// LookupTimeout bounds how long a request may wait for key material.
	//
	// The cache blocks until a key set is available, which on a cold cache
	// with an unreachable issuer means blocking indefinitely. Bounding it
	// turns an identity-provider outage into a prompt 503 instead of requests
	// piling up until they hit the server's write timeout.
	LookupTimeout time.Duration
	// HTTPClient is used to fetch the key set. Tests inject their own.
	HTTPClient *http.Client
}

const (
	defaultLeeway          = 30 * time.Second
	defaultRefreshInterval = 15 * time.Minute
	defaultFetchTimeout    = 10 * time.Second
	defaultLookupTimeout   = 5 * time.Second
)

// Verifier validates Clerk session tokens against Clerk's published keys.
type Verifier struct {
	cfg   Config
	cache *jwk.Cache
}

// compile-time check that the adapter satisfies the port.
var _ application.TokenVerifier = (*Verifier)(nil)

// New constructs a Verifier.
//
// It registers the key-set URL but deliberately does not fetch it: startup
// must not depend on Clerk being reachable, and a service that cannot fetch
// keys should fail readiness and requests, not refuse to boot.
func New(ctx context.Context, cfg Config) (*Verifier, error) {
	if strings.TrimSpace(cfg.Issuer) == "" {
		return nil, errors.New("clerk: issuer is required")
	}
	cfg.Issuer = strings.TrimRight(cfg.Issuer, "/")

	if cfg.JWKSURL == "" {
		cfg.JWKSURL = cfg.Issuer + "/.well-known/jwks.json"
	}
	if cfg.Leeway == 0 {
		cfg.Leeway = defaultLeeway
	}
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = defaultRefreshInterval
	}
	if cfg.LookupTimeout == 0 {
		cfg.LookupTimeout = defaultLookupTimeout
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultFetchTimeout}
	}

	cache, err := jwk.NewCache(ctx, httprc.NewClient(
		httprc.WithHTTPClient(cfg.HTTPClient),
	))
	if err != nil {
		return nil, fmt.Errorf("clerk: create key cache: %w", err)
	}

	// WithWaitReady(false) is load-bearing: Register otherwise performs a
	// synchronous first fetch and blocks until it succeeds, which would make
	// this service refuse to start whenever Clerk is unreachable. Registering
	// lazily means the first Verify populates the cache — under a bounded
	// timeout — and the cache refreshes in the background from then on, so no
	// request path waits on a key it already holds.
	if err := cache.Register(ctx, cfg.JWKSURL,
		jwk.WithMinInterval(cfg.RefreshInterval),
		jwk.WithWaitReady(false),
	); err != nil {
		return nil, fmt.Errorf("clerk: register key set: %w", err)
	}

	return &Verifier{cfg: cfg, cache: cache}, nil
}

// Verify validates a raw bearer token and returns the identity it asserts.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (application.Identity, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return application.Identity{}, application.ErrTokenMissing
	}

	// Check the header before doing any signature or network work. Clerk signs
	// with RS256, and pinning it here closes `alg: none` and algorithm
	// confusion — presenting an HMAC token signed with the RSA public key so a
	// permissive verifier treats the public key as a shared secret.
	if err := v.checkHeader(rawToken); err != nil {
		return application.Identity{}, err
	}

	lookupCtx, cancel := context.WithTimeout(ctx, v.cfg.LookupTimeout)
	defer cancel()

	// Because registration is lazy, the very first request may arrive before
	// the key set has been fetched. Ready blocks until it lands or the bounded
	// context expires; once the cache is warm it returns immediately, so this
	// costs nothing on the steady-state path.
	if !v.cache.Ready(lookupCtx, v.cfg.JWKSURL) {
		return application.Identity{}, fmt.Errorf(
			"%w: signing keys are not available", application.ErrVerifierUnavailable)
	}

	keys, err := v.cache.Lookup(lookupCtx, v.cfg.JWKSURL)
	if err != nil {
		// Key material is unreachable. This is a dependency outage, and
		// reporting it as "invalid token" would misattribute the failure and
		// mask an outage behind a wall of 401s.
		return application.Identity{}, fmt.Errorf("%w: %v", application.ErrVerifierUnavailable, err)
	}

	parseOptions := []jwt.ParseOption{
		jwt.WithKeySet(keys),
		jwt.WithValidate(true),
		jwt.WithIssuer(v.cfg.Issuer),
		jwt.WithAcceptableSkew(v.cfg.Leeway),
	}
	if v.cfg.Audience != "" {
		parseOptions = append(parseOptions, jwt.WithAudience(v.cfg.Audience))
	}

	token, err := jwt.Parse([]byte(rawToken), parseOptions...)
	if err != nil {
		return application.Identity{}, fmt.Errorf("%w: %v", application.ErrTokenInvalid, err)
	}

	subject, ok := token.Subject()
	if !ok || strings.TrimSpace(subject) == "" {
		return application.Identity{}, fmt.Errorf("%w: token has no subject", application.ErrTokenInvalid)
	}

	expiry, _ := token.Expiration()

	identity := application.Identity{
		Subject:     subject,
		Email:       stringClaim(token, "email"),
		DisplayName: displayName(token),
		AvatarURL:   stringClaim(token, "image_url"),
		ExpiresAt:   expiry,
	}

	if identity.Email == "" {
		// Every downstream user record requires an email. A token without one
		// means the Clerk JWT template is misconfigured; failing here is far
		// easier to diagnose than a constraint violation at insert time.
		return application.Identity{}, fmt.Errorf("%w: token carries no email claim", application.ErrTokenInvalid)
	}

	return identity, nil
}

// algRS256 is the only signature algorithm accepted from Clerk.
var algRS256 = jwa.RS256()

// checkHeader enforces the signing algorithm and token type before the token
// is verified against any key.
func (v *Verifier) checkHeader(rawToken string) error {
	message, err := jws.Parse([]byte(rawToken), jws.WithCompact())
	if err != nil {
		return fmt.Errorf("%w: malformed token", application.ErrTokenInvalid)
	}

	signatures := message.Signatures()
	if len(signatures) != 1 {
		return fmt.Errorf("%w: expected exactly one signature, got %d",
			application.ErrTokenInvalid, len(signatures))
	}

	headers := signatures[0].ProtectedHeaders()

	alg, ok := headers.Algorithm()
	if !ok || alg != algRS256 {
		return fmt.Errorf("%w: unexpected signing algorithm", application.ErrTokenInvalid)
	}

	// `typ` is optional in the JWS spec, so accept its absence — but when it
	// is present it must say JWT.
	if typ, ok := headers.Type(); ok && !strings.EqualFold(typ, "JWT") {
		return fmt.Errorf("%w: unexpected token type %q", application.ErrTokenInvalid, typ)
	}

	return nil
}

// stringClaim reads an optional private string claim.
func stringClaim(token jwt.Token, name string) string {
	var value string
	if err := token.Get(name, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// displayName prefers an explicit name claim and falls back to assembling one
// from the given and family names Clerk supplies separately.
func displayName(token jwt.Token) string {
	if name := stringClaim(token, "name"); name != "" {
		return name
	}
	parts := make([]string, 0, 2)
	for _, claim := range []string{"given_name", "family_name"} {
		if value := stringClaim(token, claim); value != "" {
			parts = append(parts, value)
		}
	}
	return strings.Join(parts, " ")
}
