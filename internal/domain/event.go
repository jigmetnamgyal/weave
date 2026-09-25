package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Session events are what a runner sends about a session: output, progress,
// failures. They are the first input the control plane accepts from the
// execution plane, and by the architecture's threat model that plane runs
// model output and customer code. So everything here treats an event as a
// claim by an untrusted producer.
//
// The contract lives in contracts/events as JSON Schema with fixtures. This
// file is its enforcement: strict decoding, checked against every fixture by
// TestEveryFixtureDecodesAsItsNameSays, so the schema and the code cannot
// drift apart unnoticed.

// EventType is the normalized type stored in session history.
type EventType string

// The types this version of the contract knows. Each arrives with the unit that
// emits it; these two are what M5.5's fake provider needs to show a session
// producing output and ending in a provider failure.
const (
	EventMessageCreated EventType = "message.created"
	EventProviderFailed EventType = "provider.failed"
)

// SupportedEventMajor is the only major schema version this consumer reads.
const SupportedEventMajor = 1

// MaxEventBytes bounds an encoded event.
//
// "Payloads stay small" is a rule in context/code-standards.md, and this is
// where it is enforced. Large data belongs in object storage behind an
// artifact reference, which does not exist yet — so an oversized event is
// refused rather than stored, and certainly not truncated into something that
// looks whole.
const MaxEventBytes = 64 << 10

// TransportName is the external name for a type at a major version, as
// `<domain>.<entity>.<action>.v<version>` requires. History stores the
// normalized type; transport names are what the schema files are called.
func TransportName(eventType EventType, major int) string {
	return "session." + string(eventType) + ".v" + strconv.Itoa(major)
}

// EventRefusal is why an event was quarantined rather than stored. Stable
// codes: operators query them.
type EventRefusal string

// Every reason an event can be refused. Each is a condition redelivery cannot
// change — except DeliveryExhausted, which is what a transient failure becomes
// once it has run out of attempts, recorded so it is not dropped silently.
const (
	RefusalMalformed          EventRefusal = "malformed"
	RefusalOversize           EventRefusal = "oversize"
	RefusalUnknownMajor       EventRefusal = "unknown_major_version"
	RefusalUnknownType        EventRefusal = "unknown_type"
	RefusalInvalidPayload     EventRefusal = "invalid_payload"
	RefusalSubjectMismatch    EventRefusal = "subject_mismatch"
	RefusalWorkspaceMismatch  EventRefusal = "workspace_mismatch"
	RefusalUnknownSession     EventRefusal = "unknown_session"
	RefusalSessionTerminal    EventRefusal = "session_terminal"
	RefusalDeliveryExhausted  EventRefusal = "delivery_exhausted"
	RefusalUnparseableSubject EventRefusal = "unparseable_subject"
)

// EventRefusalError carries the refusal reason and what parsed before it.
type EventRefusalError struct {
	Reason EventRefusal
	// Detail is for the log line, and is built only from this package's own
	// words and from identifiers — never from payload content.
	Detail string
	// Whatever parsed before the refusal, for the quarantine row.
	EventID       uuid.UUID
	SessionID     uuid.UUID
	SchemaVersion string
}

func (e *EventRefusalError) Error() string {
	if e.Detail == "" {
		return "event refused: " + string(e.Reason)
	}
	return "event refused: " + string(e.Reason) + ": " + e.Detail
}

// ErrEventRefused is matched by every *EventRefusalError.
var ErrEventRefused = errors.New("event refused")

// Is lets errors.Is(err, ErrEventRefused) match any refusal.
func (e *EventRefusalError) Is(target error) bool { return target == ErrEventRefused }

// SessionEvent is one accepted event, as stored.
type SessionEvent struct {
	EventID       uuid.UUID
	SessionID     uuid.UUID
	WorkspaceID   uuid.UUID
	RunnerID      uuid.UUID
	CorrelationID uuid.UUID
	Type          EventType
	SchemaVersion string
	// OccurredAt is the producer's claim, kept and never used for ordering.
	OccurredAt time.Time
	// ReceivedAt is when the control plane accepted it.
	ReceivedAt time.Time
	// Sequence is the per-session order of acceptance, gapless from 1. The
	// only order the control plane can vouch for.
	Sequence int64
	// Payload is re-encoded from the validated type, so a field the schema
	// does not know is never stored: "unknown fields are ignored" includes
	// not persisting them.
	Payload json.RawMessage
}

// EventEnvelope is the wire form. Producers build one and publish it.
type EventEnvelope struct {
	EventID       uuid.UUID       `json:"event_id"`
	SchemaVersion string          `json:"schema_version"`
	Type          EventType       `json:"type"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Producer      uuid.UUID       `json:"producer"`
	WorkspaceID   uuid.UUID       `json:"workspace_id"`
	SessionID     uuid.UUID       `json:"session_id"`
	CorrelationID uuid.UUID       `json:"correlation_id"`
	Payload       json.RawMessage `json:"payload"`
}

// MessageCreated is the message.created v1 payload.
type MessageCreated struct {
	MessageID uuid.UUID `json:"message_id"`
	Role      string    `json:"role"`
	Text      string    `json:"text"`
}

// ProviderFailed is the provider.failed v1 payload.
type ProviderFailed struct {
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	Message   string `json:"message"`
}

var (
	// No leading zeros and bounded, the same shape the database's CHECK
	// accepts. `"01.0"` read as major 1 here and was then rejected by the
	// table — which the ingestor took for a transient failure and retried.
	schemaVersionPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})$`)
	failureCodePattern   = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
)

// DecodeEvent parses and validates one encoded event.
//
// It establishes what the event *claims*. Whether the claims hold — the
// session matching the subject, the workspace matching the session row — is
// the ingestor's job, because only it can see the subject and the database.
//
// The checks run in the order that gives an operator the most specific
// reason: the version is judged before the type, because an unknown major
// may legitimately carry a type or payload this consumer cannot recognise.
func DecodeEvent(raw []byte) (SessionEvent, error) {
	if len(raw) > MaxEventBytes {
		return SessionEvent{}, &EventRefusalError{Reason: RefusalOversize,
			Detail: fmt.Sprintf("%d bytes, limit %d", len(raw), MaxEventBytes)}
	}

	// Decoded into raw fields first, so an envelope whose payload is a shape
	// from a future major can still yield its identifiers for the quarantine.
	var envelope struct {
		EventID       *uuid.UUID      `json:"event_id"`
		SchemaVersion string          `json:"schema_version"`
		Type          EventType       `json:"type"`
		OccurredAt    *time.Time      `json:"occurred_at"`
		Producer      *uuid.UUID      `json:"producer"`
		WorkspaceID   *uuid.UUID      `json:"workspace_id"`
		SessionID     *uuid.UUID      `json:"session_id"`
		CorrelationID *uuid.UUID      `json:"correlation_id"`
		Payload       json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return SessionEvent{}, &EventRefusalError{Reason: RefusalMalformed, Detail: "not a JSON object in envelope shape"}
	}

	refuse := func(reason EventRefusal, detail string) error {
		refusal := &EventRefusalError{Reason: reason, Detail: detail, SchemaVersion: envelope.SchemaVersion}
		if envelope.EventID != nil {
			refusal.EventID = *envelope.EventID
		}
		if envelope.SessionID != nil {
			refusal.SessionID = *envelope.SessionID
		}
		refusal.SchemaVersion = SafeText(refusal.SchemaVersion, 32)
		return refusal
	}

	switch {
	case envelope.EventID == nil || *envelope.EventID == uuid.Nil:
		return SessionEvent{}, refuse(RefusalMalformed, "event_id is missing")
	case envelope.SessionID == nil || *envelope.SessionID == uuid.Nil:
		return SessionEvent{}, refuse(RefusalMalformed, "session_id is missing")
	}

	match := schemaVersionPattern.FindStringSubmatch(envelope.SchemaVersion)
	if match == nil {
		return SessionEvent{}, refuse(RefusalMalformed, "schema_version is not MAJOR.MINOR")
	}
	major, err := strconv.Atoi(match[1])
	if err != nil || major != SupportedEventMajor {
		return SessionEvent{}, refuse(RefusalUnknownMajor,
			fmt.Sprintf("major %s, this consumer reads %d", match[1], SupportedEventMajor))
	}

	switch {
	case envelope.Producer == nil || *envelope.Producer == uuid.Nil:
		return SessionEvent{}, refuse(RefusalMalformed, "producer is missing")
	case envelope.WorkspaceID == nil || *envelope.WorkspaceID == uuid.Nil:
		return SessionEvent{}, refuse(RefusalMalformed, "workspace_id is missing")
	case envelope.CorrelationID == nil || *envelope.CorrelationID == uuid.Nil:
		return SessionEvent{}, refuse(RefusalMalformed, "correlation_id is missing")
	case envelope.OccurredAt == nil || envelope.OccurredAt.IsZero():
		return SessionEvent{}, refuse(RefusalMalformed, "occurred_at is missing")
	}

	payload, err := validatePayload(envelope.Type, envelope.Payload)
	if err != nil {
		var refusal *EventRefusalError
		if errors.As(err, &refusal) {
			return SessionEvent{}, refuse(refusal.Reason, refusal.Detail)
		}
		return SessionEvent{}, refuse(RefusalInvalidPayload, "payload does not match its schema")
	}

	return SessionEvent{
		EventID:       *envelope.EventID,
		SessionID:     *envelope.SessionID,
		WorkspaceID:   *envelope.WorkspaceID,
		RunnerID:      *envelope.Producer,
		CorrelationID: *envelope.CorrelationID,
		Type:          envelope.Type,
		SchemaVersion: envelope.SchemaVersion,
		OccurredAt:    envelope.OccurredAt.UTC(),
		Payload:       payload,
	}, nil
}

// validatePayload checks a payload against its type's v1 schema and returns
// it re-encoded from the validated struct.
//
// Re-encoding is the point. The raw payload may carry fields a newer minor
// added; they are ignored by the decoder and so absent from what is stored.
// Persisting them would put unvalidated producer data into history under a
// schema that does not describe it.
func validatePayload(eventType EventType, raw json.RawMessage) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		if eventType != EventMessageCreated && eventType != EventProviderFailed {
			return nil, &EventRefusalError{Reason: RefusalUnknownType, Detail: "type is not one this consumer knows"}
		}
		return nil, &EventRefusalError{Reason: RefusalInvalidPayload, Detail: "payload is not an object"}
	}

	switch eventType {
	case EventMessageCreated:
		var payload MessageCreated
		if err := json.Unmarshal(trimmed, &payload); err != nil {
			return nil, &EventRefusalError{Reason: RefusalInvalidPayload, Detail: "message.created payload does not decode"}
		}
		switch {
		case payload.MessageID == uuid.Nil:
			return nil, &EventRefusalError{Reason: RefusalInvalidPayload, Detail: "message_id is missing"}
		case payload.Role != "assistant" && payload.Role != "user" && payload.Role != "system":
			return nil, &EventRefusalError{Reason: RefusalInvalidPayload, Detail: "role is not assistant, user or system"}
		case payload.Text == "" || !storableText(payload.Text) || utf8.RuneCountInString(payload.Text) > 32768:
			return nil, &EventRefusalError{Reason: RefusalInvalidPayload, Detail: "text is empty, not storable text, or too long"}
		}
		return json.Marshal(payload)

	case EventProviderFailed:
		// Pointers for the required fields, so absent is told apart from
		// zero. A missing `message` used to decode as "" and be stored as
		// though the producer had sent it — history that does not satisfy
		// the contract it claims.
		var payload struct {
			Code      string  `json:"code"`
			Retryable *bool   `json:"retryable"`
			Message   *string `json:"message"`
		}
		if err := json.Unmarshal(trimmed, &payload); err != nil {
			return nil, &EventRefusalError{Reason: RefusalInvalidPayload, Detail: "provider.failed payload does not decode"}
		}
		switch {
		case !failureCodePattern.MatchString(payload.Code):
			return nil, &EventRefusalError{Reason: RefusalInvalidPayload, Detail: "code is not a stable identifier"}
		case payload.Retryable == nil:
			return nil, &EventRefusalError{Reason: RefusalInvalidPayload, Detail: "retryable is missing"}
		case payload.Message == nil:
			return nil, &EventRefusalError{Reason: RefusalInvalidPayload, Detail: "message is missing"}
		case !storableText(*payload.Message) || utf8.RuneCountInString(*payload.Message) > 2000:
			return nil, &EventRefusalError{Reason: RefusalInvalidPayload, Detail: "message is not storable text or too long"}
		}
		return json.Marshal(ProviderFailed{Code: payload.Code, Retryable: *payload.Retryable, Message: *payload.Message})

	default:
		// Unknown types are quarantined, not stored. The standards say
		// unknown fields are ignored and unknown majors rejected, and are
		// silent on unknown types; storing one would persist something
		// nothing downstream can render or validate, from an untrusted
		// producer.
		return nil, &EventRefusalError{Reason: RefusalUnknownType, Detail: "type is not one this consumer knows"}
	}
}

// storableText reports whether PostgreSQL will accept a string in jsonb.
//
// Valid UTF-8 without NUL. Provider output can contain a NUL byte;
// `json.Marshal` escapes it as \u0000, and jsonb refuses that escape outright.
// Accepting it here made the database reject the insert, which the ingestor
// could only read as a transient failure and retry — for a condition waiting
// can never change.
func storableText(s string) bool {
	return utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// SafeText makes producer-supplied text safe to store in a bounded text
// column: invalid UTF-8 and NUL replaced, then cut to at most n bytes on a
// rune boundary.
//
// Byte-slicing can split a multi-byte character, and PostgreSQL rejects the
// resulting invalid UTF-8. For the quarantine that was the worst possible
// failure: the insert failed on every redelivery, so the record meant to stop
// an event vanishing is exactly what could not be written.
func SafeText(s string, n int) string {
	s = strings.ReplaceAll(strings.ToValidUTF8(s, "\uFFFD"), "\x00", "\uFFFD")
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// EventSubject is the NATS subject a session's events travel on.
//
// Opaque identifiers only — the architecture requires subjects to carry
// opaque tenant identifiers and never names. The prefix is configurable so a
// test can use a stream no running ingestor consumes.
func EventSubject(prefix string, sessionID uuid.UUID) string {
	return prefix + "." + sessionID.String() + ".events"
}

// SessionFromSubject reads the session id out of a subject.
func SessionFromSubject(prefix, subject string) (uuid.UUID, error) {
	rest, ok := strings.CutPrefix(subject, prefix+".")
	if !ok {
		return uuid.Nil, &EventRefusalError{Reason: RefusalUnparseableSubject, Detail: "subject has the wrong prefix"}
	}
	raw, ok := strings.CutSuffix(rest, ".events")
	if !ok {
		return uuid.Nil, &EventRefusalError{Reason: RefusalUnparseableSubject, Detail: "subject has the wrong suffix"}
	}
	id, err := uuid.Parse(raw)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, &EventRefusalError{Reason: RefusalUnparseableSubject, Detail: "subject does not carry a session id"}
	}
	return id, nil
}
