package application_test

import (
	"strings"
	"testing"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// TestEgressReservedConfiguration requires deployed declarations and never
// echoes credential-bearing service URL values in configuration errors.
func TestEgressReservedConfiguration(t *testing.T) {
	if _, err := application.ParseEgressReservedConfig("production", "", nil); err == nil {
		t.Fatal("production accepted no declaration")
	}
	got, err := application.ParseEgressReservedConfig("staging", "Weave.Example.com", map[string]string{"API_BASE_URL": "https://api.weave.example.com", "RUNNER_NATS_URL": "wss://events.weave.example.com", "LOCAL": "http://localhost:8080", "IP": "http://127.0.0.1"})
	if err != nil || strings.Join(got, ",") == "" {
		t.Fatalf("parse: %v %v", got, err)
	}
	for _, want := range []string{"weave.example.com", "api.weave.example.com", "events.weave.example.com"} {
		if !strings.Contains(","+strings.Join(got, ",")+",", ","+want+",") {
			t.Errorf("missing %s from %v", want, got)
		}
	}
	for _, raw := range []string{"https://user:secret@api.weave.example.com", "https://api.weave.example.com?token=secret", "ftp://api.weave.example.com"} {
		_, err := application.ParseEgressReservedConfig("development", "", map[string]string{"API_BASE_URL": raw})
		if err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("unsafe URL %q: %v", raw, err)
		}
	}
	if _, err := application.ParseEgressReservedConfig("development", "https://weave.example.com", nil); err == nil {
		t.Error("URL accepted as namespace")
	}
}

// TestEgressServiceReservesBuiltIns keeps plain additions from replacing default
// forwarding rules, including subdomains of built-in registries.
func TestEgressServiceReservesBuiltIns(t *testing.T) {
	service, err := application.NewEgressService(nil, []string{"weave.example.com"})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"github.com", "api.github.com", "registry.npmjs.org", "x.registry.npmjs.org", "app.weave.example.com"} {
		if _, err := service.Validate(host); err == nil {
			t.Errorf("reserved %s accepted", host)
		}
	}
	if got, err := service.Validate("DOCS.Example.com"); err != nil || got != "docs.example.com" {
		t.Fatalf("valid: %q %v", got, err)
	}
}
