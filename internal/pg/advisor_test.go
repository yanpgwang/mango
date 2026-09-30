package pg

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestAdvisorConsultationsProjectThreadsEventsAndUsageIdempotently(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_advisor")
	session.AgentSnapshot = domain.Agent{
		ID: session.AgentID, Version: session.AgentVersion, Name: "coordinator",
		Model: domain.NormalizeModel(domain.Model{ID: "claude-sonnet-5"}),
		Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{
			Type: "advisor", Model: "claude-opus-5",
		}}},
	}
	session.ListCostKnown = true
	agentBody, err := json.Marshal(session.AgentSnapshot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.pool.Exec(ctx, `
INSERT INTO agents (
    id, version, name, body, created_at, updated_at, workspace_id
) VALUES ($1, $2, $3, $4, $5, $5, 'wrkspc_default')`,
		session.AgentSnapshot.ID,
		session.AgentSnapshot.Version,
		session.AgentSnapshot.Name,
		agentBody,
		session.CreatedAt,
	); err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateSession(ctx, session, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{map[string]any{
			"type": "text", "text": "review the plan",
		}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var trigger domain.Event
	for _, event := range created.Events {
		if event.Type == domain.EvUserMessage {
			trigger = event
		}
	}
	threads, err := store.ListSessionThreads(
		ctx, session.ID, app.SessionThreadListQuery{Limit: 10},
	)
	if err != nil || len(threads) != 1 {
		t.Fatalf("primary Threads = %+v, err=%v", threads, err)
	}
	primary := threads[0]
	executorUsage := domain.TokenUsage{InputTokens: 500, OutputTokens: 50, ProviderRegion: "us"}
	if err := store.AccountModelRequest(
		ctx,
		session.ID,
		primary.ID,
		"sevt_executor_usage",
		session.AgentSnapshot.Model,
		executorUsage,
		"end_turn",
	); err != nil {
		t.Fatal(err)
	}

	consultations := []domain.AdvisorConsultation{
		advisorConsultationFixture(
			"sthr_advisor_plain", "sevt_advisor_usage_plain", "plain",
			domain.TokenUsage{InputTokens: 1_000, OutputTokens: 100, ProviderRegion: "us"},
			[]any{map[string]any{"type": "text", "text": "check the shutdown race"}},
			true,
		),
		advisorConsultationFixture(
			"sthr_advisor_second", "sevt_advisor_usage_second", "second",
			domain.TokenUsage{InputTokens: 2_000, OutputTokens: 200, ProviderRegion: "us"},
			[]any{map[string]any{"type": "text", "text": "challenge the locking assumption"}},
			true,
		),
		advisorConsultationFixture(
			"sthr_advisor_failed", "sevt_advisor_usage_failed", "failed",
			domain.TokenUsage{}, nil, false,
		),
	}
	consultations[2].StopReason = "overloaded"
	if _, err := store.EnsureAttempt(
		ctx, session.ID, trigger.ID, "ratm_advisor",
	); err != nil {
		t.Fatal(err)
	}
	for index, consultation := range consultations {
		stepID := "tstep_advisor_" + []string{"plain", "second", "failed"}[index]
		if _, err := store.EnsureToolStep(
			ctx, "ratm_advisor", stepID, index,
			"sevt_tool_advisor_"+[]string{"plain", "second", "failed"}[index],
			"advisor", map[string]any{},
		); err != nil {
			t.Fatal(err)
		}
		if err := store.StartToolStep(ctx, stepID); err != nil {
			t.Fatal(err)
		}
		result := domain.ToolStepResult{
			Content: consultation.PublicContent,
			IsError: !consultation.AdviceDelivered,
		}
		if err := store.CompleteAdvisorToolStep(
			ctx, session.ID, primary.ID, trigger.ID, stepID, result, consultation,
		); err != nil {
			t.Fatal(err)
		}
		// A lost Activity acknowledgement must return the same journal result
		// without duplicating the Thread, events, usage, or Session aggregate.
		if err := store.CompleteAdvisorToolStep(
			ctx, session.ID, primary.ID, trigger.ID, stepID, result, consultation,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.CompleteWorkflowTurn(
		ctx, session.ID, trigger.ID,
		[]domain.EventDraft{{
			Type: domain.EvSessionStatusIdle,
			Payload: map[string]any{
				"stop_reason": map[string]any{"type": "end_turn"},
			},
		}},
		domain.StatusIdle,
		"ratm_advisor", domain.RunAttemptCompleted, nil, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	nextAdmission, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{map[string]any{
			"type": "text", "text": "apply the review",
		}}},
	}})
	if err != nil || len(nextAdmission.SubmittedEvents) != 1 {
		t.Fatalf("admit next turn = %+v, err=%v", nextAdmission, err)
	}
	nextTrigger := nextAdmission.SubmittedEvents[0]
	history, err := store.HistoryThrough(ctx, session.ID, nextTrigger.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	adviceEventID := consultations[0].LifecycleIDs[4]
	adviceOccurrences := 0
	for _, event := range history {
		if event.ID == adviceEventID {
			adviceOccurrences++
		}
	}
	if adviceOccurrences != 1 {
		t.Fatalf("Advisor advice appears %d times in subsequent history: %+v", adviceOccurrences, history)
	}

	threads, err = store.ListSessionThreads(
		ctx, session.ID, app.SessionThreadListQuery{Limit: 10},
	)
	if err != nil || len(threads) != 4 {
		t.Fatalf("Advisor Threads = %+v, err=%v", threads, err)
	}
	for _, thread := range threads[1:] {
		if thread.Advisor == nil || thread.Advisor.Type != "advisor" ||
			thread.Advisor.Model != "claude-opus-5" || thread.Agent.ID != "" ||
			thread.ParentThreadID == nil || *thread.ParentThreadID != primary.ID ||
			thread.Status != domain.StatusTerminated || thread.TerminatedAt == nil {
			t.Fatalf("Advisor Thread projection = %+v", thread)
		}
	}
	plain := threads[1]
	if plain.ID != "sthr_advisor_plain" || plain.Usage.InputTokens != 1_000 {
		t.Fatalf("plain Advisor Thread = %+v", plain)
	}

	accounted, err := store.GetSession(ctx, session.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantAdvisorUsage := domain.TokenUsage{InputTokens: 3_000, OutputTokens: 300, ProviderRegion: "us"}
	wantUsage := executorUsage
	wantUsage.Add(wantAdvisorUsage)
	wantAdvisorCost, err := domain.ModelUsageListCostNanoUSD(
		domain.Model{ID: "claude-opus-5"}, wantAdvisorUsage,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantExecutorCost, err := domain.ModelUsageListCostNanoUSD(
		session.AgentSnapshot.Model, executorUsage,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantCost := wantExecutorCost + wantAdvisorCost
	if accounted.Usage.InputTokens != wantUsage.InputTokens ||
		accounted.Usage.OutputTokens != wantUsage.OutputTokens ||
		accounted.ModelListCostNanoUSD != wantCost || !accounted.ListCostKnown {
		t.Fatalf("Advisor Session accounting = %+v", accounted)
	}
	primary, err = store.GetSessionThread(ctx, session.ID, primary.ID)
	if err != nil || primary.Usage.InputTokens != executorUsage.InputTokens ||
		primary.Usage.OutputTokens != executorUsage.OutputTokens ||
		primary.ModelListCostNanoUSD != wantExecutorCost {
		t.Fatalf("Advisor usage leaked into primary Thread = %+v, err=%v", primary, err)
	}

	primaryEvents, err := store.QueryEvents(
		ctx, session.ID, app.EventQuery{Limit: 100},
	)
	if err != nil {
		t.Fatal(err)
	}
	var plainPrimaryTypes []string
	var secondContent []any
	for _, event := range primaryEvents {
		threadID, _ := event.Payload["session_thread_id"].(string)
		fromThreadID, _ := event.Payload["from_session_thread_id"].(string)
		if threadID == "sthr_advisor_plain" || fromThreadID == "sthr_advisor_plain" {
			plainPrimaryTypes = append(plainPrimaryTypes, event.Type)
		}
		if fromThreadID == "sthr_advisor_second" {
			secondContent, _ = event.Payload["content"].([]any)
		}
	}
	wantPrimaryTypes := []string{
		domain.EvSessionThreadCreated,
		domain.EvSessionThreadStatusRunning,
		domain.EvAgentThreadMessageReceived,
		domain.EvSessionThreadStatusIdle,
		domain.EvSessionThreadStatusTerminated,
	}
	if !equalStrings(plainPrimaryTypes, wantPrimaryTypes) {
		t.Fatalf("plain primary lifecycle = %v, want %v", plainPrimaryTypes, wantPrimaryTypes)
	}
	if len(secondContent) != 1 ||
		secondContent[0].(map[string]any)["text"] != "challenge the locking assumption" {
		t.Fatalf("second public content = %#v", secondContent)
	}
	advisorEvents, err := store.ThreadEventsAfter(
		ctx, session.ID, "sthr_advisor_plain", 0, 20,
	)
	if err != nil {
		t.Fatal(err)
	}
	wantAdvisorTypes := []string{
		domain.EvSessionThreadStatusRunning,
		domain.EvAgentThreadMessageSent,
		domain.EvSessionThreadStatusIdle,
		domain.EvSessionThreadStatusTerminated,
	}
	if !equalStrings(eventTypes(advisorEvents), wantAdvisorTypes) {
		t.Fatalf("Advisor ledger = %v, want %v", eventTypes(advisorEvents), wantAdvisorTypes)
	}
	failedEvents, err := store.ThreadEventsAfter(
		ctx, session.ID, "sthr_advisor_failed", 0, 20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := eventTypes(failedEvents); len(got) != 3 {
		t.Fatalf("failed Advisor ledger = %v", got)
	}
	failedStopReason, _ := failedEvents[1].Payload["stop_reason"].(map[string]any)
	if failedEvents[1].Type != domain.EvSessionThreadStatusIdle ||
		failedStopReason["type"] != "end_turn" {
		t.Fatalf("failed Advisor idle event = %+v", failedEvents[1])
	}

	var usageRows, terminationOutbox int
	if err := store.pool.QueryRow(ctx, `
SELECT count(*) FROM model_request_usage WHERE session_id = $1`, session.ID).Scan(&usageRows); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, `
SELECT count(*) FROM thread_orchestration_outbox WHERE session_id = $1`, session.ID).Scan(&terminationOutbox); err != nil {
		t.Fatal(err)
	}
	if usageRows != 4 || terminationOutbox != 0 {
		t.Fatalf("Advisor persistence rows usage=%d outbox=%d", usageRows, terminationOutbox)
	}
}

func advisorConsultationFixture(
	threadID string,
	usageID string,
	suffix string,
	usage domain.TokenUsage,
	content []any,
	delivered bool,
) domain.AdvisorConsultation {
	lifecycle := make([]string, 9)
	for index := range lifecycle {
		lifecycle[index] = "sevt_advisor_" + suffix + "_" + string(rune('a'+index))
	}
	return domain.AdvisorConsultation{
		ThreadID: threadID, UsageRequestID: usageID, LifecycleIDs: lifecycle,
		Model: "claude-opus-5", UsageModel: "claude-opus-5",
		Usage: usage, UsageKnown: true, StopReason: "end_turn",
		PublicContent: content, AdviceDelivered: delivered,
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
