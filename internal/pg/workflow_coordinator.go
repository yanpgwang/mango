package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

// WorkflowCoordinatorClaim carries trusted Temporal ownership metadata, never
// business invocation identity. Only explicit Continue-As-New may transfer it.
// Reset/retry/cron and independent database restore are unsupported.
type WorkflowCoordinatorClaim struct {
	ChainID, ExecutionID, PredecessorID string
	Cursor, PreviousEpoch               int64
}

// BindWorkflowCoordinator keeps the first chain permanently bound in PG, even
// after Temporal forgets a closed history. A fresh start cannot adopt a Run.
// Session-first locking serializes binding with frontier writes and lifecycle.
func (s *Store) BindWorkflowCoordinator(ctx context.Context, sessionID, runID string, in WorkflowCoordinatorClaim) error {
	if !validWorkflowRequestID(in.ChainID) || !validWorkflowRequestID(in.ExecutionID) || (in.PredecessorID != "" && !validWorkflowRequestID(in.PredecessorID)) || in.Cursor < 0 || in.Cursor > 256 || in.PreviousEpoch < 0 || in.PreviousEpoch > 256 {
		return domain.Validation("invalid Workflow coordinator ownership")
	}
	return s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		session, err := s.lockWorkflowSession(ctx, q, sessionID)
		if err != nil {
			return err
		}
		f, err := lockWorkflowFrontier(ctx, tx, sessionID, runID)
		if err != nil {
			return err
		}
		if err = s.checkWorkflowFrontierAuthority(session, f); err != nil {
			return err
		}
		var chain string
		if err = tx.QueryRow(ctx, `SELECT coordinator_chain_id FROM workflow_runs WHERE id=$1`, runID).Scan(&chain); err != nil {
			return err
		}
		switch {
		case chain == "":
			if in.ChainID != in.ExecutionID || in.PredecessorID != "" || in.Cursor != 0 || in.PreviousEpoch != 0 || f.cursor != 0 || f.epoch != 0 {
				return domain.Conflict("Workflow coordinator requires initial ownership reconciliation")
			}
		case chain != in.ChainID:
			return domain.Conflict("Workflow coordinator chain is already bound")
		case f.coordinator == in.ExecutionID:
			return nil // lost bind ACK or same physical execution, never a transfer.
		default:
			// A waiting pump has acquired previous+1 without advancing the cursor;
			// a progressing pump has carried its committed epoch. Both are bounded.
			if in.PredecessorID != f.coordinator || in.ExecutionID == in.PredecessorID || in.Cursor != f.cursor || (f.epoch != in.PreviousEpoch && f.epoch != in.PreviousEpoch+1) {
				return domain.Conflict("Workflow coordinator handoff is stale")
			}
			if f.epoch == in.PreviousEpoch+1 {
				var waiting bool
				if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_evaluator_claims WHERE run_id=$1 AND claim_id=$2 AND previous_epoch=$3 AND epoch=$4 AND journal_cursor=$5)`, runID, fmt.Sprintf("pump/%d", in.Cursor), in.PreviousEpoch, f.epoch, in.Cursor).Scan(&waiting); err != nil {
					return err
				}
				if !waiting {
					return domain.Conflict("Workflow coordinator waiting checkpoint is not owned")
				}
			}
		}
		_, err = tx.Exec(ctx, `UPDATE workflow_runs SET coordinator_chain_id=$2,coordinator_execution_id=$3 WHERE id=$1`, runID, in.ChainID, in.ExecutionID)
		return err
	})
}

func checkWorkflowCoordinator(f workflowFrontier, owner string) error {
	if f.coordinator != owner {
		return domain.Conflict("stale Workflow coordinator execution")
	}
	return nil
}

// ClaimWorkflowCoordinatorEvaluator is the coordinator-bound variant of the
// existing frontier claim. The unfenced internal scaffold cannot bypass a bind.
func (s *Store) ClaimWorkflowCoordinatorEvaluator(ctx context.Context, sessionID, runID, claimID string, previousEpoch int64, executionID string) (WorkflowEvaluatorClaim, error) {
	if !validWorkflowRequestID(executionID) {
		return WorkflowEvaluatorClaim{}, domain.Validation("missing Workflow coordinator execution")
	}
	return s.claimWorkflowEvaluator(ctx, sessionID, runID, claimID, previousEpoch, executionID)
}
