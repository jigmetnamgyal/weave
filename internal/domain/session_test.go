package domain_test

import (
	"testing"

	"github.com/jigmetnamgyal/weave/internal/domain"
)

// TestSessionTransitionsAreExhaustive is the state machine's version of
// TestMatrixIsExhaustive, and exists for the same reason.
//
// The transition table is the only place a state implies anything about what
// may follow it. If a state could be added without every pairing being
// answered, the omissions would deny at runtime while recording no decision —
// and a reader could not tell a considered "no" from a forgotten one. This
// fails the build in that case, in both directions: every state must appear as
// a source, and every source must answer for every target.
func TestSessionTransitionsAreExhaustive(t *testing.T) {
	if len(domain.SessionStates) != 16 {
		t.Fatalf("got %d states, want the 16 in context/architecture.md — "+
			"the document is the authority and this list has drifted from it",
			len(domain.SessionStates))
	}

	for _, from := range domain.SessionStates {
		for _, to := range domain.SessionStates {
			// Reading it is the assertion: a missing source key or a missing
			// target within one reads as false, which is exactly what must not
			// be possible to do by accident. The decided table is checked
			// against the hand-written expectation below, which is what makes
			// an omission visible rather than silently permissive.
			_ = domain.CanTransition(from, to)
		}
	}

	for _, from := range domain.SessionStates {
		next := domain.NextStates(from)
		if from.Terminal() && len(next) != 0 {
			t.Errorf("%s is terminal but may move to %v — invariant 10 says terminal "+
				"history is immutable and continuation is a new session", from, next)
		}
		if !from.Terminal() && len(next) == 0 {
			t.Errorf("%s is not terminal but has nowhere to go, so a session "+
				"reaching it would stall with no way to say so", from)
		}
	}
}

// TestSessionTransitionsMatchTheWrittenExpectation pins the table against a
// second copy written out by hand.
//
// Without this the exhaustiveness check above passes on any table at all,
// including one that permits everything. Two independent statements of the
// same rule mean a change has to be made twice on purpose — the convention the
// authorization matrix set.
func TestSessionTransitionsMatchTheWrittenExpectation(t *testing.T) {
	expected := map[domain.SessionState][]domain.SessionState{
		domain.SessionDraft:              {domain.SessionQueued, domain.SessionCancelled, domain.SessionExpired},
		domain.SessionQueued:             {domain.SessionProvisioning, domain.SessionCancelled, domain.SessionFailed, domain.SessionExpired},
		domain.SessionProvisioning:       {domain.SessionRunning, domain.SessionCancelling, domain.SessionFailed, domain.SessionExpired},
		domain.SessionRunning:            {domain.SessionWaitingForInput, domain.SessionWaitingForApproval, domain.SessionPausing, domain.SessionReviewReady, domain.SessionCancelling, domain.SessionFailed, domain.SessionExpired},
		domain.SessionWaitingForInput:    {domain.SessionRunning, domain.SessionPausing, domain.SessionCancelling, domain.SessionFailed, domain.SessionExpired},
		domain.SessionWaitingForApproval: {domain.SessionRunning, domain.SessionPausing, domain.SessionCancelling, domain.SessionFailed, domain.SessionExpired},
		domain.SessionPausing:            {domain.SessionPaused, domain.SessionCancelling, domain.SessionFailed, domain.SessionExpired},
		domain.SessionPaused:             {domain.SessionResuming, domain.SessionCancelled, domain.SessionExpired},
		domain.SessionResuming:           {domain.SessionRunning, domain.SessionCancelling, domain.SessionFailed, domain.SessionExpired},
		domain.SessionReviewReady:        {domain.SessionRunning, domain.SessionPausing, domain.SessionFinalizing, domain.SessionCancelling, domain.SessionFailed, domain.SessionExpired},
		domain.SessionFinalizing:         {domain.SessionCompleted, domain.SessionFailed},
		domain.SessionCancelling:         {domain.SessionCancelled, domain.SessionFailed},
		domain.SessionCompleted:          {},
		domain.SessionCancelled:          {},
		domain.SessionFailed:             {},
		domain.SessionExpired:            {},
	}

	if len(expected) != len(domain.SessionStates) {
		t.Fatalf("the expectation covers %d states and there are %d",
			len(expected), len(domain.SessionStates))
	}

	for _, from := range domain.SessionStates {
		want, ok := expected[from]
		if !ok {
			t.Errorf("%s has no written expectation", from)
			continue
		}
		allowed := map[domain.SessionState]bool{}
		for _, to := range want {
			allowed[to] = true
		}
		for _, to := range domain.SessionStates {
			if got := domain.CanTransition(from, to); got != allowed[to] {
				t.Errorf("CanTransition(%s, %s) = %t, want %t", from, to, got, allowed[to])
			}
		}
	}
}

// TestATerminalSessionRefusesEveryTransition states invariant 10 as a test
// rather than trusting the table to be read correctly.
func TestATerminalSessionRefusesEveryTransition(t *testing.T) {
	terminal := []domain.SessionState{
		domain.SessionCompleted, domain.SessionCancelled,
		domain.SessionFailed, domain.SessionExpired,
	}
	for _, from := range terminal {
		if !from.Terminal() {
			t.Errorf("%s should report itself terminal", from)
		}
		for _, to := range domain.SessionStates {
			if domain.CanTransition(from, to) {
				t.Errorf("%s may move to %s; terminal history must be immutable", from, to)
			}
		}
	}
}

// TestEveryStateIsReachableFromCreation walks the table from `queued`, which
// is where a created session starts.
//
// A state nothing can reach is either dead or a gap in the table, and both are
// worth knowing at the point the table is written rather than in M6. `draft`
// is the deliberate exception: it precedes creation, and nothing transitions
// into it.
func TestEveryStateIsReachableFromCreation(t *testing.T) {
	seen := map[domain.SessionState]bool{domain.SessionQueued: true}
	frontier := []domain.SessionState{domain.SessionQueued}

	for len(frontier) > 0 {
		current := frontier[0]
		frontier = frontier[1:]
		for _, next := range domain.NextStates(current) {
			if !seen[next] {
				seen[next] = true
				frontier = append(frontier, next)
			}
		}
	}

	for _, state := range domain.SessionStates {
		if state == domain.SessionDraft {
			continue
		}
		if !seen[state] {
			t.Errorf("%s cannot be reached from queued, so it is either dead or "+
				"an edge is missing", state)
		}
	}
}

// TestParseSessionStateRejectsWhatTheDatabaseWould keeps the closed set in the
// domain and the CHECK constraint saying the same thing.
func TestParseSessionStateRejectsWhatTheDatabaseWould(t *testing.T) {
	for _, state := range domain.SessionStates {
		if _, err := domain.ParseSessionState(string(state)); err != nil {
			t.Errorf("ParseSessionState(%q) = %v, want nil", state, err)
		}
	}
	for _, bad := range []string{"", "  ", "RUNNING", "done", "queued ; drop"} {
		if _, err := domain.ParseSessionState(bad); err == nil {
			t.Errorf("ParseSessionState(%q) = nil, want an error", bad)
		}
	}
	// Trimmed, because a state arriving with whitespace from a row or a
	// caller is the same state.
	if got, err := domain.ParseSessionState("  running  "); err != nil || got != domain.SessionRunning {
		t.Errorf("ParseSessionState with padding = %q, %v", got, err)
	}
}
