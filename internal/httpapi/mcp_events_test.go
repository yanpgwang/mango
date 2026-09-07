package httpapi

import (
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

// The public projection is a flat tagged union, so a committed MCP event must
// already carry the documented field names. In particular the result event
// exposes mcp_tool_use_id (never tool_use_id) and carries no mcp_server_name.
func TestEventToJSON_MCPToolEventsUseDocumentedFields(t *testing.T) {
	processedAt := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	use := eventToJSON(domain.Event{
		ID:   "sevt_mcp",
		Type: domain.EvAgentMcpToolUse,
		Payload: map[string]any{
			"name":                 "list_issues",
			"mcp_server_name":      "github",
			"input":                map[string]any{"repo": "mango"},
			"evaluated_permission": "ask",
		},
		ProcessedAt: &processedAt,
	})
	if use["type"] != domain.EvAgentMcpToolUse || use["id"] != "sevt_mcp" {
		t.Fatalf("mcp tool use = %#v", use)
	}
	if use["name"] != "list_issues" || use["mcp_server_name"] != "github" {
		t.Fatalf("mcp tool use fields = %#v", use)
	}
	if use["evaluated_permission"] != "ask" {
		t.Fatalf("evaluated_permission = %#v", use["evaluated_permission"])
	}

	result := eventToJSON(domain.Event{
		ID:   "sevt_mcp_result",
		Type: domain.EvAgentMcpToolResult,
		Payload: map[string]any{
			"mcp_tool_use_id": "sevt_mcp",
			"content":         []any{map[string]any{"type": "text", "text": "#1"}},
			"is_error":        false,
		},
		ProcessedAt: &processedAt,
	})
	if result["mcp_tool_use_id"] != "sevt_mcp" {
		t.Fatalf("mcp tool result = %#v", result)
	}
	if _, present := result["tool_use_id"]; present {
		t.Fatalf("agent.mcp_tool_result must not expose tool_use_id: %#v", result)
	}
	if _, present := result["mcp_server_name"]; present {
		t.Fatalf("agent.mcp_tool_result must not expose mcp_server_name: %#v", result)
	}
}

func TestEventToJSON_RedactsResolvedFileContent(t *testing.T) {
	event := eventToJSON(domain.Event{
		ID: "sevt_outcome", Type: domain.EvUserDefineOutcome,
		Payload: map[string]any{
			"description": "produce report",
			"rubric": map[string]any{
				"type": "file", "file_id": "file_rubric",
			},
			domain.InternalOutcomeRubricContent: "private rubric bytes",
			domain.InternalFileMessageContents: map[string]any{
				"0": map[string]any{"content": "private message File bytes"},
			},
		},
	})
	if event["rubric"].(map[string]any)["file_id"] != "file_rubric" {
		t.Fatalf("public rubric = %#v", event["rubric"])
	}
	if _, present := event[domain.InternalOutcomeRubricContent]; present {
		t.Fatalf("private rubric content leaked: %#v", event)
	}
	if _, present := event[domain.InternalFileMessageContents]; present {
		t.Fatalf("private message File content leaked: %#v", event)
	}
}

// The new server-emitted types are not accepted from callers.
func TestSendEvents_RejectsMCPToolEventTypes(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"self_hosted"}}`)
	id := createID(t, h, "POST", "/v1/sessions",
		`{"agent":"`+ag+`","environment_id":"`+env+`"}`)

	for _, body := range []string{
		`{"events":[{"type":"agent.mcp_tool_use","name":"list_issues","mcp_server_name":"github","input":{}}]}`,
		`{"events":[{"type":"agent.mcp_tool_result","mcp_tool_use_id":"sevt_x"}]}`,
		// A confirmation must keep tool_use_id; mcp_tool_use_id is not an input.
		`{"events":[{"type":"user.tool_confirmation","mcp_tool_use_id":"sevt_x","result":"allow"}]}`,
	} {
		if rec := do(h, "POST", "/v1/sessions/"+id+"/events", body); rec.Code != 400 {
			t.Errorf("body %s: got %d, want 400 (%s)", body, rec.Code, rec.Body)
		}
	}

	confirmation := `{"events":[{"type":"user.tool_confirmation",` +
		`"tool_use_id":"sevt_x","result":"allow"}]}`
	if rec := do(h, "POST", "/v1/sessions/"+id+"/events", confirmation); rec.Code == 400 {
		t.Fatalf("tool_use_id must stay the confirmation field: %s", rec.Body)
	}
}
