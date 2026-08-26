package httpapi

import (
	"net/http"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

type webhookCreateRequest struct {
	URL        string   `json:"url"`
	EventTypes []string `json:"event_types"`
}

type webhookUpdateRequest struct {
	URL        optionalJSONField[string]               `json:"url"`
	EventTypes optionalJSONField[[]string]             `json:"event_types"`
	Status     optionalJSONField[domain.WebhookStatus] `json:"status"`
}

func (s *Server) createWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksConfigured(w) {
		return
	}
	var body webhookCreateRequest
	if err := decodeJSONBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	result, err := s.deps.Webhooks.CreateWebhook(r.Context(), app.WebhookCreateInput{
		URL: body.URL, EventTypes: body.EventTypes,
	})
	if err != nil {
		writeError(w, err)
		return
	}
	response := webhookToJSON(result.Webhook)
	response["signing_secret"] = result.SigningSecret
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) getWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksConfigured(w) {
		return
	}
	item, err := s.deps.Webhooks.GetWebhook(r.Context(), r.PathValue("webhook_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, webhookToJSON(item))
}

func (s *Server) updateWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksConfigured(w) {
		return
	}
	var body webhookUpdateRequest
	if err := decodeJSONBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if body.URL.Null || body.EventTypes.Null || body.Status.Null {
		writeError(w, domain.Validation("url, event_types, and status cannot be null"))
		return
	}
	input := app.WebhookUpdateInput{}
	if body.URL.Present {
		input.URL = &body.URL.Value
	}
	if body.EventTypes.Present {
		input.EventTypes = &body.EventTypes.Value
	}
	if body.Status.Present {
		input.Status = &body.Status.Value
	}
	item, err := s.deps.Webhooks.UpdateWebhook(
		r.Context(), r.PathValue("webhook_id"), input,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, webhookToJSON(item))
}

func (s *Server) listWebhooks(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksConfigured(w) {
		return
	}
	query, filter, err := parseWebhookListQuery(r)
	if err != nil {
		writeError(w, err)
		return
	}
	page, err := s.deps.Webhooks.ListWebhooks(r.Context(), query)
	if err != nil {
		writeError(w, err)
		return
	}
	data := make([]map[string]any, 0, len(page.Webhooks))
	for _, item := range page.Webhooks {
		data = append(data, webhookToJSON(item))
	}
	var next any
	if page.HasNext && len(page.Webhooks) > 0 {
		last := page.Webhooks[len(page.Webhooks)-1]
		next = encodeResourceCursor(resourceCursor{
			Kind: webhookListCursorKind, CreatedAt: last.CreatedAt.Format(timeFmt),
			ID: last.ID, Filter: filter,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "next_page": next})
}

func (s *Server) regenerateWebhookSigningSecret(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksConfigured(w) {
		return
	}
	result, err := s.deps.Webhooks.RegenerateSigningSecret(
		r.Context(), r.PathValue("webhook_id"),
	)
	if err != nil {
		writeError(w, err)
		return
	}
	response := webhookToJSON(result.Webhook)
	response["signing_secret"] = result.SigningSecret
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	if !s.webhooksConfigured(w) {
		return
	}
	id := r.PathValue("webhook_id")
	if err := s.deps.Webhooks.DeleteWebhook(r.Context(), id); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "type": "webhook_deleted"})
}

func parseWebhookListQuery(r *http.Request) (app.WebhookListQuery, string, error) {
	values := r.URL.Query()
	query := app.WebhookListQuery{Limit: app.DefaultWebhookListLimit}
	filter := resourceFilterFingerprint(struct{}{})
	if values.Has("limit") {
		limit, err := parseResourceListLimit(values.Get("limit"))
		if err != nil || limit > app.MaxWebhookListLimit {
			return query, filter, domain.Validation("limit must be between 1 and 100")
		}
		query.Limit = limit
	}
	if values.Has("page") {
		cursor, ok := decodeResourceCursor(values.Get("page"), webhookListCursorKind)
		if !ok || cursor.Filter != filter {
			return query, filter, domain.Validation("invalid page cursor")
		}
		createdAt, ok := parseTimeParam(cursor.CreatedAt)
		if !ok {
			return query, filter, domain.Validation("invalid page cursor")
		}
		query.After = &app.ResourcePageBoundary{CreatedAt: createdAt.UTC(), ID: cursor.ID}
	}
	return query, filter, nil
}

func webhookToJSON(item domain.Webhook) map[string]any {
	var disabledReason any
	if item.DisabledReason != nil {
		disabledReason = *item.DisabledReason
	}
	return map[string]any{
		"id": item.ID, "type": "webhook", "url": item.URL,
		"event_types": append([]string(nil), item.EventTypes...),
		"status":      item.Status, "disabled_reason": disabledReason,
		"created_at": item.CreatedAt.Format(timeFmt),
		"updated_at": item.UpdatedAt.Format(timeFmt),
	}
}

func (s *Server) webhooksConfigured(w http.ResponseWriter) bool {
	if s.deps.Webhooks != nil {
		return true
	}
	writeError(w, domain.Unsupported("Webhook API is not configured"))
	return false
}
