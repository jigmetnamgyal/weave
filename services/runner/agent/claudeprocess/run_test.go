//go:build linux || darwin

package claudeprocess

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/runner/agent"
)

var fakeCLI string

// TestMain builds a local synthetic CLI, never installing or invoking Claude.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("/tmp", "weave-fake-cli-")
	if err != nil {
		os.Exit(1)
	}
	fakeCLI = filepath.Join(dir, "fake-cli")
	cmd := exec.Command("go", "build", "-o", fakeCLI, "./testdata/fakecli")
	if err := cmd.Run(); err != nil {
		_ = os.RemoveAll(dir)
		os.Exit(1)
	}
	status := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(status)
}

// request supplies invented data and a synthetic key to the synthetic executable.
func request(t *testing.T, model string) Request {
	t.Helper()
	return Request{Executable: fakeCLI, Workdir: t.TempDir(), SessionID: uuid.New(), Model: model, APIKey: "synthetic-key", Task: "UNTRUSTED task $(not-a-command) --model other", AgentInstructions: "UNTRUSTED instructions --bare"}
}

// collect gathers synchronous callbacks without broker or network dependencies.
func collect(ctx context.Context, req Request) (Outcome, []agent.ProviderEvent, error) {
	var events []agent.ProviderEvent
	result, err := Run(ctx, req, func(_ context.Context, event agent.ProviderEvent) error { events = append(events, event); return nil })
	return result, events, err
}

// TestStdinEnvironmentAndFlagsAreFixed fails if the child inherits parent secrets or lacks policy switches.
func TestStdinEnvironmentAndFlagsAreFixed(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_API_KEY", "WEAVE_GIT_TOKEN", "WEAVE_NATS_CREDS", "ANTHROPIC_BASE_URL", "HTTP_PROXY", "NODE_OPTIONS", "CLAUDE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN"} {
		t.Setenv(key, "SYNTHETIC_SECRET")
	}
	result, events, err := collect(context.Background(), request(t, "ok"))
	if err != nil || result.Failed || len(events) != 1 || events[0].Payload.(domain.MessageCreated).Text != "verified" {
		t.Fatalf("result=%+v events=%d error=%v", result, len(events), err)
	}
	if result.Usage != (agent.Usage{InputTokens: 14, OutputTokens: 3}) {
		t.Fatalf("usage=%+v", result.Usage)
	}
}

// TestExitAndStreamPrecedence requires terminal success AND zero exit, and one final failure otherwise.
func TestExitAndStreamPrecedence(t *testing.T) {
	for _, model := range []string{"failure", "nonzero", "malformed", "empty"} {
		t.Run(model, func(t *testing.T) {
			result, events, err := collect(context.Background(), request(t, model))
			if err != nil || !result.Failed || len(events) == 0 || events[len(events)-1].Type != domain.EventProviderFailed {
				t.Fatalf("result=%+v events=%d error=%v", result, len(events), err)
			}
			count := 0
			for _, event := range events {
				if event.Type == domain.EventProviderFailed {
					count++
					payload := event.Payload.(domain.ProviderFailed)
					if strings.Contains(payload.Message, "SYNTHETIC_SECRET") {
						t.Fatal("stderr reflected")
					}
				}
			}
			if count != 1 {
				t.Fatalf("failure count=%d", count)
			}
		})
	}
}

// TestPrivateHomeIsRemoved checks the invocation's private state disappears after reaping.
func TestPrivateHomeIsRemoved(t *testing.T) {
	result, events, err := collect(context.Background(), request(t, "home"))
	if err != nil || result.Failed || len(events) != 1 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	home := events[0].Payload.(domain.MessageCreated).Text
	if !strings.HasPrefix(filepath.Base(home), "weave-claude-") {
		t.Fatal("missing private home")
	}
	if _, err := os.Stat(home); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("private state remained")
	}
}

// TestStderrFloodIsDiscarded avoids pipe deadlock and diagnostic reflection.
func TestStderrFloodIsDiscarded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, events, err := collect(ctx, request(t, "flood"))
	if err != nil || result.Failed || len(events) != 1 {
		t.Fatalf("result=%+v error=%v", result, err)
	}
}

// TestHungProcessCancellation verifies a blocked stdout read cannot prevent shutdown.
func TestHungProcessCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, err := collect(ctx, request(t, "hang"))
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 3*time.Second {
		t.Fatalf("error=%v duration=%v", err, time.Since(started))
	}
}

// TestBackpressuredDeliveryHonorsCancellation requires cooperative callback shutdown and child teardown.
func TestBackpressuredDeliveryHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	calls := 0
	_, err := Run(ctx, request(t, "backpressure"), func(ctx context.Context, _ agent.ProviderEvent) error { calls++; <-ctx.Done(); return ctx.Err() })
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

// TestDeliveryFailureIsSafe returns a stable category instead of reflecting callback text.
func TestDeliveryFailureIsSafe(t *testing.T) {
	for _, model := range []string{"ok", "failure"} {
		_, err := Run(context.Background(), request(t, model), func(context.Context, agent.ProviderEvent) error { return errors.New("SYNTHETIC_SECRET") })
		if err != ErrDelivery {
			t.Fatalf("error=%v", err)
		}
	}
}

// TestEOFDoesNotAllowAProcessToHang bounds a process that finishes stdout but keeps running.
func TestEOFDoesNotAllowAProcessToHang(t *testing.T) {
	// The helper closes stdout after terminal output, then remains alive.
	result, events, err := collect(context.Background(), request(t, "exit-hang"))
	if err != nil || !result.Failed || len(events) != 2 {
		t.Fatalf("result=%+v events=%d error=%v", result, len(events), err)
	}
}

// TestDescendantsHoldingStdoutAreKilled ensures a dead leader does not leave a blocked pipe indefinitely.
func TestDescendantsHoldingStdoutAreKilled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	child := 0
	_, err := Run(ctx, request(t, "descendant"), func(_ context.Context, event agent.ProviderEvent) error {
		parent := 0
		_, parseErr := fmt.Sscanf(event.Payload.(domain.MessageCreated).Text, "parent=%d child=%d", &parent, &child)
		if parseErr != nil {
			return parseErr
		}
		return nil
	})
	if child == 0 {
		t.Fatal("did not observe a synthetic descendant")
	}
	// This PID belongs to the synthetic child started by this test. Clean it up
	// even during a deliberate mutation which disables group termination.
	defer func() {
		// Do not signal a reused PID belonging to unrelated host work.
		args, _ := exec.Command("ps", "-o", "args=", "-p", fmt.Sprint(child)).Output()
		if strings.TrimSpace(string(args)) == fakeCLI+" --child" {
			_ = syscall.Kill(child, syscall.SIGKILL)
		}
	}()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		out, _ := exec.Command("ps", "-o", "stat=", "-p", fmt.Sprint(child)).Output()
		state := strings.TrimSpace(string(out))
		if state == "" || strings.HasPrefix(state, "Z") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("synthetic descendant still running after cancellation")
}

// TestVersionProbeDoesNotUseCredentials rejects a mismatched pin before prompt execution.
func TestVersionProbeDoesNotUseCredentials(t *testing.T) {
	req := request(t, "ok")
	wrong := filepath.Join(t.TempDir(), "wrong-cli")
	if err := os.Symlink(fakeCLI, wrong); err != nil {
		t.Fatal(err)
	}
	req.Executable = wrong
	_, events, err := collect(context.Background(), req)
	if err != ErrVersion || len(events) != 0 {
		t.Fatalf("error=%v events=%d", err, len(events))
	}
}

// TestProbeCancellationIsBounded verifies the caller can stop even the version phase.
func TestProbeCancellationIsBounded(t *testing.T) {
	req := request(t, "ok")
	path := filepath.Join(t.TempDir(), "probe-hang")
	if err := os.Symlink(fakeCLI, path); err != nil {
		t.Fatal(err)
	}
	req.Executable = path
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, _, err := collect(ctx, req)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v", err)
	}
}

// TestInvalidInputsAreRefusedBeforeExecution bounds secret and prompt inputs without reflection.
func TestInvalidInputsAreRefusedBeforeExecution(t *testing.T) {
	mutations := map[string]func(*Request){
		"relative executable":     func(r *Request) { r.Executable = "cli" },
		"relative workdir":        func(r *Request) { r.Workdir = "repo" },
		"missing executable":      func(r *Request) { r.Executable = filepath.Join(r.Workdir, "missing") },
		"option model":            func(r *Request) { r.Model = "--dangerously-skip-permissions" },
		"empty key":               func(r *Request) { r.APIKey = "" },
		"key newline":             func(r *Request) { r.APIKey = "SYNTHETIC_SECRET\n" },
		"nil session":             func(r *Request) { r.SessionID = uuid.Nil },
		"empty task":              func(r *Request) { r.Task = " " },
		"raw oversized input":     func(r *Request) { r.Task = strings.Repeat("x", MaxInputBytes+1) },
		"encoded oversized input": func(r *Request) { r.Task = strings.Repeat("\x01", MaxInputBytes/2) },
		"NUL prompt":              func(r *Request) { r.Task = "UNTRUSTED\x00" },
		"invalid UTF8":            func(r *Request) { r.Task = string([]byte{0xff}) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			req := request(t, "ok")
			mutate(&req)
			_, events, err := collect(context.Background(), req)
			if err != ErrConfig || len(events) != 0 {
				t.Fatalf("error=%v events=%d", err, len(events))
			}
		})
	}
}

// TestVersionOutputIsBounded verifies a noisy probe cannot retain unlimited text.
func TestVersionOutputIsBounded(t *testing.T) {
	output := &cappedOutput{}
	count, err := output.Write([]byte(strings.Repeat("SYNTHETIC_SECRET", 1024)))
	if err != nil || count != len("SYNTHETIC_SECRET")*1024 || len(output.data) != 256 || !output.overflow {
		t.Fatalf("count=%d retained=%d overflow=%v", count, len(output.data), output.overflow)
	}
}

// TestClaudeStillCannotBeSelected keeps this primitive outside production routing.
func TestClaudeStillCannotBeSelected(t *testing.T) {
	if _, err := agent.New(domain.ProviderClaudeCode); err == nil {
		t.Fatal("Claude runtime was enabled")
	}
}

// TestCleanupFailureIsReported must not claim private state was removed when it was not.
func TestCleanupFailureIsReported(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the synthetic directory permission failure")
	}
	_, events, err := collect(context.Background(), request(t, "unclean-home"))
	if len(events) != 1 {
		t.Fatal("missing cleanup evidence")
	}
	home := events[0].Payload.(domain.MessageCreated).Text
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(home, "blocked"), 0700); _ = os.RemoveAll(home) })
	if !errors.Is(err, ErrCleanup) {
		t.Fatalf("cleanup error=%v", err)
	}
}

// TestPrivateHomeCannotBeInsideCheckout refuses parent-directory and symlink tricks.
func TestPrivateHomeCannotBeInsideCheckout(t *testing.T) {
	for _, dir := range []string{"/tmp", "/"} {
		req := request(t, "ok")
		req.Workdir = dir
		_, _, err := collect(context.Background(), req)
		if err != ErrConfig {
			t.Fatalf("error=%v", err)
		}
	}
	req := request(t, "ok")
	link := filepath.Join(t.TempDir(), "checkout-link")
	if err := os.Symlink("/tmp", link); err != nil {
		t.Fatal(err)
	}
	req.Workdir = link
	if _, _, err := collect(context.Background(), req); err != ErrConfig {
		t.Fatalf("error=%v", err)
	}
}

// TestFinalFailureDeliveryIsBounded gives callbacks a deadline even after internal cancellation.
func TestFinalFailureDeliveryIsBounded(t *testing.T) {
	_, err := Run(context.Background(), request(t, "malformed"), func(ctx context.Context, event agent.ProviderEvent) error {
		if event.Type != domain.EventProviderFailed {
			t.Fatal("unexpected message")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > DeliveryTimeout || ctx.Err() != nil {
			t.Fatal("final delivery lacks a live bounded context")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("error=%v", err)
	}
}

// TestCancellationDuringFinalFailureDelivery still reports cancellation, not success or delivery refusal.
func TestCancellationDuringFinalFailureDelivery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := Run(ctx, request(t, "failure"), func(deliveryCtx context.Context, event agent.ProviderEvent) error {
		if event.Type == domain.EventProviderFailed {
			cancel()
			<-deliveryCtx.Done()
			return deliveryCtx.Err()
		}
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
}

// TestRetiredGroupSignalsCannotTargetReusedIdentity deterministically simulates
// numeric ID reuse: after retirement a stale callback must never reach the OS.
func TestRetiredGroupSignalsCannotTargetReusedIdentity(t *testing.T) {
	calls := 0
	signals := &groupSignals{send: func() error { calls++; return nil }}
	if err := signals.signal(); err != nil {
		t.Fatal(err)
	}
	signals.retire()
	if err := signals.signal(); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("error=%v", err)
	}
	if calls != 1 {
		t.Fatal("retired callback signaled a simulated unrelated group")
	}
}

// TestRetirementJoinsAnInFlightSignal prevents reaping while a callback still holds the identity.
func TestRetirementJoinsAnInFlightSignal(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	signals := &groupSignals{send: func() error { close(entered); <-release; return nil }}
	sent := make(chan struct{})
	go func() { _ = signals.signal(); close(sent) }()
	<-entered
	retired := make(chan struct{})
	go func() { signals.retire(); close(retired) }()
	select {
	case <-retired:
		close(release)
		t.Fatal("retired while signal still running")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	<-sent
	<-retired
}

// TestExitObservationRetainsTheChild proves awaitExit does not consume wait status.
func TestExitObservationRetainsTheChild(t *testing.T) {
	req := request(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := command(req.Executable, []string{"--version"}, req.Workdir, environment(req.Workdir, ""))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	signals := &groupSignals{send: func() error { return killGroup(cmd) }}
	reaped := false
	defer func() {
		if !reaped {
			_ = signals.signal()
			signals.retire()
			_ = cmd.Wait()
		}
	}()
	if err := awaitExit(ctx, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	// Signal zero only while the child is retained. Reaping below consumes it.
	if err := syscall.Kill(cmd.Process.Pid, 0); err != nil {
		t.Fatal("exit observation released the owned identity")
	}
	_ = signals.signal()
	signals.retire()
	err := cmd.Wait()
	reaped = true
	if err != nil {
		t.Fatalf("wait status was consumed or changed: %v", err)
	}
}

// TestReapSignalsBeforeRelease checks actual exit status and signal ordering at the shared boundary.
func TestReapSignalsBeforeRelease(t *testing.T) {
	req := request(t, "ok")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := command(req.Executable, []string{"--version"}, req.Workdir, environment(req.Workdir, ""))
	if cmd.Cancel != nil {
		t.Fatal("hidden exec cancellation watcher bypasses the identity lease")
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	calls := 0
	signals := &groupSignals{send: func() error {
		calls++
		if cmd.ProcessState != nil {
			t.Fatal("numeric group signal followed reaping")
		}
		return killGroup(cmd)
	}}
	waited := false
	defer func() {
		if !waited {
			_ = signals.signal()
			signals.retire()
			_ = cmd.Wait()
		}
	}()
	if err := awaitExit(ctx, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	err := reap(cmd, signals, func() {})
	waited = true
	if err != nil {
		t.Fatalf("exit status lost: %v", err)
	}
	if calls != 1 {
		t.Fatalf("signals before reaping=%d", calls)
	}
	if err := signals.signal(); !errors.Is(err, os.ErrProcessDone) || calls != 1 {
		t.Fatal("stale callback reached a released identity")
	}
}
