//go:build linux || darwin

package claudeprocess

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const processGroupsSupported = true

// command creates an isolated process group and bounds exec pipe cleanup.
func command(ctx context.Context, binary string, args []string, dir string, env []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = time.Second
	return cmd
}

// killGroup signals the direct child's process group, including inherited-pipe children.
func killGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
