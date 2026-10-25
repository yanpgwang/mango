package domain

// ToolPermissionDecision is one invocation's judgment, independent of its
// execution owner. Reason codes are machine-readable audit values.
type ToolPermissionDecision struct {
	Type       string `json:"type"`
	ReasonCode string `json:"reason_code,omitempty"`
}

func (d ToolPermissionDecision) Valid() bool {
	switch d.Type {
	case "allow":
		return d.ReasonCode == ""
	case "ask":
		return d.ReasonCode == "indeterminate"
	case "deny":
		return d.ReasonCode == "high_risk"
	default:
		return false
	}
}

type ToolPermissionEvaluation struct {
	Type                string                  `json:"type"`
	EvaluatedPermission *ToolPermissionDecision `json:"evaluated_permission,omitempty"`
}

// PermissionIntent retains original client provenance before working-model
// projection or compaction. Complete=false never grants automatic authority.
type PermissionIntent struct {
	AgentSystem string                  `json:"agent_system"`
	Entries     []PermissionIntentEntry `json:"entries"`
	Complete    bool                    `json:"complete"`
}

type PermissionIntentEntry struct {
	EventID  string `json:"event_id"`
	ThreadID string `json:"thread_id"`
	Type     string `json:"type"`
	Text     string `json:"text"`
}

// ToolPermissionOwner identifies one already-established durable turn. A
// judgment receipt never creates or revives this execution owner.
type ToolPermissionOwner struct {
	SessionID      string `json:"session_id"`
	ThreadID       string `json:"thread_id"`
	TriggerEventID string `json:"trigger_event_id"`
	AttemptID      string `json:"attempt_id"`
}

type ToolPermissionReceipt struct {
	ToolPermissionOwner
	ToolUseEventID   string                 `json:"tool_use_event_id"`
	ToolName         string                 `json:"tool_name"`
	InvocationHash   string                 `json:"invocation_hash"`
	ContextHash      string                 `json:"context_hash"`
	Decision         ToolPermissionDecision `json:"decision"`
	Model            Model                  `json:"model"`
	Usage            TokenUsage             `json:"usage"`
	StopReason       string                 `json:"stop_reason"`
	ResponseReceived bool                   `json:"response_received"`
}
