package httpapi

import (
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestDeploymentToJSONRedactsInternalInitialEventFields(t *testing.T) {
	item := domain.Deployment{InitialEvents: []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "public"}},
			"id":      "forged_event_id", "type": "forged.type",
			"processed_at":                      "forged timestamp",
			domain.InternalOutcomeRubricContent: "private rubric",
			domain.InternalFileMessageContents: map[string]any{
				"0": map[string]any{"content": "private File content"},
			},
		},
	}}}

	wire := deploymentToJSON(item)
	events, ok := wire["initial_events"].([]any)
	if !ok || len(events) != 1 {
		t.Fatalf("initial_events = %#v", wire["initial_events"])
	}
	event, ok := events[0].(map[string]any)
	if !ok || event["type"] != domain.EvUserMessage || event["content"] == nil {
		t.Fatalf("public initial event = %#v", events[0])
	}
	for _, key := range []string{
		"id", "processed_at", domain.InternalOutcomeRubricContent,
		domain.InternalFileMessageContents,
	} {
		if _, present := event[key]; present {
			t.Fatalf("private or forged field %q leaked: %#v", key, event)
		}
	}
}
