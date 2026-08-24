package app

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/secretcrypto"
)

func TestWebhookServiceEncryptsAndRotatesSigningSecret(t *testing.T) {
	repo := &webhookRepositoryFake{}
	keyring, err := secretcrypto.NewAESGCMKeyring("test", map[string][]byte{
		"test": bytes.Repeat([]byte{17}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	service := NewWebhookService(
		repo, keyring, domain.NewSeqIDGen(),
		domain.FixedClock{T: time.Unix(1000, 0).UTC()},
	)
	created, err := service.CreateWebhook(context.Background(), WebhookCreateInput{
		URL:        " https://hooks.example.test/events ",
		EventTypes: []string{domain.WebhookEventSessionStatusIdled},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(created.SigningSecret, "whsec_") {
		t.Fatalf("signing secret = %q", created.SigningSecret)
	}
	if bytes.Contains(repo.item.SecretEnvelope.Ciphertext, []byte(created.SigningSecret)) {
		t.Fatal("stored ciphertext contains the plaintext signing secret")
	}
	opened, err := OpenWebhookSigningSecret(keyring, created.Webhook.ID, *repo.item.SecretEnvelope)
	if err != nil {
		t.Fatal(err)
	}
	if string(opened) != created.SigningSecret {
		t.Fatalf("opened signing secret = %q", opened)
	}
	secretcrypto.Zero(opened)

	rotated, err := service.RegenerateSigningSecret(context.Background(), created.Webhook.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.SigningSecret == created.SigningSecret {
		t.Fatal("rotation returned the original signing secret")
	}
	if _, err := OpenWebhookSigningSecret(keyring, "wh_wrong", *repo.item.SecretEnvelope); err == nil {
		t.Fatal("ciphertext opened under the wrong webhook AAD")
	}
}

func TestWebhookServiceRejectsUnsafeOrUnknownConfiguration(t *testing.T) {
	for _, test := range []struct {
		name  string
		url   string
		event string
	}{
		{name: "plain HTTP", url: "http://hooks.example.test/events", event: domain.WebhookEventSessionStatusIdled},
		{name: "nonstandard port", url: "https://hooks.example.test:8443/events", event: domain.WebhookEventSessionStatusIdled},
		{name: "literal IP", url: "https://203.0.113.1/events", event: domain.WebhookEventSessionStatusIdled},
		{name: "unknown event", url: "https://hooks.example.test/events", event: "session.made_up"},
	} {
		t.Run(test.name, func(t *testing.T) {
			service := NewWebhookService(
				&webhookRepositoryFake{}, nil, domain.NewSeqIDGen(), domain.FixedClock{},
			)
			_, err := service.CreateWebhook(context.Background(), WebhookCreateInput{
				URL: test.url, EventTypes: []string{test.event},
			})
			if err == nil {
				t.Fatal("CreateWebhook succeeded")
			}
		})
	}
}

type webhookRepositoryFake struct{ item domain.Webhook }

func (r *webhookRepositoryFake) CreateWebhook(_ context.Context, item domain.Webhook, _ int) (domain.Webhook, error) {
	r.item = item
	return item, nil
}
func (r *webhookRepositoryFake) GetWebhook(context.Context, string) (domain.Webhook, error) {
	return r.item, nil
}
func (r *webhookRepositoryFake) UpdateWebhook(_ context.Context, _ string, mutate func(domain.Webhook) (domain.Webhook, bool, error)) (domain.Webhook, error) {
	next, changed, err := mutate(r.item)
	if err == nil && changed {
		r.item = next
	}
	return r.item, err
}
func (r *webhookRepositoryFake) ListWebhooks(context.Context, WebhookListQuery) (WebhookListPage, error) {
	return WebhookListPage{Webhooks: []domain.Webhook{r.item}}, nil
}
func (r *webhookRepositoryFake) DeleteWebhook(context.Context, string) error { return nil }
