package domain

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// MaxOutcomeRubricCharacters is the documented limit for inline and File rubrics.
const MaxOutcomeRubricCharacters = 262144

// MaxFileMessageCharacters bounds one uploaded UTF-8 File projected into a
// user.message. The limit keeps admission reads and durable event snapshots
// bounded independently of the 500 MB Files storage limit.
const MaxFileMessageCharacters = 262144

// FileMessageContent is the private, admission-time snapshot of one public
// document source that references a File. The public event retains only the
// file_id; the provider receives Content as an ordinary text block.
type FileMessageContent struct {
	ContentIndex int
	FileID       string
	Filename     string
	MimeType     string
	Content      string
}

type ContentBlock struct {
	Type          string         // typed local blocks plus losslessly preserved provider-native block types
	Text          string         // text blocks; also flattened text of a tool_result
	ToolUseID     string         // tool_use: the event id
	ToolName      string         // tool_use: tool name
	Input         map[string]any // tool_use: arguments
	ToolResultFor string         // tool_result: the tool_use id it answers
	IsError       bool           // tool_result: error flag
	// ResultContent preserves rich user/custom/MCP tool-result items (text,
	// image, document, and search_result) without flattening them. Text remains
	// populated as a convenient projection for existing executors.
	ResultContent []json.RawMessage `json:"result_content,omitempty"`
	// Raw is the complete provider content block. Provider adapters populate it
	// for responses so unknown server-tool, citation, encrypted-continuation,
	// and future fields can be round-tripped without flattening. Locally created
	// blocks leave Raw empty and are encoded from the typed fields above.
	Raw json.RawMessage `json:"raw,omitempty"`
}

type Message struct {
	Role    Role
	Content []ContentBlock
	// ContextUsage is private continuation metadata. It anchors one provider
	// response to the exact request usage the provider reported, allowing the
	// next request to measure only messages added after that response. Provider
	// adapters intentionally serialize only Role and Content.
	ContextUsage *ContextUsageAnchor `json:"context_usage,omitempty"`
}

// ContextUsageAnchor records the exact size of one completed provider request
// plus its response. RequestFingerprint protects the baseline when the model,
// system prompt, tools, or other request settings change. PrefixFingerprint
// protects it when compaction or another projection changes messages that the
// provider usage had measured. ContentBlocks marks the response boundary when
// adjacent assistant messages are merged to preserve role alternation.
type ContextUsageAnchor struct {
	Usage              ContextWindowUsage `json:"usage"`
	RequestFingerprint string             `json:"request_fingerprint"`
	PrefixFingerprint  string             `json:"prefix_fingerprint"`
	ContentBlocks      int                `json:"content_blocks"`
}

// ProviderToolUseMapping keeps provider-private tool ids separate from the
// stable public event ids exposed by Mango.
type ProviderToolUseMapping struct {
	PublicEventID     string `json:"public_event_id"`
	ProviderToolUseID string `json:"provider_tool_use_id"`
	ToolName          string `json:"tool_name"`
}

// ProviderTranscript is the committed, lossless model-continuation history for
// one Session. TriggerEventIDs identifies which public turns are represented so
// a caller can safely fall back for sessions created before transcript support.
type ProviderTranscript struct {
	Messages        []Message                `json:"messages"`
	TriggerEventIDs []string                 `json:"trigger_event_ids"`
	ToolUseMappings []ProviderToolUseMapping `json:"tool_use_mappings"`
}

// ProjectMessages folds an ordered session event log into a Messages-API
// conversation. User messages, cross-Thread messages, Agent messages, and
// paired tool calls/results become model input; status, error, and other
// server-only events are skipped. This is where "the server owns history" is
// realized: the durable event log is the single truth, projected to the model
// every turn.
//
// The real Messages API requires strictly alternating roles. Two real flows
// produce consecutive same-role messages: a user sending several user.message
// events before drainRuns claims them, and a model turn that emits no text
// (so no agent.message) leaving two user turns adjacent. To keep the request
// legal we merge adjacent same-role messages into one, concatenating their
// content blocks in order. This is a pure transformation of the event log.
func ProjectMessages(events []Event) []Message {
	// First pass: collect two sets in one scan.
	//
	//   answered — tool_use ids that some later tool_result references
	//   (agent.tool_result.tool_use_id / user.custom_tool_result.custom_tool_use_id,
	//   both pointing at a tool_use's committed Event.ID). A tool_use whose id is
	//   absent is dangling (e.g. an always_ask built-in that parked and never
	//   resumed); emitting the unpaired tool_use would make the projected request
	//   illegal (400), so those blocks are dropped in the second pass.
	//
	//   seen — ids that an actual agent.tool_use / agent.custom_tool_use event
	//   committed. Symmetrically, a tool_result whose referenced id is not in this
	//   set is an orphan (e.g. a client forging a user.custom_tool_result with a
	//   bogus custom_tool_use_id); emitting the unpaired tool_result would likewise
	//   make the request illegal (400), so those blocks are dropped too.
	answered := make(map[string]struct{})
	seen := make(map[string]struct{})
	for _, e := range events {
		switch e.Type {
		case EvAgentToolUse, EvAgentCustomToolUse, EvAgentMcpToolUse:
			id := e.ID
			if id == "" {
				id, _ = e.Payload["id"].(string)
			}
			if id != "" {
				seen[id] = struct{}{}
			}
		case EvAgentToolResult, EvAgentMcpToolResult:
			if id, _ := AgentToolResultReference(e.Type, e.Payload); id != "" {
				answered[id] = struct{}{}
			}
		case EvUserCustomToolResult:
			if id, _ := e.Payload["custom_tool_use_id"].(string); id != "" {
				answered[id] = struct{}{}
			}
		case EvUserToolResult:
			if id, _ := e.Payload["tool_use_id"].(string); id != "" {
				answered[id] = struct{}{}
			}
		}
	}

	var out []Message
	add := func(role Role, blocks []ContentBlock) {
		if len(blocks) == 0 {
			return
		}
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, blocks...)
			return
		}
		out = append(out, Message{Role: role, Content: blocks})
	}
	for _, e := range events {
		switch e.Type {
		case EvUserMessage:
			add(RoleUser, contentBlocks(e.Payload))
		case EvAgentThreadMessageReceived:
			add(RoleUser, receivedThreadMessageBlocks(e.Payload))
		case EvAgentThreadMessageSent:
			add(RoleAssistant, sentThreadMessageBlocks(e.Payload))
		case EvUserDefineOutcome:
			add(RoleUser, outcomePromptBlocks(e.Payload))
		case EvAgentMessage:
			add(RoleAssistant, contentBlocks(e.Payload))
		case EvAgentToolUse, EvAgentCustomToolUse, EvAgentMcpToolUse:
			// The correlation id is the committed event id (Event.ID), the same
			// value the public wire exposes and the value a tool_result event's
			// tool_use_id points at. payload["id"] holds the model's transient
			// id and is only a fallback for constructions without a committed ID.
			id := e.ID
			if id == "" {
				id, _ = e.Payload["id"].(string)
			}
			if id == "" {
				continue
			}
			// Drop dangling tool_use blocks: an unpaired tool_use makes the
			// projected request illegal. A paired tool_use (its id appears in a
			// later tool_result) is kept unchanged.
			if _, ok := answered[id]; !ok {
				continue
			}
			name, _ := e.Payload["name"].(string)
			input, _ := e.Payload["input"].(map[string]any)
			add(RoleAssistant, []ContentBlock{{Type: "tool_use", ToolUseID: id, ToolName: name, Input: input}})
		case EvAgentToolResult, EvAgentMcpToolResult:
			id, _ := AgentToolResultReference(e.Type, e.Payload)
			if id == "" {
				continue
			}
			// Drop orphan tool_result blocks: a result referencing a tool_use id
			// that no committed tool_use event produced is unpaired and would make
			// the projected request illegal. Symmetric to the dangling-tool_use drop.
			if _, ok := seen[id]; !ok {
				continue
			}
			add(RoleUser, []ContentBlock{resultBlock(id, e.Payload)})
		case EvUserCustomToolResult:
			id, _ := e.Payload["custom_tool_use_id"].(string)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; !ok {
				continue
			}
			add(RoleUser, []ContentBlock{resultBlock(id, e.Payload)})
		case EvUserToolResult:
			id, _ := e.Payload["tool_use_id"].(string)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; !ok {
				continue
			}
			add(RoleUser, []ContentBlock{resultBlock(id, e.Payload)})
		default:
			continue
		}
	}
	return out
}

// receivedThreadMessageBlocks preserves the sender identity carried by the public
// agent.thread_message_received event when projecting it into a provider's
// user-role input. Providers generally expose only user/assistant roles, so a
// structured envelope distinguishes an internal cross-Thread message from a
// message authored by the Session's user. The original rich content blocks are
// kept between the envelope markers rather than flattened.
func receivedThreadMessageBlocks(payload map[string]any) []ContentBlock {
	return envelopedThreadMessageBlocks(payload, struct {
		FromSessionThreadID string `json:"from_session_thread_id,omitempty"`
		FromAgentName       string `json:"from_agent_name,omitempty"`
	}{
		FromSessionThreadID: stringValue(payload["from_session_thread_id"]),
		FromAgentName:       stringValue(payload["from_agent_name"]),
	})
}

// sentThreadMessageBlocks preserves a Thread's own delegated task, follow-up,
// or report in event-ledger context reconstruction. A sent message is
// assistant-authored; omitting it would make a coordinator forget its prior
// delegation (and make a child forget its prior report) whenever the private
// provider transcript is unavailable and PrepareTurn safely falls back.
func sentThreadMessageBlocks(payload map[string]any) []ContentBlock {
	return envelopedThreadMessageBlocks(payload, struct {
		ToSessionThreadID string `json:"to_session_thread_id,omitempty"`
		ToAgentName       string `json:"to_agent_name,omitempty"`
	}{
		ToSessionThreadID: stringValue(payload["to_session_thread_id"]),
		ToAgentName:       stringValue(payload["to_agent_name"]),
	})
}

func envelopedThreadMessageBlocks(payload map[string]any, identity any) []ContentBlock {
	content := contentBlocks(payload)
	if len(content) == 0 {
		return nil
	}
	metadata, _ := json.Marshal(identity)
	blocks := make([]ContentBlock, 0, len(content)+2)
	blocks = append(blocks, ContentBlock{
		Type: "text",
		Text: "<agent-thread-message>\n<metadata>" + string(metadata) +
			"</metadata>\n<content>",
	})
	blocks = append(blocks, content...)
	blocks = append(blocks, ContentBlock{
		Type: "text", Text: "</content>\n</agent-thread-message>",
	})
	return blocks
}

func stringValue(raw any) string {
	value, _ := raw.(string)
	return value
}

func outcomePromptBlocks(payload map[string]any) []ContentBlock {
	description, _ := payload["description"].(string)
	maxIterations := 3
	if raw, ok := payload["max_iterations"].(float64); ok && raw >= 1 {
		maxIterations = int(raw)
	}
	rubricText, _ := OutcomeRubricContent(payload)
	text := "Work toward the following outcome and produce the requested deliverable.\n\n" +
		"Outcome:\n" + description + "\n\nRubric:\n" + rubricText +
		fmt.Sprintf("\n\nThe harness may evaluate and request up to %d revision cycles.", maxIterations)
	return []ContentBlock{{Type: "text", Text: text}}
}

// WithOutcomeRubricContent attaches the resolved bytes of a file-backed rubric
// to a cloned private event payload. The public rubric union remains unchanged;
// HTTP projections redact this internal field by prefix convention.
func WithOutcomeRubricContent(payload map[string]any, content string) map[string]any {
	resolved := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		resolved[key] = value
	}
	resolved[InternalOutcomeRubricContent] = content
	return resolved
}

// OutcomeRubricContent projects both public rubric variants onto the one text
// input consumed by the working agent and isolated grader. File rubrics must
// have been resolved and snapshotted before event admission.
func OutcomeRubricContent(payload map[string]any) (string, bool) {
	rubric, ok := payload["rubric"].(map[string]any)
	if !ok {
		return "", false
	}
	switch rubric["type"] {
	case "text":
		content, ok := rubric["content"].(string)
		return content, ok
	case "file":
		content, ok := payload[InternalOutcomeRubricContent].(string)
		return content, ok
	default:
		return "", false
	}
}

func resultBlock(toolUseID string, payload map[string]any) ContentBlock {
	b := ContentBlock{Type: "tool_result", ToolResultFor: toolUseID}
	b.IsError, _ = payload["is_error"].(bool)
	b.Text = flattenText(payload["content"])
	b.ResultContent = rawContentBlocks(payload["content"])
	return b
}

func flattenText(raw any) string {
	blocks, ok := raw.([]any)
	if !ok {
		return ""
	}
	var sb strings.Builder
	for _, item := range blocks {
		if m, ok := item.(map[string]any); ok {
			if t, _ := m["type"].(string); t == "text" {
				if s, _ := m["text"].(string); s != "" {
					sb.WriteString(s)
				}
			}
		}
	}
	return sb.String()
}

func contentBlocks(payload map[string]any) []ContentBlock {
	raw, ok := payload["content"].([]any)
	if !ok {
		return nil
	}
	fileContents := FileMessageContents(payload)
	var blocks []ContentBlock
	for index, item := range raw {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		t, _ := block["type"].(string)
		if t == "" {
			continue
		}
		if t == "text" {
			text, _ := block["text"].(string)
			if strings.TrimSpace(text) == "" {
				continue
			}
			blocks = append(blocks, ContentBlock{Type: "text", Text: text})
			continue
		}
		if t == "document" {
			source, _ := block["source"].(map[string]any)
			if source["type"] == "file" {
				snapshot, found := fileContents[strconv.Itoa(index)]
				if !found || snapshot.FileID != source["file_id"] {
					// File-sourced messages have always required an admission
					// snapshot. A missing or mismatched private value indicates
					// corrupt historical state; never pass the unresolved file_id
					// to an external provider.
					continue
				}
				blocks = append(blocks, ContentBlock{
					Type: "text", Text: projectFileMessageText(block, snapshot),
				})
				continue
			}
		}
		encoded, err := json.Marshal(block)
		if err != nil {
			continue
		}
		blocks = append(blocks, ContentBlock{
			Type: t,
			Raw:  json.RawMessage(encoded),
		})
	}
	return blocks
}

// WithFileMessageContents attaches immutable File text snapshots to a cloned
// event payload. HTTP projections redact this top-level internal key while the
// PostgreSQL event ledger preserves it for replay and later turns.
func WithFileMessageContents(
	payload map[string]any,
	contents []FileMessageContent,
) map[string]any {
	resolved := make(map[string]any, len(payload)+1)
	for key, value := range payload {
		resolved[key] = value
	}
	stored := make(map[string]any, len(contents))
	for _, content := range contents {
		stored[strconv.Itoa(content.ContentIndex)] = map[string]any{
			"file_id": content.FileID, "filename": content.Filename,
			"mime_type": content.MimeType, "content": content.Content,
		}
	}
	resolved[InternalFileMessageContents] = stored
	return resolved
}

// FileMessageContents decodes both newly constructed and PostgreSQL JSON
// payloads into snapshots keyed by the public content-block index.
func FileMessageContents(payload map[string]any) map[string]FileMessageContent {
	raw, ok := payload[InternalFileMessageContents].(map[string]any)
	if !ok {
		return nil
	}
	contents := make(map[string]FileMessageContent, len(raw))
	for index, value := range raw {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		parsedIndex, err := strconv.Atoi(index)
		if err != nil || parsedIndex < 0 {
			continue
		}
		content := FileMessageContent{
			ContentIndex: parsedIndex,
			FileID:       stringValue(item["file_id"]),
			Filename:     stringValue(item["filename"]),
			MimeType:     stringValue(item["mime_type"]),
			Content:      stringValue(item["content"]),
		}
		if content.FileID == "" {
			continue
		}
		contents[index] = content
	}
	return contents
}

func projectFileMessageText(block map[string]any, snapshot FileMessageContent) string {
	metadata := map[string]string{
		"file_id": snapshot.FileID, "filename": snapshot.Filename,
		"media_type": snapshot.MimeType,
	}
	if title, _ := block["title"].(string); title != "" {
		metadata["title"] = title
	}
	if context, _ := block["context"].(string); context != "" {
		metadata["context"] = context
	}
	encoded, _ := json.Marshal(metadata)
	return "The user attached the following UTF-8 text document.\n" +
		"<file_metadata>" + string(encoded) + "</file_metadata>\n" +
		"<file_content>\n" + snapshot.Content + "\n</file_content>"
}

func rawContentBlocks(raw any) []json.RawMessage {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	hasRichContent := false
	for _, item := range items {
		block, ok := item.(map[string]any)
		if !ok || block["type"] != "text" {
			hasRichContent = true
			break
		}
	}
	if !hasRichContent {
		return nil
	}
	out := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		encoded, err := json.Marshal(item)
		if err == nil {
			out = append(out, json.RawMessage(encoded))
		}
	}
	return out
}

// ProjectSystemContext appends persisted mid-conversation system messages to
// the Agent's top-level system prompt. The current accompanying system.message
// is causally linked into trigger when it follows that trigger in receipt order
// and therefore is not yet visible to HistoryThrough.
func ProjectSystemContext(base string, events []Event, trigger Event) string {
	var additions []string
	appendContent := func(raw any) {
		for _, block := range rawContentItems(raw) {
			if block["type"] != "text" {
				continue
			}
			text, _ := block["text"].(string)
			if strings.TrimSpace(text) != "" {
				additions = append(additions, text)
			}
		}
	}
	for _, event := range events {
		if event.Type == EvSystemMessage {
			appendContent(event.Payload["content"])
		}
	}
	appendContent(trigger.Payload[InternalCompanionSystemContent])
	if len(additions) == 0 {
		return base
	}
	var projected strings.Builder
	projected.WriteString(base)
	for _, addition := range additions {
		if projected.Len() > 0 {
			projected.WriteString("\n\n")
		}
		projected.WriteString("<system-message>\n")
		projected.WriteString(addition)
		projected.WriteString("\n</system-message>")
	}
	return projected.String()
}

// ProjectSessionResourceContext tells the model which immutable files and
// mounted Memory Stores are available before it chooses a tool. Contents are
// never injected into the prompt.
func ProjectSessionResourceContext(base string, resources []SessionResource) string {
	files := make([]SessionResource, 0, len(resources))
	memories := make([]SessionResource, 0, len(resources))
	for _, resource := range resources {
		if resource.State == SessionResourceActive {
			if resource.Type() == SessionResourceTypeMemoryStore {
				memories = append(memories, resource)
			} else {
				files = append(files, resource)
			}
		}
	}
	if len(files) == 0 && len(memories) == 0 {
		return base
	}
	var section strings.Builder
	section.WriteString("<session_resources>\n")
	if len(files) > 0 {
		section.WriteString("Read-only files available in the sandbox:\n")
		for _, resource := range files {
			section.WriteString("- ")
			encoded, _ := json.Marshal(struct {
				MountPath string `json:"mount_path"`
				FileID    string `json:"file_id"`
			}{MountPath: resource.MountPath, FileID: resource.FileID})
			section.Write(encoded)
			section.WriteByte('\n')
		}
	}
	if len(memories) > 0 {
		section.WriteString("Memory Stores available as ordinary sandbox files. Use standard file tools to read and, when access is read_write, modify them:\n")
		for _, resource := range memories {
			section.WriteString("- ")
			encoded, _ := json.Marshal(struct {
				MemoryStoreID string `json:"memory_store_id"`
				Name          string `json:"name"`
				Description   string `json:"description"`
				MountPath     string `json:"mount_path"`
				Access        string `json:"access"`
				Instructions  string `json:"instructions,omitempty"`
			}{
				MemoryStoreID: resource.MemoryStoreID,
				Name:          resource.MemoryStoreName,
				Description:   resource.MemoryStoreDescription,
				MountPath:     resource.MountPath,
				Access:        resource.MemoryAccess,
				Instructions:  resource.MemoryInstructions,
			})
			section.Write(encoded)
			section.WriteByte('\n')
		}
	}
	section.WriteString("</session_resources>")
	if strings.TrimSpace(base) == "" {
		return section.String()
	}
	return base + "\n\n" + section.String()
}

// ProjectSessionSkillContext exposes only trusted Skill discovery metadata.
// Bundle contents stay on disk until the model invokes the runtime's private
// Skill dispatcher, which injects the selected SKILL.md into the conversation.
func ProjectSessionSkillContext(
	base string,
	skills []SkillVersion,
	descriptionCharacterBudget int,
) string {
	return ProjectSkillRuntimeContext(
		base, SkillRuntime{Root: SessionSkillsRoot, Versions: skills},
		descriptionCharacterBudget,
	)
}

// ProjectSkillRuntimeContext exposes discovery metadata only for the current
// resolved Agent scope. Supporting files remain in the shared sandbox, but no
// other Agent's Skill list enters this Thread's model context.
func ProjectSkillRuntimeContext(
	base string,
	runtime SkillRuntime,
	descriptionCharacterBudget int,
) string {
	section := skillRuntimeContextSection(runtime, descriptionCharacterBudget)
	if section == "" {
		return base
	}
	if strings.TrimSpace(base) == "" {
		return section
	}
	return base + "\n\n" + section
}

func skillRuntimeContextSection(
	runtime SkillRuntime,
	descriptionCharacterBudget int,
) string {
	if len(runtime.Versions) == 0 {
		return ""
	}
	if descriptionCharacterBudget < 0 {
		descriptionCharacterBudget = 0
	}
	var section strings.Builder
	section.WriteString("<available_skills>\n")
	section.WriteString("Custom Skills available to the Skill tool. Invoke a Skill only when its description matches the task; the runtime will load its main instructions into the conversation. Use read or bash only for supporting files referenced by those instructions:\n")
	for _, skill := range runtime.Versions {
		section.WriteString("- ")
		description := []rune(skill.Description)
		if len(description) > descriptionCharacterBudget {
			switch descriptionCharacterBudget {
			case 0:
				description = nil
			case 1:
				description = []rune("…")
			default:
				description = append(description[:descriptionCharacterBudget-1], '…')
			}
		}
		descriptionCharacterBudget -= len(description)
		encoded, _ := json.Marshal(struct {
			Name        string `json:"name"`
			Description string `json:"description,omitempty"`
			SkillMD     string `json:"skill_md"`
		}{
			Name:        skill.Name,
			Description: string(description),
			SkillMD:     runtime.SkillPath(skill.Name) + "/SKILL.md",
		})
		section.Write(encoded)
		section.WriteByte('\n')
	}
	section.WriteString("</available_skills>")
	return section.String()
}

func rawContentItems(raw any) []map[string]any {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if block, ok := item.(map[string]any); ok {
			out = append(out, block)
		}
	}
	return out
}
