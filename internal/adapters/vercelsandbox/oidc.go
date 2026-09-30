package vercelsandbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/lestrrat-go/httprc/v3"
	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jws"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// Vercel's sandbox OIDC token (Unit M5.4c, ADR-016).
//
// Every request Vercel's firewall forwards to a `forwardURL` carries
// `Vercel-Sandbox-Oidc-Token`: an RS256 JWT from https://oidc.vercel.com/<team>
// whose audience is the exact forwardURL and whose claims name the project and
// the sandbox (measured in M5.4c's spike). The sandbox never holds it — the
// firewall adds it on the way out — so it proves the request came from a
// sandbox of ours, through Vercel, to this route.

// OIDCConfig configures verification.
type OIDCConfig struct {
	// TeamID is the Vercel team the runner project belongs to; the issuer is
	// https://oidc.vercel.com/<TeamID>.
	TeamID string
	// ProjectID is the runner project: a token for any other project's
	// sandbox is refused.
	ProjectID string
	// Leeway tolerates clock skew on exp and nbf.
	Leeway time.Duration
	// RefreshInterval bounds how often the key set is re-fetched.
	RefreshInterval time.Duration
	// LookupTimeout bounds waiting for keys on a request.
	LookupTimeout time.Duration

	// IssuerBase and HTTPClient exist for tests.
	IssuerBase string
	HTTPClient *http.Client
}

// SandboxToken is what a verified token proves.
type SandboxToken struct {
	SandboxID   string
	SandboxName string
	ProjectID   string
}

// OIDCVerifier verifies Vercel sandbox OIDC tokens.
type OIDCVerifier struct {
	issuer  string
	jwksURL string
	project string
	leeway  time.Duration
	lookup  time.Duration
	cache   *jwk.Cache
}

// NewOIDCVerifier registers the issuer's key set without fetching it: startup
// must not depend on Vercel being reachable. Requests before the first fetch
// wait, bounded, and a verifier that cannot get keys refuses rather than
// passing anything through.
func NewOIDCVerifier(ctx context.Context, cfg OIDCConfig) (*OIDCVerifier, error) {
	if strings.TrimSpace(cfg.TeamID) == "" || strings.TrimSpace(cfg.ProjectID) == "" {
		return nil, errors.New("vercelsandbox: OIDC verification needs the team and project ids")
	}
	base := strings.TrimRight(cfg.IssuerBase, "/")
	if base == "" {
		base = "https://oidc.vercel.com"
	}
	if cfg.Leeway == 0 {
		cfg.Leeway = 30 * time.Second
	}
	if cfg.RefreshInterval == 0 {
		cfg.RefreshInterval = 15 * time.Minute
	}
	if cfg.LookupTimeout == 0 {
		cfg.LookupTimeout = 5 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	issuer := base + "/" + cfg.TeamID
	jwksURL := issuer + "/.well-known/jwks"
	cache, err := jwk.NewCache(ctx, httprc.NewClient(httprc.WithHTTPClient(cfg.HTTPClient)))
	if err != nil {
		return nil, fmt.Errorf("vercelsandbox: create key cache: %w", err)
	}
	if err := cache.Register(ctx, jwksURL, jwk.WithMinInterval(cfg.RefreshInterval), jwk.WithWaitReady(false)); err != nil {
		return nil, fmt.Errorf("vercelsandbox: register key set: %w", err)
	}
	return &OIDCVerifier{issuer: issuer, jwksURL: jwksURL, project: cfg.ProjectID, leeway: cfg.Leeway,
		lookup: cfg.LookupTimeout, cache: cache}, nil
}

// Ready reports whether the key set has been fetched, for readiness.
func (v *OIDCVerifier) Ready(ctx context.Context) bool {
	return v.cache.Ready(ctx, v.jwksURL)
}

// Verify checks a token for one route. **audience is the exact forwardURL the
// request arrived on**, so a token Vercel issued for one registry's route is
// refused on another's.
//
// application.ErrTokenMissing, ErrTokenInvalid or ErrVerifierUnavailable,
// as for Clerk: unreachable keys are an outage, not an invalid token.
func (v *OIDCVerifier) Verify(ctx context.Context, raw, audience string) (SandboxToken, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return SandboxToken{}, application.ErrTokenMissing
	}
	// RS256 only, checked before any key is touched: closes `alg: none` and
	// algorithm confusion, as the Clerk verifier does.
	message, err := jws.Parse([]byte(raw), jws.WithCompact())
	if err != nil || len(message.Signatures()) != 1 {
		return SandboxToken{}, fmt.Errorf("%w: malformed token", application.ErrTokenInvalid)
	}
	if alg, ok := message.Signatures()[0].ProtectedHeaders().Algorithm(); !ok || alg != jwa.RS256() {
		return SandboxToken{}, fmt.Errorf("%w: unexpected signing algorithm", application.ErrTokenInvalid)
	}

	lookupCtx, cancel := context.WithTimeout(ctx, v.lookup)
	defer cancel()
	if !v.cache.Ready(lookupCtx, v.jwksURL) {
		return SandboxToken{}, fmt.Errorf("%w: signing keys are not available", application.ErrVerifierUnavailable)
	}
	keys, err := v.cache.Lookup(lookupCtx, v.jwksURL)
	if err != nil {
		return SandboxToken{}, fmt.Errorf("%w: %v", application.ErrVerifierUnavailable, err)
	}
	token, err := jwt.Parse([]byte(raw),
		jwt.WithKeySet(keys),
		jwt.WithValidate(true),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(audience),
		jwt.WithAcceptableSkew(v.leeway),
	)
	if err != nil {
		return SandboxToken{}, fmt.Errorf("%w: %v", application.ErrTokenInvalid, err)
	}
	// The audience check above accepts a token listing several audiences if
	// one matches. Vercel issues exactly one, the forwardURL; anything else
	// is not a token Vercel issued for this route.
	if aud, _ := token.Audience(); len(aud) != 1 || aud[0] != audience {
		return SandboxToken{}, fmt.Errorf("%w: audience is not exactly this route", application.ErrTokenInvalid)
	}
	claims := SandboxToken{
		ProjectID:   stringClaim(token, "project_id"),
		SandboxID:   stringClaim(token, "sandbox_id"),
		SandboxName: stringClaim(token, "sandbox_name"),
	}
	if claims.ProjectID != v.project {
		return SandboxToken{}, fmt.Errorf("%w: a sandbox of another project", application.ErrTokenInvalid)
	}
	if claims.SandboxName == "" || claims.SandboxID == "" {
		return SandboxToken{}, fmt.Errorf("%w: the token names no sandbox", application.ErrTokenInvalid)
	}
	return claims, nil
}

func stringClaim(token jwt.Token, name string) string {
	var value string
	if err := token.Get(name, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

// ErrNotARunnerSandbox means a sandbox name is not one this backend made.
var ErrNotARunnerSandbox = errors.New("vercelsandbox: not a runner sandbox name")

// RunnerFromSandboxName reads the runner id and scope back out of a sandbox
// name the backend made: weave-runner-<scope>-<runner id>. The inverse of
// Backend.sandboxName.
func RunnerFromSandboxName(name string) (runnerID uuid.UUID, scope string, err error) {
	const prefix = "weave-runner-"
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok || len(rest) < 36+2 || rest[len(rest)-37] != '-' {
		return uuid.Nil, "", fmt.Errorf("%w: %q", ErrNotARunnerSandbox, name)
	}
	scope, id := rest[:len(rest)-37], rest[len(rest)-36:]
	runnerID, err = uuid.Parse(id)
	if err != nil || !scopePattern.MatchString(scope) {
		return uuid.Nil, "", fmt.Errorf("%w: %q", ErrNotARunnerSandbox, name)
	}
	return runnerID, scope, nil
}
