// Package backend builds the runner manager's configured RunnerBackend.
//
// Its own package, not main, so which backend starts — and which refuses to —
// is tested: the dev backend outside development, and in staging and
// production the Vercel backend whose plan cap is below the longest a runner
// can live (M5.4b).
package backend

import (
	"fmt"
	"os"

	"github.com/jigmetnamgyal/weave/internal/adapters/devdocker"
	weavetemporal "github.com/jigmetnamgyal/weave/internal/adapters/temporal"
	"github.com/jigmetnamgyal/weave/internal/adapters/vercelsandbox"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/services/runner-manager/internal/config"
)

// New builds the backend cfg names.
//
// devdocker.New refuses anything but development and test, and
// vercelsandbox.New refuses a plan cap below temporal.MaxRunnerLifetime()
// outside them — so a misconfigured deployment fails to start rather than
// running untrusted code in a container, or losing runners to the provider's
// cap mid-session.
func New(cfg config.Config) (application.RunnerBackend, error) {
	switch cfg.Backend {
	case devdocker.BackendName:
		return devdocker.New(devdocker.Config{
			AppEnv: cfg.AppEnv, Socket: cfg.DockerSocket, Image: cfg.RunnerImage, Scope: cfg.RunnerScope,
		})
	case vercelsandbox.BackendName:
		binary, err := os.ReadFile(cfg.Vercel.RunnerBinary) // #nosec G304 -- operator-supplied path
		if err != nil {
			return nil, fmt.Errorf("read the runner binary for Vercel (RUNNER_VERCEL_BINARY; build it with `make runner-binary`): %w", err)
		}
		return vercelsandbox.New(vercelsandbox.Config{
			AppEnv: cfg.AppEnv, Token: cfg.Vercel.Token, TeamID: cfg.Vercel.TeamID,
			ProjectID: cfg.Vercel.ProjectID, Region: cfg.Vercel.Region, Scope: cfg.RunnerScope,
			MaxSession: cfg.Vercel.MaxSession, Lease: cfg.Vercel.Lease,
			RequiredLifetime: weavetemporal.MaxRunnerLifetime(),
			RunnerBinary:     binary,
		})
	default:
		return nil, fmt.Errorf("unknown RUNNER_BACKEND %q; want %q or %q",
			cfg.Backend, devdocker.BackendName, vercelsandbox.BackendName)
	}
}
