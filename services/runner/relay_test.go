package main

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
	"github.com/jigmetnamgyal/weave/services/runner/agent"
)

func start(t *testing.T, ctx context.Context, model string) <-chan agent.ProviderEvent {
	t.Helper()
	adapter, err := agent.New(domain.ProviderFake)
	if err != nil {
		t.Fatal(err)
	}
	events, err := adapter.Start(ctx, agent.StartRequest{SessionID: uuid.New(), Model: model})
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// TestAnInterruptedRunIsNeverSuccess: a signal mid-run closes the adapter's
// channel early. That must not exit 0, which ends a session review_ready.
func TestAnInterruptedRunIsNeverSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := start(t, ctx, "deterministic-v1")
	sent := 0
	code := relay(ctx, events, func(agent.ProviderEvent) error {
		sent++
		if sent == 2 {
			cancel() // SIGTERM, between events
		}
		return nil
	}, func() {})
	if code != exitInterrupted {
		t.Errorf("exit = %d after %d events, want %d (interrupted)", code, sent, exitInterrupted)
	}
}

func TestHowTheRelayExits(t *testing.T) {
	cases := map[string]struct {
		model   string
		sendErr error
		want    int
	}{
		"finished":           {"deterministic-v1", nil, 0},
		"provider failed":    {"deterministic-fail-v1", nil, exitProviderFailed},
		"unconfirmed events": {"deterministic-v1", errors.New("no ack"), exitPublishFailed},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			code := relay(ctx, start(t, ctx, tc.model), func(agent.ProviderEvent) error { return tc.sendErr }, func() {})
			if code != tc.want {
				t.Errorf("exit = %d, want %d", code, tc.want)
			}
		})
	}
}
