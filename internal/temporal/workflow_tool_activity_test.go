package temporal

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

type advisorProbeClient struct {
	mu       sync.Mutex
	calls    int
	request  model.Request
	response model.Response
	err      error
}

func (c *advisorProbeClient) CreateMessage(
	_ context.Context,
	request model.Request,
) (model.Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.request = request
	return c.response, c.err
}

func (c *advisorProbeClient) CreateMessageStream(
	ctx context.Context,
	request model.Request,
	_ func(int, string),
) (model.Response, error) {
	return c.CreateMessage(ctx, request)
}

type advisorActivitySource struct {
	*fakeSource
	journal      *memoryMCPJournal
	mu           sync.Mutex
	calls        int
	stepID       string
	result       domain.ToolStepResult
	consultation domain.AdvisorConsultation
}

func (s *advisorActivitySource) CompleteAdvisorToolStep(
	ctx context.Context,
	_ string,
	_ string,
	_ string,
	stepID string,
	result domain.ToolStepResult,
	consultation domain.AdvisorConsultation,
) error {
	if err := s.journal.CompleteToolStep(ctx, stepID, result); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.stepID = stepID
	s.result = result
	s.consultation = consultation
	return nil
}

func advisorExecuteInput() ExecuteToolInput {
	return ExecuteToolInput{
		SessionID: "sesn_advisor", ThreadID: "sthr_primary",
		TriggerEventID: "sevt_trigger", AttemptID: "ratm_advisor",
		Ordinal: 0, ToolUseEventID: "sevt_tool_advisor",
		ToolStepID: "tstep_advisor", ToolName: "advisor",
		ToolKind: TurnToolAdvisor, Input: map[string]any{},
		AdvisorRequest: model.Request{
			Model: "reviewer-model", System: "review independently", MaxTokens: 2048,
			Messages: []domain.Message{{
				Role:    domain.RoleUser,
				Content: []domain.ContentBlock{{Type: "text", Text: "quoted context"}},
			}},
		},
		AdvisorConsultation: domain.AdvisorConsultation{
			ThreadID: "sthr_advisor", UsageRequestID: "sevt_advisor_usage",
			LifecycleIDs: []string{
				"sevt_a", "sevt_b", "sevt_c", "sevt_d", "sevt_e",
				"sevt_f", "sevt_g", "sevt_h", "sevt_i",
			},
			Model: "reviewer-model",
		},
	}
}

func TestExecuteTool_AdvisorInferenceIsDurableAndNotRepeated(t *testing.T) {
	journal := &memoryMCPJournal{}
	source := &advisorActivitySource{
		fakeSource: newFakeSource(nil),
		journal:    journal,
	}
	client := &advisorProbeClient{response: model.Response{
		Content:    []domain.ContentBlock{{Type: "text", Text: "check the shutdown race"}},
		StopReason: "end_turn",
		Usage:      domain.TokenUsage{InputTokens: 120, OutputTokens: 15},
	}}
	activities := NewActivities(client, source, journal, &testIDGen{})
	in := advisorExecuteInput()

	first, err := activities.ExecuteTool(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	second, err := activities.ExecuteTool(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Result.IsError || second.Result.IsError ||
		first.Result.Content[0].(map[string]any)["text"] != "check the shutdown race" {
		t.Fatalf("advisor results = %+v / %+v", first.Result, second.Result)
	}
	if client.calls != 1 || source.calls != 1 {
		t.Fatalf("advisor calls model=%d persistence=%d, want 1/1", client.calls, source.calls)
	}
	if len(client.request.Tools) != 0 || client.request.Model != "reviewer-model" {
		t.Fatalf("advisor request = %+v", client.request)
	}
	if !source.consultation.AdviceDelivered || !source.consultation.UsageKnown ||
		source.consultation.Usage.InputTokens != 120 ||
		source.consultation.PublicContent[0].(map[string]any)["text"] != "check the shutdown race" {
		t.Fatalf("consultation = %+v", source.consultation)
	}
}

func TestExecuteTool_AdvisorRejectsOversizedContextBeforeInference(t *testing.T) {
	journal := &memoryMCPJournal{}
	source := &advisorActivitySource{
		fakeSource: newFakeSource(nil),
		journal:    journal,
	}
	client := &advisorProbeClient{}
	activities := NewActivities(client, source, journal, &testIDGen{})
	in := advisorExecuteInput()
	in.AdvisorRequest.Messages[0].Content[0].Text = strings.Repeat("x", 700_000)

	result, err := activities.ExecuteTool(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Result.IsError || client.calls != 0 || source.calls != 1 {
		t.Fatalf("oversized advisor result=%+v model=%d persistence=%d",
			result.Result, client.calls, source.calls)
	}
	if source.consultation.StopReason != "context_limit" ||
		source.consultation.UsageKnown {
		t.Fatalf("oversized consultation = %+v", source.consultation)
	}
}

func TestExecuteTool_StartedAdvisorRecoversWithoutRepeatingInference(t *testing.T) {
	in := advisorExecuteInput()
	journal := &memoryMCPJournal{step: domain.ToolStep{
		ID: in.ToolStepID, AttemptID: in.AttemptID, Ordinal: in.Ordinal,
		ToolUseEventID: in.ToolUseEventID, ToolName: in.ToolName,
		Input: in.Input, State: domain.ToolStepStarted,
	}}
	source := &advisorActivitySource{fakeSource: newFakeSource(nil), journal: journal}
	client := &advisorProbeClient{}
	activities := NewActivities(client, source, journal, &testIDGen{})

	result, err := activities.ExecuteTool(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Result.IsError || client.calls != 0 || source.calls != 1 {
		t.Fatalf("recovered advisor result=%+v model=%d persistence=%d", result, client.calls, source.calls)
	}
	if source.consultation.UsageKnown || source.consultation.StopReason != "interrupted" {
		t.Fatalf("recovered consultation = %+v", source.consultation)
	}
}

func TestExecuteTool_CancelledAdvisorCompletesUnknownUsageWithoutReplay(t *testing.T) {
	in := advisorExecuteInput()
	journal := &memoryMCPJournal{}
	source := &advisorActivitySource{fakeSource: newFakeSource(nil), journal: journal}
	client := &advisorProbeClient{err: context.Canceled}
	activities := NewActivities(client, source, journal, &testIDGen{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := activities.ExecuteTool(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Result.IsError || client.calls != 1 || source.calls != 1 ||
		journal.step.State != domain.ToolStepCompleted {
		t.Fatalf("cancelled advisor result=%+v model=%d persistence=%d step=%s",
			result, client.calls, source.calls, journal.step.State)
	}
	if source.consultation.UsageKnown || source.consultation.StopReason != "interrupted" {
		t.Fatalf("cancelled consultation = %+v", source.consultation)
	}
}
