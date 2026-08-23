// Package feed projects Mango's flat event union into a conversational feed.
// The wire ledger remains authoritative; this package contains no UI state.
package feed

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yanpgwang/mango/examples/terminal-ui/internal/mango"
)

type Kind string

const (
	User       Kind = "user"
	Agent      Kind = "agent"
	Thinking   Kind = "thinking"
	Tool       Kind = "tool"
	Delegation Kind = "delegation"
	Report     Kind = "report"
	Notice     Kind = "notice"
	Failure    Kind = "failure"
)

type Item struct {
	ID       string
	Kind     Kind
	Title    string
	Body     string
	Detail   string
	Status   string
	ToolName string
	RawType  string
	Time     time.Time
}

func Project(thread mango.Thread, events []mango.Event) []Item {
	items := make([]Item, 0, len(events))
	tools := map[string]int{}
	barrier := latestBarrier(events)
	for _, event := range events {
		typ := event.Type()
		item := Item{ID: event.ID(), RawType: typ, Time: eventTime(event)}
		switch typ {
		case "user.message":
			item.Kind, item.Title, item.Body = User, "You", ContentText(event["content"])
		case "agent.message":
			item.Kind, item.Title, item.Body = Agent, first(thread.Agent.Name, "Agent"), ContentText(event["content"])
		case "agent.thinking":
			// The durable event is a privacy-preserving marker with no body.
			// Live preview frames already drive the working indicator, so keeping
			// this row would leave a permanent, empty "thinking" entry in chat.
			continue
		case "agent.tool_use", "agent.mcp_tool_use", "agent.custom_tool_use":
			item.Kind = Tool
			item.ToolName = first(stringValue(event["name"]), "tool")
			item.Title = item.ToolName
			item.Detail = JSONText(event["input"])
			item.Status = toolStatus(event, barrier)
			if item.ID != "" {
				tools[item.ID] = len(items)
			}
		case "agent.tool_result", "agent.mcp_tool_result", "user.tool_result", "user.custom_tool_result":
			reference := first(
				stringValue(event["tool_use_id"]),
				stringValue(event["mcp_tool_use_id"]),
				stringValue(event["custom_tool_use_id"]),
			)
			if index, ok := tools[reference]; ok {
				items[index].Body = ContentText(event["content"])
				if boolValue(event["is_error"]) {
					items[index].Status = "error"
				} else {
					items[index].Status = "done"
				}
				continue
			}
			item.Kind, item.Title, item.Body = Tool, "tool result", ContentText(event["content"])
			item.Status = "error"
			if !boolValue(event["is_error"]) {
				item.Status = "done"
			}
		case "agent.thread_message_sent":
			item.Body = ContentText(event["content"])
			peer := first(stringValue(event["to_agent_name"]), "agent")
			if thread.Primary() {
				item.Kind, item.Title = Delegation, "Delegated to "+peer
			} else {
				item.Kind, item.Title = Report, "Sent to "+peer
			}
		case "agent.thread_message_received":
			item.Body = ContentText(event["content"])
			peer := first(stringValue(event["from_agent_name"]), "agent")
			if thread.Primary() {
				item.Kind, item.Title = Report, "Report from "+peer
			} else {
				item.Kind, item.Title = Delegation, "Task from "+peer
			}
		case "agent.thread_context_compacted":
			item.Kind, item.Title = Notice, "Context compacted"
		case "user.interrupt":
			item.Kind, item.Title = Notice, "Interrupted"
		case "session.error":
			item.Kind, item.Title, item.Body = Failure, "Session error", JSONText(event["error"])
		default:
			continue
		}
		items = append(items, item)
	}
	return items
}

// PendingActions derives the aggregate action barrier from the primary ledger.
// Child actions are cross-posted there with the client-visible ID that must be
// used when responding; their canonical child IDs are intentionally ignored.
func PendingActions(primaryThreadID string, events map[string][]mango.Event) []mango.Action {
	ledger := events[primaryThreadID]
	byID := make(map[string]mango.Event, len(ledger))
	var required []string
	for _, event := range ledger {
		if event.ID() != "" {
			byID[event.ID()] = event
		}
		if event.Type() != "session.status_idle" {
			continue
		}
		stop, _ := event["stop_reason"].(map[string]any)
		if stringValue(stop["type"]) == "requires_action" {
			required = stringSlice(stop["event_ids"])
		} else {
			required = nil
		}
	}
	actions := make([]mango.Action, 0, len(required))
	for _, id := range required {
		event, ok := byID[id]
		if !ok {
			continue
		}
		var kind mango.ActionKind
		switch event.Type() {
		case "agent.custom_tool_use":
			kind = mango.ActionCustomResult
		case "agent.tool_use", "agent.mcp_tool_use":
			if stringValue(event["evaluated_permission"]) == "ask" {
				kind = mango.ActionConfirmation
			} else {
				kind = mango.ActionToolResult
			}
		default:
			continue
		}
		actions = append(actions, mango.Action{
			ID: id, ThreadID: stringValue(event["session_thread_id"]), Kind: kind,
			Name: first(stringValue(event["name"]), "tool"), Input: JSONText(event["input"]),
		})
	}
	return actions
}

func ContentText(value any) string {
	blocks, ok := value.([]any)
	if !ok {
		if typed, ok := value.([]map[string]any); ok {
			blocks = make([]any, len(typed))
			for index := range typed {
				blocks[index] = typed[index]
			}
		} else {
			return stringValue(value)
		}
	}
	parts := make([]string, 0, len(blocks))
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch stringValue(block["type"]) {
		case "text":
			if text := stringValue(block["text"]); text != "" {
				parts = append(parts, text)
			}
		case "image":
			parts = append(parts, "[image]")
		case "document":
			parts = append(parts, "[document]")
		case "search_result":
			parts = append(parts, first(stringValue(block["title"]), "[search result]"))
		}
	}
	return strings.Join(parts, "\n")
}

func JSONText(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func toolStatus(event mango.Event, barrier map[string]bool) string {
	if barrier[event.ID()] {
		return "waiting"
	}
	if stringValue(event["evaluated_permission"]) == "ask" {
		return "resolved"
	}
	return "running"
}

func latestBarrier(events []mango.Event) map[string]bool {
	required := map[string]bool{}
	for _, event := range events {
		switch event.Type() {
		case "session.status_idle", "session.thread_status_idle":
			stop, _ := event["stop_reason"].(map[string]any)
			required = map[string]bool{}
			if stringValue(stop["type"]) == "requires_action" {
				for _, id := range stringSlice(stop["event_ids"]) {
					required[id] = true
				}
			}
		}
	}
	return required
}

func eventTime(event mango.Event) time.Time {
	value := stringValue(event["processed_at"])
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

func stringSlice(value any) []string {
	switch values := value.(type) {
	case []string:
		return values
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text := stringValue(value); text != "" {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func stringValue(value any) string { text, _ := value.(string); return text }
func boolValue(value any) bool     { boolean, _ := value.(bool); return boolean }
func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
