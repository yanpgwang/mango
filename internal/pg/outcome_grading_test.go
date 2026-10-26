package pg

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/domain"
)

func outcomeReservationFixture(t *testing.T) (*Store, domain.Session, WorkflowRunRequest, domain.Event, domain.OutcomeGradingReservation) {
	t.Helper()
	store, session, run, request := workflowReceiptFixture(t)
	// The receipt fixture creates a Run. Close it as test setup so the working
	// Outcome stage can independently exercise either next-admission order.
	_, err := store.pool.Exec(t.Context(), `UPDATE workflow_runs SET state='closed' WHERE id=$1`, run.ID)
	require.NoError(t, err)
	admission, err := store.AdmitEvents(t.Context(), session.ID, []domain.EventDraft{{Type: domain.EvUserDefineOutcome, Payload: map[string]any{"description": "produce report", "rubric": map[string]any{"type": "text", "content": "contains evidence"}}}})
	require.NoError(t, err)
	trigger := admission.SubmittedEvents[0]
	outcomeID := trigger.Payload["outcome_id"].(string)
	reservation := domain.OutcomeGradingReservation{SessionID: session.ID, OutcomeID: outcomeID, StartEventID: "sevt_grading_start", EndEventID: "sevt_grading_end"}
	require.NoError(t, store.AppendWorkflowEvents(t.Context(), session.ID, trigger.ID, []domain.EventDraft{{ID: reservation.StartEventID, Type: domain.EvSpanOutcomeEvaluationStart, Payload: map[string]any{"outcome_id": outcomeID, "iteration": 0}}}))
	return store, session, request, trigger, reservation
}

func gradingEnd(in domain.OutcomeGradingReservation, result string) domain.EventDraft {
	return domain.EventDraft{ID: in.EndEventID, Type: domain.EvSpanOutcomeEvaluationEnd, Payload: map[string]any{"outcome_id": in.OutcomeID, "outcome_evaluation_start_id": in.StartEventID, "iteration": in.Iteration, "result": result, "explanation": "test verdict", "usage": map[string]any{}}}
}

func TestOutcomeGradingReservationSerializesBothRunAdmissionOrders(t *testing.T) {
	t.Run("Run wins before external inference", func(t *testing.T) {
		store, session, request, _, reservation := outcomeReservationFixture(t)
		_, err := store.AdmitWorkflowRun(t.Context(), session.ID, "new-run", request)
		require.NoError(t, err, "an active Outcome working stage is allowed to start a Run")
		err = store.ReserveOutcomeGrading(t.Context(), reservation)
		require.ErrorContains(t, err, "waits for open")
		var count int
		require.NoError(t, store.pool.QueryRow(t.Context(), `SELECT count(*) FROM outcome_grading_reservations`).Scan(&count))
		require.Zero(t, count)
	})
	t.Run("grader wins before Run admission", func(t *testing.T) {
		store, session, request, trigger, reservation := outcomeReservationFixture(t)
		require.NoError(t, store.ReserveOutcomeGrading(t.Context(), reservation))
		require.NoError(t, store.ReserveOutcomeGrading(t.Context(), reservation), "Activity retry keeps the same reservation")
		_, err := store.AdmitWorkflowRun(t.Context(), session.ID, "new-run", request)
		require.ErrorContains(t, err, "reserved Outcome")
		bad := reservation
		bad.EndEventID = "different-end"
		require.Error(t, store.ReserveOutcomeGrading(t.Context(), bad))
		end := gradingEnd(reservation, "needs_revision")
		require.NoError(t, store.AppendWorkflowEvents(t.Context(), session.ID, trigger.ID, []domain.EventDraft{end}))
		require.NoError(t, store.AppendWorkflowEvents(t.Context(), session.ID, trigger.ID, []domain.EventDraft{end}), "lost append ACK replays the entire committed batch")
		err = store.ReserveOutcomeGrading(t.Context(), reservation)
		require.ErrorContains(t, err, "ended", "a stale Activity cannot repeat inference after its end committed")
		run, err := store.AdmitWorkflowRun(t.Context(), session.ID, "new-run", request)
		require.NoError(t, err, "working/revision stage is not blocked by active Outcome projection")
		_, err = store.pool.Exec(t.Context(), `UPDATE workflow_runs SET state='closed' WHERE id=$1`, run.ID)
		require.NoError(t, err)
		// Prefix persistence does not advance Session.Outcomes. Iteration one is
		// admitted by its owned persisted span, not that lagging projection.
		next := reservation
		next.StartEventID = "sevt_iteration_one"
		next.EndEventID = "sevt_iteration_one_end"
		next.Iteration = 1
		require.NoError(t, store.AppendWorkflowEvents(t.Context(), session.ID, trigger.ID, []domain.EventDraft{{ID: next.StartEventID, Type: domain.EvSpanOutcomeEvaluationStart, Payload: map[string]any{"outcome_id": next.OutcomeID, "iteration": 1}}}))
		require.NoError(t, store.ReserveOutcomeGrading(t.Context(), next))
	})
}

func TestOutcomeGradingReservationRollbackAndOwnership(t *testing.T) {
	store, session, request, trigger, reservation := outcomeReservationFixture(t)
	require.NoError(t, store.ReserveOutcomeGrading(t.Context(), reservation))
	end := gradingEnd(reservation, "satisfied")
	wrong := end
	wrong.Payload = map[string]any{"outcome_id": "other", "outcome_evaluation_start_id": reservation.StartEventID, "iteration": 0, "result": "satisfied"}
	require.Error(t, store.AppendWorkflowEvents(t.Context(), session.ID, trigger.ID, []domain.EventDraft{wrong}))
	_, err := store.pool.Exec(t.Context(), "ALTER TABLE events ADD CONSTRAINT grading_fault CHECK (type<>'"+domain.EvSpanOutcomeEvaluationEnd+"') NOT VALID")
	require.NoError(t, err)
	require.Error(t, store.AppendWorkflowEvents(t.Context(), session.ID, trigger.ID, []domain.EventDraft{end}))
	_, err = store.AdmitWorkflowRun(t.Context(), session.ID, "blocked", request)
	require.ErrorContains(t, err, "reserved Outcome", "release rolled back with the failed end event")
	_, err = store.pool.Exec(t.Context(), `ALTER TABLE events DROP CONSTRAINT grading_fault`)
	require.NoError(t, err)
	require.NoError(t, store.AppendWorkflowEvents(t.Context(), session.ID, trigger.ID, []domain.EventDraft{end}))
	stale := end
	stale.Payload = map[string]any{"outcome_id": reservation.OutcomeID, "outcome_evaluation_start_id": reservation.StartEventID, "iteration": 0, "result": "failed"}
	require.Error(t, store.AppendWorkflowEvents(t.Context(), session.ID, trigger.ID, []domain.EventDraft{stale}))
}

func TestOutcomeGradingReservationInterruptFencesExactOwner(t *testing.T) {
	store, session, request, trigger, reservation := outcomeReservationFixture(t)
	require.NoError(t, store.ReserveOutcomeGrading(t.Context(), reservation))
	_, err := store.AdmitEvents(t.Context(), session.ID, []domain.EventDraft{{Type: domain.EvUserInterrupt, Payload: map[string]any{}}})
	require.NoError(t, err)
	require.Error(t, store.ReserveOutcomeGrading(t.Context(), reservation), "interrupted Activity retries cannot issue fresh inference")
	completion, err := store.CompleteWorkflowTurn(t.Context(), session.ID, trigger.ID, []domain.EventDraft{{Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}, domain.InternalOutcomeEvaluationStart: reservation.StartEventID, domain.InternalOutcomeIteration: 0}}}, domain.StatusIdle, "", "", nil, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "interrupted", completion.Session.Outcomes[0].Result)
	var state string
	require.NoError(t, store.pool.QueryRow(t.Context(), `SELECT state FROM outcome_grading_reservations WHERE start_event_id=$1`, reservation.StartEventID).Scan(&state))
	require.Equal(t, "fenced", state)
	_, err = store.AdmitWorkflowRun(t.Context(), session.ID, "after-interrupt", request)
	require.NoError(t, err)
	_, err = store.CompleteWorkflowTurn(t.Context(), session.ID, trigger.ID, []domain.EventDraft{gradingEnd(reservation, "satisfied")}, domain.StatusIdle, "", "", nil, nil, nil)
	require.NoError(t, err, "a processed interrupted turn replays instead of replacing the outcome")
	got, err := store.GetSession(t.Context(), session.ID)
	require.NoError(t, err)
	require.Equal(t, "interrupted", got.Outcomes[0].Result)
}

func TestOutcomeGradingCompletedRevisionInterruptCommitsOnce(t *testing.T) {
	store, session, request, trigger, reservation := outcomeReservationFixture(t)
	require.NoError(t, store.ReserveOutcomeGrading(t.Context(), reservation))
	_, err := store.AdmitEvents(t.Context(), session.ID, []domain.EventDraft{{Type: domain.EvUserInterrupt, Payload: map[string]any{}}})
	require.NoError(t, err)
	// The grading Activity completed while the interrupt watcher also won.
	// The Workflow preserves the completed grade and carries its active-start
	// metadata into completion; interruption now belongs to revision work.
	drafts := []domain.EventDraft{gradingEnd(reservation, "needs_revision"), {
		Type: domain.EvSessionStatusIdle,
		Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"},
			domain.InternalOutcomeEvaluationStart: reservation.StartEventID,
			domain.InternalOutcomeIteration:       0},
	}}
	usage := domain.TokenUsage{InputTokens: 13, OutputTokens: 5}
	completion, err := store.CompleteWorkflowTurnWithUsage(t.Context(), session.ID, trigger.ID, drafts, domain.StatusIdle, "", "", nil, nil, nil, usage)
	require.NoError(t, err)
	require.True(t, completion.Applied)
	require.Equal(t, "interrupted", completion.Session.Outcomes[0].Result)
	require.Equal(t, 1, completion.Session.Outcomes[0].Iteration)
	var ends []domain.Event
	for _, event := range completion.Events {
		if event.Type == domain.EvSpanOutcomeEvaluationEnd {
			ends = append(ends, event)
		}
	}
	require.Len(t, ends, 2)
	require.Equal(t, reservation.EndEventID, ends[0].ID)
	require.Equal(t, "needs_revision", ends[0].Payload["result"])
	require.Equal(t, reservation.StartEventID, ends[0].Payload["outcome_evaluation_start_id"])
	require.Equal(t, "interrupted", ends[1].Payload["result"])
	require.Empty(t, ends[1].Payload["outcome_evaluation_start_id"], "revision work has no open grading span")
	require.Equal(t, 1, ends[1].Payload["iteration"])
	var state, terminal string
	require.NoError(t, store.pool.QueryRow(t.Context(), `SELECT state,terminal_event_id FROM outcome_grading_reservations WHERE start_event_id=$1`, reservation.StartEventID).Scan(&state, &terminal))
	require.Equal(t, "finished", state)
	require.Equal(t, reservation.EndEventID, terminal)
	_, err = store.AdmitWorkflowRun(t.Context(), session.ID, "after-completed-grade-interrupt", request)
	require.NoError(t, err, "the completed grading reservation must be released")
	replayed, err := store.CompleteWorkflowTurnWithUsage(t.Context(), session.ID, trigger.ID, drafts, domain.StatusIdle, "", "", nil, nil, nil, usage)
	require.NoError(t, err)
	require.False(t, replayed.Applied, "lost completion ACK replays without replacing the result")
	got, err := store.GetSession(t.Context(), session.ID)
	require.NoError(t, err)
	require.Equal(t, usage.InputTokens, got.Usage.InputTokens)
	require.Equal(t, usage.OutputTokens, got.Usage.OutputTokens)
}
