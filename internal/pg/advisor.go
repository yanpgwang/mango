package pg

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

// CompleteAdvisorToolStep atomically creates the automatically terminating
// Thread projection, both event-ledger views, model-specific usage, and the
// ordinary client-tool journal result. A lost Activity acknowledgement can
// therefore return the recorded result without repeating a billed inference.
func (s *Store) CompleteAdvisorToolStep(
	ctx context.Context,
	sessionID string,
	primaryThreadID string,
	triggerEventID string,
	stepID string,
	result domain.ToolStepResult,
	consultation domain.AdvisorConsultation,
) error {
	if sessionID == "" || primaryThreadID == "" || triggerEventID == "" || stepID == "" {
		return domain.Validation("advisor consultation session, primary Thread, trigger, and tool step are required")
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return err
	}
	var normalizedResult domain.ToolStepResult
	if err := json.Unmarshal(resultJSON, &normalizedResult); err != nil {
		return err
	}
	err = s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		row, err := q.LockSession(ctx, sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("session not found")
		}
		if err != nil {
			return err
		}
		if row.DeletingAt.Valid {
			return domain.Conflict("session deletion is in progress")
		}
		session, err := sessionFromLockRow(row)
		if err != nil {
			return err
		}
		configured := session.AgentSnapshot.Multiagent.Advisor()
		if configured == nil {
			return domain.Conflict("Session Agent has no advisor")
		}
		var primaryKind string
		if err := tx.QueryRow(ctx, `
SELECT kind FROM session_threads
WHERE session_id = $1 AND id = $2
FOR UPDATE`, sessionID, primaryThreadID).Scan(&primaryKind); errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("primary session Thread not found")
		} else if err != nil {
			return err
		}
		if primaryKind != "primary" {
			return domain.Conflict("advisor consultations belong to the primary Thread")
		}
		trigger, err := q.GetEvent(ctx, pgstore.GetEventParams{
			SessionID: sessionID, ID: triggerEventID,
		})
		if err != nil {
			return err
		}
		if trigger.ThreadID != primaryThreadID {
			return domain.Conflict("advisor consultation trigger belongs to another Thread")
		}
		stepRow, err := q.GetToolStep(ctx, stepID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("advisor tool step not found")
		}
		if err != nil {
			return err
		}
		attempt, err := q.GetTurnAttempt(ctx, stepRow.AttemptID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("advisor turn attempt not found")
		}
		if err != nil {
			return err
		}
		if attempt.SessionID != sessionID || attempt.TriggerEventID != triggerEventID ||
			stepRow.ToolName != "advisor" {
			return domain.Conflict("advisor tool step belongs to another turn")
		}
		if stepRow.State != string(domain.ToolStepStarted) &&
			stepRow.State != string(domain.ToolStepCompleted) {
			return domain.Conflict("advisor tool step has not crossed the execution boundary")
		}

		maxSeq, err := q.MaxEventSeq(ctx, sessionID)
		if err != nil {
			return err
		}
		now := s.clock.Now().UTC().Truncate(time.Microsecond)
		turnID := triggerEventID
		for consultationIndex, consultation := range []domain.AdvisorConsultation{consultation} {
			recordedAt := now.Add(time.Duration(consultationIndex) * time.Microsecond)
			consultation.Usage.Speed = normalizedUsageSpeed(consultation.Usage.Speed)
			if consultation.ThreadID == "" || consultation.UsageRequestID == "" ||
				len(consultation.LifecycleIDs) != 9 {
				return domain.Validation("advisor consultation identifiers are incomplete")
			}
			var exists bool
			if err := tx.QueryRow(ctx, `
SELECT EXISTS(
    SELECT 1 FROM session_threads WHERE session_id = $1 AND id = $2
)`, sessionID, consultation.ThreadID).Scan(&exists); err != nil {
				return err
			}
			if exists {
				if stepRow.State != string(domain.ToolStepCompleted) {
					return domain.Conflict("advisor projection exists before its tool result completed")
				}
				continue
			}
			if consultation.Model == "" {
				consultation.Model = configured.Model
			}
			if consultation.Model != configured.Model {
				return domain.Conflict("advisor model does not match the Session roster")
			}
			usageModel := consultation.UsageModel
			if usageModel == "" {
				usageModel = configured.Model
			}
			listCost, priceErr := domain.ModelResponseListCostNanoUSDAt(
				domain.Model{ID: usageModel}, consultation.Usage,
				consultation.StopReason, recordedAt,
			)
			listCostKnown := consultation.UsageKnown && priceErr == nil
			if !listCostKnown {
				listCost = 0
			}
			thread := domain.NewAdvisorSessionThread(
				consultation.ThreadID, sessionID, primaryThreadID,
				*configured, consultation.Usage, listCost, listCostKnown, recordedAt,
			)
			body, err := json.Marshal(thread)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO session_threads (
    id, session_id, parent_thread_id, kind, status, body,
    created_at, updated_at, archived_at
) VALUES ($1, $2, $3, 'advisor', $4, $5, $6, $7, NULL)`,
				thread.ID, sessionID, primaryThreadID, thread.Status, body,
				thread.CreatedAt, thread.UpdatedAt,
			); err != nil {
				return err
			}

			usageJSON, err := json.Marshal(consultation.Usage)
			if err != nil {
				return err
			}
			var storedCost any
			if listCostKnown {
				storedCost = listCost
			}
			if _, err := tx.Exec(ctx, `
INSERT INTO model_request_usage (
    session_id, thread_id, request_event_id, model_id,
    stop_reason, usage, list_cost_nano_usd, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
				sessionID, thread.ID, consultation.UsageRequestID, usageModel,
				consultation.StopReason, usageJSON, storedCost, recordedAt,
			); err != nil {
				return err
			}

			lifecycle := func(eventType, id string, extra map[string]any) domain.EventDraft {
				payload := map[string]any{
					"session_thread_id": thread.ID,
					"agent_name":        domain.AdvisorAgentName,
				}
				for key, value := range extra {
					payload[key] = value
				}
				return domain.EventDraft{ID: id, Type: eventType, Payload: payload}
			}
			appendOne := func(threadID string, draft domain.EventDraft, turn *string) error {
				_, next, err := s.appendThreadDraftsAt(
					ctx, q, sessionID, threadID,
					[]domain.EventDraft{draft}, maxSeq, turn, recordedAt,
				)
				maxSeq = next
				return err
			}
			if err := appendOne(primaryThreadID, lifecycle(
				domain.EvSessionThreadCreated, consultation.LifecycleIDs[0], nil,
			), &turnID); err != nil {
				return err
			}
			if err := appendOne(thread.ID, lifecycle(
				domain.EvSessionThreadStatusRunning, consultation.LifecycleIDs[1], nil,
			), nil); err != nil {
				return err
			}
			if err := appendOne(primaryThreadID, lifecycle(
				domain.EvSessionThreadStatusRunning, consultation.LifecycleIDs[2], nil,
			), &turnID); err != nil {
				return err
			}
			if consultation.AdviceDelivered {
				if err := appendOne(thread.ID, domain.EventDraft{
					ID: consultation.LifecycleIDs[3], Type: domain.EvAgentThreadMessageSent,
					Payload: map[string]any{
						"to_session_thread_id": primaryThreadID,
						"to_agent_name":        session.AgentSnapshot.Name,
						"content":              consultation.PublicContent,
					},
				}, nil); err != nil {
					return err
				}
				if err := appendOne(primaryThreadID, domain.EventDraft{
					ID: consultation.LifecycleIDs[4], Type: domain.EvAgentThreadMessageReceived,
					Payload: map[string]any{
						"from_session_thread_id": thread.ID,
						"from_agent_name":        domain.AdvisorAgentName,
						"content":                consultation.PublicContent,
					},
				}, &turnID); err != nil {
					return err
				}
				// Unlike a persistent child report, Advisor advice is returned through
				// the executor's private tool result. Mark the public
				// observation processed in the same transaction so it cannot drive a
				// second primary turn and sorts causally with the surrounding
				// lifecycle events.
				if err := q.MarkEventProcessed(ctx, pgstore.MarkEventProcessedParams{
					ProcessedAt: tsUTC(recordedAt), SessionID: sessionID,
					ID: consultation.LifecycleIDs[4],
				}); err != nil {
					return err
				}
			}
			idleExtra := map[string]any{
				"stop_reason": map[string]any{"type": "end_turn"},
			}
			if err := appendOne(thread.ID, lifecycle(
				domain.EvSessionThreadStatusIdle, consultation.LifecycleIDs[5], idleExtra,
			), nil); err != nil {
				return err
			}
			if err := appendOne(primaryThreadID, lifecycle(
				domain.EvSessionThreadStatusIdle, consultation.LifecycleIDs[6], idleExtra,
			), &turnID); err != nil {
				return err
			}
			if err := appendOne(thread.ID, lifecycle(
				domain.EvSessionThreadStatusTerminated, consultation.LifecycleIDs[7], nil,
			), nil); err != nil {
				return err
			}
			if err := appendOne(primaryThreadID, lifecycle(
				domain.EvSessionThreadStatusTerminated, consultation.LifecycleIDs[8], nil,
			), &turnID); err != nil {
				return err
			}

			session.Usage.Add(consultation.Usage)
			if listCostKnown {
				session.ModelListCostNanoUSD += listCost
			} else {
				session.ListCostKnown = false
			}
			session.UpdatedAt = recordedAt
		}
		if err := putSessionOnlyTx(ctx, tx, session); err != nil {
			return err
		}
		affected, err := q.CompleteToolStep(ctx, pgstore.CompleteToolStepParams{
			Result: resultJSON, FinishedAt: tsUTC(now), UpdatedAt: tsUTC(now), ID: stepID,
		})
		if err != nil {
			return err
		}
		if affected == 1 {
			return nil
		}
		existing, err := q.GetToolStep(ctx, stepID)
		if err != nil {
			return err
		}
		stored, err := toolStepFromRow(existing)
		if err != nil {
			return err
		}
		if stored.State == domain.ToolStepCompleted && stored.Result != nil &&
			reflect.DeepEqual(*stored.Result, normalizedResult) {
			return nil
		}
		return domain.Conflict("invalid advisor tool step transition")
	})
	if err == nil {
		s.notifySession(ctx, sessionID)
	}
	return err
}
