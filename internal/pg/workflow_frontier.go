package pg

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

// This is an internal one-command pump boundary, not an evaluator or scheduler.
// Claim/request identities are application identities, never Temporal RunIDs.
type WorkflowEvaluatorClaim struct {
	Epoch, Cursor int64
}

type WorkflowPumpCommand struct {
	Kind             string          `json:"kind"`
	AgentName        string          `json:"agent_name"`
	Prompt           string          `json:"prompt"`
	InvocationCursor int64           `json:"invocation_cursor,omitempty"`
	Value            json.RawMessage `json:"value,omitempty"`
}

type WorkflowPumpRequest struct {
	RequestID              string
	Epoch, ExpectedCursor  int64
	CoordinatorExecutionID string
	Command                WorkflowPumpCommand
}

type WorkflowPumpDecision struct {
	Cursor    int64                      `json:"cursor"`
	Type      string                     `json:"type"`
	ErrorCode string                     `json:"error_code,omitempty"`
	Attempt   *WorkflowInvocationAttempt `json:"attempt,omitempty"`
	Delivery  *WorkflowPumpDelivery      `json:"delivery,omitempty"`
	Value     json.RawMessage            `json:"value,omitempty"`
}

type workflowFrontier struct {
	epoch, cursor  int64
	bytes          int64
	state          string
	coordinator    string
	deadline       time.Time
	pauses         []string
	roster         []byte
	maxInvocations int
	maxResultBytes int
}

func lockWorkflowFrontier(ctx context.Context, tx pgx.Tx, sessionID, runID string) (workflowFrontier, error) {
	var f workflowFrontier
	err := tx.QueryRow(ctx, `SELECT evaluator_epoch,journal_cursor,journal_bytes,state,deadline,pause_reasons,roster,max_invocations,coordinator_execution_id,max_result_bytes
FROM workflow_runs WHERE session_id=$1 AND id=$2 FOR UPDATE`, sessionID, runID).Scan(&f.epoch, &f.cursor, &f.bytes, &f.state, &f.deadline, &f.pauses, &f.roster, &f.maxInvocations, &f.coordinator, &f.maxResultBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return f, domain.NotFound("Workflow Run not found")
	}
	return f, err
}

func (s *Store) checkWorkflowFrontierAuthority(session domain.Session, f workflowFrontier) error {
	if session.ArchivedAt != nil || session.Status == domain.StatusTerminated || f.state != "open" || !f.deadline.After(s.clock.Now()) {
		return domain.Conflict("Workflow frontier owner or deadline is no longer eligible")
	}
	if len(f.pauses) != 0 || session.BudgetReached(s.clock.Now()) {
		return domain.Conflict("Workflow frontier is control-paused")
	}
	return nil
}

func validWorkflowRequestID(id string) bool {
	return id != "" && len(id) <= 256 && utf8.ValidString(id)
}

// ClaimWorkflowEvaluator is an explicit CAS ownership replacement. Repeating a
// committed claim returns its original snapshot; a stale epoch cannot write.
func (s *Store) ClaimWorkflowEvaluator(ctx context.Context, sessionID, runID, claimID string, previousEpoch int64) (WorkflowEvaluatorClaim, error) {
	return s.claimWorkflowEvaluator(ctx, sessionID, runID, claimID, previousEpoch, "")
}

func (s *Store) claimWorkflowEvaluator(ctx context.Context, sessionID, runID, claimID string, previousEpoch int64, owner string) (WorkflowEvaluatorClaim, error) {
	if !validWorkflowRequestID(claimID) || previousEpoch < 0 {
		return WorkflowEvaluatorClaim{}, domain.Validation("invalid Workflow evaluator claim")
	}
	var result WorkflowEvaluatorClaim
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		session, err := s.lockWorkflowSession(ctx, q, sessionID)
		if err != nil {
			return err
		}
		f, err := lockWorkflowFrontier(ctx, tx, sessionID, runID)
		if err != nil {
			return err
		}
		if err = checkWorkflowCoordinator(f, owner); err != nil {
			return err
		}
		var prior int64
		err = tx.QueryRow(ctx, `SELECT previous_epoch,epoch,journal_cursor FROM workflow_evaluator_claims WHERE run_id=$1 AND claim_id=$2`, runID, claimID).Scan(&prior, &result.Epoch, &result.Cursor)
		if err == nil {
			if prior != previousEpoch {
				return domain.Conflict("Workflow claim identity has different input")
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err = s.checkWorkflowFrontierAuthority(session, f); err != nil {
			return err
		}
		if f.epoch != previousEpoch {
			return domain.Conflict("Workflow evaluator epoch changed")
		}
		if f.epoch == 0 {
			var priorEffects bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_invocations WHERE run_id=$1)`, runID).Scan(&priorEffects); err != nil {
				return err
			}
			if priorEffects {
				return domain.Conflict("unfenced Workflow effects require reconciliation")
			}
		}
		result = WorkflowEvaluatorClaim{Epoch: f.epoch + 1, Cursor: f.cursor}
		if _, err = tx.Exec(ctx, `UPDATE workflow_runs SET evaluator_epoch=$2 WHERE id=$1`, runID, result.Epoch); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO workflow_evaluator_claims(session_id,run_id,claim_id,previous_epoch,epoch,journal_cursor) VALUES($1,$2,$3,$4,$5,$6)`, sessionID, runID, claimID, previousEpoch, result.Epoch, result.Cursor)
		return err
	})
	return result, err
}

// CommitWorkflowPump commits the decision and real Thread/input/outbox together.
// Control/protocol errors are never script-visible catchable decisions. Only
// bounded semantic host rejections enter this journal. A current owner can read
// the same decision after ACK loss or epoch replacement without admitting again.
func (s *Store) CommitWorkflowPump(ctx context.Context, sessionID, runID string, in WorkflowPumpRequest) (WorkflowPumpDecision, error) {
	if !validWorkflowRequestID(in.RequestID) || in.Epoch <= 0 || in.ExpectedCursor < 0 || !utf8.ValidString(in.Command.Kind) || !utf8.ValidString(in.Command.AgentName) || !utf8.ValidString(in.Command.Prompt) {
		return WorkflowPumpDecision{}, domain.Validation("invalid Workflow pump envelope")
	}
	encoded, err := json.Marshal(in.Command)
	if err != nil {
		return WorkflowPumpDecision{}, err
	}
	if len(encoded) > 512*1024 || len(in.Command.Kind) > 64 || in.Command.InvocationCursor < 0 || (len(in.Command.Value) > 0 && !validWorkflowProgramValue(in.Command.Value)) {
		return WorkflowPumpDecision{}, domain.Validation("Workflow pump protocol byte limit exceeded")
	}
	var result WorkflowPumpDecision
	err = s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		session, err := s.lockWorkflowSession(ctx, q, sessionID)
		if err != nil {
			return err
		}
		f, err := lockWorkflowFrontier(ctx, tx, sessionID, runID)
		if err != nil {
			return err
		}
		if err = checkWorkflowCoordinator(f, in.CoordinatorExecutionID); err != nil {
			return err
		}
		if f.epoch != in.Epoch {
			return domain.Conflict("stale Workflow evaluator epoch")
		}
		if err = s.checkWorkflowFrontierAuthority(session, f); err != nil {
			return err
		}
		var priorCursor int64
		var same bool
		var decision []byte
		err = tx.QueryRow(ctx, `SELECT expected_cursor,command=$3::jsonb,decision FROM workflow_pump_decisions WHERE run_id=$1 AND request_id=$2`, runID, in.RequestID, encoded).Scan(&priorCursor, &same, &decision)
		if err == nil {
			if priorCursor != in.ExpectedCursor || !same {
				return domain.Conflict("Workflow pump identity has different input")
			}
			return json.Unmarshal(decision, &result)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if f.cursor != in.ExpectedCursor {
			return domain.Conflict("Workflow journal frontier changed")
		}
		if f.cursor >= 256 {
			return domain.Conflict("Workflow command budget exhausted")
		}
		var returned bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_pump_decisions WHERE run_id=$1 AND decision->>'type'='return')`, runID).Scan(&returned); err != nil {
			return err
		}
		if returned {
			return domain.Conflict("Workflow program value is already committed")
		}
		if in.Command.Kind == "invoke" && (in.Command.InvocationCursor != 0 || len(in.Command.Value) != 0) {
			return domain.Validation("invalid Workflow invocation frame")
		}

		result = WorkflowPumpDecision{Cursor: f.cursor + 1, Type: "rejected"}
		var roster []domain.Agent
		if err = json.Unmarshal(f.roster, &roster); err != nil {
			return err
		}
		switch {
		case in.Command.Kind == "deliver" || in.Command.Kind == "return":
			entry, err := workflowProgramDecisionLocked(ctx, tx, runID, f, in.Command)
			if err != nil {
				return err
			}
			result = entry
			result.Cursor = f.cursor + 1
		case in.Command.Kind != "invoke":
			result.ErrorCode = "unsupported_command"
		case in.Command.AgentName == "" || len(in.Command.AgentName) > 256 || in.Command.Prompt == "" || len(in.Command.Prompt) > 65536:
			result.ErrorCode = "invalid_arguments"
		default:
			agent, agentErr := rosterMemberByName(roster, in.Command.AgentName)
			if agentErr != nil {
				result.ErrorCode = "agent_not_admitted"
				break
			}
			var count int
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM workflow_invocations WHERE run_id=$1`, runID).Scan(&count); err != nil {
				return err
			}
			if count >= f.maxInvocations {
				return domain.Conflict("Workflow invocation budget exhausted")
			}
			if err = tx.QueryRow(ctx, `SELECT count(*) FROM session_threads WHERE session_id=$1 AND kind<>'advisor' AND archived_at IS NULL`, sessionID).Scan(&count); err != nil {
				return err
			}
			if count >= maxConcurrentSessionThreads {
				return domain.Conflict("Workflow Thread capacity wait")
			}
			attempt, err := s.createWorkflowInvocationLocked(ctx, tx, q, session, runID, int(f.cursor), WorkflowInvocationCommand{AgentName: in.Command.AgentName, Prompt: in.Command.Prompt}, agent)
			if err != nil {
				return err
			}
			result.Type, result.Attempt = "invocation", &attempt
		}
		decision, err = json.Marshal(result)
		if err != nil {
			return err
		}
		bytesAfter := f.bytes + int64(len(encoded)+len(decision))
		if bytesAfter > 8*1024*1024 {
			return domain.Conflict("Workflow journal byte budget exhausted")
		}
		if _, err = tx.Exec(ctx, `INSERT INTO workflow_pump_decisions(session_id,run_id,request_id,epoch,expected_cursor,command,decision) VALUES($1,$2,$3,$4,$5,$6,$7)`, sessionID, runID, in.RequestID, in.Epoch, in.ExpectedCursor, encoded, decision); err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, `UPDATE workflow_runs SET journal_cursor=$2,journal_bytes=$3 WHERE id=$1`, runID, result.Cursor, bytesAfter); err != nil {
			return err
		}
		return s.advanceWorkflowRunLocked(ctx, tx, sessionID, runID)
	})
	return result, err
}

// SetWorkflowRunPause changes only an internal admission reason. Clearing one
// reason never clears another, removes budget authority, or extends a deadline.
func (s *Store) SetWorkflowRunPause(ctx context.Context, sessionID, runID, reason string, paused bool) error {
	if reason != "interrupt" && reason != "approval" && reason != "capability" {
		return domain.Validation("unsupported Workflow pause reason")
	}
	return s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		if _, err := s.lockWorkflowSession(ctx, q, sessionID); err != nil {
			return err
		}
		f, err := lockWorkflowFrontier(ctx, tx, sessionID, runID)
		if err != nil {
			return err
		}
		if f.state != "open" {
			return domain.Conflict("Workflow Run is closed")
		}
		next := make([]string, 0, len(f.pauses)+1)
		found := false
		for _, r := range f.pauses {
			if r == reason {
				found = true
				if !paused {
					continue
				}
			}
			next = append(next, r)
		}
		if paused && !found {
			next = append(next, reason)
		}
		if found == paused {
			return nil
		}
		if _, err = tx.Exec(ctx, `UPDATE workflow_runs SET pause_reasons=$2 WHERE id=$1`, runID, next); err != nil {
			return err
		}
		return s.advanceWorkflowRunLocked(ctx, tx, sessionID, runID)
	})
}

// Global interrupt admission shares the Session lock with frontier appends.
func (s *Store) pauseWorkflowRunsForInterruptLocked(ctx context.Context, tx pgx.Tx, sessionID string) error {
	rows, err := tx.Query(ctx, `UPDATE workflow_runs SET pause_reasons=array_append(pause_reasons,'interrupt'),version=version+1
WHERE session_id=$1 AND state='open' AND NOT ('interrupt'=ANY(pause_reasons)) RETURNING id,version`, sessionID)
	if err != nil {
		return err
	}
	var wakes []WorkflowRunWakeup
	for rows.Next() {
		var wake WorkflowRunWakeup
		wake.SessionID = sessionID
		if err = rows.Scan(&wake.RunID, &wake.Version); err != nil {
			rows.Close()
			return err
		}
		wakes = append(wakes, wake)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, wake := range wakes {
		if err = s.enqueueWorkflowRunLocked(ctx, tx, sessionID, wake.RunID, wake.Version); err != nil {
			return err
		}
	}
	return nil
}
