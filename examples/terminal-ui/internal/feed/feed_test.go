package feed

import (
	"testing"

	"github.com/yanpgwang/mango/examples/terminal-ui/internal/mango"
)

func TestProjectPairsToolResult(t *testing.T) {
	thread := mango.Thread{ID: "sthr_primary"}
	thread.Agent.Name = "coordinator"
	events := []mango.Event{
		{"id": "sevt_use", "type": "agent.tool_use", "name": "bash", "input": map[string]any{"command": "pwd"}},
		{"id": "sevt_result", "type": "agent.tool_result", "tool_use_id": "sevt_use", "content": []any{map[string]any{"type": "text", "text": "/workspace"}}},
	}
	items := Project(thread, events)
	if len(items) != 1 || items[0].ToolName != "bash" || items[0].Status != "done" || items[0].Body != "/workspace" {
		t.Fatalf("items = %#v", items)
	}
}

func TestPendingActionsUsesCrossPostedThreadHint(t *testing.T) {
	events := map[string][]mango.Event{"sthr_primary": {
		{"id": "sevt_action", "type": "agent.tool_use", "name": "bash", "evaluated_permission": "ask", "session_thread_id": "sthr_child"},
		{"id": "sevt_idle", "type": "session.status_idle", "stop_reason": map[string]any{"type": "requires_action", "event_ids": []any{"sevt_action"}}},
	}}
	actions := PendingActions("sthr_primary", events)
	if len(actions) != 1 || actions[0].ThreadID != "sthr_child" || actions[0].Kind != mango.ActionConfirmation {
		t.Fatalf("actions = %#v", actions)
	}
	events["sthr_primary"] = append(events["sthr_primary"], mango.Event{
		"id": "sevt_done", "type": "session.status_idle", "stop_reason": map[string]any{"type": "end_turn"},
	})
	if actions = PendingActions("sthr_primary", events); len(actions) != 0 {
		t.Fatalf("resolved actions = %#v", actions)
	}
}

func TestProjectMarksFormerBarrierActionResolved(t *testing.T) {
	thread := mango.Thread{ID: "sthr_primary"}
	events := []mango.Event{
		{"id": "sevt_action", "type": "agent.tool_use", "name": "bash", "evaluated_permission": "ask"},
		{"id": "sevt_wait", "type": "session.status_idle", "stop_reason": map[string]any{"type": "requires_action", "event_ids": []any{"sevt_action"}}},
		{"id": "sevt_done", "type": "session.status_idle", "stop_reason": map[string]any{"type": "end_turn"}},
	}
	items := Project(thread, events)
	if len(items) != 1 || items[0].Status != "resolved" {
		t.Fatalf("items = %#v", items)
	}
}

func TestProjectOmitsDurableThinkingMarker(t *testing.T) {
	thread := mango.Thread{ID: "sthr_primary"}
	items := Project(thread, []mango.Event{
		{"id": "sevt_thinking", "type": "agent.thinking"},
		{"id": "sevt_message", "type": "agent.message", "content": []any{map[string]any{"type": "text", "text": "done"}}},
	})
	if len(items) != 1 || items[0].Kind != Agent || items[0].Body != "done" {
		t.Fatalf("items = %#v", items)
	}
}
