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
}

// requiredKeys are environment variables that have no safe default. Every one
// of them is listed in .env.example.
var requiredKeys = []string{
	"DATABASE_URL",
	"REDIS_URL",
	"NATS_URL",
	"TEMPORAL_HOST_PORT",
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

	cfg := Config{
		AppEnv:           valueOr("APP_ENV", "development"),
		HTTPAddr:         valueOr("API_HTTP_ADDR", ":8080"),
		DatabaseURL:      os.Getenv("DATABASE_URL"),
		RedisURL:         os.Getenv("REDIS_URL"),
		NATSURL:          os.Getenv("NATS_URL"),
		TemporalHostPort: os.Getenv("TEMPORAL_HOST_PORT"),
		OTelExporter:     valueOr("OTEL_EXPORTER", "none"),
		OTelServiceName:  valueOr("OTEL_SERVICE_NAME", "weave-api"),
		ReadinessTimeout: 3 * time.Second,
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
