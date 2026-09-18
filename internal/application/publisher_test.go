package application_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// fakeOutbox records how each row was settled.
//
// Guarded, because the publisher runs on its own goroutine while the test
// reads what it recorded. The first version shared these fields unguarded and
// `go test -race` caught it — which plain `go test` had not, and is the reason
// the gate runs with the flag.
type fakeOutbox struct {
	mu          sync.Mutex
	pending     []application.ClaimedOutboxEvent
	completed   []uuid.UUID
	retried     []uuid.UUID
	released    []uuid.UUID
	terminated  map[uuid.UUID]string
	outstanding int
	// settled closes once every seeded row has been dealt with, so the test
	// waits on a signal rather than polling shared state or sleeping.
	settled chan struct{}
}

func newFakeOutbox(events ...application.ClaimedOutboxEvent) *fakeOutbox {
	return &fakeOutbox{
		pending:     events,
		outstanding: len(events),
		settled:     make(chan struct{}),
	}
}

func (f *fakeOutbox) Claim(context.Context, int, time.Duration, int) ([]application.ClaimedOutboxEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	claimed := f.pending
	f.pending = nil
	return claimed, nil
}

// settledOne counts down, closing the channel when nothing is left. The caller
// holds the lock.
func (f *fakeOutbox) settledOne() {
	f.outstanding--
	if f.outstanding == 0 {
		close(f.settled)
	}
}

func (f *fakeOutbox) Complete(_ context.Context, event application.ClaimedOutboxEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completed = append(f.completed, event.ID)
	f.settledOne()
	return nil
}

func (f *fakeOutbox) ReleaseUnstarted(_ context.Context, event application.ClaimedOutboxEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.released = append(f.released, event.ID)
	f.settledOne()
	return nil
}

func (f *fakeOutbox) Retry(_ context.Context, event application.ClaimedOutboxEvent, _ time.Duration, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retried = append(f.retried, event.ID)
	f.settledOne()
	return nil
}

func (f *fakeOutbox) Terminate(_ context.Context, event application.ClaimedOutboxEvent, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.terminated == nil {
		f.terminated = map[uuid.UUID]string{}
	}
	f.terminated[event.ID] = reason
	f.settledOne()
	return nil
}

// outcome is a snapshot the test reads once draining has finished.
type outcome struct {
	completed  []uuid.UUID
	retried    []uuid.UUID
	released   []uuid.UUID
	terminated map[uuid.UUID]string
}

func (f *fakeOutbox) outcome() outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	return outcome{completed: f.completed, retried: f.retried, released: f.released, terminated: f.terminated}
}

// fakeStarter returns whatever the test wants StartSessionWorkflow to say.
type fakeStarter struct{ err error }

func (f fakeStarter) StartSessionWorkflow(context.Context, uuid.UUID, uuid.UUID) error {
	return f.err
}

func event(attempts int32) application.ClaimedOutboxEvent {
	return application.ClaimedOutboxEvent{
		ID:          uuid.New(),
		WorkspaceID: uuid.New(),
		Topic:       application.TopicSessionCreated,
		SubjectID:   uuid.New(),
		Attempts:    attempts,
		Claimant:    uuid.New(),
	}
}

// drain runs the publisher until every seeded row has been settled.
//
// Waits on the fake's signal rather than a duration: a sleep long enough to be
// reliable is long enough to make the suite slow, and one short enough to be
// quick is a flake waiting for a busy machine.
func drain(t *testing.T, outbox *fakeOutbox, starter application.WorkflowStarter) outcome {
	t.Helper()
	publisher := application.NewPublisher(outbox, starter,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		publisher.Run(ctx)
	}()

	select {
	case <-outbox.settled:
	case <-time.After(5 * time.Second):
		t.Fatal("the publisher did not settle every row")
	}
	cancel()
	<-done
	return outbox.outcome()
}

// TestAnAlreadyStartedWorkflowCompletesTheRow is the duplicate delivery this
// design expects rather than guards against.
//
// The publisher is at-least-once: a crash between starting the workflow and
// completing the row produces exactly this. Treating "already started" as a
// failure would retry the row forever, on work that is already done.
func TestAnAlreadyStartedWorkflowCompletesTheRow(t *testing.T) {
	outbox := newFakeOutbox(event(1))
	settled := drain(t, outbox, fakeStarter{err: application.ErrWorkflowAlreadyStarted})

	if len(settled.completed) != 1 {
		t.Errorf("completed %d rows, want 1 — a duplicate delivery is success, not failure",
			len(settled.completed))
	}
	if len(settled.retried) != 0 || len(settled.terminated) != 0 {
		t.Errorf("a duplicate was retried or terminated: retried=%d terminated=%d",
			len(settled.retried), len(settled.terminated))
	}
}

// TestARejectedStartIsTerminal separates the two error kinds.
//
// A malformed start does not become well formed by waiting, so retrying it
// spends the budget a transient failure needs.
func TestARejectedStartIsTerminal(t *testing.T) {
	outbox := newFakeOutbox(event(1))
	settled := drain(t, outbox, fakeStarter{err: application.ErrWorkflowStartRejected})

	if len(settled.terminated) != 1 {
		t.Errorf("terminated %d rows, want 1", len(settled.terminated))
	}
	if len(settled.retried) != 0 {
		t.Error("a rejected start was retried, which cannot help")
	}
}

// TestATransientFailureIsRetried is the other half.
func TestATransientFailureIsRetried(t *testing.T) {
	outbox := newFakeOutbox(event(1))
	settled := drain(t, outbox, fakeStarter{err: errors.New("temporal is briefly unreachable")})

	if len(settled.retried) != 1 {
		t.Errorf("retried %d rows, want 1", len(settled.retried))
	}
	if len(settled.terminated) != 0 {
		t.Error("a transient failure was terminated, losing work that would have succeeded")
	}
}

// TestTheAttemptCeilingBoundsTheDuplicateGuarantee is why a row cannot retry
// forever.
//
// The workflow id stops a second workflow only while Temporal still remembers
// the closed execution. A row retrying indefinitely would outlive that, and
// the duplicate would return on the row least likely to be looked at — the one
// that has been failing for weeks.
func TestTheAttemptCeilingBoundsTheDuplicateGuarantee(t *testing.T) {
	exhausted := event(application.OutboxMaxAttempts)
	outbox := newFakeOutbox(exhausted)
	settled := drain(t, outbox, fakeStarter{err: errors.New("temporal is still unreachable")})

	if len(settled.retried) != 0 {
		t.Error("a row past its attempt ceiling was retried again")
	}
	reason, terminated := settled.terminated[exhausted.ID]
	if !terminated {
		t.Fatal("a row past its attempt ceiling was not terminated")
	}
	if reason == "" {
		t.Error("the terminated row kept no reason, so nobody can act on it")
	}
}

// TestAnUnknownTopicIsTerminatedRatherThanRetried keeps the queue from growing
// behind a row nothing understands.
func TestAnUnknownTopicIsTerminatedRatherThanRetried(t *testing.T) {
	unknown := event(1)
	unknown.Topic = "something.nobody.handles"
	outbox := newFakeOutbox(unknown)
	settled := drain(t, outbox, fakeStarter{})

	if len(settled.terminated) != 1 {
		t.Errorf("terminated %d rows, want 1 — an unhandled topic retried forever "+
			"is the queue growing silently", len(settled.terminated))
	}
}

// TestBackoffGrowsAndThenStops checks the delay is bounded.
//
// Unbounded exponential backoff would push a row so far out that it never
// reaches its attempt ceiling in any useful window — neither retried nor
// terminated, just quietly distant.
func TestBackoffGrowsAndThenStops(t *testing.T) {
	first := application.OutboxBackoff(1)
	second := application.OutboxBackoff(2)
	if second <= first {
		t.Errorf("backoff did not grow: %s then %s", first, second)
	}

	const ceiling = 2 * time.Minute
	for attempts := int32(1); attempts <= application.OutboxMaxAttempts; attempts++ {
		if got := application.OutboxBackoff(attempts); got > ceiling {
			t.Errorf("backoff at attempt %d is %s, above the %s ceiling", attempts, got, ceiling)
		}
	}
}
