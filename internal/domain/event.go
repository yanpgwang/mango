package domain

import "time"

// Public event type constants. Every event is a top-level tagged union keyed on
// the "type" field following the {domain}.{action} convention. Mango currently
// retains wire values informed by the public Managed Agents events reference.
const (
	// Client-submittable input events.
	EvUserMessage          = "user.message"
	EvUserInterrupt        = "user.interrupt"
	EvUserToolConfirmation = "user.tool_confirmation"
	EvUserCustomToolResult = "user.custom_tool_result"
	EvUserToolResult       = "user.tool_result"
	EvUserDefineOutcome    = "user.define_outcome"
	EvSystemMessage        = "system.message"

	// Agent/server-emitted events (never accepted from clients).
	EvAgentMessage       = "agent.message"
	EvAgentThinking      = "agent.thinking"
	EvAgentCustomToolUse = "agent.custom_tool_use"
	EvAgentToolUse       = "agent.tool_use"
	EvAgentToolResult    = "agent.tool_result"

	// MCP tool calls are a distinct documented variant of the tool-use pair. The
	// use event additionally carries a required mcp_server_name and reports the
	// bare tool name as the server published it (not the namespaced model-facing
	// alias). The result event correlates through mcp_tool_use_id and carries no
	// server name: a client attributes a result to a server by joining back to
	// its use event.
	EvAgentMcpToolUse    = "agent.mcp_tool_use"
	EvAgentMcpToolResult = "agent.mcp_tool_result"

	EvSessionStatusIdle         = "session.status_idle"
	EvSessionStatusRunning      = "session.status_running"
	EvSessionStatusTerminated   = "session.status_terminated"
	EvSessionStatusRescheduling = "session.status_rescheduled"
	EvSessionError              = "session.error"
	EvSessionUpdated            = "session.updated"
	EvSessionUsage              = "session.usage"
	EvSessionDeleted            = "session.deleted"

	EvSessionThreadCreated           = "session.thread_created"
	EvSessionThreadStatusIdle        = "session.thread_status_idle"
	EvSessionThreadStatusRunning     = "session.thread_status_running"
	EvSessionThreadStatusTerminated  = "session.thread_status_terminated"
	EvSessionThreadStatusRescheduled = "session.thread_status_rescheduled"
	EvAgentThreadMessageReceived     = "agent.thread_message_received"
	EvAgentThreadMessageSent         = "agent.thread_message_sent"
	EvAgentThreadContextCompacted    = "agent.thread_context_compacted"

	EvSpanOutcomeEvaluationStart   = "span.outcome_evaluation_start"
	EvSpanOutcomeEvaluationOngoing = "span.outcome_evaluation_ongoing"
	EvSpanOutcomeEvaluationEnd     = "span.outcome_evaluation_end"
	EvSpanModelRequestStart        = "span.model_request_start"
	EvSpanModelRequestEnd          = "span.model_request_end"
)

// Internal event payload keys support server-side causal linking without
// changing the public Mango event shape. HTTP projections must never
// expose keys with this prefix.
const (
	InternalCompanionSystemEventID = "__companion_system_event_id"
	InternalCompanionSystemContent = "__companion_system_content"
	InternalToolExecutionOwner     = "__tool_execution_owner"
	InternalOutcomeEvaluationStart = "__outcome_evaluation_start_id"
	InternalOutcomeIteration       = "__outcome_evaluation_iteration"
	InternalOutcomeRubricContent   = "__outcome_rubric_content"
	InternalFileMessageContents    = "__file_message_contents"
)

// EventDraft is an event about to be persisted. Payload holds the type-specific
// top-level fields (everything except "type"); it is flattened onto the wire
// object, never nested under a "payload" key.
//
// ID is normally empty: the store assigns the committed id at persist time. A
// server-side emitter (the runtime sink) may pre-assign the committed id so it
// can reference the event before the persist transaction runs — for example to
// correlate a tool_result to its tool_use, or to name the parked
// agent.custom_tool_use / agent.tool_use events in a requires_action stop
// reason. Client-submitted events never carry an id (rejected at the edge).
type EventDraft struct {
	ID      string
	Type    string
	Payload map[string]any
}

// Event is the internal representation of a persisted session event. Sequence
// is an internal ordering key and must never appear on the public wire.
type Event struct {
	ID        string
	SessionID string
	// ThreadID is the ledger that owns this event. Sequence remains Session-wide
	// so aggregate ordering and cross-post causality have one durable cursor.
	ThreadID string
	Sequence int64
	Type     string
	Payload  map[string]any
	// TurnEventID is internal causal metadata. A non-nil value means this event
	// was output of that model-driving turn, not a second independent trigger.
	// HTTP projections intentionally omit it.
	TurnEventID *string `json:"-"`
	CreatedAt   time.Time
	ProcessedAt *time.Time
}

// IsUserEvent reports whether t is one of the user.* input event types.
func IsUserEvent(t string) bool {
	switch t {
	case EvUserMessage, EvUserInterrupt, EvUserToolConfirmation,
		EvUserCustomToolResult, EvUserToolResult, EvUserDefineOutcome:
		return true
	}
	return false
}

// IsClientSubmittable reports whether a client may POST an event of this type to
// the send-events endpoint. The set is exactly the documented user.* inputs plus
// system.message. Agent-, session-, and span-scoped types are server-only and
// must be rejected; that includes agent.mcp_tool_use and agent.mcp_tool_result,
// which are emitted by the runtime and never accepted from a caller.
func IsClientSubmittable(t string) bool {
	return IsUserEvent(t) || t == EvSystemMessage
}

// IsSessionCredentialEvent reports the client events an untrusted self-hosted
// execution scope may submit. A Session worker answers tool calls; it cannot
// inject user instructions or higher-priority system context.
func IsSessionCredentialEvent(t string) bool {
	return t == EvUserToolResult || t == EvUserCustomToolResult
}

// IsAgentToolUse reports whether a type is one of the server-emitted tool-call
// announcements. Both agent.tool_use and agent.mcp_tool_use name a call the
// runtime made on the model's behalf; agent.custom_tool_use names one the client
// must execute.
func IsAgentToolUse(t string) bool {
	switch t {
	case EvAgentToolUse, EvAgentMcpToolUse, EvAgentCustomToolUse:
		return true
	}
	return false
}

// AgentToolResultReference returns the tool-use event id a server-emitted tool
// result correlates to, reading the id field the documented variant uses:
// tool_use_id for agent.tool_result and mcp_tool_use_id for
// agent.mcp_tool_result. ok is false for any other event type.
func AgentToolResultReference(
	eventType string,
	payload map[string]any,
) (toolUseEventID string, ok bool) {
	switch eventType {
	case EvAgentToolResult:
		id, _ := payload["tool_use_id"].(string)
		return id, true
	case EvAgentMcpToolResult:
		id, _ := payload["mcp_tool_use_id"].(string)
		return id, true
	}
	return "", false
}

// AgentToolResultTypeFor returns the result event type that answers a
// server-emitted tool-use event: agent.mcp_tool_result for agent.mcp_tool_use
// and agent.tool_result for every other server-executed call.
//
// The pairing is a property of the committed use event, not of the code that
// happens to be running when the answer is produced. A tool call can park on a
// client confirmation for an unbounded time and be answered by a later, upgraded
// process, so the answering side must read the durable type rather than assume
// its own naming scheme.
func AgentToolResultTypeFor(toolUseEventType string) string {
	if toolUseEventType == EvAgentMcpToolUse {
		return EvAgentMcpToolResult
	}
	return EvAgentToolResult
}

// IsInitialEventType reports whether a type is allowed in a session's
// initial_events. Only user.message and user.define_outcome are accepted there;
// unlike scheduled deployments, initial_events does not accept system.message.
func IsInitialEventType(t string) bool {
	return t == EvUserMessage || t == EvUserDefineOutcome
}

// ProcessedOnReceipt reports whether an event is stamped processed_at at persist
// time rather than when a later turn consumes it. Server-only events are already
// processed when emitted; selected client events are acknowledged on receipt.
func ProcessedOnReceipt(t string) bool {
	// Cross-Thread messages are server-emitted but model-driving input on the
	// receiving Thread. They remain unprocessed until that Thread turn commits,
	// exactly like user.message on the primary ledger.
	if t == EvAgentThreadMessageReceived {
		return false
	}
	if !IsClientSubmittable(t) {
		return true
	}
	switch t {
	case EvUserDefineOutcome, EvUserCustomToolResult, EvUserToolResult:
		return true
	}
	return false
}
