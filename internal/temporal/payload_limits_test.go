package temporal

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/proto"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

// This assertion uses the actual SDK converter, independently of the guard.
func requireBoundedTransport(t *testing.T, value any) {
	t.Helper()
	payload, err := converter.GetDefaultDataConverter().ToPayload(value)
	require.NoError(t, err)
	require.LessOrEqual(t, proto.Size(payload), 1024*1024)
}

func TestPayloadLimitPreparedEscapedTextBecomesBoundedFailure(t *testing.T) {
	source := newFakeSource([]domain.Event{{ID: "sevt_large", Sequence: 1,
		Type: domain.EvUserMessage, Payload: map[string]any{"content": []any{
			map[string]any{"type": "text", "text": strings.Repeat("<", 400_000)},
		}},
	}})
	prepared, err := NewActivities(nil, source, nil, &testIDGen{}).PrepareTurn(
		context.Background(), PrepareTurnInput{SessionID: "sesn_large", TriggerEventID: "sevt_large"},
	)
	require.NoError(t, err)
	require.NotEmpty(t, prepared.FatalError)
	requireBoundedTransport(t, prepared)
}

func TestPayloadLimitModelResponsePreservesUsageWithoutRepeatingInference(t *testing.T) {
	usage := domain.TokenUsage{InputTokens: 123, OutputTokens: 42, Speed: "fast", ProviderRegion: "us"}
	probe := &advisorProbeClient{response: model.Response{
		Content: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("<", 400_000)}},
		Usage:   usage, StopReason: "end_turn",
	}}
	result, err := NewActivities(probe, nil, nil, &testIDGen{}).CallModel(
		context.Background(), CallModelInput{SessionID: "sesn_large", Request: model.Request{Model: "test"}},
	)
	require.NoError(t, err)
	require.NotEmpty(t, result.FatalError)
	require.Equal(t, usage, result.Response.Usage)
	require.Empty(t, result.Response.Content)
	require.Equal(t, 1, probe.calls)
	requireBoundedTransport(t, result)
}

func TestPayloadLimitWorkflowRejectsModelInputBeforeInference(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	modelCalls := 0
	var committed CompleteWorkflowTurnInput
	registerWorkflowTurnActivities(env,
		func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
			return PrepareTurnResult{Request: model.Request{Model: "test", Messages: []domain.Message{{
				Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("<", 400_000)}},
			}}}}, nil
		},
		func(context.Context, CallModelInput) (CallModelResult, error) {
			modelCalls++
			return CallModelResult{Response: model.Response{StopReason: "end_turn"}}, nil
		}, func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
			t.Fatal("unexpected tool execution")
			return ExecuteToolResult{}, nil
		},
		func(_ context.Context, input CompleteWorkflowTurnInput) (RunTurnResult, error) {
			committed = input
			return RunTurnResult{Disposition: TurnTerminated}, nil
		},
	)
	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{SessionID: "sesn_large", TriggerEventID: "sevt_large"})
	require.NoError(t, env.GetWorkflowError())
	require.Zero(t, modelCalls)
	require.Equal(t, domain.StatusTerminated, committed.Status)
	requireBoundedTransport(t, committed)
}

func payloadCompletionHarness(ctx workflow.Context, mode string) (RunTurnResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	state := &workflowTurnState{actx: ctx, sessionID: "sesn_large", threadID: "sthr_child", isChild: true,
		triggerEventID: "sevt_large", attemptID: "ratm_established", resolutionEventIDs: []string{"sevt_resolution"},
		terminalSessionErrors: true, usesProviderTranscript: true, perRequestUsageAccounting: true,
		usage:  domain.TokenUsage{InputTokens: 123, OutputTokens: 42},
		output: []domain.EventDraft{{Type: domain.EvAgentMessage, Payload: map[string]any{"text": "small pending output"}}},
	}
	for range 6 {
		state.transcriptDelta = append(state.transcriptDelta, domain.Message{Role: domain.RoleAssistant,
			Content: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("<", 40_000)}},
		})
	}
	if mode == "terminated" {
		return state.terminateTyped("model_request_failed_error", "permanent failure")
	}
	if mode == "retry_exhausted" {
		return state.exhaustModelRetries(ModelRetryError{Type: "model_rate_limited_error", Message: "exhausted"})
	}
	return state.complete(nil)
}

func TestPayloadLimitCompletionCanStillCommitTerminalFailure(t *testing.T) {
	for _, mode := range []string{"complete", "terminated", "retry_exhausted"} {
		t.Run(mode, func(t *testing.T) { testPayloadLimitCompletion(t, mode) })
	}
}

func testPayloadLimitCompletion(t *testing.T, mode string) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	var committed CompleteWorkflowTurnInput
	env.RegisterActivityWithOptions(func(_ context.Context, input CompleteWorkflowTurnInput) (RunTurnResult, error) {
		committed = input
		return RunTurnResult{Disposition: TurnTerminated}, nil
	}, activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn})
	env.ExecuteWorkflow(payloadCompletionHarness, mode)
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, domain.StatusTerminated, committed.Status)
	require.Equal(t, "sthr_child", committed.ThreadID)
	require.True(t, committed.IsChild)
	require.Equal(t, "ratm_established", committed.AttemptID)
	require.Equal(t, []string{"sevt_resolution"}, committed.ResolutionEventIDs)
	require.Equal(t, domain.TokenUsage{InputTokens: 123, OutputTokens: 42}, committed.Usage)
	require.True(t, committed.UsageAlreadyAccounted)
	require.Equal(t, domain.RunAttemptFailed, committed.AttemptState)
	require.Empty(t, committed.TranscriptDelta)
	require.Len(t, committed.Output, 2)
	require.Equal(t, domain.EvSessionError, committed.Output[0].Type)
	requireBoundedTransport(t, committed)
}

func payloadToolInputHarness(ctx workflow.Context) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	state := &workflowTurnState{actx: ctx, sessionID: "sesn_large", triggerEventID: "sevt_large"}
	_, _, err := state.executeTool("ratm_not_established", domain.ContentBlock{
		ToolName: "bash", Input: map[string]any{"command": strings.Repeat("<", 400_000)},
	}, "sevt_tool", "tstep_large", TurnTool{Kind: TurnToolSelfHosted}, model.Request{}, domain.AdvisorConsultation{}, "")
	if err == nil {
		return "", fmt.Errorf("oversized tool input was executed")
	}
	return state.attemptID, nil
}

func TestPayloadLimitToolInputDoesNotEstablishAnAttempt(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	calls := 0
	env.RegisterActivityWithOptions(func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
		calls++
		return ExecuteToolResult{}, nil
	}, activity.RegisterOptions{Name: ActivityExecuteTool})
	env.ExecuteWorkflow(payloadToolInputHarness)
	require.NoError(t, env.GetWorkflowError())
	var attemptID string
	require.NoError(t, env.GetWorkflowResult(&attemptID))
	require.Empty(t, attemptID)
	require.Zero(t, calls)
}

func TestPayloadLimitCachedToolResultKeepsItsJournal(t *testing.T) {
	stored := domain.ToolStepResult{Content: []any{map[string]any{
		"type": "text", "text": strings.Repeat("<", 400_000),
	}}}
	journal := &memoryMCPJournal{step: domain.ToolStep{ID: "tstep_cached", State: domain.ToolStepCompleted, Result: &stored}}
	acts := NewActivities(nil, nil, journal, &testIDGen{})
	for range 2 {
		result, err := acts.ExecuteTool(context.Background(), ExecuteToolInput{
			SessionID: "sesn_large", TriggerEventID: "sevt_large", AttemptID: "ratm_existing",
			ToolStepID: "tstep_cached", ToolKind: TurnToolMCP,
		})
		require.NoError(t, err, "a cached result must not require a configured executor")
		require.Contains(t, result.FatalError, "capacity")
		requireBoundedTransport(t, result)
	}
	require.Equal(t, domain.ToolStepCompleted, journal.step.State)
	require.Len(t, journal.step.Result.Content[0].(map[string]any)["text"], 400_000)
}

func TestPayloadLimitOutcomeResponseKeepsAccounting(t *testing.T) {
	usage := domain.TokenUsage{InputTokens: 123, OutputTokens: 42, Speed: "fast", ProviderRegion: "us"}
	probe := &advisorProbeClient{response: model.Response{Usage: usage, StopReason: "refusal",
		Content: []domain.ContentBlock{{Type: "text", Text: `{"result":"satisfied","explanation":"` + strings.Repeat("<", 400_000) + `"}`}},
	}}
	result, err := NewActivities(probe, nil, nil, &testIDGen{}).EvaluateOutcome(context.Background(), EvaluateOutcomeInput{
		Model: "test", StartEventID: "sevt_grader_start", EndEventID: "sevt_grader_end",
		Outcome:   domain.OutcomeSpec{OutcomeID: "outc_test", Description: "review", Rubric: map[string]any{"type": "text", "content": "correct"}},
		Candidate: []domain.Message{{Role: domain.RoleAssistant, Content: []domain.ContentBlock{{Type: "text", Text: "small"}}}},
	})
	require.NoError(t, err)
	require.Contains(t, result.FatalError, "capacity")
	require.Equal(t, usage, result.Usage)
	require.Equal(t, "refusal", result.StopReason)
	require.Equal(t, "sevt_grader_end", result.EndEventID)
	require.Equal(t, 1, probe.calls)
	requireBoundedTransport(t, result)
}

func TestPayloadLimitPreparedChildRetainsRouting(t *testing.T) {
	primary := "sthr_primary"
	source := &threadConfiguredTranscriptFakeSource{
		configuredTranscriptFakeSource: &configuredTranscriptFakeSource{
			transcriptFakeSource: &transcriptFakeSource{fakeSource: newFakeSource([]domain.Event{{
				ID: "sevt_child_large", ThreadID: "sthr_child", Sequence: 1, Type: domain.EvUserMessage,
				Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": strings.Repeat("<", 400_000)}}},
			}})},
			session: domain.Session{ID: "sesn_child", AgentSnapshot: domain.Agent{ID: "agent_primary", Model: domain.Model{ID: "test"}}},
		},
		thread: domain.SessionThread{ID: "sthr_child", ParentThreadID: &primary, Agent: domain.Agent{ID: "agent_child", Model: domain.Model{ID: "test"}}},
	}
	result, err := NewActivities(nil, source, nil, &testIDGen{}).PrepareTurn(context.Background(), PrepareTurnInput{
		SessionID: "sesn_child", TriggerEventID: "sevt_child_large",
	})
	require.NoError(t, err)
	require.Contains(t, result.FatalError, "capacity")
	require.True(t, result.IsChild)
	require.Equal(t, "sthr_child", result.ThreadID)
	requireBoundedTransport(t, result)
}

// The SDK's in-memory tests do not enforce the server's payload limits. Run the
// actual oversized-completion fallback through a real Temporal service too.
func TestPayloadLimitCompletionRealTemporal(t *testing.T) {
	address := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if address == "" {
		t.Skip("set a Temporal service-test address")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	connection, err := client.Dial(client.Options{HostPort: address})
	require.NoError(t, err)
	defer connection.Close()
	queue := "payload-completion-" + domain.NewRandomIDGen().NewID("")
	w := worker.New(connection, queue, worker.Options{})
	w.RegisterWorkflow(payloadCompletionHarness)
	committed := make(chan CompleteWorkflowTurnInput, 3)
	w.RegisterActivityWithOptions(func(_ context.Context, input CompleteWorkflowTurnInput) (RunTurnResult, error) {
		committed <- input
		return RunTurnResult{Disposition: TurnTerminated}, nil
	}, activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn})
	require.NoError(t, w.Start())
	defer w.Stop()
	for _, mode := range []string{"complete", "terminated", "retry_exhausted"} {
		run, err := connection.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: queue + "-" + mode, TaskQueue: queue}, payloadCompletionHarness, mode)
		require.NoError(t, err)
		defer func() {
			_ = connection.TerminateWorkflow(context.Background(), run.GetID(), run.GetRunID(), "test cleanup")
		}()
		var result RunTurnResult
		require.NoError(t, run.Get(ctx, &result))
		require.Equal(t, TurnTerminated, result.Disposition)
		select {
		case input := <-committed:
			require.Equal(t, domain.StatusTerminated, input.Status)
			require.Empty(t, input.TranscriptDelta)
			requireBoundedTransport(t, input)
		case <-ctx.Done():
			t.Fatal("terminal Activity was not delivered")
		}
	}
}

func TestPayloadLimitOutcomeClosesCommittedEvaluation(t *testing.T) {
	for _, mode := range []string{"input", "completion"} {
		t.Run(mode, func(t *testing.T) {
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestWorkflowEnvironment()
			initialSize, responseSize := 400_000, 700_000
			if mode == "completion" {
				initialSize, responseSize = 100_000, 500_000
			}
			initial := []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{
				Type: "text", Text: strings.Repeat("x", initialSize),
			}}}}
			prepared := PrepareTurnResult{Request: model.Request{Model: "test", Messages: initial},
				UsesProviderTranscript: true, TranscriptDelta: initial,
				Outcome: &domain.OutcomeSpec{OutcomeID: "outc_capacity", Description: "report",
					Rubric: map[string]any{"type": "text", "content": "correct"}, MaxIterations: 1},
			}
			requireBoundedTransport(t, prepared)
			var progress []domain.EventDraft
			var committed CompleteWorkflowTurnInput
			graderCalls := 0
			registerWorkflowTurnActivities(env,
				func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) { return prepared, nil },
				func(_ context.Context, in CallModelInput) (CallModelResult, error) {
					result := CallModelResult{MessageEventID: "sevt_candidate", ModelRequestStartID: in.ModelRequestStartID, ModelRequestEndID: in.ModelRequestEndID,
						Response: model.Response{StopReason: "end_turn", Usage: domain.TokenUsage{InputTokens: 123, OutputTokens: 42},
							Content: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("x", responseSize)}}}}
					requireBoundedTransport(t, result)
					return result, nil
				}, func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
					t.Fatal("outcome must not execute a tool")
					return ExecuteToolResult{}, nil
				},
				func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
					committed = in
					return RunTurnResult{Disposition: TurnTerminated}, nil
				},
			)
			// Observe the physical batches: the start must already be committed,
			// and the fallback itself must fit without reattaching that prefix.
			env.RegisterActivityWithOptions(func(_ context.Context, in AppendWorkflowEventsInput) error {
				progress = append(progress, in.Events...)
				return nil
			}, activity.RegisterOptions{Name: ActivityAppendWorkflowEvents})
			env.RegisterActivityWithOptions(func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
				committed = in
				return RunTurnResult{Disposition: TurnTerminated}, nil
			}, activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn})
			env.RegisterActivityWithOptions(func(_ context.Context, in EvaluateOutcomeInput) (EvaluateOutcomeResult, error) {
				graderCalls++
				result := EvaluateOutcomeResult{StartEventID: in.StartEventID, EndEventID: in.EndEventID,
					Result: "satisfied", Explanation: strings.Repeat("x", 500_000),
					Usage: domain.TokenUsage{InputTokens: 13, OutputTokens: 7, Speed: "fast"}}
				requireBoundedTransport(t, result)
				return result, nil
			}, activity.RegisterOptions{Name: ActivityEvaluateOutcome})
			env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{SessionID: "sesn_capacity", TriggerEventID: "sevt_outcome"})
			require.NoError(t, env.GetWorkflowError())
			start := draftOfType(t, progress, domain.EvSpanOutcomeEvaluationStart)
			require.NotEmpty(t, start.ID)
			end := draftOfType(t, committed.Output, domain.EvSpanOutcomeEvaluationEnd)
			require.Equal(t, "outc_capacity", end.Payload["outcome_id"])
			require.Equal(t, "failed", end.Payload["result"])
			require.Equal(t, start.ID, end.Payload["outcome_evaluation_start_id"])
			require.Contains(t, end.Payload["explanation"], "capacity")
			require.Equal(t, domain.StatusTerminated, committed.Status)
			require.Empty(t, committed.TranscriptDelta)
			requireBoundedTransport(t, committed)
			wantCalls, wantInput, wantOutput := 0, int64(123), int64(42)
			if mode == "completion" {
				wantCalls, wantInput, wantOutput = 1, 136, 49
				require.EqualValues(t, 13, end.Payload["usage"].(map[string]any)["input_tokens"])
				require.Equal(t, "fast", end.Payload["usage"].(map[string]any)["speed"])
			}
			require.Equal(t, wantCalls, graderCalls)
			require.Equal(t, wantInput, committed.Usage.InputTokens)
			require.Equal(t, wantOutput, committed.Usage.OutputTokens)
		})
	}
}

func TestPayloadLimitPreparedOutcomeRetainsFailureIdentity(t *testing.T) {
	trigger := domain.Event{ID: "sevt_outcome", Sequence: 1, Type: domain.EvUserDefineOutcome,
		Payload: map[string]any{"outcome_id": "outc_capacity", "description": "report",
			"rubric": map[string]any{"type": "text", "content": strings.Repeat("<", 200_000)}},
	}
	source := &outcomePrepareSource{fakeSource: newFakeSource([]domain.Event{trigger}),
		session: domain.Session{ID: "sesn_capacity", Status: domain.StatusRunning,
			AgentSnapshot: domain.Agent{Model: domain.Model{ID: "test"}},
			Outcomes:      []domain.OutcomeEvaluation{{OutcomeID: "outc_capacity", Result: "running"}}},
	}
	prepared, err := NewActivities(nil, source, nil, &testIDGen{}).PrepareTurn(context.Background(),
		PrepareTurnInput{SessionID: "sesn_capacity", TriggerEventID: trigger.ID})
	require.NoError(t, err)
	require.Contains(t, prepared.FatalError, "capacity")
	end := draftOfType(t, prepared.PreludeEvents, domain.EvSpanOutcomeEvaluationEnd)
	require.Equal(t, "outc_capacity", end.Payload["outcome_id"])
	require.Equal(t, "failed", end.Payload["result"])
	require.Empty(t, end.Payload["outcome_evaluation_start_id"], "no evaluation began")
	requireBoundedTransport(t, prepared)
}

func TestPayloadLimitOutcomeFailureBoundsProviderMetadata(t *testing.T) {
	graded := EvaluateOutcomeResult{StartEventID: "sevt_start", EndEventID: "sevt_end", Result: "satisfied",
		Usage: domain.TokenUsage{InputTokens: 13, OutputTokens: 7, Speed: strings.Repeat("x", 1024*1024-512)}}
	requireBoundedTransport(t, graded)
	state := &workflowTurnState{outcomeID: "outc_capacity", output: []domain.EventDraft{
		outcomeEvaluationEndDraft(domain.OutcomeSpec{OutcomeID: "outc_capacity"}, 0, graded),
	}}
	failure := state.terminalOutcomeFailure("runtime content capacity exceeded")
	require.EqualValues(t, 13, failure.Payload["usage"].(map[string]any)["input_tokens"])
	require.EqualValues(t, 7, failure.Payload["usage"].(map[string]any)["output_tokens"])
	requireBoundedTransport(t, CompleteWorkflowTurnInput{SessionID: "sesn_capacity", TriggerEventID: "sevt_outcome",
		Status: domain.StatusTerminated, Output: []domain.EventDraft{failure,
			{Type: domain.EvSessionError, Payload: map[string]any{"error": map[string]any{"type": "unknown_error", "message": "runtime content capacity exceeded", "retry_status": map[string]any{"type": "terminal"}}}},
			{Type: domain.EvSessionStatusTerminated, Payload: map[string]any{}},
		},
	})
}

func payloadPermissionInputHarness(ctx workflow.Context) (RunTurnResult, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
	state := &workflowTurnState{actx: ctx, sessionID: "sesn_permission", triggerEventID: "sevt_task"}
	use := domain.ContentBlock{Type: "tool_use", ToolUseID: "provider_read", ToolName: "read", Input: map[string]any{"path": "README.md"}}
	prepared := PrepareTurnResult{AttemptID: "ratm_established", Request: model.Request{Model: "test", Tools: []model.ToolSchema{{Name: "read"}}}}
	_, _, err := state.evaluateToolPermissions(prepared, []domain.ContentBlock{use},
		map[string]TurnTool{"read": {Name: "read", Permission: domain.PermissionPolicy{Type: "auto"}}},
		map[string]PlannedToolStep{"provider_read": {ToolUseEventID: "sevt_read"}},
		[]domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("<", 400_000)}}}},
	)
	if err == nil {
		return RunTurnResult{}, fmt.Errorf("oversized permission context was evaluated")
	}
	return state.terminateCapacity(err)
}

func TestPayloadLimitPermissionInputKeepsEstablishedAttemptWithoutEvaluation(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	var committed CompleteWorkflowTurnInput
	evaluations := 0
	env.RegisterActivityWithOptions(func(context.Context, domain.ToolPermissionOwner) (EnsureToolPermissionAttemptResult, error) {
		return EnsureToolPermissionAttemptResult{Established: true}, nil
	}, activity.RegisterOptions{Name: ActivityEnsureToolPermissionAttempt})
	env.RegisterActivityWithOptions(func(context.Context, EvaluateToolPermissionInput) (EvaluateToolPermissionResult, error) {
		evaluations++
		return EvaluateToolPermissionResult{}, nil
	}, activity.RegisterOptions{Name: ActivityEvaluateToolPermission})
	env.RegisterActivityWithOptions(func(_ context.Context, input CompleteWorkflowTurnInput) (RunTurnResult, error) {
		committed = input
		return RunTurnResult{Disposition: TurnTerminated}, nil
	}, activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn})
	env.ExecuteWorkflow(payloadPermissionInputHarness)
	require.NoError(t, env.GetWorkflowError())
	require.Zero(t, evaluations)
	require.Equal(t, "ratm_established", committed.AttemptID)
	require.Equal(t, domain.RunAttemptFailed, committed.AttemptState)
	require.Contains(t, *committed.AttemptError, "permission context")
	require.Equal(t, domain.StatusTerminated, committed.Status)
	requireBoundedTransport(t, committed)
}
