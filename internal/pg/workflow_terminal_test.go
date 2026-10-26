package pg

import (
	"encoding/json"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/domain"
)

func terminalFixture(t *testing.T) (*Store, domain.Session, WorkflowRun, WorkflowRunRequest, WorkflowPumpRequest) {
	t.Helper()
	s, session, run, req := workflowReceiptFixture(t)
	ctx := t.Context()
	require.NoError(t, s.BindWorkflowCoordinator(ctx, session.ID, run.ID, WorkflowCoordinatorClaim{ChainID: "exec", ExecutionID: "exec"}))
	c, err := s.ClaimWorkflowCoordinatorEvaluator(ctx, session.ID, run.ID, "pump/0", 0, "exec")
	require.NoError(t, err)
	_, err = s.CommitWorkflowPump(ctx, session.ID, run.ID, WorkflowPumpRequest{RequestID: "pump/0", Epoch: c.Epoch, CoordinatorExecutionID: "exec", Command: WorkflowPumpCommand{Kind: "return", Value: json.RawMessage(`"IGNORE_SYSTEM"`)}})
	require.NoError(t, err)
	c, err = s.ClaimWorkflowCoordinatorEvaluator(ctx, session.ID, run.ID, "pump/1", 1, "exec")
	require.NoError(t, err)
	return s, session, run, req, WorkflowPumpRequest{RequestID: "pump/1", Epoch: c.Epoch, ExpectedCursor: 1, CoordinatorExecutionID: "exec"}
}

func TestWorkflowTerminalAtomicReceiptRecoveryAndFences(t *testing.T) {
	s, session, run, _, in := terminalFixture(t)
	ctx := t.Context()
	for _, kind := range []string{"owner", "epoch", "cursor", "identity", "pause"} {
		bad := in
		switch kind {
		case "owner":
			bad.CoordinatorExecutionID = "stale"
		case "epoch":
			bad.Epoch--
		case "cursor":
			bad.ExpectedCursor++
		case "identity":
			bad.RequestID = "other"
		case "pause":
			require.NoError(t, s.SetWorkflowRunPause(ctx, session.ID, run.ID, "interrupt", true))
		}
		_, err := s.FinalizeWorkflowRunCandidate(ctx, session.ID, run.ID, bad)
		require.Error(t, err, kind)
		if kind == "pause" {
			require.NoError(t, s.SetWorkflowRunPause(ctx, session.ID, run.ID, "interrupt", false))
		}
	}
	_, err := s.pool.Exec(ctx, `ALTER TABLE orchestration_outbox ADD CONSTRAINT terminal_fault CHECK(false) NOT VALID`)
	require.NoError(t, err)
	_, err = s.FinalizeWorkflowRunCandidate(ctx, session.ID, run.ID, in)
	require.Error(t, err)
	var state string
	var receipts, inputs int
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT state,(SELECT count(*) FROM workflow_run_terminal_results),(SELECT count(*) FROM events WHERE type='system.message') FROM workflow_runs WHERE id=$1`, run.ID).Scan(&state, &receipts, &inputs))
	require.Equal(t, "open", state)
	require.Zero(t, receipts)
	require.Zero(t, inputs)
	_, err = s.pool.Exec(ctx, `ALTER TABLE orchestration_outbox DROP CONSTRAINT terminal_fault`)
	require.NoError(t, err)
	var wg sync.WaitGroup
	outputs := make(chan WorkflowRunTerminal, 2)
	failures := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			r, e := s.FinalizeWorkflowRunCandidate(ctx, session.ID, run.ID, in)
			outputs <- r
			failures <- e
		})
	}
	wg.Wait()
	for range 2 {
		require.NoError(t, <-failures)
		require.EqualValues(t, 1, (<-outputs).Cursor)
	}
	recovered, err := s.RecoverWorkflowRunTerminal(ctx, session.ID, run.ID, WorkflowCoordinatorClaim{ChainID: "exec", ExecutionID: "exec", Cursor: 1, PreviousEpoch: 1})
	require.NoError(t, err)
	require.EqualValues(t, 2, recovered.Epoch)
	_, err = s.RecoverWorkflowRunTerminal(ctx, session.ID, run.ID, WorkflowCoordinatorClaim{ChainID: "new-chain", ExecutionID: "exec", Cursor: 1, PreviousEpoch: 1})
	require.Error(t, err)
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workflow_run_terminal_results),(SELECT count(*) FROM events WHERE type='system.message'),state FROM workflow_runs WHERE id=$1`, run.ID).Scan(&receipts, &inputs, &state))
	require.Equal(t, "closed", state)
	require.Equal(t, 1, receipts)
	require.Equal(t, 1, inputs)
	var pendingRunWakes, primaryWakes int
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workflow_run_outbox WHERE run_id=$1),(SELECT count(*) FROM orchestration_outbox WHERE session_id=$2)`, run.ID, session.ID).Scan(&pendingRunWakes, &primaryWakes))
	require.Zero(t, pendingRunWakes)
	require.Equal(t, 1, primaryWakes)
}

func TestWorkflowTerminalConsumptionAndSecondPausedRunBarrier(t *testing.T) {
	s, session, run, req, in := terminalFixture(t)
	ctx := t.Context()
	_, err := s.FinalizeWorkflowRunCandidate(ctx, session.ID, run.ID, in)
	require.NoError(t, err)
	second, err := s.AdmitWorkflowRun(ctx, session.ID, "second", req)
	require.NoError(t, err)
	require.NoError(t, s.SetWorkflowRunPause(ctx, session.ID, second.ID, "interrupt", true))
	var trigger string
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT primary_event_id FROM workflow_run_terminal_results WHERE run_id=$1`, run.ID).Scan(&trigger))
	idle := domain.EventDraft{Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}, domain.InternalWorkflowSynthesisCandidate: domain.ThreadReturnCandidate{MessageEventID: "sevt_synthesis", StopReason: "end_turn", TextOnly: true}}}
	completion, err := s.CompleteTurn(ctx, session.ID, trigger, []domain.EventDraft{{ID: "sevt_synthesis", Type: domain.EvAgentMessage, Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": "synthesized"}}}}, idle}, domain.StatusIdle)
	require.NoError(t, err)
	require.Equal(t, domain.StatusRunning, completion.Session.Status)
	require.NotContains(t, eventTypes(completion.Events), domain.EvSessionStatusIdle)
	var consumed bool
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM workflow_run_terminal_results WHERE run_id=$1`, run.ID).Scan(&consumed))
	require.True(t, consumed)
	replay, err := s.CompleteTurn(ctx, session.ID, trigger, nil, domain.StatusIdle)
	require.NoError(t, err)
	require.False(t, replay.Applied)
}

func TestWorkflowOpenRunPreventsIdleWorkRetirement(t *testing.T) {
	f := newIdleStopFixture(t)
	ctx := t.Context()
	session := f.session
	session.MultiagentRoster = []domain.Agent{{ID: "reviewer", Name: "reviewer", Version: 1}}
	// The fixture's Session roster is an admission input, not a user database.
	body, err := json.Marshal(session)
	require.NoError(t, err)
	_, err = f.store.pool.Exec(ctx, `UPDATE sessions SET body=$2 WHERE id=$1`, session.ID, body)
	require.NoError(t, err)
	req := WorkflowRunRequest{Source: "unexecuted", RuntimeProfile: "test", Input: json.RawMessage(`{}`), Deadline: f.store.clock.Now().Add(3600000000000), MaxInvocations: 1, MaxResultBytes: 32}
	run, err := f.store.AdmitWorkflowRun(ctx, session.ID, "retirement-race", req)
	require.NoError(t, err)
	require.NoError(t, f.store.SetWorkflowRunPause(ctx, session.ID, run.ID, "interrupt", true))
	err = f.repo.StopWork(f.workCtx, f.work.EnvironmentID, f.work.ID, true, &f.idleID)
	require.Error(t, err)
}

func TestWorkflowTerminalPendingActionContinuationConsumesExactDelivery(t *testing.T) {
	s, session, run, _, in := terminalFixture(t)
	ctx := t.Context()
	_, err := s.FinalizeWorkflowRunCandidate(ctx, session.ID, run.ID, in)
	require.NoError(t, err)
	var trigger string
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT primary_event_id FROM workflow_run_terminal_results WHERE run_id=$1`, run.ID).Scan(&trigger))
	action := "sevt_workflow_custom"
	parked, err := s.CompleteWorkflowTurn(ctx, session.ID, trigger, []domain.EventDraft{{ID: action, Type: domain.EvAgentCustomToolUse, Payload: map[string]any{"name": "get_evidence", "input": map[string]any{}}}, {Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "requires_action", "event_ids": []string{action}}}}}, domain.StatusIdle, "", "", nil, []string{action}, nil)
	require.NoError(t, err)
	require.True(t, parked.Parked)
	var consumed bool
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM workflow_run_terminal_results WHERE run_id=$1`, run.ID).Scan(&consumed))
	require.False(t, consumed)
	admitted, err := s.AdmitEvents(ctx, session.ID, []domain.EventDraft{{Type: domain.EvUserCustomToolResult, Payload: map[string]any{"custom_tool_use_id": action, "content": []any{map[string]any{"type": "text", "text": "evidence"}}}}})
	require.NoError(t, err)
	resume := admitted.SubmittedEvents[0].ID
	owned, err := s.IsWorkflowResultTrigger(ctx, session.ID, resume)
	require.NoError(t, err)
	require.True(t, owned)
	done, err := s.CompleteWorkflowTurn(ctx, session.ID, resume, []domain.EventDraft{{ID: "sevt_synthesis", Type: domain.EvAgentMessage, Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": "final synthesis"}}}}, {Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}, domain.InternalWorkflowSynthesisCandidate: domain.ThreadReturnCandidate{MessageEventID: "sevt_synthesis", StopReason: "end_turn", TextOnly: true}}}}, domain.StatusIdle, "", "", nil, nil, []string{resume})
	require.NoError(t, err)
	require.Equal(t, domain.StatusIdle, done.Session.Status)
	var owner string
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT consumed_trigger_id FROM workflow_run_terminal_results WHERE run_id=$1`, run.ID).Scan(&owner))
	require.Equal(t, resume, owner)
}

func TestWorkflowTerminalInterruptDoesNotConsumeOrClaimSuccess(t *testing.T) {
	s, session, run, _, in := terminalFixture(t)
	ctx := t.Context()
	_, err := s.FinalizeWorkflowRunCandidate(ctx, session.ID, run.ID, in)
	require.NoError(t, err)
	var trigger string
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT primary_event_id FROM workflow_run_terminal_results WHERE run_id=$1`, run.ID).Scan(&trigger))
	_, err = s.AdmitEvents(ctx, session.ID, []domain.EventDraft{{Type: domain.EvUserInterrupt, Payload: map[string]any{}}})
	require.NoError(t, err)
	_, err = s.CompleteTurn(ctx, session.ID, trigger, []domain.EventDraft{{ID: "sevt_synthesis", Type: domain.EvAgentMessage, Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": "late answer"}}}}, {Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}, domain.InternalWorkflowSynthesisCandidate: domain.ThreadReturnCandidate{MessageEventID: "sevt_synthesis", StopReason: "end_turn", TextOnly: true}}}}, domain.StatusIdle)
	require.NoError(t, err)
	var consumed bool
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM workflow_run_terminal_results WHERE run_id=$1`, run.ID).Scan(&consumed))
	require.False(t, consumed)
}

func TestDeferredWorkflowOutcomeIdleInterruptClosesOriginalSpan(t *testing.T) {
	s, session, req, trigger, res := outcomeReservationFixture(t)
	ctx := t.Context()
	_, err := s.AdmitWorkflowRun(ctx, session.ID, "open-before-grade", req)
	require.NoError(t, err)
	require.ErrorIs(t, s.ReserveOutcomeGrading(ctx, res), domain.ErrWorkflowCompletionPending)
	continuation := domain.WorkflowOutcomeContinuation{TriggerID: trigger.ID, StartEventID: res.StartEventID, EndEventID: res.EndEventID, Iteration: 0, Outcome: domain.OutcomeSpec{OutcomeID: res.OutcomeID}}
	_, err = s.CompleteTurn(ctx, session.ID, trigger.ID, []domain.EventDraft{{Type: domain.EvAgentMessage, Payload: map[string]any{"content": "working draft"}}, {Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}, domain.InternalDeferredWorkflowOutcome: continuation}}}, domain.StatusIdle)
	require.NoError(t, err)
	admitted, err := s.AdmitEvents(ctx, session.ID, []domain.EventDraft{{Type: domain.EvUserInterrupt, Payload: map[string]any{}}})
	require.NoError(t, err)
	ack, err := s.CompleteTurn(ctx, session.ID, admitted.SubmittedEvents[0].ID, nil, domain.StatusIdle)
	require.NoError(t, err)
	require.Nil(t, ack.Session.ActiveOutcome())
	var state, result string
	var count int
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT state FROM workflow_outcome_waits WHERE session_id=$1`, session.ID).Scan(&state))
	require.Equal(t, "finished", state)
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT count(*),max(payload->>'result') FROM events WHERE session_id=$1 AND type='span.outcome_evaluation_end' AND payload->>'outcome_evaluation_start_id'=$2`, session.ID, res.StartEventID).Scan(&count, &result))
	require.Equal(t, 1, count)
	require.Equal(t, "interrupted", result)
	replay, err := s.CompleteTurn(ctx, session.ID, admitted.SubmittedEvents[0].ID, nil, domain.StatusIdle)
	require.NoError(t, err)
	require.False(t, replay.Applied)
}

func deferredOutcomeFixture(t *testing.T) (*Store, domain.Session, WorkflowRun, domain.Event, domain.OutcomeGradingReservation) {
	t.Helper()
	s, session, req, trigger, res := outcomeReservationFixture(t)
	ctx := t.Context()
	run, err := s.AdmitWorkflowRun(ctx, session.ID, "defer-run", req)
	require.NoError(t, err)
	require.ErrorIs(t, s.ReserveOutcomeGrading(ctx, res), domain.ErrWorkflowCompletionPending)
	attempt, err := s.BeginAttempt(ctx, session.ID, trigger.ID)
	require.NoError(t, err)
	continuation := domain.WorkflowOutcomeContinuation{EvaluationOpen: true, TriggerID: trigger.ID, StartEventID: res.StartEventID, EndEventID: res.EndEventID, Outcome: domain.OutcomeSpec{OutcomeID: res.OutcomeID}}
	drafts := []domain.EventDraft{{Type: domain.EvAgentMessage, Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": "original working context"}}}}, {Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}, domain.InternalDeferredWorkflowOutcome: continuation}}}
	first, err := s.CompleteWorkflowTurn(ctx, session.ID, trigger.ID, drafts, domain.StatusIdle, attempt.ID, domain.RunAttemptCompleted, nil, nil, nil)
	require.NoError(t, err)
	require.True(t, first.Applied)
	// The original Outcome is still active, but this exact working turn/attempt
	// already completed. Lost ACK recovery must not try to finish it again.
	replay, err := s.CompleteWorkflowTurn(ctx, session.ID, trigger.ID, drafts, domain.StatusIdle, attempt.ID, domain.RunAttemptCompleted, nil, nil, nil)
	require.NoError(t, err)
	require.False(t, replay.Applied)
	return s, session, run, trigger, res
}
func finishDeferredRunFixture(t *testing.T, s *Store, run WorkflowRun) string {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, s.BindWorkflowCoordinator(ctx, run.SessionID, run.ID, WorkflowCoordinatorClaim{ChainID: "deferred-exec", ExecutionID: "deferred-exec"}))
	c, err := s.ClaimWorkflowCoordinatorEvaluator(ctx, run.SessionID, run.ID, "pump/0", 0, "deferred-exec")
	require.NoError(t, err)
	_, err = s.CommitWorkflowPump(ctx, run.SessionID, run.ID, WorkflowPumpRequest{RequestID: "pump/0", Epoch: c.Epoch, CoordinatorExecutionID: "deferred-exec", Command: WorkflowPumpCommand{Kind: "return", Value: json.RawMessage(`"report"`)}})
	require.NoError(t, err)
	c, err = s.ClaimWorkflowCoordinatorEvaluator(ctx, run.SessionID, run.ID, "pump/1", 1, "deferred-exec")
	require.NoError(t, err)
	_, err = s.FinalizeWorkflowRunCandidate(ctx, run.SessionID, run.ID, WorkflowPumpRequest{RequestID: "pump/1", Epoch: c.Epoch, ExpectedCursor: 1, CoordinatorExecutionID: "deferred-exec"})
	require.NoError(t, err)
	var result string
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT primary_event_id FROM workflow_run_terminal_results WHERE run_id=$1`, run.ID).Scan(&result))
	return result
}
func primarySynthesisDrafts() []domain.EventDraft {
	return []domain.EventDraft{{ID: "sevt_exact_synthesis", Type: domain.EvAgentMessage, Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": "final report"}}}}, {Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}, domain.InternalWorkflowSynthesisCandidate: domain.ThreadReturnCandidate{MessageEventID: "sevt_exact_synthesis", StopReason: "end_turn", TextOnly: true}}}}
}

func TestDeferredOutcomeFlushedMaxIterationsReleasesWaitBeforeFinalAcknowledgment(t *testing.T) {
	s, session, run, _, res := deferredOutcomeFixture(t)
	ctx := t.Context()
	result := finishDeferredRunFixture(t, s, run)
	res.TriggerEventID = result
	require.NoError(t, s.ReserveOutcomeGrading(ctx, res))
	require.NoError(t, s.AppendWorkflowEvents(ctx, session.ID, result, []domain.EventDraft{gradingEnd(res, "max_iterations_reached")}))
	var state string
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT state FROM workflow_outcome_waits WHERE session_id=$1`, session.ID).Scan(&state))
	require.Equal(t, "finished", state)
	current, err := s.GetSession(ctx, session.ID)
	require.NoError(t, err)
	require.Nil(t, current.ActiveOutcome())
	done, err := s.CompleteTurn(ctx, session.ID, result, primarySynthesisDrafts(), domain.StatusIdle)
	require.NoError(t, err)
	require.Equal(t, domain.StatusIdle, done.Session.Status)
}

func TestDeferredOutcomeRevisionPendingActionResumesNewIteration(t *testing.T) {
	s, session, run, _, res := deferredOutcomeFixture(t)
	ctx := t.Context()
	result := finishDeferredRunFixture(t, s, run)
	res.TriggerEventID = result
	require.NoError(t, s.ReserveOutcomeGrading(ctx, res))
	require.NoError(t, s.AppendWorkflowEvents(ctx, session.ID, result, []domain.EventDraft{gradingEnd(res, "needs_revision")}))
	continuation, err := s.WorkflowOutcomeContinuation(ctx, session.ID, result)
	require.NoError(t, err)
	require.NotNil(t, continuation)
	require.Equal(t, 1, continuation.Iteration)
	require.False(t, continuation.EvaluationOpen)
	action := "sevt_revision_tool"
	_, err = s.CompleteWorkflowTurn(ctx, session.ID, result, []domain.EventDraft{{ID: action, Type: domain.EvAgentCustomToolUse, Payload: map[string]any{"name": "review_revision", "input": map[string]any{}}}, {Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "requires_action", "event_ids": []string{action}}}}}, domain.StatusIdle, "", "", nil, []string{action}, nil)
	require.NoError(t, err)
	admitted, err := s.AdmitEvents(ctx, session.ID, []domain.EventDraft{{Type: domain.EvUserCustomToolResult, Payload: map[string]any{"custom_tool_use_id": action, "content": []any{map[string]any{"type": "text", "text": "reviewed"}}}}})
	require.NoError(t, err)
	resume := admitted.SubmittedEvents[0].ID
	continuation, err = s.WorkflowOutcomeContinuation(ctx, session.ID, resume)
	require.NoError(t, err)
	require.Equal(t, 1, continuation.Iteration)
	require.False(t, continuation.EvaluationOpen)
	next := res
	next.TriggerEventID = resume
	next.StartEventID = "sevt_revision_start"
	next.EndEventID = "sevt_revision_end"
	next.Iteration = 1
	start := domain.EventDraft{ID: next.StartEventID, Type: domain.EvSpanOutcomeEvaluationStart, Payload: map[string]any{"outcome_id": next.OutcomeID, "iteration": 1, domain.InternalWorkflowOutcomeEndEvent: next.EndEventID}}
	require.NoError(t, s.AppendWorkflowEvents(ctx, session.ID, resume, []domain.EventDraft{start}))
	continuation, err = s.WorkflowOutcomeContinuation(ctx, session.ID, resume)
	require.NoError(t, err)
	require.True(t, continuation.EvaluationOpen)
	require.Equal(t, next.StartEventID, continuation.StartEventID)
	require.NoError(t, s.ReserveOutcomeGrading(ctx, next))
	require.NoError(t, s.AppendWorkflowEvents(ctx, session.ID, resume, []domain.EventDraft{gradingEnd(next, "satisfied")}))
	done, err := s.CompleteWorkflowTurn(ctx, session.ID, resume, primarySynthesisDrafts(), domain.StatusIdle, "", "", nil, nil, []string{resume})
	require.NoError(t, err)
	require.Equal(t, domain.StatusIdle, done.Session.Status)
	require.Nil(t, done.Session.ActiveOutcome())
}

func TestWorkflowTerminalEarlierTextCannotConsumeEmptyFinalResponse(t *testing.T) {
	s, session, run, _, in := terminalFixture(t)
	ctx := t.Context()
	_, err := s.FinalizeWorkflowRunCandidate(ctx, session.ID, run.ID, in)
	require.NoError(t, err)
	var trigger string
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT primary_event_id FROM workflow_run_terminal_results WHERE run_id=$1`, run.ID).Scan(&trigger))
	drafts := primarySynthesisDrafts()
	drafts[1].Payload[domain.InternalWorkflowSynthesisCandidate] = domain.ThreadReturnCandidate{MessageEventID: "sevt_empty_final", StopReason: "end_turn", TextOnly: true}
	done, err := s.CompleteTurn(ctx, session.ID, trigger, drafts, domain.StatusIdle)
	require.NoError(t, err)
	require.Equal(t, domain.StatusRunning, done.Session.Status)
	var consumed bool
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT consumed_at IS NOT NULL FROM workflow_run_terminal_results WHERE run_id=$1`, run.ID).Scan(&consumed))
	require.False(t, consumed)
}

func TestDeferredOutcomeCompletedRevisionInterruptKeepsGradeAndClosesWait(t *testing.T) {
	s, session, run, _, res := deferredOutcomeFixture(t)
	ctx := t.Context()
	result := finishDeferredRunFixture(t, s, run)
	res.TriggerEventID = result
	require.NoError(t, s.ReserveOutcomeGrading(ctx, res))
	_, err := s.AdmitEvents(ctx, session.ID, []domain.EventDraft{{Type: domain.EvUserInterrupt, Payload: map[string]any{}}})
	require.NoError(t, err)
	drafts := []domain.EventDraft{gradingEnd(res, "needs_revision"), {Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}, domain.InternalOutcomeEvaluationStart: res.StartEventID, domain.InternalOutcomeIteration: 0}}}
	done, err := s.CompleteTurn(ctx, session.ID, result, drafts, domain.StatusIdle)
	require.NoError(t, err)
	require.Nil(t, done.Session.ActiveOutcome())
	require.Equal(t, "interrupted", done.Session.Outcomes[len(done.Session.Outcomes)-1].Result)
	var state string
	var grades, interrupts int
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT state FROM workflow_outcome_waits WHERE session_id=$1`, session.ID).Scan(&state))
	require.Equal(t, "finished", state)
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE payload->>'result'='needs_revision'),count(*) FILTER(WHERE payload->>'result'='interrupted' AND payload->>'outcome_evaluation_start_id'='' AND (payload->>'iteration')::int=1) FROM events WHERE session_id=$1 AND type='span.outcome_evaluation_end'`, session.ID).Scan(&grades, &interrupts))
	require.Equal(t, 1, grades)
	require.Equal(t, 1, interrupts)
	replay, err := s.CompleteTurn(ctx, session.ID, result, drafts, domain.StatusIdle)
	require.NoError(t, err)
	require.False(t, replay.Applied)
}

func TestWorkflowTerminalJSONBWhitespaceDoesNotChangeAdmittedValueBudget(t *testing.T) {
	s, session, _, req := workflowReceiptFixture(t)
	ctx := t.Context()
	value := json.RawMessage(`{"a":1,"bb":2}`)
	req.MaxResultBytes = len(value)
	run, err := s.AdmitWorkflowRun(ctx, session.ID, "value-budget", req)
	require.NoError(t, err)
	require.NoError(t, s.BindWorkflowCoordinator(ctx, session.ID, run.ID, WorkflowCoordinatorClaim{ChainID: "budget", ExecutionID: "budget"}))
	c, err := s.ClaimWorkflowCoordinatorEvaluator(ctx, session.ID, run.ID, "pump/0", 0, "budget")
	require.NoError(t, err)
	_, err = s.CommitWorkflowPump(ctx, session.ID, run.ID, WorkflowPumpRequest{RequestID: "pump/0", Epoch: c.Epoch, CoordinatorExecutionID: "budget", Command: WorkflowPumpCommand{Kind: "return", Value: value}})
	require.NoError(t, err)
	var stored []byte
	require.NoError(t, s.pool.QueryRow(ctx, `SELECT decision->'value' FROM workflow_pump_decisions WHERE run_id=$1`, run.ID).Scan(&stored))
	require.Greater(t, len(stored), len(value), "JSONB adds representational whitespace")
	c, err = s.ClaimWorkflowCoordinatorEvaluator(ctx, session.ID, run.ID, "pump/1", 1, "budget")
	require.NoError(t, err)
	_, err = s.FinalizeWorkflowRunCandidate(ctx, session.ID, run.ID, WorkflowPumpRequest{RequestID: "pump/1", Epoch: c.Epoch, ExpectedCursor: 1, CoordinatorExecutionID: "budget"})
	require.NoError(t, err)
}
