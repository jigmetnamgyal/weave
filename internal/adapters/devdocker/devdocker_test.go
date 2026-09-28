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
	for _, env := range []string{"staging", "production", "Production", " staging "} {
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
