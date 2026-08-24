package pg

import (
	"context"
	"encoding/json"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

type webhookEventEnvelope struct {
	Type      string           `json:"type"`
	ID        string           `json:"id"`
	CreatedAt string           `json:"created_at"`
	Data      webhookEventData `json:"data"`
}

type webhookEventData struct {
	Type            string `json:"type"`
	ID              string `json:"id"`
	WorkspaceID     string `json:"workspace_id"`
	SessionThreadID string `json:"session_thread_id,omitempty"`
}

type sessionWebhookNotification struct {
	EventType       string
	SessionThreadID string
}

func (s *Store) enqueueWebhookEvent(
	ctx context.Context,
	q *pgstore.Queries,
	workspaceID string,
	eventType string,
	resourceID string,
	createdAt time.Time,
	extra *webhookEventData,
) error {
	createdAt = createdAt.UTC().Truncate(time.Microsecond)
	eventID := s.ids.NewID(domain.PrefixWebhookEvent)
	data := webhookEventData{
		Type: eventType, ID: resourceID, WorkspaceID: workspaceID,
	}
	if extra != nil {
		data.SessionThreadID = extra.SessionThreadID
	}
	payload, err := json.Marshal(webhookEventEnvelope{
		Type: "event", ID: eventID, CreatedAt: createdAt.Format(time.RFC3339Nano),
		Data: data,
	})
	if err != nil {
		return err
	}
	return q.EnqueueWebhookEvent(ctx, pgstore.EnqueueWebhookEventParams{
		ID: eventID, WorkspaceID: workspaceID, EventType: eventType,
		ResourceID: resourceID, Payload: payload, CreatedAt: tsUTC(createdAt),
	})
}

func (s *Store) enqueueWebhooksForSessionEvent(
	ctx context.Context,
	q *pgstore.Queries,
	workspaceID string,
	sessionID string,
	eventType string,
	payload map[string]any,
	createdAt time.Time,
) error {
	for _, notification := range sessionWebhookNotifications(eventType, payload) {
		var extra *webhookEventData
		if notification.SessionThreadID != "" {
			extra = &webhookEventData{SessionThreadID: notification.SessionThreadID}
		}
		if err := s.enqueueWebhookEvent(
			ctx, q, workspaceID, notification.EventType, sessionID, createdAt, extra,
		); err != nil {
			return err
		}
	}
	return nil
}

func webhookCandidateDrafts(drafts []domain.EventDraft) bool {
	for _, draft := range drafts {
		if len(sessionWebhookNotifications(draft.Type, draft.Payload)) > 0 {
			return true
		}
	}
	return false
}

func sessionWebhookNotifications(
	eventType string,
	payload map[string]any,
) []sessionWebhookNotification {
	switch eventType {
	case domain.EvSessionStatusRunning:
		return []sessionWebhookNotification{{EventType: domain.WebhookEventSessionStatusRunStarted}}
	case domain.EvSessionStatusIdle:
		notifications := []sessionWebhookNotification{{EventType: domain.WebhookEventSessionStatusIdled}}
		stopReason, _ := payload["stop_reason"].(map[string]any)
		if stopReason["type"] == "budget_reached" {
			notifications = append(notifications, sessionWebhookNotification{
				EventType: domain.WebhookEventSessionBudgetReached,
			})
		}
		return notifications
	case domain.EvSessionStatusRescheduling:
		return []sessionWebhookNotification{{EventType: domain.WebhookEventSessionStatusRescheduled}}
	case domain.EvSessionStatusTerminated:
		return []sessionWebhookNotification{{EventType: domain.WebhookEventSessionStatusTerminated}}
	case domain.EvSessionThreadCreated:
		return threadWebhookNotification(
			domain.WebhookEventSessionThreadCreated, payload,
		)
	case domain.EvSessionThreadStatusIdle:
		return threadWebhookNotification(
			domain.WebhookEventSessionThreadIdled, payload,
		)
	case domain.EvSessionThreadStatusTerminated:
		return threadWebhookNotification(
			domain.WebhookEventSessionThreadTerminated, payload,
		)
	case domain.EvSpanOutcomeEvaluationEnd:
		return []sessionWebhookNotification{{EventType: domain.WebhookEventSessionOutcomeEvaluationEnded}}
	case domain.EvSessionUpdated:
		return []sessionWebhookNotification{{EventType: domain.WebhookEventSessionUpdated}}
	default:
		return nil
	}
}

func threadWebhookNotification(
	eventType string,
	payload map[string]any,
) []sessionWebhookNotification {
	threadID, _ := payload["session_thread_id"].(string)
	if threadID == "" {
		return nil
	}
	return []sessionWebhookNotification{{
		EventType: eventType, SessionThreadID: threadID,
	}}
}
