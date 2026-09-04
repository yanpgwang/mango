package pg

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
	"github.com/yanpgwang/mango/internal/workspace"
)

const environmentWorkColumns = `
id, environment_id, session_id, state, metadata, created_at,
acknowledged_at, started_at, latest_heartbeat_at, ttl_seconds,
stop_requested_at, stopped_at`

const environmentWorkTargetColumns = `
work.id, work.environment_id, work.session_id, work.state, work.metadata, work.created_at,
work.acknowledged_at, work.started_at, work.latest_heartbeat_at, work.ttl_seconds,
work.stop_requested_at, work.stopped_at`

type EnvironmentWorkRepository struct{ store *Store }

func NewEnvironmentWorkRepository(store *Store) *EnvironmentWorkRepository {
	return &EnvironmentWorkRepository{store: store}
}

func (r *EnvironmentWorkRepository) authorizeEnvironment(ctx context.Context, id string) error {
	_, err := NewEnvironmentRepository(r.store).Get(ctx, id)
	return err
}

type workScanner interface{ Scan(...any) error }

func newEnvironmentWorkSecret() (string, []byte, error) {
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", nil, fmt.Errorf("pg: generate Environment Work secret: %w", err)
	}
	sessionsToken := "sess_mango_" + base64.RawURLEncoding.EncodeToString(random[:])
	payload, err := json.Marshal(struct {
		SessionsToken string `json:"sessions_token"`
	}{SessionsToken: sessionsToken})
	if err != nil {
		return "", nil, fmt.Errorf("pg: encode Environment Work secret: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(payload), hashSessionsToken(sessionsToken), nil
}

func hashSessionsToken(token string) []byte {
	digest := sha256.Sum256([]byte(token))
	return digest[:]
}

// AuthenticateSessionToken resolves the per-Work bearer carried inside the
// Poll secret. A reclaim rotates the stored digest, invalidating the former
// owner's credential without persisting the raw token or payload.
func (s *Store) AuthenticateSessionToken(
	ctx context.Context,
	token string,
) (string, workspace.SessionScope, error) {
	if token == "" {
		return "", workspace.SessionScope{}, workspace.ErrInvalidSessionToken
	}
	digest := hashSessionsToken(token)
	scope := workspace.SessionScope{
		CredentialDigest: append([]byte(nil), digest...),
		Skills:           map[workspace.SkillVersion]struct{}{},
		Files:            map[string]struct{}{},
	}
	var workspaceID string
	err := s.pool.QueryRow(ctx, `
SELECT environment.workspace_id, work.environment_id, work.id, work.session_id
FROM environment_work AS work
JOIN environments AS environment ON environment.id = work.environment_id
WHERE work.sessions_token_hash = $1
  AND (
      (work.state = 'starting' AND
       work.acknowledged_at >= $2::timestamptz - make_interval(secs => work.ttl_seconds)) OR
      (work.state = 'active' AND
       work.latest_heartbeat_at >= $2::timestamptz - make_interval(secs => work.ttl_seconds)) OR
      (work.state = 'stopping' AND
       work.stop_requested_at >= $2::timestamptz - make_interval(secs => work.ttl_seconds))
  )`, digest, s.clock.Now().UTC()).Scan(
		&workspaceID, &scope.EnvironmentID, &scope.WorkID, &scope.SessionID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", workspace.SessionScope{}, workspace.ErrInvalidSessionToken
	}
	if err != nil {
		return "", workspace.SessionScope{}, fmt.Errorf("pg: authenticate session token: %w", err)
	}
	rows, err := s.pool.Query(ctx, `
SELECT skill_id, skill_version
FROM session_skill_versions
WHERE session_id = $1`, scope.SessionID)
	if err != nil {
		return "", workspace.SessionScope{}, fmt.Errorf("pg: load session token skills: %w", err)
	}
	for rows.Next() {
		var skill workspace.SkillVersion
		if err := rows.Scan(&skill.ID, &skill.Version); err != nil {
			rows.Close()
			return "", workspace.SessionScope{}, fmt.Errorf("pg: scan session token skill: %w", err)
		}
		scope.Skills[skill] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", workspace.SessionScope{}, fmt.Errorf("pg: scan session token skills: %w", err)
	}
	rows.Close()
	fileRows, err := s.pool.Query(ctx, `
SELECT file_id
FROM session_resources
WHERE session_id = $1 AND state = 'active'`, scope.SessionID)
	if err != nil {
		return "", workspace.SessionScope{}, fmt.Errorf("pg: load session token files: %w", err)
	}
	defer fileRows.Close()
	for fileRows.Next() {
		var fileID string
		if err := fileRows.Scan(&fileID); err != nil {
			return "", workspace.SessionScope{}, fmt.Errorf("pg: scan session token file: %w", err)
		}
		scope.Files[fileID] = struct{}{}
	}
	if err := fileRows.Err(); err != nil {
		return "", workspace.SessionScope{}, fmt.Errorf("pg: scan session token files: %w", err)
	}
	return workspaceID, scope, nil
}

// ValidateSessionScope rechecks an established Session stream against the
// current lease. Authentication alone is insufficient for a long-lived SSE
// request because the Work may stop, expire, or be reclaimed after headers are
// sent.
func (s *Store) ValidateSessionScope(ctx context.Context, scope workspace.SessionScope) error {
	if scope.EnvironmentID == "" || scope.WorkID == "" || scope.SessionID == "" ||
		len(scope.CredentialDigest) == 0 {
		return domain.Precondition("work lease is no longer owned")
	}
	var found bool
	err := s.pool.QueryRow(ctx, `
SELECT true
FROM environment_work
WHERE id = $1
  AND environment_id = $2
  AND session_id = $3
  AND sessions_token_hash = $4
  AND (
      (state = 'starting' AND
       acknowledged_at >= $5::timestamptz - make_interval(secs => ttl_seconds)) OR
      (state = 'active' AND
       latest_heartbeat_at >= $5::timestamptz - make_interval(secs => ttl_seconds)) OR
      (state = 'stopping' AND
       stop_requested_at >= $5::timestamptz - make_interval(secs => ttl_seconds))
  )`, scope.WorkID, scope.EnvironmentID, scope.SessionID, scope.CredentialDigest,
		s.clock.Now().UTC()).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.Precondition("work lease is no longer owned")
	}
	if err != nil {
		return fmt.Errorf("pg: validate session scope: %w", err)
	}
	if !found {
		return domain.Precondition("work lease is no longer owned")
	}
	return nil
}

func scanEnvironmentWork(row workScanner) (domain.EnvironmentWork, error) {
	var (
		work       domain.EnvironmentWork
		state      string
		metadata   []byte
		ack, start *time.Time
		heartbeat  *time.Time
		requested  *time.Time
		stopped    *time.Time
	)
	err := row.Scan(
		&work.ID, &work.EnvironmentID, &work.SessionID, &state, &metadata,
		&work.CreatedAt, &ack, &start, &heartbeat, &work.TTLSeconds,
		&requested, &stopped,
	)
	if err != nil {
		return domain.EnvironmentWork{}, err
	}
	if err := json.Unmarshal(metadata, &work.Metadata); err != nil {
		return domain.EnvironmentWork{}, err
	}
	work.State = domain.EnvironmentWorkState(state)
	work.CreatedAt = work.CreatedAt.UTC()
	work.AcknowledgedAt = utcTimePtr(ack)
	work.StartedAt = utcTimePtr(start)
	work.LatestHeartbeatAt = utcTimePtr(heartbeat)
	work.StopRequestedAt = utcTimePtr(requested)
	work.StoppedAt = utcTimePtr(stopped)
	return work, nil
}

func (r *EnvironmentWorkRepository) GetWork(
	ctx context.Context,
	environmentID, workID string,
) (domain.EnvironmentWork, error) {
	if err := r.authorizeEnvironment(ctx, environmentID); err != nil {
		return domain.EnvironmentWork{}, err
	}
	work, err := scanEnvironmentWork(r.store.pool.QueryRow(ctx,
		`SELECT `+environmentWorkColumns+` FROM environment_work
WHERE environment_id = $1 AND id = $2`, environmentID, workID))
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.EnvironmentWork{}, domain.NotFound("work item not found")
	}
	return work, err
}

func (r *EnvironmentWorkRepository) UpdateWorkMetadata(
	ctx context.Context,
	environmentID, workID string,
	patch map[string]*string,
) (domain.EnvironmentWork, error) {
	if err := r.authorizeEnvironment(ctx, environmentID); err != nil {
		return domain.EnvironmentWork{}, err
	}
	var result domain.EnvironmentWork
	err := r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		current, err := scanEnvironmentWork(tx.QueryRow(ctx,
			`SELECT `+environmentWorkColumns+` FROM environment_work
WHERE environment_id = $1 AND id = $2 FOR UPDATE`, environmentID, workID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("work item not found")
		}
		if err != nil {
			return err
		}
		if current.Metadata == nil {
			current.Metadata = map[string]string{}
		}
		for key, value := range patch {
			if value == nil {
				delete(current.Metadata, key)
			} else {
				current.Metadata[key] = *value
			}
		}
		body, err := json.Marshal(current.Metadata)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE environment_work SET metadata = $3
WHERE environment_id = $1 AND id = $2`, environmentID, workID, body); err != nil {
			return err
		}
		result = current
		return nil
	})
	return result, err
}

func (r *EnvironmentWorkRepository) ListWork(
	ctx context.Context,
	environmentID string,
	query app.EnvironmentWorkListQuery,
) (app.EnvironmentWorkListPage, error) {
	if err := r.authorizeEnvironment(ctx, environmentID); err != nil {
		return app.EnvironmentWorkListPage{}, err
	}
	clauses := []string{"environment_id = $1"}
	args := []any{environmentID}
	if query.After != nil {
		args = append(args, query.After.CreatedAt, query.After.ID)
		clauses = append(clauses, fmt.Sprintf(
			`(created_at < $%d OR (created_at = $%d AND id < $%d))`,
			len(args)-1, len(args)-1, len(args),
		))
	}
	args = append(args, query.Limit+1)
	rows, err := r.store.pool.Query(ctx,
		`SELECT `+environmentWorkColumns+` FROM environment_work WHERE `+
			strings.Join(clauses, " AND ")+fmt.Sprintf(
			` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args)), args...)
	if err != nil {
		return app.EnvironmentWorkListPage{}, err
	}
	defer rows.Close()
	items := make([]domain.EnvironmentWork, 0, query.Limit+1)
	for rows.Next() {
		item, err := scanEnvironmentWork(rows)
		if err != nil {
			return app.EnvironmentWorkListPage{}, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return app.EnvironmentWorkListPage{}, err
	}
	page := app.EnvironmentWorkListPage{Work: items}
	if len(items) > query.Limit {
		page.HasNext = true
		page.Work = items[:query.Limit]
	}
	return page, nil
}

func (r *EnvironmentWorkRepository) PollWork(
	ctx context.Context,
	environmentID string,
	input app.EnvironmentWorkPollInput,
) (*domain.EnvironmentWork, error) {
	if err := r.authorizeEnvironment(ctx, environmentID); err != nil {
		return nil, err
	}
	var result *domain.EnvironmentWork
	now := r.store.clock.Now().UTC().Truncate(time.Microsecond)
	secret, secretHash, err := newEnvironmentWorkSecret()
	if err != nil {
		return nil, err
	}
	err = r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		if input.WorkerID != "" {
			if _, err := tx.Exec(ctx, `
INSERT INTO environment_work_pollers (environment_id, worker_id, polled_at)
VALUES ($1, $2, $3)
ON CONFLICT (environment_id, worker_id) DO UPDATE SET polled_at = EXCLUDED.polled_at`,
				environmentID, input.WorkerID, now); err != nil {
				return err
			}
		}
		// Recover workers that disappeared after Ack or after their last lease
		// heartbeat. Reusing the Work ID preserves the control-plane audit item.
		if _, err := tx.Exec(ctx, `
UPDATE environment_work
SET state = 'stopped', stopped_at = COALESCE(stopped_at, $2)
WHERE environment_id = $1 AND state = 'stopping'
  AND stop_requested_at < $2::timestamptz - make_interval(secs => ttl_seconds)`,
			environmentID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE environment_work
SET state = 'queued', acknowledged_at = NULL, started_at = NULL,
    latest_heartbeat_at = NULL, polled_at = NULL, poll_worker_id = NULL,
    sessions_token_hash = NULL
WHERE environment_id = $1 AND (
    (state = 'starting' AND acknowledged_at < $2::timestamptz - make_interval(secs => ttl_seconds)) OR
    (state = 'active' AND latest_heartbeat_at < $2::timestamptz - make_interval(secs => ttl_seconds))
)`, environmentID, now); err != nil {
			return err
		}
		row := tx.QueryRow(ctx, `
WITH candidate AS (
    SELECT id
    FROM environment_work
    WHERE environment_id = $1
      AND state = 'queued'
      AND (polled_at IS NULL OR polled_at <= $2)
      AND NOT EXISTS (
          SELECT 1 FROM environment_work AS predecessor
          WHERE predecessor.session_id = environment_work.session_id
            AND predecessor.id <> environment_work.id
            AND predecessor.state IN ('starting', 'active', 'stopping')
      )
    ORDER BY created_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE environment_work AS work
SET polled_at = $3, poll_worker_id = NULLIF($4, ''), sessions_token_hash = $5
FROM candidate
WHERE work.id = candidate.id
RETURNING `+environmentWorkTargetColumns,
			environmentID, now.Add(-input.ReclaimAge), now, input.WorkerID, secretHash)
		work, err := scanEnvironmentWork(row)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		work.Secret = secret
		result = &work
		return nil
	})
	return result, err
}

func (r *EnvironmentWorkRepository) AckWork(
	ctx context.Context,
	environmentID, workID string,
) (domain.EnvironmentWork, error) {
	if err := r.authorizeEnvironment(ctx, environmentID); err != nil {
		return domain.EnvironmentWork{}, err
	}
	now := r.store.clock.Now().UTC().Truncate(time.Microsecond)
	var result domain.EnvironmentWork
	err := r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		if requestScope, _ := workspace.FromContext(ctx); requestScope.Session != nil {
			return domain.Precondition("session credential cannot acknowledge work")
		}
		work, err := scanEnvironmentWork(tx.QueryRow(ctx,
			`SELECT `+environmentWorkColumns+` FROM environment_work
WHERE environment_id = $1 AND id = $2 FOR UPDATE`, environmentID, workID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("work item not found")
		}
		if err != nil {
			return err
		}
		switch work.State {
		case domain.EnvironmentWorkQueued:
			work, err = scanEnvironmentWork(tx.QueryRow(ctx, `
UPDATE environment_work
SET state = 'starting', acknowledged_at = $3
WHERE environment_id = $1 AND id = $2 AND polled_at IS NOT NULL
RETURNING `+environmentWorkColumns, environmentID, workID, now))
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.Conflict("work item is not awaiting acknowledgement")
			}
			if err != nil {
				return err
			}
		case domain.EnvironmentWorkStarting:
			// Ack is idempotent for the same claim. This lets a worker safely
			// retry when the first successful response was lost.
		default:
			return domain.Conflict("work item is not awaiting acknowledgement")
		}
		result = work
		return nil
	})
	return result, err
}

func (r *EnvironmentWorkRepository) HeartbeatWork(
	ctx context.Context,
	environmentID, workID string,
	expected *string,
	desiredTTL *int64,
) (domain.EnvironmentWorkHeartbeat, error) {
	if err := r.authorizeEnvironment(ctx, environmentID); err != nil {
		return domain.EnvironmentWorkHeartbeat{}, err
	}
	var response domain.EnvironmentWorkHeartbeat
	now := r.store.clock.Now().UTC().Truncate(time.Microsecond)
	err := r.store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		work, err := r.workForUpdate(ctx, tx, environmentID, workID)
		if err != nil {
			return err
		}
		requestScope, _ := workspace.FromContext(ctx)
		if requestScope.Session != nil && environmentWorkLeaseExpired(work, now) {
			return domain.Precondition("work lease has expired")
		}
		if expected != nil {
			matches := *expected == "NO_HEARTBEAT" && work.LatestHeartbeatAt == nil
			if work.LatestHeartbeatAt != nil {
				matches = *expected == work.LatestHeartbeatAt.Format(time.RFC3339Nano)
			}
			if !matches {
				return domain.Precondition("work heartbeat precondition failed")
			}
		}
		if work.State != domain.EnvironmentWorkStarting &&
			work.State != domain.EnvironmentWorkActive {
			last := now
			if work.LatestHeartbeatAt != nil {
				last = *work.LatestHeartbeatAt
			}
			response = domain.EnvironmentWorkHeartbeat{
				LastHeartbeat: last, LeaseExtended: false,
				State: work.State, TTLSeconds: work.TTLSeconds,
			}
			return nil
		}
		ttl := work.TTLSeconds
		if desiredTTL != nil {
			ttl = *desiredTTL
		}
		started := work.StartedAt
		if started == nil {
			started = &now
		}
		if _, err := tx.Exec(ctx, `
UPDATE environment_work
SET state = 'active', started_at = $3, latest_heartbeat_at = $4, ttl_seconds = $5
WHERE environment_id = $1 AND id = $2`,
			environmentID, workID, started, now, ttl); err != nil {
			return err
		}
		response = domain.EnvironmentWorkHeartbeat{
			LastHeartbeat: now, LeaseExtended: true,
			State: domain.EnvironmentWorkActive, TTLSeconds: ttl,
		}
		return nil
	})
	return response, err
}

func (r *EnvironmentWorkRepository) StopWork(
	ctx context.Context,
	environmentID, workID string,
	force bool,
) error {
	if err := r.authorizeEnvironment(ctx, environmentID); err != nil {
		return err
	}
	now := r.store.clock.Now().UTC().Truncate(time.Microsecond)
	return r.store.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		work, err := r.workForUpdate(ctx, tx, environmentID, workID)
		if err != nil {
			return err
		}
		requestScope, _ := workspace.FromContext(ctx)
		if requestScope.Session != nil && environmentWorkLeaseExpired(work, now) {
			return domain.Precondition("work lease has expired")
		}
		var activationSeq int64
		if err := tx.QueryRow(ctx, `SELECT activation_seq FROM environment_work
WHERE environment_id = $1 AND id = $2`, environmentID, workID).Scan(&activationSeq); err != nil {
			return err
		}
		if work.State == domain.EnvironmentWorkStopped {
			return domain.Conflict("work item is already stopped")
		}
		next := domain.EnvironmentWorkStopping
		stoppedAt := (*time.Time)(nil)
		if force || work.State == domain.EnvironmentWorkQueued ||
			work.State == domain.EnvironmentWorkStarting {
			next = domain.EnvironmentWorkStopped
			stoppedAt = &now
		}
		_, err = tx.Exec(ctx, `
UPDATE environment_work
SET state = $3, stop_requested_at = COALESCE(stop_requested_at, $4), stopped_at = $5
WHERE environment_id = $1 AND id = $2`, environmentID, workID, next, now, stoppedAt)
		if err != nil {
			return err
		}
		// Runnable input can race a worker's shutdown. Queue a successor while
		// retaining the stopping item, and let Poll serialize the handoff. This
		// closes the admission-before-Stop race without allowing two workers to
		// execute the same Session concurrently.
		maxSeq, err := q.MaxEventSeq(ctx, work.SessionID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
INSERT INTO environment_work (
    id, environment_id, session_id, activation_seq, state, metadata, created_at
)
SELECT $1, $2, $3, $4, 'queued', '{}'::jsonb, $5
WHERE EXISTS (
    SELECT 1 FROM events
    WHERE session_id = $3 AND seq > $6 AND processed_at IS NULL
      AND type IN (
          'user.message', 'user.define_outcome', 'user.custom_tool_result',
          'user.tool_confirmation', 'user.tool_result'
      )
)
ON CONFLICT (session_id) WHERE state IN ('queued', 'starting', 'active') DO NOTHING`,
			r.store.ids.NewID(domain.PrefixEnvironmentWork), environmentID, work.SessionID,
			maxSeq, now, activationSeq)
		return err
	})
}

// FailWork is the worker-owned terminal boundary for invalid immutable Session
// inputs. It fences the current lease, closes every Thread and active attempt,
// publishes an observable terminal error, and wakes orchestration in the same
// transaction. Transient preparation errors must not call this method: their
// Work lease is allowed to expire so another worker can retry the activation.
func (r *EnvironmentWorkRepository) FailWork(
	ctx context.Context,
	environmentID, workID, message string,
) error {
	if err := r.authorizeEnvironment(ctx, environmentID); err != nil {
		return err
	}
	now := r.store.clock.Now().UTC().Truncate(time.Microsecond)
	return r.store.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
		var sessionID string
		if err := tx.QueryRow(ctx, `
SELECT session_id FROM environment_work
WHERE environment_id = $1 AND id = $2`, environmentID, workID).Scan(&sessionID); errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("work item not found")
		} else if err != nil {
			return err
		}

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
		work, err := r.workForUpdate(ctx, tx, environmentID, workID)
		if err != nil {
			return err
		}
		requestScope, _ := workspace.FromContext(ctx)
		if requestScope.Session != nil && environmentWorkLeaseExpired(work, now) {
			return domain.Precondition("work lease has expired")
		}
		if work.State == domain.EnvironmentWorkStopped {
			return domain.Conflict("work item is already stopped")
		}
		if work.State == domain.EnvironmentWorkStopping {
			return domain.Conflict("work item is stopping")
		}
		if work.SessionID != session.ID {
			return domain.Conflict("work item does not belong to its Session")
		}

		if session.Status != domain.StatusTerminated {
			primaryID, err := q.GetPrimarySessionThreadID(ctx, session.ID)
			if err != nil {
				return err
			}
			primary, err := loadSessionThreadForUpdate(ctx, tx, session.ID, primaryID)
			if err != nil {
				return err
			}
			if err := r.store.interruptSessionThreadAttemptsLocked(
				ctx, tx, q, session.ID, primaryID,
			); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
DELETE FROM pending_actions
WHERE session_id = $1 AND thread_id = $2 AND resolved_at IS NULL`,
				session.ID, primaryID,
			); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `
UPDATE events
SET processed_at = COALESCE(processed_at, $3)
WHERE session_id = $1 AND thread_id = $2 AND processed_at IS NULL`,
				session.ID, primaryID, now,
			); err != nil {
				return err
			}

			maxSeq, err := q.MaxEventSeq(ctx, session.ID)
			if err != nil {
				return err
			}
			errorEventID := r.store.ids.NewID(domain.PrefixEvent)
			_, maxSeq, err = r.store.appendThreadDraftsAt(
				ctx, q, session.ID, primaryID,
				[]domain.EventDraft{{
					ID: errorEventID, Type: domain.EvSessionError,
					Payload: map[string]any{"error": map[string]any{
						"type": "session_input_failed_error", "message": message,
						"retry_status": map[string]any{"type": "terminal"},
					}},
				}}, maxSeq, nil, now,
			)
			if err != nil {
				return err
			}
			_, maxSeq, err = r.store.terminateChildSessionThreadsLocked(
				ctx, tx, q, session.ID, primaryID, errorEventID, maxSeq, now,
			)
			if err != nil {
				return err
			}
			_, maxSeq, err = r.store.appendThreadDraftsAt(
				ctx, q, session.ID, primaryID,
				[]domain.EventDraft{{Type: domain.EvSessionStatusTerminated, Payload: map[string]any{}}},
				maxSeq, nil, now,
			)
			if err != nil {
				return err
			}
			primary.TransitionStatus(domain.StatusTerminated, now)
			if err := putSessionThreadTx(ctx, tx, primary); err != nil {
				return err
			}
			session.TransitionStatus(domain.StatusTerminated, now)
			if err := putSessionOnlyTx(ctx, tx, session); err != nil {
				return err
			}
			if err := q.UpsertOutbox(ctx, pgstore.UpsertOutboxParams{
				SessionID: session.ID, MaxEventSeq: maxSeq, EnqueuedAt: tsUTC(now),
			}); err != nil {
				return err
			}
		}

		_, err = tx.Exec(ctx, `
UPDATE environment_work
SET state = 'stopped', stop_requested_at = COALESCE(stop_requested_at, $3), stopped_at = $3
WHERE environment_id = $1 AND id = $2`, environmentID, workID, now)
		return err
	})
}

func (r *EnvironmentWorkRepository) workForUpdate(
	ctx context.Context,
	tx pgx.Tx,
	environmentID, workID string,
) (domain.EnvironmentWork, error) {
	requestScope, _ := workspace.FromContext(ctx)
	query := `SELECT ` + environmentWorkColumns + ` FROM environment_work
WHERE environment_id = $1 AND id = $2`
	args := []any{environmentID, workID}
	if requestScope.Session != nil {
		claim := requestScope.Session
		if claim.EnvironmentID != environmentID || claim.WorkID != workID ||
			len(claim.CredentialDigest) == 0 {
			return domain.EnvironmentWork{}, domain.Precondition("work lease is no longer owned")
		}
		query += ` AND sessions_token_hash = $3`
		args = append(args, claim.CredentialDigest)
	}
	work, err := scanEnvironmentWork(tx.QueryRow(ctx, query+` FOR UPDATE`, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		if requestScope.Session != nil {
			return domain.EnvironmentWork{}, domain.Precondition("work lease is no longer owned")
		}
		return domain.EnvironmentWork{}, domain.NotFound("work item not found")
	}
	if err == nil && requestScope.Session != nil && work.SessionID != requestScope.Session.SessionID {
		return domain.EnvironmentWork{}, domain.Precondition("work lease is no longer owned")
	}
	return work, err
}

func environmentWorkLeaseExpired(work domain.EnvironmentWork, now time.Time) bool {
	var anchor *time.Time
	switch work.State {
	case domain.EnvironmentWorkStarting:
		anchor = work.AcknowledgedAt
	case domain.EnvironmentWorkActive:
		anchor = work.LatestHeartbeatAt
	case domain.EnvironmentWorkStopping:
		anchor = work.StopRequestedAt
	default:
		return false
	}
	if anchor == nil || work.TTLSeconds <= 0 ||
		work.TTLSeconds > app.MaxEnvironmentWorkTTLSeconds {
		return true
	}
	return anchor.Before(now.Add(-time.Duration(work.TTLSeconds) * time.Second))
}

func (r *EnvironmentWorkRepository) WorkStats(
	ctx context.Context,
	environmentID string,
) (domain.EnvironmentWorkQueueStats, error) {
	if err := r.authorizeEnvironment(ctx, environmentID); err != nil {
		return domain.EnvironmentWorkQueueStats{}, err
	}
	var (
		stats  domain.EnvironmentWorkQueueStats
		oldest *time.Time
	)
	err := r.store.pool.QueryRow(ctx, `
SELECT
    COUNT(*) FILTER (WHERE state = 'queued' AND polled_at IS NULL)::bigint,
    COUNT(*) FILTER (WHERE state = 'queued' AND polled_at IS NOT NULL)::bigint,
    MIN(created_at) FILTER (WHERE state = 'queued'),
    (SELECT COUNT(*)::bigint FROM environment_work_pollers
     WHERE environment_id = $1 AND polled_at >= $2)
FROM environment_work
WHERE environment_id = $1`, environmentID, r.store.clock.Now().UTC().Add(-30*time.Second)).Scan(
		&stats.Depth, &stats.Pending, &oldest, &stats.WorkersPolling,
	)
	stats.OldestQueuedAt = utcTimePtr(oldest)
	return stats, err
}
