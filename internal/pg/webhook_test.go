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
		ClaimID: "stale_claim", AttemptedAt: now, CompletedAt: now, Succeeded: true,
	})
	if err != nil || completed {
		t.Fatalf("stale completion = %v, err=%v", completed, err)
	}
	status := 204
	completed, err = repo.CompleteWebhookDelivery(ctx, app.WebhookDeliveryResult{
		WebhookID: first[0].WebhookID, EventID: first[0].EventID,
		ClaimID: first[0].ClaimID, AttemptedAt: now, CompletedAt: now, Succeeded: true,
		ResponseStatus: &status,
	})
	if err != nil || !completed {
		t.Fatalf("completion = %v, err=%v", completed, err)
	}
	removed, err := repo.CleanupWebhookEvents(ctx, now.Add(-time.Hour), 10)
	if err != nil || removed != 0 {
		t.Fatalf("cleanup before terminal retention elapsed = %d, err=%v", removed, err)
	}
	removed, err = repo.CleanupWebhookEvents(ctx, now.Add(time.Hour), 10)
	if err != nil || removed != 1 {
		t.Fatalf("cleanup after terminal retention elapsed = %d, err=%v", removed, err)
	}
}

func TestWebhookCompletionUsesEndpointThenDeliveryLockOrder(t *testing.T) {
	store := testStoreWithMaxConns(t, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	keyring, err := secretcrypto.NewAESGCMKeyring("test", map[string][]byte{
		"test": bytes.Repeat([]byte{44}, 32),
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
	if _, err := store.CreateSession(ctx, newSession("sesn_lock_order"), nil); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	claimed, err := repo.ClaimWebhookDeliveries(ctx, now, now.Add(-time.Minute), "claim_lock_order", 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim = %#v, err=%v", claimed, err)
	}

	blocker, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background()) //nolint:errcheck // best-effort test cleanup
	if _, err := blocker.Exec(ctx, `SELECT 1 FROM webhooks WHERE id = $1 FOR UPDATE`, created.Webhook.ID); err != nil {
		t.Fatal(err)
	}

	type completionResult struct {
		completed bool
		err       error
	}
	completion := make(chan completionResult, 1)
	started := make(chan struct{})
	status := http.StatusNoContent
	go func() {
		close(started)
		completed, completeErr := repo.CompleteWebhookDelivery(ctx, app.WebhookDeliveryResult{
			WebhookID: claimed[0].WebhookID, EventID: claimed[0].EventID,
			ClaimID: claimed[0].ClaimID, AttemptedAt: now, CompletedAt: now,
			Succeeded: true, ResponseStatus: &status,
		})
		completion <- completionResult{completed: completed, err: completeErr}
	}()
	<-started
	secondConnectionAcquired := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		select {
		case result := <-completion:
			t.Fatalf("completion returned before endpoint lock release: completed=%v, err=%v", result.completed, result.err)
		default:
		}
		if store.pool.Stat().AcquiredConns() >= 2 {
			secondConnectionAcquired = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !secondConnectionAcquired {
		t.Fatal("completion did not acquire a database connection")
	}
	// Give the completion transaction time to reach its first row lock. With
	// the required endpoint-first order it blocks there; delivery-first code
	// instead holds the row updated below and hits lock_timeout.
	time.Sleep(50 * time.Millisecond)
	if _, err := blocker.Exec(ctx, `SET LOCAL lock_timeout = '500ms'`); err != nil {
		t.Fatal(err)
	}
	if _, err := blocker.Exec(ctx, `
UPDATE webhook_deliveries
SET last_error = last_error
WHERE webhook_id = $1 AND event_id = $2`, claimed[0].WebhookID, claimed[0].EventID); err != nil {
		t.Fatalf("delivery was locked before endpoint: %v", err)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-completion:
		if result.err != nil || !result.completed {
			t.Fatalf("completion = %v, err=%v", result.completed, result.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestWebhookDisableRecordsPendingDeliveryCompletionTime(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	keyring, err := secretcrypto.NewAESGCMKeyring("test", map[string][]byte{
		"test": bytes.Repeat([]byte{46}, 32),
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
	if _, err := store.CreateSession(ctx, newSession("sesn_manual_disable"), nil); err != nil {
		t.Fatal(err)
	}
	disabled := domain.WebhookStatusDisabled
	updated, err := service.UpdateWebhook(ctx, created.Webhook.ID, app.WebhookUpdateInput{
		Status: &disabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	var state string
	var completedAt time.Time
	if err := store.pool.QueryRow(ctx, `
SELECT state, completed_at
FROM webhook_deliveries
WHERE webhook_id = $1`, created.Webhook.ID).Scan(&state, &completedAt); err != nil {
		t.Fatal(err)
	}
	if state != "failed" || !completedAt.Equal(updated.UpdatedAt) {
		t.Fatalf("delivery state/completed_at = %q/%s, want failed/%s", state, completedAt, updated.UpdatedAt)
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
