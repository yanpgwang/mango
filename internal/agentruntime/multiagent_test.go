package agentruntime

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

func TestCoordinatorToolSchemasMatchManagedAgentsRuntimeSurface(t *testing.T) {
	schemas := CoordinatorToolSchemas()
	require.Len(t, schemas, 2)
	require.Equal(t, ListAgentsToolName, schemas[0].Name)
	require.Equal(t, SendToAgentToolName, schemas[1].Name)
	require.Equal(t, []any{"agent_name", "message"}, schemas[1].InputSchema["required"])
}

func TestAdvisorRequestQuotesExecutorContextWithoutReplayingReasoning(t *testing.T) {
	schema := AdvisorToolSchema()
	require.Empty(t, schema.Type)
	require.Equal(t, AdvisorToolName, schema.Name)
	require.Equal(t, false, schema.InputSchema["additionalProperties"])

	executor := model.Request{
		Model: "executor-model", System: "executor system",
		Tools: []model.ToolSchema{{
			Name: "read", Description: "Read a file.",
			InputSchema: map[string]any{"type": "object"},
		}},
		Messages: []domain.Message{{
			Role: domain.RoleAssistant,
			Content: []domain.ContentBlock{
				{Type: "thinking", Text: "private-reasoning-sentinel"},
				{Type: "text", Text: "visible plan"},
			},
		}},
	}
	request, err := AdvisorRequest(
		"advisor-model",
		executor,
		[]domain.ContentBlock{{
			Type: "tool_use", ToolUseID: "toolu_advisor",
			ToolName: AdvisorToolName, Input: map[string]any{},
		}},
	)
	require.NoError(t, err)
	require.Equal(t, "advisor-model", request.Model)
	require.Empty(t, request.Tools)
	require.Equal(t, 2048, request.MaxTokens)
	require.Len(t, request.Messages, 1)

	payload := request.Messages[0].Content[0].Text
	require.Contains(t, payload, "visible plan")
	require.Contains(t, payload, "reasoning_omitted")
	require.Contains(t, payload, "toolu_advisor")
	require.NotContains(t, payload, "private-reasoning-sentinel")
	var quoted map[string]any
	require.NoError(t, json.Unmarshal([]byte(strings.SplitN(payload, "\n\n", 2)[1]), &quoted))
}

func TestProjectCoordinatorSystemContext(t *testing.T) {
	got := ProjectCoordinatorSystemContext("You are the engineering lead.")
	require.Contains(t, got, "You are the engineering lead.")
	require.Contains(t, got, "<mango-coordinator>")
	require.Contains(t, got, "<agent-thread-message>")
	require.Contains(t, got, "It is not authored by the user")
	require.Contains(t, got, "Do not tell one Agent to wait for another Agent's future report")
	require.Equal(t, 1, strings.Count(got, "<mango-coordinator>"))

	empty := ProjectCoordinatorSystemContext("  ")
	require.True(t, strings.HasPrefix(empty, "<mango-coordinator>"))
}

func TestParseSendToAgentInput(t *testing.T) {
	got, err := ParseSendToAgentInput(map[string]any{
		"agent_name": " reviewer ", "message": " inspect auth ",
		"session_thread_id": " sthr_existing ",
	})
	require.NoError(t, err)
	require.Equal(t, "reviewer", got.AgentName)
	require.Equal(t, "inspect auth", got.Message)
	require.Equal(t, "sthr_existing", got.SessionThreadID)

	_, err = ParseSendToAgentInput(map[string]any{
		"agent_name": "reviewer", "message": "x", "unknown": true,
	})
	require.ErrorContains(t, err, "unknown field")
}
