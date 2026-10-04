package pg

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/permission"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

func validatePermissionReceiptIdentity(in domain.ToolPermissionReceipt) error {
	if in.SessionID == "" || in.ThreadID == "" || in.TriggerEventID == "" || in.AttemptID == "" ||
		in.ToolUseEventID == "" || in.ToolName == "" || in.Model.ID == "" ||
		len(in.InvocationHash) != 64 || len(in.ContextHash) != 64 {
		return domain.Validation("permission receipt requires an owner, invocation, model, and context fingerprint")
	}
	return nil
}

func samePermissionReceiptIdentity(a, b domain.ToolPermissionReceipt) bool {
	return a.ToolPermissionOwner == b.ToolPermissionOwner && a.ToolUseEventID == b.ToolUseEventID &&
		a.ToolName == b.ToolName && a.InvocationHash == b.InvocationHash &&
		a.ContextHash == b.ContextHash && a.Model.ID == b.Model.ID
}

func readPermissionReceipt(ctx context.Context, query interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, in domain.ToolPermissionReceipt) (*domain.ToolPermissionReceipt, error) {
	var body []byte
	err := query.QueryRow(ctx, `SELECT body FROM tool_permission_evaluations WHERE session_id=$1 AND tool_use_event_id=$2`, in.SessionID, in.ToolUseEventID).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var stored domain.ToolPermissionReceipt
	if err := json.Unmarshal(body, &stored); err != nil {
		return nil, err
	}
	if !samePermissionReceiptIdentity(stored, in) {
		return nil, domain.Conflict("permission receipt belongs to a different owner or assessment context")
	}
	return &stored, nil
}

// GetToolPermissionReceipt reads judgment facts, not permission to execute.
// Admission must separately validate the current durable turn under locks.
func (s *Store) GetToolPermissionReceipt(ctx context.Context, in domain.ToolPermissionReceipt) (*domain.ToolPermissionReceipt, error) {
	if err := validatePermissionReceiptIdentity(in); err != nil {
		return nil, err
	}
	return readPermissionReceipt(ctx, s.pool, in)
}

// EnsureToolPermissionAttempt establishes bookkeeping in a short Activity
// before interruptible inference. An already-admitted interrupt may still close
// this empty attempt, but a consumed or terminal owner cannot be recreated.
func (s *Store) EnsureToolPermissionAttempt(ctx context.Context, owner domain.ToolPermissionOwner) (TurnAttempt, error) {
	if owner.SessionID == "" || owner.ThreadID == "" || owner.TriggerEventID == "" || owner.AttemptID == "" {
		return TurnAttempt{}, domain.Validation("permission turn owner is required")
	}
	var attempt TurnAttempt
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		if err := s.lockTurnExecutionOwner(ctx, tx, q, owner.SessionID, owner.TriggerEventID); err != nil {
			return err
		}
		row, err := q.LockSession(ctx, owner.SessionID)
		if err != nil {
			return err
		}
		session, err := sessionFromLockRow(row)
		if err != nil {
			return err
		}
		trigger, err := q.GetEvent(ctx, pgstore.GetEventParams{SessionID: owner.SessionID, ID: owner.TriggerEventID})
		if err != nil {
			return err
		}
		if trigger.ThreadID != owner.ThreadID {
			return domain.Conflict("permission trigger belongs to another Thread")
		}
		if err := validatePermissionTrigger(ctx, q, session, trigger); err != nil {
			return err
		}
		existing, err := q.GetTurnAttempt(ctx, owner.AttemptID)
		if err == nil && existing.State != string(domain.RunAttemptActive) {
			return domain.Conflict("permission attempt has already completed")
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		return s.ensureAttemptLocked(ctx, tx, q, owner.SessionID, owner.TriggerEventID, owner.AttemptID, &attempt)
	})
	return attempt, err
}

func validatePermissionTrigger(ctx context.Context, q *pgstore.Queries, session domain.Session, trigger pgstore.Event) error {
	if !trigger.ProcessedAt.Valid {
		return nil
	}
	resume, err := q.IsUnresolvedPendingResolution(ctx, pgstore.IsUnresolvedPendingResolutionParams{
		SessionID: session.ID, ThreadID: trigger.ThreadID, ResolvingEventID: &trigger.ID,
	})
	if err != nil {
		return err
	}
	var payload map[string]any
	if err := json.Unmarshal(trigger.Payload, &payload); err != nil {
		return err
	}
	if !resume && !activeReceiptProcessedOutcome(session, trigger.Type, payload) {
		return domain.Conflict("permission judgment turn has already completed")
	}
	return nil
}

// RecordToolPermissionReceipt preserves the first committed judgment
// and accounts exactly those winning facts in the same transaction. Existing
// terminal owners can retain late known usage; this operation cannot establish
// an attempt or authorize an action. Deletion never recreates missing rows.
func (s *Store) RecordToolPermissionReceipt(ctx context.Context, in domain.ToolPermissionReceipt) (domain.ToolPermissionReceipt, error) {
	if err := validatePermissionReceiptIdentity(in); err != nil {
		return domain.ToolPermissionReceipt{}, err
	}
	if !in.Decision.Valid() {
		return domain.ToolPermissionReceipt{}, domain.Validation("invalid permission judgment")
	}
	var winner domain.ToolPermissionReceipt
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		// The Session lock orders candidate commits and deletion; response facts
		// remain useful even when an interrupt has already fenced the attempt.
		if _, err := q.LockSession(ctx, in.SessionID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("session not found")
		} else if err != nil {
			return err
		}
		existing, err := readPermissionReceipt(ctx, tx, in)
		if err != nil {
			return err
		}
		if _, _, err := s.lockPermissionOwner(ctx, tx, q, in.ToolPermissionOwner, false); err != nil {
			return err
		}
		winner = in
		if existing != nil {
			winner = *existing
		} else {
			body, err := json.Marshal(winner)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO tool_permission_evaluations
    (session_id, tool_use_event_id, thread_id, trigger_event_id, attempt_id, body, created_at)
VALUES ($1,$2,$3,$4,$5,$6,$7)`, winner.SessionID, winner.ToolUseEventID, winner.ThreadID,
				winner.TriggerEventID, winner.AttemptID, body, s.clock.Now().UTC()); err != nil {
				return err
			}
		}
		if !winner.ResponseReceived {
			return nil
		}
		if err := validatePermissionAccounting(ctx, tx, winner); err != nil {
			return err
		}
		return s.accountModelRequestLocked(ctx, tx, q, winner.SessionID, winner.ThreadID,
			winner.ToolUseEventID, winner.Model, winner.Usage, winner.StopReason)
	})
	if err == nil {
		s.notifySession(ctx, in.SessionID)
	}
	return winner, err
}

func validatePermissionAccounting(ctx context.Context, tx pgx.Tx, receipt domain.ToolPermissionReceipt) error {
	var threadID, modelID, stopReason string
	var usageJSON []byte
	err := tx.QueryRow(ctx, `
SELECT thread_id, model_id, stop_reason, usage FROM model_request_usage
WHERE session_id=$1 AND request_event_id=$2`, receipt.SessionID, receipt.ToolUseEventID).Scan(&threadID, &modelID, &stopReason, &usageJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var usage domain.TokenUsage
	if err := json.Unmarshal(usageJSON, &usage); err != nil {
		return err
	}
	expected := receipt.Usage
	expected.Speed = normalizedUsageSpeed(expected.Speed)
	if threadID != receipt.ThreadID || modelID != receipt.Model.ID || stopReason != receipt.StopReason || !reflect.DeepEqual(usage, expected) {
		return domain.Conflict("permission response conflicts with recorded usage")
	}
	return nil
}

// lockPermissionOwner follows Session → Thread → attempt ordering. The facts
// path validates immutable identity; the active path additionally rejects
// lifecycle fences and any durable interrupt after this turn's trigger.
func (s *Store) lockPermissionOwner(ctx context.Context, tx pgx.Tx, q *pgstore.Queries, owner domain.ToolPermissionOwner, active bool) (domain.Session, domain.SessionThread, error) {
	row, err := q.LockSession(ctx, owner.SessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Session{}, domain.SessionThread{}, domain.NotFound("session not found")
	}
	if err != nil {
		return domain.Session{}, domain.SessionThread{}, err
	}
	session, err := sessionFromLockRow(row)
	if err != nil {
		return domain.Session{}, domain.SessionThread{}, err
	}
	thread, err := loadSessionThreadForUpdate(ctx, tx, owner.SessionID, owner.ThreadID)
	if err != nil {
		return session, thread, err
	}
	trigger, err := q.GetEvent(ctx, pgstore.GetEventParams{SessionID: owner.SessionID, ID: owner.TriggerEventID})
	if err != nil {
		return session, thread, err
	}
	attempt, err := q.GetTurnAttempt(ctx, owner.AttemptID)
	if errors.Is(err, pgx.ErrNoRows) {
		return session, thread, domain.Conflict("permission judgment requires an existing turn attempt")
	}
	if err != nil {
		return session, thread, err
	}
	if trigger.ThreadID != owner.ThreadID || attempt.SessionID != owner.SessionID || attempt.TriggerEventID != owner.TriggerEventID {
		return session, thread, domain.Conflict("permission judgment belongs to another turn")
	}
	if !active {
		return session, thread, nil
	}
	if row.DeletingAt.Valid || session.ArchivedAt != nil || session.Status == domain.StatusTerminated ||
		thread.ArchivedAt != nil || thread.Status == domain.StatusTerminated || attempt.State != string(domain.RunAttemptActive) {
		return session, thread, domain.Conflict("permission judgment no longer has execution authority")
	}
	if err := validatePermissionTrigger(ctx, q, session, trigger); err != nil {
		return session, thread, err
	}
	interrupt, err := q.FirstUnprocessedThreadInterruptAfter(ctx, pgstore.FirstUnprocessedThreadInterruptAfterParams{
		SessionID: owner.SessionID, ThreadID: owner.ThreadID, AfterSeq: trigger.Seq,
	})
	if err == nil && interrupt.ID != "" {
		return session, thread, domain.Conflict("permission judgment turn was interrupted")
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return session, thread, err
	}
	return session, thread, nil
}

// AdmitToolPermissionEvaluation atomically validates ownership with any budget
// pause. A committed receipt has already been billed, so its cache hit bypasses
// the budget it consumed while retaining the execution authority check.
func (s *Store) AdmitToolPermissionEvaluation(ctx context.Context, in domain.ToolPermissionReceipt) (bool, error) {
	if err := validatePermissionReceiptIdentity(in); err != nil {
		return false, err
	}
	allowed := false
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		if _, _, err := s.lockPermissionOwner(ctx, tx, q, in.ToolPermissionOwner, true); err != nil {
			return err
		}
		existing, err := readPermissionReceipt(ctx, tx, in)
		if err != nil {
			return err
		}
		if existing != nil {
			allowed = true
			return nil
		}
		return s.admitModelRequestLocked(ctx, tx, q, in.SessionID, in.ThreadID, &allowed)
	})
	if err == nil && !allowed {
		s.notifySession(ctx, in.SessionID)
	}
	return allowed, err
}

// StartToolStepWithPermission linearizes a recorded allow with the ordinary
// prepared→started effect boundary. Interrupts admitted first block execution;
// effects admitted first retain the existing completion/ambiguity contract.
func (s *Store) StartToolStepWithPermission(ctx context.Context, stepID, toolUseEventID string) error {
	return s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		step, err := q.GetToolStep(ctx, stepID)
		if err != nil {
			return err
		}
		var body []byte
		err = tx.QueryRow(ctx, `SELECT body FROM tool_permission_evaluations WHERE tool_use_event_id=$1 AND attempt_id=$2`, toolUseEventID, step.AttemptID).Scan(&body)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Conflict("tool invocation has no committed permission judgment")
		}
		if err != nil {
			return err
		}
		var receipt domain.ToolPermissionReceipt
		if err := json.Unmarshal(body, &receipt); err != nil {
			return err
		}
		if _, _, err := s.lockPermissionOwner(ctx, tx, q, receipt.ToolPermissionOwner, true); err != nil {
			return err
		}
		var input map[string]any
		if err := json.Unmarshal(step.Input, &input); err != nil {
			return err
		}
		hash, err := permission.FingerprintInvocation(step.ToolName, input)
		if err != nil {
			return err
		}
		if receipt.ToolUseEventID != step.ToolUseEventID || receipt.ToolName != step.ToolName ||
			receipt.InvocationHash != hash || receipt.Decision.Type != "allow" {
			return domain.Conflict("permission judgment does not allow this exact invocation")
		}
		now := tsUTC(s.clock.Now().UTC())
		count, err := q.StartToolStep(ctx, pgstore.StartToolStepParams{ID: stepID, StartedAt: now, UpdatedAt: now})
		if err != nil {
			return err
		}
		if count != 1 {
			return domain.Conflict("invalid tool step transition")
		}
		return nil
	})
}

// validateToolPermissionDraftsLocked binds published auto outcomes to the
// immutable judgment before completion closes the owner or exposes a Work
// barrier. The caller holds the Session lock, shared with interrupt admission.
func (s *Store) validateToolPermissionDraftsLocked(ctx context.Context, tx pgx.Tx, q *pgstore.Queries, owner domain.ToolPermissionOwner, drafts []domain.EventDraft) error {
	for _, draft := range drafts {
		if draft.Type != domain.EvAgentToolUse && draft.Type != domain.EvAgentMcpToolUse {
			continue
		}
		raw, err := json.Marshal(draft.Payload["evaluation"])
		if err != nil {
			return err
		}
		var evaluation domain.ToolPermissionEvaluation
		if err := json.Unmarshal(raw, &evaluation); err != nil {
			return domain.Conflict("invalid tool permission evaluation")
		}
		if evaluation.Type != "auto" {
			continue
		}
		if evaluation.EvaluatedPermission == nil || !evaluation.EvaluatedPermission.Valid() || draft.Payload["evaluated_permission"] != evaluation.EvaluatedPermission.Type {
			return domain.Conflict("automatic tool outcome disagrees with its evaluation")
		}
		var body []byte
		err = tx.QueryRow(ctx, `SELECT body FROM tool_permission_evaluations WHERE session_id=$1 AND tool_use_event_id=$2`, owner.SessionID, draft.ID).Scan(&body)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Conflict("automatic tool action has no committed judgment")
		}
		if err != nil {
			return err
		}
		var receipt domain.ToolPermissionReceipt
		if err := json.Unmarshal(body, &receipt); err != nil {
			return err
		}
		if owner.AttemptID == "" {
			owner.AttemptID = receipt.AttemptID
		}
		if receipt.ToolPermissionOwner != owner || receipt.Decision != *evaluation.EvaluatedPermission {
			return domain.Conflict("automatic tool action belongs to another judgment")
		}
		session, thread, err := s.lockPermissionOwner(ctx, tx, q, owner, false)
		if err != nil {
			return err
		}
		if session.ArchivedAt != nil || session.Status == domain.StatusTerminated || thread.ArchivedAt != nil || thread.Status == domain.StatusTerminated {
			return domain.Conflict("automatic tool action owner is no longer runnable")
		}
		if _, pending := domain.PendingActionKindForEvent(draft.Type, draft.Payload); pending {
			if _, _, err := s.lockPermissionOwner(ctx, tx, q, owner, true); err != nil {
				return err
			}
		}
		attempt, err := q.GetTurnAttempt(ctx, owner.AttemptID)
		if err != nil {
			return err
		}
		if attempt.State != string(domain.RunAttemptActive) {
			return domain.Conflict("automatic tool action owner is no longer active")
		}
		name, _ := draft.Payload[domain.InternalPermissionToolName].(string)
		input, ok := draft.Payload["input"].(map[string]any)
		if !ok || name != receipt.ToolName {
			return domain.Conflict("automatic tool action has a different invocation")
		}
		if _, mcp := draft.Payload["mcp_server_name"]; !mcp && draft.Payload["name"] != name {
			return domain.Conflict("automatic local tool name differs from its judgment")
		}
		hash, err := permission.FingerprintInvocation(name, input)
		if err != nil {
			return err
		}
		if hash != receipt.InvocationHash {
			return domain.Conflict("automatic tool action input differs from its judgment")
		}
	}
	return nil
}
