package pg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

type WorkflowRunTerminal struct{ Cursor, Epoch, Version int64 }

// RecoverWorkflowRunTerminal recovers only the exact physical owner's final
// pump after commit/ACK loss. Closed runs never bind a new owner or restart.
func (s *Store) RecoverWorkflowRunTerminal(ctx context.Context, sid, rid string, in WorkflowCoordinatorClaim) (*WorkflowRunTerminal, error) {
	var result *WorkflowRunTerminal
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		if _, err := s.lockWorkflowSession(ctx, q, sid); err != nil {
			return err
		}
		f, err := lockWorkflowFrontier(ctx, tx, sid, rid)
		if err != nil {
			return err
		}
		if f.state != "closed" {
			return nil
		}
		var chain, owner, request string
		var cursor, epoch, version int64
		err = tx.QueryRow(ctx, `SELECT r.coordinator_chain_id,t.coordinator_execution_id,t.request_id,t.candidate_cursor,t.evaluator_epoch,r.version FROM workflow_runs r JOIN workflow_run_terminal_results t ON t.run_id=r.id WHERE r.id=$1`, rid).Scan(&chain, &owner, &request, &cursor, &epoch, &version)
		if err != nil {
			return domain.Conflict("Workflow terminal receipt is unavailable")
		}
		if chain != in.ChainID || owner != in.ExecutionID || cursor != in.Cursor || epoch != in.PreviousEpoch+1 || request != workflowTerminalPumpID(in.Cursor) {
			return domain.Conflict("stale Workflow terminal recovery")
		}
		result = &WorkflowRunTerminal{Cursor: cursor, Epoch: epoch, Version: version}
		return nil
	})
	return result, err
}
func workflowTerminalPumpID(cursor int64) string { return "pump/" + strconv.FormatInt(cursor, 10) }

// FinalizeWorkflowRunCandidate commits the bounded program value, closed state,
// typed delivery binding and primary wake together. It never invents a user or
// tool call and never forwards ordinary child reports to the primary.
func (s *Store) FinalizeWorkflowRunCandidate(ctx context.Context, sid, rid string, in WorkflowPumpRequest) (WorkflowRunTerminal, error) {
	var out WorkflowRunTerminal
	if in.CoordinatorExecutionID == "" {
		return out, domain.Validation("Workflow finalizer needs a coordinator owner")
	}
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		session, err := s.lockWorkflowSession(ctx, q, sid)
		if err != nil {
			return err
		}
		f, err := lockWorkflowFrontier(ctx, tx, sid, rid)
		if err != nil {
			return err
		}
		if err = checkWorkflowCoordinator(f, in.CoordinatorExecutionID); err != nil {
			return err
		}
		if f.epoch != in.Epoch {
			return domain.Conflict("stale Workflow evaluator epoch")
		}
		if f.state == "closed" {
			err = tx.QueryRow(ctx, `SELECT t.candidate_cursor,t.evaluator_epoch,r.version FROM workflow_run_terminal_results t JOIN workflow_runs r ON r.id=t.run_id WHERE t.run_id=$1 AND t.session_id=$2 AND t.request_id=$3 AND t.candidate_cursor=$4 AND t.evaluator_epoch=$5 AND t.coordinator_execution_id=$6`, rid, sid, in.RequestID, in.ExpectedCursor, in.Epoch, in.CoordinatorExecutionID).Scan(&out.Cursor, &out.Epoch, &out.Version)
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.Conflict("stale Workflow terminal finalizer")
			}
			return err
		}
		if err = s.checkWorkflowFrontierAuthority(session, f); err != nil {
			return err
		}
		if f.cursor != in.ExpectedCursor || in.RequestID != workflowTerminalPumpID(f.cursor) || f.cursor < 1 {
			return domain.Conflict("Workflow terminal cursor changed")
		}
		var value []byte
		err = tx.QueryRow(ctx, `SELECT decision->'value' FROM workflow_pump_decisions WHERE run_id=$1 AND expected_cursor=$2 AND decision->>'type'='return'`, rid, f.cursor-1).Scan(&value)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Conflict("Workflow lacks a committed return candidate")
		}
		if err != nil {
			return err
		}
		var compact bytes.Buffer
		if err = json.Compact(&compact, value); err != nil {
			return err
		}
		value = compact.Bytes()
		if !validWorkflowProgramValue(value) || len(value) > f.maxResultBytes {
			return domain.Conflict("Workflow terminal value is invalid")
		}
		var pending bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_invocation_attempts WHERE run_id=$1 AND (state<>'settled' OR receipt IS NULL OR terminal_event_id IS NULL)) OR
 (SELECT count(*) FROM workflow_pump_decisions WHERE run_id=$1 AND command->>'kind'='invoke')<>(SELECT count(*) FROM workflow_pump_decisions WHERE run_id=$1 AND decision->>'type'='delivery')`, rid).Scan(&pending)
		if err != nil {
			return err
		}
		if pending {
			return domain.Conflict("Workflow terminal still owns pending work")
		}
		primary, err := q.GetPrimarySessionThreadID(ctx, sid)
		if err != nil {
			return err
		}
		now := s.clock.Now().UTC()
		maxSeq, err := q.MaxEventSeq(ctx, sid)
		if err != nil {
			return err
		}
		text, err := json.Marshal(map[string]any{"type": "workflow_run_result", "run_id": rid, "status": "completed", "value": json.RawMessage(value)})
		if err != nil {
			return err
		}
		events, next, err := s.appendThreadDraftsAt(ctx, q, sid, primary, []domain.EventDraft{{Type: domain.EvSystemMessage, Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}}}}, maxSeq, nil, now)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO workflow_run_terminal_results(run_id,session_id,candidate_cursor,evaluator_epoch,coordinator_execution_id,request_id,value,primary_event_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, rid, sid, f.cursor, f.epoch, in.CoordinatorExecutionID, in.RequestID, value, events[0].ID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE workflow_runs SET state='closed',version=version+1 WHERE id=$1 RETURNING version`, rid)
		if err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, `SELECT version FROM workflow_runs WHERE id=$1`, rid).Scan(&out.Version); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `DELETE FROM workflow_run_outbox WHERE run_id=$1`, rid); err != nil {
			return err
		}
		primaryThread, err := loadSessionThreadForUpdate(ctx, tx, sid, primary)
		if err != nil {
			return err
		}
		if !coordinatorCanAcceptReport(primaryThread) {
			return domain.Conflict("primary cannot accept a terminal Run result")
		}
		if primaryThread.Status == domain.StatusIdle {
			primaryThread.TransitionStatus(domain.StatusRunning, now)
			if err = putSessionThreadTx(ctx, tx, primaryThread); err != nil {
				return err
			}
		}
		session.TransitionStatus(domain.StatusRunning, now)
		if err = putSessionOnlyTx(ctx, tx, session); err != nil {
			return err
		}
		if err = q.UpsertOutbox(ctx, pgstore.UpsertOutboxParams{SessionID: sid, MaxEventSeq: next, EnqueuedAt: tsUTC(now)}); err != nil {
			return err
		}
		if session.EnvironmentType == "self_hosted" {
			if err = q.EnqueueEnvironmentWork(ctx, pgstore.EnqueueEnvironmentWorkParams{ID: s.ids.NewID(domain.PrefixEnvironmentWork), EnvironmentID: session.EnvironmentID, SessionID: &session.ID, ActivationSeq: &next, CreatedAt: tsUTC(now)}); err != nil {
				return err
			}
		}
		out.Cursor, out.Epoch = f.cursor, f.epoch
		return nil
	})
	if err == nil {
		s.notifySession(ctx, sid)
	}
	return out, err
}

// Caller owns Session. Paused runs count as open, and a queued terminal result
// is not a primary synthesis. exclude is allowed only for this owned trigger.
func workflowCompletionBlocked(ctx context.Context, tx pgx.Tx, sid, exclude string) (bool, error) {
	var blocked bool
	err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_runs WHERE session_id=$1 AND state='open') OR EXISTS(SELECT 1 FROM workflow_run_terminal_results WHERE session_id=$1 AND consumed_at IS NULL AND primary_event_id<>$2) OR ($2='' AND EXISTS(SELECT 1 FROM workflow_outcome_waits WHERE session_id=$1 AND state='waiting'))`, sid, exclude).Scan(&blocked)
	return blocked, err
}
func (s *Store) IsWorkflowResultTrigger(ctx context.Context, sid, eid string) (bool, error) {
	var root string
	err := s.pool.QueryRow(ctx, workflowDeliveryRootSQL, sid, eid).Scan(&root)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) consumeWorkflowResultLocked(ctx context.Context, tx pgx.Tx, sid, trigger string, drafts []domain.EventDraft) error {
	// An interrupt, refusal, pending-action park or empty answer is not completed
	// fresh synthesis. The binding remains pending rather than fabricating success.
	var candidate domain.ThreadReturnCandidate
	end := false
	for _, d := range drafts {
		if d.Type == domain.EvSessionStatusIdle {
			reason, _ := d.Payload["stop_reason"].(map[string]any)
			end = reason["type"] == "end_turn"
			if raw, ok := d.Payload[domain.InternalWorkflowSynthesisCandidate]; ok {
				b, err := json.Marshal(raw)
				if err != nil {
					return err
				}
				if err = json.Unmarshal(b, &candidate); err != nil {
					return err
				}
			}
		}
	}
	if !end || candidate.StopReason != "end_turn" || !candidate.TextOnly || candidate.MessageEventID == "" {
		return nil
	}
	var payload map[string]any
	for _, d := range drafts {
		if d.Type == domain.EvAgentMessage && d.ID == candidate.MessageEventID {
			payload = d.Payload
		}
	}
	if payload == nil {
		var encoded []byte
		err := tx.QueryRow(ctx, `SELECT e.payload FROM events e JOIN session_threads p ON p.id=e.thread_id WHERE e.session_id=$1 AND e.id=$2 AND e.turn_event_id=$3 AND e.type='agent.message' AND p.kind='primary'`, sid, candidate.MessageEventID, trigger).Scan(&encoded)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = json.Unmarshal(encoded, &payload); err != nil {
			return err
		}
	}
	b, err := json.Marshal(payload["content"])
	if err != nil {
		return err
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(b, &blocks) != nil || len(blocks) == 0 {
		return nil
	}
	text := ""
	for _, block := range blocks {
		if block.Type != "text" {
			return nil
		}
		text += block.Text
	}
	if strings.TrimSpace(text) == "" {
		return nil
	}
	var root string
	err = tx.QueryRow(ctx, workflowDeliveryRootSQL, sid, trigger).Scan(&root)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE workflow_run_terminal_results SET consumed_trigger_id=$2,consumed_at=$3 WHERE session_id=$1 AND primary_event_id=$4 AND consumed_at IS NULL`, sid, trigger, s.clock.Now().UTC(), root)
	return err
}
