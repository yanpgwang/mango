package agentruntime

import (
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

// RuntimeSkillToolName is the model-visible dispatcher used to activate a
// discovered Skill. It is an agent-runtime primitive, not a configurable
// Mango runtime built-in and never appears in an Agent's public tools array.
const RuntimeSkillToolName = "Skill"

const runtimeSkillBasePrefix = "Base directory for this skill: "

const (
	runtimeSkillReattachTokens   = 5_000
	runtimeSkillsReattachTokens  = 25_000
	runtimeSkillCompactionMarker = "\n\n[... skill content truncated for compaction; re-invoke the Skill to restore the full content]"
)

// RuntimeSkillToolSchema describes the private dispatcher offered only when a
// Session has effective Skills. Discovery metadata remains in the system
// context; the dispatcher accepts the selected frontmatter name and lets the
// runtime load the corresponding immutable Session pin.
func RuntimeSkillToolSchema() model.ToolSchema {
	return model.ToolSchema{
		Name: RuntimeSkillToolName,
		Description: "Load an available Skill into the conversation when its " +
			"description matches the current task. Pass exactly one Skill name " +
			"from the available Skills list. The runtime will load its instructions.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"skill": map[string]any{
					"type":        "string",
					"description": "The exact name of the Skill to load.",
				},
			},
			"required":             []any{"skill"},
			"additionalProperties": false,
		},
	}
}

// RuntimeSkillName validates the dispatcher input before it reaches any
// filesystem operation. Membership in the Session's immutable pins is checked
// separately by the execution Activity.
func RuntimeSkillName(input map[string]any) (string, error) {
	if len(input) != 1 {
		return "", fmt.Errorf("skill: input must contain exactly one skill name")
	}
	name, ok := input["skill"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("skill: skill is required")
	}
	name = strings.TrimSpace(name)
	if name != input["skill"] {
		return "", fmt.Errorf("skill: skill must be an exact available Skill name")
	}
	return name, nil
}

// RuntimeSkillInjection is the CCB-compatible context block appended after the
// Skill tool result. The main SKILL.md body enters context here; later
// references remain on disk and are loaded progressively through ordinary
// sandbox tools.
func RuntimeSkillInjection(name string, body []byte) domain.ContentBlock {
	return RuntimeSkillInjectionAt(domain.SessionSkillsRoot, name, body)
}

// RuntimeSkillInjectionAt records the actual immutable Agent-scope directory
// so Claude Code-style supporting-file reads stay inside the selected bundle.
func RuntimeSkillInjectionAt(root string, name string, body []byte) domain.ContentBlock {
	base := root + "/" + name
	return domain.ContentBlock{
		Type: "text",
		Text: runtimeSkillBasePrefix + base + "\n\n" + string(body),
	}
}

// LoadedRuntimeSkills returns the Skills whose injected instruction block is
// still present in model context. Session pins are immutable, so presence of a
// successful injection is enough to suppress an identical re-load.
func LoadedRuntimeSkills(messages []domain.Message) map[string]struct{} {
	loaded := make(map[string]struct{})
	for _, message := range messages {
		for _, block := range message.Content {
			name, ok := runtimeSkillInjectionName(block)
			if ok {
				loaded[name] = struct{}{}
			}
		}
	}
	return loaded
}

func runtimeSkillInjectionName(block domain.ContentBlock) (string, bool) {
	if block.Type != "text" || !strings.HasPrefix(block.Text, runtimeSkillBasePrefix) {
		return "", false
	}
	if strings.HasSuffix(block.Text, runtimeSkillCompactionMarker) {
		// Claude Code permits an invoked Skill to be launched again after its
		// compaction reattachment was truncated, restoring the complete body.
		return "", false
	}
	line, _, found := strings.Cut(
		strings.TrimPrefix(block.Text, runtimeSkillBasePrefix),
		"\n",
	)
	if !found {
		return "", false
	}
	prefix := ""
	for _, candidate := range []string{
		domain.SessionSkillsRoot + "/",
		domain.SessionSkillsRelativeRoot + "/",
	} {
		if strings.HasPrefix(line, candidate) {
			prefix = candidate
			break
		}
	}
	if prefix == "" {
		return "", false
	}
	parts := strings.Split(strings.TrimPrefix(line, prefix), "/")
	var name string
	switch {
	case len(parts) == 1:
		name = parts[0]
	case len(parts) == 3 && parts[0] == ".agents" && len(parts[1]) == 24:
		if _, err := hex.DecodeString(parts[1]); err != nil {
			return "", false
		}
		name = parts[2]
	default:
		return "", false
	}
	if name == "" || strings.ContainsAny(name, "\\\x00") {
		return "", false
	}
	return name, true
}

type runtimeSkillInjection struct {
	name  string
	block domain.ContentBlock
	order int
}

// RuntimeSkillReattachmentBudget returns the CCB-compatible context reserve for
// the latest invocation of each loaded Skill: at most 5,000 estimated tokens
// per Skill and 25,000 across the Session.
func RuntimeSkillReattachmentBudget(messages []domain.Message) int {
	entries := latestRuntimeSkillInjections(messages)
	total := 0
	for _, entry := range entries {
		tokens := domain.EstimateTextTokens(entry.block.Text)
		if tokens > runtimeSkillReattachTokens {
			tokens = runtimeSkillReattachTokens
		}
		if tokens > runtimeSkillsReattachTokens-total {
			return runtimeSkillsReattachTokens
		}
		total += tokens
	}
	return total
}

// ReattachRuntimeSkillInjections carries invoked Skills across request-time
// compaction. It starts with the most recent invocation when applying the
// combined budget, then restores selected blocks in their original order.
// Complete bodies remain recognizable as loaded. Truncated reattachments are
// useful context but deliberately allow a later Skill invocation to restore
// the complete body, matching Claude Code's documented content lifecycle.
func ReattachRuntimeSkillInjections(
	full []domain.Message,
	projected []domain.Message,
) []domain.Message {
	entries := latestRuntimeSkillInjections(full)
	if len(entries) == 0 {
		return projected
	}
	already := LoadedRuntimeSkills(projected)
	remaining := runtimeSkillsReattachTokens
	selected := make([]runtimeSkillInjection, 0, len(entries))
	for index := len(entries) - 1; index >= 0 && remaining > 0; index-- {
		entry := entries[index]
		if _, present := already[entry.name]; present {
			continue
		}
		limit := runtimeSkillReattachTokens
		if remaining < limit {
			limit = remaining
		}
		entry.block.Text = truncateRuntimeSkillInjection(entry.block.Text, limit)
		remaining -= min(limit, domain.EstimateTextTokens(entry.block.Text))
		selected = append(selected, entry)
	}
	if len(selected) == 0 {
		return projected
	}
	sort.Slice(selected, func(i, j int) bool {
		return selected[i].order < selected[j].order
	})
	blocks := make([]domain.ContentBlock, 0, len(selected))
	for _, entry := range selected {
		blocks = append(blocks, entry.block)
	}
	if len(projected) == 0 {
		return []domain.Message{{Role: domain.RoleUser, Content: blocks}}
	}
	last := len(projected) - 1
	if projected[last].Role == domain.RoleUser {
		if messageHasToolResult(projected[last]) {
			projected[last].Content = append(projected[last].Content, blocks...)
		} else {
			projected[last].Content = append(blocks, projected[last].Content...)
		}
		return projected
	}
	return AppendMerging(projected, []domain.Message{{
		Role: domain.RoleUser, Content: blocks,
	}})
}

func latestRuntimeSkillInjections(messages []domain.Message) []runtimeSkillInjection {
	latest := make(map[string]runtimeSkillInjection)
	order := 0
	for _, message := range messages {
		for _, block := range message.Content {
			name, ok := runtimeSkillInjectionName(block)
			if ok {
				latest[name] = runtimeSkillInjection{name: name, block: block, order: order}
			}
			order++
		}
	}
	entries := make([]runtimeSkillInjection, 0, len(latest))
	for _, entry := range latest {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].order < entries[j].order
	})
	return entries
}

func truncateRuntimeSkillInjection(text string, maxTokens int) string {
	if domain.EstimateTextTokens(text) <= maxTokens {
		return text
	}
	maxBytes := maxTokens*3 - len(runtimeSkillCompactionMarker)
	if maxBytes < len(runtimeSkillBasePrefix) {
		maxBytes = len(runtimeSkillBasePrefix)
	}
	if maxBytes > len(text) {
		maxBytes = len(text)
	}
	for maxBytes > 0 && maxBytes < len(text) && !utf8.RuneStart(text[maxBytes]) {
		maxBytes--
	}
	return text[:maxBytes] + runtimeSkillCompactionMarker
}

func messageHasToolResult(message domain.Message) bool {
	for _, block := range message.Content {
		if block.Type == "tool_result" {
			return true
		}
	}
	return false
}
