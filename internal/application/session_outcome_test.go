package application_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// outcomeWorld is a running session and the service that ends it. The store
// records the transition it would write.
type outcomeWorld struct {
	store   *transitionRecorder
	service *application.SessionOutcomeService
	session domain.Session
}

func newOutcomeWorld(t *testing.T, failureCode string) *outcomeWorld {
	t.Helper()
	session := domain.Session{ID: uuid.New(), WorkspaceID: uuid.New(), State: domain.SessionRunning, Version: 3,
		AgentVersionID: uuid.New()}
	store := &transitionRecorder{session: session}
	sessions := application.NewSessionService(store, nil, nil)
	return &outcomeWorld{
		store:   store,
		service: application.NewSessionOutcomeService(sessions, store, fakeAgents{}, fakeFailures{code: failureCode}),
		session: session,
	}
}

func (w *outcomeWorld) complete(t *testing.T, outcome application.RunOutcome) (domain.SessionState, string) {
	t.Helper()
	if _, err := w.service.Complete(context.Background(), w.session.WorkspaceID, w.session.ID, outcome); err != nil {
		t.Fatalf("complete: %v", err)
	}
	return w.store.to, w.store.reason
}

func TestHowASessionEnds(t *testing.T) {
	cases := []struct {
		name       string
		failure    string
		outcome    application.RunOutcome
		want       domain.SessionState
		wantReason string
	}{
		{"finished", "", application.RunOutcome{ExitCode: 0}, domain.SessionReviewReady, "fake provider finished"},
		{"reported failure", "fake_failure", application.RunOutcome{ExitCode: application.RunnerExitProviderFailed},
			domain.SessionFailed, "fake provider reported failure: fake_failure"},
		// Pessimistic: a failure event outranks an exit of success.
		{"exit 0 but a failure event", "fake_failure", application.RunOutcome{ExitCode: 0},
			domain.SessionFailed, "reported failure: fake_failure"},
		// And a failure exit outranks the absence of one.
		{"failure exit, no event", "", application.RunOutcome{ExitCode: application.RunnerExitProviderFailed},
			domain.SessionFailed, "without a failure event"},
		{"expired", "", application.RunOutcome{Expired: true}, domain.SessionExpired, "maximum run time"},
		{"undrained", "", application.RunOutcome{Undrained: true}, domain.SessionFailed, "not ingested in time"},
		{"unconfirmed events", "", application.RunOutcome{ExitCode: application.RunnerExitPublishFailed},
			domain.SessionFailed, "could not get its events confirmed"},
		{"lost", "", application.RunOutcome{ExitCode: -1}, domain.SessionFailed, "stopped before"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, reason := newOutcomeWorld(t, tc.failure).complete(t, tc.outcome)
			if state != tc.want || !strings.Contains(reason, tc.wantReason) {
				t.Errorf("ended %s (%q), want %s containing %q", state, reason, tc.want, tc.wantReason)
			}
		})
	}
}

type fakeFailures struct{ code string }

func (f fakeFailures) ProviderFailure(context.Context, uuid.UUID, uuid.UUID) (string, bool, error) {
	return f.code, f.code != "", nil
}

// transitionRecorder is just enough of the session store for Complete: Get,
// and Transition recording where it moved the session.
type transitionRecorder struct {
	application.SessionRepository
	session domain.Session
	to      domain.SessionState
	reason  string
}

func (r *transitionRecorder) Get(context.Context, uuid.UUID, uuid.UUID) (domain.Session, error) {
	return r.session, nil
}

func (r *transitionRecorder) RecordBranch(context.Context, uuid.UUID, uuid.UUID, string, application.Actor,
	func(domain.Session) application.AuditEvent) (domain.Session, bool, error) {
	return r.session, false, nil
}

func (r *transitionRecorder) Transition(
	_ context.Context, _, _ uuid.UUID,
	decide func(domain.Session) (domain.SessionStateTransition, error),
	_ application.Actor, _ func(domain.Session) application.AuditEvent,
) (domain.Session, error) {
	transition, err := decide(r.session)
	if err != nil {
		return domain.Session{}, err
	}
	r.to, r.reason = transition.NextState, transition.Reason
	moved := r.session
	moved.State = transition.NextState
	return moved, nil
}
