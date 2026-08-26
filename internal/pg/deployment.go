package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

type DeploymentRepository struct{ store *Store }

func NewDeploymentRepository(store *Store) *DeploymentRepository {
	return &DeploymentRepository{store: store}
}

func (r *DeploymentRepository) Create(
	ctx context.Context,
	item domain.Deployment,
) (domain.Deployment, error) {
	workspaceID, err := r.store.workspaceForWrite(ctx)
	if err != nil {
		return domain.Deployment{}, err
	}
	item = normalizeDeploymentTimes(item)
	body, err := json.Marshal(item)
	if err != nil {
		return domain.Deployment{}, err
	}
	_, err = r.store.pool.Exec(ctx, `
INSERT INTO deployments (
    id, workspace_id, agent_id, agent_version, environment_id, status, body,
    next_run_at, created_at, updated_at, archived_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		item.ID, workspaceID, item.AgentID, item.AgentVersion, item.EnvironmentID,
		item.Status, body, deploymentNextRun(item), item.CreatedAt,
		item.UpdatedAt, item.ArchivedAt,
	)
	if isForeignKeyViolation(err) {
		return domain.Deployment{}, domain.Validation(
			"deployment references a missing Agent version or Environment",
		)
	}
	return item, err
}

func (r *DeploymentRepository) Get(
	ctx context.Context,
	id string,
) (domain.Deployment, error) {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return domain.Deployment{}, err
	}
	query := `SELECT body FROM deployments WHERE id = $1`
	args := []any{id}
	if scoped {
		query += ` AND workspace_id = $2`
		args = append(args, workspaceID)
	}
	var body []byte
	err = r.store.pool.QueryRow(ctx, query, args...).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Deployment{}, domain.NotFound("deployment not found")
	}
	if err != nil {
		return domain.Deployment{}, err
	}
	return decodeDeployment(id, body)
}

func (r *DeploymentRepository) Update(
	ctx context.Context,
	id string,
	mutate func(domain.Deployment) (domain.Deployment, bool, error),
) (domain.Deployment, error) {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return domain.Deployment{}, err
	}
	var result domain.Deployment
	err = r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		var body []byte
		query := `SELECT body FROM deployments WHERE id = $1`
		args := []any{id}
		if scoped {
			query += ` AND workspace_id = $2`
			args = append(args, workspaceID)
		}
		if err := tx.QueryRow(ctx, query+` FOR UPDATE`, args...).Scan(&body); errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("deployment not found")
		} else if err != nil {
			return err
		}
		current, err := decodeDeployment(id, body)
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
		next = normalizeDeploymentTimes(next)
		nextBody, err := json.Marshal(next)
		if err != nil {
			return err
		}
		updateQuery := `
UPDATE deployments
SET agent_id = $2,
    agent_version = $3,
    environment_id = $4,
    status = $5,
    body = $6,
    schedule_claimed_at = CASE
        WHEN next_run_at IS NOT DISTINCT FROM $7 THEN schedule_claimed_at
        ELSE NULL
    END,
    schedule_claim_token = CASE
        WHEN next_run_at IS NOT DISTINCT FROM $7 THEN schedule_claim_token
        ELSE NULL
    END,
    next_run_at = $7,
    updated_at = $8,
    archived_at = $9
WHERE id = $1`
		updateArgs := []any{id, next.AgentID, next.AgentVersion, next.EnvironmentID,
			next.Status, nextBody, deploymentNextRun(next), next.UpdatedAt, next.ArchivedAt}
		if scoped {
			updateQuery += ` AND workspace_id = $10`
			updateArgs = append(updateArgs, workspaceID)
		}
		_, err = tx.Exec(ctx, updateQuery, updateArgs...)
		if isForeignKeyViolation(err) {
			return domain.Validation(
				"deployment references a missing Agent version or Environment",
			)
		}
		result = next
		return err
	})
	return result, err
}

func (r *DeploymentRepository) List(
	ctx context.Context,
	query app.DeploymentListQuery,
) (app.DeploymentListPage, error) {
	clauses := []string{}
	args := []any{}
	add := func(format string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(format, len(args)))
	}
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return app.DeploymentListPage{}, err
	}
	if scoped {
		add(`workspace_id = $%d`, workspaceID)
	}
	if !query.IncludeArchived {
		clauses = append(clauses, `archived_at IS NULL`)
	}
	if query.AgentID != "" {
		add(`agent_id = $%d`, query.AgentID)
	}
	if query.Status != "" {
		add(`status = $%d`, query.Status)
	}
	if query.CreatedAtGte != nil {
		add(`created_at >= $%d`, *query.CreatedAtGte)
	}
	if query.CreatedAtLte != nil {
		add(`created_at <= $%d`, *query.CreatedAtLte)
	}
	if query.Boundary != nil {
		args = append(args, query.Boundary.CreatedAt, query.Boundary.ID)
		clauses = append(clauses, fmt.Sprintf(
			`(created_at < $%d OR (created_at = $%d AND id < $%d))`,
			len(args)-1, len(args)-1, len(args),
		))
	}
	args = append(args, query.Limit+1)
	statement := `SELECT id, body FROM deployments`
	if len(clauses) > 0 {
		statement += ` WHERE ` + strings.Join(clauses, ` AND `)
	}
	statement += fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args))
	rows, err := r.store.pool.Query(ctx, statement, args...)
	if err != nil {
		return app.DeploymentListPage{}, err
	}
	defer rows.Close()
	items := make([]domain.Deployment, 0, query.Limit+1)
	for rows.Next() {
		var id string
		var body []byte
		if err := rows.Scan(&id, &body); err != nil {
			return app.DeploymentListPage{}, err
		}
		item, err := decodeDeployment(id, body)
		if err != nil {
			return app.DeploymentListPage{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return app.DeploymentListPage{}, err
	}
	page := app.DeploymentListPage{Deployments: items}
	if len(items) > query.Limit {
		page.HasMore = true
		page.Deployments = items[:query.Limit]
	}
	return page, nil
}

func (r *DeploymentRepository) CreateRun(
	ctx context.Context,
	run domain.DeploymentRun,
) (domain.DeploymentRun, error) {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return domain.DeploymentRun{}, err
	}
	run = normalizeDeploymentRunTimes(run)
	body, err := json.Marshal(run)
	if err != nil {
		return domain.DeploymentRun{}, err
	}
	err = r.store.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		query := `SELECT workspace_id FROM deployments WHERE id = $1`
		args := []any{run.DeploymentID}
		if scoped {
			query += ` AND workspace_id = $2`
			args = append(args, workspaceID)
		}
		var ownerWorkspaceID string
		if err := tx.QueryRow(ctx, query, args...).Scan(&ownerWorkspaceID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("deployment not found")
		} else if err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
INSERT INTO deployment_runs (
    id, deployment_id, session_id, error_type, trigger_type,
    scheduled_at, body, created_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			run.ID, run.DeploymentID, run.SessionID, nullString(run.ErrorType),
			run.TriggerType, run.ScheduledAt, body, run.CreatedAt,
		)
		if err != nil {
			return err
		}
		if run.TriggerType != domain.DeploymentTriggerSchedule || run.ErrorType == "" {
			return nil
		}
		return r.store.enqueueWebhookEvent(
			ctx, q, ownerWorkspaceID, domain.WebhookEventDeploymentRunFailed,
			run.ID, run.CreatedAt, nil,
		)
	})
	if isUniqueViolation(err) {
		return domain.DeploymentRun{}, domain.Conflict("deployment schedule occurrence already ran")
	}
	if isForeignKeyViolation(err) {
		return domain.DeploymentRun{}, domain.NotFound("deployment not found")
	}
	return run, err
}

func (r *DeploymentRepository) GetRun(
	ctx context.Context,
	id string,
) (domain.DeploymentRun, error) {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return domain.DeploymentRun{}, err
	}
	query := `SELECT dr.body FROM deployment_runs dr JOIN deployments d ON d.id = dr.deployment_id WHERE dr.id = $1`
	args := []any{id}
	if scoped {
		query += ` AND d.workspace_id = $2`
		args = append(args, workspaceID)
	}
	return r.getRun(ctx, query, args...)
}

func (r *DeploymentRepository) GetScheduledRun(
	ctx context.Context,
	deploymentID string,
	scheduledAt time.Time,
) (domain.DeploymentRun, error) {
	if _, err := r.Get(ctx, deploymentID); err != nil {
		return domain.DeploymentRun{}, err
	}
	return r.getRun(ctx, `
SELECT body FROM deployment_runs
WHERE deployment_id = $1 AND scheduled_at = $2`, deploymentID, scheduledAt.UTC())
}

func (r *DeploymentRepository) getRun(
	ctx context.Context,
	statement string,
	args ...any,
) (domain.DeploymentRun, error) {
	var body []byte
	err := r.store.pool.QueryRow(ctx, statement, args...).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.DeploymentRun{}, domain.NotFound("deployment run not found")
	}
	if err != nil {
		return domain.DeploymentRun{}, err
	}
	var run domain.DeploymentRun
	if err := json.Unmarshal(body, &run); err != nil {
		return domain.DeploymentRun{}, fmt.Errorf("pg: decode deployment run: %w", err)
	}
	return run, nil
}

func (r *DeploymentRepository) ListRuns(
	ctx context.Context,
	query app.DeploymentRunListQuery,
) (app.DeploymentRunListPage, error) {
	clauses := []string{}
	args := []any{}
	add := func(format string, value any) {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf(format, len(args)))
	}
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return app.DeploymentRunListPage{}, err
	}
	if scoped {
		add(`d.workspace_id = $%d`, workspaceID)
	}
	for _, bound := range []struct {
		value *time.Time
		op    string
	}{
		{query.CreatedAtGt, `>`}, {query.CreatedAtGte, `>=`},
		{query.CreatedAtLt, `<`}, {query.CreatedAtLte, `<=`},
	} {
		if bound.value != nil {
			add(`deployment_runs.created_at `+bound.op+` $%d`, *bound.value)
		}
	}
	if query.DeploymentID != nil {
		add(`deployment_runs.deployment_id = $%d`, *query.DeploymentID)
	}
	if query.HasError != nil {
		if *query.HasError {
			clauses = append(clauses, `deployment_runs.error_type IS NOT NULL`)
		} else {
			clauses = append(clauses, `deployment_runs.error_type IS NULL`)
		}
	}
	if query.TriggerType != "" {
		add(`deployment_runs.trigger_type = $%d`, query.TriggerType)
	}
	if query.Boundary != nil {
		args = append(args, query.Boundary.CreatedAt, query.Boundary.ID)
		clauses = append(clauses, fmt.Sprintf(
			`(deployment_runs.created_at < $%d OR (deployment_runs.created_at = $%d AND deployment_runs.id < $%d))`,
			len(args)-1, len(args)-1, len(args),
		))
	}
	args = append(args, query.Limit+1)
	statement := `SELECT deployment_runs.body FROM deployment_runs JOIN deployments d ON d.id = deployment_runs.deployment_id`
	if len(clauses) > 0 {
		statement += ` WHERE ` + strings.Join(clauses, ` AND `)
	}
	statement += fmt.Sprintf(` ORDER BY deployment_runs.created_at DESC, deployment_runs.id DESC LIMIT $%d`, len(args))
	rows, err := r.store.pool.Query(ctx, statement, args...)
	if err != nil {
		return app.DeploymentRunListPage{}, err
	}
	defer rows.Close()
	runs := make([]domain.DeploymentRun, 0, query.Limit+1)
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			return app.DeploymentRunListPage{}, err
		}
		var run domain.DeploymentRun
		if err := json.Unmarshal(body, &run); err != nil {
			return app.DeploymentRunListPage{}, fmt.Errorf("pg: decode deployment run: %w", err)
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		return app.DeploymentRunListPage{}, err
	}
	page := app.DeploymentRunListPage{Runs: runs}
	if len(runs) > query.Limit {
		page.HasMore = true
		page.Runs = runs[:query.Limit]
	}
	return page, nil
}

func (r *DeploymentRepository) ClaimDue(
	ctx context.Context,
	now time.Time,
	staleBefore time.Time,
	claimToken string,
	limit int,
) ([]app.DeploymentScheduleClaim, error) {
	if limit <= 0 {
		limit = 20
	}
	if claimToken == "" {
		return nil, errors.New("pg: deployment schedule claim token is required")
	}
	rows, err := r.store.pool.Query(ctx, `
WITH due AS (
    SELECT id
    FROM deployments
    WHERE archived_at IS NULL
      AND status = 'active'
      AND next_run_at <= $1
      AND (schedule_claimed_at IS NULL OR schedule_claimed_at < $2)
    ORDER BY next_run_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT $3
)
UPDATE deployments AS deployment
SET schedule_claimed_at = $1,
    schedule_claim_token = $4
FROM due
WHERE deployment.id = due.id
RETURNING deployment.workspace_id, deployment.id, deployment.next_run_at`, now.UTC(), staleBefore.UTC(), limit, claimToken)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var claims []app.DeploymentScheduleClaim
	for rows.Next() {
		var claim app.DeploymentScheduleClaim
		if err := rows.Scan(&claim.WorkspaceID, &claim.DeploymentID, &claim.ScheduledAt); err != nil {
			return nil, err
		}
		claim.ScheduledAt = claim.ScheduledAt.UTC()
		claim.Token = claimToken
		claims = append(claims, claim)
	}
	return claims, rows.Err()
}

func (r *DeploymentRepository) RenewScheduleClaim(
	ctx context.Context,
	id string,
	scheduledAt time.Time,
	claimToken string,
	claimedAt time.Time,
) error {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return err
	}
	query := `
UPDATE deployments
SET schedule_claimed_at = $4
WHERE id = $1
  AND next_run_at = $2
  AND schedule_claim_token = $3
  AND archived_at IS NULL
  AND status = 'active'`
	args := []any{id, scheduledAt.UTC(), claimToken, claimedAt.UTC()}
	if scoped {
		query += ` AND workspace_id = $5`
		args = append(args, workspaceID)
	}
	result, err := r.store.pool.Exec(ctx, query, args...)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return domain.Conflict("deployment schedule claim is no longer owned")
	}
	return nil
}

func (r *DeploymentRepository) CompleteSchedule(
	ctx context.Context,
	id string,
	scheduledAt time.Time,
	lastRunAt time.Time,
	upcoming []time.Time,
) error {
	workspaceID, scoped, err := r.store.workspaceForRead(ctx)
	if err != nil {
		return err
	}
	return r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		var body []byte
		var nextRun pgtype.Timestamptz
		query := `
SELECT body, next_run_at
FROM deployments
WHERE id = $1`
		args := []any{id}
		if scoped {
			query += ` AND workspace_id = $2`
			args = append(args, workspaceID)
		}
		if err := tx.QueryRow(ctx, query+` FOR UPDATE`, args...).Scan(&body, &nextRun); errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("deployment not found")
		} else if err != nil {
			return err
		}
		if !nextRun.Valid || !nextRun.Time.UTC().Equal(scheduledAt.UTC()) {
			return nil
		}
		item, err := decodeDeployment(id, body)
		if err != nil {
			return err
		}
		if item.Schedule == nil {
			return nil
		}
		last := lastRunAt.UTC().Truncate(time.Microsecond)
		item.Schedule.LastRunAt = &last
		item.Schedule.UpcomingRunsAt = append([]time.Time(nil), upcoming...)
		item.ScheduleClaimedAt = nil
		// A user update may commit after the Run was created but before the
		// scheduler advances its occurrence. Never move updated_at backwards
		// when merging that schedule bookkeeping into the latest body.
		if item.UpdatedAt.Before(last) {
			item.UpdatedAt = last
		}
		item = normalizeDeploymentTimes(item)
		next := deploymentNextRun(item)
		updatedBody, err := json.Marshal(item)
		if err != nil {
			return err
		}
		updateQuery := `
UPDATE deployments
SET body = $2, next_run_at = $3, schedule_claimed_at = NULL,
    schedule_claim_token = NULL, updated_at = $4
WHERE id = $1`
		updateArgs := []any{id, updatedBody, next, item.UpdatedAt}
		if scoped {
			updateQuery += ` AND workspace_id = $5`
			updateArgs = append(updateArgs, workspaceID)
		}
		_, err = tx.Exec(ctx, updateQuery, updateArgs...)
		return err
	})
}

func deploymentNextRun(item domain.Deployment) any {
	if item.ArchivedAt != nil || item.Status != domain.DeploymentStatusActive ||
		item.Schedule == nil || len(item.Schedule.UpcomingRunsAt) == 0 {
		return nil
	}
	return item.Schedule.UpcomingRunsAt[0].UTC()
}

func normalizeDeploymentTimes(item domain.Deployment) domain.Deployment {
	item.CreatedAt = item.CreatedAt.UTC().Truncate(time.Microsecond)
	item.UpdatedAt = item.UpdatedAt.UTC().Truncate(time.Microsecond)
	if item.ArchivedAt != nil {
		value := item.ArchivedAt.UTC().Truncate(time.Microsecond)
		item.ArchivedAt = &value
	}
	if item.ScheduleClaimedAt != nil {
		value := item.ScheduleClaimedAt.UTC().Truncate(time.Microsecond)
		item.ScheduleClaimedAt = &value
	}
	if item.Schedule != nil {
		if item.Schedule.LastRunAt != nil {
			value := item.Schedule.LastRunAt.UTC().Truncate(time.Microsecond)
			item.Schedule.LastRunAt = &value
		}
		for index := range item.Schedule.UpcomingRunsAt {
			item.Schedule.UpcomingRunsAt[index] = item.Schedule.UpcomingRunsAt[index].UTC().Truncate(time.Microsecond)
		}
	}
	return item
}

func normalizeDeploymentRunTimes(run domain.DeploymentRun) domain.DeploymentRun {
	run.CreatedAt = run.CreatedAt.UTC().Truncate(time.Microsecond)
	if run.ScheduledAt != nil {
		value := run.ScheduledAt.UTC().Truncate(time.Microsecond)
		run.ScheduledAt = &value
	}
	return run
}

func decodeDeployment(id string, body []byte) (domain.Deployment, error) {
	var item domain.Deployment
	if err := json.Unmarshal(body, &item); err != nil {
		return domain.Deployment{}, fmt.Errorf("pg: decode deployment %s: %w", id, err)
	}
	return item, nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
