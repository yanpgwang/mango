package pg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

var _ app.WebhookRepository = (*WebhookRepository)(nil)

type WebhookRepository struct{ store *Store }

func NewWebhookRepository(store *Store) *WebhookRepository {
	return &WebhookRepository{store: store}
}

func (r *WebhookRepository) CreateWebhook(
	ctx context.Context,
	item domain.Webhook,
	maxPerWorkspace int,
) (domain.Webhook, error) {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return domain.Webhook{}, err
	}
	if item.SecretEnvelope == nil {
		return domain.Webhook{}, errors.New("webhook signing secret envelope is required")
	}
	item = normalizeWebhookTimes(item)
	err = r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		var locked int
		if err := tx.QueryRow(ctx, `
SELECT 1 FROM workspaces WHERE id = $1 FOR UPDATE`, workspaceID).Scan(&locked); errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("workspace not found")
		} else if err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `
SELECT COUNT(*) FROM webhooks WHERE workspace_id = $1`, workspaceID).Scan(&count); err != nil {
			return err
		}
		if count >= maxPerWorkspace {
			return domain.Conflict("workspace already has 100 webhooks")
		}
		envelope := item.SecretEnvelope
		_, err := tx.Exec(ctx, `
INSERT INTO webhooks (
    id, workspace_id, url, event_types, status, disabled_reason,
    secret_version, secret_algorithm, secret_key_id, secret_nonce,
    secret_ciphertext, failure_started_at, last_success_at, created_at, updated_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			item.ID, workspaceID, item.URL, item.EventTypes, item.Status, item.DisabledReason,
			envelope.Version, envelope.Algorithm, envelope.KeyID, envelope.Nonce,
			envelope.Ciphertext, item.FailureStartedAt, item.LastSuccessAt,
			item.CreatedAt, item.UpdatedAt,
		)
		return err
	})
	if isUniqueViolation(err) {
		return domain.Webhook{}, domain.Conflict("webhook already exists")
	}
	return item, err
}

func (r *WebhookRepository) GetWebhook(
	ctx context.Context,
	id string,
) (domain.Webhook, error) {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return domain.Webhook{}, err
	}
	statement := webhookSelect + ` WHERE id = $1`
	args := []any{id}
	if scoped {
		statement += ` AND workspace_id = $2`
		args = append(args, workspaceID)
	}
	item, err := scanWebhook(r.store.pool.QueryRow(ctx, statement, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Webhook{}, domain.NotFound("webhook not found")
	}
	return item, err
}

func (r *WebhookRepository) UpdateWebhook(
	ctx context.Context,
	id string,
	mutate func(domain.Webhook) (domain.Webhook, bool, error),
) (domain.Webhook, error) {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return domain.Webhook{}, err
	}
	var result domain.Webhook
	err = r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		statement := webhookSelect + ` WHERE id = $1`
		args := []any{id}
		if scoped {
			statement += ` AND workspace_id = $2`
			args = append(args, workspaceID)
		}
		current, err := scanWebhook(tx.QueryRow(ctx, statement+` FOR UPDATE`, args...))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("webhook not found")
		}
		if err != nil {
			return err
		}
		next, changed, err := mutate(current)
		if err != nil {
			return err
		}
		if !changed {
			result = current
			return nil
		}
		if next.SecretEnvelope == nil {
			return errors.New("webhook signing secret envelope is required")
		}
		next = normalizeWebhookTimes(next)
		envelope := next.SecretEnvelope
		update := `
UPDATE webhooks SET
    url = $2, event_types = $3, status = $4, disabled_reason = $5,
    secret_version = $6, secret_algorithm = $7, secret_key_id = $8,
    secret_nonce = $9, secret_ciphertext = $10,
    failure_started_at = $11, last_success_at = $12, updated_at = $13
WHERE id = $1`
		updateArgs := []any{
			id, next.URL, next.EventTypes, next.Status, next.DisabledReason,
			envelope.Version, envelope.Algorithm, envelope.KeyID, envelope.Nonce,
			envelope.Ciphertext, next.FailureStartedAt, next.LastSuccessAt, next.UpdatedAt,
		}
		if scoped {
			update += ` AND workspace_id = $14`
			updateArgs = append(updateArgs, workspaceID)
		}
		if _, err := tx.Exec(ctx, update, updateArgs...); err != nil {
			return err
		}
		if current.Status != domain.WebhookStatusDisabled &&
			next.Status == domain.WebhookStatusDisabled {
			if _, err := tx.Exec(ctx, `
UPDATE webhook_deliveries
SET state = 'failed', claimed_at = NULL, claim_id = NULL,
    last_error = COALESCE($2, 'endpoint disabled'), completed_at = $3
WHERE webhook_id = $1 AND state = 'pending'`,
				id, next.DisabledReason, next.UpdatedAt,
			); err != nil {
				return err
			}
		}
		result = next
		return nil
	})
	return result, err
}

func (r *WebhookRepository) ListWebhooks(
	ctx context.Context,
	query app.WebhookListQuery,
) (app.WebhookListPage, error) {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return app.WebhookListPage{}, err
	}
	clauses := []string{"true"}
	args := []any{}
	if scoped {
		args = append(args, workspaceID)
		clauses = append(clauses, fmt.Sprintf("workspace_id = $%d", len(args)))
	}
	if query.After != nil {
		args = append(args, query.After.CreatedAt.UTC(), query.After.ID)
		clauses = append(clauses, fmt.Sprintf(
			"(created_at, id) < ($%d, $%d)", len(args)-1, len(args),
		))
	}
	args = append(args, query.Limit+1)
	rows, err := r.store.pool.Query(ctx, webhookSelect+`
WHERE `+strings.Join(clauses, " AND ")+fmt.Sprintf(`
ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return app.WebhookListPage{}, err
	}
	defer rows.Close()
	items := make([]domain.Webhook, 0, query.Limit+1)
	for rows.Next() {
		item, err := scanWebhook(rows)
		if err != nil {
			return app.WebhookListPage{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return app.WebhookListPage{}, err
	}
	page := app.WebhookListPage{Webhooks: items}
	if len(items) > query.Limit {
		page.Webhooks = items[:query.Limit]
		page.HasNext = true
	}
	return page, nil
}

func (r *WebhookRepository) DeleteWebhook(ctx context.Context, id string) error {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return err
	}
	statement := `DELETE FROM webhooks WHERE id = $1`
	args := []any{id}
	if scoped {
		statement += ` AND workspace_id = $2`
		args = append(args, workspaceID)
	}
	tag, err := r.store.pool.Exec(ctx, statement, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.NotFound("webhook not found")
	}
	return nil
}

const webhookSelect = `
SELECT id, url, event_types, status, disabled_reason,
       secret_version, secret_algorithm, secret_key_id, secret_nonce,
       secret_ciphertext, failure_started_at, last_success_at, created_at, updated_at
FROM webhooks`

type webhookScanner interface{ Scan(...any) error }

func scanWebhook(row webhookScanner) (domain.Webhook, error) {
	var item domain.Webhook
	var envelope domain.SecretEnvelope
	if err := row.Scan(
		&item.ID, &item.URL, &item.EventTypes, &item.Status, &item.DisabledReason,
		&envelope.Version, &envelope.Algorithm, &envelope.KeyID, &envelope.Nonce,
		&envelope.Ciphertext, &item.FailureStartedAt, &item.LastSuccessAt,
		&item.CreatedAt, &item.UpdatedAt,
	); err != nil {
		return domain.Webhook{}, err
	}
	item.SecretEnvelope = &envelope
	return normalizeWebhookTimes(item), nil
}

func normalizeWebhookTimes(item domain.Webhook) domain.Webhook {
	item.CreatedAt = item.CreatedAt.UTC().Truncate(time.Microsecond)
	item.UpdatedAt = item.UpdatedAt.UTC().Truncate(time.Microsecond)
	if item.FailureStartedAt != nil {
		value := item.FailureStartedAt.UTC().Truncate(time.Microsecond)
		item.FailureStartedAt = &value
	}
	if item.LastSuccessAt != nil {
		value := item.LastSuccessAt.UTC().Truncate(time.Microsecond)
		item.LastSuccessAt = &value
	}
	return item
}
