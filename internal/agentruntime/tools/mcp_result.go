package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yanpgwang/mango/internal/mcpclient"
)

// ProjectMCPResult separates a protocol-native MCP result from the content sent
// to the model. _meta and other control fields remain only in the bounded raw
// diagnostic value. The control plane cannot write into an operator-owned
// self-hosted workspace, so binary content is reported as unsupported instead
// of returning a path the worker could not read.
func ProjectMCPResult(
	input mcpclient.Result,
) (result Result, raw json.RawMessage, err error) {
	var wire struct {
		Content           []json.RawMessage `json:"content"`
		StructuredContent any               `json:"structuredContent"`
	}
	if err := json.Unmarshal(input.Raw, &wire); err != nil {
		return Result{}, nil, fmt.Errorf("decode MCP result: %w", err)
	}
	var textParts []string
	for index, content := range wire.Content {
		var block map[string]any
		if err := json.Unmarshal(content, &block); err != nil {
			return Result{}, nil, fmt.Errorf(
				"decode MCP content block %d: %w",
				index,
				err,
			)
		}
		typ, _ := block["type"].(string)
		switch typ {
		case "text":
			if text, _ := block["text"].(string); text != "" {
				textParts = append(textParts, text)
			}
		case "image", "audio":
			textParts = append(
				textParts,
				fmt.Sprintf("MCP returned %s content, which is not available to this self-hosted worker.", typ),
			)
		case "resource":
			resource, _ := block["resource"].(map[string]any)
			uri, _ := resource["uri"].(string)
			if text, _ := resource["text"].(string); text != "" {
				if uri != "" {
					textParts = append(textParts, "Resource "+uri+":\n"+text)
				} else {
					textParts = append(textParts, text)
				}
				continue
			}
			if _, ok := resource["blob"].(string); ok {
				textParts = append(
					textParts,
					fmt.Sprintf("MCP resource %s contains binary data that is not available to this self-hosted worker.", uri),
				)
			}
		case "resource_link":
			uri, _ := block["uri"].(string)
			name, _ := block["name"].(string)
			textParts = append(
				textParts,
				fmt.Sprintf("MCP resource link %s: %s", name, uri),
			)
		default:
			// Unknown protocol content remains available in the raw diagnostic
			// result but is not implicitly trusted as model context. In
			// particular, this prevents future control/_meta fields from becoming
			// prompt content before the projection policy understands them.
			if typ != "" {
				textParts = append(
					textParts,
					"MCP returned unsupported "+typ+" content.",
				)
			}
		}
	}
	if wire.StructuredContent != nil {
		structured, err := json.MarshalIndent(wire.StructuredContent, "", "  ")
		if err != nil {
			return Result{}, nil, fmt.Errorf(
				"encode MCP structured content: %w",
				err,
			)
		}
		textParts = append(textParts, "Structured content:\n"+string(structured))
	}
	if len(textParts) == 0 {
		textParts = append(textParts, "MCP tool returned no model-visible content.")
	}
	result = textResult(strings.Join(textParts, "\n\n"), input.IsError)
	result = BoundInlineResult(result)

	if len(input.Raw) <= MaxInlineResultChars {
		raw = append(json.RawMessage(nil), input.Raw...)
	}
	return result, raw, nil
}
