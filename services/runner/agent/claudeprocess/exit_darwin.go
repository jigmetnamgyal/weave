//go:build darwin

package claudeprocess

import (
	"context"
	"errors"
	"syscall"
)

// awaitExit uses kqueue NOTE_EXIT rather than waitid on macOS, where waitid
// has historically also reported stopped children. kqueue does not reap; this
// supervisor is the sole waiter and retains the child's PID until Cmd.Wait.
func awaitExit(ctx context.Context, pid int) error {
	fd, err := syscall.Kqueue()
	if err != nil {
		return ErrLifecycle
	}
	defer func() { _ = syscall.Close(fd) }()
	var change syscall.Kevent_t
	syscall.SetKevent(&change, pid, syscall.EVFILT_PROC, syscall.EV_ADD|syscall.EV_ENABLE|syscall.EV_ONESHOT)
	change.Fflags = syscall.NOTE_EXIT
	changes := []syscall.Kevent_t{change}
	events := make([]syscall.Kevent_t, 1)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		timeout := syscall.NsecToTimespec(5_000_000)
		n, err := syscall.Kevent(fd, changes, events, &timeout)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		// A completed owned child may already be unavailable for a new kqueue
		// subscription. No other code waits/reaps it, so its PID is still retained.
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return ErrLifecycle
		}
		changes = nil
		if n == 0 {
			continue
		}
		event := events[0]
		if event.Flags&syscall.EV_ERROR != 0 {
			if event.Data == int64(syscall.ESRCH) {
				return nil
			}
			return ErrLifecycle
		}
		if event.Filter == syscall.EVFILT_PROC && event.Fflags&syscall.NOTE_EXIT != 0 {
			return nil
		}
	}
}
