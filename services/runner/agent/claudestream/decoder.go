// Package claudestream normalizes Claude Code's documented single-turn NDJSON
// output offline. It does not launch Claude, hold credentials or enable an adapter.
package claudestream

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/runner/agent"
)

const (
	// MaxRecordBytes includes the required newline delimiter.
	MaxRecordBytes = 256 << 10
	MaxRecords     = 65536
	MaxMessages    = 4096
)

// Errors are fixed categories: underlying errors can contain provider output.
var (
	ErrMalformed  = errors.New("claude stream: malformed record")
	ErrOversize   = errors.New("claude stream: record too large")
	ErrLimit      = errors.New("claude stream: count limit exceeded")
	ErrTruncated  = errors.New("claude stream: incomplete record")
	ErrIncomplete = errors.New("claude stream: missing result")
	ErrProtocol   = errors.New("claude stream: unsupported or inconsistent protocol")
	ErrRead       = errors.New("claude stream: read failed")
	ErrDelivery   = errors.New("claude stream: delivery failed")
)

// Result is terminal provider evidence, not proof of process exit. Runtime
// wiring must also check exit status and cancellation before declaring success.
type Result struct {
	Failed bool
	Usage  agent.Usage
}

type record struct {
	Type              string             `json:"type"`
	UUID              string             `json:"uuid"`
	SessionID         string             `json:"session_id"`
	Subtype           string             `json:"subtype"`
	Error             string             `json:"error"`
	Aborted           bool               `json:"aborted"`
	IsError           *bool              `json:"is_error"`
	PermissionDenials *[]json.RawMessage `json:"permission_denials"`
	Message           struct {
		ID      string `json:"id"`
		Role    string `json:"role"`
		Content []struct {
			Type string  `json:"type"`
			Text *string `json:"text"`
		} `json:"content"`
	} `json:"message"`
	Usage struct {
		Input         *int64 `json:"input_tokens"`
		Output        *int64 `json:"output_tokens"`
		CacheRead     *int64 `json:"cache_read_input_tokens"`
		CacheCreation *int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// Decode consumes one bounded stream. Complete assistant text is emitted once;
// deltas and result text are not emitted. Blocking reader cancellation belongs
// to the caller. On error, the returned Result must not be treated as success.
func Decode(ctx context.Context, input io.Reader, session uuid.UUID, emit func(agent.ProviderEvent) error) (Result, error) {
	if input == nil || emit == nil || session == uuid.Nil {
		return Result{}, ErrProtocol
	}
	reader := bufio.NewReaderSize(input, 4096)
	seen := make(map[uuid.UUID][32]byte)
	providerSession := ""
	terminal := false
	result := Result{}
	fail := func(code string) error {
		if result.Failed {
			return nil
		}
		result.Failed = true
		if err := emit(agent.ProviderEvent{Type: domain.EventProviderFailed, Payload: domain.ProviderFailed{
			Code: code, Retryable: false, Message: "Claude Code did not complete the requested turn.",
		}}); err != nil {
			return ErrDelivery
		}
		return nil
	}
	for count := 0; ; count++ {
		if err := ctx.Err(); err != nil {
			return Result{}, err
		}
		raw, err := readRecord(reader)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, ctxErr
		}
		if errors.Is(err, io.EOF) {
			if !terminal {
				return Result{}, ErrIncomplete
			}
			return result, nil
		}
		if err != nil {
			return Result{}, err
		}
		if count >= MaxRecords {
			return Result{}, ErrLimit
		}
		if terminal {
			return Result{}, ErrProtocol
		}
		if !utf8.Valid(raw) {
			return Result{}, ErrMalformed
		}
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return Result{}, ErrMalformed
		}
		var rec record
		if json.Unmarshal(trimmed, &rec) != nil {
			return Result{}, ErrMalformed
		}
		if rec.SessionID == "" || len(rec.SessionID) > 256 {
			return Result{}, ErrProtocol
		}
		if providerSession == "" {
			providerSession = rec.SessionID
		}
		if rec.SessionID != providerSession {
			return Result{}, ErrProtocol
		}
		switch rec.Type {
		case "system", "user", "stream_event":
			// Explicitly supported but not persisted: no raw metadata or user echoes.
		case "assistant":
			id, err := uuid.Parse(rec.UUID)
			if err != nil || id == uuid.Nil || rec.Message.ID == "" || rec.Message.Role != "assistant" || rec.Message.Content == nil {
				return Result{}, ErrProtocol
			}
			digest := sha256.Sum256(raw)
			if old, ok := seen[id]; ok {
				if old != digest {
					return Result{}, ErrProtocol
				}
				continue
			}
			if len(seen) >= MaxMessages {
				return Result{}, ErrLimit
			}
			seen[id] = digest
			if rec.Error != "" || rec.Aborted {
				if err := fail("claude_assistant_failed"); err != nil {
					return Result{}, err
				}
				continue
			}
			var text strings.Builder
			for _, block := range rec.Message.Content {
				if block.Type == "text" {
					if block.Text == nil {
						return Result{}, ErrProtocol
					}
					text.WriteString(*block.Text)
				}
			}
			if text.Len() == 0 {
				continue
			}
			value := text.String()
			if strings.ContainsRune(value, 0) || utf8.RuneCountInString(value) > 32768 {
				return Result{}, ErrOversize
			}
			payload := domain.MessageCreated{MessageID: uuid.NewSHA1(session, id[:]), Role: "assistant", Text: value}
			encoded, err := json.Marshal(payload)
			// Reserve more than the fixed v1 envelope needs; JSON escaping can multiply
			// text size. A rune cap alone does not guarantee a publishable event.
			if err != nil || len(encoded) > domain.MaxEventBytes-2048 {
				return Result{}, ErrOversize
			}
			if err := emit(agent.ProviderEvent{Type: domain.EventMessageCreated, Payload: payload}); err != nil {
				return Result{}, ErrDelivery
			}
		case "result":
			if rec.IsError == nil || rec.PermissionDenials == nil {
				return Result{}, ErrProtocol
			}
			usage, err := resultUsage(rec)
			if err != nil {
				return Result{}, err
			}
			result.Usage = usage
			code := ""
			switch rec.Subtype {
			case "success":
				if *rec.IsError {
					code = "claude_result_failed"
				}
			case "error_max_turns", "error_during_execution", "error_max_budget_usd", "error_max_structured_output_retries":
				code = "claude_" + rec.Subtype
			default:
				return Result{}, ErrProtocol
			}
			if len(*rec.PermissionDenials) > 0 {
				code = "claude_permission_denied"
			}
			if code != "" {
				if err := fail(code); err != nil {
					return Result{}, err
				}
			}
			terminal = true
		default:
			return Result{}, ErrProtocol
		}
	}
}

func readRecord(reader *bufio.Reader) ([]byte, error) {
	var record []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(fragment) > MaxRecordBytes-len(record) {
			return nil, ErrOversize
		}
		record = append(record, fragment...)
		switch {
		case err == nil:
			return record, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case errors.Is(err, io.EOF):
			if len(record) != 0 {
				return nil, ErrTruncated
			}
			return nil, io.EOF
		default:
			return nil, ErrRead
		}
	}
}

func resultUsage(rec record) (agent.Usage, error) {
	if rec.Usage.Input == nil || rec.Usage.Output == nil || rec.Usage.CacheRead == nil || rec.Usage.CacheCreation == nil {
		return agent.Usage{}, ErrProtocol
	}
	total := int64(0)
	for _, count := range []int64{*rec.Usage.Input, *rec.Usage.CacheRead, *rec.Usage.CacheCreation} {
		if count < 0 || count > math.MaxInt64-total {
			return agent.Usage{}, ErrProtocol
		}
		total += count
	}
	if *rec.Usage.Output < 0 {
		return agent.Usage{}, ErrProtocol
	}
	return agent.Usage{InputTokens: total, OutputTokens: *rec.Usage.Output}, nil
}
