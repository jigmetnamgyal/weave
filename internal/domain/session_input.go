package domain

import "github.com/google/uuid"

// SessionInput is creation-time task data with settings from the immutable agent
// version pin. No current task/profile lookups, credentials or approval grants.
// Task text and policy are untrusted data, not executable instructions. Future
// delivery must enforce encoded bounds and keep all content out of telemetry.
type SessionInput struct {
	SessionID      uuid.UUID
	WorkspaceID    uuid.UUID
	InputVersion   int32
	TaskID         uuid.UUID
	TaskTitle      string
	TaskBody       string
	AgentVersionID uuid.UUID
	Provider       Provider
	Model          string
	Capabilities   []Capability
	ToolPolicy     []byte
}
