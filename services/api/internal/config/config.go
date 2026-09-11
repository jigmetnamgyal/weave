// Package config loads and validates the API service configuration from the
// environment. Loading fails fast at startup: a missing required variable is
// an unrecoverable configuration error, never a silent default.
package config

import (
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Config holds the validated runtime configuration for the API service.
type Config struct {
	// AppEnv names the deployment environment (development, test, staging, production).
	AppEnv string
	// HTTPAddr is the listen address for the HTTP server.
	HTTPAddr string
	// DatabaseURL is the PostgreSQL connection string used for readiness
	// probing. Migrations use it too, from their own process.
	DatabaseURL string
	// AppDatabaseURL is the connection the API serves requests on.
	//
	// Deliberately separate from DatabaseURL: it authenticates as a role that
	// does not own the tables, so row-level security applies to it. An owner
	// bypasses policies, and a single URL would make reverting to the owner a
	// one-character change that nothing would catch.
	AppDatabaseURL string
	// RedisURL is the Redis connection string.
	RedisURL string
	// NATSURL is the NATS JetStream connection string.
	NATSURL string
	// TemporalHostPort is the Temporal frontend address.
	TemporalHostPort string
	// OTelExporter selects the trace exporter: "none" or "stdout".
	OTelExporter string
	// OTelServiceName is the service.name resource attribute.
	OTelServiceName string
	// ReadinessTimeout bounds the total time a readiness probe may take.
	ReadinessTimeout time.Duration
	// ClerkIssuer is the expected `iss` claim on session tokens, and the base
	// URL the signing key set is discovered from.
	ClerkIssuer string
	// GitHubAppID is the numeric App id, used as the `iss` claim when minting
	// the App JWT that authenticates as the App itself.
	GitHubAppID string
	// GitHubAppSlug is the App's URL slug, used to build the install link
	// https://github.com/apps/<slug>/installations/new.
	GitHubAppSlug string
	// GitHubAppPrivateKeyPath points at the PEM private key. A path rather
	// than the key itself: the key is the root credential for every
	// installation, and a path keeps it out of the environment, out of process
	// listings, and out of anything that dumps configuration.
	GitHubAppPrivateKeyPath string
	// GitHubWebhookSecret is the shared secret GitHub signs deliveries with.
	GitHubWebhookSecret string
	// GitHubWebhookProxyURL is where deliveries are relayed from in
	// development, because GitHub cannot reach localhost. Empty in production,
	// where the API is reachable directly, so it is deliberately not required.
	GitHubWebhookProxyURL string
	// ClerkJWTAudience, when set, is required in the token's `aud` claim.
	// Clerk's default session token has no audience; one appears only when a
	// JWT template sets it, so enforcement is opt-in.
	ClerkJWTAudience string
}

// requiredKeys are environment variables that have no safe default. Every one
// of them is listed in .env.example.
var requiredKeys = []string{
	"DATABASE_URL",
	"APP_DATABASE_URL",
	"REDIS_URL",
	"NATS_URL",
	"TEMPORAL_HOST_PORT",
	// The API needs the issuer to know which tokens to trust and where to
	// fetch signing keys. It never needs CLERK_SECRET_KEY: verifying a
	// signature requires only public keys, so that secret stays with the web
	// application and out of this process entirely.
	"CLERK_ISSUER",
	// The GitHub App. The private key is required as a path, and read at
	// startup rather than lazily: a missing key is a misconfiguration, and the
	// moment to discover it is deployment, not the first time someone tries to
	// connect a repository.
	"GITHUB_APP_ID",
	"GITHUB_APP_SLUG",
	"GITHUB_APP_PRIVATE_KEY_PATH",
	"GITHUB_APP_WEBHOOK_SECRET",
}

// validExporters are the accepted values for OTEL_EXPORTER.
var validExporters = map[string]bool{
	"none":   true,
	"stdout": true,
}

// ErrMissingConfig is returned when one or more required variables are absent.
var ErrMissingConfig = errors.New("missing required configuration")

// Load reads configuration from the process environment and validates it.
// It reports every problem it finds at once rather than one per run, so a
// developer fixes a broken environment in a single pass.
func Load() (Config, error) {
	var missing []string
	for _, key := range requiredKeys {
		if strings.TrimSpace(os.Getenv(key)) == "" {
			missing = append(missing, key)
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		return Config{}, fmt.Errorf(
			"%w: %s. Copy .env.example to .env and set these values, or export them in your shell",
			ErrMissingConfig,
			strings.Join(missing, ", "),
		)
	}

	// Required values are trimmed for the same reason optional ones are: a
	// value pasted into .env with surrounding whitespace passed the blank check
	// above but would then be stored raw, and redis.ParseURL and pgxpool.New
	// both reject a URL whose scheme is not at the start of the string.
	// Placeholder values are refused rather than accepted.
	//
	// .env.example must carry non-empty values so the required-key check has
	// something to pass on in CI, but a template copied unchanged would then
	// start an API whose webhook secret is a string published in this
	// repository — and anyone could forge a signed delivery. Empty fails the
	// check above; this fails the ones that look filled in but are not.
	for _, key := range []string{"GITHUB_APP_WEBHOOK_SECRET", "GITHUB_APP_SLUG", "GITHUB_APP_ID"} {
		if isPlaceholder(os.Getenv(key)) {
			return Config{}, fmt.Errorf(
				"%s still holds a placeholder from .env.example. Set a real value; see .env.example for where it comes from", key)
		}
	}

	cfg := Config{
		AppEnv:           valueOr("APP_ENV", "development"),
		HTTPAddr:         valueOr("API_HTTP_ADDR", ":8080"),
		DatabaseURL:      strings.TrimSpace(os.Getenv("DATABASE_URL")),
		AppDatabaseURL:   strings.TrimSpace(os.Getenv("APP_DATABASE_URL")),
		RedisURL:         strings.TrimSpace(os.Getenv("REDIS_URL")),
		NATSURL:          strings.TrimSpace(os.Getenv("NATS_URL")),
		TemporalHostPort: strings.TrimSpace(os.Getenv("TEMPORAL_HOST_PORT")),
		OTelExporter:     valueOr("OTEL_EXPORTER", "none"),
		OTelServiceName:  valueOr("OTEL_SERVICE_NAME", "weave-api"),
		ReadinessTimeout: 3 * time.Second,
		ClerkIssuer:      strings.TrimSpace(os.Getenv("CLERK_ISSUER")),
		ClerkJWTAudience: strings.TrimSpace(os.Getenv("CLERK_JWT_AUDIENCE")),

		GitHubAppID:             strings.TrimSpace(os.Getenv("GITHUB_APP_ID")),
		GitHubAppSlug:           strings.TrimSpace(os.Getenv("GITHUB_APP_SLUG")),
		GitHubAppPrivateKeyPath: strings.TrimSpace(os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH")),
		GitHubWebhookSecret:     strings.TrimSpace(os.Getenv("GITHUB_APP_WEBHOOK_SECRET")),
		GitHubWebhookProxyURL:   strings.TrimSpace(os.Getenv("GITHUB_WEBHOOK_PROXY_URL")),
	}

	if !validExporters[cfg.OTelExporter] {
		return Config{}, fmt.Errorf(
			"invalid OTEL_EXPORTER %q: expected one of none, stdout",
			cfg.OTelExporter,
		)
	}

	return cfg, nil
}

// valueOr returns the trimmed environment value for key, or fallback when it
// is unset or blank.
func valueOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// placeholders are the template values .env.example ships with.
var placeholders = map[string]bool{
	"replace-me": true,
	"000000":     true,
	"changeme":   true,
	"secret":     true,
}

// isPlaceholder reports whether a value is one of the template's own.
func isPlaceholder(value string) bool {
	return placeholders[strings.ToLower(strings.TrimSpace(value))]
}
