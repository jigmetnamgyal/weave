package temporal

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	enums "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/jigmetnamgyal/weave/internal/application"
)

// Starter starts session workflows.
type Starter struct {
	client client.Client
	queue  string
}

// NewStarter wires the starter against the product's task queue.
func NewStarter(c client.Client) *Starter {
	return &Starter{client: c, queue: TaskQueue}
}

// NewStarterOn wires the starter against a named queue.
//
// For tests, which give themselves a queue each so two runs never take each
// other's work. Production uses NewStarter.
func NewStarterOn(c client.Client, queue string) *Starter {
	return &Starter{client: c, queue: queue}
}

// StartSessionWorkflow starts the workflow for one session.
//
// **The workflow id is the session id**, which is what makes an at-least-once
// publisher safe: Temporal refuses a second workflow with an id already in
// use, so a row delivered twice cannot produce two workflows for one session.
//
// `WorkflowIDReusePolicyRejectDuplicate` is the part that took a review to get
// right. Uniqueness among *running* workflows is the default and is not
// enough: the sequence that breaks it is a publisher that starts the workflow,
// watches it finish, and dies before completing the outbox row — the lease
// expires, another publisher claims it, and publishes again against an id that
// is now **closed**. A permissive reuse policy starts a second workflow for a
// session that already ran. Rejecting duplicates closes that, for as long as
// Temporal remembers the execution — which is why the outbox also has an
// attempt ceiling, so a row cannot still be retrying when that memory lapses.
func (s *Starter) StartSessionWorkflow(ctx context.Context, workspaceID, sessionID uuid.UUID) error {
	options := client.StartWorkflowOptions{
		ID:        sessionID.String(),
		TaskQueue: s.queue,
		// Rejects a duplicate whether the first execution is running or
		// closed. The default rejects only while it runs.
		WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}

	_, err := s.client.ExecuteWorkflow(ctx, options, SessionWorkflowName, SessionWorkflowInput{
		WorkspaceID: workspaceID.String(),
		SessionID:   sessionID.String(),
	})
	if err == nil {
		return nil
	}

	// Already started is success, not failure. The publisher is at-least-once
	// by design, so a duplicate delivery is expected — and reporting it as an
	// error would make the publisher retry a row that has nothing left to do,
	// forever.
	var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &alreadyStarted) {
		return application.ErrWorkflowAlreadyStarted
	}

	// A malformed request does not become well formed by waiting.
	var invalid *serviceerror.InvalidArgument
	if errors.As(err, &invalid) {
		return fmt.Errorf("%w: %w", application.ErrWorkflowStartRejected, err)
	}

	return fmt.Errorf("start session workflow: %w", err)
}
