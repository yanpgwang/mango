package temporal

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

type outcomePrepareSource struct {
	*fakeSource
	session domain.Session
}

func (s *outcomePrepareSource) GetSession(context.Context, string) (domain.Session, error) {
	return s.session, nil
}

func TestPrepareTurnRunsReceiptProcessedActiveOutcome(t *testing.T) {
	processedAt := time.Now().UTC()
	trigger := domain.Event{
		ID: "sevt_outcome", Sequence: 1, Type: domain.EvUserDefineOutcome,
		Payload: map[string]any{
			"outcome_id": "outc_1", "description": "produce report",
			"rubric":         map[string]any{"type": "text", "content": "has evidence"},
			"max_iterations": 3,
		},
		ProcessedAt: &processedAt,
	}
	source := &outcomePrepareSource{
		fakeSource: newFakeSource([]domain.Event{trigger}),
		session: domain.Session{
			ID: "sess_outcome", Status: domain.StatusRunning,
			AgentSnapshot: domain.Agent{Model: domain.Model{ID: "model"}},
			Outcomes: []domain.OutcomeEvaluation{{
				OutcomeID: "outc_1", Description: "produce report", Result: "running",
			}},
		},
	}
	prepared, err := NewActivities(
		nil, source, nil, &testIDGen{},
	).PrepareTurn(context.Background(), PrepareTurnInput{
		SessionID: "sess_outcome", TriggerEventID: trigger.ID,
	})
	require.NoError(t, err)
	require.False(t, prepared.AlreadyCompleted)
	require.NotNil(t, prepared.Outcome)
	require.Equal(t, "outc_1", prepared.Outcome.OutcomeID)

	source.session.Outcomes[0].Result = "satisfied"
	prepared, err = NewActivities(
		nil, source, nil, &testIDGen{},
	).PrepareTurn(context.Background(), PrepareTurnInput{
		SessionID: "sess_outcome", TriggerEventID: trigger.ID,
	})
	require.NoError(t, err)
	require.True(t, prepared.AlreadyCompleted)
}

func TestPrepareTurnFileOutcomeRubricMatchesInlineWorkingAndGraderInputs(t *testing.T) {
	processedAt := time.Now().UTC()
	const rubricContent = "# Rubric\n- cites evidence\n- produces report.md"
	basePayload := map[string]any{
		"outcome_id": "outc_1", "description": "produce report",
		"max_iterations": float64(3),
	}
	inlinePayload := make(map[string]any, len(basePayload)+1)
	for key, value := range basePayload {
		inlinePayload[key] = value
	}
	inlinePayload["rubric"] = map[string]any{
		"type": "text", "content": rubricContent,
	}
	filePayload := make(map[string]any, len(basePayload)+1)
	for key, value := range basePayload {
		filePayload[key] = value
	}
	filePayload["rubric"] = map[string]any{
		"type": "file", "file_id": "file_rubric",
	}
	filePayload = domain.WithOutcomeRubricContent(filePayload, rubricContent)

	prepare := func(payload map[string]any) PrepareTurnResult {
		trigger := domain.Event{
			ID: "sevt_outcome", Sequence: 1, Type: domain.EvUserDefineOutcome,
			Payload: payload, ProcessedAt: &processedAt,
		}
		source := &outcomePrepareSource{
			fakeSource: newFakeSource([]domain.Event{trigger}),
			session: domain.Session{
				ID: "sess_outcome", Status: domain.StatusRunning,
				AgentSnapshot: domain.Agent{Model: domain.Model{ID: "model"}},
				Outcomes: []domain.OutcomeEvaluation{{
					OutcomeID: "outc_1", Description: "produce report", Result: "running",
				}},
			},
		}
		prepared, err := NewActivities(
			nil, source, nil, &testIDGen{},
		).PrepareTurn(context.Background(), PrepareTurnInput{
			SessionID: "sess_outcome", TriggerEventID: trigger.ID,
		})
		require.NoError(t, err)
		return prepared
	}

	inline := prepare(inlinePayload)
	file := prepare(filePayload)
	require.Equal(t, inline.Request.Messages, file.Request.Messages)
	require.Equal(t, inline.Outcome, file.Outcome)
	require.Equal(t, map[string]any{
		"type": "text", "content": rubricContent,
	}, file.Outcome.Rubric)
	inlinePrompt, err := outcomeEvaluationPrompt(*inline.Outcome, inline.Request.Messages, 0)
	require.NoError(t, err)
	filePrompt, err := outcomeEvaluationPrompt(*file.Outcome, file.Request.Messages, 0)
	require.NoError(t, err)
	require.Equal(t, inlinePrompt, filePrompt)
}

type outcomeModel struct {
	request  model.Request
	response model.Response
}

func (m *outcomeModel) CreateMessage(_ context.Context, request model.Request) (model.Response, error) {
	m.request = request
	return m.response, nil
}

func (m *outcomeModel) CreateMessageStream(
	ctx context.Context,
	request model.Request,
	onDelta func(int, string),
) (model.Response, error) {
	response, err := m.CreateMessage(ctx, request)
	if err == nil && onDelta != nil {
		for _, block := range response.Content {
			if block.Type == "text" {
				onDelta(0, block.Text)
			}
		}
	}
	return response, err
}

func (m *outcomeModel) CreateMessageStreamWithCallbacks(
	ctx context.Context,
	request model.Request,
	callbacks model.StreamCallbacks,
) (model.Response, error) {
	response, err := m.CreateMessage(ctx, request)
	if err != nil {
		return response, err
	}
	thinkingStarted := false
	for index, block := range response.Content {
		if !thinkingStarted && (block.Type == "thinking" || block.Type == "redacted_thinking") {
			if callbacks.OnThinkingStart != nil {
				callbacks.OnThinkingStart()
			}
			thinkingStarted = true
		}
		if block.Type == "text" && callbacks.OnTextDelta != nil {
			callbacks.OnTextDelta(index, block.Text)
		}
	}
	return response, nil
}

func TestEvaluateOutcomeUsesIsolatedGraderContext(t *testing.T) {
	client := &outcomeModel{response: model.Response{
		Content: []domain.ContentBlock{{
			Type: "text",
			Text: `{"result":"satisfied","explanation":"all acceptance criteria are met"}`,
		}},
		Usage: domain.TokenUsage{InputTokens: 17, OutputTokens: 8},
	}}
	activities := NewActivities(
		client, nil, nil, domain.NewSeqIDGen(),
	)

	got, err := activities.EvaluateOutcome(context.Background(), EvaluateOutcomeInput{
		SessionID:    "sess_1",
		StartEventID: "sevt_supplied_start",
		EndEventID:   "sevt_supplied_end",
		Model:        "claude-sonnet",
		Effort:       "high",
		Speed:        "standard",
		Outcome: domain.OutcomeSpec{
			OutcomeID: "outc_1", Description: "ship the report",
			Rubric:        map[string]any{"type": "text", "content": "contains evidence"},
			MaxIterations: 3,
		},
		Candidate: []domain.Message{{
			Role:    domain.RoleAssistant,
			Content: []domain.ContentBlock{{Type: "text", Text: "report with evidence"}},
		}},
	})
	require.NoError(t, err)
	require.Equal(t, "satisfied", got.Result)
	require.Equal(t, int64(17), got.Usage.InputTokens)
	require.Equal(t, "sevt_supplied_start", got.StartEventID)
	require.Equal(t, "sevt_supplied_end", got.EndEventID)

	request := client.request
	require.Equal(t, outcomeGraderSystem+" Return exactly one JSON object with "+
		`{"result":"satisfied|needs_revision|failed","explanation":"..."}.`, request.System)
	require.Len(t, request.Messages, 1)
	require.Equal(t, domain.RoleUser, request.Messages[0].Role)
	require.Contains(t, request.Messages[0].Content[0].Text, "ship the report")
}

func TestEvaluateOutcomeRejectsOversizedIsolatedContextBeforeInference(t *testing.T) {
	client := &outcomeModel{response: model.Response{
		Content: []domain.ContentBlock{{
			Type: "text", Text: `{"result":"satisfied","explanation":"unused"}`,
		}},
	}}
	activities := NewActivities(client, nil, nil, domain.NewSeqIDGen())

	got, err := activities.EvaluateOutcome(context.Background(), EvaluateOutcomeInput{
		Model: "unknown-model",
		Outcome: domain.OutcomeSpec{
			OutcomeID: "outc_large", Description: "review the oversized candidate",
			Rubric: map[string]any{"type": "text", "content": "be complete"},
		},
		Candidate: []domain.Message{{
			Role: domain.RoleAssistant,
			Content: []domain.ContentBlock{{
				Type: "text", Text: strings.Repeat("x", 700_000),
			}},
		}},
	})

	require.NoError(t, err)
	require.Contains(t, got.FatalError, "context is too large")
	require.Empty(t, client.request.Model,
		"outcome grader must reject its own oversized request before inference")
}

func TestWorkflowTurnEvaluatesOutcomeAndAccountsUsage(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			Request: model.Request{
				Model: "claude-sonnet", Effort: "high", Speed: "standard",
				Messages: []domain.Message{{
					Role:    domain.RoleUser,
					Content: []domain.ContentBlock{{Type: "text", Text: "produce report"}},
				}},
			},
			Outcome: &domain.OutcomeSpec{
				OutcomeID: "outc_1", Description: "produce report",
				Rubric:        map[string]any{"type": "text", "content": "has evidence"},
				MaxIterations: 3,
			},
		}, nil
	}
	callModel := func(_ context.Context, in CallModelInput) (CallModelResult, error) {
		return CallModelResult{
			MessageEventID:      "sevt_answer",
			ModelRequestStartID: in.ModelRequestStartID,
			ModelRequestEndID:   in.ModelRequestEndID,
			Response: model.Response{
				StopReason: "end_turn",
				Content:    []domain.ContentBlock{{Type: "text", Text: "finished report"}},
				Usage: domain.TokenUsage{
					InputTokens: 10, OutputTokens: 4, Speed: "standard",
				},
			},
		}, nil
	}
	evaluate := func(context.Context, EvaluateOutcomeInput) (EvaluateOutcomeResult, error) {
		return EvaluateOutcomeResult{
			StartEventID: "sevt_eval_start", EndEventID: "sevt_eval_end",
			Result: "satisfied", Explanation: "rubric met",
			Usage: domain.TokenUsage{InputTokens: 3, OutputTokens: 2, Speed: "fast"},
		}, nil
	}
	executeTool := func(context.Context, ExecuteToolInput) (ExecuteToolResult, error) {
		t.Fatal("outcome without tool_use must not execute a tool")
		return ExecuteToolResult{}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnCompleted}, nil
	}
	registerWorkflowTurnActivities(env, prepare, callModel, executeTool, complete)
	env.RegisterActivityWithOptions(evaluate, activity.RegisterOptions{Name: ActivityEvaluateOutcome})

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_1", TriggerEventID: "sevt_outcome",
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{
		domain.EvAgentMessage,
		domain.EvSpanModelRequestEnd,
		domain.EvSpanOutcomeEvaluationStart,
		domain.EvSpanOutcomeEvaluationEnd,
		domain.EvSessionStatusIdle,
	}, draftTypes(completed.Output))
	require.Equal(t, int64(13), completed.Usage.InputTokens)
	require.Equal(t, int64(6), completed.Usage.OutputTokens)
	start := completed.Output[2]
	end := completed.Output[3].Payload
	require.NotEmpty(t, start.ID)
	require.Equal(t, start.ID, end["outcome_evaluation_start_id"])
	require.Equal(t, "satisfied", end["result"])
	require.Equal(t, "fast", end["usage"].(map[string]any)["speed"])
	modelEnd := completed.Output[1].Payload["model_usage"].(map[string]any)
	require.Equal(t, "standard", modelEnd["speed"])
}

func TestWorkflowTurnClosesStartedOutcomeEvaluationOnFatalGraderResult(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterWorkflow(workflowTurnHarness)

	prepare := func(context.Context, PrepareTurnInput) (PrepareTurnResult, error) {
		return PrepareTurnResult{
			Request: model.Request{
				Model: "claude-sonnet",
				Messages: []domain.Message{{
					Role:    domain.RoleUser,
					Content: []domain.ContentBlock{{Type: "text", Text: "produce report"}},
				}},
			},
			Outcome: &domain.OutcomeSpec{
				OutcomeID: "outc_failed", Description: "produce report",
				Rubric:        map[string]any{"type": "text", "content": "has evidence"},
				MaxIterations: 1,
			},
		}, nil
	}
	callModel := func(_ context.Context, in CallModelInput) (CallModelResult, error) {
		return CallModelResult{
			MessageEventID:      "sevt_candidate",
			ModelRequestStartID: in.ModelRequestStartID,
			ModelRequestEndID:   in.ModelRequestEndID,
			Response: model.Response{
				StopReason: "end_turn",
				Content:    []domain.ContentBlock{{Type: "text", Text: "report"}},
				Usage:      domain.TokenUsage{InputTokens: 5, OutputTokens: 2},
			},
		}, nil
	}
	evaluate := func(context.Context, EvaluateOutcomeInput) (EvaluateOutcomeResult, error) {
		return EvaluateOutcomeResult{
			FatalError: "grader returned an invalid verdict",
			Usage:      domain.TokenUsage{InputTokens: 3, OutputTokens: 1},
		}, nil
	}
	var completed CompleteWorkflowTurnInput
	complete := func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
		completed = in
		return RunTurnResult{Disposition: TurnTerminated}, nil
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
	env.RegisterActivityWithOptions(evaluate, activity.RegisterOptions{Name: ActivityEvaluateOutcome})

	env.ExecuteWorkflow(workflowTurnHarness, PrepareTurnInput{
		SessionID: "sess_failed_outcome", TriggerEventID: "sevt_outcome",
	})
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, domain.StatusTerminated, completed.Status)
	require.Equal(t, []string{
		domain.EvAgentMessage,
		domain.EvSpanModelRequestEnd,
		domain.EvSpanOutcomeEvaluationStart,
		domain.EvSpanOutcomeEvaluationEnd,
		domain.EvSessionError,
		domain.EvSessionStatusTerminated,
	}, draftTypes(completed.Output))
	start := completed.Output[2]
	end := completed.Output[3]
	require.Equal(t, start.ID, end.Payload["outcome_evaluation_start_id"])
	require.Equal(t, "failed", end.Payload["result"])
	require.Equal(t, "grader returned an invalid verdict", end.Payload["explanation"])
	require.Equal(t, int64(8), completed.Usage.InputTokens)
	require.Equal(t, int64(3), completed.Usage.OutputTokens)
}

func outcomeHeartbeatHarness(
	ctx workflow.Context,
	input EvaluateOutcomeInput,
) (EvaluateOutcomeResult, error) {
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Second,
	})
	turn := workflowTurnState{
		actx:                     actx,
		sessionID:                input.SessionID,
		triggerEventID:           "sevt_outcome",
		outcomeHeartbeats:        true,
		outcomeHeartbeatInterval: 10 * time.Millisecond,
	}
	evaluated, _, err := turn.evaluateOutcome(input)
	return evaluated, err
}

func TestEvaluateOutcomePublishesHeartbeatWhileGraderRuns(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.SetTestTimeout(5 * time.Second)
	env.RegisterWorkflow(outcomeHeartbeatHarness)

	releaseGrader := make(chan struct{})
	var releaseOnce sync.Once
	var mu sync.Mutex
	var progress []domain.EventDraft
	env.RegisterActivityWithOptions(
		func(_ context.Context, in AppendWorkflowEventsInput) error {
			mu.Lock()
			progress = append(progress, in.Events...)
			mu.Unlock()
			for _, event := range in.Events {
				if event.Type == domain.EvSpanOutcomeEvaluationOngoing {
					releaseOnce.Do(func() { close(releaseGrader) })
				}
			}
			return nil
		},
		activity.RegisterOptions{Name: ActivityAppendWorkflowEvents},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, in EvaluateOutcomeInput) (EvaluateOutcomeResult, error) {
			select {
			case <-releaseGrader:
			case <-ctx.Done():
				return EvaluateOutcomeResult{}, ctx.Err()
			}
			return EvaluateOutcomeResult{
				StartEventID: in.StartEventID, EndEventID: in.EndEventID,
				Result: "satisfied", Explanation: "rubric met",
			}, nil
		},
		activity.RegisterOptions{Name: ActivityEvaluateOutcome},
	)

	env.ExecuteWorkflow(outcomeHeartbeatHarness, EvaluateOutcomeInput{
		SessionID:    "sess_heartbeat",
		StartEventID: "sevt_eval_start",
		EndEventID:   "sevt_eval_end",
		Model:        "claude-sonnet",
		Outcome: domain.OutcomeSpec{
			OutcomeID: "outc_heartbeat", Description: "produce report",
			Rubric: map[string]any{"type": "text", "content": "has evidence"},
		},
		Iteration: 2,
	})
	require.NoError(t, env.GetWorkflowError())
	var evaluated EvaluateOutcomeResult
	require.NoError(t, env.GetWorkflowResult(&evaluated))
	require.Equal(t, "sevt_eval_start", evaluated.StartEventID)
	require.Equal(t, "sevt_eval_end", evaluated.EndEventID)

	mu.Lock()
	committedProgress := append([]domain.EventDraft(nil), progress...)
	mu.Unlock()
	heartbeat := draftOfType(t, committedProgress, domain.EvSpanOutcomeEvaluationOngoing)
	require.Equal(t, outcomeEvaluationHeartbeatID("sevt_eval_start", 0), heartbeat.ID)
	require.Equal(t, "outc_heartbeat", heartbeat.Payload["outcome_id"])
	require.Equal(t, float64(2), heartbeat.Payload["iteration"])
}
