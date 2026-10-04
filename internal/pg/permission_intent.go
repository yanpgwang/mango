package pg

import (
	"context"
	"encoding/json"
	"errors"
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
	for depth := 0; ; depth++ {
		if depth >= permissionIntentMaxOrigins || seen[root.ID] {
			return intent, nil
		}
		seen[root.ID] = true
		if root.Type == domain.EvUserMessage || root.Type == domain.EvUserDefineOutcome {
			break
		}
		var origin string
		switch root.Type {
		case domain.EvAgentThreadMessageReceived:
			origin, _ = root.Payload[domain.InternalOriginTriggerEventID].(string)
		case domain.EvUserToolResult, domain.EvUserCustomToolResult, domain.EvUserToolConfirmation:
			companions = append(companions, permissionCompanion(root)...)
			var approvalID string
			var approvalPayload []byte
			err := s.pool.QueryRow(ctx, `
SELECT COALESCE(action_event.turn_event_id,''), COALESCE(approval.id,''), COALESCE(approval.payload,'{}'::jsonb) FROM pending_actions AS action
JOIN events AS action_event ON action_event.session_id=action.session_id AND action_event.id=action.action_event_id
LEFT JOIN events AS approval ON approval.session_id=action.session_id AND approval.id=action.approval_event_id
WHERE action.session_id=$1 AND action.thread_id=$2
  AND (action.resolving_event_id=$3 OR action.approval_event_id=$3)`, sessionID, root.ThreadID, root.ID).Scan(&origin, &approvalID, &approvalPayload)
			if errors.Is(err, pgx.ErrNoRows) {
				return intent, nil
			}
			if err != nil {
				return intent, err
			}
			if approvalID != "" && approvalID != root.ID {
				approval := domain.Event{ID: approvalID, ThreadID: root.ThreadID}
				if err := json.Unmarshal(approvalPayload, &approval.Payload); err != nil {
					return intent, err
				}
				companions = append(companions, permissionCompanion(approval)...)
			}
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
	// Bound original input before fetching potentially large client payloads.
	// Unprocessed queued messages are excluded except for the exact root task.
	const eligible = `FROM events WHERE session_id=$1 AND thread_id=$2 AND seq <= $3
 AND (id=$4 OR (processed_at IS NOT NULL AND turn_event_id IS NULL
   AND type IN ('user.message','user.tool_result','user.custom_tool_result','user.tool_confirmation')))`
	var count int
	var bytes int64
	err = s.pool.QueryRow(ctx, `SELECT count(*), COALESCE(sum(octet_length(body::text)),0)::bigint FROM (
 SELECT jsonb_build_object('content',CASE WHEN type='user.message' THEN payload->'content' END,
 'description',CASE WHEN type='user.define_outcome' THEN payload->'description' END,
 'companion',payload->'__companion_system_content') AS body `+eligible+`
 ORDER BY seq LIMIT 1001) AS bounded`, sessionID, root.ThreadID, root.Sequence, root.ID).Scan(&count, &bytes)
	if err != nil {
		return intent, err
	}
	if count > permissionIntentMaxEvents || bytes > permissionIntentMaxBytes {
		return intent, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT id, type,
 jsonb_build_object('content',CASE WHEN type='user.message' THEN payload->'content' END,'description',payload->'description',
 '__companion_system_event_id',payload->'__companion_system_event_id',
 '__companion_system_content',payload->'__companion_system_content') `+eligible+` ORDER BY seq LIMIT 1001`, sessionID, root.ThreadID, root.Sequence, root.ID)
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
	// Resolution companions are later than the originating client task. Reverse
	// ancestry traversal order to preserve their causal instruction ordering.
	for i := len(companions) - 1; i >= 0; i-- {
		intent.Entries = append(intent.Entries, companions[i])
	}
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
