package devdocker_test

import (
	"errors"
	"testing"

	"github.com/jigmetnamgyal/weave/internal/adapters/devdocker"
)

// TestTheDevBackendIsRefusedOutsideDevelopment: it is not a security
// boundary, so it must not be deployable by accident. ADR-013.
func TestTheDevBackendIsRefusedOutsideDevelopment(t *testing.T) {
	cfg := devdocker.Config{Socket: "/var/run/docker.sock", Image: "weave-runner:dev", Scope: "dev"}
	// An allowlist: anything not explicitly development or test is refused,
	// including a value nobody anticipated and no value at all.
	for _, env := range []string{"staging", "production", "Production", " staging ", "prod", "live", "qa", ""} {
		cfg.AppEnv = env
		if _, err := devdocker.New(cfg); !errors.Is(err, devdocker.ErrRefusedOutsideDevelopment) {
			t.Errorf("APP_ENV=%q: %v, want ErrRefusedOutsideDevelopment", env, err)
		}
	}
	for _, env := range []string{"development", "test"} {
		cfg.AppEnv = env
		if _, err := devdocker.New(cfg); err != nil {
			t.Errorf("APP_ENV=%q refused: %v", env, err)
		}
	}
}

// TestTheScopeIsPartOfTheBackendName: reconciliation judges only runners whose
// backend name is its own, so two scopes must never share one.
func TestTheScopeIsPartOfTheBackendName(t *testing.T) {
	cfg := devdocker.Config{AppEnv: "test", Socket: "/var/run/docker.sock", Image: "weave-runner:dev"}
	names := map[string]bool{}
	for _, scope := range []string{"dev", "test0123456789"} {
		cfg.Scope = scope
		backend, err := devdocker.New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		names[backend.Name()] = true
	}
	if len(names) != 2 {
		t.Errorf("two scopes produced backend names %v; they must differ", names)
	}
	for _, bad := range []string{"", "Dev", "has-dash", "a_b", "twentyonecharacters12"} {
		cfg.Scope = bad
		if _, err := devdocker.New(cfg); err == nil {
			t.Errorf("scope %q was accepted", bad)
		}
	}
}
