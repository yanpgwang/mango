package pg

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
)

// WorkflowPumpDelivery is an immutable script-visible receipt observation in
// the existing frontier journal. Physical completion and program delivery are
// distinct: replay must deliver in journal order, not resolve every cached call.
type WorkflowPumpDelivery struct {
	InvocationCursor int64                     `json:"invocation_cursor"`
	Receipt          WorkflowInvocationReceipt `json:"receipt"`
}

func validWorkflowProgramValue(data []byte) bool {
	if len(data) > 32768 {
		return false
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return false
	}
	nodes := 4096
	var visit func(any, int) bool
	visit = func(value any, depth int) bool {
		nodes--
		if depth > 32 || nodes < 0 {
			return false
		}
		switch v := value.(type) {
		case []any:
			for _, child := range v {
				if !visit(child, depth+1) {
					return false
				}
			}
		case map[string]any:
			for _, child := range v {
				if !visit(child, depth+1) {
					return false
				}
			}
		}
		return true
	}
	return visit(value, 0)
}

// The caller already holds Session then Run locks and checked owner/epoch,
// lifecycle, cached identity and cursor. No interpreter or Thread IO runs here.
// Return persists a candidate value only: it does not close the Run, notify the
// primary, resume grading or implement the product completion barrier.
func workflowProgramDecisionLocked(ctx context.Context, tx pgx.Tx, runID string, f workflowFrontier, command WorkflowPumpCommand) (WorkflowPumpDecision, error) {
	var result WorkflowPumpDecision
	if command.AgentName != "" || command.Prompt != "" {
		return result, domain.Validation("invalid Workflow program frame")
	}
	switch command.Kind {
	case "deliver":
		if len(command.Value) != 0 || command.InvocationCursor >= f.cursor {
			return result, domain.Validation("invalid Workflow delivery target")
		}
		var priorJSON []byte
		err := tx.QueryRow(ctx, `SELECT decision FROM workflow_pump_decisions WHERE run_id=$1 AND expected_cursor=$2 AND command->>'kind'='invoke'`, runID, command.InvocationCursor).Scan(&priorJSON)
		if errors.Is(err, pgx.ErrNoRows) {
			return result, domain.Conflict("Workflow delivery does not own an invocation")
		}
		if err != nil {
			return result, err
		}
		var prior WorkflowPumpDecision
		if err = json.Unmarshal(priorJSON, &prior); err != nil {
			return result, err
		}
		var delivered bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_pump_decisions WHERE run_id=$1 AND decision->>'type'='delivery' AND (decision->'delivery'->>'invocation_cursor')::bigint=$2)`, runID, command.InvocationCursor).Scan(&delivered); err != nil {
			return result, err
		}
		if delivered {
			return result, domain.Conflict("Workflow receipt already delivered")
		}
		receipt := WorkflowInvocationReceipt{AdapterVersion: 1, Type: "failure", ErrorCode: prior.ErrorCode}
		switch prior.Type {
		case "rejected":
			if receipt.ErrorCode == "" {
				return result, domain.Conflict("Workflow rejection lacks an outcome")
			}
		case "invocation":
			if prior.Attempt == nil {
				return result, domain.Conflict("Workflow invocation lacks an owner")
			}
			var encoded []byte
			err = tx.QueryRow(ctx, `SELECT receipt FROM workflow_invocation_attempts WHERE run_id=$1 AND id=$2 AND state='settled' AND terminal_event_id IS NOT NULL`, runID, prior.Attempt.ID).Scan(&encoded)
			if errors.Is(err, pgx.ErrNoRows) {
				return result, domain.Conflict("Workflow receipt is not settled")
			}
			if err != nil {
				return result, err
			}
			if err = json.Unmarshal(encoded, &receipt); err != nil {
				return result, err
			}
		default:
			return result, domain.Conflict("Workflow delivery target is not an invocation")
		}
		result.Type = "delivery"
		result.Delivery = &WorkflowPumpDelivery{InvocationCursor: command.InvocationCursor, Receipt: receipt}
	case "return":
		if command.InvocationCursor != 0 || len(command.Value) == 0 || len(command.Value) > f.maxResultBytes || !validWorkflowProgramValue(command.Value) {
			return result, domain.Validation("invalid Workflow return value")
		}
		var pending bool
		var calls, deliveries int
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM workflow_invocation_attempts WHERE run_id=$1 AND (state<>'settled' OR receipt IS NULL)),
 (SELECT count(*) FROM workflow_pump_decisions WHERE run_id=$1 AND command->>'kind'='invoke'),
 (SELECT count(*) FROM workflow_pump_decisions WHERE run_id=$1 AND decision->>'type'='delivery')`, runID).Scan(&pending, &calls, &deliveries); err != nil {
			return result, err
		}
		if pending || calls != deliveries {
			return result, domain.Conflict("Workflow program has undelivered work")
		}
		result.Type = "return"
		result.Value = command.Value
	}
	return result, nil
}
