package agentruntime

import (
	"strings"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestRuntimeSkillToolSchemaIsPrivateDispatcherContract(t *testing.T) {
	schema := RuntimeSkillToolSchema()
	if schema.Name != RuntimeSkillToolName || schema.Type != "" {
		t.Fatalf("runtime Skill schema = %+v", schema)
	}
	if _, err := RuntimeSkillName(map[string]any{"skill": "report-tools"}); err != nil {
		t.Fatalf("valid Skill input rejected: %v", err)
	}
	for _, input := range []map[string]any{
		{},
		{"skill": ""},
		{"skill": " report-tools"},
		{"skill": 42},
		{"skill": "report-tools", "extra": true},
	} {
		if _, err := RuntimeSkillName(input); err == nil {
			t.Fatalf("invalid Skill input accepted: %#v", input)
		}
	}
}

func TestRuntimeSkillInjectionLoadsFullBodyAndCanBeRediscovered(t *testing.T) {
	body := []byte("---\nname: report-tools\ndescription: Analyze reports\n---\n\nFollow every instruction.\n")
	block := RuntimeSkillInjection("report-tools", body)
	if !strings.HasPrefix(
		block.Text,
		"Base directory for this skill: /workspace/skills/report-tools\n\n",
	) || !strings.HasSuffix(block.Text, string(body)) {
		t.Fatalf("runtime Skill injection = %q", block.Text)
	}
	loaded := LoadedRuntimeSkills([]domain.Message{{
		Role: domain.RoleUser, Content: []domain.ContentBlock{block},
	}})
	if _, ok := loaded["report-tools"]; !ok {
		t.Fatalf("loaded Skills = %#v", loaded)
	}
}

func TestRuntimeSkillInjectionRecognizesAgentScopedBaseDirectory(t *testing.T) {
	root := domain.SessionSkillsRoot + "/.agents/0123456789abcdef01234567"
	block := RuntimeSkillInjectionAt(root, "report-tools", []byte("child body"))
	if !strings.HasPrefix(
		block.Text,
		"Base directory for this skill: "+root+"/report-tools\n\n",
	) {
		t.Fatalf("Agent-scoped injection = %q", block.Text)
	}
	loaded := LoadedRuntimeSkills([]domain.Message{{
		Role: domain.RoleUser, Content: []domain.ContentBlock{block},
	}})
	if _, ok := loaded["report-tools"]; !ok {
		t.Fatalf("Agent-scoped Skill was not recognized: %#v", loaded)
	}
}

func TestRuntimeSkillInjectionRecognizesSelfHostedRelativeBaseDirectory(t *testing.T) {
	root := domain.SessionSkillsRelativeRoot + "/.agents/0123456789abcdef01234567"
	block := RuntimeSkillInjectionAt(root, "report-tools", []byte("child body"))
	loaded := LoadedRuntimeSkills([]domain.Message{{
		Role: domain.RoleUser, Content: []domain.ContentBlock{block},
	}})
	if _, ok := loaded["report-tools"]; !ok {
		t.Fatalf("relative self-hosted Skill was not recognized: %#v", loaded)
	}
}

func TestRuntimeSkillInjectionIsReattachedAfterCompaction(t *testing.T) {
	injection := RuntimeSkillInjection(
		"report-tools",
		[]byte("---\nname: report-tools\ndescription: Analyze reports\n---\ncomplete body\n"),
	)
	full := []domain.Message{
		{Role: domain.RoleUser, Content: []domain.ContentBlock{{
			Type: "text", Text: strings.Repeat("old context ", 2000),
		}}},
		{Role: domain.RoleAssistant, Content: []domain.ContentBlock{{
			Type: "tool_use", ToolUseID: "skill_1", ToolName: RuntimeSkillToolName,
			Input: map[string]any{"skill": "report-tools"},
		}}},
		{Role: domain.RoleUser, Content: []domain.ContentBlock{
			{Type: "tool_result", ToolResultFor: "skill_1", Text: "Launching skill: report-tools"},
			injection,
		}},
		{Role: domain.RoleAssistant, Content: []domain.ContentBlock{{
			Type: "text", Text: strings.Repeat("later work ", 2000),
		}}},
		{Role: domain.RoleUser, Content: []domain.ContentBlock{{
			Type: "text", Text: "current request",
		}}},
	}
	projected, projection := domain.CompactMessages(full, 300)
	if !projection.Compacted {
		t.Fatal("test transcript did not compact")
	}
	got := ReattachRuntimeSkillInjections(full, projected)
	if _, ok := LoadedRuntimeSkills(got)["report-tools"]; !ok {
		t.Fatalf("reattached messages lost loaded Skill: %#v", got)
	}
	last := got[len(got)-1]
	if last.Content[len(last.Content)-1].Text != "current request" {
		t.Fatalf("current request moved behind Skill instructions: %#v", last.Content)
	}
}

func TestTruncatedRuntimeSkillReattachmentCanBeReloaded(t *testing.T) {
	injection := RuntimeSkillInjection(
		"large-skill",
		[]byte(strings.Repeat("instruction ", 4000)),
	)
	full := []domain.Message{{
		Role: domain.RoleUser, Content: []domain.ContentBlock{injection},
	}}
	got := ReattachRuntimeSkillInjections(
		full,
		[]domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{
			Type: "text", Text: "current request",
		}}}},
	)
	if !strings.Contains(got[0].Content[0].Text, "skill content truncated for compaction") {
		t.Fatalf("large Skill was not bounded: %#v", got)
	}
	if _, loaded := LoadedRuntimeSkills(got)["large-skill"]; loaded {
		t.Fatal("truncated Skill incorrectly suppressed a full reload")
	}
}
