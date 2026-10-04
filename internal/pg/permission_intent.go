package pg

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/yanpgwang/mango/internal/domain"
)

const (
	permissionIntentMaxBytes   = 64 * 1024
	permissionIntentMaxEvents  = 1000
	permissionIntentMaxOrigins = 32
)

// PermissionIntentThrough reads original authenticated client intent, not a
// Messages role or compacted summary. Thread words remain evidence; private
// server references identify the client task that causally started their work.
func (s *Store) PermissionIntentThrough(ctx context.Context, sessionID, triggerEventID string) (domain.PermissionIntent, error) {
	trigger, err := s.GetEvent(ctx, sessionID, triggerEventID)
	if err != nil {
		return domain.PermissionIntent{}, err
	}
	executionThread, err := s.GetSessionThread(ctx, sessionID, trigger.ThreadID)
	if err != nil {
		return domain.PermissionIntent{}, err
	}
	intent := domain.PermissionIntent{}
	if executionThread.Agent.System != nil {
		intent.AgentSystem = *executionThread.Agent.System
	}
	root := trigger
	var companions []domain.PermissionIntentEntry
	seen := make(map[string]bool)
	causalBoundaries := make(map[string]int64)
	for depth := 0; ; depth++ {
		if depth >= permissionIntentMaxOrigins || seen[root.ID] {
			return intent, nil
		}
		seen[root.ID] = true
		if root.Sequence > causalBoundaries[root.ThreadID] {
			causalBoundaries[root.ThreadID] = root.Sequence
		}
		if root.Type == domain.EvUserMessage || root.Type == domain.EvUserDefineOutcome {
			break
		}
		var origin string
		switch root.Type {
		case domain.EvAgentThreadMessageReceived:
			origin, _ = root.Payload[domain.InternalOriginTriggerEventID].(string)
		case domain.EvUserToolResult, domain.EvUserCustomToolResult, domain.EvUserToolConfirmation:
			err := s.pool.QueryRow(ctx, `
SELECT COALESCE(action_event.turn_event_id,'') FROM pending_actions AS action
JOIN events AS action_event ON action_event.session_id=action.session_id AND action_event.id=action.action_event_id
WHERE action.session_id=$1 AND action.thread_id=$2
  AND (action.resolving_event_id=$3 OR action.approval_event_id=$3)`, sessionID, root.ThreadID, root.ID).Scan(&origin)
			if errors.Is(err, pgx.ErrNoRows) {
				return intent, nil
			}
			if err != nil {
				return intent, err
			}
			entries, complete, err := s.permissionBarrierCompanions(ctx, sessionID, root.ThreadID, origin)
			if err != nil || !complete {
				return intent, err
			}
			companions = append(companions, entries...)
		default:
			return intent, nil
		}
		if origin == "" {
			return intent, nil
		}
		prior, err := s.GetEvent(ctx, sessionID, origin)
		if err != nil {
			var domainErr *domain.DomainError
			if errors.As(err, &domainErr) && domainErr.Kind == domain.KindNotFound {
				return intent, nil
			}
			return intent, err
		}
		if prior.Sequence >= root.Sequence {
			return intent, nil
		}
		if root.Type == domain.EvAgentThreadMessageReceived {
			from, _ := root.Payload["from_session_thread_id"].(string)
			if from == "" || from != prior.ThreadID {
				return intent, nil
			}
			if executionThread.ParentThreadID != nil && root.ThreadID == executionThread.ID {
				// A child receives the parent's captured intent, not a query of
				// mutable processed flags after delegation. Primary report turns
				// still collect their own processed client changes below.
				raw, err := json.Marshal(root.Payload[domain.InternalOriginPermissionIntent])
				if err != nil {
					return intent, err
				}
				var snapshot struct {
					TriggerEventID string                  `json:"trigger_event_id"`
					Intent         domain.PermissionIntent `json:"intent"`
				}
				if err := json.Unmarshal(raw, &snapshot); err != nil || snapshot.TriggerEventID != prior.ID || !snapshot.Intent.Complete {
					return intent, nil
				}
				original := snapshot.Intent
				intent.Entries = append(intent.Entries, original.Entries...)
				if original.AgentSystem != "" {
					intent.Entries = append(intent.Entries, domain.PermissionIntentEntry{EventID: prior.ID, ThreadID: prior.ThreadID, Type: "agent.system", Text: original.AgentSystem})
				}
				intent.Entries = append(intent.Entries, companions...)
				return s.finishPermissionIntent(ctx, sessionID, intent)
			}
		}
		root = prior
	}
	if root.ThreadID != trigger.ThreadID {
		originThread, err := s.GetSessionThread(ctx, sessionID, root.ThreadID)
		if err != nil {
			return intent, err
		}
		if originThread.Agent.System != nil {
			intent.Entries = append(intent.Entries, domain.PermissionIntentEntry{EventID: root.ID, ThreadID: root.ThreadID, Type: "agent.system", Text: *originThread.Agent.System})
		}
	}
	// Include processed client changes through the latest validated ancestor
	// in their Thread. This carries restrictions across report→followup
	// delegation without granting authority from queued or later messages.
	throughSequence := causalBoundaries[root.ThreadID]
	// Bound original input before fetching potentially large client payloads.
	// Unprocessed queued messages are excluded except for the exact root task.
	const eligible = `FROM events AS client WHERE session_id=$1 AND thread_id=$2 AND seq <= $3
 AND (id=$4 OR (processed_at IS NOT NULL AND turn_event_id IS NULL
   AND (type IN ('user.message','user.tool_result','user.custom_tool_result','user.tool_confirmation')
    OR (type='user.define_outcome' AND EXISTS (
     SELECT 1 FROM events AS completion WHERE completion.session_id=client.session_id
      AND completion.thread_id=client.thread_id AND completion.seq <= $3
      AND completion.type='span.outcome_evaluation_end'
      AND completion.payload->>'outcome_id'=client.payload->>'outcome_id'
      AND completion.payload->>'result' IN ('satisfied','max_iterations_reached','failed','interrupted'))))))`
	var count int
	var bytes int64
	err = s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(octet_length(body::text)),0)::bigint FROM (
 SELECT jsonb_build_object('content',CASE WHEN type='user.message' THEN payload->'content' END,
 'description',CASE WHEN type='user.define_outcome' THEN payload->'description' END,
 'companion',payload->'__companion_system_content') AS body `+eligible+`
 ORDER BY seq LIMIT 1001) AS bounded`, sessionID, root.ThreadID, throughSequence, root.ID).Scan(&count, &bytes)
	if err != nil {
		return intent, err
	}
	if count > permissionIntentMaxEvents || bytes > permissionIntentMaxBytes {
		return intent, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, type,
 jsonb_build_object('content',CASE WHEN type='user.message' THEN payload->'content' END,'description',payload->'description',
 '__companion_system_event_id',payload->'__companion_system_event_id',
 '__companion_system_content',payload->'__companion_system_content') `+eligible+` ORDER BY seq LIMIT 1001`, sessionID, root.ThreadID, throughSequence, root.ID)
	if err != nil {
		return intent, err
	}
	defer rows.Close()
	fetched := 0
	for rows.Next() {
		fetched++
		if fetched > permissionIntentMaxEvents {
			return intent, nil
		}
		event := domain.Event{SessionID: sessionID, ThreadID: root.ThreadID}
		var payload []byte
		if err := rows.Scan(&event.ID, &event.Type, &payload); err != nil {
			return intent, err
		}
		if err := json.Unmarshal(payload, &event.Payload); err != nil {
			return intent, err
		}
		var text string
		switch event.Type {
		case domain.EvUserMessage:
			text = permissionClientText(event.Payload["content"])
		case domain.EvUserDefineOutcome:
			text, _ = event.Payload["description"].(string)
		}
		if text != "" {
			intent.Entries = append(intent.Entries, domain.PermissionIntentEntry{EventID: event.ID, ThreadID: event.ThreadID, Type: event.Type, Text: text})
		}
		intent.Entries = append(intent.Entries, permissionCompanion(event)...)
	}
	if err := rows.Err(); err != nil {
		return intent, err
	}
	rows.Close()
	// Resolution companions are later than the originating client task. Reverse
	// ancestry traversal order to preserve their causal instruction ordering.
	for i := len(companions) - 1; i >= 0; i-- {
		intent.Entries = append(intent.Entries, companions[i])
	}
	return s.finishPermissionIntent(ctx, sessionID, intent)
}

func (s *Store) finishPermissionIntent(ctx context.Context, sessionID string, intent domain.PermissionIntent) (domain.PermissionIntent, error) {
	entries, complete, err := s.orderPermissionIntentEntries(ctx, sessionID, intent.Entries)
	if err != nil || !complete {
		return domain.PermissionIntent{AgentSystem: intent.AgentSystem}, err
	}
	intent.Entries = entries
	intent.Complete = true
	body, err := json.Marshal(intent)
	if err != nil {
		return intent, err
	}
	if len(body) > permissionIntentMaxBytes {
		return domain.PermissionIntent{AgentSystem: intent.AgentSystem}, nil
	}
	return intent, nil
}

func permissionCompanion(event domain.Event) []domain.PermissionIntentEntry {
	id, _ := event.Payload[domain.InternalCompanionSystemEventID].(string)
	text := permissionClientText(event.Payload[domain.InternalCompanionSystemContent])
	if id == "" || text == "" {
		return nil
	}
	return []domain.PermissionIntentEntry{{EventID: id, ThreadID: event.ThreadID, Type: domain.EvSystemMessage, Text: text}}
}

func permissionClientText(raw any) string {
	if text, ok := raw.(string); ok {
		return text
	}
	var texts []string
	if items, ok := raw.([]any); ok {
		for _, item := range items {
			if block, ok := item.(map[string]any); ok && block["type"] == "text" {
				if text, ok := block["text"].(string); ok {
					texts = append(texts, text)
				}
			}
		}
	}
	return strings.Join(texts, "\n")
}

// A resumed turn consumes the whole causal action barrier, not just whichever
// resolution was selected as its trigger. Companion instructions are client
// intent; result bodies and agent-created tool content remain untrusted data.
func (s *Store) permissionBarrierCompanions(ctx context.Context, sessionID, threadID, originID string) ([]domain.PermissionIntentEntry, bool, error) {
	const eligible = `FROM events AS event
JOIN pending_actions AS action ON action.session_id=event.session_id
 AND (event.id=action.resolving_event_id OR event.id=action.approval_event_id)
JOIN events AS action_event ON action_event.session_id=action.session_id AND action_event.id=action.action_event_id
WHERE action.session_id=$1 AND action.thread_id=$2 AND action_event.turn_event_id=$3`
	const projection = `jsonb_build_object('__companion_system_event_id',event.payload->'__companion_system_event_id',
 '__companion_system_content',event.payload->'__companion_system_content')`
	var count int
	var bytes int64
	err := s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(octet_length(body::text)),0)::bigint FROM (
 SELECT `+projection+` AS body `+eligible+` ORDER BY event.seq DESC LIMIT 1001) AS bounded`, sessionID, threadID, originID).Scan(&count, &bytes)
	if err != nil || count > permissionIntentMaxEvents || bytes > permissionIntentMaxBytes {
		return nil, false, err
	}
	rows, err := s.pool.Query(ctx, `SELECT event.id, event.thread_id, `+projection+` `+eligible+` ORDER BY event.seq DESC LIMIT 1001`, sessionID, threadID, originID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var entries []domain.PermissionIntentEntry
	fetched := 0
	bytes = 0
	for rows.Next() {
		fetched++
		var event domain.Event
		var payload []byte
		if err := rows.Scan(&event.ID, &event.ThreadID, &payload); err != nil {
			return nil, false, err
		}
		bytes += int64(len(payload))
		if fetched > permissionIntentMaxEvents || bytes > permissionIntentMaxBytes || event.ThreadID != threadID {
			return nil, false, nil
		}
		if err := json.Unmarshal(payload, &event.Payload); err != nil {
			return nil, false, err
		}
		entries = append(entries, permissionCompanion(event)...)
	}
	return entries, true, rows.Err()
}

// Merge historical and causal-barrier entries by their actual event order.
// Validate private companion references instead of asserting complete intent
// for a missing or cross-Thread instruction, and deduplicate overlapping reads.
func (s *Store) orderPermissionIntentEntries(ctx context.Context, sessionID string, entries []domain.PermissionIntentEntry) ([]domain.PermissionIntentEntry, bool, error) {
	unique := make([]domain.PermissionIntentEntry, 0, len(entries))
	seen := make(map[string]bool)
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		key := entry.EventID + "\x00" + entry.Type
		if !seen[key] {
			unique = append(unique, entry)
			ids = append(ids, entry.EventID)
			seen[key] = true
		}
	}
	if len(unique) > permissionIntentMaxEvents {
		return nil, false, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, thread_id, seq FROM events WHERE session_id=$1 AND id=ANY($2::text[])`, sessionID, ids)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	sequences := make(map[string]int64)
	threads := make(map[string]string)
	for rows.Next() {
		var id, thread string
		var sequence int64
		if err := rows.Scan(&id, &thread, &sequence); err != nil {
			return nil, false, err
		}
		sequences[id], threads[id] = sequence, thread
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	for _, entry := range unique {
		if _, ok := sequences[entry.EventID]; !ok || threads[entry.EventID] != entry.ThreadID {
			return nil, false, nil
		}
	}
	sort.SliceStable(unique, func(i, j int) bool { return sequences[unique[i].EventID] < sequences[unique[j].EventID] })
	return unique, true, nil
}
