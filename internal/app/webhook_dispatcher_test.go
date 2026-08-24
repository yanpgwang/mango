package app

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpegress"
	"github.com/yanpgwang/mango/internal/secretcrypto"
)

func TestWebhookDispatcherSignsStandardWebhookAndCompletes2xx(t *testing.T) {
	now := time.Unix(1700000000, 0).UTC()
	keyring, delivery, rawKey := webhookDispatcherFixture(t)
	repo := &webhookDeliveryRepositoryFake{deliveries: []domain.WebhookDelivery{delivery}}
	var requestAssertion error
	doer := webhookHTTPDoerFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			requestAssertion = err
		}
		if !bytes.Equal(body, delivery.Payload) {
			requestAssertion = fmt.Errorf("payload = %s", body)
		}
		if request.Header.Get("webhook-id") != delivery.EventID ||
			request.Header.Get("webhook-timestamp") != "1700000000" {
			requestAssertion = fmt.Errorf("standard headers = %#v", request.Header)
		}
		message := delivery.EventID + ".1700000000." + string(delivery.Payload)
		mac := hmac.New(sha256.New, rawKey)
		_, _ = mac.Write([]byte(message))
		want := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if got := request.Header.Get("webhook-signature"); got != want {
			requestAssertion = fmt.Errorf("signature = %q, want %q", got, want)
		}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	dispatcher := NewWebhookDispatcher(
		repo, keyring, doer, domain.NewSeqIDGen(), domain.FixedClock{T: now},
	)
	if err := dispatcher.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if requestAssertion != nil {
		t.Fatal(requestAssertion)
	}
	if len(repo.results) != 1 || !repo.results[0].Succeeded || repo.results[0].ResponseStatus == nil || *repo.results[0].ResponseStatus != 204 {
		t.Fatalf("completion = %#v", repo.results)
	}
}

func TestWebhookDispatcherDisablesRedirectAndNonPublicTargets(t *testing.T) {
	for _, test := range []struct {
		name string
		do   webhookHTTPDoerFunc
		want string
	}{
		{
			name: "redirect",
			do: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 302, Body: io.NopCloser(strings.NewReader(""))}, nil
			},
			want: "redirect (3xx)",
		},
		{
			name: "private address",
			do: func(*http.Request) (*http.Response, error) {
				return nil, errors.Join(errors.New("dial failed"), httpegress.ErrNonPublicAddress)
			},
			want: "invalid address",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			keyring, delivery, _ := webhookDispatcherFixture(t)
			repo := &webhookDeliveryRepositoryFake{deliveries: []domain.WebhookDelivery{delivery}}
			dispatcher := NewWebhookDispatcher(
				repo, keyring, test.do, domain.NewSeqIDGen(),
				domain.FixedClock{T: time.Unix(1700000000, 0).UTC()},
			)
			if err := dispatcher.RunOnce(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(repo.results) != 1 || repo.results[0].DisableReason == nil ||
				!strings.Contains(*repo.results[0].DisableReason, test.want) ||
				repo.results[0].NextAttemptAt != nil {
				t.Fatalf("completion = %#v", repo.results)
			}
		})
	}
}

func webhookDispatcherFixture(t *testing.T) (secretcrypto.Cipher, domain.WebhookDelivery, []byte) {
	t.Helper()
	rawKey := bytes.Repeat([]byte{23}, 32)
	keyring, err := secretcrypto.NewAESGCMKeyring("test", map[string][]byte{
		"test": bytes.Repeat([]byte{19}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	secret := "whsec_" + base64.StdEncoding.EncodeToString(rawKey)
	envelope, err := keyring.Seal([]byte(secret), webhookSecretAAD("wh_test"))
	if err != nil {
		t.Fatal(err)
	}
	return keyring, domain.WebhookDelivery{
		WebhookID: "wh_test", EventID: "whe_test", URL: "https://hooks.example.test/events",
		SecretEnvelope: envelope, Payload: []byte(`{"type":"event"}`), ClaimID: "whclaim_test",
	}, rawKey
}

type webhookDeliveryRepositoryFake struct {
	deliveries []domain.WebhookDelivery
	results    []WebhookDeliveryResult
}

func (r *webhookDeliveryRepositoryFake) ClaimWebhookDeliveries(context.Context, time.Time, time.Time, string, int) ([]domain.WebhookDelivery, error) {
	return append([]domain.WebhookDelivery(nil), r.deliveries...), nil
}
func (r *webhookDeliveryRepositoryFake) CompleteWebhookDelivery(_ context.Context, result WebhookDeliveryResult) (bool, error) {
	r.results = append(r.results, result)
	return true, nil
}
func (r *webhookDeliveryRepositoryFake) CleanupWebhookEvents(context.Context, time.Time, int) (int64, error) {
	return 0, nil
}

type webhookHTTPDoerFunc func(*http.Request) (*http.Response, error)

func (f webhookHTTPDoerFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}
