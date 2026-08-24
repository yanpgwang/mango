package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/secretcrypto"
)

func TestWebhookHTTPSecretIsWriteOnlyAndRotatable(t *testing.T) {
	repo := &httpWebhookRepository{items: map[string]domain.Webhook{}}
	keyring, err := secretcrypto.NewAESGCMKeyring("test", map[string][]byte{
		"test": bytes.Repeat([]byte{47}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	service := app.NewWebhookService(
		repo, keyring, domain.NewSeqIDGen(),
		domain.FixedClock{T: time.Unix(1000, 0).UTC()},
	)
	handler := NewServer(Deps{Webhooks: service}, Config{}).Handler()

	created := webhookJSONRequest(t, handler, http.MethodPost, "/v1/webhooks", map[string]any{
		"url":         "https://hooks.example.test/events",
		"event_types": []string{domain.WebhookEventSessionStatusIdled},
	})
	id, _ := created["id"].(string)
	firstSecret, _ := created["signing_secret"].(string)
	if id == "" || firstSecret == "" {
		t.Fatalf("create response = %#v", created)
	}

	got := webhookJSONRequest(t, handler, http.MethodGet, "/v1/webhooks/"+id, nil)
	if _, present := got["signing_secret"]; present {
		t.Fatalf("GET exposed signing_secret: %#v", got)
	}
	rotated := webhookJSONRequest(
		t, handler, http.MethodPost,
		"/v1/webhooks/"+id+"/regenerate_signing_secret", map[string]any{},
	)
	if rotated["signing_secret"] == firstSecret || rotated["signing_secret"] == "" {
		t.Fatalf("rotation response = %#v", rotated)
	}
	updated := webhookJSONRequest(t, handler, http.MethodPost, "/v1/webhooks/"+id, map[string]any{
		"status": "disabled",
	})
	if updated["status"] != "disabled" {
		t.Fatalf("update response = %#v", updated)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/v1/webhooks/"+id, nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("delete status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestWebhookHTTPReportsUnconfiguredAPI(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/webhooks", nil)
	NewServer(Deps{}, Config{}).Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
}

func webhookJSONRequest(
	t *testing.T,
	handler http.Handler,
	method string,
	path string,
	body any,
) map[string]any {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	if body != nil {
		request.Header.Set("content-type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s %s status = %d, body=%s", method, path, recorder.Code, recorder.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

type httpWebhookRepository struct {
	mu    sync.Mutex
	items map[string]domain.Webhook
}

func (r *httpWebhookRepository) CreateWebhook(_ context.Context, item domain.Webhook, _ int) (domain.Webhook, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[item.ID] = item
	return item, nil
}
func (r *httpWebhookRepository) GetWebhook(_ context.Context, id string) (domain.Webhook, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[id]
	if !ok {
		return domain.Webhook{}, domain.NotFound("webhook not found")
	}
	return item, nil
}
func (r *httpWebhookRepository) UpdateWebhook(_ context.Context, id string, mutate func(domain.Webhook) (domain.Webhook, bool, error)) (domain.Webhook, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	item, ok := r.items[id]
	if !ok {
		return domain.Webhook{}, domain.NotFound("webhook not found")
	}
	next, changed, err := mutate(item)
	if err == nil && changed {
		r.items[id] = next
	}
	return next, err
}
func (r *httpWebhookRepository) ListWebhooks(context.Context, app.WebhookListQuery) (app.WebhookListPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	items := make([]domain.Webhook, 0, len(r.items))
	for _, item := range r.items {
		items = append(items, item)
	}
	return app.WebhookListPage{Webhooks: items}, nil
}
func (r *httpWebhookRepository) DeleteWebhook(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[id]; !ok {
		return domain.NotFound("webhook not found")
	}
	delete(r.items, id)
	return nil
}
