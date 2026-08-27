package temporal

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/yanpgwang/mango/internal/agentruntime"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

func workflowTurnHarness(ctx workflow.Context, in PrepareTurnInput) (RunTurnResult, error) {
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Millisecond,
			BackoffCoefficient: 1,
			MaximumInterval:    time.Millisecond,
		},
	})
	return runWorkflowTurnWithResolutions(
		actx,
		in.SessionID,
		in.TriggerEventID,
		in.ResolutionEventIDs,
	)
}

func registerWorkflowTurnActivities(
	env *testsuite.TestWorkflowEnvironment,
	prepare func(context.Context, PrepareTurnInput) (PrepareTurnResult, error),
	callModel func(context.Context, CallModelInput) (CallModelResult, error),
	executeTool func(context.Context, ExecuteToolInput) (ExecuteToolResult, error),
	complete func(context.Context, CompleteWorkflowTurnInput) (RunTurnResult, error),
) {
	var flushedMu sync.Mutex
	var flushed []domain.EventDraft
	env.RegisterActivityWithOptions(prepare, activity.RegisterOptions{Name: ActivityPrepareTurn})
	env.RegisterActivityWithOptions(
		func(context.Context, StartModelRequestInput) error { return nil },
		activity.RegisterOptions{Name: ActivityStartModelRequest},
	)
	registerBudgetTestActivities(env)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in AppendWorkflowEventsInput) error {
			flushedMu.Lock()
			defer flushedMu.Unlock()
			flushed = append(flushed, in.Events...)
			return nil
		},
		activity.RegisterOptions{Name: ActivityAppendWorkflowEvents},
	)
	env.RegisterActivityWithOptions(callModel, activity.RegisterOptions{Name: ActivityCallModel})
	env.RegisterActivityWithOptions(executeTool, activity.RegisterOptions{Name: ActivityExecuteTool})
	env.RegisterActivityWithOptions(
		func(ctx context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
			// Most tests assert the logical public turn rather than the physical
			// Activity batch. Recombine intermediate prefixes for those assertions.
			flushedMu.Lock()
			prefix := append([]domain.EventDraft(nil), flushed...)
			flushedMu.Unlock()
			in.Output = append(prefix, in.Output...)
			return complete(ctx, in)
		},
		activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn},
	)
}

func registerBudgetTestActivities(env *testsuite.TestWorkflowEnvironment) {
	env.RegisterActivityWithOptions(
		func(context.Context, AdmitModelRequestInput) (AdmitModelRequestResult, error) {
			return AdmitModelRequestResult{Allowed: true}, nil
		},
		activity.RegisterOptions{Name: ActivityAdmitModelRequest},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, AccountModelRequestInput) error { return nil },
		activity.RegisterOptions{Name: ActivityAccountModelRequest},
	)
}

func TestWorkflowTurn_PublishesPrimaryPreviewsOnSessionStream(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	registerWorkflowTurnActivities(
		env,
		func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
			return PrepareTurnResult{
				ThreadID: "sthr_primary",
				Request:  model.Request{Model: "test-model"},
			}, nil
		},
		func(_ context.Context, in CallModelInput) (CallModelResult, error) {
			require.Empty(t, in.ThreadID)
			return CallModelResult{
				ModelRequestStartID: in.ModelRequestStartID,
				ModelRequestEndID:   in.ModelRequestEndID,
				MessageEventID:      "sevt_primary_message",
				Response: model.Response{
					StopReason: "end_turn",
					Content:    []domain.ContentBlock{{Type: "text", Text: "done"}},
				},
			}, nil
		},
		func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
			return ExecuteToolResult{}, nil
		},
		func(context.Context, CompleteWorkflowTurnInput) (RunTurnResult, error) {
			return RunTurnResult{Disposition: TurnCompleted}, nil
		},
	)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sesn_primary_preview", TriggerEventID: "sevt_user",
	})
	require.NoError(t, env.GetWorkflowError())
}

func TestWorkflowTurn_AdmitsAndAccountsEveryModelRequestBeforeCompletion(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	var mu sync.Mutex
	var operations []string
	var accounted AccountModelRequestInput
	var completed CompleteWorkflowTurnInput
	record := func(value string) {
		mu.Lock()
		defer mu.Unlock()
		operations = append(operations, value)
	}
	env.RegisterActivityWithOptions(
		func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
			return PrepareTurnResult{
				ThreadID: "sthr_usage",
				Request:  model.Request{Model: "claude-opus-4-8"},
			}, nil
		},
		activity.RegisterOptions{Name: ActivityPrepareTurn},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, AdmitModelRequestInput) (AdmitModelRequestResult, error) {
			record("admit")
			return AdmitModelRequestResult{Allowed: true}, nil
		},
		activity.RegisterOptions{Name: ActivityAdmitModelRequest},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, StartModelRequestInput) error {
			record("start")
			return nil
		},
		activity.RegisterOptions{Name: ActivityStartModelRequest},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in CallModelInput) (CallModelResult, error) {
			record("call")
			return CallModelResult{
				ModelRequestStartID: in.ModelRequestStartID,
				ModelRequestEndID:   in.ModelRequestEndID,
				MessageEventID:      "sevt_usage_message",
				Response: model.Response{
					StopReason: "end_turn",
					Content:    []domain.ContentBlock{{Type: "text", Text: "done"}},
					Usage: domain.TokenUsage{
						InputTokens: 7, OutputTokens: 3,
						ServerToolUse: domain.ServerToolUsage{WebSearchRequests: 1},
					},
				},
			}, nil
		},
		activity.RegisterOptions{Name: ActivityCallModel},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in AccountModelRequestInput) error {
			mu.Lock()
			operations = append(operations, "account")
			accounted = in
			mu.Unlock()
			return nil
		},
		activity.RegisterOptions{Name: ActivityAccountModelRequest},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
			return ExecuteToolResult{}, nil
		},
		activity.RegisterOptions{Name: ActivityExecuteTool},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
			mu.Lock()
			operations = append(operations, "complete")
			completed = in
			mu.Unlock()
			return RunTurnResult{Disposition: TurnCompleted}, nil
		},
		activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn},
	)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sesn_usage", TriggerEventID: "sevt_usage_trigger",
	})
	require.NoError(t, env.GetWorkflowError())
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"admit", "start", "call", "account", "complete"}, operations)
	require.Equal(t, "sesn_usage", accounted.SessionID)
	require.Equal(t, "sthr_usage", accounted.ThreadID)
	require.NotEmpty(t, accounted.RequestEventID)
	require.Equal(t, "claude-opus-4-8", accounted.Model.ID)
	require.Equal(t, "end_turn", accounted.StopReason)
	require.Equal(t, int64(7), accounted.Usage.InputTokens)
	require.Equal(t, int64(1), accounted.Usage.ServerToolUse.WebSearchRequests)
	require.True(t, completed.UsageAlreadyAccounted)
	require.Equal(t, accounted.Usage, completed.Usage)
}

func TestWorkflowTurn_AdvisorIsPrivatePortableToolWithIndependentRequest(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	var mu sync.Mutex
	modelCalls := 0
	var advisorInput ExecuteToolInput
	registerWorkflowTurnActivities(
		env,
		func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
			return PrepareTurnResult{
				AttemptID: "ratm_advisor_workflow", ThreadID: "sthr_primary",
				Request: model.Request{
					Model:  "executor-model",
					System: "executor system",
					Tools:  []model.ToolSchema{agentruntime.AdvisorToolSchema()},
					Messages: []domain.Message{{
						Role:    domain.RoleUser,
						Content: []domain.ContentBlock{{Type: "text", Text: "design the shutdown path"}},
					}},
				},
				Tools: []TurnTool{{
					Name: agentruntime.AdvisorToolName, Kind: TurnToolAdvisor,
					Permission: domain.PermissionPolicy{Type: "always_allow"},
					Model:      "reviewer-model",
				}},
			}, nil
		},
		func(_ context.Context, in CallModelInput) (CallModelResult, error) {
			mu.Lock()
			defer mu.Unlock()
			modelCalls++
			if modelCalls == 1 {
				return CallModelResult{
					ModelRequestStartID: in.ModelRequestStartID,
					ModelRequestEndID:   in.ModelRequestEndID,
					Response: model.Response{
						StopReason: "tool_use",
						Content: []domain.ContentBlock{{
							Type: "tool_use", ToolUseID: "toolu_advisor",
							ToolName: agentruntime.AdvisorToolName, Input: map[string]any{},
						}},
					},
					ToolSteps: []PlannedToolStep{{
						ToolUseEventID: "sevt_advisor_private", ProviderToolUseID: "toolu_advisor",
						ToolStepID: "tstep_advisor_workflow",
					}},
				}, nil
			}
			last := in.Request.Messages[len(in.Request.Messages)-1]
			require.Equal(t, domain.RoleUser, last.Role)
			require.Equal(t, "tool_result", last.Content[0].Type)
			require.Equal(t, "toolu_advisor", last.Content[0].ToolResultFor)
			require.Contains(t, last.Content[0].Text, "shutdown race")
			return CallModelResult{
				ModelRequestStartID: in.ModelRequestStartID,
				ModelRequestEndID:   in.ModelRequestEndID,
				MessageEventID:      "sevt_final",
				Response: model.Response{
					StopReason: "end_turn",
					Content:    []domain.ContentBlock{{Type: "text", Text: "final answer"}},
				},
			}, nil
		},
		func(_ context.Context, in ExecuteToolInput) (ExecuteToolResult, error) {
			advisorInput = in
			return ExecuteToolResult{Result: domain.ToolStepResult{
				Content: []any{map[string]any{
					"type": "text", "text": "check the shutdown race",
				}},
			}}, nil
		},
		func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
			for _, draft := range in.Output {
				if draft.Type == domain.EvAgentToolUse || draft.Type == domain.EvAgentToolResult {
					t.Fatalf("advisor leaked generic public tool event: %+v", draft)
				}
			}
			return RunTurnResult{Disposition: TurnCompleted}, nil
		},
	)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sesn_advisor_workflow", TriggerEventID: "sevt_trigger",
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 2, modelCalls)
	require.Equal(t, TurnToolAdvisor, advisorInput.ToolKind)
	require.Equal(t, "reviewer-model", advisorInput.AdvisorRequest.Model)
	require.Empty(t, advisorInput.AdvisorRequest.Tools)
	require.Contains(
		t,
		advisorInput.AdvisorRequest.Messages[0].Content[0].Text,
		"design the shutdown path",
	)
	require.Contains(
		t,
		advisorInput.AdvisorRequest.Messages[0].Content[0].Text,
		"toolu_advisor",
	)
	require.True(t, strings.HasPrefix(
		advisorInput.AdvisorConsultation.ThreadID,
		domain.PrefixSessionThread,
	))
	require.Len(t, advisorInput.AdvisorConsultation.LifecycleIDs, 9)
}

func TestWorkflowTurn_EmitsContextCompactionOnOwningThread(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	var completed CompleteWorkflowTurnInput
	registerWorkflowTurnActivities(
		env,
		func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
			return PrepareTurnResult{
				ThreadID: "sthr_child", IsChild: true,
				Request: model.Request{
					Model: "test-model",
					Messages: []domain.Message{{
						Role: domain.RoleUser,
						Content: []domain.ContentBlock{{
							Type: "text", Text: "compacted input",
						}},
					}},
				},
				ContextProjection: domain.ContextProjection{Compacted: true},
				ContextSnapshotID: "csnp_child",
			}, nil
		},
		func(_ context.Context, in CallModelInput) (CallModelResult, error) {
			return CallModelResult{
				ModelRequestStartID: in.ModelRequestStartID,
				ModelRequestEndID:   in.ModelRequestEndID,
				MessageEventID:      "sevt_child_message",
				Response: model.Response{
					StopReason: "end_turn",
					Content: []domain.ContentBlock{{
						Type: "text", Text: "done",
					}},
				},
			}, nil
		},
		func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
			return ExecuteToolResult{}, nil
		},
		func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
			completed = in
			return RunTurnResult{Disposition: TurnCompleted}, nil
		},
	)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sesn_child_compaction", TriggerEventID: "sevt_child_input",
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{
		domain.EvAgentThreadContextCompacted,
		domain.EvAgentMessage,
		domain.EvSpanModelRequestEnd,
		domain.EvSessionThreadStatusIdle,
	}, draftTypes(completed.Output))
	require.Equal(t, "sthr_child", completed.ThreadID)
	require.True(t, completed.IsChild)
}

func TestWorkflowTurn_PersistsEachModelStartBeforeCallAndPreservesRoundOrder(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	var mu sync.Mutex
	var operations []string
	var starts []StartModelRequestInput
	var calls []CallModelInput
	var flushed []domain.EventDraft
	var completed CompleteWorkflowTurnInput
	record := func(operation string) {
		mu.Lock()
		defer mu.Unlock()
		operations = append(operations, operation)
	}

	env.RegisterActivityWithOptions(
		func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
			return PrepareTurnResult{
				AttemptID: "ratm_span_order",
				Request: model.Request{
					Model: "test-model",
					Messages: []domain.Message{{
						Role: domain.RoleUser,
						Content: []domain.ContentBlock{{
							Type: "text", Text: "use the tool",
						}},
					}},
					Tools: []model.ToolSchema{{Name: "read"}},
				},
				Tools: []TurnTool{{
					Name: "read", Kind: TurnToolBuiltin,
					Permission: domain.PermissionPolicy{Type: "always_allow"},
				}},
			}, nil
		},
		activity.RegisterOptions{Name: ActivityPrepareTurn},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in StartModelRequestInput) error {
			record("start")
			mu.Lock()
			defer mu.Unlock()
			starts = append(starts, in)
			return nil
		},
		activity.RegisterOptions{Name: ActivityStartModelRequest},
	)
	registerBudgetTestActivities(env)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in CallModelInput) (CallModelResult, error) {
			record("call")
			mu.Lock()
			calls = append(calls, in)
			call := len(calls)
			mu.Unlock()
			if call == 1 {
				return CallModelResult{
					ModelRequestStartID: in.ModelRequestStartID,
					ModelRequestEndID:   in.ModelRequestEndID,
					ToolSteps: []PlannedToolStep{{
						ToolUseEventID: "sevt_tool_order",
						ToolStepID:     "tstep_order",
					}},
					Response: model.Response{
						StopReason: "tool_use",
						Content: []domain.ContentBlock{{
							Type: "tool_use", ToolUseID: "sevt_tool_order",
							ToolName: "read", Input: map[string]any{"path": "a.txt"},
						}},
					},
				}, nil
			}
			return CallModelResult{
				ModelRequestStartID: in.ModelRequestStartID,
				ModelRequestEndID:   in.ModelRequestEndID,
				MessageEventID:      "sevt_final_order",
				Response: model.Response{
					StopReason: "end_turn",
					Content:    []domain.ContentBlock{{Type: "text", Text: "done"}},
				},
			}, nil
		},
		activity.RegisterOptions{Name: ActivityCallModel},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
			record("tool")
			return ExecuteToolResult{Result: domain.ToolStepResult{
				Content: []any{map[string]any{"type": "text", "text": "contents"}},
			}}, nil
		},
		activity.RegisterOptions{Name: ActivityExecuteTool},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in AppendWorkflowEventsInput) error {
			record("append")
			mu.Lock()
			defer mu.Unlock()
			flushed = append(flushed, in.Events...)
			return nil
		},
		activity.RegisterOptions{Name: ActivityAppendWorkflowEvents},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
			record("complete")
			mu.Lock()
			defer mu.Unlock()
			completed = in
			return RunTurnResult{}, nil
		},
		activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn},
	)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_span_order", TriggerEventID: "sevt_trigger_order",
	})
	require.NoError(t, env.GetWorkflowError())

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{
		"start", "call", "tool", "append", "start", "call", "complete",
	}, operations)
	require.Len(t, starts, 2)
	require.Len(t, calls, 2)
	require.NotEmpty(t, starts[0].ModelRequestStartID)
	require.NotEqual(t, starts[0].ModelRequestStartID, starts[1].ModelRequestStartID)
	for i := range starts {
		require.Equal(t, "sess_span_order", starts[i].SessionID)
		require.Equal(t, "sevt_trigger_order", starts[i].TriggerEventID)
		require.Equal(t, starts[i].ModelRequestStartID, calls[i].ModelRequestStartID)
	}
	require.Equal(t, []string{
		domain.EvSpanModelRequestEnd,
		domain.EvAgentToolUse,
		domain.EvAgentToolResult,
	}, draftTypes(flushed))
	for _, draft := range flushed {
		require.NotEmpty(t, draft.ID)
	}
	require.Equal(t, []string{
		domain.EvAgentMessage,
		domain.EvSpanModelRequestEnd,
		domain.EvSessionStatusIdle,
	}, draftTypes(completed.Output))
}

func TestWorkflowTurn_PreservesTextAndMultipleTools(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	initial := []domain.Message{{
		Role: domain.RoleUser,
		Content: []domain.ContentBlock{{
			Type: "text", Text: "inspect both",
		}},
	}}
	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			AttemptID: "ratm_1",
			Request: model.Request{
				Model:    "test-model",
				Messages: initial,
				Tools: []model.ToolSchema{
					{Name: "read", InputSchema: map[string]any{"type": "object"}},
					{Name: "grep", InputSchema: map[string]any{"type": "object"}},
				},
			},
			Tools: []TurnTool{
				{Name: "read", Kind: TurnToolBuiltin, Permission: domain.PermissionPolicy{Type: "always_allow"}},
				{Name: "grep", Kind: TurnToolBuiltin, Permission: domain.PermissionPolicy{Type: "always_allow"}},
			},
		}, nil
	}

	var mu sync.Mutex
	var modelRequests []model.Request
	callModel := func(_ context.Context, in CallModelInput) (CallModelResult, error) {
		mu.Lock()
		modelRequests = append(modelRequests, in.Request)
		call := len(modelRequests)
		mu.Unlock()
		if call == 1 {
			return CallModelResult{
				MessageEventID: "sevt_text_1",
				ToolSteps: []PlannedToolStep{
					{ToolUseEventID: "sevt_tool_1", ToolStepID: "tstep_1"},
					{ToolUseEventID: "sevt_tool_2", ToolStepID: "tstep_2"},
				},
				Response: model.Response{
					StopReason: "tool_use",
					Content: []domain.ContentBlock{
						{Type: "text", Text: "I will inspect both files."},
						{Type: "tool_use", ToolUseID: "sevt_tool_1", ToolName: "read", Input: map[string]any{"path": "a.txt"}},
						{Type: "tool_use", ToolUseID: "sevt_tool_2", ToolName: "grep", Input: map[string]any{"pattern": "x"}},
					},
				},
			}, nil
		}
		return CallModelResult{
			MessageEventID: "sevt_text_2",
			Response: model.Response{
				StopReason: "end_turn",
				Content:    []domain.ContentBlock{{Type: "text", Text: "done"}},
			},
		}, nil
	}

	var toolCalls []ExecuteToolInput
	executeTool := func(_ context.Context, in ExecuteToolInput) (ExecuteToolResult, error) {
		mu.Lock()
		toolCalls = append(toolCalls, in)
		mu.Unlock()
		return ExecuteToolResult{
			Result: domain.ToolStepResult{
				Content: []any{map[string]any{"type": "text", "text": in.ToolName + " result"}},
			},
		}, nil
	}

	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		mu.Lock()
		completed = in
		mu.Unlock()
		return RunTurnResult{}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_1", TriggerEventID: "sevt_trigger",
	})
	require.NoError(t, env.GetWorkflowError())

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, modelRequests, 2)
	require.Len(t, toolCalls, 2)

	postTool := modelRequests[1].Messages
	require.Len(t, postTool, 3)
	require.Equal(t, domain.RoleAssistant, postTool[1].Role)
	require.Equal(t, []domain.ContentBlock{
		{Type: "text", Text: "I will inspect both files."},
		{Type: "tool_use", ToolUseID: "sevt_tool_1", ToolName: "read", Input: map[string]any{"path": "a.txt"}},
		{Type: "tool_use", ToolUseID: "sevt_tool_2", ToolName: "grep", Input: map[string]any{"pattern": "x"}},
	}, postTool[1].Content, "assistant text and both tool uses must stay in one model round")
	require.Equal(t, domain.RoleUser, postTool[2].Role)
	require.Equal(t, []domain.ContentBlock{
		{Type: "tool_result", ToolResultFor: "sevt_tool_1", Text: "read result"},
		{Type: "tool_result", ToolResultFor: "sevt_tool_2", Text: "grep result"},
	}, postTool[2].Content)

	var eventTypes []string
	for _, draft := range completed.Output {
		eventTypes = append(eventTypes, draft.Type)
	}
	require.Equal(t, []string{
		domain.EvAgentMessage,
		domain.EvAgentToolUse,
		domain.EvAgentToolUse,
		domain.EvAgentToolResult,
		domain.EvAgentToolResult,
		domain.EvAgentMessage,
		domain.EvSessionStatusIdle,
	}, eventTypes)
	require.Equal(t, "ratm_1", completed.AttemptID)
	require.Equal(t, domain.RunAttemptCompleted, completed.AttemptState)
}

func TestWorkflowTurn_RuntimeSkillInjectsFullBodyAndSuppressesDuplicate(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	initial := []domain.Message{{
		Role: domain.RoleUser,
		Content: []domain.ContentBlock{{
			Type: "text", Text: "analyze the report",
		}},
	}}
	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			AttemptID:              "ratm_skill",
			UsesProviderTranscript: true,
			TranscriptDelta:        initial,
			Request: model.Request{
				Model:    "test-model",
				Messages: initial,
				Tools:    []model.ToolSchema{agentruntime.RuntimeSkillToolSchema()},
			},
			Tools: []TurnTool{{
				Name: agentruntime.RuntimeSkillToolName, Kind: TurnToolRuntimeSkill,
				Permission: domain.PermissionPolicy{Type: "always_allow"},
			}},
		}, nil
	}

	const (
		reportSkill = "---\nname: report-tools\ndescription: Analyze reports\n---\n\nUse the canonical report workflow.\n"
		chartSkill  = "---\nname: chart-tools\ndescription: Build charts\n---\n\nUse the canonical chart workflow.\n"
	)
	countInjectedBodies := func(messages []domain.Message) int {
		count := 0
		for _, message := range messages {
			for _, block := range message.Content {
				if block.Type == "text" &&
					(strings.Contains(block.Text, reportSkill) || strings.Contains(block.Text, chartSkill)) {
					count++
				}
			}
		}
		return count
	}

	var mu sync.Mutex
	var modelRequests []model.Request
	callModel := func(_ context.Context, in CallModelInput) (CallModelResult, error) {
		mu.Lock()
		modelRequests = append(modelRequests, in.Request)
		call := len(modelRequests)
		mu.Unlock()
		switch call {
		case 1:
			return CallModelResult{
				ToolSteps: []PlannedToolStep{
					{
						ToolUseEventID: "public_skill_1", ProviderToolUseID: "provider_skill_1",
						ToolStepID: "tstep_skill_1",
					},
					{
						ToolUseEventID: "public_skill_chart", ProviderToolUseID: "provider_skill_chart",
						ToolStepID: "tstep_skill_chart",
					},
				},
				Response: model.Response{
					StopReason: "tool_use",
					Content: []domain.ContentBlock{
						{
							Type: "tool_use", ToolUseID: "provider_skill_1",
							ToolName: agentruntime.RuntimeSkillToolName,
							Input:    map[string]any{"skill": "report-tools"},
						},
						{
							Type: "tool_use", ToolUseID: "provider_skill_chart",
							ToolName: agentruntime.RuntimeSkillToolName,
							Input:    map[string]any{"skill": "chart-tools"},
						},
					},
				},
			}, nil
		case 2:
			require.Equal(t, 2, countInjectedBodies(in.Request.Messages))
			blocks := in.Request.Messages[len(in.Request.Messages)-1].Content
			require.Equal(t, []string{"tool_result", "tool_result", "text", "text"}, []string{
				blocks[0].Type, blocks[1].Type, blocks[2].Type, blocks[3].Type,
			})
			return CallModelResult{
				ToolSteps: []PlannedToolStep{{
					ToolUseEventID: "public_skill_2", ProviderToolUseID: "provider_skill_2",
					ToolStepID: "tstep_skill_2",
				}},
				Response: model.Response{
					StopReason: "tool_use",
					Content: []domain.ContentBlock{{
						Type: "tool_use", ToolUseID: "provider_skill_2",
						ToolName: agentruntime.RuntimeSkillToolName,
						Input:    map[string]any{"skill": "report-tools"},
					}},
				},
			}, nil
		default:
			require.Equal(t, 2, countInjectedBodies(in.Request.Messages))
			return CallModelResult{
				MessageEventID: "sevt_skill_answer",
				Response: model.Response{
					StopReason: "end_turn",
					Content: []domain.ContentBlock{{
						Type: "text", Text: "report analyzed",
					}},
				},
			}, nil
		}
	}

	var toolCalls []ExecuteToolInput
	executeTool := func(_ context.Context, in ExecuteToolInput) (ExecuteToolResult, error) {
		mu.Lock()
		toolCalls = append(toolCalls, in)
		mu.Unlock()
		name, err := agentruntime.RuntimeSkillName(in.Input)
		require.NoError(t, err)
		result := domain.ToolStepResult{Content: []any{map[string]any{
			"type": "text", "text": "Launching skill: " + name,
		}}}
		if in.SkillAlreadyLoaded {
			result.Content = []any{map[string]any{
				"type": "text", "text": "Skill " + name + " is already loaded",
			}}
		} else {
			body := reportSkill
			if name == "chart-tools" {
				body = chartSkill
			}
			result.InjectedContent = []domain.ContentBlock{
				agentruntime.RuntimeSkillInjection(name, []byte(body)),
			}
		}
		return ExecuteToolResult{Result: result}, nil
	}

	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_skill", TriggerEventID: "sevt_trigger",
	})
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, modelRequests, 3)
	require.Len(t, toolCalls, 3)
	require.False(t, toolCalls[0].SkillAlreadyLoaded)
	require.False(t, toolCalls[1].SkillAlreadyLoaded)
	require.True(t, toolCalls[2].SkillAlreadyLoaded)
	require.Equal(t, 2, countInjectedBodies(completed.TranscriptDelta))
	require.Len(t, completed.ToolUseMappings, 3)
	for _, mapping := range completed.ToolUseMappings {
		require.Equal(t, agentruntime.RuntimeSkillToolName, mapping.ToolName)
	}
}

func TestWorkflowTurn_MixedExecutableAndPendingToolsCommitExecutedTranscriptResult(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	initial := []domain.Message{{
		Role: domain.RoleUser,
		Content: []domain.ContentBlock{{
			Type: "text", Text: "read then ask",
		}},
	}}
	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			AttemptID:              "ratm_mixed",
			UsesProviderTranscript: true,
			TranscriptDelta:        initial,
			Request: model.Request{
				Model:    "test-model",
				Messages: initial,
				Tools: []model.ToolSchema{
					{Name: "read"},
					{Name: "ask_client"},
				},
			},
			Tools: []TurnTool{
				{
					Name: "read", Kind: TurnToolBuiltin,
					Permission: domain.PermissionPolicy{Type: "always_allow"},
				},
				{Name: "ask_client", Kind: TurnToolCustom},
			},
		}, nil
	}
	callModel := func(context.Context, CallModelInput) (CallModelResult, error) {
		return CallModelResult{
			ToolSteps: []PlannedToolStep{
				{
					ToolUseEventID: "public_read", ProviderToolUseID: "provider_read",
					ToolStepID: "tstep_read",
				},
				{
					ToolUseEventID: "public_ask", ProviderToolUseID: "provider_ask",
					ToolStepID: "tstep_ask",
				},
			},
			Response: model.Response{StopReason: "tool_use", Content: []domain.ContentBlock{
				{
					Type: "tool_use", ToolUseID: "provider_read", ToolName: "read",
					Input: map[string]any{"path": "a.txt"},
				},
				{
					Type: "tool_use", ToolUseID: "provider_ask", ToolName: "ask_client",
					Input: map[string]any{"question": "continue?"},
				},
			}},
		}, nil
	}
	executeTool := func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
		return ExecuteToolResult{Result: domain.ToolStepResult{
			Content: []any{map[string]any{
				"type": "text", "text": "file contents",
			}},
		}}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnParked}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_mixed", TriggerEventID: "sevt_trigger",
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{"public_ask"}, completed.PendingActionEventIDs)
	require.Len(t, completed.TranscriptDelta, 3)
	require.Equal(t, domain.RoleUser, completed.TranscriptDelta[2].Role)
	require.Equal(t, []domain.ContentBlock{{
		Type:          "tool_result",
		ToolResultFor: "provider_read",
		Text:          "file contents",
	}}, completed.TranscriptDelta[2].Content)
}

func TestWorkflowTurn_ToolActivityRetryDoesNotRepeatModelStep(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			AttemptID: "ratm_retry",
			Request: model.Request{
				Model:    "test-model",
				Messages: []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "run"}}}},
				Tools:    []model.ToolSchema{{Name: "bash", InputSchema: map[string]any{"type": "object"}}},
			},
			Tools: []TurnTool{{
				Name: "bash", Kind: TurnToolBuiltin,
				Permission: domain.PermissionPolicy{Type: "always_allow"},
			}},
		}, nil
	}

	var mu sync.Mutex
	modelCalls := 0
	callModel := func(_ context.Context, _ CallModelInput) (CallModelResult, error) {
		mu.Lock()
		defer mu.Unlock()
		modelCalls++
		if modelCalls == 1 {
			return CallModelResult{
				ToolSteps: []PlannedToolStep{{
					ToolUseEventID: "sevt_tool_retry", ToolStepID: "tstep_retry",
				}},
				Response: model.Response{
					StopReason: "tool_use",
					Content: []domain.ContentBlock{{
						Type: "tool_use", ToolUseID: "sevt_tool_retry", ToolName: "bash",
						Input: map[string]any{"command": "echo ok"},
					}},
				},
			}, nil
		}
		return CallModelResult{Response: model.Response{StopReason: "end_turn"}}, nil
	}

	toolAttempts := 0
	executeTool := func(_ context.Context, _ ExecuteToolInput) (ExecuteToolResult, error) {
		mu.Lock()
		defer mu.Unlock()
		toolAttempts++
		if toolAttempts == 1 {
			return ExecuteToolResult{}, errors.New("activity result acknowledgement lost")
		}
		return ExecuteToolResult{
			Result: domain.ToolStepResult{
				Content: []any{map[string]any{"type": "text", "text": "ok"}},
			},
		}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		mu.Lock()
		completed = in
		mu.Unlock()
		return RunTurnResult{}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_retry", TriggerEventID: "sevt_trigger",
	})
	require.NoError(t, env.GetWorkflowError())

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 2, toolAttempts, "Temporal should retry only the failed tool Activity")
	require.Equal(t, 2, modelCalls, "the first model response must remain in Workflow history")
	resultEvents := 0
	for _, draft := range completed.Output {
		if draft.Type == domain.EvAgentToolResult {
			resultEvents++
		}
	}
	require.Equal(t, 1, resultEvents)
}

func TestWorkflowTurn_AmbiguousToolTerminatesHonestly(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			AttemptID: "ratm_ambiguous",
			Request: model.Request{
				Model:    "test-model",
				Messages: []domain.Message{{Role: domain.RoleUser}},
				Tools:    []model.ToolSchema{{Name: "bash"}},
			},
			Tools: []TurnTool{{
				Name: "bash", Kind: TurnToolBuiltin,
				Permission: domain.PermissionPolicy{Type: "always_allow"},
			}},
		}, nil
	}
	callModel := func(context.Context, CallModelInput) (CallModelResult, error) {
		return CallModelResult{
			ToolSteps: []PlannedToolStep{{
				ToolUseEventID: "sevt_ambiguous", ToolStepID: "tstep_ambiguous",
			}},
			Response: model.Response{StopReason: "tool_use", Content: []domain.ContentBlock{{
				Type: "tool_use", ToolUseID: "sevt_ambiguous", ToolName: "bash",
				Input: map[string]any{"command": "side effect"},
			}}},
		}, nil
	}
	executeTool := func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
		return ExecuteToolResult{Ambiguous: true}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnTerminated}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_ambiguous", TriggerEventID: "sevt_trigger",
	})
	require.NoError(t, env.GetWorkflowError())
	var result RunTurnResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, TurnTerminated, result.Disposition)
	require.Equal(t, domain.StatusTerminated, completed.Status)
	require.Equal(t, domain.RunAttemptFailed, completed.AttemptState)
	require.NotNil(t, completed.AttemptError)
	require.Equal(t, []string{
		domain.EvAgentToolUse,
		domain.EvSessionError,
		domain.EvSessionStatusTerminated,
	}, draftTypes(completed.Output))
}

func TestWorkflowTurn_PermanentToolPreparationTerminatesHonestly(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			AttemptID: "ratm_resource_permanent",
			Request: model.Request{
				Model:    "test-model",
				Messages: []domain.Message{{Role: domain.RoleUser}},
				Tools:    []model.ToolSchema{{Name: "bash"}},
			},
			Tools: []TurnTool{{
				Name: "bash", Kind: TurnToolBuiltin,
				Permission: domain.PermissionPolicy{Type: "always_allow"},
			}},
		}, nil
	}
	callModel := func(context.Context, CallModelInput) (CallModelResult, error) {
		return CallModelResult{
			ToolSteps: []PlannedToolStep{{
				ToolUseEventID: "sevt_resource_permanent",
				ToolStepID:     "tstep_resource_permanent",
			}},
			Response: model.Response{StopReason: "tool_use", Content: []domain.ContentBlock{{
				Type: "tool_use", ToolUseID: "sevt_resource_permanent", ToolName: "bash",
				Input: map[string]any{"command": "true"},
			}}},
		}, nil
	}
	executeTool := func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
		return ExecuteToolResult{FatalError: "File Resource mount is unavailable"}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnTerminated}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_resource_permanent", TriggerEventID: "sevt_trigger",
	})
	require.NoError(t, env.GetWorkflowError())
	var result RunTurnResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, TurnTerminated, result.Disposition)
	require.Equal(t, domain.StatusTerminated, completed.Status)
	require.Equal(t, domain.RunAttemptFailed, completed.AttemptState)
	require.NotNil(t, completed.AttemptError)
	require.Contains(t, *completed.AttemptError, "File Resource mount")
	require.Equal(t, []string{
		domain.EvAgentToolUse,
		domain.EvSessionError,
		domain.EvSessionStatusTerminated,
	}, draftTypes(completed.Output))
}

func TestWorkflowTurn_PermanentModelErrorTerminatesHonestly(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			Request: model.Request{
				Model: "test-model",
				Messages: []domain.Message{{
					Role: domain.RoleUser,
					Content: []domain.ContentBlock{{
						Type: "text", Text: "invalid request",
					}},
				}},
			},
		}, nil
	}
	client := model.NewFake()
	client.SetError(&model.APIError{
		Kind:       model.ErrorInvalidRequest,
		StatusCode: 400,
		Type:       "invalid_request_error",
		Message:    "invalid messages",
	})
	activities := NewActivities(
		client, nil, nil, nil, domain.NewSeqIDGen(),
	)
	executions := 0
	executeTool := func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
		executions++
		return ExecuteToolResult{}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnTerminated}, nil
	}
	registerWorkflowTurnActivities(env, prepare, activities.CallModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_model_permanent", TriggerEventID: "sevt_trigger",
	})
	require.NoError(t, env.GetWorkflowError())
	var result RunTurnResult
	require.NoError(t, env.GetWorkflowResult(&result))
	require.Equal(t, TurnTerminated, result.Disposition)
	require.Zero(t, executions, "permanent model failure must not execute a tool")
	require.Equal(t, domain.StatusTerminated, completed.Status)
	require.Equal(t, []string{
		domain.EvSpanModelRequestEnd,
		domain.EvSessionError,
		domain.EvSessionStatusTerminated,
	}, draftTypes(completed.Output))
	errorPayload, ok := completed.Output[1].Payload["error"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, errorPayload["message"], "invalid_request_error")
	require.Equal(t, "model_request_failed_error", errorPayload["type"])
	require.Equal(t, "terminal", errorPayload["retry_status"].(map[string]any)["type"])
}

func TestWorkflowTurn_PublishesThinkingWithoutPrivateContent(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{Request: model.Request{Model: "test-model"}}, nil
	}
	callModel := func(context.Context, CallModelInput) (CallModelResult, error) {
		return CallModelResult{
			ThinkingEventID: "sevt_thinking",
			MessageEventID:  "sevt_answer",
			Response: model.Response{
				StopReason: "end_turn",
				Content: []domain.ContentBlock{
					{Type: "thinking", Text: "must remain private"},
					{Type: "text", Text: "public answer"},
				},
			},
		}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnCompleted}, nil
	}
	registerWorkflowTurnActivities(
		env,
		prepare,
		callModel,
		func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
			return ExecuteToolResult{}, nil
		},
		complete,
	)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_thinking", TriggerEventID: "sevt_trigger",
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{
		domain.EvAgentThinking,
		domain.EvAgentMessage,
		domain.EvSessionStatusIdle,
	}, draftTypes(completed.Output))
	require.Empty(t, completed.Output[0].Payload)
	require.NotContains(t, completed.Output[0].Payload, "thinking")
	for _, draft := range completed.Output {
		require.NotContains(t, fmt.Sprint(draft.Payload), "must remain private")
	}
}

func TestWorkflowTurn_PublishesModelRetryLifecycleAndRecovers(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)
	registerBudgetTestActivities(env)

	env.RegisterActivityWithOptions(
		func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
			return PrepareTurnResult{Request: model.Request{
				Model: "test-model",
				Messages: []domain.Message{{
					Role:    domain.RoleUser,
					Content: []domain.ContentBlock{{Type: "text", Text: "hello"}},
				}},
			}}, nil
		},
		activity.RegisterOptions{Name: ActivityPrepareTurn},
	)
	var starts []StartModelRequestInput
	env.RegisterActivityWithOptions(
		func(_ context.Context, in StartModelRequestInput) error {
			starts = append(starts, in)
			return nil
		},
		activity.RegisterOptions{Name: ActivityStartModelRequest},
	)
	var flushed []domain.EventDraft
	env.RegisterActivityWithOptions(
		func(_ context.Context, in AppendWorkflowEventsInput) error {
			flushed = append(flushed, in.Events...)
			return nil
		},
		activity.RegisterOptions{Name: ActivityAppendWorkflowEvents},
	)
	var calls []CallModelInput
	env.RegisterActivityWithOptions(
		func(_ context.Context, in CallModelInput) (CallModelResult, error) {
			calls = append(calls, in)
			if len(calls) <= 2 {
				return CallModelResult{
					ModelRequestStartID: in.ModelRequestStartID,
					ModelRequestEndID:   in.ModelRequestEndID,
					RetryError: &ModelRetryError{
						Type:    "model_overloaded_error",
						Message: "provider overloaded",
					},
				}, nil
			}
			return CallModelResult{
				ModelRequestStartID: in.ModelRequestStartID,
				ModelRequestEndID:   in.ModelRequestEndID,
				MessageEventID:      "sevt_retry_recovered",
				Response: model.Response{
					StopReason: "end_turn",
					Content:    []domain.ContentBlock{{Type: "text", Text: "recovered"}},
				},
			}, nil
		},
		activity.RegisterOptions{Name: ActivityCallModel},
	)
	var retrying []RecordModelRetryInput
	env.RegisterActivityWithOptions(
		func(_ context.Context, in RecordModelRetryInput) error {
			retrying = append(retrying, in)
			return nil
		},
		activity.RegisterOptions{Name: ActivityRecordModelRetry},
	)
	var resumed []ResumeModelRetryInput
	env.RegisterActivityWithOptions(
		func(_ context.Context, in ResumeModelRetryInput) error {
			resumed = append(resumed, in)
			return nil
		},
		activity.RegisterOptions{Name: ActivityResumeModelRetry},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
			return ExecuteToolResult{}, errors.New("unexpected tool execution")
		},
		activity.RegisterOptions{Name: ActivityExecuteTool},
	)
	var completed CompleteWorkflowTurnInput
	env.RegisterActivityWithOptions(
		func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
			in.Output = append(append([]domain.EventDraft(nil), flushed...), in.Output...)
			completed = in
			return RunTurnResult{Disposition: TurnCompleted}, nil
		},
		activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn},
	)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_retry_recovery", TriggerEventID: "sevt_trigger",
	})
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, calls, 3)
	require.Len(t, starts, 3)
	require.Len(t, retrying, 2)
	require.Len(t, resumed, 2)
	for _, call := range calls {
		require.True(t, call.HandleRetryableErrors)
	}
	require.NotEqual(t, calls[0].ModelRequestStartID, calls[1].ModelRequestStartID)
	require.NotEqual(t, calls[1].ModelRequestStartID, calls[2].ModelRequestStartID)
	require.Equal(t, "model_overloaded_error", retrying[0].Error.Type)
	require.NotEmpty(t, retrying[0].ErrorEventID)
	require.NotEmpty(t, retrying[0].StatusEventID)
	require.NotEmpty(t, resumed[0].StatusEventID)
	require.Equal(t, domain.StatusIdle, completed.Status)
	require.Equal(t, []string{
		domain.EvSpanModelRequestEnd,
		domain.EvSpanModelRequestEnd,
		domain.EvAgentMessage,
		domain.EvSpanModelRequestEnd,
		domain.EvSessionStatusIdle,
	}, draftTypes(completed.Output))
}

func TestWorkflowTurn_ExhaustsModelRetriesAndReturnsIdle(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{Request: model.Request{Model: "test-model"}}, nil
	}
	calls := 0
	callModel := func(_ context.Context, in CallModelInput) (CallModelResult, error) {
		calls++
		return CallModelResult{
			ModelRequestStartID: in.ModelRequestStartID,
			ModelRequestEndID:   in.ModelRequestEndID,
			RetryError: &ModelRetryError{
				Type:    "model_rate_limited_error",
				Message: "rate limited",
			},
		}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnCompleted}, nil
	}
	registerWorkflowTurnActivities(
		env,
		prepare,
		callModel,
		func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
			return ExecuteToolResult{}, nil
		},
		complete,
	)
	env.RegisterActivityWithOptions(
		func(context.Context, RecordModelRetryInput) error { return nil },
		activity.RegisterOptions{Name: ActivityRecordModelRetry},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, ResumeModelRetryInput) error { return nil },
		activity.RegisterOptions{Name: ActivityResumeModelRetry},
	)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_retry_exhausted", TriggerEventID: "sevt_trigger",
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, maxModelRequestAttempts, calls)
	require.Equal(t, domain.StatusIdle, completed.Status)
	require.Equal(t, []string{
		domain.EvSpanModelRequestEnd,
		domain.EvSpanModelRequestEnd,
		domain.EvSpanModelRequestEnd,
		domain.EvSessionError,
		domain.EvSessionStatusIdle,
	}, draftTypes(completed.Output))
	errorPayload := completed.Output[len(completed.Output)-2].Payload["error"].(map[string]any)
	require.Equal(t, "exhausted", errorPayload["retry_status"].(map[string]any)["type"])
	stopReason := completed.Output[len(completed.Output)-1].Payload["stop_reason"].(map[string]any)
	require.Equal(t, "retries_exhausted", stopReason["type"])
}

func TestWorkflowTurnCompactsLargeToolResultBeforeNextModelRequest(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	initial := []domain.Message{{
		Role:    domain.RoleUser,
		Content: []domain.ContentBlock{{Type: "text", Text: "read the large result"}},
	}}
	largeResult := strings.Repeat("tool-output-", 55_000)
	var calls []CallModelInput
	var completed CompleteWorkflowTurnInput
	registerWorkflowTurnActivities(
		env,
		func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
			return PrepareTurnResult{
				AttemptID: "ratm_context_tool", UsesProviderTranscript: true,
				TranscriptDelta: initial,
				Request: model.Request{
					Model: "test-model", Messages: initial,
					Tools: []model.ToolSchema{{Name: "read"}},
				},
				Tools: []TurnTool{{
					Name: "read", Kind: TurnToolBuiltin,
					Permission: domain.PermissionPolicy{Type: "always_allow"},
				}},
			}, nil
		},
		func(_ context.Context, in CallModelInput) (CallModelResult, error) {
			calls = append(calls, in)
			if len(calls) == 1 {
				return CallModelResult{
					ModelRequestStartID: in.ModelRequestStartID,
					ModelRequestEndID:   in.ModelRequestEndID,
					Response: model.Response{
						StopReason: "tool_use",
						Content: []domain.ContentBlock{{
							Type: "tool_use", ToolUseID: "tool_large",
							ToolName: "read", Input: map[string]any{"path": "large.log"},
						}},
						Usage: domain.TokenUsage{InputTokens: 100, OutputTokens: 10},
					},
					ToolSteps: []PlannedToolStep{{
						ToolUseEventID:    "sevt_tool_large",
						ProviderToolUseID: "tool_large",
						ToolStepID:        "tstep_tool_large",
					}},
				}, nil
			}
			last := in.Request.Messages[len(in.Request.Messages)-1]
			require.Equal(t, domain.RoleUser, last.Role)
			require.Equal(t, "tool_result", last.Content[0].Type)
			require.Contains(t, last.Content[0].Text, "Tool result compacted")
			require.Less(t, len(last.Content[0].Text), 4_000)
			return CallModelResult{
				ModelRequestStartID: in.ModelRequestStartID,
				ModelRequestEndID:   in.ModelRequestEndID,
				MessageEventID:      "sevt_after_compaction",
				Response: model.Response{
					StopReason: "end_turn",
					Content:    []domain.ContentBlock{{Type: "text", Text: "done"}},
				},
			}, nil
		},
		func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
			return ExecuteToolResult{Result: domain.ToolStepResult{
				Content: []any{map[string]any{"type": "text", "text": largeResult}},
			}}, nil
		},
		func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
			completed = in
			return RunTurnResult{Disposition: TurnCompleted}, nil
		},
	)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_context_tool", TriggerEventID: "sevt_context_tool",
	})
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, calls, 2)
	require.Contains(t, draftTypes(completed.Output), domain.EvAgentThreadContextCompacted)
	var durableResult string
	for _, message := range completed.TranscriptDelta {
		for _, block := range message.Content {
			if block.Type == "tool_result" {
				durableResult = block.Text
			}
		}
	}
	require.Equal(t, largeResult, durableResult,
		"request compaction must not mutate the lossless transcript")
}

func TestWorkflowTurnReactivelyCompactsRequestTooLargeOnce(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	messages := []domain.Message{
		{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("old ", 15_000)}}},
		{Role: domain.RoleAssistant, Content: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("analysis ", 10_000)}}},
		{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "current request"}}},
	}
	var calls []CallModelInput
	var completed CompleteWorkflowTurnInput
	registerWorkflowTurnActivities(
		env,
		func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
			return PrepareTurnResult{Request: model.Request{
				Model: "test-model", Messages: messages,
			}}, nil
		},
		func(_ context.Context, in CallModelInput) (CallModelResult, error) {
			calls = append(calls, in)
			if len(calls) == 1 {
				return CallModelResult{
					ModelRequestStartID:  in.ModelRequestStartID,
					ModelRequestEndID:    in.ModelRequestEndID,
					ContextOverflow:      true,
					ContextOverflowError: "provider rejected an oversized request",
				}, nil
			}
			require.Less(t,
				domain.EstimateMessagesTokens(in.Request.Messages),
				domain.EstimateMessagesTokens(calls[0].Request.Messages),
			)
			return CallModelResult{
				ModelRequestStartID: in.ModelRequestStartID,
				ModelRequestEndID:   in.ModelRequestEndID,
				MessageEventID:      "sevt_reactive_done",
				Response: model.Response{
					StopReason: "end_turn",
					Content:    []domain.ContentBlock{{Type: "text", Text: "recovered"}},
				},
			}, nil
		},
		func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
			return ExecuteToolResult{}, errors.New("unexpected tool execution")
		},
		func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
			completed = in
			return RunTurnResult{Disposition: TurnCompleted}, nil
		},
	)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_reactive_context", TriggerEventID: "sevt_reactive_context",
	})
	require.NoError(t, env.GetWorkflowError())
	require.Len(t, calls, 2)
	require.Contains(t, draftTypes(completed.Output), domain.EvAgentThreadContextCompacted)
}

func TestWorkflowTurn_MixedBatchExecutesBuiltinAndParksClientAction(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			AttemptID: "ratm_client_action",
			Request: model.Request{
				Model:    "test-model",
				Messages: []domain.Message{{Role: domain.RoleUser}},
				Tools: []model.ToolSchema{
					{Name: "bash"},
					{Name: "ask_client"},
				},
			},
			Tools: []TurnTool{
				{
					Name: "bash", Kind: TurnToolBuiltin,
					Permission: domain.PermissionPolicy{Type: "always_allow"},
				},
				{Name: "ask_client", Kind: TurnToolCustom},
			},
		}, nil
	}
	callModel := func(context.Context, CallModelInput) (CallModelResult, error) {
		return CallModelResult{
			ToolSteps: []PlannedToolStep{
				{ToolUseEventID: "sevt_builtin", ToolStepID: "tstep_builtin"},
				{ToolUseEventID: "sevt_custom", ToolStepID: "tstep_custom"},
			},
			Response: model.Response{StopReason: "tool_use", Content: []domain.ContentBlock{
				{
					Type: "tool_use", ToolUseID: "sevt_builtin", ToolName: "bash",
					Input: map[string]any{"command": "must not run"},
				},
				{
					Type: "tool_use", ToolUseID: "sevt_custom", ToolName: "ask_client",
					Input: map[string]any{"question": "continue?"},
				},
			}},
		}, nil
	}
	executions := 0
	executeTool := func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
		executions++
		return ExecuteToolResult{}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnParked}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_client_action", TriggerEventID: "sevt_trigger",
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 1, executions)
	require.Equal(t, domain.StatusIdle, completed.Status)
	require.Equal(t, domain.RunAttemptCompleted, completed.AttemptState)
	require.Equal(t, []string{"sevt_custom"}, completed.PendingActionEventIDs)
	require.Equal(t, []string{
		domain.EvAgentToolUse,
		domain.EvAgentCustomToolUse,
		domain.EvAgentToolResult,
		domain.EvSessionStatusIdle,
	}, draftTypes(completed.Output))
}

func TestWorkflowTurn_ResumesMixedBarrierAsOneToolResultRound(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	resolutionIDs := []string{"sevt_custom_result", "sevt_confirmation"}
	prepare := func(_ context.Context, in PrepareTurnInput) (PrepareTurnResult, error) {
		require.Equal(t, resolutionIDs, in.ResolutionEventIDs)
		return PrepareTurnResult{
			AttemptID: "ratm_resume",
			Request: model.Request{
				Model: "test-model",
				Messages: []domain.Message{
					{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "inspect"}}},
					{Role: domain.RoleAssistant, Content: []domain.ContentBlock{{
						Type: "tool_use", ToolUseID: "sevt_inline", ToolName: "read",
						Input: map[string]any{"path": "a.txt"},
					}}},
					{Role: domain.RoleUser, Content: []domain.ContentBlock{{
						Type: "tool_result", ToolResultFor: "sevt_inline", Text: "inline result",
					}}},
				},
			},
			Tools: []TurnTool{
				{Name: "ask_client", Kind: TurnToolCustom},
				{
					Name: "bash", Kind: TurnToolBuiltin,
					Permission: domain.PermissionPolicy{Type: "always_ask"},
				},
			},
			ResumeActions: []ResumeAction{
				{
					ActionEventID:     "sevt_custom",
					Kind:              domain.PendingCustomToolResult,
					ToolName:          "ask_client",
					Input:             map[string]any{"question": "continue?"},
					ResolutionEventID: "sevt_custom_result",
					Content: []any{
						map[string]any{"type": "text", "text": "yes"},
					},
				},
				{
					ActionEventID:     "sevt_ask",
					Kind:              domain.PendingToolConfirmation,
					ToolName:          "bash",
					Input:             map[string]any{"command": "pwd"},
					ResolutionEventID: "sevt_confirmation",
					Confirmation:      "allow",
					ToolStepID:        "tstep_resume",
				},
			},
		}, nil
	}

	var modelInput CallModelInput
	callModel := func(_ context.Context, in CallModelInput) (CallModelResult, error) {
		modelInput = in
		return CallModelResult{
			MessageEventID: "sevt_final",
			Response: model.Response{
				StopReason: "end_turn",
				Content:    []domain.ContentBlock{{Type: "text", Text: "done"}},
			},
		}, nil
	}
	executeTool := func(_ context.Context, in ExecuteToolInput) (ExecuteToolResult, error) {
		require.Equal(t, "sevt_ask", in.ToolUseEventID)
		require.Equal(t, "tstep_resume", in.ToolStepID)
		require.Equal(t, map[string]any{"command": "pwd"}, in.Input)
		return ExecuteToolResult{Result: domain.ToolStepResult{
			Content: []any{map[string]any{"type": "text", "text": "/workspace"}},
		}}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnCompleted}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID:          "sess_resume",
		TriggerEventID:     "sevt_confirmation",
		ResolutionEventIDs: resolutionIDs,
	})
	require.NoError(t, env.GetWorkflowError())

	require.Len(t, modelInput.Request.Messages, 5)
	require.Equal(t, []domain.ContentBlock{
		{
			Type: "tool_use", ToolUseID: "sevt_custom", ToolName: "ask_client",
			Input: map[string]any{"question": "continue?"},
		},
		{
			Type: "tool_use", ToolUseID: "sevt_ask", ToolName: "bash",
			Input: map[string]any{"command": "pwd"},
		},
	}, modelInput.Request.Messages[3].Content)
	require.Equal(t, []domain.ContentBlock{
		{Type: "tool_result", ToolResultFor: "sevt_custom", Text: "yes"},
		{Type: "tool_result", ToolResultFor: "sevt_ask", Text: "/workspace"},
	}, modelInput.Request.Messages[4].Content)
	require.Equal(t, resolutionIDs, completed.ResolutionEventIDs)
	require.Equal(t, []string{
		domain.EvAgentToolResult,
		domain.EvAgentMessage,
		domain.EvSessionStatusIdle,
	}, draftTypes(completed.Output))
	require.Equal(t, "sevt_ask", completed.Output[0].Payload["tool_use_id"])
}

func TestWorkflowTurn_DeniedConfirmationDoesNotExecute(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			Request: model.Request{Model: "test-model"},
			Tools: []TurnTool{{
				Name: "bash", Kind: TurnToolBuiltin,
				Permission: domain.PermissionPolicy{Type: "always_ask"},
			}},
			ResumeActions: []ResumeAction{{
				ActionEventID:     "sevt_ask",
				Kind:              domain.PendingToolConfirmation,
				ToolName:          "bash",
				Input:             map[string]any{"command": "rm file"},
				ResolutionEventID: "sevt_deny",
				Confirmation:      "deny",
				DenyMessage:       "not safe",
			}},
		}, nil
	}
	var modelInput CallModelInput
	callModel := func(_ context.Context, in CallModelInput) (CallModelResult, error) {
		modelInput = in
		return CallModelResult{Response: model.Response{StopReason: "end_turn"}}, nil
	}
	executions := 0
	executeTool := func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
		executions++
		return ExecuteToolResult{}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnCompleted}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID:          "sess_deny",
		TriggerEventID:     "sevt_deny",
		ResolutionEventIDs: []string{"sevt_deny"},
	})
	require.NoError(t, env.GetWorkflowError())
	require.Zero(t, executions)
	require.Len(t, modelInput.Request.Messages, 2)
	result := modelInput.Request.Messages[1].Content[0]
	require.True(t, result.IsError)
	require.Equal(t, "Tool call denied by user. not safe", result.Text)
	require.Equal(t, true, completed.Output[0].Payload["is_error"])
}

func TestWorkflowTurn_SelfHostedToolResultResumesWithoutServerExecution(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			Request: model.Request{Model: "test-model"},
			Tools: []TurnTool{{
				Name: "read", Kind: TurnToolSelfHosted,
				Permission: domain.PermissionPolicy{Type: "always_allow"},
			}},
			ResumeActions: []ResumeAction{{
				ActionEventID: "sevt_read", Kind: domain.PendingToolResult,
				ToolName: "read", Input: map[string]any{"path": "report.md"},
				ResolutionEventID: "sevt_read_result",
				Content:           []any{map[string]any{"type": "text", "text": "contents"}},
			}},
		}, nil
	}
	var modelInput CallModelInput
	callModel := func(_ context.Context, in CallModelInput) (CallModelResult, error) {
		modelInput = in
		return CallModelResult{
			MessageEventID: "sevt_final",
			Response: model.Response{StopReason: "end_turn", Content: []domain.ContentBlock{{
				Type: "text", Text: "used client result",
			}}},
		}, nil
	}
	executions := 0
	executeTool := func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
		executions++
		return ExecuteToolResult{}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnCompleted}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_self_hosted", TriggerEventID: "sevt_read_result",
		ResolutionEventIDs: []string{"sevt_read_result"},
	})
	require.NoError(t, env.GetWorkflowError())
	require.Zero(t, executions)
	require.Len(t, modelInput.Request.Messages, 2)
	require.Equal(t, "sevt_read", modelInput.Request.Messages[0].Content[0].ToolUseID)
	require.Equal(t, "contents", modelInput.Request.Messages[1].Content[0].Text)
	require.Equal(t, []string{
		domain.EvAgentMessage,
		domain.EvSessionStatusIdle,
	}, draftTypes(completed.Output), "user.tool_result itself is the public result event")
}

func TestWorkflowTurn_InvalidResumeTerminatesBeforeModelOrTool(t *testing.T) {
	tests := []struct {
		name        string
		attemptID   string
		tools       []TurnTool
		action      ResumeAction
		wantMessage string
	}{
		{
			name: "tool no longer enabled",
			action: ResumeAction{
				ActionEventID: "sevt_custom", Kind: domain.PendingCustomToolResult,
				ToolName: "missing", ResolutionEventID: "sevt_result",
			},
			wantMessage: "pending action names a tool that is not enabled: missing",
		},
		{
			name: "custom result references builtin",
			tools: []TurnTool{{
				Name: "bash", Kind: TurnToolBuiltin,
				Permission: domain.PermissionPolicy{Type: "always_allow"},
			}},
			action: ResumeAction{
				ActionEventID: "sevt_custom", Kind: domain.PendingCustomToolResult,
				ToolName: "bash", ResolutionEventID: "sevt_result",
			},
			wantMessage: "custom tool result does not reference a custom tool",
		},
		{
			name: "confirmation references non ask builtin",
			tools: []TurnTool{{
				Name: "bash", Kind: TurnToolBuiltin,
				Permission: domain.PermissionPolicy{Type: "always_allow"},
			}},
			action: ResumeAction{
				ActionEventID: "sevt_ask", Kind: domain.PendingToolConfirmation,
				ToolName: "bash", ResolutionEventID: "sevt_confirmation",
				Confirmation: "deny",
			},
			wantMessage: "tool confirmation does not reference an always_ask built-in",
		},
		{
			name:      "allowed confirmation has no operation id",
			attemptID: "ratm_resume",
			tools: []TurnTool{{
				Name: "bash", Kind: TurnToolBuiltin,
				Permission: domain.PermissionPolicy{Type: "always_ask"},
			}},
			action: ResumeAction{
				ActionEventID: "sevt_ask", Kind: domain.PendingToolConfirmation,
				ToolName: "bash", ResolutionEventID: "sevt_confirmation",
				Confirmation: "allow",
			},
			wantMessage: "allowed confirmation has no durable operation id",
		},
		{
			name:  "unknown pending action kind",
			tools: []TurnTool{{Name: "ask_client", Kind: TurnToolCustom}},
			action: ResumeAction{
				ActionEventID: "sevt_custom", Kind: domain.PendingActionKind("unknown"),
				ToolName: "ask_client", ResolutionEventID: "sevt_result",
			},
			wantMessage: "unknown pending action kind",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterWorkflow(workflowTurnHarness)

			prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
				return PrepareTurnResult{
					AttemptID:     tc.attemptID,
					Request:       model.Request{Model: "test-model"},
					Tools:         tc.tools,
					ResumeActions: []ResumeAction{tc.action},
				}, nil
			}
			modelCalls := 0
			callModel := func(context.Context, CallModelInput) (CallModelResult, error) {
				modelCalls++
				return CallModelResult{}, nil
			}
			toolCalls := 0
			executeTool := func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
				toolCalls++
				return ExecuteToolResult{}, nil
			}
			var completed CompleteWorkflowTurnInput
			complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
				completed = in
				return RunTurnResult{Disposition: TurnTerminated}, nil
			}
			registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

			env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
				SessionID:          "sess_invalid_resume",
				TriggerEventID:     tc.action.ResolutionEventID,
				ResolutionEventIDs: []string{tc.action.ResolutionEventID},
			})

			require.NoError(t, env.GetWorkflowError())
			require.Zero(t, modelCalls)
			require.Zero(t, toolCalls)
			require.Equal(t, domain.StatusTerminated, completed.Status)
			require.Equal(t, []string{
				domain.EvSessionError,
				domain.EvSessionStatusTerminated,
			}, draftTypes(completed.Output))
			errorPayload, ok := completed.Output[0].Payload["error"].(map[string]any)
			require.True(t, ok)
			require.Equal(t, tc.wantMessage, errorPayload["message"])
			require.Equal(t, []string{tc.action.ResolutionEventID}, completed.ResolutionEventIDs)
		})
	}
}

func TestWorkflowTurn_AllowedConfirmationAmbiguousTerminatesHonestly(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			AttemptID: "ratm_resume",
			Request:   model.Request{Model: "test-model"},
			Tools: []TurnTool{{
				Name: "bash", Kind: TurnToolBuiltin,
				Permission: domain.PermissionPolicy{Type: "always_ask"},
			}},
			ResumeActions: []ResumeAction{{
				ActionEventID:     "sevt_ask",
				Kind:              domain.PendingToolConfirmation,
				ToolName:          "bash",
				Input:             map[string]any{"command": "touch marker"},
				ResolutionEventID: "sevt_confirmation",
				Confirmation:      "allow",
				ToolStepID:        "tstep_resume",
			}},
		}, nil
	}
	modelCalls := 0
	callModel := func(context.Context, CallModelInput) (CallModelResult, error) {
		modelCalls++
		return CallModelResult{}, nil
	}
	toolCalls := 0
	executeTool := func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
		toolCalls++
		return ExecuteToolResult{Ambiguous: true}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnTerminated}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID:          "sess_ambiguous_resume",
		TriggerEventID:     "sevt_confirmation",
		ResolutionEventIDs: []string{"sevt_confirmation"},
	})

	require.NoError(t, env.GetWorkflowError())
	require.Zero(t, modelCalls)
	require.Equal(t, 1, toolCalls)
	require.Equal(t, domain.StatusTerminated, completed.Status)
	require.Equal(t, domain.RunAttemptFailed, completed.AttemptState)
	require.Equal(t, []string{
		domain.EvSessionError,
		domain.EvSessionStatusTerminated,
	}, draftTypes(completed.Output))
	require.Equal(t, []string{"sevt_confirmation"}, completed.ResolutionEventIDs)
}

func TestWorkflowTurn_ResumeExecutionKeepsToolOrdinal(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			AttemptID: "ratm_resume",
			Request:   model.Request{Model: "test-model"},
			Tools: []TurnTool{
				{
					Name: "write", Kind: TurnToolBuiltin,
					Permission: domain.PermissionPolicy{Type: "always_ask"},
				},
				{
					Name: "bash", Kind: TurnToolBuiltin,
					Permission: domain.PermissionPolicy{Type: "always_allow"},
				},
			},
			ResumeActions: []ResumeAction{{
				ActionEventID:     "sevt_write",
				Kind:              domain.PendingToolConfirmation,
				ToolName:          "write",
				Input:             map[string]any{"path": "a.txt", "content": "hello"},
				ResolutionEventID: "sevt_confirmation",
				Confirmation:      "allow",
				ToolStepID:        "tstep_write",
			}},
		}, nil
	}
	modelCalls := 0
	callModel := func(context.Context, CallModelInput) (CallModelResult, error) {
		modelCalls++
		if modelCalls == 1 {
			return CallModelResult{
				ToolSteps: []PlannedToolStep{{
					ToolUseEventID: "sevt_bash",
					ToolStepID:     "tstep_bash",
				}},
				Response: model.Response{StopReason: "tool_use", Content: []domain.ContentBlock{{
					Type:      "tool_use",
					ToolUseID: "sevt_bash",
					ToolName:  "bash",
					Input:     map[string]any{"command": "pwd"},
				}}},
			}, nil
		}
		return CallModelResult{
			MessageEventID: "sevt_done",
			Response: model.Response{
				StopReason: "end_turn",
				Content:    []domain.ContentBlock{{Type: "text", Text: "done"}},
			},
		}, nil
	}
	var executions []ExecuteToolInput
	executeTool := func(_ context.Context, in ExecuteToolInput) (ExecuteToolResult, error) {
		executions = append(executions, in)
		return ExecuteToolResult{Result: domain.ToolStepResult{
			Content: []any{map[string]any{"type": "text", "text": "ok"}},
		}}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnCompleted}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID:          "sess_resume_ordinal",
		TriggerEventID:     "sevt_confirmation",
		ResolutionEventIDs: []string{"sevt_confirmation"},
	})

	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 2, modelCalls)
	require.Len(t, executions, 2)
	require.Equal(t, "sevt_write", executions[0].ToolUseEventID)
	require.Equal(t, "tstep_write", executions[0].ToolStepID)
	require.Equal(t, 0, executions[0].Ordinal)
	require.Equal(t, "sevt_bash", executions[1].ToolUseEventID)
	require.Equal(t, "tstep_bash", executions[1].ToolStepID)
	require.Equal(t, 1, executions[1].Ordinal)
	require.Equal(t, domain.RunAttemptCompleted, completed.AttemptState)
	require.Equal(t, []string{"sevt_confirmation"}, completed.ResolutionEventIDs)
	require.Equal(t, []string{
		domain.EvAgentToolResult,
		domain.EvAgentToolUse,
		domain.EvAgentToolResult,
		domain.EvAgentMessage,
		domain.EvSessionStatusIdle,
	}, draftTypes(completed.Output))
}

func draftTypes(drafts []domain.EventDraft) []string {
	types := make([]string, 0, len(drafts))
	for _, draft := range drafts {
		types = append(types, draft.Type)
	}
	return types
}
