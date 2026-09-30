// Package config reads and validates the registry proxy's environment.
//
// The proxy (Unit M5.4c, ADR-016) holds no credential of its own beyond its
// database role: it authenticates sandboxes by the token Vercel signs, which
// needs only the team and project ids — public identifiers, not secrets.
package config

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// Config is what the registry proxy needs to run.
type Config struct {
	AppEnv         string
	AppDatabaseURL string
	// Addr is the public listener Vercel forwards to (through TLS at Weave's
	// edge in production, a tunnel in development).
	Addr string
	// HealthAddr serves liveness and readiness, apart from the public
	// listener.
	HealthAddr string
	// PublicURL is this proxy's public base URL, exactly as the runner manager
	// puts it in forwardURL (RUNNER_REGISTRY_PROXY_URL): every token's
	// audience is checked against it.
	PublicURL string
	// VercelTeamID and VercelProjectID say whose sandboxes may use the proxy:
	// the token's issuer is the team's, and its project must be this one.
	VercelTeamID    string
	VercelProjectID string
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
		AppEnv:          get("APP_ENV", ""),
		AppDatabaseURL:  get("APP_DATABASE_URL", ""),
		Addr:            get("REGISTRY_PROXY_ADDR", ":8095"),
		HealthAddr:      get("REGISTRY_PROXY_HEALTH_ADDR", ":8096"),
		PublicURL:       strings.TrimSuffix(get("REGISTRY_PROXY_PUBLIC_URL", ""), "/"),
		VercelTeamID:    get("REGISTRY_PROXY_VERCEL_TEAM_ID", ""),
		VercelProjectID: get("REGISTRY_PROXY_VERCEL_PROJECT_ID", ""),
	}
	var missing []string
	for key, value := range map[string]string{
		"APP_ENV": cfg.AppEnv, "APP_DATABASE_URL": cfg.AppDatabaseURL,
		"REGISTRY_PROXY_PUBLIC_URL":        cfg.PublicURL,
		"REGISTRY_PROXY_VERCEL_TEAM_ID":    cfg.VercelTeamID,
		"REGISTRY_PROXY_VERCEL_PROJECT_ID": cfg.VercelProjectID,
	} {
		if value == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if err := application.ValidateRegistryProxyURL(cfg.PublicURL); err != nil {
		return Config{}, fmt.Errorf("REGISTRY_PROXY_PUBLIC_URL: %w", err)
	}
	return cfg, nil
}
