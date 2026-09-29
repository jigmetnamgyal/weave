package backend

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jigmetnamgyal/weave/internal/adapters/devdocker"
	weavetemporal "github.com/jigmetnamgyal/weave/internal/adapters/temporal"
	"github.com/jigmetnamgyal/weave/internal/adapters/vercelsandbox"
	"github.com/jigmetnamgyal/weave/services/runner-manager/internal/config"
)

// amd64Binary writes the smallest linux/amd64 ELF file the backend accepts.
func amd64Binary(t *testing.T) string {
	t.Helper()
	h := make([]byte, 64)
	copy(h, "\x7fELF")
	h[4], h[5], h[6] = 2, 1, 1
	binary.LittleEndian.PutUint16(h[16:], 2)
	binary.LittleEndian.PutUint16(h[18:], 62)
	binary.LittleEndian.PutUint32(h[20:], 1)
	binary.LittleEndian.PutUint16(h[52:], 64)
	binary.LittleEndian.PutUint16(h[54:], 56)
	binary.LittleEndian.PutUint16(h[58:], 64)
	path := filepath.Join(t.TempDir(), "weave-runner")
	if err := os.WriteFile(path, h, 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func vercelConfig(t *testing.T, env string, maxSession time.Duration) config.Config {
	return config.Config{
		AppEnv: env, Backend: vercelsandbox.BackendName, RunnerScope: "dev",
		Vercel: config.VercelConfig{
			Token: "t", TeamID: "team", ProjectID: "prj", Region: "iad1",
			MaxSession: maxSession, RunnerBinary: amd64Binary(t),
		},
	}
}

// TestProductionRefusesACapBelowARunnersLifetime is the startup refusal M5.4b
// requires, against the real bound: a runner manager whose plan would stop a
// sandbox before its workflow can halt it and confirm its events does not
// start in staging or production. Both sides of the boundary.
func TestProductionRefusesACapBelowARunnersLifetime(t *testing.T) {
	lifetime := weavetemporal.MaxRunnerLifetime()
	for _, tc := range []struct {
		env string
		cap time.Duration
		ok  bool
	}{
		{"production", lifetime, true},
		{"production", lifetime - time.Second, false},
		{"staging", lifetime - time.Second, false},
		{"production", 24 * time.Hour, true},    // Vercel Pro
		{"production", 45 * time.Minute, false}, // Vercel Hobby
		{"development", 45 * time.Minute, true},
	} {
		_, err := New(vercelConfig(t, tc.env, tc.cap))
		if (err == nil) != tc.ok || (!tc.ok && !errors.Is(err, vercelsandbox.ErrCapBelowLifetime)) {
			t.Errorf("APP_ENV=%s cap %s (lifetime %s): %v, want ok=%v", tc.env, tc.cap, lifetime, err, tc.ok)
		}
	}
}

func TestTheDevBackendIsRefusedOutsideDevelopment(t *testing.T) {
	_, err := New(config.Config{AppEnv: "production", Backend: devdocker.BackendName, RunnerScope: "dev",
		DockerSocket: "/var/run/docker.sock", RunnerImage: "weave-runner:dev"})
	if !errors.Is(err, devdocker.ErrRefusedOutsideDevelopment) {
		t.Errorf("dev backend in production = %v, want refused", err)
	}
}

func TestAMissingRunnerBinarySaysHowToBuildIt(t *testing.T) {
	cfg := vercelConfig(t, "development", 45*time.Minute)
	cfg.Vercel.RunnerBinary = filepath.Join(t.TempDir(), "absent")
	if _, err := New(cfg); err == nil || !strings.Contains(err.Error(), "make runner-binary") {
		t.Errorf("missing binary = %v, want a pointer to `make runner-binary`", err)
	}
}

func TestAnUnknownBackendIsRefused(t *testing.T) {
	if _, err := New(config.Config{AppEnv: "development", Backend: "docker"}); err == nil {
		t.Error("an unknown backend was accepted")
	}
}
