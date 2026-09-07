package agentruntime

import (
	"github.com/yanpgwang/mango/internal/agentruntime/tools"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

// ValidateToolCapabilities rejects permission semantics that a provider-native
// model tool cannot pause for Mango to approve.
func ValidateToolCapabilities(toolSet domain.ToolSet) error {
	for _, name := range []string{"web_search", "web_fetch"} {
		enabled, policy := toolSet.BuiltinEnabled(name)
		if enabled && policy.Type != "always_allow" {
			return domain.Validation(name + " requires always_allow while it is provider-native")
		}
	}
	return nil
}

// AppendMerging appends messages while preserving strict role alternation.
func AppendMerging(base, added []domain.Message) []domain.Message {
	out := base
	for _, message := range added {
		if count := len(out); count > 0 && out[count-1].Role == message.Role {
			merged := make([]domain.ContentBlock, 0, len(out[count-1].Content)+len(message.Content))
			merged = append(merged, out[count-1].Content...)
			merged = append(merged, message.Content...)
			copyOfMessages := append([]domain.Message(nil), out...)
			anchor := out[count-1].ContextUsage
			if message.ContextUsage != nil {
				anchor = message.ContextUsage
			}
			copyOfMessages[count-1] = domain.Message{
				Role: out[count-1].Role, Content: merged, ContextUsage: anchor,
			}
			out = copyOfMessages
			continue
		}
		out = append(out, message)
	}
	return out
}

// EnabledToolSchemas returns enabled built-ins in canonical order, followed by
// application-owned custom tools.
func EnabledToolSchemas(toolSet domain.ToolSet) []model.ToolSchema {
	schemas := enabledBuiltinSchemas(toolSet)
	for _, custom := range toolSet.Custom {
		schemas = append(schemas, model.ToolSchema{
			Name: custom.Name, Description: custom.Description, InputSchema: custom.InputSchema,
		})
	}
	return schemas
}

// EnabledSelfHostedToolSchemas declares the contracts implemented by an
// external Environment worker while retaining provider-native Web tools.
func EnabledSelfHostedToolSchemas(toolSet domain.ToolSet) []model.ToolSchema {
	schemas := EnabledToolSchemas(toolSet)
	for index := range schemas {
		if schemas[index].Name == "bash" {
			schemas[index].InputSchema = tools.SelfHostedSchema("bash")
		}
	}
	return schemas
}

func enabledBuiltinSchemas(toolSet domain.ToolSet) []model.ToolSchema {
	var schemas []model.ToolSchema
	for _, name := range domain.BuiltinToolNames {
		if enabled, _ := toolSet.BuiltinEnabled(name); !enabled {
			continue
		}
		switch name {
		case "web_search":
			schemas = append(schemas, model.ToolSchema{Type: "web_search_20260318", Name: name})
			continue
		case "web_fetch":
			schemas = append(schemas, model.ToolSchema{Type: "web_fetch_20260318", Name: name})
			continue
		}
		if schema := tools.Schema(name); schema != nil {
			schemas = append(schemas, model.ToolSchema{Name: name, InputSchema: schema})
		}
	}
	return schemas
}

func TextBlocksToContent(blocks []domain.ContentBlock) []any {
	content := make([]any, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "text" && block.Text != "" {
			content = append(content, map[string]any{"type": "text", "text": block.Text})
		}
	}
	return content
}

func HasThinkingBlocks(blocks []domain.ContentBlock) bool {
	for _, block := range blocks {
		if block.Type == "thinking" || block.Type == "redacted_thinking" {
			return true
		}
	}
	return false
}

func FlattenResultText(content []any) string {
	var text string
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok || block["type"] != "text" {
			continue
		}
		value, _ := block["text"].(string)
		text += value
	}
	return text
}
