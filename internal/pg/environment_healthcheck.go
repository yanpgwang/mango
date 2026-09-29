package pg

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
	"github.com/yanpgwang/mango/internal/workspace"
)

const healthcheckLifetime = 120 * time.Second

// Reconciliation is request-driven. A row becomes terminal before a caller can
// observe, claim, renew, or complete an overdue check; no background timer is
// needed, and an idle database need not eagerly rewrite historical rows.
func (r *EnvironmentWorkRepository) expireHealthchecks(ctx context.Context, environmentID string) error {
	return r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		if err := r.store.authorizeEnvironmentKeyTx(ctx, tx, environmentID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `
UPDATE environment_work SET state = 'stopped', stopped_at = expires_at,
    result = '{"status":"timed_out","message":"Healthcheck deadline exceeded"}'::jsonb
WHERE environment_id = $1 AND work_type = 'healthcheck' AND state <> 'stopped' AND expires_at <= $2`, environmentID, r.store.clock.Now().UTC())
		return err
	})
}

// expireHealthcheckLocked is the final deadline fence after a potentially
// blocking row lock. Pre-operation reconciliation alone cannot provide it.
func expireHealthcheckLocked(ctx context.Context, tx pgx.Tx, work *domain.EnvironmentWork, now time.Time) (bool, error) {
	if work.Type != "healthcheck" || work.State == domain.EnvironmentWorkStopped || work.ExpiresAt == nil || now.Before(*work.ExpiresAt) {
		return false, nil
	}
	updated, err := scanEnvironmentWork(tx.QueryRow(ctx, `UPDATE environment_work SET state='stopped',stopped_at=expires_at,result='{"status":"timed_out","message":"Healthcheck deadline exceeded"}'::jsonb WHERE id=$1 RETURNING `+environmentWorkColumns, work.ID))
	if err != nil {
		return false, err
	}
	*work = updated
	return true, nil
}

func (r *EnvironmentWorkRepository) CreateHealthcheck(ctx context.Context, environmentID string) (domain.EnvironmentWork, error) {
	if err := r.authorizeEnvironment(ctx, environmentID); err != nil {
		return domain.EnvironmentWork{}, err
	}
	if scope, _ := workspace.FromContext(ctx); scope.Session != nil || scope.Environment != nil {
		return domain.EnvironmentWork{}, domain.Permission("worker credentials cannot create healthchecks")
	}
	now := r.store.clock.Now().UTC().Truncate(time.Microsecond)
	return scanEnvironmentWork(r.store.pool.QueryRow(ctx, `
INSERT INTO environment_work (id,environment_id,work_type,state,created_at,expires_at)
VALUES ($1,$2,'healthcheck','queued',$3,$4) RETURNING `+environmentWorkColumns,
		r.store.ids.NewID(domain.PrefixEnvironmentWork), environmentID, now, now.Add(healthcheckLifetime)))
}

func (r *EnvironmentWorkRepository) CompleteHealthcheck(ctx context.Context, environmentID, workID string, result domain.EnvironmentWorkResult) (domain.EnvironmentWork, error) {
	if err := r.authorizeEnvironment(ctx, environmentID); err != nil {
		return domain.EnvironmentWork{}, err
	}
	scope, _ := workspace.FromContext(ctx)
	if scope.Session == nil || scope.Session.SessionID != "" {
		return domain.EnvironmentWork{}, domain.Permission("only the healthcheck worker may report a result")
	}
	if err := r.expireHealthchecks(ctx, environmentID); err != nil {
		return domain.EnvironmentWork{}, err
	}
	var completed domain.EnvironmentWork
	var completionErr error
	err := r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		work, err := r.workForUpdate(ctx, tx, environmentID, workID)
		if err != nil {
			return err
		}
		now := r.store.clock.Now().UTC().Truncate(time.Microsecond)
		if work.Type != "healthcheck" {
			return domain.Conflict("work item is not a healthcheck")
		}
		if work.Result != nil {
			if work.StoppedAt == nil || !now.Before(work.StoppedAt.Add(30*time.Second)) {
				return domain.Precondition("result retry window has expired")
			}
			if *work.Result != result {
				return domain.Conflict("healthcheck result is already committed")
			}
			completed = work
			return nil
		}
		if expired, err := expireHealthcheckLocked(ctx, tx, &work, now); err != nil {
			return err
		} else if expired {
			completionErr = domain.Precondition("healthcheck deadline has expired")
			return nil
		}
		if work.State != domain.EnvironmentWorkActive || environmentWorkLeaseExpired(work, now) || work.ExpiresAt == nil || !now.Before(*work.ExpiresAt) {
			return domain.Precondition("healthcheck lease is not active")
		}
		body, err := json.Marshal(result)
		if err != nil {
			return err
		}
		completed, err = scanEnvironmentWork(tx.QueryRow(ctx, `
UPDATE environment_work SET state='stopped',stopped_at=$3,result=$4
WHERE environment_id=$1 AND id=$2 RETURNING `+environmentWorkColumns, environmentID, workID, now, body))
		return err
	})
	if err == nil {
		err = completionErr
	}
	return completed, err
}
