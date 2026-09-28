// Package config reads and validates the runner manager's environment.
//
// The runner manager is the only process that holds a runner backend's
// credentials — the Docker socket today, a vendor's API key from M5.4b — and
// the only one besides the API that mints GitHub tokens, which it does for
// clones. It holds no Clerk issuer and no webhook secret.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config is what the runner manager needs to run.
type Config struct {
	AppEnv           string
	AppDatabaseURL   string
	TemporalHostPort string
	HealthAddr       string

	// RedisURL, GitHubAppID and GitHubAppPrivateKeyPath mint the
	// repository-scoped token each runner clones with.
	RedisURL                string
	GitHubAppID             string
	GitHubAppPrivateKeyPath string
	// GitBaseURL is where repositories are cloned from.
	GitBaseURL string

	// Backend names the RunnerBackend. Only "dev-docker" exists until M5.4b,
	// and it is refused in staging and production.
	Backend      string
	DockerSocket string
	RunnerImage  string
	// RunnerScope labels this manager's runners, so two managers — a
	// developer's and a test's — never reconcile each other's away.
	RunnerScope string
}

// Load reads the environment, reporting every missing value at once.
func Load() (Config, error) {
	get := func(key, fallback string) string {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
		return fallback
	}
	cfg := Config{
		// Required, with no default: the dev backend is allowed only in
		// development and test, and a deployment that forgot to set this
		// must fail to start rather than pass as development.
		AppEnv:                  get("APP_ENV", ""),
		AppDatabaseURL:          get("APP_DATABASE_URL", ""),
		TemporalHostPort:        get("TEMPORAL_HOST_PORT", ""),
		HealthAddr:              get("RUNNER_MANAGER_HEALTH_ADDR", ":8092"),
		RedisURL:                get("REDIS_URL", ""),
		GitHubAppID:             get("GITHUB_APP_ID", ""),
		GitHubAppPrivateKeyPath: get("GITHUB_APP_PRIVATE_KEY_PATH", ""),
		GitBaseURL:              strings.TrimSuffix(get("RUNNER_GIT_BASE_URL", "https://github.com"), "/"),
		Backend:                 get("RUNNER_BACKEND", "dev-docker"),
		DockerSocket:            get("RUNNER_DOCKER_SOCKET", "/var/run/docker.sock"),
		RunnerImage:             get("RUNNER_IMAGE", "weave-runner:dev"),
		RunnerScope:             get("RUNNER_SCOPE", "dev"),
	}

	var missing []string
	for key, value := range map[string]string{
		"APP_ENV": cfg.AppEnv, "APP_DATABASE_URL": cfg.AppDatabaseURL, "TEMPORAL_HOST_PORT": cfg.TemporalHostPort,
		"REDIS_URL": cfg.RedisURL, "GITHUB_APP_ID": cfg.GitHubAppID,
		"GITHUB_APP_PRIVATE_KEY_PATH": cfg.GitHubAppPrivateKeyPath,
	} {
		if value == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	return cfg, nil
}
