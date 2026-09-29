package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/runner/agent"
)

func collect(t *testing.T, model string, session uuid.UUID) []agent.ProviderEvent {
	t.Helper()
	events, err := agent.NewFake().Start(context.Background(), agent.StartRequest{SessionID: session, Model: model})
	if err != nil {
		t.Fatalf("start %s: %v", model, err)
	}
	var out []agent.ProviderEvent
	for event := range events {
		out = append(out, event)
	}
	return out
}

func encode(t *testing.T, events []agent.ProviderEvent) string {
	t.Helper()
	raw, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// TestTheFakeIsDeterministic: the same model on the same session produces the
// same events, byte for byte.
func TestTheFakeIsDeterministic(t *testing.T) {
	session := uuid.New()
	for _, model := range []string{agent.FakeModelSucceeds, agent.FakeModelFails} {
		if a, b := encode(t, collect(t, model, session)), encode(t, collect(t, model, session)); a != b {
			t.Errorf("%s produced different events on two runs", model)
		}
	}
}

func TestTheSucceedingModelNarratesAndFinishes(t *testing.T) {
	events := collect(t, agent.FakeModelSucceeds, uuid.New())
	if len(events) == 0 {
		t.Fatal("no events")
	}
	for _, event := range events {
		if event.Type != domain.EventMessageCreated {
			t.Errorf("event %s; the succeeding model only sends messages", event.Type)
		}
		// Labelled as a fake wherever a person could see it.
		if msg := event.Payload.(domain.MessageCreated); !strings.HasPrefix(msg.Text, "[fake provider]") {
			t.Errorf("message %q is not labelled as the fake's", msg.Text)
		}
	}
}

func TestTheFailingModelEndsInAProviderFailure(t *testing.T) {
	events := collect(t, agent.FakeModelFails, uuid.New())
	last := events[len(events)-1]
	failed, ok := last.Payload.(domain.ProviderFailed)
	if last.Type != domain.EventProviderFailed || !ok || failed.Code != agent.FakeFailureCode {
		t.Errorf("last event = %+v, want provider.failed %s", last, agent.FakeFailureCode)
	}
}

// TestTheFakeDeclaresWhatItCannotDo: capabilities are honest, and the
// operations it declines return ErrUnsupported rather than pretending.
func TestTheFakeDeclaresWhatItCannotDo(t *testing.T) {
	fake := agent.NewFake()
	caps, _ := fake.Capabilities(context.Background())
	if caps != (agent.Capabilities{}) {
		t.Errorf("capabilities = %+v; the fake supports none of the optional operations", caps)
	}
	for name, op := range map[string]func() error{
		"SendInstruction": func() error { return fake.SendInstruction(context.Background(), agent.InstructionRequest{}) },
		"Pause":           func() error { return fake.Pause(context.Background()) },
		"Resume":          func() error { return fake.Resume(context.Background()) },
	} {
		if err := op(); !errors.Is(err, agent.ErrUnsupported) {
			t.Errorf("%s = %v, want ErrUnsupported", name, err)
		}
	}
}

func TestUnknownModelsAndProvidersAreRefused(t *testing.T) {
	if _, err := agent.NewFake().Start(context.Background(), agent.StartRequest{Model: "gpt-9"}); !errors.Is(err, agent.ErrUnknownModel) {
		t.Errorf("unknown model = %v, want ErrUnknownModel", err)
	}
	if _, err := agent.New(domain.Provider("claude")); err == nil {
		t.Error("the runner claimed an adapter for a provider it does not have")
	}
}
