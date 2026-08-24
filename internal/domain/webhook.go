package domain

import "time"

type WebhookStatus string

const (
	WebhookStatusEnabled  WebhookStatus = "enabled"
	WebhookStatusDisabled WebhookStatus = "disabled"
)

// Webhook is a Workspace-scoped outbound notification endpoint. The signing
// secret is encrypted control-plane state and must never be serialized by an
// HTTP response.
type Webhook struct {
	ID               string
	URL              string
	EventTypes       []string
	Status           WebhookStatus
	DisabledReason   *string
	SecretEnvelope   *SecretEnvelope
	FailureStartedAt *time.Time
	LastSuccessAt    *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

// WebhookDelivery is one leased endpoint/event pair. Payload contains the
// exact bytes signed and sent on every attempt; retries change only the
// attempt timestamp and signature.
type WebhookDelivery struct {
	WebhookID      string
	EventID        string
	URL            string
	SecretEnvelope SecretEnvelope
	Payload        []byte
	AttemptCount   int
	ClaimID        string
	CreatedAt      time.Time
}

const (
	WebhookEventSessionStatusScheduled        = "session.status_scheduled"
	WebhookEventSessionStatusRunStarted       = "session.status_run_started"
	WebhookEventSessionStatusIdled            = "session.status_idled"
	WebhookEventSessionStatusRescheduled      = "session.status_rescheduled"
	WebhookEventSessionStatusTerminated       = "session.status_terminated"
	WebhookEventSessionBudgetReached          = "session.budget_reached"
	WebhookEventSessionThreadCreated          = "session.thread_created"
	WebhookEventSessionThreadIdled            = "session.thread_idled"
	WebhookEventSessionThreadTerminated       = "session.thread_terminated"
	WebhookEventSessionOutcomeEvaluationEnded = "session.outcome_evaluation_ended"
	WebhookEventSessionUpdated                = "session.updated"
	WebhookEventSessionArchived               = "session.archived"
	WebhookEventSessionDeleted                = "session.deleted"
	WebhookEventDeploymentRunSucceeded        = "deployment_run.succeeded"
	WebhookEventDeploymentRunFailed           = "deployment_run.failed"
)
