// Package claudeprocess supervises a tool-disabled Claude CLI invocation. It is
// not registered as a provider adapter; credential/input delivery and real-CLI
// isolation acceptance remain activation gates (M6.1b.1).
package claudeprocess

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/runner/agent"
	"github.com/jigmetnamgyal/weave/services/runner/agent/claudestream"
)

const (
	// CLIVersion is the only accepted version, not an artifact-integrity proof.
	CLIVersion      = "2.1.285"
	MaxInputBytes   = 128 << 10
	ProbeTimeout    = 5 * time.Second
	MaxRunTime      = 8 * time.Hour
	ExitGrace       = 2 * time.Second
	DeliveryTimeout = time.Minute
)

// Safe errors never contain prompts, keys, command arguments, paths or stderr.
var (
	ErrConfig      = errors.New("claude process: invalid configuration")
	ErrVersion     = errors.New("claude process: unsupported CLI version")
	ErrStart       = errors.New("claude process: start failed")
	ErrDelivery    = errors.New("claude process: delivery failed")
	ErrCleanup     = errors.New("claude process: private state cleanup failed")
	errExitTimeout = errors.New("claude process: exit deadline exceeded")
)

// Request is trusted runtime configuration plus untrusted prompt data. This
// primitive does not obtain credentials or snapshot mutable task records.
type Request struct {
	Executable string
	Workdir    string
	SessionID  uuid.UUID
	Model      string
	// APIKey is a caller-supplied secret. Never supplied to the version probe.
	APIKey            string
	Task              string
	AgentInstructions string
}

// Outcome requires both terminal stream evidence and OS exit success. Usage is
// measured only when the decoder validated a result; not a pricing estimate.
type Outcome struct {
	Failed bool
	Usage  agent.Usage
}

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,95}$`)

// Run invokes a trusted absolute executable without a shell. emit must honor
// its context; Go cannot forcibly interrupt a callback that ignores it. Failure
// is emitted once, after messages and after process completion. Cancellation
// and configuration/start/delivery errors are returned without claiming success.
func Run(ctx context.Context, req Request, emit func(context.Context, agent.ProviderEvent) error) (out Outcome, runErr error) {
	prompt, err := validate(req)
	if err != nil || emit == nil {
		return Outcome{}, ErrConfig
	}
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	home, err := os.MkdirTemp("/tmp", "weave-claude-")
	if err != nil {
		return Outcome{}, ErrStart
	}
	defer func() {
		if os.RemoveAll(home) != nil {
			out = Outcome{}
			runErr = errors.Join(runErr, ErrCleanup)
		}
	}()
	realWorkdir, err := filepath.EvalSymlinks(req.Workdir)
	if err != nil {
		return Outcome{}, ErrConfig
	}
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		return Outcome{}, ErrConfig
	}
	relative, err := filepath.Rel(realWorkdir, realHome)
	if err != nil || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return Outcome{}, ErrConfig
	}
	if err := probe(ctx, req.Executable, home); err != nil {
		return Outcome{}, err
	}

	deadlineCtx, deadlineCancel := context.WithTimeout(ctx, MaxRunTime)
	defer deadlineCancel()
	runCtx, cancel := context.WithCancelCause(deadlineCtx)
	defer cancel(nil)
	cmd := command(runCtx, req.Executable, arguments(req.Model), req.Workdir, environment(home, req.APIKey))
	cmd.Stdin = strings.NewReader(string(prompt))
	cmd.Stderr = io.Discard
	stdout, writer, err := os.Pipe()
	if err != nil {
		return Outcome{}, ErrStart
	}
	defer func() { _ = stdout.Close() }()
	cmd.Stdout = writer
	if err := cmd.Start(); err != nil {
		_ = writer.Close()
		return Outcome{}, ErrStart
	}
	_ = writer.Close()
	// Explicitly own this read end, not Cmd.StdoutPipe: Wait must not discard
	// buffered records. Cancellation also closes a read held open by descendants.
	watchDone := make(chan struct{})
	stop := context.AfterFunc(runCtx, func() {
		defer close(watchDone)
		_ = killGroup(cmd)
		_ = stdout.Close()
	})
	defer func() {
		if !stop() {
			<-watchDone
		}
	}()
	var failure *agent.ProviderEvent
	deliveryFailed := false
	result, decodeErr := claudestream.Decode(runCtx, stdout, req.SessionID, func(event agent.ProviderEvent) error {
		if event.Type == domain.EventProviderFailed {
			copy := event
			failure = &copy
			return nil
		}
		if err := deliver(runCtx, emit, event); err != nil {
			deliveryFailed = true
			return ErrDelivery
		}
		return nil
	})
	if decodeErr != nil {
		cancel(decodeErr)
	}
	// EOF alone cannot let a still-running process hang indefinitely.
	timer := time.AfterFunc(ExitGrace, func() { cancel(errExitTimeout) })
	waitErr := cmd.Wait()
	timer.Stop()
	// A child which closed stdout or finished must not leave background work.
	_ = killGroup(cmd)
	if err := ctx.Err(); err != nil {
		return Outcome{}, err
	}
	if err := deadlineCtx.Err(); err != nil {
		return Outcome{}, err
	}
	if deliveryFailed {
		return Outcome{}, ErrDelivery
	}
	code := ""
	switch {
	case errors.Is(context.Cause(runCtx), errExitTimeout):
		code = "claude_exit_timeout"
	case decodeErr != nil:
		code = "claude_stream_refused"
	case waitErr != nil:
		code = "claude_process_failed"
	}
	outcome := Outcome{Failed: result.Failed, Usage: result.Usage}
	if decodeErr != nil {
		outcome.Usage = agent.Usage{}
	}
	if code != "" && failure == nil {
		event := agent.ProviderEvent{Type: domain.EventProviderFailed, Payload: domain.ProviderFailed{Code: code, Retryable: false, Message: "Claude Code did not complete the requested turn."}}
		failure = &event
	}
	if failure != nil {
		outcome.Failed = true
		// Decode refusal cancels runCtx to stop the child. Final delivery gets the
		// still-live caller context, never a context cancelled for internal cleanup.
		if err := deliver(deadlineCtx, emit, *failure); err != nil {
			if ctx.Err() != nil {
				return Outcome{}, ctx.Err()
			}
			if deadlineCtx.Err() != nil {
				return Outcome{}, deadlineCtx.Err()
			}
			return Outcome{}, ErrDelivery
		}
	}
	if ctx.Err() != nil {
		return Outcome{}, ctx.Err()
	}
	if deadlineCtx.Err() != nil {
		return Outcome{}, deadlineCtx.Err()
	}
	return outcome, nil
}

// deliver gives cooperative callbacks their own deadline, including final
// failure delivery after the internal process context was cancelled.
func deliver(ctx context.Context, emit func(context.Context, agent.ProviderEvent) error, event agent.ProviderEvent) error {
	deliveryCtx, cancel := context.WithTimeout(ctx, DeliveryTimeout)
	defer cancel()
	return emit(deliveryCtx, event)
}

// validate bounds inputs before encoding and refuses paths/models which could
// become command options. Prompt JSON remains data on stdin, never argv.
func validate(req Request) ([]byte, error) {
	if !processGroupsSupported || req.SessionID == uuid.Nil || !filepath.IsAbs(req.Executable) || !filepath.IsAbs(req.Workdir) || !modelPattern.MatchString(req.Model) || req.APIKey == "" || len(req.APIKey) > 512 || strings.ContainsAny(req.APIKey, "\x00\r\n") {
		return nil, ErrConfig
	}
	binary, err := os.Stat(req.Executable)
	if err != nil || !binary.Mode().IsRegular() || binary.Mode().Perm()&0111 == 0 {
		return nil, ErrConfig
	}
	dir, err := os.Stat(req.Workdir)
	if err != nil || !dir.IsDir() {
		return nil, ErrConfig
	}
	if strings.TrimSpace(req.Task) == "" || len(req.Task) > MaxInputBytes || len(req.AgentInstructions) > MaxInputBytes || !utf8.ValidString(req.Task) || !utf8.ValidString(req.AgentInstructions) || strings.ContainsRune(req.Task, 0) || strings.ContainsRune(req.AgentInstructions, 0) {
		return nil, ErrConfig
	}
	data, err := json.Marshal(struct {
		Task         string `json:"task"`
		Instructions string `json:"agent_instructions"`
	}{req.Task, req.AgentInstructions})
	if err != nil || len(data) > MaxInputBytes {
		return nil, ErrConfig
	}
	return data, nil
}

// arguments fixes the tool/permission profile; callers cannot add CLI flags.
func arguments(model string) []string {
	return []string{"--bare", "--print", "--input-format", "text", "--output-format", "stream-json", "--verbose", "--model", model, "--tools", "", "--disallowedTools", "*", "--permission-mode", "dontAsk", "--setting-sources", "", "--settings", "{}", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--no-session-persistence"}
}

// environment deliberately does not inherit parent secrets or overrides.
func environment(home, key string) []string {
	env := []string{"HOME=" + home, "TMPDIR=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "PATH=/usr/local/bin:/usr/bin:/bin", "LANG=C.UTF-8", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}
	if key != "" {
		env = append(env, "ANTHROPIC_API_KEY="+key)
	}
	return env
}

// probe checks a bounded version response in the private home, without credentials.
func probe(ctx context.Context, binary, home string) error {
	probeCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	output := &cappedOutput{}
	cmd := command(probeCtx, binary, []string{"--version"}, home, environment(home, ""))
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrVersion
	}
	_ = killGroup(cmd)
	if output.overflow || strings.TrimSpace(string(output.data)) != CLIVersion+" (Claude Code)" {
		return ErrVersion
	}
	return nil
}

// cappedOutput consumes output without retaining an unbounded version banner.
type cappedOutput struct {
	data     []byte
	overflow bool
}

func (w *cappedOutput) Write(data []byte) (int, error) {
	count := len(data)
	remaining := 256 - len(w.data)
	if len(data) > remaining {
		w.overflow = true
		data = data[:remaining]
	}
	w.data = append(w.data, data...)
	return count, nil
}
