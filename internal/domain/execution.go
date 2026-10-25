package domain

import (
	"encoding/json"
	"time"
)

const (
	PrefixRunAttempt = "ratm_"
	PrefixToolStep   = "tstep_"
)

// RunAttemptState is the durable lifecycle of one execution attempt for a
// logical SessionRun. A retry creates another attempt; it never rewrites the
// facts recorded by an earlier attempt.
type RunAttemptState string

const (
	RunAttemptActive      RunAttemptState = "active"
	RunAttemptCompleted   RunAttemptState = "completed"
	RunAttemptFailed      RunAttemptState = "failed"
	RunAttemptInterrupted RunAttemptState = "interrupted"
)

// RunAttempt is internal execution bookkeeping and is never serialized onto
// the Mango public API.
type RunAttempt struct {
	ID         string
	RunID      string
	AttemptNo  int
	State      RunAttemptState
	Error      *string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	FinishedAt *time.Time
}

// ToolStepState makes the side-effect uncertainty boundary explicit.
//
//	prepared -> started -> completed
//	                   \-> ambiguous
//
// Prepared means the request is durable but execution has not begun. Started
// means the executor may have changed the world. Completed carries a durable
// result. Ambiguous means execution began but no trustworthy result was
// recorded, so the step must never be silently retried.
type ToolStepState string

const (
	ToolStepPrepared  ToolStepState = "prepared"
	ToolStepStarted   ToolStepState = "started"
	ToolStepCompleted ToolStepState = "completed"
	ToolStepAmbiguous ToolStepState = "ambiguous"
)

type ToolStepResult struct {
	Content []any `json:"content"`
	// FileID references a published textual result File. FullOutput is a private
	// byte receipt retained only until publication; JSON encodes it as base64 so
	// every UTF-8 byte (including NUL) survives PostgreSQL JSONB. It never enters Workflow history.
	FileID     string `json:"file_id,omitempty"`
	FullOutput []byte `json:"full_output,omitempty"`
	IsError    bool   `json:"is_error"`
	// InjectedContent is private provider-continuation content produced by an
	// agent-runtime primitive after its ordinary tool_result. Skill activation
	// uses it to persist the rendered SKILL.md as a sibling user content block,
	// matching Claude Code's context lifecycle without exposing it as a public
	// configurable tool.
	InjectedContent []ContentBlock `json:"injected_content,omitempty"`
	// Events are server-side companion events that must commit with a durable
	// tool result and survive a lost Activity acknowledgement. They are removed
	// from the model-facing result projection.
	Events []EventDraft `json:"events,omitempty"`
	// Raw retains a bounded executor-native result for diagnostics/UI without
	// placing it in model context. Oversized native diagnostics are omitted;
	// FileID retains only the projected model-visible textual result.
	Raw     json.RawMessage `json:"raw,omitempty"`
	RawPath string          `json:"raw_path,omitempty"`
}

// ToolStep is an internal durable record of one built-in tool request and its
// execution boundary. ToolUseEventID is preassigned from the public event ID
// space so the eventual agent.tool_use and agent.tool_result can use the same
// stable correlation ID.
type ToolStep struct {
	ID             string
	AttemptID      string
	Ordinal        int
	ToolUseEventID string
	ToolName       string
	Input          map[string]any
	State          ToolStepState
	Result         *ToolStepResult
	CreatedAt      time.Time
	UpdatedAt      time.Time
	StartedAt      *time.Time
	FinishedAt     *time.Time
}
