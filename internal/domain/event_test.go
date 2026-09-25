package domain_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// fixturesDir is the contract's own examples, read in place rather than
// copied, so a fixture edited in contracts/events is the one tested here.
const fixturesDir = "../../contracts/events/fixtures"

// TestEveryFixtureDecodesAsItsNameSays is the drift check between the schema
// files and the decoder — the same shape as M3.2's contract check.
//
// A fixture's name states its expected outcome, and every file in the
// directory must have a recognised name, so a new fixture cannot be added
// without saying what it proves.
func TestEveryFixtureDecodesAsItsNameSays(t *testing.T) {
	entries, err := os.ReadDir(fixturesDir)
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no fixtures; the contract has nothing checking it")
	}

	for _, entry := range entries {
		name := entry.Name()
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(fixturesDir, name))
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			event, err := domain.DecodeEvent(raw)

			switch {
			case strings.HasSuffix(name, ".valid.json"):
				if err != nil {
					t.Fatalf("a valid fixture was refused: %v", err)
				}
			case strings.HasSuffix(name, ".unknown-field.json"):
				if err != nil {
					t.Fatalf("an unknown field must be ignored, not refused: %v", err)
				}
				// Ignored includes not stored: the re-encoded payload carries
				// only what the schema knows.
				var stored map[string]any
				if err := json.Unmarshal(event.Payload, &stored); err != nil {
					t.Fatalf("stored payload: %v", err)
				}
				for key := range stored {
					if key == "tokens" || key == "provider_detail" {
						t.Errorf("unknown payload field %q was stored", key)
					}
				}
			case strings.HasSuffix(name, ".unknown-major.json"):
				assertRefused(t, err, domain.RefusalUnknownMajor)
			case name == "unknown-type.json":
				assertRefused(t, err, domain.RefusalUnknownType)
			default:
				t.Fatalf("fixture %q does not say what it proves; name it *.valid, *.unknown-field, "+
					"*.unknown-major or unknown-type", name)
			}
		})
	}
}

// TestRefusalsKeepTheIdentifiersThatParsed: a quarantined event is only
// useful to an operator if it can be matched to its producer's logs.
func TestRefusalsKeepTheIdentifiersThatParsed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(fixturesDir, "message.created.v2.unknown-major.json"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = domain.DecodeEvent(raw)
	var refusal *domain.EventRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("DecodeEvent = %v, want a refusal", err)
	}
	if refusal.EventID == uuid.Nil || refusal.SessionID == uuid.Nil || refusal.SchemaVersion != "2.0" {
		t.Errorf("refusal kept %+v; want the event id, session id and version that parsed", refusal)
	}
}

func TestEventsAreRefusedForTheRightReason(t *testing.T) {
	valid, err := os.ReadFile(filepath.Join(fixturesDir, "message.created.v1.valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	mutate := func(edit func(map[string]any)) []byte {
		var envelope map[string]any
		if err := json.Unmarshal(valid, &envelope); err != nil {
			t.Fatal(err)
		}
		edit(envelope)
		out, _ := json.Marshal(envelope)
		return out
	}

	cases := map[string]struct {
		raw  []byte
		want domain.EventRefusal
	}{
		"not JSON":            {[]byte("{nope"), domain.RefusalMalformed},
		"missing event id":    {mutate(func(e map[string]any) { delete(e, "event_id") }), domain.RefusalMalformed},
		"missing producer":    {mutate(func(e map[string]any) { delete(e, "producer") }), domain.RefusalMalformed},
		"version not semver":  {mutate(func(e map[string]any) { e["schema_version"] = "one" }), domain.RefusalMalformed},
		"empty text":          {mutate(func(e map[string]any) { e["payload"].(map[string]any)["text"] = "" }), domain.RefusalInvalidPayload},
		"unknown role":        {mutate(func(e map[string]any) { e["payload"].(map[string]any)["role"] = "root" }), domain.RefusalInvalidPayload},
		"payload not object":  {mutate(func(e map[string]any) { e["payload"] = "hello" }), domain.RefusalInvalidPayload},
		"oversize":            {append(valid, []byte(strings.Repeat(" ", domain.MaxEventBytes))...), domain.RefusalOversize},
		"text over the limit": {mutate(func(e map[string]any) { e["payload"].(map[string]any)["text"] = strings.Repeat("x", 32769) }), domain.RefusalInvalidPayload},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := domain.DecodeEvent(tc.raw)
			assertRefused(t, err, tc.want)
		})
	}
}

// TestProviderFailedRequiresEveryField: a missing field is not an empty one.
// A payload without `message` was stored with a synthesized "", which the
// v1 schema does not allow.
func TestProviderFailedRequiresEveryField(t *testing.T) {
	valid, err := os.ReadFile(filepath.Join(fixturesDir, "provider.failed.v1.valid.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"code", "retryable", "message"} {
		var envelope map[string]any
		_ = json.Unmarshal(valid, &envelope)
		delete(envelope["payload"].(map[string]any), field)
		raw, _ := json.Marshal(envelope)
		_, err := domain.DecodeEvent(raw)
		assertRefused(t, err, domain.RefusalInvalidPayload)
	}
}

// TestSafeTextNeverSplitsARune: the quarantine insert must never fail on its
// own input, or the record meant to stop an event vanishing cannot be written.
func TestSafeTextNeverSplitsARune(t *testing.T) {
	cases := map[string]struct {
		in   string
		n    int
		want string
	}{
		"short is untouched":         {"weave.session", 32, "weave.session"},
		"cut before a split rune":    {"ab€", 4, "ab"},
		"invalid bytes are replaced": {"a\xffb", 16, "a\uFFFDb"},
		"NUL is replaced":            {"a\x00b", 16, "a\uFFFDb"},
	}
	for name, tc := range cases {
		got := domain.SafeText(tc.in, tc.n)
		if got != tc.want {
			t.Errorf("%s: SafeText(%q, %d) = %q, want %q", name, tc.in, tc.n, got, tc.want)
		}
		if !utf8.ValidString(got) || len(got) > tc.n {
			t.Errorf("%s: %q is not valid UTF-8 within %d bytes", name, got, tc.n)
		}
	}
}

func TestTransportNamesFollowTheConvention(t *testing.T) {
	if got := domain.TransportName(domain.EventMessageCreated, 1); got != "session.message.created.v1" {
		t.Errorf("TransportName = %q", got)
	}
	// And the schema files are named by it, so the mapping is not only prose.
	for _, eventType := range []domain.EventType{domain.EventMessageCreated, domain.EventProviderFailed} {
		name := strings.TrimPrefix(domain.TransportName(eventType, 1), "session.") + ".schema.json"
		if _, err := os.Stat(filepath.Join("../../contracts/events/schemas", name)); err != nil {
			t.Errorf("no schema file %s for %s: %v", name, eventType, err)
		}
	}
}

func TestSubjectsRoundTripAndRefuseOthers(t *testing.T) {
	id := uuid.New()
	subject := domain.EventSubject("weave.session", id)
	if subject != "weave.session."+id.String()+".events" {
		t.Errorf("subject = %q", subject)
	}
	if got, err := domain.SessionFromSubject("weave.session", subject); err != nil || got != id {
		t.Errorf("SessionFromSubject = %v, %v", got, err)
	}
	for _, bad := range []string{
		"other.session." + id.String() + ".events",
		"weave.session." + id.String() + ".commands",
		"weave.session.not-a-uuid.events",
	} {
		_, err := domain.SessionFromSubject("weave.session", bad)
		assertRefused(t, err, domain.RefusalUnparseableSubject)
	}
}

func assertRefused(t *testing.T, err error, want domain.EventRefusal) {
	t.Helper()
	var refusal *domain.EventRefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want refusal %q", err, want)
	}
	if refusal.Reason != want {
		t.Errorf("refused as %q, want %q (%v)", refusal.Reason, want, err)
	}
	if !errors.Is(err, domain.ErrEventRefused) {
		t.Error("a refusal does not match ErrEventRefused")
	}
}
