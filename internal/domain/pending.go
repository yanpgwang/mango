package domain

import "time"

// PendingActionKind is the next expected response for a parked action. It is
// derived from the committed action event's type and payload, then may advance
// after a durable approval — never from an arbitrary caller-provided kind.
type PendingActionKind string

const (
	// PendingCustomToolResult is parked by an agent.custom_tool_use event and is
	// resolved by a user.custom_tool_result whose custom_tool_use_id references
	// the parked event.
	PendingCustomToolResult PendingActionKind = "custom_tool_result"
	// PendingToolConfirmation is parked by an always_ask agent.tool_use event and
	// is resolved by a user.tool_confirmation whose tool_use_id references the
	// parked event. Allow executes a server-owned tool or advances an external
	// call to PendingToolResult; deny produces an error result without execution.
	PendingToolConfirmation PendingActionKind = "tool_confirmation"
	// PendingToolResult is parked by a self-hosted agent.tool_use. The client
	// executes the sandbox-routed tool and resolves it with user.tool_result.
	PendingToolResult PendingActionKind = "tool_result"
)

// PendingAction is a first-class durable record that a run parked awaiting a
// client response. It is internal-only and never serialized onto the public
// Mango wire. ResolvedAt is nil while the action still blocks ordinary
// queued work on its owning Thread.
type PendingAction struct {
	ID                  string
	SessionID           string
	ThreadID            string
	ActionEventID       string
	ClientActionEventID string
	Kind                PendingActionKind
	// ApprovalEventID is the durable allow receipt for an external tool. It
	// advances the expected response to tool_result without resolving the call.
	ApprovalEventID  *string
	ResolvingEventID *string
	CreatedAt        time.Time
	ResolvedAt       *time.Time
}

// PrefixPendingAction is the id prefix for durable pending-action records.
const PrefixPendingAction = "pact_"

// PendingActionKindForEvent derives the expected response kind for a parked
// action event from its committed type AND payload. ok is false when the event
// cannot park a run.
//
// Server-executed agent.tool_use and agent.mcp_tool_use park only for "ask";
// permitted calls execute inline and denied calls are not confirmation gates.
// Self-hosted agent.tool_use parks for confirmation on "ask" or a result on
// "allow". Execution ownership must never bypass the permission gate.
// agent.custom_tool_use always parks regardless of payload.
func PendingActionKindForEvent(eventType string, payload map[string]any) (PendingActionKind, bool) {
	switch eventType {
	case EvAgentCustomToolUse:
		return PendingCustomToolResult, true
	case EvAgentToolUse, EvAgentMcpToolUse:
		if perm, _ := payload["evaluated_permission"].(string); perm == "ask" {
			return PendingToolConfirmation, true
		}
		if IsSelfHostedToolCall(eventType, payload) && payload["evaluated_permission"] == "allow" {
			return PendingToolResult, true
		}
		return "", false
	}
	return "", false
}

// IsSelfHostedToolCall uses the server-owned execution marker, never a client
// assertion, to distinguish external built-ins from server-executed tools.
func IsSelfHostedToolCall(eventType string, payload map[string]any) bool {
	return eventType == EvAgentToolUse && payload[InternalToolExecutionOwner] == "self_hosted"
}

// ResolutionReference reports whether an event resolves a pending action and, if
// so, the parked action event id it references and the kind it satisfies. The
// referenced id lives in a type-specific payload field (custom_tool_use_id for
// user.custom_tool_result, tool_use_id for user.tool_confirmation); a resolution
// event missing that field returns ok=false.
//
// user.tool_confirmation keeps tool_use_id for both agent.tool_use and
// agent.mcp_tool_use parks: the documented confirmation input has exactly one id
// field and never a separate MCP spelling.
func ResolutionReference(eventType string, payload map[string]any) (actionEventID string, kind PendingActionKind, ok bool) {
	switch eventType {
	case EvUserCustomToolResult:
		id, _ := payload["custom_tool_use_id"].(string)
		if id == "" {
			return "", "", false
		}
		return id, PendingCustomToolResult, true
	case EvUserToolConfirmation:
		id, _ := payload["tool_use_id"].(string)
		if id == "" {
			return "", "", false
		}
		return id, PendingToolConfirmation, true
	case EvUserToolResult:
		id, _ := payload["tool_use_id"].(string)
		if id == "" {
			return "", "", false
		}
		return id, PendingToolResult, true
	}
	return "", "", false
}
