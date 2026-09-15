package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// TestParseCapabilitiesRejectsUnknownNames is the point of the closed set.
//
// Free text would make an unrecognised capability a typo that silently
// disables a feature: the interface would simply never offer pause, and
// nothing would say why. Rejecting the write turns a silent misconfiguration
// into an error at the moment it is made.
func TestParseCapabilitiesRejectsUnknownNames(t *testing.T) {
	// Not in this list: "pause " with a trailing space. Surrounding whitespace
	// is trimmed, deliberately — forgiving a stray space in a JSON array is not
	// the same as accepting a name nothing recognises, and the danger this set
	// guards against is a capability that silently does nothing, not a tidy
	// one. Case is *not* forgiven: "Pause" is a different string and treating
	// it as equal would mean guessing at intent.
	for _, name := range []string{"teleport", "Pause", "", "token-accounting", "structuredToolCalls"} {
		if _, err := domain.ParseCapabilities([]string{name}); !errors.Is(err, domain.ErrUnknownCapability) {
			t.Errorf("ParseCapabilities(%q) = %v, want ErrUnknownCapability", name, err)
		}
	}
}

// TestParseCapabilitiesAcceptsTheKnownSet is the control: without it the test
// above would pass against a function that rejects everything.
func TestParseCapabilitiesAcceptsTheKnownSet(t *testing.T) {
	known := []string{"pause", "resume", "cancel", "send_instruction", "structured_tool_calls", "token_accounting"}

	capabilities, err := domain.ParseCapabilities(known)
	if err != nil {
		t.Fatalf("ParseCapabilities: %v", err)
	}
	if len(capabilities) != len(known) {
		t.Fatalf("got %d capabilities, want %d", len(capabilities), len(known))
	}

	// Surrounding whitespace is forgiven, which is why it is absent from the
	// rejection list above.
	if _, err := domain.ParseCapabilities([]string{"  pause  "}); err != nil {
		t.Errorf("a padded but valid capability was rejected: %v", err)
	}
}

// TestParseCapabilitiesNormalises pins that two equivalent declarations store
// identically, so a later comparison means what it looks like.
func TestParseCapabilitiesNormalises(t *testing.T) {
	first, err := domain.ParseCapabilities([]string{"resume", "pause", "pause"})
	if err != nil {
		t.Fatalf("ParseCapabilities: %v", err)
	}
	second, err := domain.ParseCapabilities([]string{"pause", "resume"})
	if err != nil {
		t.Fatalf("ParseCapabilities: %v", err)
	}

	if len(first) != 2 {
		t.Fatalf("duplicates were not collapsed: %v", first)
	}
	for i := range first {
		if first[i] != second[i] {
			t.Errorf("equivalent declarations stored differently: %v vs %v", first, second)
		}
	}
}

// TestValidateModelDoesNotCheckAgainstAList records a deliberate omission.
//
// Model names change far faster than this code does, and refusing an
// unrecognised one would make a new provider release unusable until somebody
// edited a constant. The provider rejects a model it does not have, which is
// the authority that stays current.
func TestValidateModelDoesNotCheckAgainstAList(t *testing.T) {
	for _, model := range []string{"claude-opus-5", "a-model-released-tomorrow", "gpt-99"} {
		if _, err := domain.ValidateModel(model); err != nil {
			t.Errorf("ValidateModel(%q) = %v, want nil", model, err)
		}
	}
	if _, err := domain.ValidateModel("   "); !errors.Is(err, domain.ErrInvalidAgent) {
		t.Error("a blank model was accepted")
	}
	if _, err := domain.ValidateModel(strings.Repeat("m", 300)); !errors.Is(err, domain.ErrInvalidAgent) {
		t.Error("an unbounded model identifier was accepted")
	}
}

// TestTaskBodyIsNotAltered is the property the whole untrusted-input argument
// rests on.
//
// Sanitising would make the stored value and the returned value disagree, and
// then nobody can tell what is actually stored. Bounded, and nothing else.
func TestTaskBodyIsNotAltered(t *testing.T) {
	bodies := []string{
		"  leading and trailing  ",
		"\n\tindented code block\n",
		"line one\r\nline two",
		"<script>alert(1)</script>",
		"${injected} $(also) `and`",
		"emoji 🙂 and ünïcødé",
		"",
	}

	for _, body := range bodies {
		stored, err := domain.ValidateTaskBody(body)
		if err != nil {
			t.Fatalf("ValidateTaskBody(%q) = %v", body, err)
		}
		if stored != body {
			t.Errorf("body was altered: stored %q, given %q", stored, body)
		}
	}
}

// TestTaskBodyIsBounded covers the limit, at it and one past it.
func TestTaskBodyIsBounded(t *testing.T) {
	atLimit := strings.Repeat("a", 50000)
	if stored, err := domain.ValidateTaskBody(atLimit); err != nil || stored != atLimit {
		t.Errorf("a body at the limit was refused or altered: %v", err)
	}
	if _, err := domain.ValidateTaskBody(atLimit + "a"); !errors.Is(err, domain.ErrInvalidTask) {
		t.Error("a body over the limit was accepted")
	}
}

// TestReadyRequiresRepository pins the invariant the database also carries.
func TestReadyRequiresRepository(t *testing.T) {
	if err := domain.ReadyRequiresRepository(domain.TaskReady, nil); !errors.Is(err, domain.ErrTaskNotReady) {
		t.Errorf("a ready task without a repository was accepted: %v", err)
	}
	if err := domain.ReadyRequiresRepository(domain.TaskDraft, nil); err != nil {
		t.Errorf("a draft without a repository was refused: %v", err)
	}
}
