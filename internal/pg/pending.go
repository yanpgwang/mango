package pg

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

type routedClientDraft struct {
	Draft     domain.EventDraft
	ThreadID  string
	Submitted bool
}

type interruptWakeTargets struct {
	Primary  bool
	Children map[string]struct{}
}

func newInterruptWakeTargets() interruptWakeTargets {
	return interruptWakeTargets{Children: make(map[string]struct{})}
}

func (t *interruptWakeTargets) add(thread domain.SessionThread) {
	if thread.ParentThreadID == nil {
		t.Primary = true
		return
	}
	t.Children[thread.ID] = struct{}{}
}

// routeClientDraftsLocked resolves client-action references before persistence.
// The client normally answers the cross-posted primary event id; PostgreSQL
// maps it back to the owning Thread and enriches the persisted result with the
// documented session_thread_id when the caller omitted the redundant hint.
func (s *Store) routeClientDraftsLocked(
	ctx context.Context,
	tx pgx.Tx,
	q *pgstore.Queries,
	sessionID string,
	primaryThreadID string,
	drafts []domain.EventDraft,
) ([]routedClientDraft, map[string]struct{}, interruptWakeTargets, error) {
	routed := make([]routedClientDraft, 0, len(drafts))
	resolutionThreads := make(map[string]struct{})
	interruptTargets := newInterruptWakeTargets()
	var globalChildren []string
	globalChildrenLoaded := false
	companionThreadID := ""
	companionEventID := ""
	for _, draft := range drafts {
		copies := make([]routedClientDraft, 0)
		targetThreadID := primaryThreadID
		if draft.Type == domain.EvSystemMessage && draft.ID != "" &&
			draft.ID == companionEventID {
			targetThreadID = companionThreadID
		}

		if referencedID, _, ok := domain.ResolutionReference(
			draft.Type, draft.Payload,
		); ok {
			row, err := q.GetPendingActionForUpdate(
				ctx,
				pgstore.GetPendingActionForUpdateParams{
					SessionID: sessionID, ReferencedEventID: referencedID,
				},
			)
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, nil, interruptWakeTargets{}, domain.Validation(
					"resolution references unknown pending action",
				)
			}
			if err != nil {
				return nil, nil, interruptWakeTargets{}, err
			}
			// Validate response kinds while claiming in request order below. An
			// earlier allow in this atomic batch may enable a later tool result.
			if hinted, _ := draft.Payload["session_thread_id"].(string); hinted != "" &&
				hinted != row.ThreadID {
				return nil, nil, interruptWakeTargets{}, domain.Validation(
					"session_thread_id does not match the pending action",
				)
			}
			targetThreadID = row.ThreadID
			payload := make(map[string]any, len(draft.Payload)+1)
			for key, value := range draft.Payload {
				payload[key] = value
			}
			if targetThreadID != primaryThreadID {
				payload["session_thread_id"] = targetThreadID
			}
			draft.Payload = payload
			resolutionThreads[targetThreadID] = struct{}{}
		}

		if draft.Type == domain.EvUserInterrupt {
			hinted, targeted := draft.Payload["session_thread_id"]
			if targeted {
				threadID, _ := hinted.(string)
				if threadID == "" {
					return nil, nil, interruptWakeTargets{}, domain.Validation(
						"session_thread_id must name a Session Thread",
					)
				}
				thread, err := loadSessionThreadForUpdate(
					ctx, tx, sessionID, threadID,
				)
				if err != nil {
					var domainErr *domain.DomainError
					if errors.As(err, &domainErr) && domainErr.Kind == domain.KindNotFound {
						return nil, nil, interruptWakeTargets{}, domain.Validation(
							"session_thread_id does not name a Session Thread in this Session",
						)
					}
					return nil, nil, interruptWakeTargets{}, err
				}
				if thread.ArchivedAt != nil || thread.Status == domain.StatusTerminated {
					return nil, nil, interruptWakeTargets{}, domain.Conflict(
						"cannot interrupt an archived or terminated Session Thread",
					)
				}
				targetThreadID = thread.ID
				interruptTargets.add(thread)
			} else {
				interruptTargets.Primary = true
				if !globalChildrenLoaded {
					rows, err := tx.Query(ctx, `
SELECT id
FROM session_threads
WHERE session_id = $1
  AND kind = 'child'
  AND archived_at IS NULL
  AND status <> 'terminated'
ORDER BY created_at, id`, sessionID)
					if err != nil {
						return nil, nil, interruptWakeTargets{}, err
					}
					for rows.Next() {
						var threadID string
						if err := rows.Scan(&threadID); err != nil {
							rows.Close()
							return nil, nil, interruptWakeTargets{}, err
						}
						globalChildren = append(globalChildren, threadID)
					}
					if err := rows.Err(); err != nil {
						rows.Close()
						return nil, nil, interruptWakeTargets{}, err
					}
					rows.Close()
					globalChildrenLoaded = true
				}
				for _, threadID := range globalChildren {
					copyDraft := draft
					copyDraft.ID = ""
					copyDraft.Payload = cloneEventPayload(draft.Payload)
					copies = append(copies, routedClientDraft{
						Draft: copyDraft, ThreadID: threadID,
					})
					interruptTargets.Children[threadID] = struct{}{}
				}
			}
		}

		if companionID, _ := draft.Payload[domain.InternalCompanionSystemEventID].(string); companionID != "" {
			companionEventID = companionID
			companionThreadID = targetThreadID
		}
		routed = append(routed, routedClientDraft{
			Draft: draft, ThreadID: targetThreadID, Submitted: true,
		})
		routed = append(routed, copies...)
	}
	return routed, resolutionThreads, interruptTargets, nil
}

// UnresolvedPendingActions returns every client action that still gates the
// primary Thread, including an action whose matching resolution has been
// admitted but whose resume turn has not completed. PostgreSQL is the source of
// truth for this wait state; Temporal wakeups carry only event-sequence
// metadata.
func (s *Store) UnresolvedPendingActions(
	ctx context.Context,
	sessionID string,
) ([]domain.PendingAction, error) {
	threadID, err := s.q.GetPrimarySessionThreadID(ctx, sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.NotFound("primary session Thread not found")
	}
	if err != nil {
		return nil, err
	}
	return s.UnresolvedThreadPendingActions(ctx, sessionID, threadID)
}

// UnresolvedThreadPendingActions returns the independent requires-action
// barrier owned by one Thread. A child waiting for client input must not gate
// the primary or any sibling Thread.
func (s *Store) UnresolvedThreadPendingActions(
	ctx context.Context,
	sessionID string,
	threadID string,
) ([]domain.PendingAction, error) {
	rows, err := s.q.ListUnresolvedPendingActions(
		ctx,
		pgstore.ListUnresolvedPendingActionsParams{
			SessionID: sessionID,
			ThreadID:  threadID,
		},
	)
	if err != nil {
		return nil, err
	}
	out := make([]domain.PendingAction, 0, len(rows))
	for _, row := range rows {
		out = append(out, domain.PendingAction{
			ID: row.ID, SessionID: row.SessionID, ThreadID: row.ThreadID,
			ActionEventID:       row.ActionEventID,
			ClientActionEventID: row.ClientActionEventID,
			Kind:                domain.PendingActionKind(row.Kind),
			ApprovalEventID:     row.ApprovalEventID,
			ResolvingEventID:    row.ResolvingEventID,
			CreatedAt:           row.CreatedAt.Time.UTC(), ResolvedAt: timePtr(row.ResolvedAt),
		})
	}
	return out, nil
}

// validatePendingCompletion requires the internal completion contract to match
// the public requires_action boundary. The pending ids are never accepted as an
// independent caller assertion: they must exactly match the ids in the
// owning Thread's status_idle draft committed by the same transaction.
func validatePendingCompletion(
	status domain.Status,
	drafts []domain.EventDraft,
	pendingActionEventIDs []string,
) error {
	if len(pendingActionEventIDs) == 0 {
		return nil
	}
	if status != domain.StatusIdle {
		return domain.Validation("a pending-action turn must complete idle")
	}

	expected := make(map[string]struct{}, len(pendingActionEventIDs))
	for _, id := range pendingActionEventIDs {
		if id == "" {
			return domain.Validation("pending action event id is required")
		}
		if _, duplicate := expected[id]; duplicate {
			return domain.Validation("duplicate pending action event id")
		}
		expected[id] = struct{}{}
	}

	var required []string
	for _, draft := range drafts {
		if draft.Type != domain.EvSessionStatusIdle &&
			draft.Type != domain.EvSessionThreadStatusIdle {
			continue
		}
		stopReason, _ := draft.Payload["stop_reason"].(map[string]any)
		if stopReason["type"] != "requires_action" {
			continue
		}
		var ok bool
		required, ok = stringList(stopReason["event_ids"])
		if !ok {
			return domain.Validation("requires_action event_ids must be a string array")
		}
		break
	}
	if required == nil {
		return domain.Validation("pending actions require a status_idle event with requires_action")
	}
	if len(required) != len(expected) {
		return domain.Validation("requires_action event_ids must match pending action ids")
	}
	seen := make(map[string]struct{}, len(required))
	for _, id := range required {
		if _, duplicate := seen[id]; duplicate {
			return domain.Validation("requires_action contains a duplicate event id")
		}
		seen[id] = struct{}{}
		if _, ok := expected[id]; !ok {
			return domain.Validation("requires_action event_ids must match pending action ids")
		}
	}
	return nil
}

func stringList(value any) ([]string, bool) {
	switch values := value.(type) {
	case []string:
		return append([]string(nil), values...), true
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			item, ok := value.(string)
			if !ok || item == "" {
				return nil, false
			}
			out = append(out, item)
		}
		return out, true
	default:
		return nil, false
	}
}

// insertPendingActionsLocked persists the gate rows for action events committed
// by this completion. allowed contains only the events appended by the current
// turn, preventing a stale same-session event id from being reused as a new
// park.
func (s *Store) insertPendingActionsLocked(
	ctx context.Context,
	q *pgstore.Queries,
	sessionID string,
	threadID string,
	pendingActionEventIDs []string,
	allowed map[string]domain.Event,
	clientActionEventIDs map[string]string,
) error {
	for _, actionEventID := range pendingActionEventIDs {
		event, ok := allowed[actionEventID]
		if !ok {
			return domain.Validation(
				"pending action must reference an action event committed by this turn",
			)
		}
		kind, ok := domain.PendingActionKindForEvent(event.Type, event.Payload)
		if !ok {
			return domain.Validation("event cannot park a pending action")
		}
		clientActionEventID := actionEventID
		if projected := clientActionEventIDs[actionEventID]; projected != "" {
			clientActionEventID = projected
		}
		if err := q.InsertPendingAction(ctx, pgstore.InsertPendingActionParams{
			ID: s.ids.NewID(domain.PrefixPendingAction), SessionID: sessionID,
			ThreadID: threadID, ActionEventID: actionEventID,
			ClientActionEventID: clientActionEventID, Kind: string(kind),
			CreatedAt: tsUTC(s.clock.Now().UTC()),
		}); err != nil {
			return err
		}
	}
	return nil
}

// claimPendingResolutionsLocked validates and claims every resolution in an
// admitted batch. A claimed row remains unresolved until its resume turn closes,
// which keeps ordinary queued work behind the durable wait boundary.
func (s *Store) claimPendingResolutionsLocked(
	ctx context.Context,
	q *pgstore.Queries,
	sessionID string,
	events []domain.Event,
) (bool, error) {
	claimed := false
	for index := range events {
		event := events[index]
		actionEventID, kind, ok := domain.ResolutionReference(event.Type, event.Payload)
		if !ok {
			switch event.Type {
			case domain.EvUserCustomToolResult, domain.EvUserToolConfirmation,
				domain.EvUserToolResult:
				return false, domain.Validation("resolution event is missing its action event id")
			default:
				continue
			}
		}

		row, err := q.GetPendingActionForUpdate(ctx, pgstore.GetPendingActionForUpdateParams{
			SessionID: sessionID, ReferencedEventID: actionEventID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return false, domain.Validation("resolution references unknown pending action")
		}
		if err != nil {
			return false, err
		}
		if kind == domain.PendingToolConfirmation && row.ApprovalEventID != nil {
			return false, domain.Conflict("pending action already has an approval")
		}
		if domain.PendingActionKind(row.Kind) != kind {
			return false, domain.Validation("resolution kind does not match the pending action")
		}
		if event.ThreadID != row.ThreadID {
			return false, domain.Validation("resolution was routed to the wrong session Thread")
		}
		if routed, _ := event.Payload["session_thread_id"].(string); routed != "" &&
			routed != row.ThreadID {
			return false, domain.Validation("session_thread_id does not match the pending action")
		}
		if row.ResolvedAt.Valid {
			return false, domain.Conflict("pending action is already resolved")
		}
		if row.ResolvingEventID != nil {
			return false, domain.Conflict("pending action already has a pending resolution")
		}
		if kind == domain.PendingToolConfirmation {
			verdict, _ := event.Payload["result"].(string)
			if verdict != "allow" && verdict != "deny" {
				return false, domain.Validation("tool confirmation must be allow or deny")
			}
			actionRow, err := q.GetEvent(ctx, pgstore.GetEventParams{
				SessionID: sessionID, ID: row.ActionEventID,
			})
			if err != nil {
				return false, err
			}
			action, err := eventFromRow(actionRow)
			if err != nil {
				return false, err
			}
			if verdict == "allow" && domain.IsSelfHostedToolCall(action.Type, action.Payload) {
				if action.Payload["evaluated_permission"] != "ask" {
					return false, domain.Validation("external approval does not reference an ask-gated tool")
				}
				affected, err := q.ApproveExternalPendingAction(ctx, pgstore.ApproveExternalPendingActionParams{
					SessionID: sessionID, ID: row.ID, ApprovalEventID: &event.ID,
				})
				if err != nil {
					return false, err
				}
				if affected != 1 {
					return false, domain.Conflict("pending action already has an approval")
				}
				// Approval is consumed by admission, not by a model turn. The
				// unchanged action remains unclaimed until the external result.
				now := s.clock.Now().UTC()
				if err := q.MarkEventProcessed(ctx, pgstore.MarkEventProcessedParams{
					SessionID: sessionID, ID: event.ID, ProcessedAt: tsUTC(now),
				}); err != nil {
					return false, err
				}
				events[index].ProcessedAt = &now
				continue
			}
		}
		affected, err := q.ClaimPendingAction(ctx, pgstore.ClaimPendingActionParams{
			ResolvingEventID: &event.ID,
			SessionID:        sessionID,
			ID:               row.ID,
		})
		if err != nil {
			return false, err
		}
		if affected != 1 {
			return false, domain.Conflict("pending action already has a pending resolution")
		}
		claimed = true
	}
	return claimed, nil
}

// resolvePendingBarrierLocked atomically closes an entire requires_action
// barrier. The caller-supplied resolution ids are accepted only when they
// exactly match every unresolved row and include the turn trigger. This keeps a
// Workflow from accidentally clearing a partial or unrelated wait.
//
// An empty resolutionEventIDs slice retains the pre-barrier single-trigger
// behavior for existing Workflow histories.
func (s *Store) resolvePendingBarrierLocked(
	ctx context.Context,
	q *pgstore.Queries,
	sessionID string,
	threadID string,
	triggerEventID string,
	resolutionEventIDs []string,
) (bool, error) {
	if len(resolutionEventIDs) == 0 {
		affected, err := q.ResolvePendingActionsForTrigger(
			ctx,
			pgstore.ResolvePendingActionsForTriggerParams{
				ResolvedAt:       tsUTC(s.clock.Now().UTC()),
				SessionID:        sessionID,
				ResolvingEventID: &triggerEventID,
			},
		)
		return affected > 0, err
	}

	expected := make(map[string]struct{}, len(resolutionEventIDs))
	for _, id := range resolutionEventIDs {
		if id == "" {
			return false, domain.Validation("resolution event id is required")
		}
		if _, duplicate := expected[id]; duplicate {
			return false, domain.Validation("duplicate resolution event id")
		}
		expected[id] = struct{}{}
	}
	if _, ok := expected[triggerEventID]; !ok {
		return false, domain.Validation("resume trigger must be part of the resolution barrier")
	}

	rows, err := q.ListUnresolvedPendingActions(
		ctx,
		pgstore.ListUnresolvedPendingActionsParams{
			SessionID: sessionID, ThreadID: threadID,
		},
	)
	if err != nil {
		return false, err
	}
	if len(rows) != len(expected) {
		return false, domain.Validation(
			"resolution event ids must match the complete pending-action barrier",
		)
	}
	// The schema's UNIQUE(session_id, resolving_event_id) constraint makes this
	// membership+cardinality check a bijection between rows and supplied ids.
	for _, row := range rows {
		if row.ResolvingEventID == nil {
			return false, domain.Validation("pending-action barrier is not fully claimed")
		}
		if _, ok := expected[*row.ResolvingEventID]; !ok {
			return false, domain.Validation(
				"resolution event ids must match the complete pending-action barrier",
			)
		}
	}

	affected, err := q.ResolvePendingActionsForEvents(
		ctx,
		pgstore.ResolvePendingActionsForEventsParams{
			ResolvedAt:        tsUTC(s.clock.Now().UTC()),
			SessionID:         sessionID,
			ThreadID:          threadID,
			ResolvingEventIds: resolutionEventIDs,
		},
	)
	if err != nil {
		return false, err
	}
	if affected != int64(len(resolutionEventIDs)) {
		return false, domain.Conflict("pending-action barrier changed during completion")
	}
	return true, nil
}
