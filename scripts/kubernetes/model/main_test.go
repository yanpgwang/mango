package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

func TestFixtureTranscriptActionsAndResults(t *testing.T) {
	for _, tc := range []struct {
		instruction, tool, id string
	}{
		{"alpha:write", "bash", "provider_write"},
		{"alpha:read", "bash", "provider_read"},
		{"alpha:custom", "checkpoint", "provider_custom"},
	} {
		t.Run(tc.instruction, func(t *testing.T) {
			input := map[string]any{"messages": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": tc.instruction}}}}}
			post := func() *httptest.ResponseRecorder {
				data, err := json.Marshal(input)
				if err != nil {
					t.Fatal(err)
				}
				out := httptest.NewRecorder()
				handler().ServeHTTP(out, httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(data)))
				return out
			}
			out := post()
			if out.Code != 200 {
				t.Fatalf("initial status = %d", out.Code)
			}
			var got struct {
				Content []map[string]any
				Stop    string `json:"stop_reason"`
			}
			if err := json.Unmarshal(out.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if len(got.Content) != 1 || got.Stop != "tool_use" || got.Content[0]["name"] != tc.tool || got.Content[0]["id"] != tc.id {
				t.Fatalf("initial response: %s", out.Body.String())
			}
			result := map[string]any{"type": "tool_result", "tool_use_id": tc.id, "content": []any{map[string]any{"type": "text", "text": "alpha-proof alpha-skill-bundle alpha-memory"}}}
			input["messages"] = append(input["messages"].([]any), map[string]any{"role": "user", "content": []any{result}})
			out = post()
			if out.Code != 200 || !strings.Contains(out.Body.String(), "\"stop_reason\":\"end_turn\"") {
				t.Fatalf("resume: %d %s", out.Code, out.Body.String())
			}
			result["is_error"] = true
			if out = post(); out.Code != 422 {
				t.Fatalf("erroneous tool result accepted: %d", out.Code)
			}
		})
	}
}

func TestFixtureSSEUsesActualMessagesAdapter(t *testing.T) {
	server := httptest.NewServer(handler())
	defer server.Close()
	client, err := model.NewAnthropic(model.AnthropicConfig{BaseURL: server.URL, APIKey: "fixture-only", Model: "alpha-fixture", HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.CreateMessageStream(t.Context(), model.Request{Messages: []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "alpha:custom"}}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if response.StopReason != "tool_use" || len(response.Content) != 1 || response.Content[0].ToolUseID != "provider_custom" || response.Content[0].ToolName != "checkpoint" || response.Content[0].Input["label"] != "alpha-proof" {
		t.Fatalf("streamed response: %#v", response)
	}
}
