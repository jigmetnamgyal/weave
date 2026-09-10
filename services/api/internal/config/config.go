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
	// DatabaseURL is the PostgreSQL connection string.
	DatabaseURL string
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
	// ClerkJWTAudience, when set, is required in the token's `aud` claim.
	// Clerk's default session token has no audience; one appears only when a
	// JWT template sets it, so enforcement is opt-in.
	ClerkJWTAudience string
}

// requiredKeys are environment variables that have no safe default. Every one
// of them is listed in .env.example.
var requiredKeys = []string{
	"DATABASE_URL",
	"REDIS_URL",
	"NATS_URL",
	"TEMPORAL_HOST_PORT",
	// The API needs the issuer to know which tokens to trust and where to
	// fetch signing keys. It never needs CLERK_SECRET_KEY: verifying a
	// signature requires only public keys, so that secret stays with the web
	// application and out of this process entirely.
	"CLERK_ISSUER",
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
	cfg := Config{
		AppEnv:           valueOr("APP_ENV", "development"),
		HTTPAddr:         valueOr("API_HTTP_ADDR", ":8080"),
		DatabaseURL:      strings.TrimSpace(os.Getenv("DATABASE_URL")),
		RedisURL:         strings.TrimSpace(os.Getenv("REDIS_URL")),
		NATSURL:          strings.TrimSpace(os.Getenv("NATS_URL")),
		TemporalHostPort: strings.TrimSpace(os.Getenv("TEMPORAL_HOST_PORT")),
		OTelExporter:     valueOr("OTEL_EXPORTER", "none"),
		OTelServiceName:  valueOr("OTEL_SERVICE_NAME", "weave-api"),
		ReadinessTimeout: 3 * time.Second,
		ClerkIssuer:      strings.TrimSpace(os.Getenv("CLERK_ISSUER")),
		ClerkJWTAudience: strings.TrimSpace(os.Getenv("CLERK_JWT_AUDIENCE")),
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
