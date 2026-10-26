package pg

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

const WorkflowRunDeliveryLimit = 8
const WorkflowRunDeliveryLease = 30 * time.Second

// ClaimWorkflowRunWakeups reserves due rows before IO. No Session lock is taken
// after an outbox lock: admission/completion retain their Session-first order.
// The relay claims one at a time so a sequential batch cannot outlive its leases.
// Exhaustion quarantines the durable row; a newer Run version re-arms it.
func (s *Store) ClaimWorkflowRunWakeups(ctx context.Context, limit int) ([]WorkflowRunWakeup, error) {
	if limit < 1 || limit > 100 {
		limit = 100
	}
	id, _, err := s.workspaceForRead(ctx)
	if err != nil {
		return nil, err
	}
	token := s.ids.NewID("wrlease_")
	rows, err := s.pool.Query(ctx, `WITH due AS (
 SELECT wake.run_id FROM workflow_run_outbox wake
 JOIN sessions session ON session.id=wake.session_id
 WHERE ($1::text='' OR session.workspace_id=$1) AND session.deleting_at IS NULL
 AND wake.delivery_attempts<8 AND wake.available_at<=statement_timestamp()
 ORDER BY wake.available_at,wake.enqueued_at,wake.run_id
 LIMIT $2 FOR UPDATE OF wake SKIP LOCKED
 ) UPDATE workflow_run_outbox wake SET delivery_attempts=wake.delivery_attempts+1,lease_token=$3,available_at=statement_timestamp()+($4::bigint*interval '1 microsecond')
 FROM due WHERE wake.run_id=due.run_id
 RETURNING wake.session_id,wake.run_id,wake.version,wake.lease_token,wake.delivery_attempts`, id, limit, token, WorkflowRunDeliveryLease.Microseconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []WorkflowRunWakeup{}
	for rows.Next() {
		var wake WorkflowRunWakeup
		if err = rows.Scan(&wake.SessionID, &wake.RunID, &wake.Version, &wake.LeaseToken, &wake.DeliveryAttempt); err != nil {
			return nil, err
		}
		out = append(out, wake)
	}
	return out, rows.Err()
}

// DeferWorkflowRunWakeup never stores transport error text. A lost claim/failure
// ACK is safe: the lease expires, but only the current token/version may defer.
func (s *Store) DeferWorkflowRunWakeup(ctx context.Context, wake WorkflowRunWakeup) error {
	delay := time.Second
	for n := 1; n < wake.DeliveryAttempt && delay < time.Minute; n++ {
		delay = min(2*delay, time.Minute)
	}
	return s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		if _, err := s.lockWorkflowSession(ctx, q, wake.SessionID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE workflow_run_outbox SET available_at=statement_timestamp()+($5::bigint*interval '1 microsecond'),lease_token=''
 WHERE session_id=$1 AND run_id=$2 AND version=$3 AND lease_token=$4 AND $4<>''`, wake.SessionID, wake.RunID, wake.Version, wake.LeaseToken, delay.Microseconds())
		return err
	})
}

// PrepareWorkflowRunStart reserves the sole automatic start before remote IO,
// under Session->Run->outbox locks. Once attempted, all subsequent deliveries
// may only signal; history absence can never turn a retry into a fresh start.
// A lost prepare ACK (or crash before RPC) deliberately requires reconciliation.
func (s *Store) PrepareWorkflowRunStart(ctx context.Context, wake WorkflowRunWakeup) (bool, error) {
	allowed := false
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		if _, err := s.lockWorkflowSession(ctx, q, wake.SessionID); err != nil {
			return err
		}
		var attempted bool
		if err := tx.QueryRow(ctx, `SELECT coordinator_start_attempted FROM workflow_runs WHERE session_id=$1 AND id=$2 FOR UPDATE`, wake.SessionID, wake.RunID).Scan(&attempted); err != nil {
			return err
		}
		var token string
		err := tx.QueryRow(ctx, `SELECT lease_token FROM workflow_run_outbox
 WHERE session_id=$1 AND run_id=$2 AND version=$3 AND lease_token=$4 AND $4<>'' AND available_at>statement_timestamp()
 FOR UPDATE`, wake.SessionID, wake.RunID, wake.Version, wake.LeaseToken).Scan(&token)
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Conflict("Workflow wake lease requires reconciliation")
		}
		if err != nil {
			return err
		}
		if attempted {
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE workflow_runs SET coordinator_start_attempted=true WHERE id=$1`, wake.RunID)
		allowed = err == nil
		return err
	})
	return allowed, err
}
