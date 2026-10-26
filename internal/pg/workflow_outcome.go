package pg

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

func (s *Store) WorkflowOutcomeTurnDeferred(ctx context.Context, sid, trigger string) (bool, error) {
	var found bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_outcome_waits WHERE session_id=$1 AND trigger_id=$2)`, sid, trigger).Scan(&found)
	return found, err
}

// Progress spans can commit before the turn's final acknowledgment. Advance
// continuation and quality projection with that exact committed progress, so a
// pending-action resume never reloads an already-ended evaluation span.
func advanceDeferredOutcomeProgressLocked(ctx context.Context, tx pgx.Tx, session *domain.Session, drafts []domain.EventDraft, now time.Time) error {
	changed := false
	for _, d := range drafts {
		outcome, _ := d.Payload["outcome_id"].(string)
		if outcome == "" {
			continue
		}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_outcome_waits w JOIN events e ON e.id=w.trigger_id WHERE w.session_id=$1 AND e.payload->>'outcome_id'=$2)`, session.ID, outcome).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			continue
		}
		switch d.Type {
		case domain.EvSpanOutcomeEvaluationStart:
			end, _ := d.Payload[domain.InternalWorkflowOutcomeEndEvent].(string)
			if end == "" {
				continue
			}
			_, err := tx.Exec(ctx, `UPDATE workflow_outcome_waits SET start_event_id=$3,end_event_id=$4,evaluation_open=true WHERE session_id=$1 AND state='waiting' AND iteration=$2 AND evaluation_open=false`, session.ID, intFromAny(d.Payload["iteration"]), d.ID, end)
			if err != nil {
				return err
			}
		case domain.EvSpanOutcomeEvaluationEnd:
			result, _ := d.Payload["result"].(string)
			if result == "needs_revision" {
				_, err := tx.Exec(ctx, `UPDATE workflow_outcome_waits SET iteration=$2,evaluation_open=false WHERE session_id=$1 AND state='waiting'`, session.ID, intFromAny(d.Payload["iteration"])+1)
				if err != nil {
					return err
				}
			} else {
				if err := finishDeferredWorkflowOutcomeLocked(ctx, tx, session.ID, []domain.EventDraft{d}); err != nil {
					return err
				}
			}
			if result == "needs_revision" {
				explanation, _ := d.Payload["explanation"].(string)
				session.ApplyOutcomeResult(outcome, "running", explanation, intFromAny(d.Payload["iteration"])+1, now)
			} else {
				applyOutcomeResults(session, []domain.EventDraft{d}, now)
			}
			changed = true
		}
	}
	if changed {
		return putSessionOnlyTx(ctx, tx, *session)
	}
	return nil
}

// Follow only durable pending-action ownership, never message text or a caller
// supplied Run ID. This also supports successive custom-tool/approval barriers.
const workflowDeliveryRootSQL = `WITH RECURSIVE roots(id,depth) AS (
 SELECT $2::text,0 UNION ALL
 SELECT e.turn_event_id,r.depth+1 FROM roots r
 JOIN pending_actions p ON p.session_id=$1 AND p.resolving_event_id=r.id
 JOIN events e ON e.session_id=p.session_id AND e.id=p.action_event_id
 WHERE r.depth<32 AND e.turn_event_id IS NOT NULL
) SELECT t.primary_event_id FROM workflow_run_terminal_results t JOIN roots r ON r.id=t.primary_event_id WHERE t.session_id=$1 LIMIT 1`

func (s *Store) WorkflowOutcomeContinuation(ctx context.Context, sid, trigger string) (*domain.WorkflowOutcomeContinuation, error) {
	var root string
	err := s.pool.QueryRow(ctx, workflowDeliveryRootSQL, sid, trigger).Scan(&root)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var c domain.WorkflowOutcomeContinuation
	var payload []byte
	err = s.pool.QueryRow(ctx, `SELECT w.trigger_id,w.start_event_id,w.end_event_id,w.iteration,w.evaluation_open,e.payload FROM workflow_outcome_waits w JOIN events e ON e.id=w.trigger_id WHERE w.session_id=$1 AND w.state='waiting'`, sid).Scan(&c.TriggerID, &c.StartEventID, &c.EndEventID, &c.Iteration, &c.EvaluationOpen, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p map[string]any
	if err = json.Unmarshal(payload, &p); err != nil {
		return nil, err
	}
	rubric, ok := domain.OutcomeRubricContent(p)
	if !ok {
		return nil, domain.Conflict("deferred Outcome rubric is unavailable")
	}
	id, _ := p["outcome_id"].(string)
	description, _ := p["description"].(string)
	c.Outcome = domain.OutcomeSpec{OutcomeID: id, Description: description, Rubric: map[string]any{"type": "text", "content": rubric}, MaxIterations: intFromAny(p["max_iterations"])}
	if c.Outcome.MaxIterations < 1 {
		c.Outcome.MaxIterations = 3
	}
	session, err := s.GetSession(ctx, sid)
	if err != nil {
		return nil, err
	}
	active := session.ActiveOutcome()
	if active == nil || active.OutcomeID != id {
		return nil, nil
	}
	return &c, nil
}

// The evaluator never reserves provider I/O when this intent is produced. The
// same completion that saves working context yields the existing primary loop.
func (s *Store) deferWorkflowOutcomeLocked(ctx context.Context, tx pgx.Tx, q *pgstore.Queries, session domain.Session, current string, drafts []domain.EventDraft) error {
	for _, d := range drafts {
		raw, ok := d.Payload[domain.InternalDeferredWorkflowOutcome]
		if !ok {
			continue
		}
		b, err := json.Marshal(raw)
		if err != nil {
			return err
		}
		var c domain.WorkflowOutcomeContinuation
		if err = json.Unmarshal(b, &c); err != nil {
			return err
		}
		active := session.ActiveOutcome()
		if active == nil || active.OutcomeID != c.Outcome.OutcomeID || c.Iteration < 0 || !validWorkflowRequestID(c.EndEventID) {
			return domain.Conflict("deferred Outcome no longer owns its evaluation")
		}
		original, err := q.GetEvent(ctx, pgstore.GetEventParams{SessionID: session.ID, ID: c.TriggerID})
		if err != nil {
			return err
		}
		var p map[string]any
		if err = json.Unmarshal(original.Payload, &p); err != nil {
			return err
		}
		if original.Type != domain.EvUserDefineOutcome || p["outcome_id"] != active.OutcomeID {
			return domain.Conflict("deferred Outcome origin changed")
		}
		start, err := q.GetEvent(ctx, pgstore.GetEventParams{SessionID: session.ID, ID: c.StartEventID})
		if err != nil {
			return err
		}
		var span map[string]any
		if err = json.Unmarshal(start.Payload, &span); err != nil {
			return err
		}
		if start.Type != domain.EvSpanOutcomeEvaluationStart || span["outcome_id"] != active.OutcomeID || !gradingIterationMatches(span["iteration"], c.Iteration) {
			return domain.Conflict("deferred Outcome span changed")
		}
		ownedSpan := start.TurnEventID != nil && (*start.TurnEventID == current || *start.TurnEventID == c.TriggerID)
		if !ownedSpan {
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_outcome_waits WHERE session_id=$1 AND trigger_id=$2 AND start_event_id=$3 AND end_event_id=$4 AND iteration=$5 AND state='waiting' AND evaluation_open=true)`, session.ID, c.TriggerID, c.StartEventID, c.EndEventID, c.Iteration).Scan(&ownedSpan); err != nil {
				return err
			}
		}
		if !ownedSpan {
			return domain.Conflict("deferred span belongs to another turn")
		}
		if current != c.TriggerID {
			var root string
			if err = tx.QueryRow(ctx, workflowDeliveryRootSQL, session.ID, current).Scan(&root); err != nil {
				return domain.Conflict("deferred continuation lacks owned Run delivery")
			}
		}
		var reserved bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM outcome_grading_reservations WHERE session_id=$1 AND state='active')`, session.ID).Scan(&reserved); err != nil {
			return err
		}
		if reserved {
			return domain.Conflict("reserved grader cannot be deferred")
		}
		_, err = tx.Exec(ctx, `INSERT INTO workflow_outcome_waits(session_id,trigger_id,start_event_id,end_event_id,iteration) VALUES($1,$2,$3,$4,$5) ON CONFLICT(session_id,trigger_id) DO UPDATE SET start_event_id=$3,end_event_id=$4,iteration=$5,evaluation_open=true WHERE workflow_outcome_waits.state='waiting'`, session.ID, c.TriggerID, c.StartEventID, c.EndEventID, c.Iteration)
		if err != nil {
			return err
		}
	}
	return nil
}

func finishDeferredWorkflowOutcomeLocked(ctx context.Context, tx pgx.Tx, sid string, drafts []domain.EventDraft) error {
	for _, d := range drafts {
		if d.Type != domain.EvSpanOutcomeEvaluationEnd {
			continue
		}
		result, _ := d.Payload["result"].(string)
		if result == "needs_revision" {
			continue
		}
		outcomeID, _ := d.Payload["outcome_id"].(string)
		if _, err := tx.Exec(ctx, `UPDATE workflow_outcome_waits SET state='finished' WHERE session_id=$1 AND state='waiting' AND trigger_id IN (SELECT id FROM events WHERE session_id=$1 AND type='user.define_outcome' AND payload->>'outcome_id'=$2)`, sid, outcomeID); err != nil {
			return err
		}
	}
	return nil
}

func interruptDeferredWorkflowOutcomeLocked(ctx context.Context, tx pgx.Tx, sid, current string, drafts []domain.EventDraft) ([]domain.EventDraft, error) {
	var root string
	err := tx.QueryRow(ctx, workflowDeliveryRootSQL, sid, current).Scan(&root)
	if errors.Is(err, pgx.ErrNoRows) {
		var idleInterrupt bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM events e JOIN session_threads p ON p.id=e.thread_id WHERE e.session_id=$1 AND e.id=$2 AND e.type='user.interrupt' AND p.kind='primary')`, sid, current).Scan(&idleInterrupt); err != nil {
			return nil, err
		}
		if !idleInterrupt {
			return drafts, nil
		}
	} else if err != nil {
		return nil, err
	}
	var start, end, outcome string
	var evaluationOpen bool
	var iteration int
	err = tx.QueryRow(ctx, `SELECT w.start_event_id,w.end_event_id,w.iteration,w.evaluation_open,e.payload->>'outcome_id' FROM workflow_outcome_waits w JOIN events e ON e.id=w.trigger_id WHERE w.session_id=$1 AND w.state='waiting'`, sid).Scan(&start, &end, &iteration, &evaluationOpen, &outcome)
	if errors.Is(err, pgx.ErrNoRows) {
		return drafts, nil
	}
	if err != nil {
		return nil, err
	}
	for _, d := range drafts {
		if d.Type != domain.EvSpanOutcomeEvaluationEnd || d.Payload["outcome_id"] != outcome {
			continue
		}
		if d.Payload["result"] != "needs_revision" {
			return drafts, nil
		}
		iteration = max(iteration, intFromAny(d.Payload["iteration"])+1)
		if d.Payload["outcome_evaluation_start_id"] == start {
			evaluationOpen = false
		}
	}
	if !evaluationOpen {
		start = ""
		end = ""
	}
	return append(drafts, domain.EventDraft{ID: end, Type: domain.EvSpanOutcomeEvaluationEnd, Payload: map[string]any{"outcome_evaluation_start_id": start, "outcome_id": outcome, "iteration": iteration, "result": "interrupted", "explanation": "Outcome synthesis was interrupted before completion.", "usage": map[string]any{"input_tokens": 0, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "speed": nil}}}), nil
}
