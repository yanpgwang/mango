package model

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yanpgwang/mango/internal/domain"
)

var _ Client = (*Anthropic)(nil)

const anthropicVersion = "2023-06-01"
const defaultMaxTokens = 4096

// AnthropicConfig configures the real Messages-API client. Everything comes
// from the caller (env in production); no value is hardcoded and no credential
// is ever compiled in or logged.
type AnthropicConfig struct {
	BaseURL    string // e.g. https://api.anthropic.com
	APIKey     string
	Model      string
	AuthHeader string // "x-api-key" (default) or "authorization-bearer"
	HTTPClient *http.Client
}

type Anthropic struct {
	cfg  AnthropicConfig
	http *http.Client
}

func NewAnthropic(cfg AnthropicConfig) (*Anthropic, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, errors.New("model: anthropic base URL is required")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("model: anthropic API key is required")
	}
	if cfg.AuthHeader == "" {
		cfg.AuthHeader = "x-api-key"
	}
	hc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 120 * time.Second}
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Anthropic{cfg: cfg, http: hc}, nil
}

// AnthropicFromEnv builds a client from environment variables. It returns
// (nil, false, nil) when the base URL or key is unset, so the caller falls back
// to the offline fake without error.
func AnthropicFromEnv() (*Anthropic, bool, error) {
	base := os.Getenv("MANGO_MODEL_BASE_URL")
	key := os.Getenv("MANGO_MODEL_API_KEY")
	if base == "" || key == "" {
		return nil, false, nil
	}
	auth := os.Getenv("MANGO_MODEL_AUTH")
	c, err := NewAnthropic(AnthropicConfig{
		BaseURL:    base,
		APIKey:     key,
		Model:      os.Getenv("MANGO_MODEL_ID"),
		AuthHeader: auth,
	})
	if err != nil {
		return nil, false, err
	}
	return c, true, nil
}

// wireBlock is one content block in the Anthropic Messages wire format. A block
// is a tagged union keyed on Type; only the fields relevant to that type are
// emitted. Input is a pointer because Messages requires tool_use.input even for
// an empty object, while non-tool blocks must omit it.
type wireBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input *map[string]any `json:"input,omitempty"`
	// tool_result
	ToolUseID string            `json:"tool_use_id,omitempty"`
	Content   []json.RawMessage `json:"content,omitempty"`
	IsError   bool              `json:"is_error,omitempty"`
}
type wireMessage struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}
type wireTool struct {
	Type        string         `json:"type,omitempty"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema,omitempty"`
}
type wireRequest struct {
	Model        string            `json:"model"`
	Speed        string            `json:"speed,omitempty"`
	OutputConfig *wireOutputConfig `json:"output_config,omitempty"`
	System       string            `json:"system,omitempty"`
	MaxTokens    int               `json:"max_tokens"`
	Messages     []wireMessage     `json:"messages"`
	Tools        []wireTool        `json:"tools,omitempty"`
	Stream       bool              `json:"stream,omitempty"`
}
type wireOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}
type wireCacheCreationUsage struct {
	Ephemeral1hInputTokens int64 `json:"ephemeral_1h_input_tokens"`
	Ephemeral5mInputTokens int64 `json:"ephemeral_5m_input_tokens"`
}
type wireUsage struct {
	CacheCreation        wireCacheCreationUsage `json:"cache_creation"`
	CacheReadInputTokens int64                  `json:"cache_read_input_tokens"`
	InputTokens          int64                  `json:"input_tokens"`
	OutputTokens         int64                  `json:"output_tokens"`
	ServerToolUse        wireServerToolUsage    `json:"server_tool_use"`
	Speed                string                 `json:"speed"`
	InferenceGeo         string                 `json:"inference_geo"`
}
type wireServerToolUsage struct {
	WebFetchRequests  int64 `json:"web_fetch_requests"`
	WebSearchRequests int64 `json:"web_search_requests"`
}
type wireCacheCreationUsagePatch struct {
	Ephemeral1hInputTokens *int64 `json:"ephemeral_1h_input_tokens"`
	Ephemeral5mInputTokens *int64 `json:"ephemeral_5m_input_tokens"`
}
type wireServerToolUsagePatch struct {
	WebFetchRequests  *int64 `json:"web_fetch_requests"`
	WebSearchRequests *int64 `json:"web_search_requests"`
}
type wireUsagePatch struct {
	CacheCreation        *wireCacheCreationUsagePatch `json:"cache_creation"`
	CacheReadInputTokens *int64                       `json:"cache_read_input_tokens"`
	InputTokens          *int64                       `json:"input_tokens"`
	OutputTokens         *int64                       `json:"output_tokens"`
	ServerToolUse        *wireServerToolUsagePatch    `json:"server_tool_use"`
	Speed                *string                      `json:"speed"`
	InferenceGeo         *string                      `json:"inference_geo"`
}
type wireResponse struct {
	Content    []json.RawMessage `json:"content"`
	StopReason string            `json:"stop_reason"`
	Usage      wireUsage         `json:"usage"`
}

// buildWireRequest maps a domain Request to the Anthropic Messages wire request,
// applying config defaults for model and max_tokens. stream toggles the
// server-sent-events variant ("stream": true).
func (a *Anthropic) buildWireRequest(req Request, stream bool) (wireRequest, error) {
	model := req.Model
	if model == "" {
		model = a.cfg.Model
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	body := wireRequest{
		Model: model, System: req.System,
		MaxTokens: maxTokens, Stream: stream,
	}
	// high and standard are the Managed Agents and Messages defaults. Omitting
	// those exact defaults preserves their semantics while keeping the adapter
	// compatible with older Claude-compatible endpoints (including Bedrock
	// gateways) that reject the preview request fields entirely. Non-default
	// values must be forwarded so an unsupported endpoint fails explicitly
	// instead of silently ignoring the Agent configuration.
	if req.Effort != "" && req.Effort != domain.DefaultModelEffort {
		body.OutputConfig = &wireOutputConfig{Effort: req.Effort}
	}
	if req.Speed == "fast" {
		body.Speed = req.Speed
	}
	for _, t := range req.Tools {
		body.Tools = append(body.Tools, wireTool{
			Type:        t.Type,
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: string(m.Role)}
		for _, b := range m.Content {
			raw, err := toWireBlock(b)
			if err != nil {
				return wireRequest{}, fmt.Errorf(
					"model: encode %s content block: %w",
					b.Type,
					err,
				)
			}
			wm.Content = append(wm.Content, raw)
		}
		body.Messages = append(body.Messages, wm)
	}
	return body, nil
}

// newHTTPRequest marshals the wire body and builds the POST /v1/messages request
// with version and auth headers set. No credential is ever logged.
func (a *Anthropic) newHTTPRequest(ctx context.Context, body wireRequest) (*http.Request, error) {
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+"/v1/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", anthropicVersion)
	if body.Speed == "fast" {
		// Fast mode remains a Messages API research preview. Managed Agents hides
		// this provider detail, but the self-hosted adapter must opt in when it
		// forwards the resolved Agent speed to Anthropic directly.
		httpReq.Header.Set("anthropic-beta", "fast-mode-2026-02-01")
	}
	if a.cfg.AuthHeader == "authorization-bearer" {
		httpReq.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	} else {
		httpReq.Header.Set("x-api-key", a.cfg.APIKey)
	}
	return httpReq, nil
}

func (a *Anthropic) CreateMessage(ctx context.Context, req Request) (Response, error) {
	body, err := a.buildWireRequest(req, false)
	if err != nil {
		return Response{}, err
	}
	httpReq, err := a.newHTTPRequest(ctx, body)
	if err != nil {
		return Response{}, err
	}

	resp, err := a.http.Do(httpReq)
	if err != nil {
		return Response{}, classifyRequestError(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Response{}, classifyHTTPError(resp.StatusCode, raw, resp.Header)
	}
	var wr wireResponse
	if err := json.Unmarshal(raw, &wr); err != nil {
		return Response{}, fmt.Errorf("model: decode response: %w", err)
	}
	out := Response{StopReason: wr.StopReason, Usage: usageFromWire(wr.Usage)}
	for _, raw := range wr.Content {
		cb, err := parseProviderBlock(raw)
		if err != nil {
			return Response{}, fmt.Errorf("model: decode content block: %w", err)
		}
		out.Content = append(out.Content, cb)
	}
	return out, nil
}

// CreateMessageStream opens the Messages-API server-sent-events stream
// ("stream": true) and decodes it incrementally. It scans the SSE body line by
// line, parses each `data:` payload as JSON, and dispatches on the top-level
// `type`:
//
//   - content_block_start   — opens a block at `index`; for tool_use it captures
//     id/name so the block can be assembled from later deltas.
//   - content_block_delta    — for a text_delta it accumulates text and calls
//     onDelta(index, text) per chunk; for an input_json_delta it accumulates the
//     tool_use partial_json (no onDelta — tool input is not previewed).
//   - content_block_stop     — finalizes a block; a tool_use block's accumulated
//     partial_json is parsed into its input map here.
//   - message_delta          — captures stop_reason.
//   - message_stop           — end of stream.
//
// It assembles and returns the final Response (content blocks in index order +
// stop_reason). A non-2xx status is handled exactly as the non-streaming path:
// the sanitized, length-bounded upstream body is folded into the returned error.
// The request context threads through the HTTP read, so a cancelled context
// aborts the stream.
//
// tool_use streaming coverage: text is fully incremental; tool_use blocks are
// assembled from content_block_start + accumulated input_json_delta and parsed
// at content_block_stop. This is enough for the tool loop (which needs the
// complete tool call), but partial_json is not surfaced incrementally.
func (a *Anthropic) CreateMessageStream(
	ctx context.Context,
	req Request,
	onDelta func(index int, text string),
) (Response, error) {
	return a.CreateMessageStreamWithCallbacks(ctx, req, StreamCallbacks{OnTextDelta: onDelta})
}

// CreateMessageStreamWithCallbacks adds privacy-safe lifecycle signals to the
// basic streaming client. In particular, thinking is start-only: no reasoning
// bytes cross this callback boundary.
func (a *Anthropic) CreateMessageStreamWithCallbacks(
	ctx context.Context,
	req Request,
	callbacks StreamCallbacks,
) (Response, error) {
	// Server-tool responses contain provider-private blocks and citation
	// structures whose streaming deltas evolve independently of the client-tool
	// wire shape. Until the streaming assembler can losslessly retain every
	// unknown delta, use the non-streaming endpoint whenever a native tool is
	// enabled. This preserves the exact response block while still publishing
	// complete text blocks as best-effort preview deltas.
	if hasNativeTool(req.Tools) {
		resp, err := a.CreateMessage(ctx, req)
		if err != nil {
			return Response{}, err
		}
		thinkingStarted := false
		for _, block := range resp.Content {
			if !thinkingStarted && (block.Type == "thinking" || block.Type == "redacted_thinking") {
				if callbacks.OnThinkingStart != nil {
					callbacks.OnThinkingStart()
				}
				thinkingStarted = true
			}
		}
		if callbacks.OnTextDelta != nil {
			for index, block := range resp.Content {
				if block.Type == "text" && block.Text != "" {
					callbacks.OnTextDelta(index, block.Text)
				}
			}
		}
		return resp, nil
	}

	body, err := a.buildWireRequest(req, true)
	if err != nil {
		return Response{}, err
	}
	httpReq, err := a.newHTTPRequest(ctx, body)
	if err != nil {
		return Response{}, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")

	resp, err := a.http.Do(httpReq)
	if err != nil {
		return Response{}, classifyRequestError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return Response{}, classifyHTTPError(resp.StatusCode, raw, resp.Header)
	}
	return decodeMessageStreamWithCallbacks(resp.Body, callbacks)
}

// streamBlock accumulates one content block as its deltas arrive over the SSE
// stream. text blocks fill text; tool_use blocks fill id/name and buffer the
// tool input as partial_json fragments until the block stops.
type streamBlock struct {
	typ         string
	text        strings.Builder
	thinking    strings.Builder
	signature   strings.Builder
	data        string
	toolID      string
	toolName    string
	toolInput   map[string]any
	partialJSON strings.Builder
}

// sseEvent is the union of Messages-API streaming event fields we read. Only the
// fields relevant to a given `type` are populated; the rest stay zero.
type sseEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock struct {
		Type      string         `json:"type"`
		Text      string         `json:"text"`
		Thinking  string         `json:"thinking"`
		Signature string         `json:"signature"`
		Data      string         `json:"data"`
		ID        string         `json:"id"`
		Name      string         `json:"name"`
		Input     map[string]any `json:"input"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Message struct {
		Usage wireUsage `json:"usage"`
	} `json:"message"`
	Usage wireUsagePatch `json:"usage"`
}

// decodeMessageStream reads an Anthropic Messages-API SSE body and assembles the
// final Response, invoking onDelta for each text_delta.
func decodeMessageStream(body io.Reader, onDelta func(index int, text string)) (Response, error) {
	return decodeMessageStreamWithCallbacks(body, StreamCallbacks{OnTextDelta: onDelta})
}

func decodeMessageStreamWithCallbacks(body io.Reader, callbacks StreamCallbacks) (Response, error) {
	sc := bufio.NewScanner(body)
	// Allow long data: lines (a single tool_use input_json_delta or a large text
	// chunk can exceed the default 64 KiB token size).
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)

	blocks := map[int]*streamBlock{}
	var order []int
	stopReason := ""
	var usage wireUsage
	thinkingStarted := false

	finalize := func(idx int) {
		b := blocks[idx]
		if b == nil || b.typ != "tool_use" {
			return
		}
		if pj := b.partialJSON.String(); strings.TrimSpace(pj) != "" {
			var input map[string]any
			if err := json.Unmarshal([]byte(pj), &input); err == nil {
				b.toolInput = input
			}
		}
	}

	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			// event:, id:, retry:, comments, and blank separators are ignored;
			// dispatch is driven entirely by the JSON `type` on data: lines.
			continue
		}
		// Assumes one complete JSON payload per data: line, which is what the
		// Anthropic Messages API emits. The SSE spec permits an event's data to
		// span multiple data: lines (joined by "\n"); we do not reassemble those.
		// If the upstream ever splits a payload across lines, each fragment would
		// fail to parse — revisit with a per-event data-line accumulator then.
		data = strings.TrimSpace(data)
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev sseEvent
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return Response{}, fmt.Errorf("model: decode stream event: %w", err)
		}
		switch ev.Type {
		case "message_start":
			usage = ev.Message.Usage
		case "content_block_start":
			b := &streamBlock{typ: ev.ContentBlock.Type}
			if b.typ == "" {
				b.typ = "text"
			}
			if b.typ == "text" {
				b.text.WriteString(ev.ContentBlock.Text)
			}
			if b.typ == "tool_use" {
				b.toolID = ev.ContentBlock.ID
				b.toolName = ev.ContentBlock.Name
				b.toolInput = ev.ContentBlock.Input
			}
			if b.typ == "thinking" {
				if !thinkingStarted && callbacks.OnThinkingStart != nil {
					callbacks.OnThinkingStart()
				}
				thinkingStarted = true
				b.thinking.WriteString(ev.ContentBlock.Thinking)
				b.signature.WriteString(ev.ContentBlock.Signature)
			}
			if b.typ == "redacted_thinking" {
				if !thinkingStarted && callbacks.OnThinkingStart != nil {
					callbacks.OnThinkingStart()
				}
				thinkingStarted = true
				b.data = ev.ContentBlock.Data
			}
			if _, seen := blocks[ev.Index]; !seen {
				order = append(order, ev.Index)
			}
			blocks[ev.Index] = b
		case "content_block_delta":
			b := blocks[ev.Index]
			if b == nil {
				b = &streamBlock{typ: "text"}
				blocks[ev.Index] = b
				order = append(order, ev.Index)
			}
			switch ev.Delta.Type {
			case "text_delta":
				b.text.WriteString(ev.Delta.Text)
				if callbacks.OnTextDelta != nil && ev.Delta.Text != "" {
					callbacks.OnTextDelta(ev.Index, ev.Delta.Text)
				}
			case "input_json_delta":
				b.partialJSON.WriteString(ev.Delta.PartialJSON)
			case "thinking_delta":
				b.thinking.WriteString(ev.Delta.Thinking)
			case "signature_delta":
				b.signature.WriteString(ev.Delta.Signature)
			}
		case "content_block_stop":
			finalize(ev.Index)
		case "message_delta":
			if ev.Delta.StopReason != "" {
				stopReason = ev.Delta.StopReason
			}
			// message_delta usage is a cumulative snapshot, but compatible
			// endpoints do not all include the same fields. Merge fields that
			// are actually present so final server-tool counts are captured
			// without erasing input/cache usage reported at message_start.
			applyWireUsagePatch(&usage, ev.Usage)
		case "message_stop":
			// End of stream; loop exits when the body is drained.
		}
	}
	if err := sc.Err(); err != nil {
		return Response{}, fmt.Errorf("model: read stream: %w", err)
	}
	// Finalize any tool_use block that never received an explicit stop.
	for _, idx := range order {
		finalize(idx)
	}

	out := Response{StopReason: stopReason, Usage: usageFromWire(usage)}
	for _, idx := range order {
		b := blocks[idx]
		cb := domain.ContentBlock{Type: b.typ}
		var raw json.RawMessage
		var err error
		switch b.typ {
		case "tool_use":
			cb.ToolUseID = b.toolID
			cb.ToolName = b.toolName
			cb.Input = b.toolInput
		case "thinking":
			cb.Text = b.thinking.String()
			raw, err = json.Marshal(map[string]any{
				"type":      "thinking",
				"thinking":  b.thinking.String(),
				"signature": b.signature.String(),
			})
		case "redacted_thinking":
			raw, err = json.Marshal(map[string]any{
				"type": "redacted_thinking",
				"data": b.data,
			})
		default:
			cb.Text = b.text.String()
		}
		if raw == nil && err == nil {
			raw, err = marshalTypedBlock(cb)
		}
		if err != nil {
			return Response{}, fmt.Errorf("model: encode streamed content block: %w", err)
		}
		cb.Raw = raw
		out.Content = append(out.Content, cb)
	}
	return out, nil
}

func applyWireUsagePatch(usage *wireUsage, patch wireUsagePatch) {
	if patch.CacheCreation != nil {
		if patch.CacheCreation.Ephemeral1hInputTokens != nil {
			usage.CacheCreation.Ephemeral1hInputTokens =
				*patch.CacheCreation.Ephemeral1hInputTokens
		}
		if patch.CacheCreation.Ephemeral5mInputTokens != nil {
			usage.CacheCreation.Ephemeral5mInputTokens =
				*patch.CacheCreation.Ephemeral5mInputTokens
		}
	}
	if patch.CacheReadInputTokens != nil {
		usage.CacheReadInputTokens = *patch.CacheReadInputTokens
	}
	if patch.InputTokens != nil {
		usage.InputTokens = *patch.InputTokens
	}
	if patch.OutputTokens != nil {
		usage.OutputTokens = *patch.OutputTokens
	}
	if patch.ServerToolUse != nil {
		if patch.ServerToolUse.WebFetchRequests != nil {
			usage.ServerToolUse.WebFetchRequests = *patch.ServerToolUse.WebFetchRequests
		}
		if patch.ServerToolUse.WebSearchRequests != nil {
			usage.ServerToolUse.WebSearchRequests = *patch.ServerToolUse.WebSearchRequests
		}
	}
	if patch.Speed != nil {
		usage.Speed = *patch.Speed
	}
	if patch.InferenceGeo != nil {
		usage.InferenceGeo = *patch.InferenceGeo
	}
}

func usageFromWire(usage wireUsage) domain.TokenUsage {
	return domain.TokenUsage{
		CacheCreation: domain.CacheCreationUsage{
			Ephemeral1hInputTokens: usage.CacheCreation.Ephemeral1hInputTokens,
			Ephemeral5mInputTokens: usage.CacheCreation.Ephemeral5mInputTokens,
		},
		CacheReadInputTokens: usage.CacheReadInputTokens,
		InputTokens:          usage.InputTokens,
		OutputTokens:         usage.OutputTokens,
		ServerToolUse: domain.ServerToolUsage{
			WebFetchRequests:  usage.ServerToolUse.WebFetchRequests,
			WebSearchRequests: usage.ServerToolUse.WebSearchRequests,
		},
		Speed:          usage.Speed,
		ProviderRegion: usage.InferenceGeo,
	}
}

// toWireBlock maps a domain ContentBlock to its Anthropic Messages wire shape.
// text blocks carry only text; tool_use blocks carry id/name/input; tool_result
// blocks carry tool_use_id, is_error, and a content array of text blocks (the
// wire shape the API requires, [{type:"text",text:...}]).
func toWireBlock(b domain.ContentBlock) (json.RawMessage, error) {
	if len(b.Raw) > 0 {
		return append(json.RawMessage(nil), b.Raw...), nil
	}
	return marshalTypedBlock(b)
}

func marshalTypedBlock(b domain.ContentBlock) (json.RawMessage, error) {
	var value any
	switch b.Type {
	case "tool_use":
		input := b.Input
		if input == nil {
			input = map[string]any{}
		}
		value = wireBlock{
			Type: "tool_use", ID: b.ToolUseID, Name: b.ToolName, Input: &input,
		}
	case "tool_result":
		wb := wireBlock{Type: "tool_result", ToolUseID: b.ToolResultFor, IsError: b.IsError}
		if len(b.ResultContent) > 0 {
			wb.Content = append([]json.RawMessage(nil), b.ResultContent...)
		} else if b.Text != "" {
			raw, err := json.Marshal(wireBlock{Type: "text", Text: b.Text})
			if err != nil {
				return nil, err
			}
			wb.Content = []json.RawMessage{raw}
		}
		value = wb
	default:
		value = wireBlock{Type: b.Type, Text: b.Text}
	}
	raw, err := json.Marshal(value)
	return json.RawMessage(raw), err
}

func parseProviderBlock(raw json.RawMessage) (domain.ContentBlock, error) {
	var header struct {
		Type  string         `json:"type"`
		Text  string         `json:"text"`
		ID    string         `json:"id"`
		Name  string         `json:"name"`
		Input map[string]any `json:"input"`
	}
	if err := json.Unmarshal(raw, &header); err != nil {
		return domain.ContentBlock{}, err
	}
	if header.Type == "" {
		return domain.ContentBlock{}, errors.New("content block type is required")
	}
	block := domain.ContentBlock{
		Type: header.Type,
		Text: header.Text,
		Raw:  append(json.RawMessage(nil), raw...),
	}
	if header.Type == "tool_use" {
		block.ToolUseID = header.ID
		block.ToolName = header.Name
		block.Input = header.Input
	}
	return block, nil
}

func hasNativeTool(tools []ToolSchema) bool {
	for _, tool := range tools {
		if tool.Type != "" {
			return true
		}
	}
	return false
}

const maxUpstreamErrorLen = 512

// sanitizeErrorText collapses whitespace/control characters to single spaces and
// truncates to maxUpstreamErrorLen bytes so a hostile or verbose upstream body
// cannot produce a multi-line or unbounded error string.
func sanitizeErrorText(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxUpstreamErrorLen {
		// Truncate on a rune boundary: back up from the byte limit until we are
		// at the start of a valid rune, so we never split a multibyte UTF-8
		// sequence and never emit invalid UTF-8.
		cut := maxUpstreamErrorLen
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…(truncated)"
	}
	return s
}
