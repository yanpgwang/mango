package temporal

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

func TestCallModelPublishesCorrelatedPreviewFrames(t *testing.T) {
	publisher := &previewRecorder{}
	activities := NewActivities(
		model.NewFake(),
		nil,
		nil,

		domain.NewSeqIDGen(),
		publisher,
	)
	result, err := activities.CallModel(context.Background(), CallModelInput{
		SessionID: "sesn_1",
		Request: model.Request{Messages: []domain.Message{{
			Role: domain.RoleUser,
			Content: []domain.ContentBlock{{
				Type: "text", Text: "hello",
			}},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	frames := publisher.snapshot()
	if len(frames) < 3 {
		t.Fatalf("preview frames = %d, want start plus at least two deltas", len(frames))
	}
	if frames[0].sessionID != "sesn_1" ||
		frames[0].frame.Kind != domain.PreviewEventStart ||
		frames[0].frame.EventID != result.MessageEventID ||
		frames[0].frame.ModelRequestStartID != result.ModelRequestStartID {
		t.Fatalf("start frame = %+v, result id=%s", frames[0], result.MessageEventID)
	}
	var text strings.Builder
	for _, frame := range frames[1:] {
		if frame.frame.Kind != domain.PreviewEventDelta {
			t.Fatalf("non-delta frame after start: %+v", frame)
		}
		if frame.frame.EventID != result.MessageEventID {
			t.Fatalf("delta id = %s, want %s", frame.frame.EventID, result.MessageEventID)
		}
		if frame.frame.ModelRequestStartID != result.ModelRequestStartID {
			t.Fatalf(
				"delta model request id = %s, want %s",
				frame.frame.ModelRequestStartID,
				result.ModelRequestStartID,
			)
		}
		text.WriteString(frame.frame.Text)
	}
	if got, want := text.String(), "echo: hello"; got != want {
		t.Fatalf("preview text = %q, want %q", got, want)
	}
}

func TestCallModelPermanentAPIErrorBecomesFatalResult(t *testing.T) {
	client := model.NewFake()
	client.SetError(&model.APIError{
		Kind:       model.ErrorInvalidRequest,
		StatusCode: 400,
		Type:       "invalid_request_error",
		Message:    "invalid messages",
	})
	activities := NewActivities(
		client, nil, nil, domain.NewSeqIDGen(),
	)

	result, err := activities.CallModel(context.Background(), CallModelInput{
		SessionID: "sesn_permanent",
		Request:   model.Request{Model: "test-model"},
	})
	if err != nil {
		t.Fatalf("CallModel returned Activity error for permanent failure: %v", err)
	}
	if result.FatalError == "" {
		t.Fatal("CallModel returned no FatalError for permanent failure")
	}
	if result.FatalErrorType != "model_request_failed_error" {
		t.Fatalf("FatalErrorType = %q", result.FatalErrorType)
	}
}

func TestCallModelRequestTooLargeBecomesContextRecoveryResult(t *testing.T) {
	client := model.NewFake()
	client.SetError(&model.APIError{
		Kind:       model.ErrorRequestTooLarge,
		StatusCode: 413,
		Type:       "request_too_large",
		Message:    "request exceeds context window",
	})
	activities := NewActivities(client, nil, nil, domain.NewSeqIDGen())

	result, err := activities.CallModel(context.Background(), CallModelInput{
		SessionID: "sesn_context_overflow",
		Request:   model.Request{Model: "test-model"},
	})
	if err != nil {
		t.Fatalf("CallModel returned Activity error: %v", err)
	}
	if !result.ContextOverflow || result.ContextOverflowError == "" {
		t.Fatalf("context overflow result = %#v", result)
	}
	if result.FatalError != "" || result.RetryError != nil {
		t.Fatalf("context overflow entered the ordinary failure path: %#v", result)
	}
	if result.ModelRequestStartID == "" || result.ModelRequestEndID == "" {
		t.Fatal("context overflow result is missing model span ids")
	}
}

func TestCallModelBillingErrorUsesDocumentedVariant(t *testing.T) {
	client := model.NewFake()
	client.SetError(&model.APIError{
		Kind:       model.ErrorBilling,
		StatusCode: 402,
		Type:       "billing_error",
		Message:    "credits exhausted",
	})
	activities := NewActivities(client, nil, nil, domain.NewSeqIDGen())
	result, err := activities.CallModel(context.Background(), CallModelInput{
		SessionID: "sesn_billing", Request: model.Request{Model: "test-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.FatalErrorType != "billing_error" || result.FatalError == "" {
		t.Fatalf("billing result = %#v", result)
	}
}

func TestCallModelAllocatesPublicThinkingEventWithoutExposingContent(t *testing.T) {
	client := &outcomeModel{response: model.Response{
		Content: []domain.ContentBlock{
			{Type: "thinking", Text: "private reasoning"},
			{Type: "text", Text: "public answer"},
		},
		StopReason: "end_turn",
	}}
	publisher := &previewRecorder{}
	activities := NewActivities(client, nil, nil, domain.NewSeqIDGen(), publisher)
	result, err := activities.CallModel(context.Background(), CallModelInput{
		SessionID: "sesn_thinking", Request: model.Request{Model: "test-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ThinkingEventID == "" {
		t.Fatal("thinking response has no public event id")
	}
	if result.MessageEventID == "" {
		t.Fatal("text response has no public message event id")
	}
	frames := publisher.snapshot()
	var thinkingStarts int
	for _, frame := range frames {
		if frame.frame.EventType != domain.EvAgentThinking {
			continue
		}
		thinkingStarts++
		if frame.frame.Kind != domain.PreviewEventStart ||
			frame.frame.EventID != result.ThinkingEventID ||
			frame.frame.ModelRequestStartID != result.ModelRequestStartID ||
			frame.frame.Text != "" {
			t.Fatalf("thinking preview = %#v, result = %#v", frame.frame, result)
		}
	}
	if thinkingStarts != 1 {
		t.Fatalf("thinking preview starts = %d, want 1", thinkingStarts)
	}
}

func TestCallModelTransientAPIErrorRemainsActivityError(t *testing.T) {
	client := model.NewFake()
	want := &model.APIError{
		Kind:       model.ErrorOverloaded,
		StatusCode: 529,
		Type:       "overloaded_error",
		Message:    "try again",
	}
	client.SetError(want)
	activities := NewActivities(
		client, nil, nil, domain.NewSeqIDGen(),
	)

	result, err := activities.CallModel(context.Background(), CallModelInput{
		SessionID: "sesn_transient",
		Request:   model.Request{Model: "test-model"},
	})
	if err == nil {
		t.Fatal("CallModel returned nil Activity error for transient failure")
	}
	var got *model.APIError
	if !errors.As(err, &got) || got.Kind != model.ErrorOverloaded {
		t.Fatalf("Activity error = %#v, want overloaded APIError", err)
	}
	if result.FatalError != "" {
		t.Fatalf("FatalError = %q, want empty for transient failure", result.FatalError)
	}
}

func TestCallModelTransientAPIErrorBecomesRetryResultWhenOptedIn(t *testing.T) {
	client := model.NewFake()
	client.SetError(&model.APIError{
		Kind:       model.ErrorRateLimit,
		StatusCode: 429,
		Type:       "rate_limit_error",
		Message:    "slow down",
		RetryAfter: 2500 * time.Millisecond,
	})
	activities := NewActivities(
		client, nil, nil, domain.NewSeqIDGen(),
	)

	result, err := activities.CallModel(context.Background(), CallModelInput{
		SessionID:             "sesn_retry_result",
		HandleRetryableErrors: true,
		Request:               model.Request{Model: "test-model"},
	})
	if err != nil {
		t.Fatalf("CallModel returned Activity error: %v", err)
	}
	if result.RetryError == nil {
		t.Fatal("CallModel returned no RetryError")
	}
	if result.RetryError.Type != "model_rate_limited_error" {
		t.Fatalf("RetryError.Type = %q", result.RetryError.Type)
	}
	if result.RetryError.RetryAfterMillis != 2500 {
		t.Fatalf("RetryAfterMillis = %d", result.RetryError.RetryAfterMillis)
	}
	if result.ModelRequestStartID == "" || result.ModelRequestEndID == "" {
		t.Fatal("retry result is missing model span correlation ids")
	}
}

func TestCompleteWorkflowTurnForwardsPendingBarrierIDs(t *testing.T) {
	source := newFakeSource(nil)
	activities := NewActivities(
		nil,
		source,
		nil,

		domain.NewSeqIDGen(),
	)
	wantPending := []string{"sevt_action_1", "sevt_action_2"}
	wantResolved := []string{"sevt_result_1", "sevt_result_2"}
	result, err := activities.CompleteWorkflowTurn(
		context.Background(),
		CompleteWorkflowTurnInput{
			SessionID:             "sesn_pending",
			TriggerEventID:        "sevt_trigger",
			Status:                domain.StatusIdle,
			PendingActionEventIDs: wantPending,
			ResolutionEventIDs:    wantResolved,
		},
	)
	if err != nil {
		t.Fatalf("CompleteWorkflowTurn: %v", err)
	}
	if result.Disposition != TurnParked {
		t.Fatalf(
			"legacy completion disposition = %s, want %s",
			result.Disposition,
			TurnParked,
		)
	}
	source.mu.Lock()
	gotPending := append([]string(nil), source.pending["sevt_trigger"]...)
	gotResolved := append([]string(nil), source.resolved["sevt_trigger"]...)
	source.mu.Unlock()
	if len(gotPending) != len(wantPending) ||
		gotPending[0] != wantPending[0] ||
		gotPending[1] != wantPending[1] {
		t.Fatalf("pending action ids = %v, want %v", gotPending, wantPending)
	}
	if len(gotResolved) != len(wantResolved) ||
		gotResolved[0] != wantResolved[0] ||
		gotResolved[1] != wantResolved[1] {
		t.Fatalf("resolution event ids = %v, want %v", gotResolved, wantResolved)
	}

	// New PostgreSQL results explicitly override the requested barrier. This is
	// the interrupt-wins case: the Workflow asked to park, but the completion
	// transaction observed an earlier interrupt and committed idle/end_turn.
	notParked := false
	source = newFakeSource(nil)
	source.completionParked = &notParked
	activities = NewActivities(
		nil,
		source,
		nil,

		domain.NewSeqIDGen(),
	)
	result, err = activities.CompleteWorkflowTurn(
		context.Background(),
		CompleteWorkflowTurnInput{
			SessionID:             "sesn_interrupted_park",
			TriggerEventID:        "sevt_trigger",
			Status:                domain.StatusIdle,
			PendingActionEventIDs: wantPending,
		},
	)
	if err != nil {
		t.Fatalf("CompleteWorkflowTurn interrupted park: %v", err)
	}
	if result.Disposition != TurnCompleted {
		t.Fatalf(
			"authoritative completion disposition = %s, want %s",
			result.Disposition,
			TurnCompleted,
		)
	}
}

type recordedPreview struct {
	sessionID string
	frame     domain.PreviewFrame
}

type previewRecorder struct {
	mu     sync.Mutex
	frames []recordedPreview
}

func (p *previewRecorder) PublishPreview(
	_ context.Context,
	sessionID string,
	frame domain.PreviewFrame,
) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frames = append(p.frames, recordedPreview{sessionID: sessionID, frame: frame})
	return nil
}

func (p *previewRecorder) snapshot() []recordedPreview {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]recordedPreview(nil), p.frames...)
}
