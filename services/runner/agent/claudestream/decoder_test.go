package claudestream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/uuid"
	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/runner/agent"
)

var session = uuid.MustParse("10000000-0000-4000-8000-000000000001")

const messageUUID = "20000000-0000-4000-8000-000000000001"

// fixture loads explicitly synthetic NDJSON test evidence.
func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".ndjson")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// assistant builds a synthetic completed text block with stable identity.
func assistant(text string) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "session_id": "synthetic-provider-session", "uuid": messageUUID,
		"message": map[string]any{"id": "shared-api-id", "role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
	return string(b) + "\n"
}

// terminal builds a valid single-turn success result with complete usage.
func terminal() string {
	return `{"type":"result","session_id":"synthetic-provider-session","subtype":"success","is_error":false,"usage":{"input_tokens":11,"output_tokens":19,"cache_read_input_tokens":13,"cache_creation_input_tokens":17},"permission_denials":[]}` + "\n"
}

// decode collects normalized events without a broker or provider invocation.
func decode(input io.Reader) (Result, []agent.ProviderEvent, error) {
	var events []agent.ProviderEvent
	result, err := Decode(context.Background(), input, session, func(e agent.ProviderEvent) error { events = append(events, e); return nil })
	return result, events, err
}

// TestSyntheticCompleteBlocksAreEmittedOnce checks short reads, replay absorption and scoped block identity.
func TestSyntheticCompleteBlocksAreEmittedOnce(t *testing.T) {
	for name, wrap := range map[string]func(io.Reader) io.Reader{"normal": func(r io.Reader) io.Reader { return r }, "one-byte": iotest.OneByteReader, "short-reads": iotest.HalfReader} {
		t.Run(name, func(t *testing.T) {
			result, events, err := decode(wrap(strings.NewReader(fixture(t, "success"))))
			if err != nil || result.Failed || result.Usage != (agent.Usage{InputTokens: 41, OutputTokens: 19}) {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			if len(events) != 2 {
				t.Fatalf("want two completed blocks, got %d", len(events))
			}
			first := events[0].Payload.(domain.MessageCreated)
			second := events[1].Payload.(domain.MessageCreated)
			if first.Text != "Synthetic complete text." || second.Text != "Another complete block." || first.MessageID == second.MessageID {
				t.Fatal("deltas, replay or shared API identity corrupted completed blocks")
			}
			for _, event := range events {
				assertContract(t, event)
			}
			_, again, err := decode(strings.NewReader(fixture(t, "success")))
			if err != nil || again[0].Payload.(domain.MessageCreated).MessageID != first.MessageID {
				t.Fatal("message identity is not deterministic")
			}
			var different []agent.ProviderEvent
			_, err = Decode(context.Background(), strings.NewReader(fixture(t, "success")), uuid.New(), func(e agent.ProviderEvent) error { different = append(different, e); return nil })
			if err != nil || different[0].Payload.(domain.MessageCreated).MessageID == first.MessageID {
				t.Fatal("message identity is not scoped to the Weave session")
			}
		})
	}
}

// assertContract validates normalized payloads against the real event envelope and safe failure policy.
func assertContract(t *testing.T, event agent.ProviderEvent) {
	t.Helper()
	if event.Metadata != nil {
		t.Fatal("raw provider metadata escaped")
	}
	payload, err := json.Marshal(event.Payload)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(domain.EventEnvelope{EventID: uuid.New(), Producer: uuid.New(), WorkspaceID: uuid.New(), SessionID: session, CorrelationID: uuid.New(), SchemaVersion: "1.0", OccurredAt: time.Now().UTC(), Type: event.Type, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.DecodeEvent(raw); err != nil {
		t.Fatalf("normalized event violates domain contract: %v", err)
	}
	if event.Type == domain.EventProviderFailed && (bytes.Contains(raw, []byte("SYNTHETIC_SECRET")) || bytes.Contains(raw, []byte("private-path"))) {
		t.Fatal("raw provider diagnostics escaped")
	}
}

// TestFailuresCannotBecomeSuccessfulResults preserves assistant/result failures despite a success subtype.
func TestFailuresCannotBecomeSuccessfulResults(t *testing.T) {
	cases := map[string]string{
		"error result":                 fixture(t, "failure"),
		"success marked error":         strings.Replace(terminal(), `"is_error":false`, `"is_error":true`, 1),
		"permission denied":            strings.Replace(terminal(), `"permission_denials":[]`, `"permission_denials":[{"tool_name":"SYNTHETIC_SECRET"}]`, 1),
		"assistant error then success": strings.Replace(assistant("SYNTHETIC_SECRET"), `"type":"assistant"`, `"type":"assistant","error":"SYNTHETIC_SECRET"`, 1) + terminal(),
		"assistant abort then success": strings.Replace(assistant("partial"), `"type":"assistant"`, `"type":"assistant","aborted":true`, 1) + terminal(),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			result, events, err := decode(strings.NewReader(input))
			if err != nil || !result.Failed || len(events) != 1 || events[0].Type != domain.EventProviderFailed {
				t.Fatalf("result=%+v events=%d err=%v", result, len(events), err)
			}
			failure := events[0].Payload.(domain.ProviderFailed)
			if failure.Retryable || failure.Code == "" {
				t.Fatal("failure is not conservative and stable")
			}
			assertContract(t, events[0])
		})
	}
}

// TestInvalidStreamsAreRefusedWithSafeCategories checks framing, schema and usage refusals without reflecting content.
func TestInvalidStreamsAreRefusedWithSafeCategories(t *testing.T) {
	cases := []struct {
		name, input string
		want        error
	}{
		{"empty", "", ErrIncomplete},
		{"missing result", assistant("text"), ErrIncomplete},
		{"unterminated JSON", `{"type":"result"`, ErrTruncated},
		{"no final newline", strings.TrimSuffix(terminal(), "\n"), ErrTruncated},
		{"malformed", `{"SYNTHETIC_SECRET":` + "\n", ErrMalformed},
		{"non-object", "[]\n", ErrMalformed},
		{"null", "null\n", ErrMalformed},
		{"blank", "\n", ErrMalformed},
		{"unknown kind", `{"type":"SYNTHETIC_SECRET","session_id":"s"}` + "\n", ErrProtocol},
		{"missing provider session", strings.Replace(terminal(), `"session_id":"synthetic-provider-session",`, "", 1), ErrProtocol},
		{"cross-session", assistant("text") + strings.Replace(terminal(), "synthetic-provider-session", "another-session", 1), ErrProtocol},
		{"missing result flag", strings.Replace(terminal(), `"is_error":false,`, "", 1), ErrProtocol},
		{"unknown result", strings.Replace(terminal(), `"subtype":"success"`, `"subtype":"future"`, 1), ErrProtocol},
		{"missing permission denials", strings.Replace(terminal(), `,"permission_denials":[]`, "", 1), ErrProtocol},
		{"null permission denials", strings.Replace(terminal(), `"permission_denials":[]`, `"permission_denials":null`, 1), ErrProtocol},
		{"fractional usage", strings.Replace(terminal(), `"input_tokens":11`, `"input_tokens":1.5`, 1), ErrMalformed},
		{"negative usage", strings.Replace(terminal(), `"output_tokens":19`, `"output_tokens":-1`, 1), ErrProtocol},
		{"missing usage", strings.Replace(terminal(), `"input_tokens":11,`, "", 1), ErrProtocol},
		{"null usage", strings.Replace(terminal(), `"cache_read_input_tokens":13`, `"cache_read_input_tokens":null`, 1), ErrProtocol},
		{"usage overflow", strings.Replace(terminal(), `"input_tokens":11`, fmt.Sprintf(`"input_tokens":%d`, math.MaxInt64), 1), ErrProtocol},
		{"extra result", terminal() + terminal(), ErrProtocol},
		{"text after result", terminal() + assistant("text"), ErrProtocol},
		{"bad UUID", strings.Replace(assistant("text"), messageUUID, "bad", 1), ErrProtocol},
		{"nil UUID", strings.Replace(assistant("text"), messageUUID, uuid.Nil.String(), 1), ErrProtocol},
		{"not assistant role", strings.Replace(assistant("text"), `"role":"assistant"`, `"role":"user"`, 1), ErrProtocol},
		{"missing text", strings.Replace(assistant("text"), `"text":"text",`, "", 1), ErrProtocol},
		{"conflicting replay", assistant("one") + assistant("two") + terminal(), ErrProtocol},
		{"NUL text", assistant("\x00"), ErrOversize},
		{"text rune limit", assistant(strings.Repeat("x", 32769)), ErrOversize},
		{"escaped payload limit", assistant(strings.Repeat("\x01", 12000)), ErrOversize},
		{"invalid UTF8", string([]byte{'{', '"', 0xff, '"', ':', '1', '}', '\n'}), ErrMalformed},
		{"oversize record", strings.Repeat("x", MaxRecordBytes) + "\n", ErrOversize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := decode(strings.NewReader(tc.input))
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "SYNTHETIC_SECRET") {
				t.Fatal("error reflected untrusted content")
			}
		})
	}
}

// TestCountLimitsBoundMemoryAndWork exercises record and identity caps.
func TestCountLimitsBoundMemoryAndWork(t *testing.T) {
	t.Run("records", func(t *testing.T) {
		input := strings.Repeat(`{"type":"system","session_id":"s"}`+"\n", MaxRecords) + terminal()
		_, _, err := decode(strings.NewReader(input))
		if !errors.Is(err, ErrLimit) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("messages", func(t *testing.T) {
		var input strings.Builder
		for i := 0; i <= MaxMessages; i++ {
			input.WriteString(strings.Replace(assistant("text"), messageUUID, uuid.New().String(), 1))
		}
		_, _, err := decode(strings.NewReader(input.String()))
		if !errors.Is(err, ErrLimit) {
			t.Fatalf("got %v", err)
		}
	})
}

type failingReader struct{}

// Read simulates a reader error containing content that must never escape.
func (failingReader) Read([]byte) (int, error) { return 0, errors.New("SYNTHETIC_SECRET") }

type cancellingReader struct {
	cancel context.CancelFunc
	reader io.Reader
}

// Read cancels during a successful read to exercise delivery suppression.
func (r cancellingReader) Read(p []byte) (int, error) { r.cancel(); return r.reader.Read(p) }

// TestReadDeliveryAndCancellationErrorsDoNotLeak checks safe callback/read errors and cancellation during reads.
func TestReadDeliveryAndCancellationErrorsDoNotLeak(t *testing.T) {
	if _, _, err := decode(failingReader{}); err != ErrRead {
		t.Fatalf("read error=%v", err)
	}
	_, err := Decode(context.Background(), strings.NewReader(assistant("text")), session, func(agent.ProviderEvent) error { return errors.New("SYNTHETIC_SECRET") })
	if err != ErrDelivery {
		t.Fatalf("delivery error=%v", err)
	}
	_, err = Decode(context.Background(), strings.NewReader(fixture(t, "failure")), session, func(agent.ProviderEvent) error { return errors.New("SYNTHETIC_SECRET") })
	if err != ErrDelivery {
		t.Fatalf("failure delivery error=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err = Decode(ctx, cancellingReader{cancel: cancel, reader: strings.NewReader(assistant("text"))}, session, func(agent.ProviderEvent) error { calls++; return nil })
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("cancel error=%v emissions=%d", err, calls)
	}
}

// TestBoundaryRecordsAndToolOnlyContent checks the exact framing boundary and ignored thinking.
func TestBoundaryRecordsAndToolOnlyContent(t *testing.T) {
	// A known ignored object padded to exactly the framing limit is accepted.
	raw := `{"type":"system","session_id":"synthetic-provider-session","ignored":""}`
	raw = strings.Replace(raw, `"ignored":""`, `"ignored":"`+strings.Repeat("x", MaxRecordBytes-len(raw)-1)+`"`, 1) + "\n"
	if len(raw) != MaxRecordBytes {
		t.Fatal("bad boundary fixture")
	}
	if _, events, err := decode(strings.NewReader(raw + terminal())); err != nil || len(events) != 0 {
		t.Fatalf("boundary: events=%d err=%v", len(events), err)
	}
	input := strings.Replace(assistant("text"), `"type":"text"`, `"type":"thinking"`, 1) + terminal()
	if _, events, err := decode(strings.NewReader(input)); err != nil || len(events) != 0 {
		t.Fatalf("thinking: events=%d err=%v", len(events), err)
	}
}

// TestClaudeRemainsDisabled prevents an offline decoder from registering paid runtime execution.
func TestClaudeRemainsDisabled(t *testing.T) {
	if _, err := agent.New(domain.ProviderClaudeCode); err == nil {
		t.Fatal("offline decoder must not enable paid execution")
	}
}

// Cancelling at EOF must not turn an interrupted run into a success.
type cancellingEOFReader struct {
	reader io.Reader
	cancel context.CancelFunc
}

// Read cancels on EOF so a terminal success cannot hide an interrupted run.
func (r cancellingEOFReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if errors.Is(err, io.EOF) {
		r.cancel()
	}
	return n, err
}

// TestCancellationAtEOFOverridesTerminalSuccess verifies cancellation wins at the final read.
func TestCancellationAtEOFOverridesTerminalSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := Decode(ctx, cancellingEOFReader{reader: strings.NewReader(terminal()), cancel: cancel}, session, func(agent.ProviderEvent) error { return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

// FuzzDecode checks bounded decoding never panics or emits invalid domain payloads.
func FuzzDecode(f *testing.F) {
	f.Add([]byte(assistant("Synthetic text") + terminal()))
	f.Add([]byte("null\n"))
	f.Add([]byte("{\n"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, events, err := decode(bytes.NewReader(raw))
		for _, event := range events {
			assertContract(t, event)
		}
		if err != nil && len(err.Error()) > 100 {
			t.Fatal("decoder returned non-category error")
		}
	})
}

// TestFailureIsTheFinalEvent preserves the failing-history ordering invariant.
func TestFailureIsTheFinalEvent(t *testing.T) {
	first := strings.Replace(assistant("failed block"), `"type":"assistant"`, `"type":"assistant","error":"SYNTHETIC_SECRET"`, 1)
	late := strings.Replace(assistant("late completed text"), messageUUID, "20000000-0000-4000-8000-000000000002", 1)
	result, events, err := decode(strings.NewReader(first + late + terminal()))
	if err != nil || !result.Failed || len(events) != 2 {
		t.Fatalf("result=%+v events=%d error=%v", result, len(events), err)
	}
	if events[0].Type != domain.EventMessageCreated || events[1].Type != domain.EventProviderFailed {
		t.Fatal("failure must follow all completed messages")
	}
	if events[0].Payload.(domain.MessageCreated).Text != "late completed text" {
		t.Fatal("completed text was lost")
	}
	for _, event := range events {
		assertContract(t, event)
	}
}

// TestUnknownContentBlocksAreRefused makes schema drift visible, including mixed blocks.
func TestUnknownContentBlocksAreRefused(t *testing.T) {
	for _, kind := range []string{"future_content", "", "SYNTHETIC_SECRET"} {
		for _, aborted := range []bool{false, true} {
			input := strings.Replace(assistant("text"), `"content":[`, `"content":[{"type":"`+kind+`"},`, 1)
			if aborted {
				input = strings.Replace(input, `"type":"assistant"`, `"type":"assistant","aborted":true`, 1)
			}
			_, events, err := decode(strings.NewReader(input + terminal()))
			if err != ErrProtocol || len(events) != 0 {
				t.Fatalf("kind=%q aborted=%v events=%d error=%v", kind, aborted, len(events), err)
			}
		}
	}
}

// TestKnownNonTextBlocksRemainIgnored keeps the intentionally supported subset usable.
func TestKnownNonTextBlocksRemainIgnored(t *testing.T) {
	for _, kind := range []string{"tool_use", "thinking", "redacted_thinking"} {
		input := strings.Replace(assistant("not persisted"), `"type":"text"`, `"type":"`+kind+`"`, 1)
		result, events, err := decode(strings.NewReader(input + terminal()))
		if err != nil || result.Failed || len(events) != 0 {
			t.Fatalf("kind=%q result=%+v events=%d error=%v", kind, result, len(events), err)
		}
	}
}

// TestInvalidTailDoesNotEmitPrematureFailure requires framing validation before final emission.
func TestInvalidTailDoesNotEmitPrematureFailure(t *testing.T) {
	failed := strings.Replace(assistant("failed block"), `"type":"assistant"`, `"type":"assistant","aborted":true`, 1)
	for _, tail := range []string{"", terminal() + terminal(), terminal() + "{"} {
		_, events, err := decode(strings.NewReader(failed + tail))
		if err == nil || len(events) != 0 {
			t.Fatalf("events=%d error=%v", len(events), err)
		}
	}
}
