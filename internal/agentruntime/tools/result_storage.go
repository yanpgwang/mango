package tools

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

const (
	// Managed Agents writes serialized tool output above 100,000 characters to
	// the Session sandbox and gives the model a bounded preview plus file path.
	MaxInlineResultChars = 100_000
	ResultPreviewChars   = 2_000
)

// BoundInlineResult limits control-plane tool output without inventing a
// server-local file path that an operator-owned worker cannot read.
func BoundInlineResult(result Result) Result {
	serialized, _, err := serializeResult(result.Content)
	if err != nil || utf8.RuneCount(serialized) <= MaxInlineResultChars {
		return result
	}
	characters := utf8.RuneCount(serialized)
	preview := truncateRunes(string(serialized), ResultPreviewChars)
	return textResult(fmt.Sprintf(
		"<truncated-output>\nTool output exceeded %d characters. The control plane omitted the full result (%d characters) because self-hosted workers do not share its filesystem.\n\nPreview:\n%s\n</truncated-output>",
		MaxInlineResultChars, characters, preview,
	), result.IsError)
}

func serializeResult(content []any) ([]byte, string, error) {
	if len(content) == 1 {
		if block, ok := content[0].(map[string]any); ok {
			if typ, _ := block["type"].(string); typ == "text" {
				if text, ok := block["text"].(string); ok {
					return []byte(text), ".txt", nil
				}
			}
		}
	}
	raw, err := json.MarshalIndent(content, "", "  ")
	return raw, ".json", err
}

func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit])
}
