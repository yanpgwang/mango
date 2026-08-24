package app

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/secretcrypto"
)

const (
	DefaultWebhookListLimit      = 20
	MaxWebhookListLimit          = 100
	MaxWebhooksPerWorkspace      = 100
	MaxWebhookURLBytes           = 2048
	MaxWebhookEventTypes         = 64
	webhookSigningSecretByteSize = 32
)

var supportedWebhookEventTypes = []string{
	domain.WebhookEventSessionStatusScheduled,
	domain.WebhookEventSessionStatusRunStarted,
	domain.WebhookEventSessionStatusIdled,
	domain.WebhookEventSessionStatusRescheduled,
	domain.WebhookEventSessionStatusTerminated,
	domain.WebhookEventSessionBudgetReached,
	domain.WebhookEventSessionThreadCreated,
	domain.WebhookEventSessionThreadIdled,
	domain.WebhookEventSessionThreadTerminated,
	domain.WebhookEventSessionOutcomeEvaluationEnded,
	domain.WebhookEventSessionUpdated,
	domain.WebhookEventSessionArchived,
	domain.WebhookEventSessionDeleted,
	domain.WebhookEventDeploymentRunSucceeded,
	domain.WebhookEventDeploymentRunFailed,
}

type WebhookCreateInput struct {
	URL        string
	EventTypes []string
}

type WebhookUpdateInput struct {
	URL        *string
	EventTypes *[]string
	Status     *domain.WebhookStatus
}

type WebhookListQuery struct {
	After *ResourcePageBoundary
	Limit int
}

type WebhookListPage struct {
	Webhooks []domain.Webhook
	HasNext  bool
}

type WebhookSecretResult struct {
	Webhook       domain.Webhook
	SigningSecret string
}

type WebhookRepository interface {
	CreateWebhook(context.Context, domain.Webhook, int) (domain.Webhook, error)
	GetWebhook(context.Context, string) (domain.Webhook, error)
	UpdateWebhook(context.Context, string, func(domain.Webhook) (domain.Webhook, bool, error)) (domain.Webhook, error)
	ListWebhooks(context.Context, WebhookListQuery) (WebhookListPage, error)
	DeleteWebhook(context.Context, string) error
}

type WebhookService struct {
	repo   WebhookRepository
	cipher secretcrypto.Cipher
	ids    domain.IDGenerator
	clock  domain.Clock
}

func NewWebhookService(
	repo WebhookRepository,
	cipher secretcrypto.Cipher,
	ids domain.IDGenerator,
	clock domain.Clock,
) *WebhookService {
	return &WebhookService{repo: repo, cipher: cipher, ids: ids, clock: clock}
}

func (s *WebhookService) CreateWebhook(
	ctx context.Context,
	input WebhookCreateInput,
) (WebhookSecretResult, error) {
	target, err := normalizeWebhookURL(input.URL)
	if err != nil {
		return WebhookSecretResult{}, err
	}
	eventTypes, err := validateWebhookEventTypes(input.EventTypes)
	if err != nil {
		return WebhookSecretResult{}, err
	}
	id := s.ids.NewID(domain.PrefixWebhook)
	secret, envelope, err := s.newSigningSecret(id)
	if err != nil {
		return WebhookSecretResult{}, err
	}
	now := s.clock.Now().UTC().Truncate(time.Microsecond)
	created, err := s.repo.CreateWebhook(ctx, domain.Webhook{
		ID: id, URL: target, EventTypes: eventTypes,
		Status: domain.WebhookStatusEnabled, SecretEnvelope: &envelope,
		CreatedAt: now, UpdatedAt: now,
	}, MaxWebhooksPerWorkspace)
	if err != nil {
		return WebhookSecretResult{}, err
	}
	return WebhookSecretResult{Webhook: created, SigningSecret: secret}, nil
}

func (s *WebhookService) GetWebhook(ctx context.Context, id string) (domain.Webhook, error) {
	return s.repo.GetWebhook(ctx, id)
}

func (s *WebhookService) ListWebhooks(
	ctx context.Context,
	query WebhookListQuery,
) (WebhookListPage, error) {
	if query.Limit == 0 {
		query.Limit = DefaultWebhookListLimit
	}
	if query.Limit < 1 || query.Limit > MaxWebhookListLimit {
		return WebhookListPage{}, domain.Validation("limit must be between 1 and 100")
	}
	return s.repo.ListWebhooks(ctx, query)
}

func (s *WebhookService) UpdateWebhook(
	ctx context.Context,
	id string,
	input WebhookUpdateInput,
) (domain.Webhook, error) {
	var target *string
	if input.URL != nil {
		normalized, err := normalizeWebhookURL(*input.URL)
		if err != nil {
			return domain.Webhook{}, err
		}
		target = &normalized
	}
	var eventTypes *[]string
	if input.EventTypes != nil {
		validated, err := validateWebhookEventTypes(*input.EventTypes)
		if err != nil {
			return domain.Webhook{}, err
		}
		eventTypes = &validated
	}
	if input.Status != nil && *input.Status != domain.WebhookStatusEnabled &&
		*input.Status != domain.WebhookStatusDisabled {
		return domain.Webhook{}, domain.Validation("status must be enabled or disabled")
	}
	return s.repo.UpdateWebhook(ctx, id, func(current domain.Webhook) (domain.Webhook, bool, error) {
		next := current
		if target != nil {
			next.URL = *target
		}
		if eventTypes != nil {
			next.EventTypes = append([]string(nil), (*eventTypes)...)
		}
		if input.Status != nil {
			next.Status = *input.Status
			if next.Status == domain.WebhookStatusEnabled {
				next.DisabledReason = nil
				next.FailureStartedAt = nil
			}
		}
		changed := current.URL != next.URL ||
			!slices.Equal(current.EventTypes, next.EventTypes) || current.Status != next.Status ||
			!equalOptionalString(current.DisabledReason, next.DisabledReason)
		if changed {
			next.UpdatedAt = s.clock.Now().UTC().Truncate(time.Microsecond)
		}
		return next, changed, nil
	})
}

func (s *WebhookService) RegenerateSigningSecret(
	ctx context.Context,
	id string,
) (WebhookSecretResult, error) {
	secret, envelope, err := s.newSigningSecret(id)
	if err != nil {
		return WebhookSecretResult{}, err
	}
	updated, err := s.repo.UpdateWebhook(ctx, id, func(current domain.Webhook) (domain.Webhook, bool, error) {
		current.SecretEnvelope = &envelope
		current.UpdatedAt = s.clock.Now().UTC().Truncate(time.Microsecond)
		return current, true, nil
	})
	if err != nil {
		return WebhookSecretResult{}, err
	}
	return WebhookSecretResult{Webhook: updated, SigningSecret: secret}, nil
}

func (s *WebhookService) DeleteWebhook(ctx context.Context, id string) error {
	return s.repo.DeleteWebhook(ctx, id)
}

func (s *WebhookService) newSigningSecret(id string) (string, domain.SecretEnvelope, error) {
	if s.cipher == nil {
		return "", domain.SecretEnvelope{}, errors.New("webhook signing cipher is unavailable")
	}
	raw := make([]byte, webhookSigningSecretByteSize)
	if _, err := rand.Read(raw); err != nil {
		return "", domain.SecretEnvelope{}, err
	}
	defer secretcrypto.Zero(raw)
	secret := "whsec_" + base64.StdEncoding.EncodeToString(raw)
	plaintext := []byte(secret)
	defer secretcrypto.Zero(plaintext)
	envelope, err := s.cipher.Seal(plaintext, webhookSecretAAD(id))
	return secret, envelope, err
}

func OpenWebhookSigningSecret(
	cipher secretcrypto.Cipher,
	webhookID string,
	envelope domain.SecretEnvelope,
) ([]byte, error) {
	if cipher == nil {
		return nil, errors.New("webhook signing cipher is unavailable")
	}
	return cipher.Open(envelope, webhookSecretAAD(webhookID))
}

func webhookSecretAAD(id string) []byte {
	return []byte("mango:webhook:" + id + ":signing-secret:v1")
}

func normalizeWebhookURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > MaxWebhookURLBytes {
		return "", domain.Validation("url must contain between 1 and 2048 bytes")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return "", domain.Validation("url must be an absolute HTTPS URL")
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return "", domain.Validation("url must not contain credentials or a fragment")
	}
	if parsed.Port() != "" && parsed.Port() != "443" {
		return "", domain.Validation("url must use HTTPS port 443")
	}
	if net.ParseIP(parsed.Hostname()) != nil || strings.EqualFold(parsed.Hostname(), "localhost") {
		return "", domain.Validation("url must use a publicly resolvable hostname")
	}
	return parsed.String(), nil
}

func validateWebhookEventTypes(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > MaxWebhookEventTypes {
		return nil, domain.Validation("event_types must contain between 1 and 64 entries")
	}
	allowed := make(map[string]struct{}, len(supportedWebhookEventTypes))
	for _, value := range supportedWebhookEventTypes {
		allowed[value] = struct{}{}
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, len(values))
	for index, value := range values {
		if _, ok := allowed[value]; !ok {
			return nil, domain.Validation("event_types contains unsupported event type: " + value)
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, domain.Validation("event_types must not contain duplicates")
		}
		seen[value] = struct{}{}
		out[index] = value
	}
	return out, nil
}

func SupportedWebhookEventTypes() []string {
	return append([]string(nil), supportedWebhookEventTypes...)
}
