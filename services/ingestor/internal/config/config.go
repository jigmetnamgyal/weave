// Package config reads and validates the ingestor's environment.
//
// Its own package, like the worker's, so the ingestor fails to start only over
// what it actually uses — and holds nothing it does not: no GitHub key, no
// Clerk issuer, no Temporal address.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config is what the ingestor needs to run.
type Config struct {
	// AppDatabaseURL authenticates as the application role, so row-level
	// security applies. The one cross-tenant read — resolving a session's
	// workspace — goes through a bounded SECURITY DEFINER function.
	AppDatabaseURL string
	// NATSURL is the JetStream server events arrive on.
	NATSURL string
	// HealthAddr serves liveness and readiness for the deployment system.
	HealthAddr string
	// AppEnv names the environment in logs.
	AppEnv string
}

// Load reads the environment, reporting every missing value at once.
func Load() (Config, error) {
	cfg := Config{
		AppDatabaseURL: strings.TrimSpace(os.Getenv("APP_DATABASE_URL")),
		NATSURL:        strings.TrimSpace(os.Getenv("NATS_URL")),
		HealthAddr:     strings.TrimSpace(os.Getenv("INGESTOR_HEALTH_ADDR")),
		AppEnv:         strings.TrimSpace(os.Getenv("APP_ENV")),
	}

	var missing []string
	if cfg.AppDatabaseURL == "" {
		missing = append(missing, "APP_DATABASE_URL")
	}
	if cfg.NATSURL == "" {
		missing = append(missing, "NATS_URL")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	if cfg.AppEnv == "" {
		cfg.AppEnv = "development"
	}
	if cfg.HealthAddr == "" {
		cfg.HealthAddr = ":8091"
	}
	return cfg, nil
}
