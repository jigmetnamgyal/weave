// Package vercelsandboxtest is a stand-in for Vercel's sandbox OIDC issuer,
// for tests only: its own RSA key, served as a JWKS at the path Vercel uses,
// and tokens minted with the claims Vercel's carry (measured in M5.4c's spike).
package vercelsandboxtest

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/lestrrat-go/jwx/v3/jwa"
	"github.com/lestrrat-go/jwx/v3/jwk"
	"github.com/lestrrat-go/jwx/v3/jwt"
)

// Issuer serves a key set for TeamID and mints tokens with its key.
type Issuer struct {
	Server *httptest.Server
	TeamID string
	key    jwk.Key
}

// NewIssuer starts an issuer for a team, closed when the test ends.
func NewIssuer(t *testing.T, teamID string) *Issuer {
	t.Helper()
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	private, err := jwk.Import(raw)
	if err != nil {
		t.Fatal(err)
	}
	_ = private.Set(jwk.KeyIDKey, "test-key")
	_ = private.Set(jwk.AlgorithmKey, jwa.RS256())
	public, err := jwk.PublicKeyOf(private)
	if err != nil {
		t.Fatal(err)
	}
	set := jwk.NewSet()
	_ = set.AddKey(public)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /"+teamID+"/.well-known/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(set)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return &Issuer{Server: server, TeamID: teamID, key: private}
}

// IssuerURL is what the verifier expects in `iss`.
func (i *Issuer) IssuerURL() string { return i.Server.URL + "/" + i.TeamID }

// Token describes one token; zero fields take Vercel-like defaults.
type Token struct {
	Issuer      string
	Audience    []string
	ProjectID   string
	SandboxID   string
	SandboxName string
	Expires     time.Time
	// Key signs with another key when set, for forged tokens.
	Key jwk.Key
}

// Mint signs a token.
func (i *Issuer) Mint(t *testing.T, spec Token) string {
	t.Helper()
	if spec.Issuer == "" {
		spec.Issuer = i.IssuerURL()
	}
	if spec.SandboxID == "" {
		spec.SandboxID = "sbx_test"
	}
	if spec.Expires.IsZero() {
		spec.Expires = time.Now().Add(24 * time.Hour)
	}
	key := i.key
	if spec.Key != nil {
		key = spec.Key
	}
	builder := jwt.NewBuilder().Issuer(spec.Issuer).Audience(spec.Audience).
		IssuedAt(time.Now()).NotBefore(time.Now().Add(-time.Second)).Expiration(spec.Expires).
		Subject("team:"+i.TeamID+":project:"+spec.ProjectID+":sandbox:"+spec.SandboxID).
		Claim("project_id", spec.ProjectID).Claim("sandbox_id", spec.SandboxID).
		Claim("sandbox_name", spec.SandboxName).Claim("team_id", i.TeamID)
	token, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	signed, err := jwt.Sign(token, jwt.WithKey(jwa.RS256(), key))
	if err != nil {
		t.Fatal(err)
	}
	return string(signed)
}

// ForeignKey is a key the issuer does not publish, for forged tokens.
func ForeignKey(t *testing.T) jwk.Key {
	t.Helper()
	raw, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	key, err := jwk.Import(raw)
	if err != nil {
		t.Fatal(err)
	}
	_ = key.Set(jwk.KeyIDKey, "test-key")
	return key
}
