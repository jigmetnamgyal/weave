// Package config reads and validates the worker's environment.
//
// Separate from the API's config rather than shared, because the two processes
// need different things and sharing would mean the worker failing to start over
// a Clerk key it never uses. The shape is the same on purpose: everything is
// read once at startup and every missing value is reported together, so a
// misconfigured deployment fails immediately with the whole list rather than
// one variable at a time.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config is what the worker needs to run.
type Config struct {
	// AppDatabaseURL authenticates as the application role, so row-level
	// security applies to this process too. The one cross-workspace read the
	// publisher needs goes through a SECURITY DEFINER function rather than
	// through a privileged connection.
	AppDatabaseURL string
	// TemporalHostPort is the Temporal frontend. Unlike the API, this process
	// cannot do its job without it.
	TemporalHostPort string
	// HealthAddr is where liveness and readiness are served. The worker
	// handles no product traffic, so this is for the deployment system.
	HealthAddr string
	// AppEnv names the environment in logs.
	AppEnv string
}

// Load reads the environment, reporting every missing value at once.
func Load() (Config, error) {
	cfg := Config{
		AppDatabaseURL:   strings.TrimSpace(os.Getenv("APP_DATABASE_URL")),
		TemporalHostPort: strings.TrimSpace(os.Getenv("TEMPORAL_HOST_PORT")),
		HealthAddr:       strings.TrimSpace(os.Getenv("WORKER_HEALTH_ADDR")),
		AppEnv:           strings.TrimSpace(os.Getenv("APP_ENV")),
	}

	var missing []string
	if cfg.AppDatabaseURL == "" {
		missing = append(missing, "APP_DATABASE_URL")
	}
	if cfg.TemporalHostPort == "" {
		missing = append(missing, "TEMPORAL_HOST_PORT")
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment variables: %s",
			strings.Join(missing, ", "))
	}

	if cfg.AppEnv == "" {
		cfg.AppEnv = "development"
	}
	// Defaulted rather than required: a worker with no health address would
	// still work, and failing to start over it would be worse than serving it
	// somewhere predictable.
	if cfg.HealthAddr == "" {
		cfg.HealthAddr = ":8090"
	}
	return cfg, nil
}
