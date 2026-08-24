package pg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

// GetSessionThread returns a Thread only when both path identifiers name the
// same resource. The Thread row is the execution projection source of truth;
// the parent Session is not decoded on this read path.
func (s *Store) GetSessionThread(
	ctx context.Context,
	sessionID string,
	threadID string,
) (domain.SessionThread, error) {
	var (
		id, storedSessionID string
		parentID            *string
		status              string
		body                []byte
		createdAt           time.Time
		updatedAt           time.Time
		archivedAt          *time.Time
	)
	err := s.pool.QueryRow(ctx, `
SELECT id, session_id, parent_thread_id, status, body,
       created_at, updated_at, archived_at
FROM session_threads
WHERE session_id = $1 AND id = $2`,
		sessionID, threadID,
	).Scan(
		&id, &storedSessionID, &parentID, &status, &body,
		&createdAt, &updatedAt, &archivedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return domain.SessionThread{}, domain.NotFound("session thread not found")
	}
	if err != nil {
		return domain.SessionThread{}, err
	}
	return sessionThreadFromRow(
		id, storedSessionID, parentID, domain.Status(status), body,
		createdAt, updatedAt, archivedAt,
	)
}

// ListSessionThreads returns threads in the documented order: primary first,
// then children in spawn order. Each result is decoded from its own projection.
func (s *Store) ListSessionThreads(
	ctx context.Context,
	sessionID string,
	query app.SessionThreadListQuery,
) ([]domain.SessionThread, error) {
	if query.Limit <= 0 {
		query.Limit = app.DefaultSessionThreadListLimit
	}
	var exists int
	if err := s.pool.QueryRow(ctx,
		`SELECT 1 FROM sessions WHERE id = $1`, sessionID,
	).Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.NotFound("session not found")
	} else if err != nil {
		return nil, err
	}

	args := []any{sessionID}
	boundary := ""
	if query.Boundary != nil {
		args = append(args, query.Boundary.CreatedAt, query.Boundary.ID)
		boundary = fmt.Sprintf(
			` AND (thread.created_at > $%d OR (thread.created_at = $%d AND thread.id > $%d))`,
			len(args)-1, len(args)-1, len(args),
		)
	}
	args = append(args, query.Limit)
	rows, err := s.pool.Query(ctx, `
SELECT thread.id, thread.session_id, thread.parent_thread_id,
       thread.status, thread.body, thread.created_at,
       thread.updated_at, thread.archived_at
FROM session_threads AS thread
WHERE thread.session_id = $1`+boundary+`
ORDER BY CASE WHEN thread.kind = 'primary' THEN 0 ELSE 1 END,
         thread.created_at, thread.id
LIMIT $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]domain.SessionThread, 0, query.Limit)
	for rows.Next() {
		var (
			id, storedSessionID string
			parentID            *string
			status              string
			body                []byte
			createdAt           time.Time
			updatedAt           time.Time
			archivedAt          *time.Time
		)
		if err := rows.Scan(
			&id, &storedSessionID, &parentID, &status, &body,
			&createdAt, &updatedAt, &archivedAt,
		); err != nil {
			return nil, err
		}
		thread, err := sessionThreadFromRow(
			id, storedSessionID, parentID, domain.Status(status), body,
			createdAt, updatedAt, archivedAt,
		)
		if err != nil {
			return nil, err
		}
		result = append(result, thread)
	}
	return result, rows.Err()
}

// ListChildSessionThreadIDs returns every child orchestration identity,
// including already archived Threads. Session deletion uses the complete set:
// Temporal termination is idempotent, and an archived row may still have a
// shutdown instruction waiting in the relay after a prior process crash.
func (s *Store) ListChildSessionThreadIDs(
	ctx context.Context,
	sessionID string,
) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
SELECT id
FROM session_threads
WHERE session_id = $1 AND kind = 'child'
ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]string, 0)
	for rows.Next() {
		var threadID string
		if err := rows.Scan(&threadID); err != nil {
			return nil, err
		}
		result = append(result, threadID)
	}
	return result, rows.Err()
}

// ArchiveSessionThread durably ends one idle or rescheduling child without
// changing the primary Thread lifecycle. The Thread projection, queued-input
// flush, pending-action closure, lifecycle events, Session aggregate, and
// terminate outbox instruction share the Session serialization transaction.
func (s *Store) ArchiveSessionThread(
	ctx context.Context,
	sessionID string,
	threadID string,
) (domain.SessionThread, error) {
	if sessionID == "" || threadID == "" {
		return domain.SessionThread{}, domain.Validation(
			"session and session Thread are required",
		)
	}
	var result domain.SessionThread
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
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
		thread, err := loadSessionThreadForUpdate(ctx, tx, sessionID, threadID)
		if err != nil {
			return err
		}
		var threadKind string
		if err := tx.QueryRow(ctx, `
SELECT kind FROM session_threads
WHERE session_id = $1 AND id = $2`, sessionID, threadID).Scan(&threadKind); err != nil {
			return err
		}
		if thread.ParentThreadID == nil {
			return domain.Conflict("primary Thread archival follows the Session lifecycle")
		}

		now := s.clock.Now().UTC()
		maxSeq, err := q.MaxEventSeq(ctx, sessionID)
		if err != nil {
			return err
		}
		if thread.ArchivedAt != nil {
			if threadKind != "advisor" {
				if err := q.UpsertThreadTermination(
					ctx,
					pgstore.UpsertThreadTerminationParams{
						SessionID: sessionID, ThreadID: threadID,
						MaxEventSeq: maxSeq, EnqueuedAt: tsUTC(now),
					},
				); err != nil {
					return err
				}
			}
			result = thread
			return nil
		}
		if thread.Status == domain.StatusRunning {
			return domain.Conflict(
				"cannot archive a running session Thread; interrupt first",
			)
		}

		pendingResult, err := tx.Exec(ctx, `
DELETE FROM pending_actions
WHERE session_id = $1 AND thread_id = $2 AND resolved_at IS NULL`,
			sessionID, threadID,
		)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
UPDATE events
SET processed_at = COALESCE(processed_at, $3)
WHERE session_id = $1 AND thread_id = $2 AND processed_at IS NULL`,
			sessionID, threadID, now,
		); err != nil {
			return err
		}

		wasTerminated := thread.Status == domain.StatusTerminated
		thread.TransitionStatus(domain.StatusTerminated, now)
		archivedAt := now
		thread.ArchivedAt = &archivedAt
		if err := putSessionThreadTx(ctx, tx, thread); err != nil {
			return err
		}
		if !wasTerminated {
			lifecycle := domain.EventDraft{
				Type:    domain.EvSessionThreadStatusTerminated,
				Payload: threadLifecyclePayload(thread),
			}
			_, next, err := s.appendThreadDrafts(
				ctx, q, sessionID, threadID,
				[]domain.EventDraft{lifecycle}, maxSeq, nil,
			)
			if err != nil {
				return err
			}
			maxSeq = next
			_, next, err = s.appendThreadDrafts(
				ctx, q, sessionID, *thread.ParentThreadID,
				[]domain.EventDraft{lifecycle}, maxSeq, nil,
			)
			if err != nil {
				return err
			}
			maxSeq = next
		}

		aggregated, err := aggregateSessionThreadStatus(ctx, tx, sessionID)
		if err != nil {
			return err
		}
		oldStatus := session.Status
		session.TransitionStatus(aggregated, now)
		if err := putSessionOnlyTx(ctx, tx, session); err != nil {
			return err
		}
		pendingChanged := pendingResult.RowsAffected() > 0
		if oldStatus != aggregated || (aggregated == domain.StatusIdle && pendingChanged) {
			var statusDraft *domain.EventDraft
			switch aggregated {
			case domain.StatusRunning:
				statusDraft = &domain.EventDraft{
					Type: domain.EvSessionStatusRunning, Payload: map[string]any{},
				}
			case domain.StatusRescheduling:
				statusDraft = &domain.EventDraft{
					Type: domain.EvSessionStatusRescheduling, Payload: map[string]any{},
				}
			case domain.StatusIdle:
				pendingIDs, err := q.SessionPendingClientActionEventIDs(ctx, sessionID)
				if err != nil {
					return err
				}
				stopReason := map[string]any{"type": "end_turn"}
				if len(pendingIDs) > 0 {
					stopReason = map[string]any{
						"type": "requires_action", "event_ids": pendingIDs,
					}
				}
				statusDraft = &domain.EventDraft{
					Type:    domain.EvSessionStatusIdle,
					Payload: map[string]any{"stop_reason": stopReason},
				}
			case domain.StatusTerminated:
				statusDraft = &domain.EventDraft{
					Type: domain.EvSessionStatusTerminated, Payload: map[string]any{},
				}
			}
			if statusDraft != nil {
				_, next, err := s.appendThreadDrafts(
					ctx, q, sessionID, *thread.ParentThreadID,
					[]domain.EventDraft{*statusDraft}, maxSeq, nil,
				)
				if err != nil {
					return err
				}
				maxSeq = next
			}
		}
		if threadKind != "advisor" {
			if err := q.UpsertThreadTermination(
				ctx,
				pgstore.UpsertThreadTerminationParams{
					SessionID: sessionID, ThreadID: threadID,
					MaxEventSeq: maxSeq, EnqueuedAt: tsUTC(now),
				},
			); err != nil {
				return err
			}
		}
		result = thread
		return nil
	})
	if err != nil {
		return domain.SessionThread{}, err
	}
	s.notifySession(ctx, sessionID)
	return result, nil
}

func enqueueChildThreadTerminationsLocked(
	ctx context.Context,
	tx pgx.Tx,
	q *pgstore.Queries,
	sessionID string,
	maxSeq int64,
	now time.Time,
) error {
	rows, err := tx.Query(ctx, `
SELECT id
FROM session_threads
WHERE session_id = $1 AND kind = 'child'
ORDER BY created_at, id`, sessionID)
	if err != nil {
		return err
	}
	threadIDs := make([]string, 0)
	for rows.Next() {
		var threadID string
		if err := rows.Scan(&threadID); err != nil {
			rows.Close()
			return err
		}
		threadIDs = append(threadIDs, threadID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, threadID := range threadIDs {
		if err := q.UpsertThreadTermination(
			ctx,
			pgstore.UpsertThreadTerminationParams{
				SessionID: sessionID, ThreadID: threadID,
				MaxEventSeq: maxSeq, EnqueuedAt: tsUTC(now),
			},
		); err != nil {
			return err
		}
	}
	return nil
}

// terminateChildSessionThreadsLocked fences every persistent child when its
// primary owner reaches a terminal state. Child completion takes the same
// Session row lock, so either it commits before this transition or observes the
// terminated projection and becomes a no-op. Lifecycle events and durable
// Workflow termination intents commit in this same transaction.
func (s *Store) terminateChildSessionThreadsLocked(
	ctx context.Context,
	tx pgx.Tx,
	q *pgstore.Queries,
	sessionID string,
	primaryThreadID string,
	triggerEventID string,
	maxSeq int64,
	now time.Time,
) ([]domain.Event, int64, error) {
	rows, err := tx.Query(ctx, `
SELECT body
FROM session_threads
WHERE session_id = $1
  AND kind = 'child'
  AND archived_at IS NULL
  AND status <> 'terminated'
ORDER BY created_at, id
FOR UPDATE`, sessionID)
	if err != nil {
		return nil, maxSeq, err
	}
	children := make([]domain.SessionThread, 0)
	for rows.Next() {
		var body []byte
		if err := rows.Scan(&body); err != nil {
			rows.Close()
			return nil, maxSeq, err
		}
		var thread domain.SessionThread
		if err := json.Unmarshal(body, &thread); err != nil {
			rows.Close()
			return nil, maxSeq, err
		}
		children = append(children, thread)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, maxSeq, err
	}
	rows.Close()

	committed := make([]domain.Event, 0, len(children)*2)
	for _, thread := range children {
		if _, err := tx.Exec(ctx, `
DELETE FROM pending_actions
WHERE session_id = $1 AND thread_id = $2 AND resolved_at IS NULL`,
			sessionID, thread.ID,
		); err != nil {
			return nil, maxSeq, err
		}
		if _, err := tx.Exec(ctx, `
UPDATE events
SET processed_at = COALESCE(processed_at, $3)
WHERE session_id = $1 AND thread_id = $2 AND processed_at IS NULL`,
			sessionID, thread.ID, now,
		); err != nil {
			return nil, maxSeq, err
		}

		thread.TransitionStatus(domain.StatusTerminated, now)
		if err := putSessionThreadTx(ctx, tx, thread); err != nil {
			return nil, maxSeq, err
		}
		lifecycle := domain.EventDraft{
			Type:    domain.EvSessionThreadStatusTerminated,
			Payload: threadLifecyclePayload(thread),
		}
		childEvents, next, err := s.appendThreadDraftsAt(
			ctx, q, sessionID, thread.ID,
			[]domain.EventDraft{lifecycle}, maxSeq, nil, now,
		)
		if err != nil {
			return nil, maxSeq, err
		}
		maxSeq = next
		committed = append(committed, childEvents...)
		primaryEvents, next, err := s.appendThreadDraftsAt(
			ctx, q, sessionID, primaryThreadID,
			[]domain.EventDraft{lifecycle}, maxSeq, &triggerEventID, now,
		)
		if err != nil {
			return nil, maxSeq, err
		}
		maxSeq = next
		committed = append(committed, primaryEvents...)
	}
	if err := enqueueChildThreadTerminationsLocked(
		ctx, tx, q, sessionID, maxSeq, now,
	); err != nil {
		return nil, maxSeq, err
	}
	return committed, maxSeq, nil
}

// CreateChildSessionThread atomically captures a callable Agent from the
// Session-owned resolved roster, inserts its independent Thread projection,
// and appends session.thread_created to the parent ledger. It deliberately does
// not start execution; the child workflow transition owns status_running.
func (s *Store) CreateChildSessionThread(
	ctx context.Context,
	sessionID string,
	parentThreadID string,
	agentName string,
) (domain.SessionThread, domain.Event, error) {
	if parentThreadID == "" || agentName == "" {
		return domain.SessionThread{}, domain.Event{}, domain.Validation(
			"parent session thread and agent name are required",
		)
	}
	var (
		created domain.SessionThread
		event   domain.Event
	)
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
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
		if session.ArchivedAt != nil || session.Status == domain.StatusTerminated {
			return domain.Conflict("cannot create a child Thread in a terminated Session")
		}
		if session.AgentSnapshot.Multiagent == nil || len(session.MultiagentRoster) == 0 {
			return domain.Conflict("Session Agent is not a multiagent coordinator")
		}

		var parentKind string
		if err := tx.QueryRow(ctx, `
SELECT kind
FROM session_threads
WHERE session_id = $1 AND id = $2
FOR UPDATE`, sessionID, parentThreadID).Scan(&parentKind); errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("parent session thread not found")
		} else if err != nil {
			return err
		}
		if parentKind != "primary" {
			return domain.Conflict("child Threads cannot spawn nested Threads")
		}

		var member *domain.Agent
		for index := range session.MultiagentRoster {
			if session.MultiagentRoster[index].Name != agentName {
				continue
			}
			if member != nil {
				return domain.Conflict("agent name does not uniquely identify a Session roster member")
			}
			candidate := session.MultiagentRoster[index]
			member = &candidate
		}
		if member == nil {
			return domain.Validation("agent name is not present in the Session roster")
		}

		created = domain.NewChildSessionThread(
			s.ids.NewID(domain.PrefixSessionThread), sessionID,
			parentThreadID, *member, s.clock.Now(),
		)
		body, err := json.Marshal(created)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
INSERT INTO session_threads (
    id, session_id, parent_thread_id, kind, status, body,
    created_at, updated_at, archived_at
) VALUES ($1, $2, $3, 'child', $4, $5, $6, $7, NULL)`,
			created.ID, created.SessionID, created.ParentThreadID,
			created.Status, body, created.CreatedAt, created.UpdatedAt,
		); err != nil {
			return err
		}
		maxSeq, err := q.MaxEventSeq(ctx, sessionID)
		if err != nil {
			return err
		}
		committed, _, err := s.appendThreadDrafts(
			ctx, q, sessionID, parentThreadID,
			[]domain.EventDraft{{
				Type: domain.EvSessionThreadCreated,
				Payload: map[string]any{
					"agent_name":        agentName,
					"session_thread_id": created.ID,
				},
			}},
			maxSeq, nil,
		)
		if err != nil {
			return err
		}
		event = committed[0]
		return nil
	})
	if err != nil {
		return domain.SessionThread{}, domain.Event{}, err
	}
	s.notifySession(ctx, sessionID)
	return created, event, nil
}

// AppendThreadEvents appends server-owned events to one Thread ledger under the
// Session serialization fence. Projection transitions and child workflow
// wakeups are intentionally separate operations.
func (s *Store) AppendThreadEvents(
	ctx context.Context,
	sessionID string,
	threadID string,
	drafts []domain.EventDraft,
) ([]domain.Event, error) {
	if threadID == "" || len(drafts) == 0 {
		return nil, domain.Validation("session Thread and events are required")
	}
	for _, draft := range drafts {
		if domain.IsClientSubmittable(draft.Type) || isSessionProjectionEvent(draft.Type) {
			return nil, domain.Validation("Thread ledger append accepts server events only")
		}
	}
	var committed []domain.Event
	err := s.withPGXTx(ctx, func(tx pgx.Tx, q *pgstore.Queries) error {
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
		if session.ArchivedAt != nil || session.Status == domain.StatusTerminated {
			return domain.Conflict("cannot append events to a terminated Session")
		}
		var status string
		var archivedAt *time.Time
		if err := tx.QueryRow(ctx, `
SELECT status, archived_at
FROM session_threads
WHERE session_id = $1 AND id = $2
FOR UPDATE`, sessionID, threadID).Scan(&status, &archivedAt); errors.Is(err, pgx.ErrNoRows) {
			return domain.NotFound("session thread not found")
		} else if err != nil {
			return err
		}
		if archivedAt != nil || domain.Status(status) == domain.StatusTerminated {
			return domain.Conflict("cannot append events to a terminated Session Thread")
		}
		maxSeq, err := q.MaxEventSeq(ctx, sessionID)
		if err != nil {
			return err
		}
		committed, _, err = s.appendThreadDrafts(
			ctx, q, sessionID, threadID, drafts, maxSeq, nil,
		)
		return err
	})
	if err != nil {
		return nil, err
	}
	s.notifySession(ctx, sessionID)
	return committed, nil
}

// ThreadEventsAfter is the authoritative cursor read for one independent child
// Workflow. Sequence remains Session-wide, but rows from sibling ledgers never
// enter this execution history.
func (s *Store) ThreadEventsAfter(
	ctx context.Context,
	sessionID string,
	threadID string,
	cursor int64,
	limit int,
) ([]domain.Event, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
SELECT event.*
FROM events AS event
WHERE event.session_id = $1 AND event.thread_id = $2 AND event.seq > $3
ORDER BY event.seq
LIMIT $4`, sessionID, threadID, cursor, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	stored, err := scanEventRows(rows)
	if err != nil {
		return nil, err
	}
	return eventsFromRows(stored)
}

func sessionThreadFromRow(
	threadID string,
	sessionID string,
	parentID *string,
	status domain.Status,
	body []byte,
	createdAt time.Time,
	updatedAt time.Time,
	archivedAt *time.Time,
) (domain.SessionThread, error) {
	var thread domain.SessionThread
	if err := json.Unmarshal(body, &thread); err != nil {
		return domain.SessionThread{}, fmt.Errorf("pg: decode session thread projection: %w", err)
	}
	thread.ID = threadID
	thread.SessionID = sessionID
	thread.ParentThreadID = parentID
	thread.Status = status
	thread.CreatedAt = createdAt.UTC()
	thread.UpdatedAt = updatedAt.UTC()
	thread.ArchivedAt = utcTimePtr(archivedAt)
	return thread, nil
}

func putPrimaryThreadProjection(
	ctx context.Context,
	q *pgstore.Queries,
	thread domain.SessionThread,
) error {
	body, err := json.Marshal(thread)
	if err != nil {
		return err
	}
	return q.UpdatePrimarySessionThreadProjection(
		ctx,
		pgstore.UpdatePrimarySessionThreadProjectionParams{
			Status: string(thread.Status), Body: body,
			UpdatedAt:  tsUTC(thread.UpdatedAt),
			ArchivedAt: tsPtr(thread.ArchivedAt),
			SessionID:  thread.SessionID,
		},
	)
}

// putPrimarySessionThreadProjection synchronizes the current single-Thread
// execution into the independent primary projection. Callers already hold the
// Session row lock, which is the serialization fence for every Thread mutation
// in that Session.
func (s *Store) putPrimarySessionThreadProjection(
	ctx context.Context,
	q *pgstore.Queries,
	session domain.Session,
) error {
	body, err := q.GetPrimarySessionThreadProjection(ctx, session.ID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("pg: primary session thread is missing for %s", session.ID)
	}
	if err != nil {
		return err
	}
	var thread domain.SessionThread
	if err := json.Unmarshal(body, &thread); err != nil {
		return fmt.Errorf("pg: decode primary session thread projection: %w", err)
	}
	if session.AgentSnapshot.Multiagent == nil {
		thread.ApplyPrimarySessionProjection(session)
	} else {
		thread.ApplyIndependentPrimarySessionProjection(session)
	}
	body, err = json.Marshal(thread)
	if err != nil {
		return err
	}
	return q.UpdatePrimarySessionThreadProjection(
		ctx,
		pgstore.UpdatePrimarySessionThreadProjectionParams{
			Status: string(thread.Status), Body: body,
			UpdatedAt: tsUTC(thread.UpdatedAt), ArchivedAt: tsPtr(thread.ArchivedAt),
			SessionID: session.ID,
		},
	)
}
