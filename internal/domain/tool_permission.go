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
