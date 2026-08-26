-- Typed queries for the Temporal/PostgreSQL session path, compiled by sqlc into
-- internal/pg/pgstore. Transactions are kept explicit in Go; these are the
-- individual statements the admission, completion, cursor, and relay operations
-- compose under a single tx.

-- name: InsertSession :exec
INSERT INTO sessions (
    id, status, body, created_at, updated_at,
    agent_id, agent_version, environment_id, deployment_id, archived_at,
    workspace_id
)
VALUES (
    @id, @status, @body, @created_at, @updated_at,
    @agent_id, @agent_version, @environment_id, @deployment_id, @archived_at,
    @workspace_id
);

-- name: GetSession :one
SELECT id, status, body, created_at, updated_at, workspace_id
FROM sessions
WHERE id = @id;

-- name: GetSessionWorkspaceID :one
SELECT workspace_id FROM sessions WHERE id = @id;

-- LockSession takes the per-session admission lock. Every admission and
-- completion for a session serializes on this row, which is what makes receipt
-- sequence assignment and the coalescing outbox upsert race-free.
-- name: LockSession :one
SELECT id, status, body, created_at, updated_at, deleting_at, workspace_id
FROM sessions
WHERE id = @id
FOR UPDATE;

-- name: UpdateSessionStatus :exec
UPDATE sessions
SET status = @status, body = @body, updated_at = @updated_at
WHERE id = @id;

-- The primary Thread has an independent projection even while the current
-- single-thread runtime updates it alongside the Session aggregate.
-- name: GetPrimarySessionThreadProjection :one
SELECT body
FROM session_threads
WHERE session_id = @session_id AND kind = 'primary';

-- name: UpdatePrimarySessionThreadProjection :exec
UPDATE session_threads
SET status = @status,
    body = @body,
    updated_at = @updated_at,
    archived_at = @archived_at
WHERE session_id = @session_id AND kind = 'primary';

-- name: GetPrimarySessionThreadID :one
SELECT id
FROM session_threads
WHERE session_id = @session_id AND kind = 'primary';

-- HasActiveChildThreads lets primary completion preserve the aggregate Session
-- running state while one or more independently executing children are active.
-- name: HasActiveChildThreads :one
SELECT EXISTS(
    SELECT 1
    FROM session_threads
    WHERE session_id = @session_id
      AND kind = 'child'
      AND archived_at IS NULL
      AND status IN ('running', 'rescheduling')
) AS active;

-- MaxEventSeq returns the current highest receipt sequence for a session, or 0
-- when the session has no events yet. Called while holding the admission lock.
-- name: MaxEventSeq :one
SELECT COALESCE(MAX(seq), 0)::bigint AS max_seq
FROM events
WHERE session_id = @session_id;

-- name: InsertEvent :exec
INSERT INTO events (
    id, session_id, thread_id, seq, type, payload,
    turn_event_id, created_at, processed_at
)
VALUES (
    @id, @session_id, @thread_id, @seq, @type, @payload,
    @turn_event_id, @created_at, @processed_at
);

-- EnqueueWebhookEvent snapshots the enabled subscriptions in the same
-- transaction as the source lifecycle change. No event row is retained when
-- no endpoint is subscribed, and later subscriptions never backfill it.
-- name: EnqueueWebhookEvent :exec
WITH subscribers AS (
    SELECT id
    FROM webhooks
    WHERE webhooks.workspace_id = sqlc.arg(workspace_id)
      AND webhooks.status = 'enabled'
      AND sqlc.arg(event_type)::text = ANY(webhooks.event_types)
    ORDER BY webhooks.id
    FOR UPDATE
), inserted_event AS (
    INSERT INTO webhook_events (
        id, workspace_id, event_type, resource_id, payload, created_at
    )
    SELECT sqlc.arg(id), sqlc.arg(workspace_id), sqlc.arg(event_type),
           sqlc.arg(resource_id), sqlc.arg(payload), sqlc.arg(created_at)
    WHERE EXISTS (SELECT 1 FROM subscribers)
    RETURNING id, created_at
)
INSERT INTO webhook_deliveries (
    webhook_id, event_id, next_attempt_at, created_at
)
SELECT subscribers.id, inserted_event.id,
       inserted_event.created_at, inserted_event.created_at
FROM subscribers CROSS JOIN inserted_event;

-- ListEventsAfter returns events with seq strictly greater than a cursor, in
-- ascending receipt order. This is how the SessionWorkflow consumes the ledger
-- after its durable cursor; the limit bounds one wakeup's batch.
-- name: ListEventsAfter :many
SELECT event.*
FROM events AS event
WHERE event.session_id = @session_id
  AND event.thread_id = (
      SELECT thread.id FROM session_threads AS thread
      WHERE thread.session_id = @session_id AND thread.kind = 'primary'
  )
  AND event.seq > @after_seq
ORDER BY event.seq
LIMIT @row_limit;

-- ListEventsByTurn returns the output events a completed turn committed,
-- identified by the trigger event id that caused them, in receipt order. The
-- idempotent completion path replays these when a turn is already processed.
-- name: ListEventsByTurn :many
SELECT events.*
FROM events
WHERE session_id = @session_id AND turn_event_id = @turn_event_id
ORDER BY seq;

-- name: GetEvent :one
SELECT events.*
FROM events
WHERE session_id = @session_id AND id = @id;

-- FirstUnprocessedInterruptAfter finds the earliest durable interrupt that can
-- race the named turn. The caller holds the session row lock, so an empty result
-- means turn completion linearized before any later interrupt admission.
-- name: FirstUnprocessedInterruptAfter :one
SELECT event.*
FROM events AS event
WHERE event.session_id = @session_id
  AND event.thread_id = (
      SELECT thread.id FROM session_threads AS thread
      WHERE thread.session_id = @session_id AND thread.kind = 'primary'
  )
  AND event.seq > @after_seq
  AND event.type = 'user.interrupt'
  AND event.processed_at IS NULL
ORDER BY event.seq
LIMIT 1;

-- FirstUnprocessedThreadInterruptAfter is the child-Thread counterpart of the
-- primary query above. Global interrupts are fanned out into each active child
-- ledger during admission, so every Workflow owns an independently processable
-- interrupt receipt.
-- name: FirstUnprocessedThreadInterruptAfter :one
SELECT event.*
FROM events AS event
WHERE event.session_id = @session_id
  AND event.thread_id = @thread_id
  AND event.seq > @after_seq
  AND event.type = 'user.interrupt'
  AND event.processed_at IS NULL
ORDER BY event.seq
LIMIT 1;

-- PriorProcessedModelTriggers returns processed events that can drive a model
-- turn before a given sequence, in receipt order. Resolution events are included
-- because a completed custom-tool/confirmation resume may itself have committed
-- model output that later turns must replay.
-- name: PriorProcessedModelTriggers :many
SELECT event.*
FROM events AS event
WHERE event.session_id = @session_id
  AND event.thread_id = (
      SELECT thread.id FROM session_threads AS thread
      WHERE thread.session_id = @session_id AND thread.kind = 'primary'
  )
  AND event.type IN ('user.message', 'user.custom_tool_result', 'user.tool_confirmation')
  AND event.processed_at IS NOT NULL
  AND event.seq < @before_seq
ORDER BY event.seq;

-- CountUnprocessedUserMessages counts primary model triggers still awaiting a turn,
-- excluding one id (the trigger just processed in the same transaction). It lets
-- CompleteTurn decide whether this turn is the last: only then does the session
-- go idle; otherwise it stays running with no intermediate idle event.
-- name: CountUnprocessedUserMessages :one
SELECT COUNT(*)::int AS n
FROM events AS event
WHERE event.session_id = @session_id
  AND event.thread_id = (
      SELECT thread.id FROM session_threads AS thread
      WHERE thread.session_id = @session_id AND thread.kind = 'primary'
  )
  AND event.type IN ('user.message', 'agent.thread_message_received')
  AND event.processed_at IS NULL
  AND event.id <> @exclude_id;

-- name: CountUnprocessedThreadMessages :one
SELECT COUNT(*)::int AS n
FROM events AS event
WHERE event.session_id = @session_id
  AND event.thread_id = @thread_id
  AND event.type = 'agent.thread_message_received'
  AND event.processed_at IS NULL
  AND event.id <> @exclude_id;

-- MarkEventProcessed stamps a trigger event processed, but only once
-- (COALESCE keeps the first timestamp). Returns the row so the caller can tell a
-- first processing from a repeat.
-- name: MarkEventProcessed :exec
UPDATE events
SET processed_at = COALESCE(processed_at, @processed_at)
WHERE session_id = @session_id AND id = @id;

-- FlushQueuedUserMessages stamps every other queued user.message, plus its
-- optional companion system.message, when the active turn exhausts its retry
-- budget. The events remain in the ledger but can no longer start model work.
-- name: FlushQueuedUserMessages :exec
WITH queued AS (
    SELECT queued_event.id,
           queued_event.payload->>'__companion_system_event_id' AS companion_id
    FROM events AS queued_event
    WHERE queued_event.session_id = @session_id
      AND queued_event.thread_id = (
          SELECT thread.id FROM session_threads AS thread
          WHERE thread.session_id = @session_id AND thread.kind = 'primary'
      )
      AND queued_event.type IN ('user.message', 'agent.thread_message_received')
      AND queued_event.processed_at IS NULL
      AND queued_event.id <> @exclude_id
), flushed_ids AS (
    SELECT id FROM queued
    UNION
    SELECT companion_id FROM queued WHERE companion_id IS NOT NULL
)
UPDATE events AS target
SET processed_at = COALESCE(target.processed_at, @processed_at)
WHERE target.session_id = @session_id
  AND target.id IN (SELECT id FROM flushed_ids);

-- FlushQueuedThreadMessages stamps follow-up messages that must not execute
-- after a child turn exhausts its retry budget.
-- name: FlushQueuedThreadMessages :exec
UPDATE events
SET processed_at = COALESCE(processed_at, @processed_at)
WHERE session_id = @session_id
  AND thread_id = @thread_id
  AND type = 'agent.thread_message_received'
  AND processed_at IS NULL
  AND id <> @exclude_id;

-- UpsertOutbox writes or coalesces the pending wakeup for a session. When a
-- wakeup is already pending, it keeps the newer enqueue time and raises
-- max_event_seq to the highest known receipt sequence rather than adding a row.
-- name: UpsertOutbox :exec
INSERT INTO orchestration_outbox (session_id, max_event_seq, enqueued_at)
VALUES (@session_id, @max_event_seq, @enqueued_at)
ON CONFLICT (session_id) DO UPDATE
SET max_event_seq = GREATEST(orchestration_outbox.max_event_seq, EXCLUDED.max_event_seq),
    enqueued_at   = EXCLUDED.enqueued_at;

-- UpsertThreadOutbox is the independent coalescible wakeup for one child
-- Thread Workflow. Session-wide sequence numbers still preserve causal order.
-- name: UpsertThreadOutbox :exec
INSERT INTO thread_orchestration_outbox (
    session_id, thread_id, max_event_seq, enqueued_at, intent
)
VALUES (@session_id, @thread_id, @max_event_seq, @enqueued_at, 'wake')
ON CONFLICT (session_id, thread_id) DO UPDATE
SET max_event_seq = GREATEST(
        thread_orchestration_outbox.max_event_seq,
        EXCLUDED.max_event_seq
    ),
    enqueued_at = EXCLUDED.enqueued_at,
    intent = CASE
        WHEN thread_orchestration_outbox.intent = 'terminate' THEN 'terminate'
        ELSE EXCLUDED.intent
    END;

-- UpsertThreadTermination permanently upgrades one child orchestration key to
-- shutdown. A later stale wake may raise the observed sequence but cannot
-- downgrade the intent.
-- name: UpsertThreadTermination :exec
INSERT INTO thread_orchestration_outbox (
    session_id, thread_id, max_event_seq, enqueued_at, intent
)
VALUES (@session_id, @thread_id, @max_event_seq, @enqueued_at, 'terminate')
ON CONFLICT (session_id, thread_id) DO UPDATE
SET max_event_seq = GREATEST(
        thread_orchestration_outbox.max_event_seq,
        EXCLUDED.max_event_seq
    ),
    enqueued_at = EXCLUDED.enqueued_at,
    intent = 'terminate';

-- EnqueueEnvironmentWork writes the self-hosted worker activation in the same
-- transaction as event admission and the Temporal wakeup. The partial unique
-- index coalesces admissions while a worker already owns the Session.
-- name: EnqueueEnvironmentWork :exec
INSERT INTO environment_work (
    id, environment_id, session_id, activation_seq, state, metadata, created_at
)
VALUES (@id, @environment_id, @session_id, @activation_seq, 'queued', '{}'::jsonb, @created_at)
ON CONFLICT (session_id) WHERE state IN ('queued', 'starting', 'active') DO NOTHING;

-- name: GetOutbox :one
SELECT session_id, max_event_seq, enqueued_at, attempts, last_attempt_at, last_error
FROM orchestration_outbox
WHERE session_id = @session_id;

-- ListOutboxBatch reads the oldest pending wakeups for delivery, oldest first.
-- This is a plain read, not a lease/claim: delivery to Temporal happens outside
-- any transaction, so two relay instances can read the same row and both send a
-- Signal. That is deliberately harmless — delivery is at-least-once and the
-- SessionWorkflow deduplicates by receipt sequence (a wakeup at or below its
-- cursor is a no-op). A delivered row is removed only by DeleteOutboxIfSeq, which
-- is itself guarded by the sequence.
-- name: ListOutboxBatch :many
SELECT session_id, max_event_seq, enqueued_at, attempts, last_attempt_at, last_error
FROM orchestration_outbox
ORDER BY enqueued_at
LIMIT @row_limit;

-- ListOrchestrationWakeups merges primary and child delivery queues without
-- changing either queue's idempotency key.
-- name: ListOrchestrationWakeups :many
SELECT session_id, ''::text AS thread_id, max_event_seq, enqueued_at, attempts,
       'wake'::text AS intent
FROM orchestration_outbox
UNION ALL
SELECT session_id, thread_id, max_event_seq, enqueued_at, attempts, intent
FROM thread_orchestration_outbox
ORDER BY enqueued_at
LIMIT @row_limit;

-- DeleteOutboxIfSeq removes a delivered wakeup, but only if no later admission
-- raised its sequence since it was read. A mismatch means new work coalesced
-- into the row after the relay signaled, so the row is left for the next cycle
-- and re-delivered with the higher sequence (a harmless duplicate wakeup).
-- name: DeleteOutboxIfSeq :execrows
DELETE FROM orchestration_outbox
WHERE session_id = @session_id AND max_event_seq = @max_event_seq;

-- name: DeleteThreadOutboxIfUnchanged :execrows
DELETE FROM thread_orchestration_outbox
WHERE session_id = @session_id
  AND thread_id = @thread_id
  AND max_event_seq = @max_event_seq
  AND intent = @intent;

-- MarkOutboxAttempt records a failed delivery attempt for backoff and
-- observability without removing the wakeup.
-- name: MarkOutboxAttempt :exec
UPDATE orchestration_outbox
SET attempts = attempts + 1, last_attempt_at = @last_attempt_at, last_error = @last_error
WHERE session_id = @session_id;

-- name: MarkThreadOutboxAttempt :exec
UPDATE thread_orchestration_outbox
SET attempts = attempts + 1,
    last_attempt_at = @last_attempt_at,
    last_error = @last_error
WHERE session_id = @session_id AND thread_id = @thread_id;
