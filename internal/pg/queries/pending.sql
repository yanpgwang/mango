-- Typed queries for durable client-action gates on the Temporal/PostgreSQL
-- path. Callers hold the session row lock, so park, admission claim, and resume
-- completion serialize with the public event ledger and session projection.

-- name: InsertPendingAction :exec
INSERT INTO pending_actions (
    id, session_id, thread_id, action_event_id, client_action_event_id,
    kind, resolving_event_id, created_at, resolved_at
)
VALUES (
    @id, @session_id, @thread_id, @action_event_id, @client_action_event_id,
    @kind, NULL, @created_at, NULL
);

-- name: GetPendingActionForUpdate :one
SELECT id, session_id, thread_id, action_event_id, client_action_event_id,
       kind, approval_event_id, resolving_event_id, created_at, resolved_at
FROM pending_actions
WHERE session_id = @session_id
  AND (
      action_event_id = @referenced_event_id
      OR client_action_event_id = @referenced_event_id
  )
FOR UPDATE;

-- name: ApproveExternalPendingAction :execrows
UPDATE pending_actions
SET kind = 'tool_result', approval_event_id = @approval_event_id
WHERE session_id = @session_id AND id = @id
  AND kind = 'tool_confirmation'
  AND approval_event_id IS NULL
  AND resolving_event_id IS NULL AND resolved_at IS NULL;

-- name: ClaimPendingAction :execrows
UPDATE pending_actions
SET resolving_event_id = @resolving_event_id
WHERE session_id = @session_id
  AND id = @id
  AND resolving_event_id IS NULL
  AND resolved_at IS NULL;

-- name: ResolvePendingActionsForTrigger :execrows
UPDATE pending_actions
SET resolved_at = @resolved_at
WHERE session_id = @session_id
  AND resolving_event_id = @resolving_event_id
  AND resolved_at IS NULL;

-- ResolvePendingActionsForEvents closes one complete client-action barrier.
-- The caller validates that the supplied ids exactly match every unresolved
-- row before executing this update under the session lock.
-- name: ResolvePendingActionsForEvents :execrows
UPDATE pending_actions
SET resolved_at = @resolved_at
WHERE session_id = @session_id
  AND thread_id = @thread_id
  AND resolving_event_id = ANY(@resolving_event_ids::text[])
  AND resolved_at IS NULL;

-- name: HasUnresolvedPendingActions :one
SELECT EXISTS(
    SELECT 1
    FROM pending_actions
    WHERE session_id = @session_id
      AND thread_id = @thread_id
      AND resolved_at IS NULL
) AS unresolved;

-- name: HasUnclaimedPendingActions :one
SELECT EXISTS(
    SELECT 1
    FROM pending_actions
    WHERE session_id = @session_id
      AND thread_id = @thread_id
      AND resolved_at IS NULL
      AND resolving_event_id IS NULL
) AS unclaimed;

-- IsUnresolvedPendingResolution distinguishes a processed-on-receipt client
-- result that still has to drive its resume turn from an already completed
-- trigger. user.custom_tool_result carries processed_at at admission by public
-- contract, so processed_at alone cannot be the turn-completion idempotency key.
-- name: IsUnresolvedPendingResolution :one
SELECT EXISTS(
    SELECT 1
    FROM pending_actions
    WHERE session_id = @session_id
      AND thread_id = @thread_id
      AND resolving_event_id = @resolving_event_id
      AND resolved_at IS NULL
) AS pending_resume;

-- name: ListUnresolvedPendingActions :many
SELECT id, session_id, thread_id, action_event_id, client_action_event_id,
       kind, approval_event_id, resolving_event_id, created_at, resolved_at
FROM pending_actions
WHERE session_id = @session_id
  AND thread_id = @thread_id
  AND resolved_at IS NULL
ORDER BY created_at, id;

-- SessionPendingClientActionEventIDs projects the unresolved action ids a
-- client can answer from the primary stream. Child rows expose their cross-post
-- ids; primary rows use the same id for both columns.
-- name: SessionPendingClientActionEventIDs :many
SELECT client_action_event_id
FROM pending_actions
WHERE session_id = @session_id AND resolved_at IS NULL
ORDER BY created_at, id;
