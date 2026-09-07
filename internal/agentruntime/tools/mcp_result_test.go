package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yanpgwang/mango/internal/mcpclient"
)

func TestProjectMCPResult_SeparatesRawAndRejectsBinaryContent(t *testing.T) {
	input := mcpclient.Result{
		Raw: json.RawMessage(`{
			"_meta":{"trace":"private-meta"},
			"content":[
				{"type":"text","text":"hello"},
				{"type":"image","mimeType":"image/png","data":"aW1hZ2UtYnl0ZXM="},
				{"type":"future_control","_meta":{"secret":"do-not-project"}}
			],
			"structuredContent":{"count":2}
		}`),
	}
	result, raw, err := ProjectMCPResult(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "private-meta") {
		t.Fatalf("raw=%s", raw)
	}
	text := result.Content[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "hello") ||
		!strings.Contains(text, "not available to this self-hosted worker") ||
		!strings.Contains(text, `"count": 2`) {
		t.Fatalf("model projection = %q", text)
	}
	if strings.Contains(text, "private-meta") {
		t.Fatalf("MCP _meta leaked into model projection: %q", text)
	}
	if strings.Contains(text, "do-not-project") ||
		!strings.Contains(text, "unsupported future_control content") {
		t.Fatalf("unknown MCP control content was projected unsafely: %q", text)
	}
}

func TestProjectMCPResult_LargeRawIsOmitted(t *testing.T) {
	raw := json.RawMessage(`{"content":[{"type":"text","text":"ok"}],"_meta":{"large":"` +
		strings.Repeat("x", MaxInlineResultChars) + `"}}`)
	_, inline, err := ProjectMCPResult(mcpclient.Result{Raw: raw})
	if err != nil {
		t.Fatal(err)
	}
	if inline != nil {
		t.Fatalf("inline=%s", inline)
	}
}
