package temporal

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	temporalsdk "go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

func TestPlanAutomaticToolOutcomesUseRecordedJudgments(t *testing.T) {
	for _, kind := range []TurnToolKind{TurnToolSelfHosted, TurnToolMCP} {
		for _, decision := range []domain.ToolPermissionDecision{{Type: "allow"}, {Type: "ask", ReasonCode: "indeterminate"}, {Type: "deny", ReasonCode: "high_risk"}} {
			t.Run(string(kind)+"/"+decision.Type, func(t *testing.T) {
				tool := TurnTool{Name: "read", Kind: kind, Permission: domain.PermissionPolicy{Type: "auto"}}
				if kind == TurnToolMCP {
					tool.Name = "mcp__issues__read"
					tool.MCPServer = domain.MCPServer{Name: "issues"}
					tool.MCPToolName = "read"
				}
				use := domain.ContentBlock{Type: "tool_use", ToolUseID: "provider_call", ToolName: tool.Name, Input: map[string]any{"path": "README.md"}}
				plan, failure := planToolBatch([]domain.ContentBlock{use}, indexTurnTools([]TurnTool{tool}), map[string]PlannedToolStep{"provider_call": {ToolUseEventID: "sevt_call", ToolStepID: "tstep_call"}}, true, map[string]domain.ToolPermissionDecision{"provider_call": decision})
				require.Empty(t, failure)
				require.Len(t, plan.actionDrafts, 1)
				require.Equal(t, decision.Type, plan.actionDrafts[0].Payload["evaluated_permission"])
				require.Equal(t, domain.ToolPermissionEvaluation{Type: "auto",
					EvaluatedPermission: &decision}, plan.actionDrafts[0].Payload["evaluation"])
				if decision.Type == "deny" {
					require.Empty(t, plan.executable)
					require.Empty(t, plan.pendingActionEventIDs)
					require.Len(t, plan.denied.resultBlocks, 1)
					require.True(t, plan.denied.resultBlocks[0].IsError)
					require.Equal(t, "provider_call", plan.denied.resultBlocks[0].ToolResultFor)
					require.Equal(t, "Permission to use read has been denied.", plan.denied.resultBlocks[0].Text)
				} else if decision.Type == "ask" || kind == TurnToolSelfHosted {
					require.Equal(t, []string{"sevt_call"}, plan.pendingActionEventIDs)
					require.Empty(t, plan.executable)
				} else {
					require.Len(t, plan.executable, 1)
					require.Empty(t, plan.pendingActionEventIDs)
				}
			})
		}
	}
}

func TestPlanAutomaticToolWithoutDurableJudgmentFails(t *testing.T) {
	_, failure := planToolBatch([]domain.ContentBlock{{Type: "tool_use", ToolUseID: "call", ToolName: "read"}}, indexTurnTools([]TurnTool{{Name: "read", Kind: TurnToolSelfHosted, Permission: domain.PermissionPolicy{Type: "auto"}}}), map[string]PlannedToolStep{"call": {ToolUseEventID: "sevt_call", ToolStepID: "step"}}, true, nil)
	require.NotEmpty(t, failure)
}

func TestWorkflowAutomaticPermissionControlsEffectsAndClosesAttempt(t *testing.T) {
	for _, kind := range []TurnToolKind{TurnToolSelfHosted, TurnToolMCP} {
		for _, decision := range []domain.ToolPermissionDecision{{Type: "allow"}, {Type: "ask", ReasonCode: "indeterminate"}, {Type: "deny", ReasonCode: "high_risk"}} {
			t.Run(string(kind)+"/"+decision.Type, func(t *testing.T) {
				var suite testsuite.WorkflowTestSuite
				env := suite.NewTestWorkflowEnvironment()
				env.RegisterWorkflow(workflowTurnHarness)
				tool := TurnTool{Name: "read", Kind: kind, Permission: domain.PermissionPolicy{Type: "auto"}}
				if kind == TurnToolMCP {
					tool.Name = "mcp__issues__read"
					tool.MCPServer = domain.MCPServer{Name: "issues"}
					tool.MCPToolName = "read"
				}
				initial := []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: "Read README.md"}}}}
				prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
					return PrepareTurnResult{AttemptID: "ratm_auto", ThreadID: "sthr_auto", UsesProviderTranscript: true, TranscriptDelta: initial, PermissionIntent: domain.PermissionIntent{Complete: true, Entries: []domain.PermissionIntentEntry{{Text: "Read README.md"}}}, Tools: []TurnTool{tool}, Request: model.Request{Model: "test-model", Messages: initial, Tools: []model.ToolSchema{{Name: tool.Name}}}}, nil
				}
				calls := 0
				call := func(_ context.Context, in CallModelInput) (CallModelResult, error) {
					calls++
					if calls == 1 {
						return CallModelResult{ToolSteps: []PlannedToolStep{{ProviderToolUseID: "provider_call", ToolUseEventID: "sevt_auto", ToolStepID: "tstep_auto"}}, Response: model.Response{StopReason: "tool_use", Content: []domain.ContentBlock{{Type: "tool_use", ToolUseID: "provider_call", ToolName: tool.Name, Input: map[string]any{"path": "README.md"}}}}}, nil
					}
					if decision.Type == "deny" {
						last := in.Request.Messages[len(in.Request.Messages)-1]
						require.True(t, last.Content[0].IsError)
						require.Equal(t, "provider_call", last.Content[0].ToolResultFor)
					}
					return CallModelResult{MessageEventID: "sevt_done", Response: model.Response{StopReason: "end_turn", Content: []domain.ContentBlock{{Type: "text", Text: "done"}}}}, nil
				}
				effects := 0
				execute := func(_ context.Context, in ExecuteToolInput) (ExecuteToolResult, error) {
					effects++
					require.Equal(t, "sevt_auto", in.PermissionReceiptID)
					return ExecuteToolResult{Result: domain.ToolStepResult{Content: []any{map[string]any{"type": "text", "text": "contents"}}}}, nil
				}
				var completed CompleteWorkflowTurnInput
				registerWorkflowTurnActivities(env, prepare, call, execute, func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
					completed = in
					return RunTurnResult{}, nil
				})
				established := false
				env.RegisterActivityWithOptions(func(context.Context, domain.ToolPermissionOwner) (EnsureToolPermissionAttemptResult, error) {
					established = true
					return EnsureToolPermissionAttemptResult{Established: true}, nil
				}, activity.RegisterOptions{Name: ActivityEnsureToolPermissionAttempt})
				env.RegisterActivityWithOptions(func(_ context.Context, in EvaluateToolPermissionInput) (EvaluateToolPermissionResult, error) {
					require.True(t, established)
					require.Equal(t, "ratm_auto", in.Owner.AttemptID)
					require.Equal(t, "sthr_auto", in.Owner.ThreadID)
					require.Equal(t, "Read README.md", in.Input.Intent.Entries[0].Text)
					require.Equal(t, tool.Name, in.Input.Tool.Name)
					return EvaluateToolPermissionResult{Decision: decision}, nil
				}, activity.RegisterOptions{Name: ActivityEvaluateToolPermission})
				env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{SessionID: "sesn_auto", TriggerEventID: "sevt_task"})
				require.NoError(t, env.GetWorkflowError())
				require.True(t, established)
				require.Equal(t, "ratm_auto", completed.AttemptID)
				require.Equal(t, domain.RunAttemptCompleted, completed.AttemptState)
				if decision.Type == "ask" || (decision.Type == "allow" && kind == TurnToolSelfHosted) {
					require.Equal(t, []string{"sevt_auto"}, completed.PendingActionEventIDs)
					require.Equal(t, 1, calls)
				} else {
					require.Empty(t, completed.PendingActionEventIDs)
					require.Equal(t, 2, calls)
				}
				if decision.Type == "allow" && kind == TurnToolMCP {
					require.Equal(t, 1, effects)
				} else {
					require.Equal(t, 0, effects)
				}
			})
		}
	}
}

func automaticInterruptWorkflowHarness(ctx workflow.Context, in PrepareTurnInput) (RunTurnResult, error) {
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute, RetryPolicy: &temporalsdk.RetryPolicy{MaximumAttempts: 1}})
	watcher := newTurnInterruptWatcher(actx, workflow.GetSignalChannel(ctx, WakeupSignalName), in.SessionID, "sthr_auto", 1)
	return runWorkflowTurnInternal(actx, in.SessionID, in.TriggerEventID, nil, watcher)
}
func TestWorkflowAutomaticBudgetPauseIsInterruptedBeforeAnotherEvaluation(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(automaticInterruptWorkflowHarness)
	var paused atomic.Bool
	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{AttemptID: "ratm_auto", ThreadID: "sthr_auto", UsesProviderTranscript: true, Request: model.Request{Model: "test-model", Tools: []model.ToolSchema{{Name: "read"}}}, Tools: []TurnTool{{Name: "read", Kind: TurnToolSelfHosted, Permission: domain.PermissionPolicy{Type: "auto"}}}}, nil
	}
	calls := 0
	call := func(context.Context, CallModelInput) (CallModelResult, error) {
		calls++
		return CallModelResult{Response: model.Response{StopReason: "tool_use", Content: []domain.ContentBlock{{Type: "tool_use", ToolUseID: "provider_call", ToolName: "read", Input: map[string]any{"path": "README.md"}}}}, ToolSteps: []PlannedToolStep{{ProviderToolUseID: "provider_call", ToolUseEventID: "sevt_auto", ToolStepID: "tstep_auto"}}}, nil
	}
	var completed CompleteWorkflowTurnInput
	registerWorkflowTurnActivities(env, prepare, call, func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
		t.Fatal("interrupted permission cannot execute")
		return ExecuteToolResult{}, nil
	}, func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{}, nil
	})
	env.RegisterActivityWithOptions(func(context.Context, domain.ToolPermissionOwner) (EnsureToolPermissionAttemptResult, error) {
		return EnsureToolPermissionAttemptResult{Established: true}, nil
	}, activity.RegisterOptions{Name: ActivityEnsureToolPermissionAttempt})
	evaluations := 0
	env.RegisterActivityWithOptions(func(context.Context, EvaluateToolPermissionInput) (EvaluateToolPermissionResult, error) {
		evaluations++
		paused.Store(true)
		return EvaluateToolPermissionResult{BudgetPaused: true}, nil
	}, activity.RegisterOptions{Name: ActivityEvaluateToolPermission})
	env.RegisterActivityWithOptions(func(context.Context, LoadInterruptInput) (LoadInterruptResult, error) {
		if paused.Load() {
			return LoadInterruptResult{Interrupt: &EventRef{ID: "sevt_interrupt", Seq: 2, Type: domain.EvUserInterrupt}}, nil
		}
		return LoadInterruptResult{}, nil
	}, activity.RegisterOptions{Name: ActivityLoadInterrupt})
	env.ExecuteWorkflow(automaticInterruptWorkflowHarness, PrepareTurnInput{SessionID: "sesn_auto", TriggerEventID: "sevt_task"})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 1, evaluations)
	require.Equal(t, 1, calls)
	require.Equal(t, "ratm_auto", completed.AttemptID)
	require.Equal(t, domain.RunAttemptInterrupted, completed.AttemptState)
	require.Empty(t, completed.PendingActionEventIDs)
	for _, draft := range completed.Output {
		require.NotEqual(t, domain.EvAgentToolUse, draft.Type)
	}
	last := completed.TranscriptDelta[len(completed.TranscriptDelta)-1]
	require.Equal(t, "provider_call", last.Content[0].ToolResultFor)
	require.True(t, last.Content[0].IsError)
}

func TestWorkflowAutomaticAskResumesFromRecordedOutcomeWithoutRejudging(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)
	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{AttemptID: "ratm_resume", ThreadID: "sthr_auto", Tools: []TurnTool{{Name: "mcp__issues__read", Kind: TurnToolMCP, Permission: domain.PermissionPolicy{Type: "auto"}}}, Request: model.Request{Model: "test-model"}, ResumeActions: []ResumeAction{{ActionEventID: "sevt_original", ActionEventType: domain.EvAgentMcpToolUse, Kind: domain.PendingToolConfirmation, EvaluatedPermission: "ask", ToolName: "mcp__issues__read", Input: map[string]any{"path": "README.md"}, Confirmation: "allow", ToolStepID: "tstep_resume"}}}, nil
	}
	effects := 0
	execute := func(_ context.Context, in ExecuteToolInput) (ExecuteToolResult, error) {
		effects++
		require.Empty(t, in.PermissionReceiptID, "human confirmation uses the ordinary executor, not the old auto-ask receipt")
		return ExecuteToolResult{Result: domain.ToolStepResult{Content: []any{map[string]any{"type": "text", "text": "contents"}}}}, nil
	}
	call := func(context.Context, CallModelInput) (CallModelResult, error) {
		return CallModelResult{MessageEventID: "sevt_done", Response: model.Response{StopReason: "end_turn", Content: []domain.ContentBlock{{Type: "text", Text: "done"}}}}, nil
	}
	registerWorkflowTurnActivities(env, prepare, call, execute, func(context.Context, CompleteWorkflowTurnInput) (RunTurnResult, error) { return RunTurnResult{}, nil })
	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{SessionID: "sesn_auto", TriggerEventID: "sevt_confirmation"})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, 1, effects)
}

func unfinishedToolRoundFlushHarness(ctx workflow.Context) ([]domain.EventDraft, error) {
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	turn := workflowTurnState{actx: actx, sessionID: "sesn_auto", triggerEventID: "sevt_task", output: []domain.EventDraft{
		{ID: "sevt_message", Type: domain.EvAgentMessage, Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": "Checking the repository."}}}},
		{ID: "sevt_auto", Type: domain.EvAgentMcpToolUse, Payload: map[string]any{"name": "read", "mcp_server_name": "issues", "input": map[string]any{}, "evaluated_permission": "allow"}},
	}}
	err := turn.flushOutput()
	return turn.output, err
}
func TestWorkflowProgressDoesNotPublishAnUnfinishedToolRound(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(unfinishedToolRoundFlushHarness)
	env.RegisterActivityWithOptions(func(_ context.Context, in AppendWorkflowEventsInput) error {
		require.Len(t, in.Events, 1)
		require.Equal(t, domain.EvAgentMessage, in.Events[0].Type)
		return nil
	}, activity.RegisterOptions{Name: ActivityAppendWorkflowEvents})
	env.ExecuteWorkflow(unfinishedToolRoundFlushHarness)
	require.NoError(t, env.GetWorkflowError())
	var remaining []domain.EventDraft
	require.NoError(t, env.GetWorkflowResult(&remaining))
	require.Len(t, remaining, 1)
	require.Equal(t, "sevt_auto", remaining[0].ID)
}

func ownedBudgetAdmissionHarness(ctx workflow.Context) (bool, error) {
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	watcher := newTurnInterruptWatcher(actx, workflow.GetSignalChannel(ctx, WakeupSignalName), "sesn_auto", "sthr_auto", 1)
	turn := workflowTurnState{actx: actx, sessionID: "sesn_auto", threadID: "sthr_auto", attemptID: "ratm_auto", interrupts: watcher}
	err := turn.awaitModelRequestAdmission()
	return err != nil, nil
}
func TestWorkflowOwnedBudgetPauseCannotReviveInterruptedRound(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(ownedBudgetAdmissionHarness)
	admissions := 0
	env.RegisterActivityWithOptions(func(context.Context, AdmitModelRequestInput) (AdmitModelRequestResult, error) {
		admissions++
		return AdmitModelRequestResult{Allowed: admissions > 1}, nil
	}, activity.RegisterOptions{Name: ActivityAdmitModelRequest})
	env.RegisterActivityWithOptions(func(context.Context, LoadInterruptInput) (LoadInterruptResult, error) {
		return LoadInterruptResult{Interrupt: &EventRef{ID: "sevt_interrupt", Seq: 2, Type: domain.EvUserInterrupt}}, nil
	}, activity.RegisterOptions{Name: ActivityLoadInterrupt})
	env.RegisterDelayedCallback(func() { env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 3}) }, time.Millisecond)
	env.ExecuteWorkflow(ownedBudgetAdmissionHarness)
	require.NoError(t, env.GetWorkflowError())
	var interrupted bool
	require.NoError(t, env.GetWorkflowResult(&interrupted))
	require.True(t, interrupted)
	require.Equal(t, 1, admissions)
}

func TestAutomaticDenialPreservesKnownResultsOnSiblingFailure(t *testing.T) {
	for _, failure := range []string{"ambiguous", "fatal"} {
		t.Run(failure, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			env.RegisterWorkflow(workflowTurnHarness)
			tools := []TurnTool{
				{Name: "read", Kind: TurnToolSelfHosted, Permission: domain.PermissionPolicy{Type: "auto"}},
				{Name: "mcp__svc__write", Kind: TurnToolMCP, Permission: domain.PermissionPolicy{Type: "always_allow"}, MCPServer: domain.MCPServer{Name: "svc"}, MCPToolName: "write"},
				{Name: "mcp__svc__remove", Kind: TurnToolMCP, Permission: domain.PermissionPolicy{Type: "always_allow"}, MCPServer: domain.MCPServer{Name: "svc"}, MCPToolName: "remove"},
			}
			prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
				return PrepareTurnResult{AttemptID: "ratm_review", ThreadID: "sthr_review", Request: model.Request{Model: "model", Tools: []model.ToolSchema{{Name: "read"}, {Name: "mcp__svc__write"}, {Name: "mcp__svc__remove"}}}, Tools: tools}, nil
			}
			call := func(context.Context, CallModelInput) (CallModelResult, error) {
				return CallModelResult{Response: model.Response{StopReason: "tool_use", Content: []domain.ContentBlock{
					{Type: "tool_use", ToolUseID: "deny", ToolName: "read", Input: map[string]any{}},
					{Type: "tool_use", ToolUseID: "done", ToolName: "mcp__svc__write", Input: map[string]any{}},
					{Type: "tool_use", ToolUseID: "ambig", ToolName: "mcp__svc__remove", Input: map[string]any{}},
				}}, ToolSteps: []PlannedToolStep{{ProviderToolUseID: "deny", ToolUseEventID: "sevt_deny", ToolStepID: "step_deny"}, {ProviderToolUseID: "done", ToolUseEventID: "sevt_done", ToolStepID: "step_done"}, {ProviderToolUseID: "ambig", ToolUseEventID: "sevt_ambig", ToolStepID: "step_ambig"}}}, nil
			}
			execute := func(_ context.Context, in ExecuteToolInput) (ExecuteToolResult, error) {
				if in.ToolName == "mcp__svc__remove" {
					if failure == "fatal" {
						return ExecuteToolResult{FatalError: "fatal execution failure"}, nil
					}
					return ExecuteToolResult{Ambiguous: true}, nil
				}
				return ExecuteToolResult{Result: domain.ToolStepResult{Content: []any{map[string]any{"type": "text", "text": "written"}}}}, nil
			}
			var completed CompleteWorkflowTurnInput
			registerWorkflowTurnActivities(env, prepare, call, execute, func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
				completed = in
				return RunTurnResult{}, nil
			})
			env.RegisterActivityWithOptions(func(context.Context, domain.ToolPermissionOwner) (EnsureToolPermissionAttemptResult, error) {
				return EnsureToolPermissionAttemptResult{Established: true}, nil
			}, activity.RegisterOptions{Name: ActivityEnsureToolPermissionAttempt})
			env.RegisterActivityWithOptions(func(context.Context, EvaluateToolPermissionInput) (EvaluateToolPermissionResult, error) {
				return EvaluateToolPermissionResult{Decision: domain.ToolPermissionDecision{Type: "deny", ReasonCode: "high_risk"}}, nil
			}, activity.RegisterOptions{Name: ActivityEvaluateToolPermission})
			env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{SessionID: "sesn_review", TriggerEventID: "sevt_review"})
			require.NoError(t, env.GetWorkflowError())
			require.Equal(t, domain.StatusTerminated, completed.Status)
			results := map[string]bool{}
			for _, draft := range completed.Output {
				if id, ok := domain.AgentToolResultReference(draft.Type, draft.Payload); ok {
					results[id] = true
				}
			}
			require.True(t, results["sevt_deny"], "an automatic denial must retain its matching error result after sibling ambiguity")
			require.True(t, results["sevt_done"], "the sibling's known completed side effect must retain its matching result")

		})
	}
}
