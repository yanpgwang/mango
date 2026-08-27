// Package temporal implements the platform-spine's durable session
// orchestration: a SessionWorkflow keyed by the public session ID, granular
// model/tool Activities behind a Workflow-owned agent loop, idempotent
// PostgreSQL completion, and the outbox relay that wakes the workflow with
// Signal-With-Start.
//
// PostgreSQL remains the source of truth for public events; Temporal owns only
// in-flight orchestration. Signals carry wakeup metadata only — never event
// payloads.
package temporal

import (
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

const (
	// TaskQueue is the single task queue the session worker listens on for this
	// slice. Split queues (session vs thread vs webhook) are a later concern.
	TaskQueue = "mango-session"

	// WakeupSignalName is the Signal the outbox relay (and the API fast path)
	// send to wake a SessionWorkflow. Its payload is metadata only.
	WakeupSignalName = "session-wakeup"

	// SessionWorkflowType is the registered workflow name. The Workflow ID is the
	// public session ID, so starting the same session twice is idempotent.
	SessionWorkflowType = "SessionWorkflow"

	// SessionThreadWorkflowType is the independent durable loop for one child
	// Thread. Its Workflow ID is derived solely from the public Thread id.
	SessionThreadWorkflowType = "SessionThreadWorkflow"

	// SandboxCleanupWorkflowType is a short durable teardown workflow started
	// after the long-lived SessionWorkflow has been stopped for public deletion.
	SandboxCleanupWorkflowType = "SandboxCleanupWorkflow"
)

// WakeupSignal is the wakeup metadata delivered to a SessionWorkflow. It carries
// only the highest known public receipt sequence, never event payloads. The
// workflow loads authoritative events from PostgreSQL after its own durable
// cursor and ignores anything at or below it, so a duplicate or out-of-order
// signal cannot reorder public events or double-process a turn.
type WakeupSignal struct {
	MaxEventSeq int64 `json:"max_event_seq"`
}

// SessionWorkflowInput starts (or restarts, via Continue-As-New) a
// SessionWorkflow. StartCursor is the durable last-observed event sequence; a
// fresh session starts at 0, and Continue-As-New carries the current cursor
// forward so a new history run does not reprocess consumed events.
type SessionWorkflowInput struct {
	SessionID   string `json:"session_id"`
	StartCursor int64  `json:"start_cursor"`
}

type SessionThreadWorkflowInput struct {
	SessionID   string `json:"session_id"`
	ThreadID    string `json:"thread_id"`
	StartCursor int64  `json:"start_cursor"`
}

type ReleaseSandboxInput struct {
	SessionID string `json:"session_id"`
}

type PublishSessionOutputsInput struct {
	SessionID string `json:"session_id"`
}

type PublishSessionOutputsResult struct {
	FatalError string `json:"fatal_error,omitempty"`
}

// RunTurnResult reports whether the workflow-owned turn completed, parked on a
// client-action barrier, or terminated the session.
type RunTurnResult struct {
	Disposition TurnDisposition `json:"disposition"`
}

// TurnDisposition tells the SessionWorkflow whether a completed turn finished,
// parked on a newly committed client-action barrier, or terminated the session.
type TurnDisposition string

const (
	TurnCompleted  TurnDisposition = "completed"
	TurnParked     TurnDisposition = "parked"
	TurnTerminated TurnDisposition = "terminated"
)

// TurnToolKind is the execution owner recorded by PrepareTurn. The Workflow
// consumes this durable Activity result rather than consulting a mutable
// process-global registry while replaying.
type TurnToolKind string

const (
	TurnToolBuiltin      TurnToolKind = "builtin"
	TurnToolCustom       TurnToolKind = "custom"
	TurnToolMCP          TurnToolKind = "mcp"
	TurnToolSelfHosted   TurnToolKind = "self_hosted"
	TurnToolRuntimeSkill TurnToolKind = "runtime_skill"
	TurnToolCoordinator  TurnToolKind = "coordinator"
	TurnToolAdvisor      TurnToolKind = "advisor"
)

// TurnTool is the immutable, Workflow-facing classification of an offered tool.
type TurnTool struct {
	Name        string                  `json:"name"`
	Kind        TurnToolKind            `json:"kind"`
	Permission  domain.PermissionPolicy `json:"permission"`
	MCPServer   domain.MCPServer        `json:"mcp_server,omitempty"`
	MCPToolName string                  `json:"mcp_tool_name,omitempty"`
	Model       string                  `json:"model,omitempty"`
}

// PrepareTurnInput identifies one public trigger whose model turn should run.
type PrepareTurnInput struct {
	SessionID          string   `json:"session_id"`
	TriggerEventID     string   `json:"trigger_event_id"`
	ResolutionEventIDs []string `json:"resolution_event_ids,omitempty"`
}

// PrepareTurnResult is the immutable starting state for a Workflow-owned turn.
// The projected messages and tool definitions are Activity output, so Temporal
// records them in history and deterministic replay never rereads PostgreSQL.
type PrepareTurnResult struct {
	AlreadyCompleted      bool           `json:"already_completed"`
	Terminated            bool           `json:"terminated"`
	FatalError            string         `json:"fatal_error,omitempty"`
	SessionOutputsEnabled bool           `json:"session_outputs_enabled,omitempty"`
	AttemptID             string         `json:"attempt_id,omitempty"`
	ThreadID              string         `json:"thread_id,omitempty"`
	IsChild               bool           `json:"is_child,omitempty"`
	SkillRuntimeRoot      string         `json:"skill_runtime_root,omitempty"`
	Request               model.Request  `json:"request"`
	Tools                 []TurnTool     `json:"tools,omitempty"`
	ResumeActions         []ResumeAction `json:"resume_actions,omitempty"`
	// PreludeEvents are recoverable setup diagnostics, such as one unavailable
	// MCP server. The Workflow commits them with the turn while continuing with
	// the remaining tool surface.
	PreludeEvents []domain.EventDraft `json:"prelude_events,omitempty"`
	// UsesProviderTranscript is true when Request.Messages came from the
	// lossless private transcript rather than the legacy public-event
	// projection. TranscriptDelta contains only the new input represented by
	// this turn; Workflow code appends provider responses and tool results.
	UsesProviderTranscript bool                     `json:"uses_provider_transcript,omitempty"`
	TranscriptDelta        []domain.Message         `json:"transcript_delta,omitempty"`
	Outcome                *domain.OutcomeSpec      `json:"outcome,omitempty"`
	ContextProjection      domain.ContextProjection `json:"context_projection"`
	ContextSnapshotID      string                   `json:"context_snapshot_id,omitempty"`
}

type EvaluateOutcomeInput struct {
	SessionID    string             `json:"session_id"`
	StartEventID string             `json:"start_event_id,omitempty"`
	EndEventID   string             `json:"end_event_id,omitempty"`
	Model        string             `json:"model"`
	Effort       string             `json:"effort,omitempty"`
	Speed        string             `json:"speed,omitempty"`
	Outcome      domain.OutcomeSpec `json:"outcome"`
	Candidate    []domain.Message   `json:"candidate"`
	Iteration    int                `json:"iteration"`
	FinalCycle   bool               `json:"final_cycle"`
}

type EvaluateOutcomeResult struct {
	StartEventID string            `json:"start_event_id"`
	EndEventID   string            `json:"end_event_id"`
	Result       string            `json:"result"`
	Explanation  string            `json:"explanation"`
	Usage        domain.TokenUsage `json:"usage"`
	StopReason   string            `json:"stop_reason,omitempty"`
	FatalError   string            `json:"fatal_error,omitempty"`
}

// ResumeAction is the server-owned reconstruction of one parked tool call and
// its admitted client result. The Activity validates and normalizes the raw
// event payloads before they enter Workflow history. Confirmations also carry a
// stable journal step id for an allowed built-in execution.
type ResumeAction struct {
	ActionEventID string `json:"action_event_id"`
	// ActionEventType is the public event type PostgreSQL durably holds for the
	// parked call. The result that answers this action must be the matching
	// documented variant, and that decision cannot be made from Workflow code
	// alone: a barrier can outlive both a worker upgrade and a Continue-As-New,
	// so the answering execution may run newer code than the one that wrote the
	// call. An empty value predates this field and means the legacy
	// agent.tool_use spelling.
	ActionEventType   string                   `json:"action_event_type,omitempty"`
	Kind              domain.PendingActionKind `json:"kind"`
	ToolName          string                   `json:"tool_name"`
	Input             map[string]any           `json:"input"`
	ResolutionEventID string                   `json:"resolution_event_id"`
	Content           []any                    `json:"content,omitempty"`
	IsError           bool                     `json:"is_error,omitempty"`
	Confirmation      string                   `json:"confirmation,omitempty"`
	DenyMessage       string                   `json:"deny_message,omitempty"`
	ToolStepID        string                   `json:"tool_step_id,omitempty"`
	ProviderToolUseID string                   `json:"provider_tool_use_id,omitempty"`
}

// StartModelRequestInput identifies one logical provider request span. The
// Workflow derives the ID from Session, trigger, and model-round ordinal.
type StartModelRequestInput struct {
	SessionID           string `json:"session_id"`
	TriggerEventID      string `json:"trigger_event_id"`
	ModelRequestStartID string `json:"model_request_start_id"`
}

// AppendWorkflowEventsInput carries a completed non-terminal event prefix that
// must become visible before another model request begins.
type AppendWorkflowEventsInput struct {
	SessionID      string              `json:"session_id"`
	TriggerEventID string              `json:"trigger_event_id"`
	Events         []domain.EventDraft `json:"events"`
}

// CallModelInput is one plan/observe step. Each call is its own Activity so its
// completed response is recorded independently in Workflow history.
type CallModelInput struct {
	SessionID           string `json:"session_id"`
	ThreadID            string `json:"thread_id,omitempty"`
	ModelRequestStartID string `json:"model_request_start_id,omitempty"`
	ModelRequestEndID   string `json:"model_request_end_id,omitempty"`
	// HandleRetryableErrors opts new Workflow histories into the public retry
	// lifecycle. Older histories leave it false and retain Activity-level retry
	// behavior, which keeps replay compatible across the rollout.
	HandleRetryableErrors bool          `json:"handle_retryable_errors,omitempty"`
	Request               model.Request `json:"request"`
}

// ModelRetryError is a provider failure that may succeed without changing the
// logical request. It is an Activity result, not an Activity error, so the
// Workflow can publish the documented retry lifecycle deterministically.
type ModelRetryError struct {
	Type             string `json:"type"`
	Message          string `json:"message"`
	RetryAfterMillis int64  `json:"retry_after_millis,omitempty"`
}

// PlannedToolStep binds one public tool-use event to its internal journal step.
// Both ids are Activity output recorded in Workflow history; retries therefore
// reuse explicit operation ids rather than deriving one namespace from another.
type PlannedToolStep struct {
	ToolUseEventID    string `json:"tool_use_event_id"`
	ProviderToolUseID string `json:"provider_tool_use_id"`
	ToolStepID        string `json:"tool_step_id"`
}

// CallModelResult carries a normalized model response. Provider tool-use IDs
// remain untouched for exact replay; ToolSteps maps each one to server-owned
// public/journal IDs. MessageEventID names the public agent.message when the
// response contains non-empty text.
type CallModelResult struct {
	Response             model.Response    `json:"response"`
	MessageEventID       string            `json:"message_event_id,omitempty"`
	ThinkingEventID      string            `json:"thinking_event_id,omitempty"`
	ModelRequestStartID  string            `json:"model_request_start_id,omitempty"`
	ModelRequestEndID    string            `json:"model_request_end_id,omitempty"`
	ToolSteps            []PlannedToolStep `json:"tool_steps,omitempty"`
	FatalError           string            `json:"fatal_error,omitempty"`
	FatalErrorType       string            `json:"fatal_error_type,omitempty"`
	ContextOverflow      bool              `json:"context_overflow,omitempty"`
	ContextOverflowError string            `json:"context_overflow_error,omitempty"`
	RetryError           *ModelRetryError  `json:"retry_error,omitempty"`
}

type AccountModelRequestInput struct {
	SessionID      string            `json:"session_id"`
	ThreadID       string            `json:"thread_id,omitempty"`
	RequestEventID string            `json:"request_event_id"`
	Model          domain.Model      `json:"model"`
	Usage          domain.TokenUsage `json:"usage"`
	StopReason     string            `json:"stop_reason,omitempty"`
}

type AdmitModelRequestInput struct {
	SessionID string `json:"session_id"`
	ThreadID  string `json:"thread_id,omitempty"`
}

type AdmitModelRequestResult struct {
	Allowed bool `json:"allowed"`
}

type RecordModelRetryInput struct {
	SessionID      string          `json:"session_id"`
	ThreadID       string          `json:"thread_id,omitempty"`
	IsChild        bool            `json:"is_child,omitempty"`
	TriggerEventID string          `json:"trigger_event_id"`
	ErrorEventID   string          `json:"error_event_id"`
	StatusEventID  string          `json:"status_event_id"`
	Error          ModelRetryError `json:"error"`
}

type ResumeModelRetryInput struct {
	SessionID      string `json:"session_id"`
	ThreadID       string `json:"thread_id,omitempty"`
	IsChild        bool   `json:"is_child,omitempty"`
	TriggerEventID string `json:"trigger_event_id"`
	StatusEventID  string `json:"status_event_id"`
}

// ExecuteToolInput identifies one logical built-in tool step. ToolUseEventID is
// stable because it came from the completed CallModel Activity result.
type ExecuteToolInput struct {
	SessionID           string                     `json:"session_id"`
	ThreadID            string                     `json:"thread_id,omitempty"`
	TriggerEventID      string                     `json:"trigger_event_id"`
	AttemptID           string                     `json:"attempt_id"`
	Ordinal             int                        `json:"ordinal"`
	ToolUseEventID      string                     `json:"tool_use_event_id"`
	ToolStepID          string                     `json:"tool_step_id"`
	ToolName            string                     `json:"tool_name"`
	ToolKind            TurnToolKind               `json:"tool_kind,omitempty"`
	MCPServer           domain.MCPServer           `json:"mcp_server,omitempty"`
	MCPToolName         string                     `json:"mcp_tool_name,omitempty"`
	Input               map[string]any             `json:"input"`
	SkillRuntimeRoot    string                     `json:"skill_runtime_root,omitempty"`
	SkillAlreadyLoaded  bool                       `json:"skill_already_loaded,omitempty"`
	AdvisorRequest      model.Request              `json:"advisor_request,omitempty"`
	AdvisorConsultation domain.AdvisorConsultation `json:"advisor_consultation,omitempty"`
}

// ExecuteToolResult is the durable result of one tool Activity. Ambiguous is a
// successful Activity result (not a retryable error): the Workflow must
// terminate the turn honestly without scheduling the side effect again.
type ExecuteToolResult struct {
	Result     domain.ToolStepResult `json:"result"`
	Events     []domain.EventDraft   `json:"events,omitempty"`
	Ambiguous  bool                  `json:"ambiguous"`
	FatalError string                `json:"fatal_error,omitempty"`
}

// CompleteWorkflowTurnInput atomically finalizes the optional tool attempt and
// commits the public turn output in PostgreSQL.
type CompleteWorkflowTurnInput struct {
	SessionID      string                 `json:"session_id"`
	ThreadID       string                 `json:"thread_id,omitempty"`
	IsChild        bool                   `json:"is_child,omitempty"`
	TriggerEventID string                 `json:"trigger_event_id"`
	Output         []domain.EventDraft    `json:"output"`
	Status         domain.Status          `json:"status"`
	AttemptID      string                 `json:"attempt_id,omitempty"`
	AttemptState   domain.RunAttemptState `json:"attempt_state,omitempty"`
	AttemptError   *string                `json:"attempt_error,omitempty"`
	// PendingActionEventIDs names action events in Output that park this turn
	// awaiting client input.
	PendingActionEventIDs []string `json:"pending_action_event_ids,omitempty"`
	// ResolutionEventIDs names every client event that closes the current
	// pending-action barrier. PostgreSQL validates the set atomically.
	ResolutionEventIDs []string `json:"resolution_event_ids,omitempty"`
	// TranscriptDelta and ToolUseMappings are provider-private continuation
	// state. PostgreSQL commits them atomically with Output when supported.
	TranscriptDelta       []domain.Message                `json:"transcript_delta,omitempty"`
	ToolUseMappings       []domain.ProviderToolUseMapping `json:"tool_use_mappings,omitempty"`
	Usage                 domain.TokenUsage               `json:"usage,omitempty"`
	UsageAlreadyAccounted bool                            `json:"usage_already_accounted,omitempty"`
}

// LoadEventsInput requests the ordered public events after a cursor.
type LoadEventsInput struct {
	SessionID string `json:"session_id"`
	ThreadID  string `json:"thread_id,omitempty"`
	Cursor    int64  `json:"cursor"`
	Limit     int    `json:"limit"`
}

// EventRef is the minimal projection of a public event the workflow needs to
// decide what to do. Payloads never enter workflow history; the Activity holds
// the authoritative event.
type EventRef struct {
	ID   string `json:"id"`
	Seq  int64  `json:"seq"`
	Type string `json:"type"`
}

// LoadEventsResult carries the ordered event references after the cursor.
type LoadEventsResult struct {
	Events []EventRef `json:"events"`
}

// LoadInterruptInput asks the Activity read side for the first still-unprocessed
// interrupt after a turn's trigger sequence. Wakeup Signals remain metadata
// only; PostgreSQL decides whether an interrupt was durably admitted.
type LoadInterruptInput struct {
	SessionID string `json:"session_id"`
	ThreadID  string `json:"thread_id,omitempty"`
	AfterSeq  int64  `json:"after_seq"`
}

type LoadInterruptResult struct {
	Interrupt *EventRef `json:"interrupt,omitempty"`
}

// LoadPendingActionsInput asks PostgreSQL for the current durable
// requires_action barrier. The result, rather than mutable database state read
// directly by Workflow code, drives the Workflow selector.
type LoadPendingActionsInput struct {
	SessionID string `json:"session_id"`
	ThreadID  string `json:"thread_id,omitempty"`
}

// PendingActionRef is the minimal deterministic selector projection for one
// unresolved pending action. Payloads remain behind the Activity boundary.
type PendingActionRef struct {
	ActionEventID      string                   `json:"action_event_id"`
	ActionEventSeq     int64                    `json:"action_event_seq"`
	Kind               domain.PendingActionKind `json:"kind"`
	ResolutionEventID  string                   `json:"resolution_event_id,omitempty"`
	ResolutionEventSeq int64                    `json:"resolution_event_seq,omitempty"`
}

type LoadPendingActionsResult struct {
	Actions []PendingActionRef `json:"actions,omitempty"`
}
