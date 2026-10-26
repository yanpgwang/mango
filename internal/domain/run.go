package domain

import (
	"errors"
	"time"
)

// ThreadReturnCandidate identifies the actual terminal model response. It is
// internal execution metadata, not a user-supplied result or public wire type.
type ThreadReturnCandidate struct {
	MessageEventID string `json:"message_event_id"`
	StopReason     string `json:"stop_reason"`
	TextOnly       bool   `json:"text_only"`
}

type RunState string

// OutcomeGradingReservation binds trusted grading I/O to an already committed
// primary evaluation start. It is internal, never an HTTP or SDK resource.
var ErrWorkflowCompletionPending = errors.New("outcome grading waits for open Workflow Runs or unconsumed results")

type WorkflowOutcomeContinuation struct {
	EvaluationOpen                      bool
	TriggerID, StartEventID, EndEventID string
	Iteration                           int
	Outcome                             OutcomeSpec
}
type OutcomeGradingReservation struct {
	TriggerEventID                                 string
	SessionID, OutcomeID, StartEventID, EndEventID string
	Iteration                                      int
}

const (
	RunQueued    RunState = "queued"
	RunRunning   RunState = "running"
	RunCompleted RunState = "completed"
	RunFailed    RunState = "failed"
)

// SessionRun is an internal durable work item. It is never serialized onto the
// Mango public API.
type SessionRun struct {
	ID              string
	SessionID       string
	AdmissionSeq    int64
	TriggerEventIDs []string
	// OutputEventIDs are the exact committed event ids this run appended when it
	// closed (agent output plus the run's terminal/status events), persisted in
	// the same transaction that closes the run. Empty until completion. This is
	// internal-only durable state and is never serialized onto the public API.
	OutputEventIDs []string
	State          RunState
	Error          *string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
