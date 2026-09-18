package application

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
)

// ErrWorkflowAlreadyStarted reports that a workflow with this id is already
// running or has already run.
//
// **Not a failure.** The publisher is at-least-once, so a row delivered twice
// is expected — a crash between starting the workflow and completing the row
// produces exactly this. Treating it as an error would retry forever; treating
// it as success is what makes the workflow id the deduplication it is meant to
// be.
var ErrWorkflowAlreadyStarted = errors.New("a workflow for this session has already been started")

// ErrWorkflowStartRejected reports a start that will never succeed.
//
// Distinct from a transport failure, because retrying cannot help: a malformed
// payload does not become well formed by waiting.
var ErrWorkflowStartRejected = errors.New("the workflow start was rejected")

// WorkflowStarter starts the durable workflow for a session.
//
// The workflow id is the session id, which is the second half of this unit's
// deduplication: the idempotency key stops a retried request creating a second
// session, and the id stops a retried publish creating a second workflow for
// the session that exists.
type WorkflowStarter interface {
	StartSessionWorkflow(ctx context.Context, workspaceID, sessionID uuid.UUID) error
}

// Publisher drains the outbox and starts workflows.
type Publisher struct {
	outbox  OutboxRepository
	starter WorkflowStarter
	logger  *slog.Logger
}

// NewPublisher wires the publisher.
func NewPublisher(outbox OutboxRepository, starter WorkflowStarter, logger *slog.Logger) *Publisher {
	return &Publisher{outbox: outbox, starter: starter, logger: logger}
}

// Run drains the outbox until the context is cancelled.
//
// It runs in the worker rather than the API. The API already runs two sweeps
// as goroutines so a third would have been easiest, and it was refused: every
// API replica would poll one table, and Temporal would become a serving
// dependency of a process whose requests never touch it.
func (p *Publisher) Run(ctx context.Context) {
	ticker := time.NewTicker(OutboxPollInterval)
	defer ticker.Stop()

	// Drain before the first tick. A worker that has just started may be
	// starting *because* something is queued, and waiting out a poll interval
	// to notice would put the delay on exactly the case that cares about it.
	p.drain(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.drain(ctx)
		}
	}
}

// drain empties the queue, a batch at a time.
//
// A batch that filled completely probably has more waiting behind it, so it
// keeps going rather than sleeping on a backlog.
func (p *Publisher) drain(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		delivered, err := p.drainOnce(ctx)
		if err != nil {
			p.logger.WarnContext(ctx, "draining the outbox failed",
				slog.String("error", err.Error()))
			return
		}
		if delivered < OutboxBatchSize {
			return
		}
	}
}

// drainOnce claims a batch and settles each row, returning how many it took.
func (p *Publisher) drainOnce(ctx context.Context) (int, error) {
	claimed, err := p.outbox.Claim(ctx, OutboxBatchSize, OutboxLease, OutboxMaxAttempts)
	if err != nil {
		return 0, err
	}

	// The lease covers the whole batch, not each row in turn.
	//
	// Delivering serially means a slow start early in the batch eats the lease
	// the later rows are relying on — after which another publisher reclaims
	// them, spends another attempt each, and the row moves toward its ceiling
	// for no reason but our own slowness. So the batch stops when the lease is
	// close to expiring, and the rows left behind are picked up on the next
	// poll with a fresh lease rather than delivered under one that has run out.
	deadline := time.Now().Add(OutboxLease - OutboxLeaseSafetyMargin)
	for index, event := range claimed {
		if time.Now().After(deadline) {
			p.logger.WarnContext(ctx, "stopping the batch before its lease expires",
				slog.Int("delivered", index),
				slog.Int("claimed", len(claimed)),
			)
			// Reported as a short batch so the caller does not immediately ask
			// for another: the queue is not drained, but this publisher is out
			// of time to drain it.
			return index, nil
		}
		p.deliver(ctx, event)
	}
	return len(claimed), nil
}

// deliver settles one claimed row.
//
// Every outcome ends with the row settled, which is the property that matters:
// a row left claimed is one that waits out its lease before anyone looks at it
// again, and a row left pending forever is the queue growing silently.
func (p *Publisher) deliver(ctx context.Context, event ClaimedOutboxEvent) {
	logger := p.logger.With(
		slog.String("outbox_id", event.ID.String()),
		slog.String("topic", event.Topic),
		slog.String("subject_id", event.SubjectID.String()),
		slog.Int("attempts", int(event.Attempts)),
	)

	switch event.Topic {
	case TopicSessionCreated:
		err := p.starter.StartSessionWorkflow(ctx, event.WorkspaceID, event.SubjectID)
		switch {
		case err == nil, errors.Is(err, ErrWorkflowAlreadyStarted):
			// Already started is success. The publisher is at-least-once, so a
			// row delivered twice is expected rather than exceptional.
			if errors.Is(err, ErrWorkflowAlreadyStarted) {
				logger.InfoContext(ctx, "the workflow was already started, so this delivery is a duplicate")
			}
			p.settleCompleted(ctx, logger, event)

		case errors.Is(err, ErrWorkflowStartRejected):
			// Terminal: waiting does not make a rejected start acceptable.
			p.settleTerminated(ctx, logger, event, err)

		default:
			p.settleRetry(ctx, logger, event, err)
		}

	default:
		// An unknown topic cannot be delivered by any amount of retrying, and
		// leaving it pending would make the queue grow behind a row nothing
		// understands.
		p.settleTerminated(ctx, logger, event,
			fmt.Errorf("no publisher handles the topic %q", event.Topic))
	}
}

func (p *Publisher) settleCompleted(ctx context.Context, logger *slog.Logger, event ClaimedOutboxEvent) {
	if err := p.outbox.Complete(ctx, event); err != nil {
		p.logSettleFailure(ctx, logger, "complete", err)
	}
}

func (p *Publisher) settleTerminated(ctx context.Context, logger *slog.Logger, event ClaimedOutboxEvent, cause error) {
	logger.ErrorContext(ctx, "the outbox event will never be delivered",
		slog.String("error", cause.Error()))
	if err := p.outbox.Terminate(ctx, event, cause.Error()); err != nil {
		p.logSettleFailure(ctx, logger, "terminate", err)
	}
}

// settleRetry puts the row back, or gives up when it has been tried enough.
//
// The ceiling is what bounds the duplicate guarantee. The workflow id stops a
// second workflow only while Temporal still remembers the closed execution, so
// a row that could retry forever would outlive the protection and the
// duplicate would return — on the row least likely to be looked at, because it
// is the one that has been failing for weeks.
func (p *Publisher) settleRetry(ctx context.Context, logger *slog.Logger, event ClaimedOutboxEvent, cause error) {
	if event.Attempts >= OutboxMaxAttempts {
		p.settleTerminated(ctx, logger, event,
			fmt.Errorf("gave up after %d attempts: %w", event.Attempts, cause))
		return
	}

	backoff := OutboxBackoff(event.Attempts)
	logger.WarnContext(ctx, "the outbox event will be retried",
		slog.String("error", cause.Error()),
		slog.Duration("backoff", backoff),
	)
	if err := p.outbox.Retry(ctx, event, backoff, cause.Error()); err != nil {
		p.logSettleFailure(ctx, logger, "retry", err)
	}
}

// logSettleFailure records a settle that did not land.
//
// A lost claim is expected rather than alarming: this publisher's lease
// expired and another took the row, which is the recovery path working. Any
// other failure leaves the row leased until it expires, which is also
// recoverable — so neither is worth failing the loop over, and both are worth
// being able to see.
func (p *Publisher) logSettleFailure(ctx context.Context, logger *slog.Logger, operation string, err error) {
	if errors.Is(err, ErrOutboxClaimLost) {
		logger.InfoContext(ctx, "another publisher took this row while it was being delivered",
			slog.String("operation", operation))
		return
	}
	logger.ErrorContext(ctx, "settling the outbox event failed",
		slog.String("operation", operation),
		slog.String("error", err.Error()),
	)
}
