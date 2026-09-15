package domain

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Errors the agent domain raises.
var (
	// ErrInvalidAgent is returned when input fails an agent invariant.
	ErrInvalidAgent = errors.New("invalid agent")
	// ErrUnknownCapability is returned for a capability name outside the
	// closed set. Separate from ErrInvalidAgent because the fix is different:
	// a typo here silently disables a feature everywhere else, so it is worth
	// naming what was rejected.
	ErrUnknownCapability = errors.New("unknown capability")
	// ErrAgentNameTaken is returned when a workspace already has an agent of
	// that name.
	ErrAgentNameTaken = errors.New("an agent of that name already exists in this workspace")
)

// agentNameMaxLen matches the CHECK constraint in the migration.
const agentNameMaxLen = 80

// Provider is the coding agent behind a profile.
type Provider string

// The providers the product recognises.
const (
	ProviderClaudeCode Provider = "claude_code"
	ProviderCodex      Provider = "codex"
	// ProviderFake is a first-class provider, not a placeholder. The session
	// notes require a deterministic adapter to verify orchestration, events
	// and approvals before a paid provider is wired in.
	ProviderFake Provider = "fake"
)

// ParseProvider validates a provider from a caller.
func ParseProvider(value string) (Provider, error) {
	switch Provider(strings.TrimSpace(value)) {
	case ProviderClaudeCode:
		return ProviderClaudeCode, nil
	case ProviderCodex:
		return ProviderCodex, nil
	case ProviderFake:
		return ProviderFake, nil
	default:
		return "", fmt.Errorf("%w: unknown provider %q", ErrInvalidAgent, value)
	}
}

// Capability is something a provider either supports or does not.
type Capability string

// The closed set of capabilities.
//
// `context/architecture.md`: "No feature may assume every provider supports
// pause, structured tool calls, token accounting, or identical permission
// semantics." Each name here corresponds to something an adapter either
// implements or does not, so the UI can derive its controls from the answer
// rather than assuming.
const (
	CapabilityPause               Capability = "pause"
	CapabilityResume              Capability = "resume"
	CapabilityCancel              Capability = "cancel"
	CapabilitySendInstruction     Capability = "send_instruction"
	CapabilityStructuredToolCalls Capability = "structured_tool_calls"
	CapabilityTokenAccounting     Capability = "token_accounting"
)

// knownCapabilities is the set the database also enforces. Keep them in step.
var knownCapabilities = map[Capability]bool{
	CapabilityPause:               true,
	CapabilityResume:              true,
	CapabilityCancel:              true,
	CapabilitySendInstruction:     true,
	CapabilityStructuredToolCalls: true,
	CapabilityTokenAccounting:     true,
}

// ParseCapabilities validates a declared capability set.
//
// A closed set rather than free text, because an unrecognised name is a typo
// that silently disables a feature — the UI would simply never offer pause,
// and nothing would say why. Rejecting the write turns a silent
// misconfiguration into an error at the moment it is made.
//
// Duplicates are collapsed and the result is sorted, so two equivalent
// declarations store identically and a later comparison means what it looks
// like.
func ParseCapabilities(values []string) ([]Capability, error) {
	seen := make(map[Capability]bool, len(values))
	for _, value := range values {
		capability := Capability(strings.TrimSpace(value))
		if !knownCapabilities[capability] {
			return nil, fmt.Errorf("%w: %q", ErrUnknownCapability, value)
		}
		seen[capability] = true
	}

	capabilities := make([]Capability, 0, len(seen))
	for capability := range seen {
		capabilities = append(capabilities, capability)
	}
	sort.Slice(capabilities, func(i, j int) bool { return capabilities[i] < capabilities[j] })
	return capabilities, nil
}

// Agent is a profile's identity and its pointer at the settings in use.
//
// Everything here is editable, because none of it changes what a finished
// session did. The settings are in AgentVersion, which is not.
type Agent struct {
	ID          uuid.UUID
	WorkspaceID uuid.UUID
	Name        string
	// CurrentVersionID is absent only between creating the agent and its first
	// version, which happens in one transaction.
	CurrentVersionID *uuid.UUID
	CreatedBy        uuid.UUID
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// AgentVersion is the settings an agent ran under, frozen.
//
// Immutable once written. A session is pinned to a version rather than an
// agent, because invariants 9 and 10 make session history append-only and
// terminal history immutable — which cannot hold if the profile a session ran
// under can be edited afterwards.
type AgentVersion struct {
	ID           uuid.UUID
	AgentID      uuid.UUID
	WorkspaceID  uuid.UUID
	Version      int32
	Provider     Provider
	Model        string
	Capabilities []Capability
	// ToolPolicy is opaque here and interpreted in M7. It is versioned because
	// it is a security control: a session must be readable against the policy
	// it actually ran under, not the one in force today.
	ToolPolicy []byte
	CreatedBy  uuid.UUID
	CreatedAt  time.Time
}

// Supports reports whether the version declares a capability.
func (v AgentVersion) Supports(capability Capability) bool {
	for _, declared := range v.Capabilities {
		if declared == capability {
			return true
		}
	}
	return false
}

// ValidateAgentName normalises and checks a name.
func ValidateAgentName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	switch {
	case trimmed == "":
		return "", fmt.Errorf("%w: a name is required", ErrInvalidAgent)
	case len(trimmed) > agentNameMaxLen:
		return "", fmt.Errorf("%w: a name may be at most %d characters", ErrInvalidAgent, agentNameMaxLen)
	}
	return trimmed, nil
}

// ValidateModel checks a model identifier.
//
// Not checked against a list of known models, deliberately: model names change
// far faster than this code would, and refusing an unrecognised one would make
// a new release unusable until someone edited a constant. The provider
// rejects a model it does not have, which is the authority that stays current.
func ValidateModel(model string) (string, error) {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return "", fmt.Errorf("%w: a model is required", ErrInvalidAgent)
	}
	if len(trimmed) > 200 {
		return "", fmt.Errorf("%w: a model identifier may be at most 200 characters", ErrInvalidAgent)
	}
	return trimmed, nil
}

// NewAgentID returns an identifier for a new agent.
func NewAgentID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate agent id: %w", err)
	}
	return id, nil
}

// NewAgentVersionID returns an identifier for a new agent version.
func NewAgentVersionID() (uuid.UUID, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, fmt.Errorf("generate agent version id: %w", err)
	}
	return id, nil
}

// CapabilityStrings renders capabilities for storage and transport.
func CapabilityStrings(capabilities []Capability) []string {
	values := make([]string, 0, len(capabilities))
	for _, capability := range capabilities {
		values = append(values, string(capability))
	}
	return values
}
