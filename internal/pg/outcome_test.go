package pg

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestActiveReceiptProcessedOutcome(t *testing.T) {
	session := newSession("sess_outcome_receipt")
	if err := session.StartOutcome(domain.OutcomeSpec{OutcomeID: "outc_active"}); err != nil {
		t.Fatalf("start outcome: %v", err)
	}
	session.MarkActiveOutcomeRunning()
	payload := map[string]any{"outcome_id": "outc_active"}
	if !activeReceiptProcessedOutcome(session, domain.EvUserDefineOutcome, payload) {
		t.Fatal("active define_outcome was mistaken for a completed turn")
	}
	if activeReceiptProcessedOutcome(session, domain.EvUserMessage, payload) {
		t.Fatal("ordinary event was mistaken for a receipt-processed outcome")
	}
	if activeReceiptProcessedOutcome(session, domain.EvUserDefineOutcome, map[string]any{"outcome_id": "outc_other"}) {
		t.Fatal("mismatched outcome was treated as active")
	}
	session.ApplyOutcomeResult("outc_active", "satisfied", "done", 0, time.Now())
	if activeReceiptProcessedOutcome(session, domain.EvUserDefineOutcome, payload) {
		t.Fatal("terminal outcome was treated as still runnable")
	}
}

func TestDefineOutcomeProjectsLifecycleAndTerminalEvaluation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sess_outcome")
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	admission, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserDefineOutcome,
		Payload: map[string]any{
			"description": "produce report",
			"rubric":      map[string]any{"type": "text", "content": "includes evidence"},
		},
	}})
	if err != nil {
		t.Fatalf("admit outcome: %v", err)
	}
	if len(admission.Events) != 2 || admission.Events[0].Type != domain.EvUserDefineOutcome {
		t.Fatalf("admission events = %+v", admission.Events)
	}
	outcomeID, _ := admission.Events[0].Payload["outcome_id"].(string)
	if outcomeID == "" {
		t.Fatal("server did not assign outcome_id")
	}
	if admission.Events[0].Payload["max_iterations"] != 3 {
		t.Fatalf("default max_iterations = %v, want 3", admission.Events[0].Payload["max_iterations"])
	}
	if len(admission.Session.Outcomes) != 1 || admission.Session.Outcomes[0].Result != "running" {
		t.Fatalf("outcome projection after admission = %+v", admission.Session.Outcomes)
	}

	completion, err := store.CompleteWorkflowTurn(
		ctx,
		session.ID,
		admission.Events[0].ID,
		[]domain.EventDraft{
			{
				ID: "sevt_eval_start", Type: domain.EvSpanOutcomeEvaluationStart,
				Payload: map[string]any{"outcome_id": outcomeID, "iteration": 0},
			},
			{
				ID: "sevt_eval_end", Type: domain.EvSpanOutcomeEvaluationEnd,
				Payload: map[string]any{
					"outcome_evaluation_start_id": "sevt_eval_start",
					"outcome_id":                  outcomeID, "iteration": 0,
					"result": "satisfied", "explanation": "rubric met",
					"usage": map[string]any{},
				},
			},
			{
				Type:    domain.EvSessionStatusIdle,
				Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}},
			},
		},
		domain.StatusIdle,
		"",
		"",
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("complete outcome: %v", err)
	}
	if len(completion.Session.Outcomes) != 1 {
		t.Fatalf("outcomes = %+v", completion.Session.Outcomes)
	}
	got := completion.Session.Outcomes[0]
	if got.Result != "satisfied" || got.Explanation != "rubric met" || got.CompletedAt == nil {
		t.Fatalf("terminal outcome = %+v", got)
	}

	replayed, err := store.CompleteWorkflowTurn(
		ctx,
		session.ID,
		admission.Events[0].ID,
		nil,
		domain.StatusIdle,
		"",
		"",
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("replay outcome completion: %v", err)
	}
	if replayed.Applied || len(replayed.Session.Outcomes) != 1 {
		t.Fatalf("replayed outcome completion = %+v", replayed)
	}
}

func TestInterruptedOutcomePreservesDistinctOpenGradingStart(t *testing.T) {
	drafts, iteration, startID := interruptedTurnDrafts([]domain.EventDraft{
		{ID: "sevt_completed_grade", Type: domain.EvSpanOutcomeEvaluationEnd, Payload: map[string]any{
			"outcome_evaluation_start_id": "sevt_start_0", "iteration": 0, "result": "needs_revision",
		}},
		{Type: domain.EvSessionStatusIdle, Payload: map[string]any{
			domain.InternalOutcomeEvaluationStart: "sevt_start_1",
			domain.InternalOutcomeIteration:       1,
		}},
	})
	if iteration != 1 || startID != "sevt_start_1" {
		t.Fatalf("interruption lost its distinct open grading span: iteration=%d start=%q", iteration, startID)
	}
	if len(drafts) != 1 || drafts[0].ID != "sevt_completed_grade" {
		t.Fatalf("completed grading prefix changed: %+v", drafts)
	}
}

func TestOutcomeInterruptUsesEmptyStartIDAndPreservesCompletedRevision(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sess_outcome_interrupt")
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	admission, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserDefineOutcome,
		Payload: map[string]any{
			"description": "produce report",
			"rubric":      map[string]any{"type": "text", "content": "includes evidence"},
		},
	}})
	if err != nil {
		t.Fatalf("admit outcome: %v", err)
	}
	trigger := admission.Events[0]
	outcomeID, _ := trigger.Payload["outcome_id"].(string)
	if _, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserInterrupt, Payload: map[string]any{},
	}}); err != nil {
		t.Fatalf("admit interrupt: %v", err)
	}

	completion, err := store.CompleteWorkflowTurn(
		ctx,
		session.ID,
		trigger.ID,
		[]domain.EventDraft{
			{ID: "sevt_eval_start_0", Type: domain.EvSpanOutcomeEvaluationStart, Payload: map[string]any{
				"outcome_id": outcomeID, "iteration": 0,
			}},
			{ID: "sevt_eval_end_0", Type: domain.EvSpanOutcomeEvaluationEnd, Payload: map[string]any{
				"outcome_evaluation_start_id": "sevt_eval_start_0",
				"outcome_id":                  outcomeID, "iteration": 0,
				"result": "needs_revision", "explanation": "add evidence",
				"usage": map[string]any{},
			}},
		},
		domain.StatusIdle,
		"",
		"",
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("complete interrupted outcome: %v", err)
	}
	if got := []string{
		completion.Events[0].Type,
		completion.Events[1].Type,
		completion.Events[2].Type,
		completion.Events[3].Type,
	}; fmt.Sprint(got) != fmt.Sprint([]string{
		domain.EvSpanOutcomeEvaluationStart,
		domain.EvSpanOutcomeEvaluationEnd,
		domain.EvSpanOutcomeEvaluationEnd,
		domain.EvSessionStatusIdle,
	}) {
		t.Fatalf("completion event types = %v", got)
	}
	interrupted := completion.Events[2]
	if interrupted.Payload["outcome_evaluation_start_id"] != "" ||
		interrupted.Payload["iteration"] != 1 ||
		interrupted.Payload["result"] != "interrupted" {
		t.Fatalf("interrupted outcome end = %+v", interrupted.Payload)
	}
	if got := completion.Session.Outcomes[0]; got.Result != "interrupted" || got.Iteration != 1 {
		t.Fatalf("outcome projection = %+v", got)
	}
}

func TestOutcomeInterruptCorrelatesPrepublishedEvaluationStart(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sess_outcome_interrupt_started")
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	admission, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserDefineOutcome,
		Payload: map[string]any{
			"description": "produce report",
			"rubric":      map[string]any{"type": "text", "content": "includes evidence"},
		},
	}})
	if err != nil {
		t.Fatalf("admit outcome: %v", err)
	}
	trigger := admission.Events[0]
	outcomeID, _ := trigger.Payload["outcome_id"].(string)
	const startID = "sevt_eval_started_before_interrupt"
	if err := store.AppendWorkflowEvents(ctx, session.ID, trigger.ID, []domain.EventDraft{{
		ID: startID, Type: domain.EvSpanOutcomeEvaluationStart,
		Payload: map[string]any{"outcome_id": outcomeID, "iteration": 0},
	}}); err != nil {
		t.Fatalf("append evaluation start: %v", err)
	}
	if _, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserInterrupt, Payload: map[string]any{},
	}}); err != nil {
		t.Fatalf("admit interrupt: %v", err)
	}

	completion, err := store.CompleteWorkflowTurn(
		ctx,
		session.ID,
		trigger.ID,
		[]domain.EventDraft{{
			Type: domain.EvSessionStatusIdle,
			Payload: map[string]any{
				"stop_reason":                         map[string]any{"type": "end_turn"},
				domain.InternalOutcomeEvaluationStart: startID,
				domain.InternalOutcomeIteration:       0,
			},
		}},
		domain.StatusIdle,
		"",
		"",
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("complete interrupted outcome: %v", err)
	}
	if len(completion.Events) != 2 {
		t.Fatalf("completion events = %+v", completion.Events)
	}
	interrupted := completion.Events[0]
	if interrupted.Type != domain.EvSpanOutcomeEvaluationEnd ||
		interrupted.Payload["outcome_evaluation_start_id"] != startID ||
		interrupted.Payload["iteration"] != 0 ||
		interrupted.Payload["result"] != "interrupted" {
		t.Fatalf("interrupted outcome end = %+v", interrupted)
	}
	if got := completion.Session.Outcomes[0]; got.Result != "interrupted" || got.Iteration != 0 {
		t.Fatalf("outcome projection = %+v", got)
	}
}

func TestInterruptOrdinaryTurnDoesNotTerminateQueuedOutcome(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	const sessionID = "sess_interrupt_before_outcome_turn"
	messageTriggerID := journalTurn(t, store, sessionID)

	outcomeAdmission, err := store.AdmitEvents(ctx, sessionID, []domain.EventDraft{{
		Type: domain.EvUserDefineOutcome,
		Payload: map[string]any{
			"description": "produce report",
			"rubric":      map[string]any{"type": "text", "content": "includes evidence"},
		},
	}})
	if err != nil {
		t.Fatalf("admit queued outcome: %v", err)
	}
	outcomeTrigger := outcomeAdmission.Events[0]
	outcomeID, _ := outcomeTrigger.Payload["outcome_id"].(string)
	if outcomeTrigger.ProcessedAt == nil {
		t.Fatal("define_outcome was not stamped processed on receipt")
	}
	if _, err := store.AdmitEvents(ctx, sessionID, []domain.EventDraft{{
		Type: domain.EvUserInterrupt, Payload: map[string]any{},
	}}); err != nil {
		t.Fatalf("admit interrupt: %v", err)
	}

	interrupted, err := store.CompleteWorkflowTurn(
		ctx,
		sessionID,
		messageTriggerID,
		nil,
		domain.StatusIdle,
		"",
		"",
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("complete interrupted message: %v", err)
	}
	for _, event := range interrupted.Events {
		if event.Type == domain.EvSpanOutcomeEvaluationEnd {
			t.Fatalf("ordinary interrupt terminated queued outcome: %+v", event)
		}
	}
	active := interrupted.Session.ActiveOutcome()
	if active == nil || active.OutcomeID != outcomeID || active.CompletedAt != nil {
		t.Fatalf("queued outcome after unrelated interrupt = %+v", interrupted.Session.Outcomes)
	}

	completed, err := store.CompleteWorkflowTurn(
		ctx,
		sessionID,
		outcomeTrigger.ID,
		[]domain.EventDraft{
			{
				ID: "sevt_queued_eval_start", Type: domain.EvSpanOutcomeEvaluationStart,
				Payload: map[string]any{"outcome_id": outcomeID, "iteration": 0},
			},
			{
				ID: "sevt_queued_eval_end", Type: domain.EvSpanOutcomeEvaluationEnd,
				Payload: map[string]any{
					"outcome_evaluation_start_id": "sevt_queued_eval_start",
					"outcome_id":                  outcomeID, "iteration": 0,
					"result": "satisfied", "explanation": "rubric met",
					"usage": map[string]any{},
				},
			},
			{
				Type:    domain.EvSessionStatusIdle,
				Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}},
			},
		},
		domain.StatusIdle,
		"",
		"",
		nil,
		nil,
		nil,
	)
	if err != nil {
		t.Fatalf("complete queued outcome: %v", err)
	}
	if !completed.Applied {
		t.Fatal("queued outcome was mistaken for an already-completed turn")
	}
	if got := completed.Session.Outcomes[0]; got.Result != "satisfied" || got.CompletedAt == nil {
		t.Fatalf("completed queued outcome = %+v", got)
	}
}
