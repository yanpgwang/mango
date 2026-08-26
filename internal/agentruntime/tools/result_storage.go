package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/yanpgwang/mango/internal/sandbox"
)

const (
	// Managed Agents writes serialized tool output above 100,000 characters to
	// the Session sandbox and gives the model a bounded preview plus file path.
	MaxInlineResultChars = 100_000
	ResultPreviewChars   = 2_000
	ToolResultsDirectory = "tool-results"
)

// MaterializeLargeResult applies the common result-size policy shared by
// built-ins and, later, managed MCP executors. Small results are returned
// unchanged. Large results are written under tool-results/ using the stable
// public tool-use id and replaced with a model-readable preview.
func MaterializeLargeResult(
	ctx context.Context,
	sb sandbox.Sandbox,
	toolUseID string,
	result Result,
) (Result, error) {
	serialized, extension, err := serializeResult(result.Content)
	if err != nil {
		return Result{}, fmt.Errorf("serialize tool result: %w", err)
	}
	characters := utf8.RuneCount(serialized)
	if characters <= MaxInlineResultChars {
		return result, nil
	}
	if sb == nil {
		return Result{}, fmt.Errorf("materialize tool result: sandbox is required")
	}
	filename := safeResultFilename(toolUseID) + extension
	resultPath := path.Join(ToolResultsDirectory, filename)
	if err := sb.WriteFile(ctx, resultPath, serialized); err != nil {
		return Result{}, fmt.Errorf("materialize tool result %q: %w", resultPath, err)
	}
	preview := truncateRunes(string(serialized), ResultPreviewChars)
	message := fmt.Sprintf(
		"<persisted-output>\n"+
			"Tool output exceeded %d characters. The full output was saved to %s (%d characters).\n"+
			"Use bash to inspect it in byte chunks. For example:\n"+
			"dd if=%s bs=%d skip=0 count=1 2>/dev/null\n"+
			"Increase skip by 1 to continue without emitting another oversized result.\n\n"+
			"Preview:\n%s\n"+
			"</persisted-output>",
		MaxInlineResultChars,
		resultPath,
		characters,
		resultPath,
		MaxReadFileBytes,
		preview,
	)
	return textResult(message, result.IsError), nil
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

func safeResultFilename(toolUseID string) string {
	var b strings.Builder
	for _, r := range toolUseID {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "tool-result"
	}
	return b.String()
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
