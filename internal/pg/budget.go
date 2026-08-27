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

func (s *Store) resumeBudgetPausedThreadsLocked(
	ctx context.Context,
	tx pgx.Tx,
	q *pgstore.Queries,
	session *domain.Session,
	maxSeq int64,
) (int64, error) {
	rows, err := tx.Query(ctx, `
SELECT body
FROM session_threads
WHERE session_id = $1 AND archived_at IS NULL
ORDER BY id
FOR UPDATE`, session.ID)
	if err != nil {
		return maxSeq, err
	}
	var paused []domain.SessionThread
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			rows.Close()
			return maxSeq, err
		}
		var thread domain.SessionThread
		if err := json.Unmarshal(body, &thread); err != nil {
			rows.Close()
			return maxSeq, err
		}
		if thread.BudgetPaused {
			paused = append(paused, thread)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return maxSeq, err
	}
	rows.Close()
	if len(paused) == 0 {
		return maxSeq, nil
	}

	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	for _, thread := range paused {
		thread.BudgetPaused = false
		thread.TransitionStatus(domain.StatusRunning, now)
		if err := putSessionThreadTx(ctx, tx, thread); err != nil {
			return maxSeq, err
		}
		payload := threadLifecyclePayload(thread)
		_, maxSeq, err = s.appendThreadDrafts(
			ctx, q, session.ID, thread.ID,
			[]domain.EventDraft{{
				Type: domain.EvSessionThreadStatusRunning, Payload: payload,
			}}, maxSeq, nil,
		)
		if err != nil {
			return maxSeq, err
		}
		if thread.ParentThreadID == nil {
			if err := q.UpsertOutbox(ctx, pgstore.UpsertOutboxParams{
				SessionID: session.ID, MaxEventSeq: maxSeq, EnqueuedAt: tsUTC(now),
			}); err != nil {
				return maxSeq, err
			}
			continue
		}
		_, maxSeq, err = s.appendThreadDrafts(
			ctx, q, session.ID, *thread.ParentThreadID,
			[]domain.EventDraft{{
				Type: domain.EvSessionThreadStatusRunning, Payload: payload,
			}}, maxSeq, nil,
		)
		if err != nil {
			return maxSeq, err
		}
		if err := q.UpsertThreadOutbox(ctx, pgstore.UpsertThreadOutboxParams{
			SessionID: session.ID, ThreadID: thread.ID,
			MaxEventSeq: maxSeq, EnqueuedAt: tsUTC(now),
		}); err != nil {
			return maxSeq, err
		}
	}
	aggregated, err := aggregateSessionThreadStatus(ctx, tx, session.ID)
	if err != nil {
		return maxSeq, err
	}
	if session.Status != aggregated {
		session.TransitionStatus(aggregated, now)
		if aggregated == domain.StatusRunning {
			_, maxSeq, err = s.appendDrafts(
				ctx, q, session.ID,
				[]domain.EventDraft{{
					Type: domain.EvSessionStatusRunning, Payload: map[string]any{},
				}}, maxSeq, nil,
			)
			if err != nil {
				return maxSeq, err
			}
		}
	}
	return maxSeq, nil
}

// AccountModelRequest records one provider response exactly once and updates
// the owning Thread plus the shared Session projection in the same transaction.
func (s *Store) AccountModelRequest(
	ctx context.Context,
	sessionID string,
	threadID string,
	requestEventID string,
	model domain.Model,
	usage domain.TokenUsage,
	stopReason string,
) error {
	if requestEventID == "" {
		return domain.Validation("model request event id is required")
	}
	usage.Speed = normalizedUsageSpeed(usage.Speed)
	usageJSON, err := json.Marshal(usage)
	if err != nil {
		return err
	}
	pricedAt := s.clock.Now().UTC()
	listCost, priceErr := domain.ModelResponseListCostNanoUSDAt(
		model, usage, stopReason, pricedAt,
	)
	known := priceErr == nil
	err = s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		row, err := q.LockSession(ctx, sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("session not found")
		}
		if err != nil {
			return err
		}
		session, err := sessionFromLockRow(row)
		if err != nil {
			return err
		}
		if threadID == "" {
			threadID, err = q.GetPrimarySessionThreadID(ctx, sessionID)
			if err != nil {
				return err
			}
		}
		thread, err := loadSessionThreadForUpdate(ctx, tx, sessionID, threadID)
		if err != nil {
			return err
		}
		var storedCost any
		if known {
			storedCost = listCost
		}
		command, err := tx.Exec(ctx, `
INSERT INTO model_request_usage (
    session_id, thread_id, request_event_id, model_id, stop_reason,
    usage, list_cost_nano_usd, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (session_id, request_event_id) DO NOTHING`,
			sessionID, threadID, requestEventID, model.ID, stopReason,
			usageJSON, storedCost, pricedAt,
		)
		if err != nil {
			return err
		}
		if command.RowsAffected() == 0 {
			return nil
		}

		session.Usage.Add(usage)
		thread.Usage.Add(usage)
		if known {
			session.ModelListCostNanoUSD += listCost
			thread.ModelListCostNanoUSD += listCost
		} else {
			session.ListCostKnown = false
			thread.ListCostKnown = false
		}
		now := s.clock.Now().UTC().Truncate(time.Microsecond)
		session.UpdatedAt = now
		thread.UpdatedAt = now
		if err := putSessionThreadTx(ctx, tx, thread); err != nil {
			return err
		}
		return putSessionOnlyTx(ctx, tx, session)
	})
	if err == nil {
		s.notifySession(ctx, sessionID)
	}
	return err
}

func normalizedUsageSpeed(speed string) string {
	if speed == "standard" || speed == "fast" {
		return speed
	}
	return "standard"
}

// AdmitModelRequest checks the shared Session ceiling under the Session row
// lock. Once reached, the owning Thread becomes durably budget-paused; a later
// budget update wakes this same in-flight Workflow turn to retry admission.
func (s *Store) AdmitModelRequest(
	ctx context.Context,
	sessionID string,
	threadID string,
) (bool, error) {
	allowed := false
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		row, err := q.LockSession(ctx, sessionID)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("session not found")
		}
		if err != nil {
			return err
		}
		session, err := sessionFromLockRow(row)
		if err != nil {
			return err
		}
		now := s.clock.Now().UTC().Truncate(time.Microsecond)
		if !session.BudgetReached(now) {
			allowed = true
			return nil
		}
		if threadID == "" {
			threadID, err = q.GetPrimarySessionThreadID(ctx, sessionID)
			if err != nil {
				return err
			}
		}
		thread, err := loadSessionThreadForUpdate(ctx, tx, sessionID, threadID)
		if err != nil {
			return err
		}
		if thread.BudgetPaused {
			return nil
		}

		thread.BudgetPaused = true
		thread.TransitionStatus(domain.StatusIdle, now)
		if err := putSessionThreadTx(ctx, tx, thread); err != nil {
			return err
		}
		maxSeq, err := q.MaxEventSeq(ctx, sessionID)
		if err != nil {
			return err
		}
		threadPayload := threadLifecyclePayload(thread)
		threadPayload["stop_reason"] = map[string]any{"type": "budget_reached"}
		_, maxSeq, err = s.appendThreadDrafts(
			ctx, q, sessionID, thread.ID,
			[]domain.EventDraft{{
				Type: domain.EvSessionThreadStatusIdle, Payload: threadPayload,
			}}, maxSeq, nil,
		)
		if err != nil {
			return err
		}
		if thread.ParentThreadID != nil {
			_, maxSeq, err = s.appendThreadDrafts(
				ctx, q, sessionID, *thread.ParentThreadID,
				[]domain.EventDraft{{
					Type: domain.EvSessionThreadStatusIdle, Payload: threadPayload,
				}}, maxSeq, nil,
			)
			if err != nil {
				return err
			}
		}

		aggregated, err := aggregateSessionThreadStatus(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		if aggregated != session.Status {
			session.TransitionStatus(aggregated, now)
			if aggregated == domain.StatusIdle {
				pendingIDs, err := q.SessionPendingClientActionEventIDs(ctx, sessionID)
				if err != nil {
					return err
				}
				stopReason := map[string]any{"type": "budget_reached"}
				drafts := []domain.EventDraft(nil)
				if len(pendingIDs) > 0 {
					stopReason = map[string]any{
						"type": "requires_action", "event_ids": pendingIDs,
					}
				} else {
					drafts = append(drafts, domain.EventDraft{
						Type:    domain.EvSessionUsage,
						Payload: session.UsageEventPayload(now),
					})
				}
				drafts = append(drafts, domain.EventDraft{
					Type:    domain.EvSessionStatusIdle,
					Payload: map[string]any{"stop_reason": stopReason},
				})
				_, _, err = s.appendDrafts(
					ctx, q, sessionID, drafts, maxSeq, nil,
				)
				if err != nil {
					return err
				}
			}
		}
		return putSessionOnlyTx(ctx, tx, session)
	})
	if err == nil && !allowed {
		s.notifySession(ctx, sessionID)
	}
	return allowed, err
}
