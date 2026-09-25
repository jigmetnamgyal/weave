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

	// RedisURL is where installation tokens are cached. Required since M5.2,
	// when the workflow began creating branches: without the cache every
	// activity would mint a fresh token, and GitHub rate-limits minting.
	RedisURL string
	// GitHubAppID and GitHubAppPrivateKeyPath authenticate as the App, which is
	// how an installation token is minted. The same values the API holds, and
	// the key is read at startup for the same reason: a missing key is a
	// deployment mistake, not something to discover on the first session.
	//
	// Not the webhook secret or the slug. This process neither receives
	// deliveries nor builds install links, and holding a secret it never uses
	// is only one more place for it to leak from.
	GitHubAppID             string
	GitHubAppPrivateKeyPath string
}

// Load reads the environment, reporting every missing value at once.
func Load() (Config, error) {
	cfg := Config{
		AppDatabaseURL:   strings.TrimSpace(os.Getenv("APP_DATABASE_URL")),
		TemporalHostPort: strings.TrimSpace(os.Getenv("TEMPORAL_HOST_PORT")),
		HealthAddr:       strings.TrimSpace(os.Getenv("WORKER_HEALTH_ADDR")),
		AppEnv:           strings.TrimSpace(os.Getenv("APP_ENV")),

		RedisURL:                strings.TrimSpace(os.Getenv("REDIS_URL")),
		GitHubAppID:             strings.TrimSpace(os.Getenv("GITHUB_APP_ID")),
		GitHubAppPrivateKeyPath: strings.TrimSpace(os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH")),
	}

	var missing []string
	if cfg.AppDatabaseURL == "" {
		missing = append(missing, "APP_DATABASE_URL")
	}
	if cfg.TemporalHostPort == "" {
		missing = append(missing, "TEMPORAL_HOST_PORT")
	}
	if cfg.RedisURL == "" {
		missing = append(missing, "REDIS_URL")
	}
	if cfg.GitHubAppID == "" {
		missing = append(missing, "GITHUB_APP_ID")
	}
	if cfg.GitHubAppPrivateKeyPath == "" {
		missing = append(missing, "GITHUB_APP_PRIVATE_KEY_PATH")
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
