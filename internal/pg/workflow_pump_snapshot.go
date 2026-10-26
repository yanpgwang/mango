package pg

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

// WorkflowPumpEntry combines an immutable admission decision with its current
// owned receipt. This is not an ordered Promise-delivery journal or replay ABI.
type WorkflowPumpEntry struct {
	RequestID string
	Command   WorkflowPumpCommand
	Decision  WorkflowPumpDecision
	Receipt   *WorkflowInvocationReceipt
}

// WorkflowPumpSnapshot stays inside an Activity. Source, prompts and receipts
// must not be returned through Temporal history or Continue-As-New parameters.
type WorkflowPumpSnapshot struct {
	Version   int64
	Request   WorkflowRunRequest
	Journal   []WorkflowPumpEntry
	Committed *WorkflowPumpDecision
}

// LoadWorkflowPumpSnapshot first recovers a committed request after ACK loss.
// Otherwise it reads the existing bounded prefix and receipts under the same
// Session/Run ownership locks used by admission and completion.
func (s *Store) LoadWorkflowPumpSnapshot(ctx context.Context, sessionID, runID string, in WorkflowPumpRequest) (WorkflowPumpSnapshot, error) {
	var result WorkflowPumpSnapshot
	if !validWorkflowRequestID(in.RequestID) || in.Epoch <= 0 || in.ExpectedCursor < 0 {
		return result, domain.Validation("invalid Workflow pump snapshot request")
	}
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
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
		var request []byte
		if err = tx.QueryRow(ctx, `SELECT request,version FROM workflow_runs WHERE id=$1`, runID).Scan(&request, &result.Version); err != nil {
			return err
		}
		var cursor int64
		var decision []byte
		err = tx.QueryRow(ctx, `SELECT expected_cursor,decision FROM workflow_pump_decisions WHERE run_id=$1 AND request_id=$2`, runID, in.RequestID).Scan(&cursor, &decision)
		if err == nil {
			if cursor != in.ExpectedCursor {
				return domain.Conflict("Workflow pump identity has different cursor")
			}
			return json.Unmarshal(decision, &result.Committed)
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if f.cursor != in.ExpectedCursor {
			return domain.Conflict("Workflow journal frontier changed")
		}
		if err = json.Unmarshal(request, &result.Request); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT pump.request_id,pump.command,pump.decision,attempt.id,attempt.receipt
FROM workflow_pump_decisions pump
LEFT JOIN workflow_invocations invocation ON invocation.run_id=pump.run_id AND invocation.ordinal=pump.expected_cursor
LEFT JOIN workflow_invocation_attempts attempt ON attempt.invocation_id=invocation.id AND attempt.attempt_no=1
WHERE pump.run_id=$1 ORDER BY pump.expected_cursor`, runID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var entry WorkflowPumpEntry
			var command, decision, receipt []byte
			var attemptID *string
			if err = rows.Scan(&entry.RequestID, &command, &decision, &attemptID, &receipt); err != nil {
				return err
			}
			if err = json.Unmarshal(command, &entry.Command); err != nil {
				return err
			}
			if err = json.Unmarshal(decision, &entry.Decision); err != nil {
				return err
			}
			if entry.Decision.Cursor != int64(len(result.Journal)+1) {
				return domain.Conflict("Workflow journal prefix is incomplete")
			}
			if entry.Decision.Attempt != nil && (attemptID == nil || *attemptID != entry.Decision.Attempt.ID) {
				return domain.Conflict("Workflow journal attempt ownership changed")
			}
			if receipt != nil {
				if err = json.Unmarshal(receipt, &entry.Receipt); err != nil {
					return err
				}
			}
			result.Journal = append(result.Journal, entry)
		}
		if err = rows.Err(); err != nil {
			return err
		}
		if int64(len(result.Journal)) != f.cursor {
			return domain.Conflict("Workflow journal prefix is incomplete")
		}
		return nil
	})
	return result, err
}
