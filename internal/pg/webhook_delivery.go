package pg

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

var _ app.WebhookDeliveryRepository = (*WebhookRepository)(nil)

func (r *WebhookRepository) CleanupWebhookEvents(
	ctx context.Context,
	before time.Time,
	limit int,
) (int64, error) {
	tag, err := r.store.pool.Exec(ctx, `
DELETE FROM webhook_events
WHERE id IN (
    SELECT event.id
    FROM webhook_events AS event
    WHERE event.created_at < $1
      AND NOT EXISTS (
          SELECT 1 FROM webhook_deliveries AS delivery
          WHERE delivery.event_id = event.id AND delivery.state = 'pending'
      )
    ORDER BY event.created_at, event.id
    LIMIT $2
)`, before.UTC(), limit)
	return tag.RowsAffected(), err
}

func (r *WebhookRepository) ClaimWebhookDeliveries(
	ctx context.Context,
	now time.Time,
	staleBefore time.Time,
	claimID string,
	limit int,
) ([]domain.WebhookDelivery, error) {
	rows, err := r.store.pool.Query(ctx, `
WITH candidates AS (
    SELECT delivery.webhook_id, delivery.event_id
    FROM webhook_deliveries AS delivery
    JOIN webhooks AS endpoint ON endpoint.id = delivery.webhook_id
    WHERE delivery.state = 'pending'
      AND delivery.next_attempt_at <= $1
      AND (delivery.claimed_at IS NULL OR delivery.claimed_at < $2)
      AND endpoint.status = 'enabled'
    ORDER BY delivery.next_attempt_at, delivery.created_at,
             delivery.webhook_id, delivery.event_id
    FOR UPDATE OF delivery SKIP LOCKED
    LIMIT $4
), claimed AS (
    UPDATE webhook_deliveries AS delivery
    SET claimed_at = $1, claim_id = $3
    FROM candidates
    WHERE delivery.webhook_id = candidates.webhook_id
      AND delivery.event_id = candidates.event_id
    RETURNING delivery.webhook_id, delivery.event_id,
              delivery.attempt_count, delivery.claim_id, delivery.created_at
)
SELECT claimed.webhook_id, claimed.event_id, endpoint.url,
       endpoint.secret_version, endpoint.secret_algorithm,
       endpoint.secret_key_id, endpoint.secret_nonce, endpoint.secret_ciphertext,
       event.payload, claimed.attempt_count, claimed.claim_id, claimed.created_at
FROM claimed
JOIN webhooks AS endpoint ON endpoint.id = claimed.webhook_id
JOIN webhook_events AS event ON event.id = claimed.event_id`,
		now.UTC(), staleBefore.UTC(), claimID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	deliveries := make([]domain.WebhookDelivery, 0, limit)
	for rows.Next() {
		var delivery domain.WebhookDelivery
		if err := rows.Scan(
			&delivery.WebhookID, &delivery.EventID, &delivery.URL,
			&delivery.SecretEnvelope.Version, &delivery.SecretEnvelope.Algorithm,
			&delivery.SecretEnvelope.KeyID, &delivery.SecretEnvelope.Nonce,
			&delivery.SecretEnvelope.Ciphertext, &delivery.Payload,
			&delivery.AttemptCount, &delivery.ClaimID, &delivery.CreatedAt,
		); err != nil {
			return nil, err
		}
		delivery.CreatedAt = delivery.CreatedAt.UTC().Truncate(time.Microsecond)
		deliveries = append(deliveries, delivery)
	}
	return deliveries, rows.Err()
}

func (r *WebhookRepository) CompleteWebhookDelivery(
	ctx context.Context,
	result app.WebhookDeliveryResult,
) (bool, error) {
	completed := false
	err := r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		var attemptCount int
		err := tx.QueryRow(ctx, `
SELECT attempt_count
FROM webhook_deliveries
WHERE webhook_id = $1 AND event_id = $2 AND state = 'pending' AND claim_id = $3
FOR UPDATE`, result.WebhookID, result.EventID, result.ClaimID).Scan(&attemptCount)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		attemptCount++
		if result.Succeeded {
			if _, err := tx.Exec(ctx, `
UPDATE webhook_deliveries
SET state = 'succeeded', attempt_count = $4, claimed_at = NULL,
    claim_id = NULL, last_attempt_at = $5, delivered_at = $5,
    response_status = $6, last_error = NULL
WHERE webhook_id = $1 AND event_id = $2 AND claim_id = $3`,
				result.WebhookID, result.EventID, result.ClaimID,
				attemptCount, result.AttemptedAt.UTC(), result.ResponseStatus,
			); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `
UPDATE webhooks
SET failure_started_at = NULL, last_success_at = $2
WHERE id = $1 AND status = 'enabled'`, result.WebhookID, result.AttemptedAt.UTC())
			if err != nil {
				return err
			}
			completed = true
			return nil
		}

		state := "failed"
		nextAttemptAt := result.AttemptedAt.UTC()
		if result.DisableReason == nil && result.NextAttemptAt != nil &&
			attemptCount < app.MaxWebhookDeliveryAttempts {
			state = "pending"
			nextAttemptAt = result.NextAttemptAt.UTC()
		}
		if _, err := tx.Exec(ctx, `
UPDATE webhook_deliveries
SET state = $4, attempt_count = $5, next_attempt_at = $6,
    claimed_at = NULL, claim_id = NULL, last_attempt_at = $7,
    response_status = $8, last_error = $9
WHERE webhook_id = $1 AND event_id = $2 AND claim_id = $3`,
			result.WebhookID, result.EventID, result.ClaimID, state,
			attemptCount, nextAttemptAt, result.AttemptedAt.UTC(),
			result.ResponseStatus, result.Error,
		); err != nil {
			return err
		}
		if result.DisableReason != nil {
			if _, err := tx.Exec(ctx, `
UPDATE webhooks
SET status = 'disabled', disabled_reason = $2,
    failure_started_at = COALESCE(failure_started_at, $3), updated_at = $3
WHERE id = $1`, result.WebhookID, *result.DisableReason, result.AttemptedAt.UTC()); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
UPDATE webhook_deliveries
SET state = 'failed', claimed_at = NULL, claim_id = NULL,
    last_error = $2
WHERE webhook_id = $1 AND state = 'pending'`,
				result.WebhookID, *result.DisableReason,
			); err != nil {
				return err
			}
		} else {
			if _, err := tx.Exec(ctx, `
UPDATE webhooks
SET failure_started_at = COALESCE(failure_started_at, $2)
WHERE id = $1 AND status = 'enabled'`, result.WebhookID, result.AttemptedAt.UTC()); err != nil {
				return err
			}
		}
		completed = true
		return nil
	})
	return completed, err
}
