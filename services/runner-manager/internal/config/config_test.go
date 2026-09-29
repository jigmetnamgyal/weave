package config

import "testing"

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
