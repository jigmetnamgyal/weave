//go:build !linux && !darwin

package claudeprocess

import (
	"context"
	"os/exec"
)

const processGroupsSupported = false

// command is unreachable: validation refuses unsupported process-group platforms.
func command(ctx context.Context, binary string, args []string, dir string, env []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd
}

// killGroup refuses unsupported platforms rather than promising subtree termination.
func killGroup(*exec.Cmd) error { return ErrConfig }
