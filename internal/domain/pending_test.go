package domain

import "testing"

func TestPendingActionKindForEvent(t *testing.T) {
	cases := []struct {
		eventType string
		payload   map[string]any
		want      PendingActionKind
		ok        bool
	}{
		{EvAgentCustomToolUse, nil, PendingCustomToolResult, true},
		// agent.tool_use parks only when its evaluated permission is "ask".
		{EvAgentToolUse, map[string]any{"evaluated_permission": "ask"}, PendingToolConfirmation, true},
		{EvAgentToolUse, map[string]any{InternalToolExecutionOwner: "self_hosted", "evaluated_permission": "allow"}, PendingToolResult, true},
		{EvAgentToolUse, map[string]any{InternalToolExecutionOwner: "self_hosted", "evaluated_permission": "ask"}, PendingToolConfirmation, true},
		{EvAgentToolUse, map[string]any{InternalToolExecutionOwner: "self_hosted", "evaluated_permission": "deny"}, "", false},
		{EvAgentToolUse, map[string]any{InternalToolExecutionOwner: "self_hosted"}, "", false},
		{EvAgentToolUse, map[string]any{"evaluated_permission": "always_allow"}, "", false},
		{EvAgentToolUse, map[string]any{"evaluated_permission": "always_deny"}, "", false},
		{EvAgentToolUse, map[string]any{}, "", false},
		{EvAgentToolUse, nil, "", false},
		// agent.mcp_tool_use parks on the same confirmation gate: upstream
		// documents one evaluated_permission enum and one user.tool_confirmation
		// input for both tool-use variants.
		{EvAgentMcpToolUse, map[string]any{"evaluated_permission": "ask"}, PendingToolConfirmation, true},
		{EvAgentMcpToolUse, map[string]any{"evaluated_permission": "allow"}, "", false},
		{EvAgentMcpToolUse, map[string]any{}, "", false},
		// A result event never parks anything.
		{EvAgentMcpToolResult, map[string]any{"mcp_tool_use_id": "sevt_1"}, "", false},
		{EvAgentMessage, nil, "", false},
		{EvUserMessage, nil, "", false},
	}
	for _, c := range cases {
		got, ok := PendingActionKindForEvent(c.eventType, c.payload)
		if got != c.want || ok != c.ok {
			t.Errorf("PendingActionKindForEvent(%q, %v) = %q,%v want %q,%v", c.eventType, c.payload, got, ok, c.want, c.ok)
		}
	}
}

func TestResolutionReference(t *testing.T) {
	id, kind, ok := ResolutionReference(EvUserCustomToolResult, map[string]any{"custom_tool_use_id": "sevt_1"})
	if !ok || id != "sevt_1" || kind != PendingCustomToolResult {
		t.Fatalf("custom_tool_result = %q,%q,%v", id, kind, ok)
	}
	id, kind, ok = ResolutionReference(EvUserToolConfirmation, map[string]any{"tool_use_id": "sevt_2"})
	if !ok || id != "sevt_2" || kind != PendingToolConfirmation {
		t.Fatalf("tool_confirmation = %q,%q,%v", id, kind, ok)
	}
	id, kind, ok = ResolutionReference(EvUserToolResult, map[string]any{"tool_use_id": "sevt_3"})
	if !ok || id != "sevt_3" || kind != PendingToolResult {
		t.Fatalf("tool_result = %q,%q,%v", id, kind, ok)
	}
	// A resolution event missing its reference field is not a valid resolution.
	if _, _, ok := ResolutionReference(EvUserCustomToolResult, map[string]any{}); ok {
		t.Error("custom_tool_result without custom_tool_use_id should not resolve")
	}
	// Non-resolution event types never resolve.
	if _, _, ok := ResolutionReference(EvUserMessage, map[string]any{"custom_tool_use_id": "x"}); ok {
		t.Error("user.message should not be a resolution")
	}
}

// A confirmation for an MCP call keeps the single documented tool_use_id field.
// There is no mcp_tool_use_id on the confirmation input, so the barrier must
// resolve an agent.mcp_tool_use park through tool_use_id alone.
func TestResolutionReference_MCPConfirmationUsesToolUseID(t *testing.T) {
	kind, ok := PendingActionKindForEvent(
		EvAgentMcpToolUse,
		map[string]any{"evaluated_permission": "ask"},
	)
	if !ok || kind != PendingToolConfirmation {
		t.Fatalf("mcp park = %q,%v", kind, ok)
	}
	id, refKind, ok := ResolutionReference(
		EvUserToolConfirmation,
		map[string]any{"tool_use_id": "sevt_mcp", "result": "allow"},
	)
	if !ok || id != "sevt_mcp" || refKind != kind {
		t.Fatalf("confirmation = %q,%q,%v", id, refKind, ok)
	}
	// mcp_tool_use_id belongs to the agent.mcp_tool_result event, never to a
	// client confirmation.
	if _, _, ok := ResolutionReference(
		EvUserToolConfirmation,
		map[string]any{"mcp_tool_use_id": "sevt_mcp", "result": "allow"},
	); ok {
		t.Error("mcp_tool_use_id must not be accepted on user.tool_confirmation")
	}
}

func TestAgentToolResultReference(t *testing.T) {
	if id, ok := AgentToolResultReference(
		EvAgentToolResult,
		map[string]any{"tool_use_id": "sevt_builtin"},
	); !ok || id != "sevt_builtin" {
		t.Fatalf("tool_result = %q,%v", id, ok)
	}
	if id, ok := AgentToolResultReference(
		EvAgentMcpToolResult,
		map[string]any{"mcp_tool_use_id": "sevt_mcp"},
	); !ok || id != "sevt_mcp" {
		t.Fatalf("mcp_tool_result = %q,%v", id, ok)
	}
	// The variants do not share an id field in either direction.
	if id, _ := AgentToolResultReference(
		EvAgentMcpToolResult,
		map[string]any{"tool_use_id": "sevt_mcp"},
	); id != "" {
		t.Error("agent.mcp_tool_result must not read tool_use_id")
	}
	if _, ok := AgentToolResultReference(EvUserToolResult, nil); ok {
		t.Error("user.tool_result is not a server tool result")
	}
}

// The MCP tool events are server-emitted and must never be accepted from a
// caller on the send-events endpoint.
func TestMCPToolEventsAreServerOnly(t *testing.T) {
	for _, eventType := range []string{EvAgentMcpToolUse, EvAgentMcpToolResult} {
		if IsClientSubmittable(eventType) {
			t.Errorf("%s must not be client-submittable", eventType)
		}
		if IsUserEvent(eventType) {
			t.Errorf("%s must not be a user event", eventType)
		}
		if !ProcessedOnReceipt(eventType) {
			t.Errorf("%s is server-emitted and is processed on receipt", eventType)
		}
		if IsInitialEventType(eventType) {
			t.Errorf("%s must not be accepted in initial_events", eventType)
		}
	}
	if !IsAgentToolUse(EvAgentMcpToolUse) {
		t.Error("agent.mcp_tool_use is a tool-use announcement")
	}
	if IsAgentToolUse(EvAgentMcpToolResult) {
		t.Error("agent.mcp_tool_result is not a tool-use announcement")
	}
}
