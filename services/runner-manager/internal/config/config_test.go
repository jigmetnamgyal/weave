package config

import (
	"strings"
	"testing"
	"time"
)

// TestRunnersReachNATSOnlyOverWebSocket holds ADR-015 at startup: a runner's
// broker URL is the WebSocket listener, and TLS outside development and test.
func TestRunnersReachNATSOnlyOverWebSocket(t *testing.T) {
	for _, tc := range []struct {
		env, url string
		ok       bool
	}{
		{"development", "ws://host.docker.internal:54280", true},
		{"test", "ws://127.0.0.1:54280", true},
		{"development", "wss://events.example.trycloudflare.com", true},
		{"production", "wss://events.example.com", true},
		{"staging", "wss://events.example.com:443", true},
		// The standard port: a runner credential is refused there.
		{"development", "nats://host.docker.internal:54222", false},
		{"production", "tls://events.example.com:4222", false},
		// Plain WebSocket across the internet.
		{"production", "ws://events.example.com", false},
		{"staging", "ws://events.example.com", false},
		{"", "ws://events.example.com", false},
		{"development", "not a url", false},
	} {
		err := validateRunnerNATSURL(tc.env, tc.url)
		if (err == nil) != tc.ok {
			t.Errorf("APP_ENV=%q RUNNER_NATS_URL=%q: err = %v, want ok=%v", tc.env, tc.url, err, tc.ok)
		}
	}
}

// TestTheRunnerURLIsDerivedFromTheWebSocketListener: unset, the runner's URL
// comes from NATS_WEBSOCKET_URL as a container reaches it — never from
// NATS_URL, the standard port.
func TestTheRunnerURLIsDerivedFromTheWebSocketListener(t *testing.T) {
	for key, value := range map[string]string{
		"APP_ENV": "development", "APP_DATABASE_URL": "postgres://x", "TEMPORAL_HOST_PORT": "localhost:1",
		"REDIS_URL": "redis://x", "GITHUB_APP_ID": "1", "GITHUB_APP_PRIVATE_KEY_PATH": "/k",
		"NATS_URL": "nats://localhost:54222", "NATS_WEBSOCKET_URL": "ws://localhost:54280",
		"RUNNER_NATS_URL": "",
	} {
		t.Setenv(key, value)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.RunnerNATSURL != "ws://host.docker.internal:54280" {
		t.Errorf("RunnerNATSURL = %q, want the WebSocket listener as a container reaches it", cfg.RunnerNATSURL)
	}

	t.Setenv("NATS_WEBSOCKET_URL", "")
	if _, err := Load(); err == nil {
		t.Error("with no WebSocket URL the runner's URL fell back to something; it must fail to start")
	}
}

// TestTheVercelBackendNeedsItsAccountStated: token, team, project and the
// plan's cap are required, the cap never defaulted; the runner binary
// defaults only in development and test.
func TestTheVercelBackendNeedsItsAccountStated(t *testing.T) {
	base := map[string]string{
		"APP_ENV": "development", "APP_DATABASE_URL": "postgres://x", "TEMPORAL_HOST_PORT": "localhost:1",
		"REDIS_URL": "redis://x", "GITHUB_APP_ID": "1", "GITHUB_APP_PRIVATE_KEY_PATH": "/k",
		"NATS_URL": "nats://localhost:54222", "NATS_WEBSOCKET_URL": "ws://localhost:54280",
		"RUNNER_BACKEND": "vercel", "RUNNER_VERCEL_TOKEN": "", "RUNNER_VERCEL_TEAM_ID": "",
		"RUNNER_VERCEL_PROJECT_ID": "", "RUNNER_VERCEL_MAX_SESSION": "", "RUNNER_VERCEL_BINARY": "",
		"RUNNER_VERCEL_REGION": "", "RUNNER_VERCEL_LEASE": "", "RUNNER_NATS_URL": "", "RUNNER_REGISTRY_PROXY_URL": "",
	}
	set := func(overrides map[string]string) {
		for k, v := range base {
			t.Setenv(k, v)
		}
		for k, v := range overrides {
			t.Setenv(k, v)
		}
	}

	set(nil)
	_, err := Load()
	for _, want := range []string{"RUNNER_VERCEL_TOKEN", "RUNNER_VERCEL_TEAM_ID", "RUNNER_VERCEL_PROJECT_ID", "RUNNER_VERCEL_MAX_SESSION"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("missing %s not reported: %v", want, err)
		}
	}

	account := map[string]string{"RUNNER_VERCEL_TOKEN": "t", "RUNNER_VERCEL_TEAM_ID": "team",
		"RUNNER_VERCEL_PROJECT_ID": "prj", "RUNNER_VERCEL_MAX_SESSION": "45m",
		"RUNNER_NATS_URL": "wss://events.example.trycloudflare.com"}
	set(account)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Vercel.MaxSession != 45*time.Minute || cfg.Vercel.Region != "iad1" || cfg.Vercel.RunnerBinary != "bin/runner-linux-amd64" {
		t.Errorf("= %+v", cfg.Vercel)
	}

	account["RUNNER_VERCEL_MAX_SESSION"] = "forever"
	set(account)
	if _, err := Load(); err == nil {
		t.Error("an unparseable session cap was accepted")
	}

	account["RUNNER_VERCEL_MAX_SESSION"] = "24h"
	account["APP_ENV"] = "production"
	account["RUNNER_NATS_SIGNING_KEY"] = "/secrets/signing.nk"
	account["RUNNER_NATS_ACCOUNT"] = "/secrets/account.pub"
	account["RUNNER_MANAGER_NATS_CREDS"] = "/secrets/runner-manager.creds"
	account["RUNNER_REGISTRY_PROXY_URL"] = "https://registry.weave.example"
	set(account)
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "RUNNER_VERCEL_BINARY") {
		t.Errorf("production without a stated runner binary = %v", err)
	}
}

// TestStagingAndProductionRefuseToStartWithoutARegistryProxy (M5.4c,
// ADR-016): unset, runners' registry requests would pass unrecorded.
func TestStagingAndProductionRefuseToStartWithoutARegistryProxy(t *testing.T) {
	for _, tc := range []struct {
		env, url string
		ok       bool
	}{
		{"development", "", true},
		{"test", "", true},
		{"staging", "", false},
		{"production", "", false},
		{"", "", false},
		{"production", "https://registry.weave.example", true},
		{"development", "https://quick-name.trycloudflare.com", true},
		{"development", "http://registry.weave.example", false},
		{"production", "https://u:p@registry.weave.example", false},
		{"production", "https://registry.weave.example/?t=1", false},
		{"production", "https://registry.weave.example/prefix", false},
	} {
		if err := validateRegistryProxy(tc.env, tc.url); (err == nil) != tc.ok {
			t.Errorf("APP_ENV=%q RUNNER_REGISTRY_PROXY_URL=%q: %v, want ok=%v", tc.env, tc.url, err, tc.ok)
		}
	}
}
