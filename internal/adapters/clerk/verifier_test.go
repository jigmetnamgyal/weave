package clerk

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"

	"github.com/jigmetnamgyal/weave/internal/application"
)

const (
	testIssuer   = "https://test.clerk.example"
	testSubject  = "user_2abcXYZ"
	testEmail    = "dev@example.com"
	testAudience = "weave-api"
	testKeyID    = "test-key-1"
)

// jwksServer serves a JWKS for a generated key pair and counts fetches, so a
// test can assert the key set is cached rather than fetched per request.
type jwksServer struct {
	*httptest.Server
	fetches *atomic.Int64
}

// newSigningKey generates an RSA key pair and serves its public half as JWKS.
func newSigningKey(t *testing.T) (*rsa.PrivateKey, jwk.Key, *jwksServer) {
	t.Helper()

	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}

	private, err := jwk.Import(raw)
	if err != nil {
		t.Fatalf("import private key: %v", err)
	}
	if err := private.Set(jwk.KeyIDKey, testKeyID); err != nil {
		t.Fatalf("set kid: %v", err)
	}
	if err := private.Set(jwk.AlgorithmKey, jwa.RS256()); err != nil {
		t.Fatalf("set alg: %v", err)
	}

	public, err := jwk.PublicKeyOf(private)
	if err != nil {
		t.Fatalf("derive public key: %v", err)
	}

	set := jwk.NewSet()
	if err := set.AddKey(public); err != nil {
		t.Fatalf("add key to set: %v", err)
	}

	var fetches atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(set); err != nil {
			t.Errorf("encode jwks: %v", err)
		}
	}))
	t.Cleanup(server.Close)

	return raw, private, &jwksServer{Server: server, fetches: &fetches}
}

// tokenOptions describes a token to mint for a test case.
type tokenOptions struct {
	issuer    string
	subject   string
	audience  string
	email     string
	name      string
	issuedAt  time.Time
	expiresAt time.Time
	omitEmail bool
}

// mint builds and signs a token with the given options.
func mint(t *testing.T, key jwk.Key, opts tokenOptions) string {
	t.Helper()

	now := time.Now()
	if opts.issuedAt.IsZero() {
		opts.issuedAt = now
	}
	if opts.expiresAt.IsZero() {
		opts.expiresAt = now.Add(time.Hour)
	}
	if opts.issuer == "" {
		opts.issuer = testIssuer
	}
	if opts.subject == "" {
		opts.subject = testSubject
	}
	if opts.email == "" {
		opts.email = testEmail
	}

	builder := jwt.NewBuilder().
		Issuer(opts.issuer).
		Subject(opts.subject).
		IssuedAt(opts.issuedAt).
		Expiration(opts.expiresAt)

	if opts.audience != "" {
		builder = builder.Audience([]string{opts.audience})
	}
	if !opts.omitEmail {
		builder = builder.Claim("email", opts.email)
	}
	if opts.name != "" {
		builder = builder.Claim("name", opts.name)
	}

	token, err := builder.Build()
	if err != nil {
		t.Fatalf("build token: %v", err)
	}

	signed, err := jwt.Sign(token, jwt.WithKey(jwa.RS256(), key))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return string(signed)
}

// newVerifier builds a Verifier pointed at the test JWKS server.
func newVerifier(t *testing.T, server *jwksServer, audience string) *Verifier {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	verifier, err := New(ctx, Config{
		Issuer:        testIssuer,
		JWKSURL:       server.URL,
		Audience:      audience,
		HTTPClient:    server.Client(),
		LookupTimeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatalf("construct verifier: %v", err)
	}
	return verifier
}

func TestVerifyAcceptsValidToken(t *testing.T) {
	_, key, server := newSigningKey(t)
	verifier := newVerifier(t, server, "")

	token := mint(t, key, tokenOptions{name: "Ada Lovelace"})

	identity, err := verifier.Verify(context.Background(), token)
	if err != nil {
		t.Fatalf("Verify() returned error: %v", err)
	}
	if identity.Subject != testSubject {
		t.Errorf("Subject = %q, want %q", identity.Subject, testSubject)
	}
	if identity.Email != testEmail {
		t.Errorf("Email = %q, want %q", identity.Email, testEmail)
	}
	if identity.DisplayName != "Ada Lovelace" {
		t.Errorf("DisplayName = %q, want %q", identity.DisplayName, "Ada Lovelace")
	}
	if identity.ExpiresAt.IsZero() {
		t.Error("ExpiresAt is zero, want the token expiry")
	}
}

func TestVerifyRejects(t *testing.T) {
	_, key, server := newSigningKey(t)

	// A second, unrelated key stands in for a forged signature: correctly
	// formed, signed by something the issuer never published.
	_, foreignKey, _ := newSigningKey(t)

	tests := []struct {
		name     string
		audience string
		token    func(t *testing.T) string
	}{
		{
			name:  "expired token",
			token: func(t *testing.T) string { return mint(t, key, tokenOptions{expiresAt: time.Now().Add(-time.Hour)}) },
		},
		{
			name:  "wrong issuer",
			token: func(t *testing.T) string { return mint(t, key, tokenOptions{issuer: "https://attacker.example"}) },
		},
		{
			name:     "wrong audience",
			audience: testAudience,
			token:    func(t *testing.T) string { return mint(t, key, tokenOptions{audience: "some-other-service"}) },
		},
		{
			name:     "missing audience when one is required",
			audience: testAudience,
			token:    func(t *testing.T) string { return mint(t, key, tokenOptions{}) },
		},
		{
			name:  "signed by an unpublished key",
			token: func(t *testing.T) string { return mint(t, foreignKey, tokenOptions{}) },
		},
		{
			name: "tampered payload",
			token: func(t *testing.T) string {
				parts := strings.Split(mint(t, key, tokenOptions{}), ".")
				// Corrupt the payload while leaving the signature intact.
				parts[1] = "eyJzdWIiOiJ1c2VyX2F0dGFja2VyIn0"
				return strings.Join(parts, ".")
			},
		},
		{
			name:  "malformed token",
			token: func(t *testing.T) string { return "not-a-jwt" },
		},
		{
			name:  "empty token",
			token: func(t *testing.T) string { return "" },
		},
		{
			name:  "token with no email claim",
			token: func(t *testing.T) string { return mint(t, key, tokenOptions{omitEmail: true}) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			verifier := newVerifier(t, server, tt.audience)

			identity, err := verifier.Verify(context.Background(), tt.token(t))
			if err == nil {
				t.Fatalf("Verify() accepted the token, returning %+v", identity)
			}
			if errors.Is(err, application.ErrVerifierUnavailable) {
				t.Errorf("Verify() reported a dependency failure, want a credential failure: %v", err)
			}
		})
	}
}

// TestVerifyRejectsAlgorithmConfusion covers the attack where a token is
// signed with HMAC using the RSA public key as the shared secret, in the hope
// that the verifier infers the algorithm from the token rather than pinning it.
func TestVerifyRejectsAlgorithmConfusion(t *testing.T) {
	_, key, server := newSigningKey(t)
	verifier := newVerifier(t, server, "")

	public, err := jwk.PublicKeyOf(key)
	if err != nil {
		t.Fatalf("derive public key: %v", err)
	}
	publicJSON, err := json.Marshal(public)
	if err != nil {
		t.Fatalf("marshal public key: %v", err)
	}

	hmacKey, err := jwk.Import(publicJSON)
	if err != nil {
		t.Fatalf("import hmac key: %v", err)
	}
	if err := hmacKey.Set(jwk.KeyIDKey, testKeyID); err != nil {
		t.Fatalf("set kid: %v", err)
	}

	token, err := jwt.NewBuilder().
		Issuer(testIssuer).
		Subject(testSubject).
		IssuedAt(time.Now()).
		Expiration(time.Now().Add(time.Hour)).
		Claim("email", testEmail).
		Build()
	if err != nil {
		t.Fatalf("build token: %v", err)
	}

	signed, err := jwt.Sign(token, jwt.WithKey(jwa.HS256(), hmacKey))
	if err != nil {
		// Some versions refuse to sign this at all, which is itself a pass.
		t.Skipf("could not construct an HMAC-signed token: %v", err)
	}

	if _, err := verifier.Verify(context.Background(), string(signed)); err == nil {
		t.Fatal("Verify() accepted an HMAC-signed token, allowing algorithm confusion")
	}
}

// TestVerifyCachesKeySet proves the key set is not re-fetched per request.
func TestVerifyCachesKeySet(t *testing.T) {
	_, key, server := newSigningKey(t)
	verifier := newVerifier(t, server, "")

	for range 5 {
		if _, err := verifier.Verify(context.Background(), mint(t, key, tokenOptions{})); err != nil {
			t.Fatalf("Verify() returned error: %v", err)
		}
	}

	if fetches := server.fetches.Load(); fetches != 1 {
		t.Errorf("JWKS fetched %d times across 5 verifications, want 1", fetches)
	}
}

// TestVerifyReportsUnavailableKeySet proves an unreachable key set surfaces as
// a dependency failure and never as a silently accepted token.
func TestVerifyReportsUnavailableKeySet(t *testing.T) {
	_, key, server := newSigningKey(t)
	token := mint(t, key, tokenOptions{})

	// Take the key set away before the first fetch.
	server.Close()

	verifier := newVerifier(t, server, "")

	_, err := verifier.Verify(context.Background(), token)
	if err == nil {
		t.Fatal("Verify() accepted a token without reachable key material")
	}
	if !errors.Is(err, application.ErrVerifierUnavailable) {
		t.Errorf("error = %v, want application.ErrVerifierUnavailable", err)
	}
}

func TestNewRequiresIssuer(t *testing.T) {
	if _, err := New(context.Background(), Config{}); err == nil {
		t.Fatal("New() accepted an empty issuer")
	}
}

// TestNewDoesNotFetchAtStartup proves construction never depends on the
// identity provider being reachable.
func TestNewDoesNotFetchAtStartup(t *testing.T) {
	_, _, server := newSigningKey(t)

	if _, err := New(context.Background(), Config{
		Issuer:     testIssuer,
		JWKSURL:    server.URL,
		HTTPClient: server.Client(),
	}); err != nil {
		t.Fatalf("New() returned error: %v", err)
	}

	if fetches := server.fetches.Load(); fetches != 0 {
		t.Errorf("JWKS fetched %d times during construction, want 0", fetches)
	}
}
