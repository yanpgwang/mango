package temporal

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestPlanToolBatch_ClassifiesWholeRoundBeforeExecution(t *testing.T) {
	uses := []domain.ContentBlock{
		{
			Type: "tool_use", ToolUseID: "sevt_custom", ToolName: "ask_client",
			Input: map[string]any{"question": "continue?"},
		},
		{
			Type: "tool_use", ToolUseID: "sevt_builtin", ToolName: "bash",
			Input: map[string]any{"command": "pwd"},
		},
		{
			Type: "tool_use", ToolUseID: "sevt_ask", ToolName: "write",
			Input: map[string]any{"path": "a.txt", "content": "hello"},
		},
	}
	tools := indexTurnTools([]TurnTool{
		{Name: "ask_client", Kind: TurnToolCustom},
		{
			Name: "bash", Kind: TurnToolBuiltin,
			Permission: domain.PermissionPolicy{Type: "always_allow"},
		},
		{
			Name: "write", Kind: TurnToolBuiltin,
			Permission: domain.PermissionPolicy{Type: "always_ask"},
		},
	})
	steps := map[string]PlannedToolStep{
		"sevt_custom": {
			ToolUseEventID: "sevt_custom", ToolStepID: "tstep_custom",
		},
		"sevt_builtin": {
			ToolUseEventID: "sevt_builtin", ToolStepID: "tstep_builtin",
		},
		"sevt_ask": {
			ToolUseEventID: "sevt_ask", ToolStepID: "tstep_ask",
		},
	}

	plan, failure := planToolBatch(uses, tools, steps, true)

	require.Empty(t, failure)
	require.Equal(t, []string{
		domain.EvAgentCustomToolUse,
		domain.EvAgentToolUse,
		domain.EvAgentToolUse,
	}, draftTypes(plan.actionDrafts))
	require.Equal(t, []string{"sevt_custom", "sevt_ask"}, plan.pendingActionEventIDs)
	require.Equal(t, []plannedToolUse{{
		use:           uses[1],
		publicEventID: "sevt_builtin",
		useEventType:  domain.EvAgentToolUse,
		stepID:        "tstep_builtin",
		definition:    tools["bash"],
	}}, plan.executable)
	require.Equal(t, "allow", plan.actionDrafts[1].Payload["evaluated_permission"])
	require.Equal(
		t,
		"ask",
		plan.actionDrafts[2].Payload["evaluated_permission"],
	)
}

func TestPlanToolBatch_KeepsCoordinatorToolsOutOfPublicToolEvents(t *testing.T) {
	use := domain.ContentBlock{
		Type: "tool_use", ToolUseID: "provider_delegate",
		ToolName: "send_to_agent",
		Input:    map[string]any{"agent_name": "reviewer", "message": "review auth"},
	}
	tools := indexTurnTools([]TurnTool{{
		Name: "send_to_agent", Kind: TurnToolCoordinator,
		Permission: domain.PermissionPolicy{Type: "always_allow"},
	}})
	plan, failure := planToolBatch(
		[]domain.ContentBlock{use}, tools,
		map[string]PlannedToolStep{"provider_delegate": {
			ToolUseEventID:    "sevt_private_delegate",
			ProviderToolUseID: "provider_delegate",
			ToolStepID:        "tstep_delegate",
		}},
		true,
	)

	require.Empty(t, failure)
	require.Empty(t, plan.actionDrafts)
	require.Empty(t, plan.pendingActionEventIDs)
	require.Equal(t, []plannedToolUse{{
		use: use, publicEventID: "sevt_private_delegate",
		stepID: "tstep_delegate", definition: tools["send_to_agent"],
	}}, plan.executable)
}

func TestPlanToolBatch_RejectsInvalidRoundBeforePlanning(t *testing.T) {
	tests := []struct {
		name        string
		use         domain.ContentBlock
		tools       []TurnTool
		steps       map[string]PlannedToolStep
		wantFailure turnFailure
	}{
		{
			name: "missing durable operation id",
			use: domain.ContentBlock{
				Type: "tool_use", ToolUseID: "sevt_bash", ToolName: "bash",
			},
			tools: []TurnTool{{
				Name: "bash", Kind: TurnToolBuiltin,
				Permission: domain.PermissionPolicy{Type: "always_allow"},
			}},
			steps:       map[string]PlannedToolStep{},
			wantFailure: failTurn("model tool request has no durable operation id"),
		},
		{
			name: "tool not enabled",
			use: domain.ContentBlock{
				Type: "tool_use", ToolUseID: "sevt_missing", ToolName: "missing",
			},
			steps: map[string]PlannedToolStep{"sevt_missing": {
				ToolUseEventID: "sevt_missing", ToolStepID: "tstep_missing",
			}},
			wantFailure: failTurn(
				"model requested a tool that is not enabled: missing",
			),
		},
		{
			name: "unsupported builtin permission",
			use: domain.ContentBlock{
				Type: "tool_use", ToolUseID: "sevt_bash", ToolName: "bash",
			},
			tools: []TurnTool{{
				Name: "bash", Kind: TurnToolBuiltin,
				Permission: domain.PermissionPolicy{Type: "always_deny"},
			}},
			steps: map[string]PlannedToolStep{"sevt_bash": {
				ToolUseEventID: "sevt_bash", ToolStepID: "tstep_bash",
			}},
			wantFailure: failTurn(
				"built-in tool has unsupported permission policy: always_deny",
			),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			plan, failure := planToolBatch(
				[]domain.ContentBlock{tc.use},
				indexTurnTools(tc.tools),
				tc.steps,
				true,
			)
			require.Equal(t, tc.wantFailure, failure)
			require.Empty(t, plan.actionDrafts)
			require.Empty(t, plan.executable)
			require.Empty(t, plan.pendingActionEventIDs)
		})
	}
}

func TestPlanToolBatch_SelfHostedBuiltinParksForClientResult(t *testing.T) {
	use := domain.ContentBlock{
		Type: "tool_use", ToolUseID: "provider_read", ToolName: "read",
		Input: map[string]any{"path": "report.md"},
	}
	plan, failure := planToolBatch(
		[]domain.ContentBlock{use},
		indexTurnTools([]TurnTool{{
			Name: "read", Kind: TurnToolSelfHosted,
			Permission: domain.PermissionPolicy{Type: "always_allow"},
		}}),
		map[string]PlannedToolStep{"provider_read": {
			ToolUseEventID: "sevt_read", ProviderToolUseID: "provider_read",
			ToolStepID: "tstep_read",
		}},
		true,
	)
	require.Empty(t, failure)
	require.Empty(t, plan.executable)
	require.Equal(t, []string{"sevt_read"}, plan.pendingActionEventIDs)
	require.Equal(t, domain.EvAgentToolUse, plan.actionDrafts[0].Type)
	require.Equal(t, "allow", plan.actionDrafts[0].Payload["evaluated_permission"])
	require.Equal(t, "self_hosted", plan.actionDrafts[0].Payload[domain.InternalToolExecutionOwner])
}

func TestIndexTurnTools_PreservesFirstOwner(t *testing.T) {
	tools := indexTurnTools([]TurnTool{
		{
			Name: "read", Kind: TurnToolBuiltin,
			Permission: domain.PermissionPolicy{Type: "always_allow"},
		},
		{Name: "read", Kind: TurnToolCustom},
	})

	require.Equal(t, TurnToolBuiltin, tools["read"].Kind)
}

func TestPlanToolBatch_ExternalPermissionIsIndependentOfExecutionOwner(t *testing.T) {
	for _, policy := range []string{"always_allow", "always_ask", "unknown"} {
		t.Run(policy, func(t *testing.T) {
			plan, failure := planToolBatch(
				[]domain.ContentBlock{{Type: "tool_use", ToolUseID: "provider_read", ToolName: "read"}},
				indexTurnTools([]TurnTool{{Name: "read", Kind: TurnToolSelfHosted,
					Permission: domain.PermissionPolicy{Type: policy}}}),
				map[string]PlannedToolStep{"provider_read": {ToolUseEventID: "sevt_read", ToolStepID: "tstep_read"}}, true,
			)
			if policy == "unknown" {
				require.NotEmpty(t, failure)
				require.Empty(t, plan.actionDrafts)
				return
			}
			require.Empty(t, failure)
			require.Empty(t, plan.executable)
			require.Equal(t, []string{"sevt_read"}, plan.pendingActionEventIDs)
			want := "allow"
			if policy == "always_ask" {
				want = "ask"
			}
			require.Equal(t, want, plan.actionDrafts[0].Payload["evaluated_permission"])
			require.True(t, domain.IsSelfHostedToolCall(plan.actionDrafts[0].Type, plan.actionDrafts[0].Payload))
		})
	}
}

func TestCloseInterruptedProviderToolRound_PairsEveryProviderToolUse(t *testing.T) {
	turn := &workflowTurnState{
		usesProviderTranscript: true,
		transcriptDelta: []domain.Message{{
			Role: domain.RoleAssistant,
			Content: []domain.ContentBlock{
				{Type: "tool_use", ToolUseID: "provider_1", ToolName: "read"},
				{Type: "tool_use", ToolUseID: "provider_2", ToolName: "write"},
			},
		}},
		toolUseMappings: []domain.ProviderToolUseMapping{
			{
				PublicEventID: "public_prior", ProviderToolUseID: "provider_prior",
				ToolName: "grep",
			},
			{
				PublicEventID: "public_1", ProviderToolUseID: "provider_1",
				ToolName: "read",
			},
			{
				PublicEventID: "public_2", ProviderToolUseID: "provider_2",
				ToolName: "write",
			},
		},
	}
	uses := append(
		[]domain.ContentBlock(nil),
		turn.transcriptDelta[0].Content...,
	)
	completed := []domain.ContentBlock{{
		Type: "tool_result", ToolResultFor: "provider_1", Text: "done",
	}}

	closeInterruptedProviderToolRound(turn, uses, completed, 1)

	require.Len(t, turn.transcriptDelta, 2)
	require.Equal(t, domain.RoleUser, turn.transcriptDelta[1].Role)
	require.Equal(t, []domain.ContentBlock{
		{Type: "tool_result", ToolResultFor: "provider_1", Text: "done"},
		{
			Type: "tool_result", ToolResultFor: "provider_2",
			Text:    "Tool execution was interrupted before a result was committed.",
			IsError: true,
		},
	}, turn.transcriptDelta[1].Content)
	require.Equal(t, []domain.ProviderToolUseMapping{
		{
			PublicEventID: "public_prior", ProviderToolUseID: "provider_prior",
			ToolName: "grep",
		},
		{
			PublicEventID: "public_1", ProviderToolUseID: "provider_1",
			ToolName: "read",
		},
	}, turn.toolUseMappings)
}
