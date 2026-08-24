package pg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/secretcrypto"
)

func TestWebhookSubscriptionSnapshotDoesNotBackfillAndPayloadIsThin(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	before := newSession("sesn_before_webhook")
	if _, err := store.CreateSession(ctx, before, nil); err != nil {
		t.Fatal(err)
	}

	keyring, err := secretcrypto.NewAESGCMKeyring("test", map[string][]byte{
		"test": bytes.Repeat([]byte{41}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewWebhookRepository(store)
	service := app.NewWebhookService(repo, keyring, domain.NewSeqIDGen(), fixedClock{})
	created, err := service.CreateWebhook(ctx, app.WebhookCreateInput{
		URL:        "https://hooks.example.test/events",
		EventTypes: []string{domain.WebhookEventSessionStatusScheduled},
	})
	if err != nil {
		t.Fatal(err)
	}
	after := newSession("sesn_after_webhook")
	if _, err := store.CreateSession(ctx, after, nil); err != nil {
		t.Fatal(err)
	}

	deliveries, err := repo.ClaimWebhookDeliveries(
		ctx, time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), "claim_test", 10,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(deliveries))
	}
	if deliveries[0].WebhookID != created.Webhook.ID {
		t.Fatalf("webhook id = %q", deliveries[0].WebhookID)
	}
	var payload struct {
		Type string         `json:"type"`
		ID   string         `json:"id"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(deliveries[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Type != "event" || payload.ID != deliveries[0].EventID ||
		payload.Data["type"] != domain.WebhookEventSessionStatusScheduled ||
		payload.Data["id"] != after.ID || payload.Data["workspace_id"] == "" {
		t.Fatalf("payload = %s", deliveries[0].Payload)
	}
	if _, exists := payload.Data["organization_id"]; exists {
		t.Fatalf("Mango payload leaked a hosted organization field: %s", deliveries[0].Payload)
	}
}

func TestWebhookDeliveryClaimIsLeasedAndCompletionIsClaimFenced(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	keyring, err := secretcrypto.NewAESGCMKeyring("test", map[string][]byte{
		"test": bytes.Repeat([]byte{43}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewWebhookRepository(store)
	service := app.NewWebhookService(repo, keyring, domain.NewSeqIDGen(), fixedClock{})
	if _, err := service.CreateWebhook(ctx, app.WebhookCreateInput{
		URL:        "https://hooks.example.test/events",
		EventTypes: []string{domain.WebhookEventSessionStatusScheduled},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSession(ctx, newSession("sesn_claim_fence"), nil); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	first, err := repo.ClaimWebhookDeliveries(ctx, now, now.Add(-time.Minute), "claim_first", 10)
	if err != nil || len(first) != 1 {
		t.Fatalf("first claim = %#v, err=%v", first, err)
	}
	second, err := repo.ClaimWebhookDeliveries(ctx, now, now.Add(-time.Minute), "claim_second", 10)
	if err != nil || len(second) != 0 {
		t.Fatalf("concurrent claim = %#v, err=%v", second, err)
	}
	completed, err := repo.CompleteWebhookDelivery(ctx, app.WebhookDeliveryResult{
		WebhookID: first[0].WebhookID, EventID: first[0].EventID,
		ClaimID: "stale_claim", AttemptedAt: now, Succeeded: true,
	})
	if err != nil || completed {
		t.Fatalf("stale completion = %v, err=%v", completed, err)
	}
	status := 204
	completed, err = repo.CompleteWebhookDelivery(ctx, app.WebhookDeliveryResult{
		WebhookID: first[0].WebhookID, EventID: first[0].EventID,
		ClaimID: first[0].ClaimID, AttemptedAt: now, Succeeded: true,
		ResponseStatus: &status,
	})
	if err != nil || !completed {
		t.Fatalf("completion = %v, err=%v", completed, err)
	}
}

func TestWebhookDispatcherCompletesPersistedDelivery(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	keyring, err := secretcrypto.NewAESGCMKeyring("test", map[string][]byte{
		"test": bytes.Repeat([]byte{45}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewWebhookRepository(store)
	service := app.NewWebhookService(repo, keyring, domain.NewSeqIDGen(), fixedClock{})
	if _, err := service.CreateWebhook(ctx, app.WebhookCreateInput{
		URL:        "https://hooks.example.test/events",
		EventTypes: []string{domain.WebhookEventSessionStatusScheduled},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateSession(ctx, newSession("sesn_dispatch"), nil); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	requests := 0
	var requestAssertion error
	dispatcher := app.NewWebhookDispatcher(
		repo, keyring, webhookPGHTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			if request.Header.Get("webhook-id") == "" || request.Header.Get("webhook-signature") == "" {
				requestAssertion = fmt.Errorf("missing Standard Webhooks headers: %#v", request.Header)
			}
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}),
		domain.NewSeqIDGen(), domain.FixedClock{T: now},
	)
	if err := dispatcher.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if requestAssertion != nil {
		t.Fatal(requestAssertion)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	remaining, err := repo.ClaimWebhookDeliveries(
		ctx, now.Add(time.Hour), now, "claim_after_success", 10,
	)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("remaining deliveries = %#v, err=%v", remaining, err)
	}
}

type webhookPGHTTPDoerFunc func(*http.Request) (*http.Response, error)

func (function webhookPGHTTPDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return function(request)
}
