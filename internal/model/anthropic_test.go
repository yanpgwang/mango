package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestAnthropic_SendsMessagesAndParsesResponse(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotVersion, gotBeta string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %s, want /v1/messages", r.URL.Path)
		}
		gotAuth = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotBeta = r.Header.Get("anthropic-beta")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"hi back"}],"stop_reason":"end_turn","usage":{"cache_creation":{"ephemeral_1h_input_tokens":3,"ephemeral_5m_input_tokens":4},"cache_read_input_tokens":5,"input_tokens":11,"output_tokens":7,"server_tool_use":{"web_fetch_requests":2,"web_search_requests":1},"speed":"standard","inference_geo":"us"}}`))
	}))
	defer srv.Close()

	c, err := NewAnthropic(AnthropicConfig{
		BaseURL: srv.URL, APIKey: "sk-test", Model: "claude-x", HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.CreateMessage(context.Background(), Request{
		Model:    "claude-x",
		Effort:   "max",
		Speed:    "fast",
		System:   "sys",
		Messages: []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "hi"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "sk-test" {
		t.Errorf("x-api-key = %q, want sk-test", gotAuth)
	}
	if gotVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", gotVersion)
	}
	if gotBeta != "fast-mode-2026-02-01" {
		t.Errorf("anthropic-beta = %q, want fast mode beta", gotBeta)
	}
	if gotBody["model"] != "claude-x" || gotBody["system"] != "sys" {
		t.Errorf("body model/system = %v/%v", gotBody["model"], gotBody["system"])
	}
	if gotBody["speed"] != "fast" {
		t.Errorf("body speed = %v, want fast", gotBody["speed"])
	}
	if _, present := gotBody["inference_geo"]; present {
		t.Errorf("provider-specific inference_geo leaked into request: %#v", gotBody)
	}
	outputConfig, ok := gotBody["output_config"].(map[string]any)
	if !ok || outputConfig["effort"] != "max" {
		t.Errorf("body output_config = %#v, want effort=max", gotBody["output_config"])
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "hi back" || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %#v", resp)
	}
	if resp.Usage.InputTokens != 11 || resp.Usage.OutputTokens != 7 ||
		resp.Usage.CacheReadInputTokens != 5 ||
		resp.Usage.CacheCreation.Ephemeral1hInputTokens != 3 ||
		resp.Usage.CacheCreation.Ephemeral5mInputTokens != 4 ||
		resp.Usage.ServerToolUse.WebFetchRequests != 2 ||
		resp.Usage.ServerToolUse.WebSearchRequests != 1 ||
		resp.Usage.Speed != "standard" || resp.Usage.ProviderRegion != "us" {
		t.Fatalf("usage = %#v", resp.Usage)
	}
}

func TestAnthropic_OrdinaryAdvisorToolUsesPortableWireContract(t *testing.T) {
	var gotBody map[string]any
	var gotBetas []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBetas = append([]string(nil), r.Header.Values("anthropic-beta")...)
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"content":[{"type":"tool_use","id":"tool_advisor","name":"advisor","input":{}}],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":11,"output_tokens":7}
		}`))
	}))
	defer srv.Close()

	client, err := NewAnthropic(AnthropicConfig{
		BaseURL: srv.URL, APIKey: "sk-test", Model: "claude-sonnet-5",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.CreateMessage(context.Background(), Request{
		Model: "claude-sonnet-5",
		Tools: []ToolSchema{{
			Name: "advisor", Description: "Request an independent review.",
			InputSchema: map[string]any{
				"type": "object", "properties": map[string]any{},
			},
		}},
		Messages: []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{
			Type: "text", Text: "review this",
		}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(gotBetas, ","), "advisor-tool") {
		t.Fatalf("ordinary advisor tool leaked native beta header: %#v", gotBetas)
	}
	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("advisor tools body = %#v", gotBody["tools"])
	}
	tool := tools[0].(map[string]any)
	if _, present := tool["type"]; present || tool["name"] != "advisor" ||
		tool["description"] != "Request an independent review." {
		t.Fatalf("advisor tool = %#v", tool)
	}
	if response.Usage.InputTokens != 11 || response.Usage.OutputTokens != 7 {
		t.Fatalf("top-level usage = %#v", response.Usage)
	}
	if len(response.Content) != 1 || response.Content[0].Type != "tool_use" ||
		response.Content[0].ToolName != "advisor" {
		t.Fatalf("ordinary advisor tool response = %#v", response.Content)
	}
}

func TestAnthropic_OmitsSemanticModelDefaults(t *testing.T) {
	c, err := NewAnthropic(AnthropicConfig{
		BaseURL: "https://example.com", APIKey: "sk-test", Model: "claude-x",
	})
	if err != nil {
		t.Fatal(err)
	}
	body, err := c.buildWireRequest(Request{
		Model: "claude-x", Effort: "high", Speed: "standard",
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if _, present := wire["output_config"]; present {
		t.Fatalf("default effort must be omitted for compatible endpoints: %s", encoded)
	}
	if _, present := wire["speed"]; present {
		t.Fatalf("default speed must be omitted for compatible endpoints: %s", encoded)
	}
	if _, present := wire["inference_geo"]; present {
		t.Fatalf("provider-specific inference_geo must not be configurable: %s", encoded)
	}
}

func TestDecodeMessageStream_AccumulatesUsage(t *testing.T) {
	stream := strings.NewReader(
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"cache_creation\":{\"ephemeral_1h_input_tokens\":2,\"ephemeral_5m_input_tokens\":3},\"cache_read_input_tokens\":4,\"input_tokens\":10,\"output_tokens\":1,\"inference_geo\":\"us\"}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"input_tokens\":10,\"output_tokens\":6,\"server_tool_use\":{\"web_fetch_requests\":2,\"web_search_requests\":1}}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n",
	)
	resp, err := decodeMessageStream(stream, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 6 ||
		resp.Usage.CacheReadInputTokens != 4 ||
		resp.Usage.CacheCreation.Ephemeral1hInputTokens != 2 ||
		resp.Usage.CacheCreation.Ephemeral5mInputTokens != 3 ||
		resp.Usage.ServerToolUse.WebFetchRequests != 2 ||
		resp.Usage.ServerToolUse.WebSearchRequests != 1 ||
		resp.Usage.ProviderRegion != "us" {
		t.Fatalf("stream usage = %#v", resp.Usage)
	}
}

func TestDecodeMessageStream_PreservesThinkingContinuationBlocks(t *testing.T) {
	stream := strings.NewReader(
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\",\"signature\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"private reasoning\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig_123\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"redacted_thinking\",\"data\":\"opaque_456\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n",
	)
	thinkingStarts := 0
	response, err := decodeMessageStreamWithCallbacks(stream, StreamCallbacks{
		OnThinkingStart: func() { thinkingStarts++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Content) != 2 {
		t.Fatalf("content blocks = %d, want 2", len(response.Content))
	}
	if thinkingStarts != 1 {
		t.Fatalf("thinking starts = %d, want one privacy-safe signal", thinkingStarts)
	}
	var thinking map[string]any
	if err := json.Unmarshal(response.Content[0].Raw, &thinking); err != nil {
		t.Fatal(err)
	}
	if thinking["type"] != "thinking" || thinking["thinking"] != "private reasoning" ||
		thinking["signature"] != "sig_123" {
		t.Fatalf("thinking block = %#v", thinking)
	}
	var redacted map[string]any
	if err := json.Unmarshal(response.Content[1].Raw, &redacted); err != nil {
		t.Fatal(err)
	}
	if redacted["type"] != "redacted_thinking" || redacted["data"] != "opaque_456" {
		t.Fatalf("redacted thinking block = %#v", redacted)
	}
}

func TestAnthropic_InvalidTypedBlockFailsBeforeHTTPRequest(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer srv.Close()
	c, err := NewAnthropic(AnthropicConfig{
		BaseURL: srv.URL, APIKey: "sk-test", Model: "m", HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateMessage(context.Background(), Request{
		Messages: []domain.Message{{
			Role: domain.RoleAssistant,
			Content: []domain.ContentBlock{{
				Type: "tool_use",
				Input: map[string]any{
					"invalid": func() {},
				},
			}},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "encode tool_use content block") {
		t.Fatalf("expected typed block encoding error, got %v", err)
	}
	if requests != 0 {
		t.Fatalf("sent %d requests after local encoding failure", requests)
	}
}

func TestAnthropic_BearerAuthAndErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want Bearer sk-test", got)
		}
		w.Header().Set("request-id", "req_rate_limit")
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"rate limited"}}`))
	}))
	defer srv.Close()
	c, _ := NewAnthropic(AnthropicConfig{
		BaseURL: srv.URL, APIKey: "sk-test", Model: "m",
		AuthHeader: "authorization-bearer", HTTPClient: srv.Client(),
	})
	_, err := c.CreateMessage(context.Background(), Request{Messages: []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "x"}}}}})
	if err == nil {
		t.Fatal("expected error on 429 status")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("error should include upstream message, got %q", err.Error())
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != ErrorRateLimit ||
		!apiErr.Retryable() || apiErr.RetryAfter != 3*time.Second ||
		apiErr.RequestID != "req_rate_limit" {
		t.Fatalf("typed error = %#v", err)
	}
}

func TestAnthropic_StreamErrorStatusIsTyped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":{"type":"request_too_large","message":"request exceeds limit"}}`))
	}))
	defer srv.Close()
	c, _ := NewAnthropic(AnthropicConfig{
		BaseURL: srv.URL, APIKey: "sk-test", Model: "m", HTTPClient: srv.Client(),
	})

	_, err := c.CreateMessageStream(context.Background(), Request{}, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Kind != ErrorRequestTooLarge || apiErr.Retryable() {
		t.Fatalf("typed stream error = %#v", err)
	}
}

// A non-2xx response must surface the upstream error text so an operator can
// see the cause (rate limit, auth, bad request). Both JSON error envelopes and
// plain-text bodies are handled, sanitized, and length-bounded.
func TestAnthropic_NonJSONErrorBodyIsSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
		_, _ = w.Write([]byte("upstream\nboom: bad gateway"))
	}))
	defer srv.Close()
	c, _ := NewAnthropic(AnthropicConfig{
		BaseURL: srv.URL, APIKey: "sk-test", Model: "m", HTTPClient: srv.Client(),
	})
	_, err := c.CreateMessage(context.Background(), Request{Messages: []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "x"}}}}})
	if err == nil {
		t.Fatal("expected error on 502 status")
	}
	msg := err.Error()
	if !strings.Contains(msg, "bad gateway") {
		t.Fatalf("error should include upstream body text, got %q", msg)
	}
	if strings.Contains(msg, "\n") {
		t.Fatalf("error must not contain newlines, got %q", msg)
	}
	if !strings.Contains(msg, "502") {
		t.Fatalf("error should include status code, got %q", msg)
	}
}

// A request carrying Tools plus multi-turn tool history (an assistant tool_use
// block followed by a user tool_result block) must serialize to the official
// Anthropic wire shape: top-level tools[] with name/description/input_schema,
// and message content blocks {type:tool_use,id,name,input} and
// {type:tool_result,tool_use_id,content:[{type:text,text}]}.
func TestAnthropic_SerializesToolsAndToolBlocks(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	c, err := NewAnthropic(AnthropicConfig{
		BaseURL: srv.URL, APIKey: "sk-test", Model: "m", HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.CreateMessage(context.Background(), Request{
		Tools: []ToolSchema{{
			Name:        "get_weather",
			Description: "Get the weather",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}},
		Messages: []domain.Message{
			{Role: domain.RoleAssistant, Content: []domain.ContentBlock{
				{Type: "tool_use", ToolUseID: "tu_1", ToolName: "get_weather", Input: map[string]any{"city": "SF"}},
			}},
			{Role: domain.RoleUser, Content: []domain.ContentBlock{
				{Type: "tool_result", ToolResultFor: "tu_1", Text: "72F", IsError: false},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v, want 1 element", gotBody["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "get_weather" {
		t.Errorf("tools[0].name = %v, want get_weather", tool["name"])
	}
	if tool["description"] != "Get the weather" {
		t.Errorf("tools[0].description = %v", tool["description"])
	}
	if _, ok := tool["input_schema"].(map[string]any); !ok {
		t.Errorf("tools[0].input_schema missing/wrong type: %#v", tool["input_schema"])
	}

	msgs := gotBody["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len = %d, want 2", len(msgs))
	}
	// assistant tool_use block
	asst := msgs[0].(map[string]any)
	tu := asst["content"].([]any)[0].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "tu_1" || tu["name"] != "get_weather" {
		t.Errorf("tool_use block = %#v", tu)
	}
	input, ok := tu["input"].(map[string]any)
	if !ok || input["city"] != "SF" {
		t.Errorf("tool_use.input = %#v", tu["input"])
	}
	if _, present := tu["text"]; present {
		t.Errorf("tool_use block should not carry a text field: %#v", tu)
	}
	// user tool_result block
	usr := msgs[1].(map[string]any)
	tr := usr["content"].([]any)[0].(map[string]any)
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "tu_1" {
		t.Errorf("tool_result block = %#v", tr)
	}
	content, ok := tr["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("tool_result.content = %#v, want 1 text block", tr["content"])
	}
	cb := content[0].(map[string]any)
	if cb["type"] != "text" || cb["text"] != "72F" {
		t.Errorf("tool_result.content[0] = %#v", cb)
	}
}

func TestAnthropic_SerializesEmptyToolUseInputAsObject(t *testing.T) {
	for _, input := range []map[string]any{nil, {}} {
		raw, err := marshalTypedBlock(domain.ContentBlock{
			Type: "tool_use", ToolUseID: "toolu_advisor",
			ToolName: "advisor", Input: input,
		})
		if err != nil {
			t.Fatal(err)
		}
		var block map[string]any
		if err := json.Unmarshal(raw, &block); err != nil {
			t.Fatal(err)
		}
		value, present := block["input"]
		object, objectOK := value.(map[string]any)
		if !present || !objectOK || len(object) != 0 {
			t.Fatalf("tool_use.input = %#v (present=%v), want required empty object; raw=%s", value, present, raw)
		}
	}
}

func TestAnthropic_SerializesRichToolResultContent(t *testing.T) {
	image := json.RawMessage(`{"type":"image","source":{"type":"url","url":"https://example.com/result.png"}}`)
	raw, err := marshalTypedBlock(domain.ContentBlock{
		Type:          "tool_result",
		ToolResultFor: "toolu_1",
		ResultContent: []json.RawMessage{
			json.RawMessage(`{"type":"text","text":"caption"}`),
			image,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var block map[string]any
	if err := json.Unmarshal(raw, &block); err != nil {
		t.Fatal(err)
	}
	content, ok := block["content"].([]any)
	if !ok || len(content) != 2 {
		t.Fatalf("content = %#v", block["content"])
	}
	if content[1].(map[string]any)["type"] != "image" {
		t.Fatalf("rich content = %#v", content)
	}
}

// A response containing a tool_use block must parse into a domain tool_use
// ContentBlock with ToolUseID, ToolName, and Input populated.
func TestAnthropic_ParsesToolUseResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"tool_use","id":"tu_9","name":"get_weather","input":{"city":"LA"}}],"stop_reason":"tool_use"}`))
	}))
	defer srv.Close()
	c, _ := NewAnthropic(AnthropicConfig{
		BaseURL: srv.URL, APIKey: "sk-test", Model: "m", HTTPClient: srv.Client(),
	})
	resp, err := c.CreateMessage(context.Background(), Request{
		Messages: []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "weather?"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("content len = %d, want 1", len(resp.Content))
	}
	b := resp.Content[0]
	if b.Type != "tool_use" || b.ToolUseID != "tu_9" || b.ToolName != "get_weather" {
		t.Errorf("parsed block = %#v", b)
	}
	if b.Input["city"] != "LA" {
		t.Errorf("parsed input = %#v", b.Input)
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %q", resp.StopReason)
	}
}

func TestAnthropic_NativeWebToolsPreserveOpaqueBlocksForReplay(t *testing.T) {
	var requests []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		requests = append(requests, body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"content": [
				{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"today"}},
				{"type":"web_search_tool_result","tool_use_id":"srv_1","content":[{"type":"web_search_result","url":"https://example.com","title":"Example","encrypted_content":"opaque-token"}]},
				{"type":"text","text":"answer","citations":[{"type":"web_search_result_location","url":"https://example.com","cited_text":"source"}]}
			],
			"stop_reason":"end_turn"
		}`)
	}))
	defer srv.Close()

	c, _ := NewAnthropic(AnthropicConfig{
		BaseURL: srv.URL, APIKey: "sk-test", Model: "m", HTTPClient: srv.Client(),
	})
	req := Request{
		Tools: []ToolSchema{
			{Type: "web_search_20260318", Name: "web_search"},
			{Type: "web_fetch_20260318", Name: "web_fetch"},
		},
		Messages: []domain.Message{{
			Role: domain.RoleUser,
			Content: []domain.ContentBlock{{
				Type: "text", Text: "what happened today?",
			}},
		}},
	}
	resp, err := c.CreateMessageStream(context.Background(), req, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 3 || resp.Content[1].Type != "web_search_tool_result" {
		t.Fatalf("response content = %#v", resp.Content)
	}

	req.Messages = append(req.Messages, domain.Message{
		Role: domain.RoleAssistant, Content: resp.Content,
	})
	req.Messages = append(req.Messages, domain.Message{
		Role:    domain.RoleUser,
		Content: []domain.ContentBlock{{Type: "text", Text: "continue"}},
	})
	if _, err := c.CreateMessage(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(requests))
	}
	tools := requests[0]["tools"].([]any)
	search := tools[0].(map[string]any)
	if search["type"] != "web_search_20260318" || search["name"] != "web_search" {
		t.Fatalf("native search declaration = %#v", search)
	}
	if _, exists := search["input_schema"]; exists {
		t.Fatalf("native search must not be encoded as a client tool: %#v", search)
	}
	replayed := requests[1]["messages"].([]any)[1].(map[string]any)["content"].([]any)
	searchResult := replayed[1].(map[string]any)
	nested := searchResult["content"].([]any)[0].(map[string]any)
	if nested["encrypted_content"] != "opaque-token" {
		t.Fatalf("opaque server-tool content was lost: %#v", searchResult)
	}
	text := replayed[2].(map[string]any)
	if len(text["citations"].([]any)) != 1 {
		t.Fatalf("citations were lost: %#v", text)
	}
}

// An upstream error body long enough to hit the truncation limit, with a
// multibyte rune straddling the byte boundary, must be truncated on a rune
// boundary so the returned error string is valid UTF-8.
func TestAnthropic_ErrorTruncationIsRuneSafe(t *testing.T) {
	// Build a body of multibyte runes so the byte-length limit falls in the
	// middle of a rune. '世' is 3 bytes; 400 of them = 1200 bytes > 512.
	body := strings.Repeat("世", 400)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c, _ := NewAnthropic(AnthropicConfig{
		BaseURL: srv.URL, APIKey: "sk-test", Model: "m", HTTPClient: srv.Client(),
	})
	_, err := c.CreateMessage(context.Background(), Request{
		Messages: []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "x"}}}},
	})
	if err == nil {
		t.Fatal("expected error on 500 status")
	}
	if !utf8.ValidString(err.Error()) {
		t.Fatalf("error string is not valid UTF-8: %q", err.Error())
	}
}

func TestAnthropic_CreateMessageStream_DecodesSSE(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// minimal canned Messages API stream: one text content block in two deltas
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
		io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n")
		io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()
	c, _ := NewAnthropic(AnthropicConfig{BaseURL: srv.URL, APIKey: "sk-test", Model: "m", HTTPClient: srv.Client()})
	var got []string
	resp, err := c.CreateMessageStream(context.Background(), Request{
		Messages: []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "hi"}}}},
	}, func(index int, text string) { got = append(got, text) })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "") != "Hello" {
		t.Fatalf("deltas = %v, want join 'Hello'", got)
	}
	if len(resp.Content) != 1 || resp.Content[0].Text != "Hello" || resp.StopReason != "end_turn" {
		t.Fatalf("resp = %#v", resp)
	}
}

func TestNewAnthropic_RequiresBaseURLAndKey(t *testing.T) {
	if _, err := NewAnthropic(AnthropicConfig{APIKey: "k"}); err == nil {
		t.Error("expected error for missing base URL")
	}
	if _, err := NewAnthropic(AnthropicConfig{BaseURL: "http://x"}); err == nil {
		t.Error("expected error for missing key")
	}
}
