// Package config reads and validates the runner manager's environment.
//
// The runner manager is the only process that holds a runner backend's
// credentials — the Docker socket today, a vendor's API key from M5.4b — and
// the only one besides the API that mints GitHub tokens, which it does for
// clones. It holds no Clerk issuer and no webhook secret.
package config

import (
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/jigmetnamgyal/weave/internal/adapters/natsauth"
	"github.com/jigmetnamgyal/weave/internal/application"
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

	// Backend names the RunnerBackend: "dev-docker", refused outside
	// development and test, or "vercel" (M5.4b).
	Backend      string
	DockerSocket string
	RunnerImage  string
	// RunnerScope labels this manager's runners, so two managers — a
	// developer's and a test's — never reconcile each other's away.
	RunnerScope string

	// Vercel is the Vercel Sandbox backend's configuration (M5.4b), read
	// only when Backend is "vercel".
	Vercel VercelConfig

	// RegistryProxyURL is the registry proxy's public base URL (M5.4c,
	// ADR-016): every registry request from a runner is forwarded there and
	// recorded. Required outside development and test — unset, registry
	// traffic would pass unrecorded, the gap the proxy exists to close.
	RegistryProxyURL string

	// EgressProxyURL is the egress proxy's public base URL (M5.4d.3a,
	// ADR-017): a workspace's added hosts are forwarded there and nowhere
	// else. Required outside development and test; where unset, a runner with
	// any added host fails to provision rather than reaching it directly.
	EgressProxyURL string
	// EgressReserved are the namespaces no added host may fall under — the
	// deployment's declaration and its public service hosts — re-checked at
	// every provisioning against the runner's immutable snapshot.
	EgressReserved []string

	// NATSSigningKey and NATSAccount mint each runner's broker credential
	// (M5.5a, ADR-014). The signing key is a secret, read from a path; this
	// is the only process that holds one.
	NATSSigningKey string
	NATSAccount    string
	// RunnerNATSURL is the broker as a runner reaches it — from inside a
	// container, not from this host.
	RunnerNATSURL string
	// NATSURL and NATSCreds are this process's own connection (M5.5b), used
	// only for the drain check: its identity may read the event stream's
	// information and nothing else (ADR-014).
	NATSURL   string
	NATSCreds string
}

// VercelConfig is what the Vercel backend needs. The token is a secret: it
// can create sandboxes that reach the internet, lives only in the runner
// manager (like the NATS signing key), and is never logged.
type VercelConfig struct {
	Token     string
	TeamID    string
	ProjectID string
	// Region is where sandboxes run: iad1 until Open Question 8 (initial
	// hosting region) is answered.
	Region string
	// MaxSession is the plan's cap on one sandbox's life: 45m on Hobby, 24h
	// on Pro. Stated, never defaulted: it is a fact about the account.
	MaxSession time.Duration
	// Lease overrides the backend's default lease; zero keeps it.
	Lease time.Duration
	// RunnerBinary is the runner built for the sandbox (linux/amd64), by
	// `make runner-binary`.
	RunnerBinary string
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

	var err error
	if cfg.AppEnv != "" {
		if cfg.NATSSigningKey, err = natsauth.ResolvePath(cfg.AppEnv, get("RUNNER_NATS_SIGNING_KEY", ""), natsauth.RunnerSigningFile); err != nil {
			return Config{}, fmt.Errorf("RUNNER_NATS_SIGNING_KEY: %w", err)
		}
		if cfg.NATSAccount, err = natsauth.ResolvePath(cfg.AppEnv, get("RUNNER_NATS_ACCOUNT", ""), natsauth.AccountPublicFile); err != nil {
			return Config{}, fmt.Errorf("RUNNER_NATS_ACCOUNT: %w", err)
		}
	}
	cfg.RunnerNATSURL = get("RUNNER_NATS_URL", runnerReachable(get("NATS_WEBSOCKET_URL", "")))
	cfg.NATSURL = get("NATS_URL", "")
	if cfg.AppEnv != "" {
		if cfg.NATSCreds, err = natsauth.ResolvePath(cfg.AppEnv, get("RUNNER_MANAGER_NATS_CREDS", ""),
			string(natsauth.IdentityRunnerManager)+".creds"); err != nil {
			return Config{}, fmt.Errorf("RUNNER_MANAGER_NATS_CREDS: %w", err)
		}
	}

	var missing []string
	for key, value := range map[string]string{
		"APP_ENV": cfg.AppEnv, "APP_DATABASE_URL": cfg.AppDatabaseURL, "TEMPORAL_HOST_PORT": cfg.TemporalHostPort,
		"REDIS_URL": cfg.RedisURL, "GITHUB_APP_ID": cfg.GitHubAppID,
		"GITHUB_APP_PRIVATE_KEY_PATH":             cfg.GitHubAppPrivateKeyPath,
		"RUNNER_NATS_URL (or NATS_WEBSOCKET_URL)": cfg.RunnerNATSURL,
		"NATS_URL": cfg.NATSURL,
	} {
		if value == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if err := validateRunnerNATSURL(cfg.AppEnv, cfg.RunnerNATSURL); err != nil {
		return Config{}, err
	}
	cfg.RegistryProxyURL = strings.TrimSuffix(get("RUNNER_REGISTRY_PROXY_URL", ""), "/")
	if err := validateRegistryProxy(cfg.AppEnv, cfg.RegistryProxyURL); err != nil {
		return Config{}, err
	}
	if cfg.Backend == "vercel" {
		if cfg.Vercel, err = loadVercel(cfg.AppEnv, get); err != nil {
			return Config{}, err
		}
	}
	cfg.EgressProxyURL = strings.TrimSuffix(get("RUNNER_EGRESS_PROXY_URL", ""), "/")
	if err := validateEgressProxy(cfg.AppEnv, cfg.EgressProxyURL); err != nil {
		return Config{}, err
	}
	urls := map[string]string{
		"RUNNER_NATS_URL": cfg.RunnerNATSURL, "RUNNER_REGISTRY_PROXY_URL": cfg.RegistryProxyURL,
		"RUNNER_EGRESS_PROXY_URL": cfg.EgressProxyURL,
		"API_BASE_URL":            get("API_BASE_URL", ""), "NEXT_PUBLIC_APP_URL": get("NEXT_PUBLIC_APP_URL", ""),
	}
	if cfg.EgressReserved, err = application.ParseEgressReservedConfig(strings.ToLower(cfg.AppEnv),
		get("EGRESS_RESERVED_HOSTS", ""), urls); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// validateEgressProxy holds ADR-017's rule at startup: outside development and
// test the egress proxy must be configured, so an added host is never granted
// by a plain rule. The URL follows the registry proxy's rules: https, no path.
func validateEgressProxy(appEnv, raw string) error {
	if raw == "" {
		switch strings.ToLower(strings.TrimSpace(appEnv)) {
		case "development", "test":
			return nil
		}
		return fmt.Errorf("RUNNER_EGRESS_PROXY_URL is required outside development and test (APP_ENV=%q): "+
			"workspace-added hosts are reachable only through it (ADR-017)", appEnv)
	}
	if err := application.ValidateRegistryProxyURL(raw); err != nil {
		return fmt.Errorf("RUNNER_EGRESS_PROXY_URL: %w", err)
	}
	return nil
}

// loadVercel reads the Vercel backend's configuration, reporting every
// missing value at once. The runner binary defaults to the `make` output in
// development and test only; anywhere else it is stated.
func loadVercel(appEnv string, get func(key, fallback string) string) (VercelConfig, error) {
	binaryDefault := ""
	switch strings.ToLower(strings.TrimSpace(appEnv)) {
	case "development", "test":
		binaryDefault = "bin/runner-linux-amd64"
	}
	v := VercelConfig{
		Token:        get("RUNNER_VERCEL_TOKEN", ""),
		TeamID:       get("RUNNER_VERCEL_TEAM_ID", ""),
		ProjectID:    get("RUNNER_VERCEL_PROJECT_ID", ""),
		Region:       get("RUNNER_VERCEL_REGION", "iad1"),
		RunnerBinary: get("RUNNER_VERCEL_BINARY", binaryDefault),
	}
	var missing []string
	for key, value := range map[string]string{
		"RUNNER_VERCEL_TOKEN": v.Token, "RUNNER_VERCEL_TEAM_ID": v.TeamID,
		"RUNNER_VERCEL_PROJECT_ID": v.ProjectID, "RUNNER_VERCEL_BINARY": v.RunnerBinary,
		"RUNNER_VERCEL_MAX_SESSION": get("RUNNER_VERCEL_MAX_SESSION", ""),
	} {
		if value == "" {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return VercelConfig{}, fmt.Errorf("RUNNER_BACKEND=vercel needs %s", strings.Join(missing, ", "))
	}
	var err error
	if v.MaxSession, err = time.ParseDuration(get("RUNNER_VERCEL_MAX_SESSION", "")); err != nil || v.MaxSession <= 0 {
		return VercelConfig{}, fmt.Errorf("RUNNER_VERCEL_MAX_SESSION must be a positive duration such as 45m or 24h")
	}
	if lease := get("RUNNER_VERCEL_LEASE", ""); lease != "" {
		if v.Lease, err = time.ParseDuration(lease); err != nil || v.Lease <= 0 {
			return VercelConfig{}, fmt.Errorf("RUNNER_VERCEL_LEASE must be a positive duration")
		}
	}
	return v, nil
}

// validateRunnerNATSURL holds ADR-015's rule at startup: a runner reaches NATS
// only through its WebSocket listener, and outside development and test only
// over TLS. A nats:// URL is refused rather than left to fail later — a runner
// credential cannot use the standard port, so every session would fail
// broker_refused, which reads like a credential fault rather than a
// misconfiguration.
func validateRunnerNATSURL(appEnv, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return fmt.Errorf("RUNNER_NATS_URL %q is not a URL", raw)
	}
	switch parsed.Scheme {
	case "wss":
		return nil
	case "ws":
		switch strings.ToLower(strings.TrimSpace(appEnv)) {
		case "development", "test":
			return nil
		}
		return fmt.Errorf("RUNNER_NATS_URL must be wss:// outside development and test (APP_ENV=%q): "+
			"runner events cross the internet (ADR-015)", appEnv)
	default:
		return fmt.Errorf("RUNNER_NATS_URL must be NATS's WebSocket listener (ws:// or wss://), not %q: "+
			"runner credentials are refused on the standard port (ADR-015)", parsed.Scheme+"://")
	}
}

// runnerReachable turns the host's NATS URL into one a local container can
// reach: a runner's "localhost" is its own sandbox, not the host.
//
// host.docker.internal reaches the host's loopback on Docker Desktop (macOS,
// Windows). On Linux it is the bridge gateway, which a loopback-published
// port does not answer; there RUNNER_NATS_URL must be set explicitly, with
// NATS published on the bridge — see .env.example. A runner that cannot reach
// the broker does not become ready (broker_refused), so the misconfiguration
// is loud rather than silent.
func runnerReachable(hostURL string) string {
	for _, local := range []string{"localhost", "127.0.0.1"} {
		if strings.Contains(hostURL, "://"+local+":") {
			return strings.Replace(hostURL, "://"+local+":", "://host.docker.internal:", 1)
		}
	}
	return hostURL
}

// validateRegistryProxy refuses to start without a registry proxy outside
// development and test, and refuses a proxy URL Vercel would not accept.
func validateRegistryProxy(appEnv, raw string) error {
	if raw == "" {
		switch strings.ToLower(strings.TrimSpace(appEnv)) {
		case "development", "test":
			return nil
		}
		return fmt.Errorf("RUNNER_REGISTRY_PROXY_URL is required outside development and test (APP_ENV=%q): "+
			"without it runners' registry requests are not recorded (ADR-016)", appEnv)
	}
	if err := application.ValidateRegistryProxyURL(raw); err != nil {
		return fmt.Errorf("RUNNER_REGISTRY_PROXY_URL: %w", err)
	}
	return nil
}
