package application

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// RunOutcome is how a session's run ended, as the workflow observed it.
type RunOutcome struct {
	// ExitCode is the runner's exit, read from the backend. Meaningful only
	// when neither Expired nor Undrained.
	ExitCode int
	// Expired: the provider was still running at SessionMaxRunTime.
	Expired bool
	// Undrained: the stream still held the session's events at DrainTimeout,
	// or the runner could not be halted, so an empty stream proves nothing.
	Undrained bool
	// Interrupted: the session's workflow was cancelled before the provider
	// finished.
	Interrupted bool
}

// SessionProviderFailures reads whether a session's history holds a
// provider.failed event, and its code.
type SessionProviderFailures interface {
	ProviderFailure(ctx context.Context, sessionID, workspaceID uuid.UUID) (code string, found bool, err error)
}

// SessionOutcomeService ends a session whose run is over, deciding how.
type SessionOutcomeService struct {
	sessions *SessionService
	reader   SessionBranchStore
	agents   RunnerAgentReader
	failures SessionProviderFailures
}

// NewSessionOutcomeService wires the service.
func NewSessionOutcomeService(
	sessions *SessionService,
	reader SessionBranchStore,
	agents RunnerAgentReader,
	failures SessionProviderFailures,
) *SessionOutcomeService {
	return &SessionOutcomeService{sessions: sessions, reader: reader, agents: agents, failures: failures}
}

// Complete moves a running session to where its run leaves it.
//
// Called only after the drain, so the history it reads is complete. Two
// sources say how the provider ended — the runner's exit code and the events —
// and when they disagree **the pessimistic one wins**: a provider.failed event
// in history fails the session even after an exit of 0, and an exit of failure
// fails it even with no such event. A session reported as finished must be one
// nothing said failed.
//
// Every reason names the provider, so a session run by the fake says so
// wherever a person reads its history. Only our own words and the provider's
// validated failure *code* reach a reason — never the provider's message text.
func (s *SessionOutcomeService) Complete(
	ctx context.Context,
	workspaceID, sessionID uuid.UUID,
	outcome RunOutcome,
) (domain.Session, error) {
	session, err := s.reader.Get(ctx, sessionID, workspaceID)
	if err != nil {
		return domain.Session{}, err
	}
	version, err := s.agents.GetVersion(ctx, session.AgentVersionID, workspaceID)
	if err != nil {
		return domain.Session{}, fmt.Errorf("read the session's agent version: %w", err)
	}
	provider := string(version.Provider)

	end := func(state domain.SessionState, reason string) (domain.Session, error) {
		return s.sessions.TransitionAsSystem(ctx, workspaceID, sessionID, state, reason)
	}

	switch {
	case outcome.Undrained:
		return end(domain.SessionFailed, SessionFailureReason(string(RunnerFailureEventsNotDrained)))
	case outcome.Interrupted:
		return end(domain.SessionFailed,
			fmt.Sprintf("the session's run was interrupted before the %s provider finished", provider))
	case outcome.Expired:
		return end(domain.SessionExpired,
			fmt.Sprintf("the %s provider was still running at the session's maximum run time", provider))
	}

	code, failed, err := s.failures.ProviderFailure(ctx, sessionID, workspaceID)
	if err != nil {
		return domain.Session{}, err
	}
	if failed {
		return end(domain.SessionFailed, fmt.Sprintf("the %s provider reported failure: %s", provider, code))
	}

	switch outcome.ExitCode {
	case 0:
		return end(domain.SessionReviewReady, fmt.Sprintf(
			"the %s provider finished; reviewing its work arrives with M7", provider))
	case RunnerExitProviderFailed:
		return end(domain.SessionFailed, fmt.Sprintf(
			"the %s provider's runner reported failure without a failure event", provider))
	case RunnerExitProviderUnavailable:
		return end(domain.SessionFailed, SessionFailureReason(string(RunnerFailureProviderUnavailable)))
	case RunnerExitInterrupted:
		return end(domain.SessionFailed,
			fmt.Sprintf("the %s provider's runner was stopped before the provider finished", provider))
	case RunnerExitPublishFailed:
		return end(domain.SessionFailed, SessionFailureReason(string(RunnerFailureEventsUnconfirmed)))
	default:
		return end(domain.SessionFailed, SessionFailureReason(string(RunnerFailureLost)))
	}
}
