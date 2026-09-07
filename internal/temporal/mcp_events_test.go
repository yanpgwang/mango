package temporal

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/testsuite"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/mcpclient"
	"github.com/yanpgwang/mango/internal/model"
)

// mcpTurnTool is the pinned definition PrepareTurn produces for a discovered
// remote tool: the model sees the namespaced alias, the public event reports the
// bare name plus the server it came from.
func mcpTurnTool(permission string) TurnTool {
	return TurnTool{
		Name:       "mcp__github__list_issues",
		Kind:       TurnToolMCP,
		Permission: domain.PermissionPolicy{Type: permission},
		MCPServer: domain.MCPServer{
			Name: "github", URL: "https://mcp.example.com",
		},
		MCPToolName: "list_issues",
	}
}

func draftOfType(
	t *testing.T,
	drafts []domain.EventDraft,
	eventType string,
) domain.EventDraft {
	t.Helper()
	for _, draft := range drafts {
		if draft.Type == eventType {
			return draft
		}
	}
	t.Fatalf("no %s draft in %v", eventType, draftTypes(drafts))
	return domain.EventDraft{}
}

// An executed MCP call must publish the documented pair: agent.mcp_tool_use
// carrying mcp_server_name and the bare server-side tool name, answered by an
// agent.mcp_tool_result that correlates through mcp_tool_use_id and carries no
// server name of its own.
func TestWorkflowTurn_MCPToolRoundPublishesMcpEventPair(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			AttemptID: "ratm_mcp",
			Request: model.Request{
				Model:    "test-model",
				Messages: []domain.Message{{Role: domain.RoleUser}},
				Tools:    []model.ToolSchema{{Name: "mcp__github__list_issues"}},
			},
			Tools: []TurnTool{mcpTurnTool("always_allow")},
		}, nil
	}
	modelCalls := 0
	callModel := func(context.Context, CallModelInput) (CallModelResult, error) {
		modelCalls++
		if modelCalls == 1 {
			return CallModelResult{
				ToolSteps: []PlannedToolStep{{
					ToolUseEventID: "sevt_mcp_use",
					ToolStepID:     "tstep_mcp",
				}},
				Response: model.Response{
					StopReason: "tool_use",
					Content: []domain.ContentBlock{{
						Type:      "tool_use",
						ToolUseID: "sevt_mcp_use",
						ToolName:  "mcp__github__list_issues",
						Input:     map[string]any{"repo": "mango"},
					}},
				},
			}, nil
		}
		return CallModelResult{
			MessageEventID: "sevt_answer",
			Response: model.Response{
				StopReason: "end_turn",
				Content:    []domain.ContentBlock{{Type: "text", Text: "two issues"}},
			},
		}, nil
	}
	var executed ExecuteToolInput
	executeTool := func(_ context.Context, in ExecuteToolInput) (ExecuteToolResult, error) {
		executed = in
		return ExecuteToolResult{Result: domain.ToolStepResult{
			Content: []any{map[string]any{"type": "text", "text": "#1, #2"}},
		}}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnCompleted}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_mcp", TriggerEventID: "sevt_trigger",
	})
	require.NoError(t, env.GetWorkflowError())

	require.Equal(t, []string{
		domain.EvAgentMcpToolUse,
		domain.EvAgentMcpToolResult,
		domain.EvAgentMessage,
		domain.EvSessionStatusIdle,
	}, draftTypes(completed.Output))

	use := draftOfType(t, completed.Output, domain.EvAgentMcpToolUse)
	require.Equal(t, "sevt_mcp_use", use.ID)
	require.Equal(t, "list_issues", use.Payload["name"],
		"the public event reports the bare server-side tool name, not the alias")
	require.Equal(t, "github", use.Payload["mcp_server_name"])
	require.Equal(t, map[string]any{"repo": "mango"}, use.Payload["input"])
	require.Equal(t, "allow", use.Payload["evaluated_permission"])

	result := draftOfType(t, completed.Output, domain.EvAgentMcpToolResult)
	require.Equal(t, "sevt_mcp_use", result.Payload["mcp_tool_use_id"])
	require.NotContains(t, result.Payload, "tool_use_id",
		"an MCP result correlates only through mcp_tool_use_id")
	require.NotContains(t, result.Payload, "mcp_server_name",
		"upstream attributes a result to a server by joining back to the use event")
	require.Equal(t, false, result.Payload["is_error"])

	// The private provider call keeps the namespaced alias and the pinned server.
	require.Equal(t, "mcp__github__list_issues", executed.ToolName)
	require.Equal(t, "list_issues", executed.MCPToolName)
	require.Equal(t, "github", executed.MCPServer.Name)
}

// An always_ask MCP tool parks the run on the same requires_action barrier a
// built-in uses. The parked event is an agent.mcp_tool_use, it is named in
// stop_reason.requires_action.event_ids, and the internal pending kind stays
// tool_confirmation so a user.tool_confirmation with tool_use_id resolves it.
func TestWorkflowTurn_AlwaysAskMCPToolParksOnRequiresActionBarrier(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			AttemptID: "ratm_mcp_ask",
			Request: model.Request{
				Model:    "test-model",
				Messages: []domain.Message{{Role: domain.RoleUser}},
				Tools:    []model.ToolSchema{{Name: "mcp__github__list_issues"}},
			},
			Tools: []TurnTool{mcpTurnTool("always_ask")},
		}, nil
	}
	callModel := func(context.Context, CallModelInput) (CallModelResult, error) {
		return CallModelResult{
			ToolSteps: []PlannedToolStep{{
				ToolUseEventID: "sevt_mcp_ask", ToolStepID: "tstep_mcp_ask",
			}},
			Response: model.Response{
				StopReason: "tool_use",
				Content: []domain.ContentBlock{{
					Type:      "tool_use",
					ToolUseID: "sevt_mcp_ask",
					ToolName:  "mcp__github__list_issues",
					Input:     map[string]any{"repo": "mango"},
				}},
			},
		}, nil
	}
	executeTool := func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
		t.Fatal("an always_ask MCP tool must not execute before confirmation")
		return ExecuteToolResult{}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnCompleted}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_mcp_ask", TriggerEventID: "sevt_trigger",
	})
	require.NoError(t, env.GetWorkflowError())

	require.Equal(t, []string{
		domain.EvAgentMcpToolUse,
		domain.EvSessionStatusIdle,
	}, draftTypes(completed.Output))
	require.Equal(t, []string{"sevt_mcp_ask"}, completed.PendingActionEventIDs)

	use := draftOfType(t, completed.Output, domain.EvAgentMcpToolUse)
	require.Equal(t, "ask", use.Payload["evaluated_permission"])
	require.Equal(t, "github", use.Payload["mcp_server_name"])

	idle := draftOfType(t, completed.Output, domain.EvSessionStatusIdle)
	stopReason := idle.Payload["stop_reason"].(map[string]any)
	require.Equal(t, "requires_action", stopReason["type"])
	// The Activity payload round-trips through JSON, so the barrier ids arrive
	// as []any here; the store validates them through the same shape.
	require.Equal(t, []any{"sevt_mcp_ask"}, stopReason["event_ids"])

	// The barrier the store enforces must still derive tool_confirmation from
	// the committed event, and user.tool_confirmation must still resolve it
	// through tool_use_id.
	kind, ok := domain.PendingActionKindForEvent(use.Type, use.Payload)
	require.True(t, ok)
	require.Equal(t, domain.PendingToolConfirmation, kind)
	refID, refKind, ok := domain.ResolutionReference(
		domain.EvUserToolConfirmation,
		map[string]any{"tool_use_id": "sevt_mcp_ask", "result": "allow"},
	)
	require.True(t, ok)
	require.Equal(t, "sevt_mcp_ask", refID)
	require.Equal(t, kind, refKind)
}

// Resuming an allowed MCP confirmation executes the original call and answers
// the parked agent.mcp_tool_use with an agent.mcp_tool_result.
func TestWorkflowTurn_AllowedMCPConfirmationEmitsMcpToolResult(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			AttemptID: "ratm_mcp_resume",
			Request:   model.Request{Model: "test-model"},
			Tools:     []TurnTool{mcpTurnTool("always_ask")},
			ResumeActions: []ResumeAction{{
				ActionEventID:     "sevt_mcp_ask",
				ActionEventType:   domain.EvAgentMcpToolUse,
				Kind:              domain.PendingToolConfirmation,
				ToolName:          "mcp__github__list_issues",
				Input:             map[string]any{"repo": "mango"},
				ResolutionEventID: "sevt_confirmation",
				Confirmation:      "allow",
				ToolStepID:        "tstep_mcp_resume",
			}},
		}, nil
	}
	callModel := func(context.Context, CallModelInput) (CallModelResult, error) {
		return CallModelResult{
			MessageEventID: "sevt_answer",
			Response: model.Response{
				StopReason: "end_turn",
				Content:    []domain.ContentBlock{{Type: "text", Text: "done"}},
			},
		}, nil
	}
	executeTool := func(_ context.Context, in ExecuteToolInput) (ExecuteToolResult, error) {
		require.Equal(t, "sevt_mcp_ask", in.ToolUseEventID)
		require.Equal(t, "list_issues", in.MCPToolName)
		return ExecuteToolResult{Result: domain.ToolStepResult{
			Content: []any{map[string]any{"type": "text", "text": "#1, #2"}},
		}}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnCompleted}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID:          "sess_mcp_resume",
		TriggerEventID:     "sevt_confirmation",
		ResolutionEventIDs: []string{"sevt_confirmation"},
	})
	require.NoError(t, env.GetWorkflowError())

	require.Equal(t, []string{
		domain.EvAgentMcpToolResult,
		domain.EvAgentMessage,
		domain.EvSessionStatusIdle,
	}, draftTypes(completed.Output))
	result := completed.Output[0]
	require.Equal(t, "sevt_mcp_ask", result.Payload["mcp_tool_use_id"])
	require.NotContains(t, result.Payload, "tool_use_id")
	require.NotContains(t, result.Payload, "mcp_server_name")
}

// A denied MCP confirmation still answers the parked call, and still does so on
// the MCP result type.
func TestWorkflowTurn_DeniedMCPConfirmationEmitsMcpToolResult(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			Request: model.Request{Model: "test-model"},
			Tools:   []TurnTool{mcpTurnTool("always_ask")},
			ResumeActions: []ResumeAction{{
				ActionEventID:     "sevt_mcp_ask",
				ActionEventType:   domain.EvAgentMcpToolUse,
				Kind:              domain.PendingToolConfirmation,
				ToolName:          "mcp__github__list_issues",
				Input:             map[string]any{"repo": "mango"},
				ResolutionEventID: "sevt_confirmation",
				Confirmation:      "deny",
				DenyMessage:       "not this repo",
			}},
		}, nil
	}
	callModel := func(context.Context, CallModelInput) (CallModelResult, error) {
		return CallModelResult{Response: model.Response{StopReason: "end_turn"}}, nil
	}
	executeTool := func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
		t.Fatal("a denied confirmation must not execute the tool")
		return ExecuteToolResult{}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnCompleted}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID:          "sess_mcp_deny",
		TriggerEventID:     "sevt_confirmation",
		ResolutionEventIDs: []string{"sevt_confirmation"},
	})
	require.NoError(t, env.GetWorkflowError())

	result := draftOfType(t, completed.Output, domain.EvAgentMcpToolResult)
	require.Equal(t, "sevt_mcp_ask", result.Payload["mcp_tool_use_id"])
	require.Equal(t, true, result.Payload["is_error"])
}

// The cross-execution case the version gate cannot cover. A confirmation parked
// before the upgrade is durably an agent.tool_use; SessionWorkflow then
// continues-as-new, so the resuming execution has a fresh history and its gate
// resolves to the new version. The result must still pair with what PostgreSQL
// actually holds, so it stays agent.tool_result carrying tool_use_id.
func TestWorkflowTurn_LegacyParkResumedOnUpgradedExecutionKeepsLegacyResult(t *testing.T) {
	prepared := prepareLegacyMCPParkResume(t)
	require.Len(t, prepared.ResumeActions, 1)

	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return prepared, nil
	}
	callModel := func(context.Context, CallModelInput) (CallModelResult, error) {
		return CallModelResult{
			MessageEventID: "sevt_answer",
			Response: model.Response{
				StopReason: "end_turn",
				Content:    []domain.ContentBlock{{Type: "text", Text: "done"}},
			},
		}, nil
	}
	executeTool := func(_ context.Context, in ExecuteToolInput) (ExecuteToolResult, error) {
		require.Equal(t, "sevt_legacy_mcp_ask", in.ToolUseEventID)
		return ExecuteToolResult{Result: domain.ToolStepResult{
			Content: []any{map[string]any{"type": "text", "text": "#1, #2"}},
		}}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnCompleted}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	// A fresh execution records no marker for the change id, so the gate resolves
	// to the current version exactly as it would on an upgraded worker after
	// Continue-As-New.
	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID:          "sess_legacy_park",
		TriggerEventID:     "sevt_confirmation",
		ResolutionEventIDs: []string{"sevt_confirmation"},
	})
	require.NoError(t, env.GetWorkflowError())

	result := completed.Output[0]
	require.Equal(t, domain.EvAgentToolResult, result.Type,
		"a park durably recorded as agent.tool_use must be answered on the legacy type")
	require.Equal(t, "sevt_legacy_mcp_ask", result.Payload["tool_use_id"])
	require.NotContains(t, result.Payload, "mcp_tool_use_id",
		"mcp_tool_use_id must never point at an agent.tool_use event")
}

// prepareLegacyMCPParkResume runs the real PrepareTurn Activity over a ledger
// that parked an MCP confirmation on the legacy agent.tool_use type, which is
// what a session parked before this change durably holds.
func prepareLegacyMCPParkResume(t *testing.T) PrepareTurnResult {
	t.Helper()
	processedAt := time.Now().UTC()
	resolutionID := "sevt_confirmation"
	base := newFakeSource([]domain.Event{
		{
			ID: "sevt_user", Sequence: 1, Type: domain.EvUserMessage,
			Payload: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "check github"},
			}},
			ProcessedAt: &processedAt,
		},
		{
			ID: "sevt_legacy_mcp_ask", Sequence: 2, Type: domain.EvAgentToolUse,
			Payload: map[string]any{
				"name":                 "list_issues",
				"mcp_server_name":      "github",
				"input":                map[string]any{"repo": "mango"},
				"evaluated_permission": "ask",
			},
		},
		{
			ID: resolutionID, Sequence: 3, Type: domain.EvUserToolConfirmation,
			Payload: map[string]any{
				"tool_use_id": "sevt_legacy_mcp_ask",
				"result":      "allow",
			},
			ProcessedAt: &processedAt,
		},
	})
	base.pendingActions = []domain.PendingAction{{
		ID:               "pact_legacy",
		SessionID:        "sess_legacy_park",
		ActionEventID:    "sevt_legacy_mcp_ask",
		Kind:             domain.PendingToolConfirmation,
		ResolvingEventID: &resolutionID,
	}}
	source := &mcpPrepareSource{
		fakeSource: base,
		session: domain.Session{
			ID:     "sess_legacy_park",
			Status: domain.StatusRunning,
			AgentSnapshot: domain.Agent{
				Model: domain.Model{ID: "model"},
				MCPServers: []any{map[string]any{
					"type": "url", "name": "github",
					"url": "https://mcp.example.com",
				}},
				Tools: []any{map[string]any{
					"type": "mcp_toolset", "mcp_server_name": "github",
					"permission": map[string]any{"type": "always_ask"},
				}},
			},
		},
	}
	client := &fakeMCPClient{tools: []mcpclient.Tool{{
		Name:        "list_issues",
		InputSchema: map[string]any{"type": "object"},
	}}}

	prepared, err := NewActivities(
		nil, source, nil, &testIDGen{},
	).WithMCPClient(client).PrepareTurn(context.Background(), PrepareTurnInput{
		SessionID:          "sess_legacy_park",
		TriggerEventID:     resolutionID,
		ResolutionEventIDs: []string{resolutionID},
	})
	require.NoError(t, err)
	require.Empty(t, prepared.FatalError)
	return prepared
}

// A Workflow history recorded before the mcp-tool-event-types change replays
// with the gate closed and must keep emitting the legacy pair, so its already
// published ledger stays internally consistent.
func TestPlanToolBatch_LegacyHistoryKeepsMCPOnToolUse(t *testing.T) {
	uses := []domain.ContentBlock{{
		Type: "tool_use", ToolUseID: "provider_mcp",
		ToolName: "mcp__github__list_issues",
		Input:    map[string]any{"repo": "mango"},
	}}
	tools := indexTurnTools([]TurnTool{mcpTurnTool("always_allow")})
	steps := map[string]PlannedToolStep{"provider_mcp": {
		ToolUseEventID: "sevt_mcp", ProviderToolUseID: "provider_mcp",
		ToolStepID: "tstep_mcp",
	}}

	legacy, failure := planToolBatch(uses, tools, steps, false)
	require.Empty(t, failure)
	require.Equal(t, domain.EvAgentToolUse, legacy.actionDrafts[0].Type)
	require.Equal(t, "list_issues", legacy.actionDrafts[0].Payload["name"])
	require.Equal(t, "github", legacy.actionDrafts[0].Payload["mcp_server_name"])
	// The executable entry carries the type it just committed, so the result the
	// same round produces cannot drift from the use event.
	require.Equal(t, domain.EvAgentToolUse, legacy.executable[0].useEventType)

	current, failure := planToolBatch(uses, tools, steps, true)
	require.Empty(t, failure)
	require.Equal(t, domain.EvAgentMcpToolUse, current.actionDrafts[0].Type)
	require.Equal(t, domain.EvAgentMcpToolUse, current.executable[0].useEventType)

	legacyResult := toolResultDraft(domain.EvAgentToolUse, "sevt_mcp", nil, false)
	require.Equal(t, domain.EvAgentToolResult, legacyResult.Type)
	require.Equal(t, "sevt_mcp", legacyResult.Payload["tool_use_id"])

	currentResult := toolResultDraft(domain.EvAgentMcpToolUse, "sevt_mcp", nil, false)
	require.Equal(t, domain.EvAgentMcpToolResult, currentResult.Type)
	require.Equal(t, "sevt_mcp", currentResult.Payload["mcp_tool_use_id"])

	// Built-ins are untouched by the gate in either direction.
	builtin := TurnTool{
		Name: "bash", Kind: TurnToolBuiltin,
		Permission: domain.PermissionPolicy{Type: "always_allow"},
	}
	require.Equal(t, domain.EvAgentToolUse, serverToolUseType(builtin, true))
	require.Equal(
		t,
		domain.EvAgentToolResult,
		toolResultDraft(serverToolUseType(builtin, true), "sevt_bash", nil, false).Type,
	)
}

// The aliased model-facing tool name must still be reconstructable from the
// committed agent.mcp_tool_use payload, so a resumed confirmation maps back to
// the pinned MCP definition on the next model request.
func TestPrepareTurn_ResumedMCPConfirmationRebuildsAliasedToolName(t *testing.T) {
	processedAt := time.Now().UTC()
	resolutionID := "sevt_confirmation"
	base := newFakeSource([]domain.Event{
		{
			ID: "sevt_user", Sequence: 1, Type: domain.EvUserMessage,
			Payload: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "check github"},
			}},
			ProcessedAt: &processedAt,
		},
		{
			ID: "sevt_mcp_ask", Sequence: 2, Type: domain.EvAgentMcpToolUse,
			Payload: map[string]any{
				"name":                 "list_issues",
				"mcp_server_name":      "github",
				"input":                map[string]any{"repo": "mango"},
				"evaluated_permission": "ask",
			},
		},
		{
			ID: resolutionID, Sequence: 3, Type: domain.EvUserToolConfirmation,
			Payload: map[string]any{
				"tool_use_id": "sevt_mcp_ask",
				"result":      "allow",
			},
			ProcessedAt: &processedAt,
		},
	})
	base.pendingActions = []domain.PendingAction{{
		ID:               "pact_mcp",
		SessionID:        "sess_mcp_resume",
		ActionEventID:    "sevt_mcp_ask",
		Kind:             domain.PendingToolConfirmation,
		ResolvingEventID: &resolutionID,
	}}
	source := &mcpPrepareSource{
		fakeSource: base,
		session: domain.Session{
			ID:     "sess_mcp_resume",
			Status: domain.StatusRunning,
			AgentSnapshot: domain.Agent{
				Model: domain.Model{ID: "model"},
				MCPServers: []any{map[string]any{
					"type": "url", "name": "github",
					"url": "https://mcp.example.com",
				}},
				Tools: []any{map[string]any{
					"type": "mcp_toolset", "mcp_server_name": "github",
				}},
			},
		},
	}
	client := &fakeMCPClient{tools: []mcpclient.Tool{{
		Name:        "list_issues",
		InputSchema: map[string]any{"type": "object"},
	}}}

	prepared, err := NewActivities(
		nil, source, nil, &testIDGen{},
	).WithMCPClient(client).PrepareTurn(context.Background(), PrepareTurnInput{
		SessionID:          "sess_mcp_resume",
		TriggerEventID:     resolutionID,
		ResolutionEventIDs: []string{resolutionID},
	})

	require.NoError(t, err)
	require.Empty(t, prepared.FatalError)
	require.Len(t, prepared.ResumeActions, 1)
	require.Equal(
		t,
		domain.EvAgentMcpToolUse,
		prepared.ResumeActions[0].ActionEventType,
		"the durable park type must reach the Workflow so the result can pair with it",
	)
	require.Equal(
		t,
		"mcp__github__list_issues",
		prepared.ResumeActions[0].ToolName,
		"the bare name plus mcp_server_name must rebuild the model-facing alias",
	)
	require.Equal(
		t,
		domain.PendingToolConfirmation,
		prepared.ResumeActions[0].Kind,
	)
	require.Equal(t, "allow", prepared.ResumeActions[0].Confirmation)

	// The rebuilt name must resolve against the pinned tool set, otherwise the
	// resume round would fail with "names a tool that is not enabled".
	definition, ok := indexTurnTools(prepared.Tools)[prepared.ResumeActions[0].ToolName]
	require.True(t, ok)
	require.Equal(t, TurnToolMCP, definition.Kind)
	require.Equal(t, "list_issues", definition.MCPToolName)
}

// An execution that is mid-turn when the worker is upgraded replays a
// PrepareTurn result the previous binary recorded, which has no
// action_event_type field. The zero value must mean the legacy agent.tool_use
// spelling, because that is what every park written before this change is.
func TestResumeAction_MissingActionEventTypeMeansLegacyPair(t *testing.T) {
	recordedByPreviousBinary := []byte(`{
		"action_event_id": "sevt_park",
		"kind": "tool_confirmation",
		"tool_name": "mcp__github__list_issues",
		"input": {"repo": "mango"},
		"resolution_event_id": "sevt_confirmation",
		"confirmation": "allow",
		"tool_step_id": "tstep_resume"
	}`)

	var action ResumeAction
	require.NoError(t, json.Unmarshal(recordedByPreviousBinary, &action))
	require.Empty(t, action.ActionEventType)

	draft := toolResultDraft(
		action.ActionEventType,
		action.ActionEventID,
		nil,
		false,
	)
	require.Equal(t, domain.EvAgentToolResult, draft.Type)
	require.Equal(t, "sevt_park", draft.Payload["tool_use_id"])
}
