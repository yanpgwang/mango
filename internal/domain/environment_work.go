package domain

import "time"

type EnvironmentWorkState string

const (
	EnvironmentWorkQueued   EnvironmentWorkState = "queued"
	EnvironmentWorkStarting EnvironmentWorkState = "starting"
	EnvironmentWorkActive   EnvironmentWorkState = "active"
	EnvironmentWorkStopping EnvironmentWorkState = "stopping"
	EnvironmentWorkStopped  EnvironmentWorkState = "stopped"
)

// EnvironmentWork is a durable execution lease for a self-hosted Session or
// a bounded Environment healthcheck.
type EnvironmentWork struct {
	ID            string
	EnvironmentID string
	SessionID     string
	Type          string
	ExpiresAt     *time.Time
	Result        *EnvironmentWorkResult
	// Secret is populated only for the worker that tentatively claimed this
	// activation. Persistence stores only its digest, and read/list surfaces
	// redact it.
	Secret            string
	State             EnvironmentWorkState
	Metadata          map[string]string
	CreatedAt         time.Time
	AcknowledgedAt    *time.Time
	StartedAt         *time.Time
	LatestHeartbeatAt *time.Time
	StopRequestedAt   *time.Time
	StoppedAt         *time.Time
	TTLSeconds        int64
}

type EnvironmentWorkResult struct {
	Status  string `json:"status"`
	Message string `json:"message"`
}

type EnvironmentWorkHeartbeat struct {
	LastHeartbeat time.Time
	LeaseExtended bool
	State         EnvironmentWorkState
	TTLSeconds    int64
}

type EnvironmentWorkQueueStats struct {
	Depth          int64
	Pending        int64
	OldestQueuedAt *time.Time
	WorkersPolling int64
}
