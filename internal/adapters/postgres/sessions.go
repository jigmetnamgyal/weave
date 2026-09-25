package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jigmetnamgyal/weave/internal/adapters/postgres/postgresdb"
	"github.com/jigmetnamgyal/weave/internal/application"
	"github.com/jigmetnamgyal/weave/internal/domain"
)

// SessionStore persists sessions, their participants and their history.
type SessionStore struct {
	pool *pgxpool.Pool
}

// NewSessionStore returns a store backed by pool.
func NewSessionStore(pool *pgxpool.Pool) *SessionStore {
	return &SessionStore{pool: pool}
}

// inTx runs fn inside a transaction carrying the caller's tenant context.
func (s *SessionStore) inTx(ctx context.Context, fn func(*postgresdb.Queries) error) error {
	return inTenantTx(ctx, s.pool, fn)
}

// Create writes a session and everything that must exist with it.
//
// Six rows in one transaction: the session, its first participant, its first
// state transition, the audit event, and the outbox record that is the durable
// promise something will pick it up. The policy snapshot is the sixth and is
// the `agent_version_id` column rather than a row of its own — the version is
// append-only and cannot change, so the pin already says what this session
// runs under.
//
// The atomicity is the feature, not an implementation detail. Writing the
// session and then enqueuing would leave a window where a session exists that
// nothing will ever start, and the window is open exactly when the process
// dies — after which no retry helps, because the session is already there.
func (s *SessionStore) Create(
	ctx context.Context,
	session domain.Session,
	participant domain.SessionParticipant,
	transition domain.SessionStateTransition,
	event application.OutboxEvent,
	verifyTask func(domain.Task) error,
	completion *application.IdempotentCompletion,
	actor application.Actor,
	audit application.AuditEvent,
) (domain.Session, error) {
	var created domain.Session

	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		// The same in-transaction re-check every other mutation does: the
		// handler authorized from a snapshot and the actor may have been
		// removed or demoted since.
		if err := authorizeActor(ctx, q, session.WorkspaceID, actor); err != nil {
			return err
		}

		// The task, locked and re-checked here rather than trusted from the
		// read that chose it. That read was its own transaction and the PATCH
		// route can commit in between, so without this a queued session could
		// reference a task patched back to a draft, or carry a branch intent
		// for a repository the task has since moved off. The composite
		// foreign key does not catch either: it checks identity and
		// workspace, never status.
		locked, err := q.LockTaskForUpdate(ctx, postgresdb.LockTaskForUpdateParams{
			ID:          session.TaskID,
			WorkspaceID: session.WorkspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrTaskNotFound
			}
			return fmt.Errorf("lock task: %w", err)
		}
		if err := verifyTask(taskToDomain(locked)); err != nil {
			return err
		}

		row, err := q.CreateSession(ctx, postgresdb.CreateSessionParams{
			ID:             session.ID,
			WorkspaceID:    session.WorkspaceID,
			TaskID:         session.TaskID,
			AgentVersionID: session.AgentVersionID,
			State:          string(session.State),
			RepositoryID:   session.RepositoryID,
			BranchName:     session.BranchName,
			BaseBranch:     session.BaseBranch,
			CreatedBy:      session.CreatedBy,
		})
		if err != nil {
			return translateSessionError(err)
		}

		if _, err := q.AddSessionParticipant(ctx, postgresdb.AddSessionParticipantParams{
			SessionID:   session.ID,
			WorkspaceID: session.WorkspaceID,
			UserID:      participant.UserID,
			Capacity:    string(participant.Capacity),
		}); err != nil {
			return translateSessionError(err)
		}

		if _, err := q.AppendSessionTransition(ctx, postgresdb.AppendSessionTransitionParams{
			ID:              transition.ID,
			SessionID:       session.ID,
			WorkspaceID:     session.WorkspaceID,
			PreviousState:   nullableState(transition.PreviousState),
			NextState:       string(transition.NextState),
			ObservedVersion: transition.ObservedVersion,
			Reason:          transition.Reason,
			ActorUserID:     nullableUUID(transition.ActorUserID),
		}); err != nil {
			return translateSessionError(err)
		}

		if _, err := q.EnqueueOutboxEvent(ctx, postgresdb.EnqueueOutboxEventParams{
			ID:          event.ID,
			WorkspaceID: session.WorkspaceID,
			Topic:       event.Topic,
			SubjectID:   event.SubjectID,
			Payload:     payloadOrEmpty(event.Payload),
		}); err != nil {
			return translateSessionError(err)
		}

		created = sessionToDomain(row)

		// The idempotent response, written here rather than after the
		// transaction. Render is the transport's, because only the transport
		// knows what the response looks like; it runs inside the transaction
		// so the session and the answer describing it commit together or not
		// at all.
		if completion != nil {
			status, body, err := completion.Render(created)
			if err != nil {
				return fmt.Errorf("render idempotent response: %w", err)
			}
			if err := completeIdempotency(ctx, q, *completion, status, body); err != nil {
				return err
			}
		}

		return appendAudit(ctx, q, audit)
	})
	if err != nil {
		// The zero session, not the one built above.
		//
		// `created` is assigned before the audit write, so a failure after it
		// would otherwise hand back a fully populated session — with an id —
		// for a row the rollback has just removed. A caller that logged the
		// error and carried on would be holding an identifier for nothing.
		//
		// This is the opposite call to the one M3.3 makes, deliberately.
		// There the branch had really been created on GitHub before the audit
		// failed, so returning it alongside the error was the only way not to
		// hide an irreversible write. Here nothing happened, so saying
		// something did would be the false statement.
		return domain.Session{}, err
	}
	return created, nil
}

// Transition moves a session to another state and records the move.
//
// Lock, decide, write, append — all in one transaction. The decision is passed
// in as a closure because the rules are the domain's: which state pairs are
// legal, and whether the version the caller observed is still current. What
// this contributes is that the row the closure inspects is the row the update
// then writes, which is the only way "the version has not moved" can be true
// when it is acted on.
//
// No HTTP route reaches this in M4.2. It exists because the alternative is an
// insert-only transition query that looks like the way to record a move, which
// would let M5 write immutable history for a transition the session never
// actually made.
func (s *SessionStore) Transition(
	ctx context.Context,
	sessionID, workspaceID uuid.UUID,
	decide func(domain.Session) (domain.SessionStateTransition, error),
	actor application.Actor,
	audit func(domain.Session) application.AuditEvent,
) (domain.Session, error) {
	var moved domain.Session

	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		if err := authorizeActor(ctx, q, workspaceID, actor); err != nil {
			return err
		}

		locked, err := q.LockSessionForUpdate(ctx, postgresdb.LockSessionForUpdateParams{
			ID:          sessionID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrSessionNotFound
			}
			return fmt.Errorf("lock session: %w", err)
		}
		current := sessionToDomain(locked)

		transition, err := decide(current)
		if err != nil {
			return err
		}

		row, err := q.UpdateSessionState(ctx, postgresdb.UpdateSessionStateParams{
			ID:          sessionID,
			WorkspaceID: workspaceID,
			State:       string(transition.NextState),
			Version:     transition.ObservedVersion,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// The lock is held, so the row cannot have moved underneath
				// this. Reaching here means the observed version never matched
				// in the first place, which the closure should already have
				// refused — treated as a conflict rather than a missing row so
				// a caller is never told a session vanished.
				return domain.ErrSessionVersionConflict
			}
			return translateSessionError(err)
		}

		previous := current.State
		if _, err := q.AppendSessionTransition(ctx, postgresdb.AppendSessionTransitionParams{
			ID:              transition.ID,
			SessionID:       sessionID,
			WorkspaceID:     workspaceID,
			PreviousState:   nullableState(&previous),
			NextState:       string(transition.NextState),
			ObservedVersion: transition.ObservedVersion,
			Reason:          transition.Reason,
			ActorUserID:     nullableUUID(transition.ActorUserID),
		}); err != nil {
			return translateSessionError(err)
		}

		moved = sessionToDomain(row)
		return appendAudit(ctx, q, audit(moved))
	})
	if err != nil {
		// The zero session, for the reason Create returns one: the rollback
		// has removed the state change, so handing back a moved session would
		// describe something that did not happen.
		return domain.Session{}, err
	}
	return moved, nil
}

// RecordBranch writes the session's branch commit, once, with its audit row.
//
// One transaction for both, which is the point of doing it here rather than
// through the installation store's standalone AppendAudit. The branch itself
// was made on GitHub and nothing here can roll that back; what this can do is
// make the *record* of it all-or-nothing. A SHA with no audit, or an audit with
// no SHA, is the seam M3.3 shipped three regressions in.
//
// Three outcomes under the lock:
//
//   - nothing recorded yet: write the SHA and the audit, report recorded;
//   - the same SHA already recorded: a redelivered activity finding its own
//     work, so success, and nothing written twice;
//   - a different SHA already recorded: refused as a conflict. The column is
//     write-once in the database as well, so this is the friendly half of a
//     rule that holds regardless.
func (s *SessionStore) RecordBranch(
	ctx context.Context,
	sessionID, workspaceID uuid.UUID,
	sha string,
	actor application.Actor,
	audit func(domain.Session) application.AuditEvent,
) (domain.Session, bool, error) {
	var (
		session  domain.Session
		recorded bool
	)

	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		if err := authorizeActor(ctx, q, workspaceID, actor); err != nil {
			return err
		}

		locked, err := q.LockSessionForUpdate(ctx, postgresdb.LockSessionForUpdateParams{
			ID:          sessionID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrSessionNotFound
			}
			return fmt.Errorf("lock session: %w", err)
		}

		if locked.BranchSha != nil {
			session = sessionToDomain(locked)
			if *locked.BranchSha == sha {
				return nil
			}
			return fmt.Errorf("%w: this session already recorded its branch at %s",
				domain.ErrBranchConflict, shortSHA(*locked.BranchSha))
		}

		row, err := q.RecordSessionBranch(ctx, postgresdb.RecordSessionBranchParams{
			ID:          sessionID,
			WorkspaceID: workspaceID,
			BranchSha:   &sha,
		})
		if err != nil {
			if constraintViolated(err, "sessions_branch_sha_format") {
				return fmt.Errorf("%w: %q is not a commit id", domain.ErrInvalidSession, sha)
			}
			return fmt.Errorf("record session branch: %w", err)
		}

		session = sessionToDomain(row)
		recorded = true
		return appendAudit(ctx, q, audit(session))
	})
	if err != nil {
		return domain.Session{}, false, err
	}
	return session, recorded, nil
}

// shortSHA truncates a commit id for a message, as Git does.
func shortSHA(sha string) string {
	if len(sha) <= 7 {
		return sha
	}
	return sha[:7]
}

// Get returns one session, scoped to the workspace in context.
func (s *SessionStore) Get(ctx context.Context, sessionID, workspaceID uuid.UUID) (domain.Session, error) {
	var session domain.Session
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		row, err := q.GetSessionForWorkspace(ctx, postgresdb.GetSessionForWorkspaceParams{
			ID:          sessionID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return application.ErrSessionNotFound
			}
			return fmt.Errorf("select session: %w", err)
		}
		session = sessionToDomain(row)
		return nil
	})
	return session, err
}

// List returns a workspace's sessions, newest first.
func (s *SessionStore) List(ctx context.Context, workspaceID uuid.UUID) ([]domain.Session, error) {
	var sessions []domain.Session
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		rows, err := q.ListSessionsForWorkspace(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("list sessions: %w", err)
		}
		sessions = make([]domain.Session, 0, len(rows))
		for _, row := range rows {
			sessions = append(sessions, sessionToDomain(row))
		}
		return nil
	})
	return sessions, err
}

// ListParticipants returns who is in a session.
func (s *SessionStore) ListParticipants(
	ctx context.Context,
	sessionID, workspaceID uuid.UUID,
) ([]domain.SessionParticipant, error) {
	var participants []domain.SessionParticipant
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		rows, err := q.ListSessionParticipants(ctx, postgresdb.ListSessionParticipantsParams{
			SessionID:   sessionID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			return fmt.Errorf("list session participants: %w", err)
		}
		participants = make([]domain.SessionParticipant, 0, len(rows))
		for _, row := range rows {
			participants = append(participants, domain.SessionParticipant{
				SessionID:   row.SessionID,
				WorkspaceID: row.WorkspaceID,
				UserID:      row.UserID,
				Capacity:    domain.SessionParticipantCapacity(row.Capacity),
				CreatedAt:   timestamp(row.CreatedAt),
			})
		}
		return nil
	})
	return participants, err
}

// ListTransitions returns a session's history, oldest first.
func (s *SessionStore) ListTransitions(
	ctx context.Context,
	sessionID, workspaceID uuid.UUID,
) ([]domain.SessionStateTransition, error) {
	var transitions []domain.SessionStateTransition
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		rows, err := q.ListSessionTransitions(ctx, postgresdb.ListSessionTransitionsParams{
			SessionID:   sessionID,
			WorkspaceID: workspaceID,
		})
		if err != nil {
			return fmt.Errorf("list session transitions: %w", err)
		}
		transitions = make([]domain.SessionStateTransition, 0, len(rows))
		for _, row := range rows {
			transition := domain.SessionStateTransition{
				ID:              row.ID,
				SessionID:       row.SessionID,
				WorkspaceID:     row.WorkspaceID,
				NextState:       domain.SessionState(row.NextState),
				ObservedVersion: row.ObservedVersion,
				Reason:          row.Reason,
				CreatedAt:       timestamp(row.CreatedAt),
			}
			if row.PreviousState != nil {
				previous := domain.SessionState(*row.PreviousState)
				transition.PreviousState = &previous
			}
			if row.ActorUserID.Valid {
				actor := uuid.UUID(row.ActorUserID.Bytes)
				transition.ActorUserID = &actor
			}
			transitions = append(transitions, transition)
		}
		return nil
	})
	return transitions, err
}

// ListPendingOutbox returns the rows a claimer would find, without claiming.
//
// Read-only on purpose: the claim itself belongs to M5's publisher, and a
// claim written now with nothing exercising it would be a protocol nobody has
// watched work. What this does allow is asserting that creating a session
// leaves exactly one pending promise behind.
func (s *SessionStore) ListPendingOutbox(
	ctx context.Context,
	workspaceID uuid.UUID,
) ([]application.OutboxEvent, error) {
	var events []application.OutboxEvent
	err := s.inTx(ctx, func(q *postgresdb.Queries) error {
		rows, err := q.ListPendingOutboxEvents(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("list pending outbox events: %w", err)
		}
		events = make([]application.OutboxEvent, 0, len(rows))
		for _, row := range rows {
			events = append(events, application.OutboxEvent{
				ID:          row.ID,
				WorkspaceID: row.WorkspaceID,
				Topic:       row.Topic,
				SubjectID:   row.SubjectID,
				Payload:     row.Payload,
				Attempts:    row.Attempts,
			})
		}
		return nil
	})
	return events, err
}

// nullableState renders an absent previous state as SQL NULL.
//
// NULL rather than an empty string, because a session's first transition has
// no previous state at all — and "" is a value that a reader could mistake for
// one, or that a future CHECK would have to carve an exception for.
func nullableState(state *domain.SessionState) *string {
	if state == nil {
		return nil
	}
	value := string(*state)
	return &value
}

// payloadOrEmpty renders an absent payload as an empty JSON object.
//
// The column is NOT NULL and constrained to an object, so a nil slice is a
// constraint violation whose message says nothing about what the caller did
// wrong. The same shape as toolPolicyOrEmpty, and for the same reason.
func payloadOrEmpty(payload []byte) []byte {
	if len(payload) == 0 {
		return []byte("{}")
	}
	return payload
}

// translateSessionError turns the constraints into errors a caller can act on.
//
// The database carries these checks so no path can write a row the workflow
// would later fail on. Relaying a raw constraint violation would be accurate
// and useless, so each one a caller can fix is named.
func translateSessionError(err error) error {
	switch {
	case constraintViolated(err, "sessions_task_fkey"):
		return application.ErrTaskNotFound
	case constraintViolated(err, "sessions_agent_version_fkey"):
		return application.ErrAgentVersionNotFound
	case constraintViolated(err, "sessions_repository_fkey"):
		return application.ErrRepositoryNotFound
	case constraintViolated(err, "sessions_state_valid"),
		constraintViolated(err, "session_state_transitions_next_valid"),
		constraintViolated(err, "session_state_transitions_previous_valid"):
		return fmt.Errorf("%w: unknown state", domain.ErrInvalidSession)
	case constraintViolated(err, "sessions_branch_name_length"),
		constraintViolated(err, "sessions_base_branch_length"):
		return fmt.Errorf("%w: the branch name is empty or too long", domain.ErrInvalidBranchName)
	case constraintViolated(err, "session_participants_capacity_valid"):
		return fmt.Errorf("%w: unknown participant capacity", domain.ErrInvalidSession)
	case constraintViolated(err, "outbox_events_payload_is_object"):
		return fmt.Errorf("%w: an outbox payload must be a JSON object", domain.ErrInvalidSession)
	default:
		return fmt.Errorf("write session: %w", err)
	}
}

// sessionToDomain converts a generated row.
func sessionToDomain(row postgresdb.Session) domain.Session {
	session := domain.Session{
		ID:             row.ID,
		WorkspaceID:    row.WorkspaceID,
		TaskID:         row.TaskID,
		AgentVersionID: row.AgentVersionID,
		State:          domain.SessionState(row.State),
		Version:        row.Version,
		RepositoryID:   row.RepositoryID,
		BranchName:     row.BranchName,
		BaseBranch:     row.BaseBranch,
		CreatedBy:      row.CreatedBy,
		CreatedAt:      timestamp(row.CreatedAt),
		UpdatedAt:      timestamp(row.UpdatedAt),
	}
	if row.ContinuesID.Valid {
		id := uuid.UUID(row.ContinuesID.Bytes)
		session.ContinuesID = &id
	}
	if row.BranchSha != nil {
		session.BranchSHA = *row.BranchSha
	}
	return session
}
