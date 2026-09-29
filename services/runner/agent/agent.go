// Package agent is the runner-side provider adapter contract, and its first
// implementation: a deterministic fake provider (Unit M5.5b).
//
// The contract is context/architecture.md's CodingAgent, unchanged. The runner
// translates the ProviderEvents an adapter produces into the event contract
// (M5.3) and publishes them; an adapter never touches the broker. Claude Code
// (M6) implements the same interface, through the same runner, credentials and
// events — so a shortcut the fake could take and a real adapter could not would
// leave the chain it verifies different from the chain that runs.
package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// Capabilities is what an adapter supports. The runner consults it rather than
// assuming: no feature may assume every provider can pause, take structured
// tool calls, or account tokens.
type Capabilities struct {
	SendInstruction bool
	Pause           bool
	Resume          bool
	StructuredTools bool
	TokenAccounting bool
}

// StartRequest is what an adapter is given to begin. Identifiers only: the fake
// reads nothing from the repository and nothing from the task, and a real
// adapter will receive the task text through a separate, untrusted channel
// (M6), not here.
type StartRequest struct {
	SessionID uuid.UUID
	Model     string
	// Workdir is the checkout. The fake does not read it.
	Workdir string
}

// ProviderEvent is one thing the provider did, in the normalized vocabulary.
type ProviderEvent struct {
	Type    domain.EventType
	Payload any
	// Metadata is where provider-specific fields may live — namespaced, never
	// promoted into the normalized payload. The fake has none.
	Metadata map[string]any
}

// InstructionRequest is a follow-up instruction from a participant (M6/M7).
type InstructionRequest struct {
	Text string
}

// Usage is what a provider consumed.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// ErrUnsupported is returned by an operation the adapter declared it does not
// support. A caller that consulted Capabilities never sees it.
var ErrUnsupported = errors.New("the provider does not support this operation")

// CodingAgent is the adapter contract from context/architecture.md.
type CodingAgent interface {
	Capabilities(ctx context.Context) (Capabilities, error)
	Start(ctx context.Context, req StartRequest) (<-chan ProviderEvent, error)
	SendInstruction(ctx context.Context, req InstructionRequest) error
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	Cancel(ctx context.Context) error
	CollectUsage(ctx context.Context) (Usage, error)
	Close(ctx context.Context) error
}

// Models the fake provider understands. The outcome is chosen by the agent
// version's model — something every session already has — rather than by a new
// field or a test-only switch in production code.
const (
	// FakeModelSucceeds narrates a plan it does not carry out, then finishes.
	FakeModelSucceeds = "deterministic-v1"
	// FakeModelFails narrates, then reports a provider failure.
	FakeModelFails = "deterministic-fail-v1"
	// FakeFailureCode is the stable code the failing model reports.
	FakeFailureCode = "fake_failure"
)

// ErrUnknownModel is returned by Start for a model the fake does not have.
var ErrUnknownModel = errors.New("the fake provider has no such model")

// fakeScript is what the fake says, in order. Fixed text of its own, labelled
// as a fake wherever a person could see it, derived from nothing it was given.
var fakeScript = []string{
	"[fake provider] I am a deterministic fake agent. I read nothing and change nothing.",
	"[fake provider] Plan: 1) read the failing test, 2) find the cause, 3) fix it, 4) run the suite.",
	"[fake provider] I would now read the repository. I do not.",
	"[fake provider] I would now make a change. I do not.",
	"[fake provider] Done narrating. A real provider arrives with M6.",
}

// Fake is the deterministic fake provider.
type Fake struct {
	cancel context.CancelFunc
}

// NewFake returns the fake provider.
func NewFake() *Fake { return &Fake{} }

var _ CodingAgent = (*Fake)(nil)

// Capabilities are declared honestly: the fake does none of the optional
// things, so it says it does none of them.
func (f *Fake) Capabilities(context.Context) (Capabilities, error) {
	return Capabilities{}, nil
}

// Start emits the model's script on the returned channel and closes it. The
// same model produces the same events, every time.
func (f *Fake) Start(ctx context.Context, req StartRequest) (<-chan ProviderEvent, error) {
	var failing bool
	switch req.Model {
	case FakeModelSucceeds:
	case FakeModelFails:
		failing = true
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownModel, req.Model)
	}

	ctx, cancel := context.WithCancel(ctx)
	f.cancel = cancel
	script := fakeScript
	if failing {
		script = fakeScript[:2]
	}

	events := make(chan ProviderEvent)
	go func() {
		defer close(events)
		for i, text := range script {
			// Message ids are derived from the session and position, so two
			// runs of the same model on the same session are identical, and
			// two sessions differ only by the id they were given.
			id := uuid.NewSHA1(req.SessionID, []byte(fmt.Sprintf("message-%d", i)))
			if !send(ctx, events, ProviderEvent{
				Type:    domain.EventMessageCreated,
				Payload: domain.MessageCreated{MessageID: id, Role: "assistant", Text: text},
			}) {
				return
			}
		}
		if failing {
			send(ctx, events, ProviderEvent{
				Type: domain.EventProviderFailed,
				Payload: domain.ProviderFailed{
					Code: FakeFailureCode, Retryable: false,
					Message: "[fake provider] The failing model always fails here.",
				},
			})
		}
	}()
	return events, nil
}

func send(ctx context.Context, events chan<- ProviderEvent, event ProviderEvent) bool {
	select {
	case events <- event:
		return true
	case <-ctx.Done():
		return false
	}
}

// SendInstruction is not supported; Capabilities says so.
func (f *Fake) SendInstruction(context.Context, InstructionRequest) error { return ErrUnsupported }

// Pause is not supported; Capabilities says so.
func (f *Fake) Pause(context.Context) error { return ErrUnsupported }

// Resume is not supported; Capabilities says so.
func (f *Fake) Resume(context.Context) error { return ErrUnsupported }

// Cancel stops the script.
func (f *Fake) Cancel(context.Context) error {
	if f.cancel != nil {
		f.cancel()
	}
	return nil
}

// CollectUsage reports nothing consumed. TokenAccounting is false, so no caller
// should read it as a measurement.
func (f *Fake) CollectUsage(context.Context) (Usage, error) { return Usage{}, nil }

// Close releases the provider.
func (f *Fake) Close(ctx context.Context) error { return f.Cancel(ctx) }

// New returns the adapter for a provider, or an error for one the runner does
// not have.
func New(provider domain.Provider) (CodingAgent, error) {
	switch provider {
	case domain.ProviderFake:
		return NewFake(), nil
	default:
		return nil, fmt.Errorf("the runner has no adapter for provider %q", provider)
	}
}
