package claudeprocess

import (
	"context"
	"os"
	"os/exec"
	"sync"
)

// groupSignals fences numeric group signaling before reaping. Retiring joins
// an in-flight signal and makes every stale callback harmless, even if the
// numeric process-group ID later belongs to unrelated work.
type groupSignals struct {
	mu      sync.Mutex
	retired bool
	send    func() error
}

// signal runs only while this supervisor still retains its unreaped child PID.
func (s *groupSignals) signal() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retired {
		return os.ErrProcessDone
	}
	return s.send()
}

// retire must finish BEFORE Wait can release the PID for reuse.
func (s *groupSignals) retire() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.retired = true
}

// watchCancellation joins cancellation-driven group signaling before reaping.
func watchCancellation(ctx context.Context, signals *groupSignals, stdout *os.File) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); _ = signals.signal(); _ = stdout.Close() })
	var once sync.Once
	return func() {
		once.Do(func() {
			if !stop() {
				<-done
			}
		})
	}
}

// reap signals remaining descendants while the direct child's identity is
// retained, fences all future group signals, and only then releases it via Wait.
func reap(cmd *exec.Cmd, signals *groupSignals, stopWatch func()) error {
	stopWatch()
	_ = signals.signal()
	signals.retire()
	return cmd.Wait()
}
