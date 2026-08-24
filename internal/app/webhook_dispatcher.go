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
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpegress"
	"github.com/yanpgwang/mango/internal/secretcrypto"
)

const (
	DefaultWebhookPollInterval = time.Second
	DefaultWebhookClaimLease   = 2 * time.Minute
	DefaultWebhookHTTPTimeout  = 15 * time.Second
	DefaultWebhookBatchSize    = 25
	DefaultWebhookRetention    = 30 * 24 * time.Hour
	DefaultWebhookCleanupEvery = time.Hour
	MaxWebhookDeliveryAttempts = 3
	maxWebhookDiagnosticBytes  = 1024
)

type WebhookDeliveryResult struct {
	WebhookID      string
	EventID        string
	ClaimID        string
	AttemptedAt    time.Time
	Succeeded      bool
	ResponseStatus *int
	Error          string
	NextAttemptAt  *time.Time
	DisableReason  *string
}

type WebhookDeliveryRepository interface {
	ClaimWebhookDeliveries(
		context.Context, time.Time, time.Time, string, int,
	) ([]domain.WebhookDelivery, error)
	CompleteWebhookDelivery(context.Context, WebhookDeliveryResult) (bool, error)
	CleanupWebhookEvents(context.Context, time.Time, int) (int64, error)
}

type WebhookHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type WebhookDispatcher struct {
	repo         WebhookDeliveryRepository
	cipher       secretcrypto.Cipher
	client       WebhookHTTPDoer
	ids          domain.IDGenerator
	clock        domain.Clock
	pollInterval time.Duration
	claimLease   time.Duration
	batchSize    int
	retryDelay   func(int) time.Duration
	retention    time.Duration
	cleanupEvery time.Duration
	lastCleanup  time.Time
}

func NewWebhookDispatcher(
	repo WebhookDeliveryRepository,
	cipher secretcrypto.Cipher,
	client WebhookHTTPDoer,
	ids domain.IDGenerator,
	clock domain.Clock,
) *WebhookDispatcher {
	return &WebhookDispatcher{
		repo: repo, cipher: cipher, client: client, ids: ids, clock: clock,
		pollInterval: DefaultWebhookPollInterval,
		claimLease:   DefaultWebhookClaimLease,
		batchSize:    DefaultWebhookBatchSize,
		retryDelay:   webhookRetryDelay,
		retention:    DefaultWebhookRetention,
		cleanupEvery: DefaultWebhookCleanupEvery,
	}
}

func (d *WebhookDispatcher) Run(ctx context.Context) error {
	if d.repo == nil || d.cipher == nil || d.client == nil || d.ids == nil || d.clock == nil {
		return errors.New("webhook dispatcher is not configured")
	}
	for {
		if err := d.RunOnce(ctx); err != nil {
			return err
		}
		timer := time.NewTimer(d.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (d *WebhookDispatcher) RunOnce(ctx context.Context) error {
	now := d.clock.Now().UTC()
	if d.lastCleanup.IsZero() || now.Sub(d.lastCleanup) >= d.cleanupEvery {
		if _, err := d.repo.CleanupWebhookEvents(ctx, now.Add(-d.retention), 1000); err != nil {
			return err
		}
		d.lastCleanup = now
	}
	claimID := d.ids.NewID(domain.PrefixWebhookClaim)
	deliveries, err := d.repo.ClaimWebhookDeliveries(
		ctx, now, now.Add(-d.claimLease), claimID, d.batchSize,
	)
	if err != nil {
		return err
	}
	var wait sync.WaitGroup
	errorsByDelivery := make(chan error, len(deliveries))
	for _, delivery := range deliveries {
		wait.Add(1)
		go func(delivery domain.WebhookDelivery) {
			defer wait.Done()
			if err := d.deliver(ctx, delivery); err != nil {
				errorsByDelivery <- err
			}
		}(delivery)
	}
	wait.Wait()
	close(errorsByDelivery)
	var combined error
	for deliveryErr := range errorsByDelivery {
		combined = errors.Join(combined, deliveryErr)
	}
	return combined
}

func (d *WebhookDispatcher) deliver(ctx context.Context, delivery domain.WebhookDelivery) error {
	attemptedAt := d.clock.Now().UTC()
	secret, err := OpenWebhookSigningSecret(d.cipher, delivery.WebhookID, delivery.SecretEnvelope)
	if err != nil {
		return d.finishFailure(ctx, delivery, attemptedAt, nil, err, nil)
	}
	defer secretcrypto.Zero(secret)

	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, delivery.URL, bytes.NewReader(delivery.Payload),
	)
	if err != nil {
		return d.finishFailure(ctx, delivery, attemptedAt, nil, err, nil)
	}
	timestamp := strconv.FormatInt(attemptedAt.Unix(), 10)
	signature, err := signWebhookPayload(secret, delivery.EventID, timestamp, delivery.Payload)
	if err != nil {
		return d.finishFailure(ctx, delivery, attemptedAt, nil, err, nil)
	}
	request.Header.Set("content-type", "application/json")
	request.Header.Set("webhook-id", delivery.EventID)
	request.Header.Set("webhook-timestamp", timestamp)
	request.Header.Set("webhook-signature", signature)

	response, requestErr := d.client.Do(request)
	var status *int
	if response != nil {
		value := response.StatusCode
		status = &value
		if response.Body != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxWebhookDiagnosticBytes))
			_ = response.Body.Close()
		}
	}
	if requestErr == nil && status != nil && *status >= 200 && *status < 300 {
		_, err := d.repo.CompleteWebhookDelivery(ctx, WebhookDeliveryResult{
			WebhookID: delivery.WebhookID, EventID: delivery.EventID,
			ClaimID: delivery.ClaimID, AttemptedAt: attemptedAt,
			Succeeded: true, ResponseStatus: status,
		})
		return err
	}

	var disableReason *string
	if status != nil && *status >= 300 && *status < 400 {
		reason := "auto-disabled: endpoint URL returned a redirect (3xx)"
		disableReason = &reason
	} else if errors.Is(requestErr, httpegress.ErrNonPublicAddress) {
		reason := "auto-disabled: endpoint URL resolved to an invalid address"
		disableReason = &reason
	}
	if requestErr == nil {
		requestErr = fmt.Errorf("endpoint returned HTTP %d", *status)
	}
	return d.finishFailure(ctx, delivery, attemptedAt, status, requestErr, disableReason)
}

func (d *WebhookDispatcher) finishFailure(
	ctx context.Context,
	delivery domain.WebhookDelivery,
	attemptedAt time.Time,
	status *int,
	deliveryErr error,
	disableReason *string,
) error {
	message := truncateWebhookDiagnostic(deliveryErr)
	var nextAttemptAt *time.Time
	if disableReason == nil && delivery.AttemptCount+1 < MaxWebhookDeliveryAttempts {
		next := attemptedAt.Add(d.retryDelay(delivery.AttemptCount + 1)).UTC()
		nextAttemptAt = &next
	}
	_, err := d.repo.CompleteWebhookDelivery(ctx, WebhookDeliveryResult{
		WebhookID: delivery.WebhookID, EventID: delivery.EventID,
		ClaimID: delivery.ClaimID, AttemptedAt: attemptedAt,
		ResponseStatus: status, Error: message, NextAttemptAt: nextAttemptAt,
		DisableReason: disableReason,
	})
	return err
}

func signWebhookPayload(secret []byte, eventID, timestamp string, payload []byte) (string, error) {
	const prefix = "whsec_"
	if !strings.HasPrefix(string(secret), prefix) {
		return "", errors.New("webhook signing secret has invalid format")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(string(secret), prefix))
	if err != nil || len(key) != webhookSigningSecretByteSize {
		return "", errors.New("webhook signing secret has invalid format")
	}
	defer secretcrypto.Zero(key)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(eventID))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(payload)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

func webhookRetryDelay(attempt int) time.Duration {
	// CMA documents a jittered exponential range of 5–120 seconds without
	// publishing an exact schedule. Each retry draws within an exponential
	// window while preserving those public bounds.
	ceiling := 5 * time.Second
	for index := 0; index < attempt; index++ {
		ceiling *= 2
	}
	if ceiling > 120*time.Second {
		ceiling = 120 * time.Second
	}
	floor := ceiling / 2
	if floor < 5*time.Second {
		floor = 5 * time.Second
	}
	return floor + time.Duration(rand.Int64N(int64(ceiling-floor)+1))
}

func truncateWebhookDiagnostic(err error) string {
	if err == nil {
		return "delivery failed"
	}
	value := err.Error()
	if len(value) > maxWebhookDiagnosticBytes {
		value = value[:maxWebhookDiagnosticBytes]
	}
	return value
}
