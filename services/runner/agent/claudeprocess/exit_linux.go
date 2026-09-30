//go:build linux

package claudeprocess

import (
	"context"
	"errors"
	"time"

	"golang.org/x/sys/unix"
)

// awaitExit observes an owned child's exit without reaping it. WNOWAIT retains
// the PID until signaling is fenced and the later exec.Cmd.Wait releases it.
func awaitExit(ctx context.Context, pid int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT|unix.WNOHANG, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return ErrLifecycle
		}
		if info.Signo != 0 {
			return nil
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
