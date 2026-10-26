package pg

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

func rejectActiveOutcomeGrading(ctx context.Context, tx pgx.Tx, sessionID string) error {
	var active bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outcome_grading_reservations WHERE session_id=$1 AND state='active')`, sessionID).Scan(&active); err != nil {
		return err
	}
	if active {
		return domain.Conflict("Workflow admission waits for the reserved Outcome grader")
	}
	return nil
}

// ReserveOutcomeGrading fences Run admission before actual provider I/O. It
// requires an already persisted owned evaluation start, never guesses an owner
// from an active Outcome alone, and deliberately has no timer/defer release.
func (s *Store) ReserveOutcomeGrading(ctx context.Context, in domain.OutcomeGradingReservation) error {
	if in.StartEventID == "" || in.EndEventID == "" || in.StartEventID == in.EndEventID || in.OutcomeID == "" || in.Iteration < 0 {
		return domain.Validation("invalid Outcome grading reservation")
	}
	return s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		session, err := s.lockWorkflowSession(ctx, q, in.SessionID)
		if err != nil {
			return err
		}
		if session.ArchivedAt != nil || session.Status == domain.StatusTerminated || session.BudgetReached(s.clock.Now()) {
			return domain.Conflict("Outcome grading owner is ineligible")
		}
		active := session.ActiveOutcome()
		if active == nil || active.OutcomeID != in.OutcomeID {
			return domain.Conflict("Outcome grading is no longer active")
		}
		start, err := q.GetEvent(ctx, pgstore.GetEventParams{SessionID: in.SessionID, ID: in.StartEventID})
		if err != nil {
			return err
		}
		var payload map[string]any
		if err = json.Unmarshal(start.Payload, &payload); err != nil {
			return err
		}
		primaryID, err := q.GetPrimarySessionThreadID(ctx, in.SessionID)
		if err != nil {
			return err
		}
		if start.Type != domain.EvSpanOutcomeEvaluationStart || start.ThreadID != primaryID || start.TurnEventID == nil || payload["outcome_id"] != in.OutcomeID || !gradingIterationMatches(payload["iteration"], in.Iteration) {
			return domain.Conflict("Outcome grading start belongs to another evaluation")
		}
		trigger, err := q.GetEvent(ctx, pgstore.GetEventParams{SessionID: in.SessionID, ID: *start.TurnEventID})
		if err != nil {
			return err
		}
		execution := trigger
		if in.TriggerEventID != "" && in.TriggerEventID != trigger.ID {
			execution, err = q.GetEvent(ctx, pgstore.GetEventParams{SessionID: in.SessionID, ID: in.TriggerEventID})
			if err != nil {
				return err
			}
		}
		if trigger.ThreadID != primaryID || execution.ThreadID != primaryID {
			return domain.Conflict("Outcome grading trigger is not primary")
		}
		origin := trigger
		var deliveryRoot string
		rootErr := tx.QueryRow(ctx, workflowDeliveryRootSQL, in.SessionID, execution.ID).Scan(&deliveryRoot)
		if rootErr != nil && !errors.Is(rootErr, pgx.ErrNoRows) {
			return rootErr
		}
		if trigger.Type != domain.EvUserDefineOutcome || execution.ID != trigger.ID {
			var originID string
			if rootErr != nil {
				return domain.Conflict("Outcome continuation lacks an owned Run result")
			}
			if err = tx.QueryRow(ctx, `SELECT trigger_id FROM workflow_outcome_waits WHERE session_id=$1 AND state='waiting'`, in.SessionID).Scan(&originID); err != nil {
				return domain.Conflict("Outcome continuation is not waiting")
			}
			origin, err = q.GetEvent(ctx, pgstore.GetEventParams{SessionID: in.SessionID, ID: originID})
			if err != nil {
				return err
			}
		}
		var triggerPayload map[string]any
		if err = json.Unmarshal(origin.Payload, &triggerPayload); err != nil {
			return err
		}
		if origin.Type != domain.EvUserDefineOutcome || triggerPayload["outcome_id"] != in.OutcomeID {
			return domain.Conflict("Outcome grading trigger has different ownership")
		}
		thread, err := loadSessionThreadForUpdate(ctx, tx, in.SessionID, primaryID)
		if err != nil {
			return err
		}
		if thread.ArchivedAt != nil || thread.Status != domain.StatusRunning {
			return domain.Conflict("Outcome grading Thread is ineligible")
		}
		_, err = q.FirstUnprocessedInterruptAfter(ctx, pgstore.FirstUnprocessedInterruptAfterParams{SessionID: in.SessionID, AfterSeq: execution.Seq})
		if err == nil {
			return domain.Conflict("Outcome grading was interrupted")
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		blocked, err := workflowCompletionBlocked(ctx, tx, in.SessionID, deliveryRoot)
		if err != nil {
			return err
		}
		if blocked {
			return domain.ErrWorkflowCompletionPending
		}
		var end, state string
		err = tx.QueryRow(ctx, `SELECT end_event_id,state FROM outcome_grading_reservations WHERE session_id=$1 AND start_event_id=$2`, in.SessionID, in.StartEventID).Scan(&end, &state)
		if err == nil {
			if end != in.EndEventID || state != "active" {
				return domain.Conflict("Outcome grading reservation changed or ended")
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var previousEnd bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM events WHERE session_id=$1 AND type=$2 AND payload->>'outcome_evaluation_start_id'=$3)`, in.SessionID, domain.EvSpanOutcomeEvaluationEnd, in.StartEventID).Scan(&previousEnd); err != nil {
			return err
		}
		if previousEnd {
			return domain.Conflict("Outcome grading span already ended")
		}
		if err = rejectActiveOutcomeGrading(ctx, tx, in.SessionID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO outcome_grading_reservations(session_id,thread_id,trigger_id,outcome_id,iteration,start_event_id,end_event_id) VALUES($1,$2,$3,$4,$5,$6,$7)`, in.SessionID, primaryID, execution.ID, in.OutcomeID, in.Iteration, in.StartEventID, in.EndEventID)
		return err
	})
}

// The caller holds Session. Release only alongside the exact committed end,
// including a correlated interrupted/fatal end synthesized by completion.
func (s *Store) finishOutcomeGradingLocked(ctx context.Context, tx pgx.Tx, q *pgstore.Queries, sessionID, triggerID string, drafts []domain.EventDraft) error {
	for i := range drafts {
		draft := &drafts[i]
		if draft.Type != domain.EvSpanOutcomeEvaluationEnd {
			continue
		}
		outcomeID, _ := draft.Payload["outcome_id"].(string)
		startID, _ := draft.Payload["outcome_evaluation_start_id"].(string)
		var owner, end, state, reservedOutcome string
		var terminal *string
		var iteration int
		err := tx.QueryRow(ctx, `SELECT trigger_id,end_event_id,state,terminal_event_id,iteration,outcome_id FROM outcome_grading_reservations WHERE session_id=$1 AND start_event_id=$2`, sessionID, startID).Scan(&owner, &end, &state, &terminal, &iteration, &reservedOutcome)
		if errors.Is(err, pgx.ErrNoRows) {
			var active bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outcome_grading_reservations WHERE session_id=$1 AND outcome_id=$2 AND state='active')`, sessionID, outcomeID).Scan(&active); err != nil {
				return err
			}
			if active {
				return domain.Conflict("Outcome end does not own the active grading reservation")
			}
			continue
		}
		if err != nil {
			return err
		}
		if owner != triggerID || reservedOutcome != outcomeID || !gradingIterationMatches(draft.Payload["iteration"], iteration) {
			return domain.Conflict("Outcome end belongs to another grading turn")
		}
		if state != "active" {
			if terminal == nil || *terminal != draft.ID {
				return domain.Conflict("stale Outcome grading end")
			}
			existing, err := q.GetEvent(ctx, pgstore.GetEventParams{SessionID: sessionID, ID: draft.ID})
			if err != nil {
				return err
			}
			if !workflowEventMatches(existing, triggerID, *draft) {
				return domain.Conflict("Outcome grading end changed after settlement")
			}
			continue
		}
		result, _ := draft.Payload["result"].(string)
		next := "finished"
		if result == "interrupted" || result == "failed" {
			next = "fenced"
			if draft.ID == "" {
				draft.ID = end
			}
		} else if draft.ID != end {
			return domain.Conflict("Outcome end has different reserved identity")
		}
		_, err = tx.Exec(ctx, `UPDATE outcome_grading_reservations SET state=$3,terminal_event_id=$4 WHERE session_id=$1 AND start_event_id=$2`, sessionID, startID, next, draft.ID)
		if err != nil {
			return err
		}
	}
	return nil
}

func gradingIterationMatches(raw any, expected int) bool {
	switch value := raw.(type) {
	case int:
		return value == expected
	case int32:
		return int64(value) == int64(expected)
	case int64:
		return value == int64(expected)
	case float64:
		return value == float64(expected)
	default:
		return false
	}
}
