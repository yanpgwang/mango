package temporal

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"go.temporal.io/sdk/activity"

	"github.com/yanpgwang/mango/internal/agentruntime"
	"github.com/yanpgwang/mango/internal/agentruntime/tools"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/mcpclient"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/workspace"
)

// Registered activity names. Referenced by the workflow through the exported
// symbols; named explicitly so a rename cannot silently break replay.
const (
	ActivityLoadEvents           = "LoadEvents"
	ActivityLoadInterrupt        = "LoadInterrupt"
	ActivityLoadPendingActions   = "LoadPendingActions"
	ActivityPrepareTurn          = "PrepareTurn"
	ActivityAdmitModelRequest    = "AdmitModelRequest"
	ActivityStartModelRequest    = "StartModelRequest"
	ActivityAppendWorkflowEvents = "AppendWorkflowEvents"
	ActivityRecordModelRetry     = "RecordModelRetry"
	ActivityResumeModelRetry     = "ResumeModelRetry"
	ActivityCallModel            = "CallModel"
	ActivityAccountModelRequest  = "AccountModelRequest"
	ActivityEvaluateOutcome      = "EvaluateOutcome"
	ActivityExecuteTool          = "ExecuteTool"
	ActivityCompleteWorkflowTurn = "CompleteWorkflowTurn"
)

// EventSource is the read side of the PostgreSQL ledger the Activities depend
// on. The concrete implementation is *pg.Store; the interface keeps the
// Activities testable with an in-memory fake.
type EventSource interface {
	EventsAfter(ctx context.Context, sessionID string, cursor int64, limit int) ([]domain.Event, error)
	FirstUnprocessedInterruptAfter(
		ctx context.Context,
		sessionID string,
		afterSeq int64,
	) (*domain.Event, error)
	HistoryThrough(ctx context.Context, sessionID, triggerEventID string, limit int) ([]domain.Event, error)
	GetSession(ctx context.Context, id string) (domain.Session, error)
	GetEvent(ctx context.Context, sessionID, id string) (domain.Event, error)
	UnresolvedPendingActions(ctx context.Context, sessionID string) ([]domain.PendingAction, error)
	CompleteWorkflowTurn(
		ctx context.Context,
		sessionID string,
		triggerEventID string,
		output []domain.EventDraft,
		status domain.Status,
		attemptID string,
		attemptState domain.RunAttemptState,
		attemptError *string,
		pendingActionEventIDs []string,
		resolutionEventIDs []string,
	) (TurnCompletionResult, error)
}

type ThreadInterruptSource interface {
	FirstUnprocessedThreadInterruptAfter(
		ctx context.Context,
		sessionID string,
		threadID string,
		afterSeq int64,
	) (*domain.Event, error)
}

type ThreadEventSource interface {
	ThreadEventsAfter(
		context.Context, string, string, int64, int,
	) ([]domain.Event, error)
}

type ThreadPendingActionSource interface {
	UnresolvedThreadPendingActions(
		context.Context, string, string,
	) ([]domain.PendingAction, error)
}

type CoordinatorToolSource interface {
	ExecuteCoordinatorToolStep(
		context.Context, string, string, string, string, string, map[string]any,
	) (domain.ToolStepResult, error)
}

type ThreadWorkflowRetrySource interface {
	RecordThreadWorkflowRetry(
		context.Context, string, string, string, string, string, map[string]any,
	) error
	ResumeThreadWorkflowRetry(
		context.Context, string, string, string, string,
	) error
}

// UsageCompletionSource is implemented by stores that can atomically account
// model usage while completing a public turn. It is optional so lightweight
// EventSource implementations remain source-compatible.
type UsageCompletionSource interface {
	CompleteWorkflowTurnWithUsage(
		ctx context.Context,
		sessionID string,
		triggerEventID string,
		output []domain.EventDraft,
		status domain.Status,
		attemptID string,
		attemptState domain.RunAttemptState,
		attemptError *string,
		pendingActionEventIDs []string,
		resolutionEventIDs []string,
		usage domain.TokenUsage,
	) (TurnCompletionResult, error)
}

type ModelRequestUsageSource interface {
	AccountModelRequest(
		context.Context,
		string,
		string,
		string,
		domain.Model,
		domain.TokenUsage,
		string,
	) error
}

type AdvisorToolSource interface {
	CompleteAdvisorToolStep(
		context.Context,
		string,
		string,
		string,
		string,
		domain.ToolStepResult,
		domain.AdvisorConsultation,
	) error
}

type ModelRequestAdmissionSource interface {
	AdmitModelRequest(context.Context, string, string) (bool, error)
}

// ProviderTranscriptSource is the optional private-context capability supplied
// by the PostgreSQL adapter. Tests and legacy stores that do not implement it
// continue to use the public-event projection.
type ProviderTranscriptSource interface {
	LoadProviderTranscript(
		ctx context.Context,
		sessionID string,
	) (domain.ProviderTranscript, error)
}

type ThreadProviderTranscriptSource interface {
	LoadThreadProviderTranscript(
		ctx context.Context,
		sessionID string,
		threadID string,
	) (domain.ProviderTranscript, error)
}

// ThreadContextSnapshotSource persists an immutable compacted projection for
// one Thread trigger. Production PostgreSQL implements it; lightweight
// test sources may omit it and continue exercising projection in memory.
type ThreadContextSnapshotSource interface {
	GetThreadContextSnapshotForTrigger(
		ctx context.Context,
		sessionID string,
		threadID string,
		triggerEventID string,
	) (domain.ContextSnapshot, bool, error)
	PutThreadContextSnapshot(
		ctx context.Context,
		sessionID string,
		threadID string,
		triggerEventID string,
		transcriptTriggerEventIDs []string,
		messages []domain.Message,
		projection domain.ContextProjection,
	) (domain.ContextSnapshot, error)
}

// ProviderTranscriptCompletionSource atomically commits the public turn and
// the private provider transcript delta.
type ProviderTranscriptCompletionSource interface {
	CompleteWorkflowTurnWithTranscript(
		ctx context.Context,
		sessionID string,
		triggerEventID string,
		output []domain.EventDraft,
		status domain.Status,
		attemptID string,
		attemptState domain.RunAttemptState,
		attemptError *string,
		pendingActionEventIDs []string,
		resolutionEventIDs []string,
		transcriptDelta []domain.Message,
		toolUseMappings []domain.ProviderToolUseMapping,
	) (TurnCompletionResult, error)
}

// ProviderTranscriptUsageCompletionSource is the full atomic completion
// capability used by the PostgreSQL adapter.
type ProviderTranscriptUsageCompletionSource interface {
	CompleteWorkflowTurnWithTranscriptAndUsage(
		ctx context.Context,
		sessionID string,
		triggerEventID string,
		output []domain.EventDraft,
		status domain.Status,
		attemptID string,
		attemptState domain.RunAttemptState,
		attemptError *string,
		pendingActionEventIDs []string,
		resolutionEventIDs []string,
		transcriptDelta []domain.Message,
		toolUseMappings []domain.ProviderToolUseMapping,
		usage domain.TokenUsage,
	) (TurnCompletionResult, error)
}

type ThreadCompletionSource interface {
	CompleteThreadWorkflowTurn(
		ctx context.Context,
		sessionID string,
		threadID string,
		triggerEventID string,
		output []domain.EventDraft,
		status domain.Status,
		attemptID string,
		attemptState domain.RunAttemptState,
		attemptError *string,
		pendingActionEventIDs []string,
		resolutionEventIDs []string,
		transcriptDelta []domain.Message,
		toolUseMappings []domain.ProviderToolUseMapping,
		usage domain.TokenUsage,
	) (TurnCompletionResult, error)
}

// WorkflowEventSource appends already-completed public progress without
// processing the turn trigger or changing the Session projection. PostgreSQL
// implements it idempotently by explicit event ID; the Workflow uses it to
// make model-span starts and completed intermediate rounds visible in order.
type WorkflowEventSource interface {
	AppendWorkflowEvents(
		ctx context.Context,
		sessionID string,
		triggerEventID string,
		drafts []domain.EventDraft,
	) error
}

// WorkflowRetrySource atomically publishes retry status events with the
// corresponding Session projection transition.
type WorkflowRetrySource interface {
	RecordWorkflowRetry(
		ctx context.Context,
		sessionID string,
		triggerEventID string,
		errorEventID string,
		statusEventID string,
		errorPayload map[string]any,
	) error
	ResumeWorkflowRetry(
		ctx context.Context,
		sessionID string,
		triggerEventID string,
		statusEventID string,
	) error
}

// MCPDiscoveryStore pins the discovered tool surface for each Thread/server.
// Threads share a Session sandbox, but their Agent configurations and remote
// tool surfaces remain private. The first PrepareTurn discovers remotely;
// later turns in that Thread reuse the durable snapshot.
type MCPDiscoveryStore interface {
	GetMCPDiscoverySnapshot(
		ctx context.Context,
		sessionID string,
		threadID string,
		server domain.MCPServer,
	) ([]mcpclient.Tool, bool, error)
	PutMCPDiscoverySnapshot(
		ctx context.Context,
		sessionID string,
		threadID string,
		server domain.MCPServer,
		tools []mcpclient.Tool,
	) ([]mcpclient.Tool, error)
}

// JournalStore is the durable tool-execution journal used by the granular
// ExecuteTool Activity. It preserves the prepared/started/completed/ambiguous
// boundary across Activity retries. *pg.Store implements it.
type JournalStore interface {
	EnsureAttempt(ctx context.Context, sessionID, triggerEventID, attemptID string) error
	EnsureToolStep(ctx context.Context, attemptID, stepID string, ordinal int, toolUseEventID, toolName string, input map[string]any) (domain.ToolStep, error)
	StartToolStep(ctx context.Context, stepID string) error
	CompleteToolStep(ctx context.Context, stepID string, result domain.ToolStepResult) error
	MarkToolStepAmbiguous(ctx context.Context, stepID string) error
}

// SkillInstructionLoader reads the immutable instruction entry owned by the
// control plane. The Agent loop uses the same canonical source for every
// execution environment; self-hosted workers independently receive the pinned
// bundle for supporting-file access, without reverse filesystem access.
type SkillInstructionLoader interface {
	LoadSkillInstructions(context.Context, domain.SkillVersion) ([]byte, error)
}

type SessionSkillSource interface {
	SessionSkillsForRuntime(context.Context, string) ([]domain.SkillVersion, error)
}

type SessionThreadSkillSource interface {
	SessionThreadSkillRuntime(
		context.Context,
		string,
		string,
	) (domain.SkillRuntime, error)
}

type SessionThreadSource interface {
	GetSessionThread(context.Context, string, string) (domain.SessionThread, error)
}

// PreviewPublisher carries best-effort model deltas to live subscribers. It is
// never part of turn correctness and may be nil.
type PreviewPublisher interface {
	PublishPreview(context.Context, string, domain.PreviewFrame) error
}

// TurnCompletionResult mirrors pg.TurnCompletion without importing the pg
// package into the workflow-facing types, keeping the domain boundary intact.
// Status is the session's projected status after the completion committed, so
// the Activity can tell the workflow whether the session is now terminated.
type TurnCompletionResult struct {
	Events  []domain.Event
	Applied bool
	Status  domain.Status
	// Parked is a pointer for Workflow-history compatibility. Activity results
	// recorded before durable interrupt support have no field; nil tells
	// CompleteWorkflowTurn to use the legacy input-derived disposition. New
	// results always carry an explicit true/false from PostgreSQL.
	Parked *bool
}

// historyScanLimit bounds the ledger scan used to verify transcript coverage
// and support legacy sessions. The actual model-context bound is token-aware
// and applied after projection.
const historyScanLimit = 10000

// toolResultWriteAttempts gives a known in-memory tool result a brief chance to
// cross a transient PostgreSQL outage before the Activity returns an error. A
// later Activity retry must conservatively classify a still-started step as
// ambiguous, so this bounded write-only retry belongs before that boundary.
const toolResultWriteAttempts = 3

// Activities holds the I/O dependencies of the session Activities. Self-hosted
// built-ins execute through Environment Work; only control-plane tools such as
// MCP, coordinator, Advisor, and Skill activation execute here.
type Activities struct {
	modelClient        model.Client
	source             EventSource
	journal            JournalStore
	ids                domain.IDGenerator
	previews           PreviewPublisher
	mcp                mcpclient.Client
	mcpAuth            mcpclient.AuthSource
	contextTokenBudget int
	skillInstructions  SkillInstructionLoader
}

func NewActivities(
	modelClient model.Client,
	source EventSource,
	journal JournalStore,
	ids domain.IDGenerator,
	previewPublisher ...PreviewPublisher,
) *Activities {
	activities := &Activities{
		modelClient: modelClient, source: source,
		journal: journal, ids: ids,
		mcp: mcpclient.NewRemote(nil),
	}
	if len(previewPublisher) > 0 {
		activities.previews = previewPublisher[0]
	}
	return activities
}

// WithContextTokenBudget overrides the request-time message budget. It is
// primarily useful for deterministic conformance tests and smaller providers.
func (a *Activities) WithContextTokenBudget(tokens int) *Activities {
	a.contextTokenBudget = tokens
	return a
}

// WithMCPClient replaces the remote MCP adapter. Production uses the official
// Go SDK-backed client by default; tests can inject a deterministic fake.
func (a *Activities) WithMCPClient(client mcpclient.Client) *Activities {
	a.mcp = client
	return a
}

func (a *Activities) WithMCPAuthSource(source mcpclient.AuthSource) *Activities {
	a.mcpAuth = source
	return a
}

func (a *Activities) WithSkillInstructionLoader(loader SkillInstructionLoader) *Activities {
	a.skillInstructions = loader
	return a
}

// LoadEvents returns the ordered public event references after a cursor. Only
// metadata (id, seq, type) crosses back into workflow history; payloads stay in
// PostgreSQL.
func (a *Activities) LoadEvents(ctx context.Context, in LoadEventsInput) (LoadEventsResult, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = loadBatchLimit
	}
	var events []domain.Event
	var err error
	if in.ThreadID != "" {
		threadSource, ok := a.source.(ThreadEventSource)
		if !ok {
			return LoadEventsResult{}, fmt.Errorf(
				"temporal: event source cannot read child Thread events",
			)
		}
		events, err = threadSource.ThreadEventsAfter(
			ctx, in.SessionID, in.ThreadID, in.Cursor, limit,
		)
	} else {
		events, err = a.source.EventsAfter(ctx, in.SessionID, in.Cursor, limit)
	}
	if err != nil {
		return LoadEventsResult{}, err
	}
	refs := make([]EventRef, 0, len(events))
	for _, e := range events {
		refs = append(refs, EventRef{ID: e.ID, Seq: e.Sequence, Type: e.Type})
	}
	return LoadEventsResult{Events: refs}, nil
}

// LoadInterrupt scans authoritative history after one active trigger for the
// first unprocessed interrupt. It runs once before a turn's first interruptible
// Activity, after later wakeups, or while a durable pending-action barrier is
// parked. Signals remain metadata and ordinary model/tool progress does not poll
// PostgreSQL continuously.
func (a *Activities) LoadInterrupt(
	ctx context.Context,
	in LoadInterruptInput,
) (LoadInterruptResult, error) {
	var event *domain.Event
	var err error
	if in.ThreadID == "" {
		event, err = a.source.FirstUnprocessedInterruptAfter(
			ctx, in.SessionID, in.AfterSeq,
		)
	} else {
		source, ok := a.source.(ThreadInterruptSource)
		if !ok {
			return LoadInterruptResult{}, fmt.Errorf(
				"temporal: event source cannot load child Thread interrupts",
			)
		}
		event, err = source.FirstUnprocessedThreadInterruptAfter(
			ctx, in.SessionID, in.ThreadID, in.AfterSeq,
		)
	}
	if err != nil {
		return LoadInterruptResult{}, err
	}
	if event == nil {
		return LoadInterruptResult{}, nil
	}
	return LoadInterruptResult{Interrupt: &EventRef{
		ID: event.ID, Seq: event.Sequence, Type: event.Type,
	}}, nil
}

// LoadPendingActions returns the durable requires_action barrier as a small
// selector projection. The Workflow uses only this recorded Activity result to
// choose between parking, resuming the full barrier, and consuming ordinary
// messages.
func (a *Activities) LoadPendingActions(
	ctx context.Context,
	in LoadPendingActionsInput,
) (LoadPendingActionsResult, error) {
	var pending []domain.PendingAction
	var err error
	if in.ThreadID != "" {
		source, ok := a.source.(ThreadPendingActionSource)
		if !ok {
			return LoadPendingActionsResult{}, fmt.Errorf(
				"temporal: event source cannot read child Thread pending actions",
			)
		}
		pending, err = source.UnresolvedThreadPendingActions(
			ctx, in.SessionID, in.ThreadID,
		)
	} else {
		pending, err = a.source.UnresolvedPendingActions(ctx, in.SessionID)
	}
	if err != nil {
		return LoadPendingActionsResult{}, err
	}
	result := LoadPendingActionsResult{
		Actions: make([]PendingActionRef, 0, len(pending)),
	}
	for _, action := range pending {
		actionEvent, err := a.source.GetEvent(ctx, in.SessionID, action.ActionEventID)
		if err != nil {
			return LoadPendingActionsResult{}, err
		}
		ref := PendingActionRef{
			ActionEventID:  action.ActionEventID,
			ActionEventSeq: actionEvent.Sequence,
			Kind:           action.Kind,
		}
		if action.ResolvingEventID != nil {
			resolution, err := a.source.GetEvent(ctx, in.SessionID, *action.ResolvingEventID)
			if err != nil {
				return LoadPendingActionsResult{}, err
			}
			ref.ResolutionEventID = resolution.ID
			ref.ResolutionEventSeq = resolution.Sequence
		}
		result.Actions = append(result.Actions, ref)
	}
	sort.Slice(result.Actions, func(i, j int) bool {
		if result.Actions[i].ActionEventSeq == result.Actions[j].ActionEventSeq {
			return result.Actions[i].ActionEventID < result.Actions[j].ActionEventID
		}
		return result.Actions[i].ActionEventSeq < result.Actions[j].ActionEventSeq
	})
	return result, nil
}

// StartModelRequest durably publishes the span start before the long-running
// CallModel Activity can emit any best-effort preview. The explicit Workflow-
// owned ID makes Activity retries harmless.
func (a *Activities) StartModelRequest(
	ctx context.Context,
	in StartModelRequestInput,
) error {
	if in.ModelRequestStartID == "" {
		return domain.Validation("model request start id is required")
	}
	return a.appendWorkflowEvents(ctx, AppendWorkflowEventsInput{
		SessionID:      in.SessionID,
		TriggerEventID: in.TriggerEventID,
		Events: []domain.EventDraft{{
			ID:      in.ModelRequestStartID,
			Type:    domain.EvSpanModelRequestStart,
			Payload: map[string]any{},
		}},
	})
}

// AppendWorkflowEvents publishes a completed, non-terminal prefix before the
// next model request starts. Final status, pending barriers, usage, attempts,
// and provider transcript still commit through CompleteWorkflowTurn.
func (a *Activities) AppendWorkflowEvents(
	ctx context.Context,
	in AppendWorkflowEventsInput,
) error {
	return a.appendWorkflowEvents(ctx, in)
}

// RecordModelRetry publishes the retrying error and rescheduled projection in
// one PostgreSQL transaction. Keeping them together prevents observers from
// seeing a retry error while the Session still claims to be running.
func (a *Activities) RecordModelRetry(
	ctx context.Context,
	in RecordModelRetryInput,
) error {
	errorPayload := map[string]any{
		"type":    in.Error.Type,
		"message": in.Error.Message,
		"retry_status": map[string]any{
			"type": "retrying",
		},
	}
	if in.IsChild {
		source, ok := a.source.(ThreadWorkflowRetrySource)
		if !ok {
			return fmt.Errorf("temporal: event source does not support child Thread retries")
		}
		return source.RecordThreadWorkflowRetry(
			ctx, in.SessionID, in.ThreadID, in.TriggerEventID,
			in.ErrorEventID, in.StatusEventID, errorPayload,
		)
	}
	source, ok := a.source.(WorkflowRetrySource)
	if !ok {
		return fmt.Errorf("temporal: event source does not support workflow retries")
	}
	return source.RecordWorkflowRetry(
		ctx,
		in.SessionID,
		in.TriggerEventID,
		in.ErrorEventID,
		in.StatusEventID,
		errorPayload,
	)
}

// ResumeModelRetry publishes status_running at the same linearization point
// that the Session projection returns to running.
func (a *Activities) ResumeModelRetry(
	ctx context.Context,
	in ResumeModelRetryInput,
) error {
	if in.IsChild {
		source, ok := a.source.(ThreadWorkflowRetrySource)
		if !ok {
			return fmt.Errorf("temporal: event source does not support child Thread retries")
		}
		return source.ResumeThreadWorkflowRetry(
			ctx, in.SessionID, in.ThreadID, in.TriggerEventID,
			in.StatusEventID,
		)
	}
	source, ok := a.source.(WorkflowRetrySource)
	if !ok {
		return fmt.Errorf("temporal: event source does not support workflow retries")
	}
	return source.ResumeWorkflowRetry(
		ctx,
		in.SessionID,
		in.TriggerEventID,
		in.StatusEventID,
	)
}

func (a *Activities) appendWorkflowEvents(
	ctx context.Context,
	in AppendWorkflowEventsInput,
) error {
	source, ok := a.source.(WorkflowEventSource)
	if !ok {
		return errors.New("temporal: event source cannot append workflow progress")
	}
	if in.SessionID == "" || in.TriggerEventID == "" || len(in.Events) == 0 {
		return domain.Validation("session, trigger, and workflow events are required")
	}
	return source.AppendWorkflowEvents(
		ctx,
		in.SessionID,
		in.TriggerEventID,
		in.Events,
	)
}

// PrepareTurn reads one turn's immutable starting state from PostgreSQL. The
// result becomes Workflow history; replay never performs these reads in
// Workflow code.
func (a *Activities) PrepareTurn(ctx context.Context, in PrepareTurnInput) (PrepareTurnResult, error) {
	trigger, err := a.source.GetEvent(ctx, in.SessionID, in.TriggerEventID)
	if err != nil {
		return PrepareTurnResult{}, err
	}
	session, err := a.source.GetSession(ctx, in.SessionID)
	if err != nil {
		return PrepareTurnResult{}, err
	}
	executionAgent := session.AgentSnapshot
	var executionThread *domain.SessionThread
	if trigger.ThreadID != "" {
		if threads, ok := a.source.(SessionThreadSource); ok {
			thread, err := threads.GetSessionThread(ctx, in.SessionID, trigger.ThreadID)
			if err != nil {
				return PrepareTurnResult{}, err
			}
			executionAgent = thread.Agent
			executionThread = &thread
		}
	}
	if executionThread != nil && executionThread.ParentThreadID != nil &&
		(executionThread.ArchivedAt != nil ||
			executionThread.Status == domain.StatusTerminated) {
		return PrepareTurnResult{
			AlreadyCompleted: true,
			Terminated:       true,
			ThreadID:         executionThread.ID,
			IsChild:          true,
		}, nil
	}
	var pending []domain.PendingAction
	if len(in.ResolutionEventIDs) > 0 {
		if executionThread != nil && executionThread.ParentThreadID != nil {
			source, ok := a.source.(ThreadPendingActionSource)
			if !ok {
				return PrepareTurnResult{}, fmt.Errorf(
					"temporal: event source cannot read Thread pending actions",
				)
			}
			pending, err = source.UnresolvedThreadPendingActions(
				ctx, in.SessionID, trigger.ThreadID,
			)
		} else {
			pending, err = a.source.UnresolvedPendingActions(ctx, in.SessionID)
		}
		if err != nil {
			return PrepareTurnResult{}, err
		}
	}
	if trigger.ProcessedAt != nil && !pendingBarrierContainsTrigger(pending, trigger.ID) {
		completed := true
		if trigger.Type == domain.EvUserDefineOutcome {
			outcomeID, _ := trigger.Payload["outcome_id"].(string)
			active := session.ActiveOutcome()
			// define_outcome is stamped processed_at on receipt by the public
			// contract, before its asynchronous work completes. It remains runnable
			// only while the matching Session outcome projection is active.
			completed = active == nil || active.OutcomeID != outcomeID
		}
		if completed {
			return PrepareTurnResult{
				AlreadyCompleted: true,
				Terminated:       session.Status == domain.StatusTerminated,
			}, nil
		}
	}
	history, err := a.source.HistoryThrough(ctx, in.SessionID, in.TriggerEventID, historyScanLimit)
	if err != nil {
		return PrepareTurnResult{}, err
	}
	toolSet, err := domain.ParseTools(executionAgent.Tools)
	if err != nil {
		return PrepareTurnResult{FatalError: "invalid toolset: " + err.Error()}, nil
	}
	if err := domain.ValidateStoredToolConfiguration(
		executionAgent.Tools,
		executionAgent.MCPServers,
	); err != nil {
		return PrepareTurnResult{
			FatalError: "invalid tool configuration: " + err.Error(),
		}, nil
	}
	if err := domain.ValidateSkillToolConfiguration(
		executionAgent.Tools,
		len(executionAgent.Skills) > 0,
	); err != nil {
		return PrepareTurnResult{
			FatalError: "invalid Skill tool configuration: " + err.Error(),
		}, nil
	}
	if err := agentruntime.ValidateToolCapabilities(toolSet); err != nil {
		return PrepareTurnResult{FatalError: "unsupported tool capability: " + err.Error()}, nil
	}
	runtimeSkills := domain.SkillRuntime{Root: domain.SessionSkillsRoot}
	if len(executionAgent.Skills) > 0 {
		if a.skillInstructions == nil {
			return PrepareTurnResult{
				FatalError: "custom Skill instruction loading is unavailable",
			}, nil
		}
		if source, ok := a.source.(SessionThreadSkillSource); ok && trigger.ThreadID != "" {
			runtimeSkills, err = source.SessionThreadSkillRuntime(
				ctx, in.SessionID, trigger.ThreadID,
			)
		} else if source, ok := a.source.(SessionSkillSource); ok {
			runtimeSkills.Versions, err = source.SessionSkillsForRuntime(ctx, in.SessionID)
		} else {
			return PrepareTurnResult{
				FatalError: "custom Skill runtime metadata is unavailable",
			}, nil
		}
		if err != nil {
			return PrepareTurnResult{}, err
		}
		var valid bool
		runtimeSkills, valid = selfHostedSkillRuntime(runtimeSkills)
		if !valid {
			return PrepareTurnResult{
				FatalError: "custom Skill runtime path is outside the Skill root",
			}, nil
		}
		if validationErr := validateRuntimeSkillPins(
			executionAgent.Skills, runtimeSkills.Versions,
		); validationErr != "" {
			return PrepareTurnResult{FatalError: validationErr}, nil
		}
	}

	system := ""
	if executionAgent.System != nil {
		system = *executionAgent.System
	}
	system = domain.ProjectSystemContext(system, history, trigger)
	system = domain.ProjectSessionResourceContext(system, session.Resources)
	system = domain.ProjectSkillRuntimeContext(
		system,
		runtimeSkills,
		a.contextTokenBudget/100,
	)
	toolSchemas := agentruntime.EnabledSelfHostedToolSchemas(toolSet)
	if len(runtimeSkills.Versions) > 0 {
		toolSchemas = append(toolSchemas, agentruntime.RuntimeSkillToolSchema())
	}
	result := PrepareTurnResult{
		AttemptID:        a.ids.NewID(domain.PrefixRunAttempt),
		ThreadID:         trigger.ThreadID,
		IsChild:          executionThread != nil && executionThread.ParentThreadID != nil,
		SkillRuntimeRoot: runtimeSkills.Root,
		Request: model.Request{
			Model:  executionAgent.Model.ID,
			System: system,
			Tools:  toolSchemas,
		},
	}
	if executionAgent.Multiagent.HasCallableAgents() && !result.IsChild {
		result.Request.System = agentruntime.ProjectCoordinatorSystemContext(
			result.Request.System,
		)
		result.Request.Tools = append(
			result.Request.Tools,
			agentruntime.CoordinatorToolSchemas()...,
		)
		for _, name := range []string{
			agentruntime.ListAgentsToolName,
			agentruntime.SendToAgentToolName,
		} {
			result.Tools = append(result.Tools, TurnTool{
				Name: name, Kind: TurnToolCoordinator,
				Permission: domain.PermissionPolicy{Type: "always_allow"},
			})
		}
	}
	if advisor := executionAgent.Multiagent.Advisor(); advisor != nil && !result.IsChild {
		result.Request.System = agentruntime.ProjectAdvisorSystemContext(
			result.Request.System,
		)
		result.Request.Tools = append(result.Request.Tools, agentruntime.AdvisorToolSchema())
		result.Tools = append(result.Tools, TurnTool{
			Name: agentruntime.AdvisorToolName, Kind: TurnToolAdvisor,
			Permission: domain.PermissionPolicy{Type: "always_allow"},
			Model:      advisor.Model,
		})
	}
	if trigger.Type == domain.EvUserDefineOutcome {
		outcomeID, _ := trigger.Payload["outcome_id"].(string)
		description, _ := trigger.Payload["description"].(string)
		rubricContent, rubricOK := domain.OutcomeRubricContent(trigger.Payload)
		maxIterations := 3
		if configured := intValue(trigger.Payload["max_iterations"]); configured > 0 {
			maxIterations = configured
		}
		if outcomeID == "" || description == "" || !rubricOK || rubricContent == "" {
			result.FatalError = "define_outcome is missing its server id, description, or rubric"
		} else {
			result.Outcome = &domain.OutcomeSpec{
				OutcomeID: outcomeID, Description: description,
				Rubric: map[string]any{
					"type": "text", "content": rubricContent,
				},
				MaxIterations: maxIterations,
			}
		}
	}
	if executionAgent.Model.EffortExplicit {
		result.Request.Effort = executionAgent.Model.Effort
	}
	if executionAgent.Model.SpeedExplicit {
		result.Request.Speed = executionAgent.Model.Speed
	}
	originalHistory := history
	if len(in.ResolutionEventIDs) > 0 {
		resumeActions, err := a.prepareResumeActions(
			ctx,
			in.SessionID,
			trigger.ID,
			in.ResolutionEventIDs,
			pending,
		)
		if err != nil {
			var domainErr *domain.DomainError
			if errors.As(err, &domainErr) {
				return PrepareTurnResult{
					FatalError: "invalid pending-action resume: " + err.Error(),
				}, nil
			}
			return PrepareTurnResult{}, err
		}
		result.ResumeActions = resumeActions
		history = withoutResumeEvents(history, resumeActions)
	}
	result.Request.Messages = domain.ProjectMessages(history)
	var snapshotTranscriptEventIDs []string
	if transcriptSource, ok := a.source.(ProviderTranscriptSource); ok {
		var transcript domain.ProviderTranscript
		if threadSource, ok := a.source.(ThreadProviderTranscriptSource); ok &&
			trigger.ThreadID != "" {
			transcript, err = threadSource.LoadThreadProviderTranscript(
				ctx, in.SessionID, trigger.ThreadID,
			)
		} else {
			transcript, err = transcriptSource.LoadProviderTranscript(ctx, in.SessionID)
		}
		if err != nil {
			return PrepareTurnResult{}, err
		}
		if transcriptCoversPriorTurns(
			transcript,
			originalHistory,
			trigger.ID,
			in.ResolutionEventIDs,
		) {
			mappings := make(map[string]string, len(transcript.ToolUseMappings))
			for _, mapping := range transcript.ToolUseMappings {
				mappings[mapping.PublicEventID] = mapping.ProviderToolUseID
			}
			usable := true
			for i := range result.ResumeActions {
				providerID := mappings[result.ResumeActions[i].ActionEventID]
				if providerID == "" {
					usable = false
					break
				}
				result.ResumeActions[i].ProviderToolUseID = providerID
			}
			if usable {
				var delta []domain.Message
				if len(in.ResolutionEventIDs) == 0 {
					delta = domain.ProjectMessages([]domain.Event{trigger})
				}
				result.UsesProviderTranscript = true
				result.TranscriptDelta = delta
				result.Request.Messages = agentruntime.AppendMerging(
					append([]domain.Message(nil), transcript.Messages...),
					delta,
				)
				snapshotTranscriptEventIDs = append(
					[]string(nil), transcript.TriggerEventIDs...,
				)
				if len(in.ResolutionEventIDs) > 0 {
					snapshotTranscriptEventIDs = append(
						snapshotTranscriptEventIDs,
						in.ResolutionEventIDs...,
					)
				} else {
					snapshotTranscriptEventIDs = append(
						snapshotTranscriptEventIDs,
						trigger.ID,
					)
				}
			}
		}
	}
	if len(runtimeSkills.Versions) > 0 {
		result.Tools = append(result.Tools, TurnTool{
			Name:       agentruntime.RuntimeSkillToolName,
			Kind:       TurnToolRuntimeSkill,
			Permission: domain.PermissionPolicy{Type: "always_allow"},
		})
	}
	for _, name := range domain.BuiltinToolNames {
		if name == "web_search" || name == "web_fetch" {
			// Web tools execute inside the model request in either Environment.
			// Never authorize an ordinary client tool_use for them: it must not
			// create a sandbox Activity or an external tool-result barrier.
			continue
		}
		enabled, policy := toolSet.BuiltinEnabled(name)
		if enabled {
			result.Tools = append(result.Tools, TurnTool{
				Name: name, Kind: TurnToolSelfHosted, Permission: policy,
			})
		}
	}
	for _, custom := range toolSet.Custom {
		result.Tools = append(result.Tools, TurnTool{
			Name: custom.Name, Kind: TurnToolCustom,
		})
	}
	if len(toolSet.MCP) > 0 || len(executionAgent.MCPServers) > 0 {
		setupEvents, err := a.addMCPTools(
			ctx,
			in.SessionID,
			trigger.ThreadID,
			executionAgent.MCPServers,
			toolSet,
			&result,
		)
		if err != nil {
			var domainErr *domain.DomainError
			if errors.As(err, &domainErr) {
				return PrepareTurnResult{
					FatalError: "mcp capability resolution failed: " + err.Error(),
				}, nil
			}
			return PrepareTurnResult{}, err
		}
		result.PreludeEvents = append(result.PreludeEvents, setupEvents...)
	}
	preparedContext, err := prepareRequestContext(
		result.Request,
		false,
		a.contextTokenBudget,
	)
	if err != nil {
		return PrepareTurnResult{FatalError: err.Error()}, nil
	}
	result.Request = preparedContext.Request
	result.ContextProjection = preparedContext.Projection
	if result.ThreadID != "" {
		if snapshots, ok := a.source.(ThreadContextSnapshotSource); ok {
			snapshot, found, err := snapshots.GetThreadContextSnapshotForTrigger(
				ctx,
				in.SessionID,
				result.ThreadID,
				trigger.ID,
			)
			if err != nil {
				return PrepareTurnResult{}, err
			}
			if !found && result.ContextProjection.Compacted {
				snapshot, err = snapshots.PutThreadContextSnapshot(
					ctx,
					in.SessionID,
					result.ThreadID,
					trigger.ID,
					snapshotTranscriptEventIDs,
					result.Request.Messages,
					result.ContextProjection,
				)
				if err != nil {
					return PrepareTurnResult{}, err
				}
				found = true
			}
			if found {
				// The first committed snapshot is authoritative for an Activity
				// retry, even when an upgraded worker would no longer compact or
				// would project the same transcript differently.
				result.ContextSnapshotID = snapshot.ID
				result.Request.Messages = snapshot.Messages
				result.ContextProjection = snapshot.Projection
			}
		}
	}
	return result, nil
}

func validateRuntimeSkillPins(
	references []domain.SkillReference,
	versions []domain.SkillVersion,
) string {
	if len(references) != len(versions) {
		return "custom Skill runtime pins do not match the Session snapshot"
	}
	seenNames := make(map[string]struct{}, len(versions))
	var expandedBytes int64
	for index, reference := range references {
		version := versions[index]
		if reference.IsLegacy() || reference.Type != "custom" ||
			reference.SkillID != version.SkillID || reference.Version != version.Version {
			return "custom Skill runtime pins do not match the Session snapshot"
		}
		if _, exists := seenNames[version.Name]; exists {
			return "custom Skill runtime names conflict"
		}
		seenNames[version.Name] = struct{}{}
		expanded, valid := app.SkillExpandedBudgetBytes(version.UncompressedSizeBytes)
		if !valid || expanded > app.MaxSessionSkillBytes-expandedBytes {
			return "custom Skills exceed the expanded-size limit"
		}
		expandedBytes += expanded
	}
	return ""
}

func requestContextOverhead(request model.Request) int {
	overhead := domain.EstimateTextTokens(request.System)
	if encoded, err := json.Marshal(request.Tools); err == nil {
		overhead += domain.EstimateTextTokens(string(encoded))
	}
	return overhead
}

func (a *Activities) addMCPTools(
	ctx context.Context,
	sessionID string,
	threadID string,
	rawServers []any,
	toolSet domain.ToolSet,
	result *PrepareTurnResult,
) ([]domain.EventDraft, error) {
	if a.mcp == nil {
		return nil, domain.Validation("MCP client is not configured")
	}
	servers, err := domain.ParseMCPServers(rawServers)
	if err != nil {
		return nil, domain.Validation(err.Error())
	}
	var setupEvents []domain.EventDraft
	referenced := make(map[string]struct{}, len(toolSet.MCP))
	aliases := make(map[string]struct{})
	for _, configured := range result.Request.Tools {
		aliases[configured.Name] = struct{}{}
	}
	for _, configured := range toolSet.MCP {
		server, ok := servers[configured.ServerName]
		if !ok {
			return nil, domain.Validation(fmt.Sprintf(
				"mcp_toolset references unknown server %q",
				configured.ServerName,
			))
		}
		referenced[server.Name] = struct{}{}
		var discovered []mcpclient.Tool
		if snapshots, ok := a.source.(MCPDiscoveryStore); ok {
			var found bool
			discovered, found, err = snapshots.GetMCPDiscoverySnapshot(
				ctx,
				sessionID,
				threadID,
				server,
			)
			if err != nil {
				return nil, err
			}
			if !found {
				discovered, err = a.discoverMCP(ctx, sessionID, server)
				if err != nil {
					failure := mcpConnectionFailureEvent(server)
					if mcpclient.IsAuthenticationError(err) {
						failure = mcpAuthenticationFailureEvent(server)
					}
					setupEvents = append(
						setupEvents,
						failure,
					)
					continue
				}
				discovered, err = snapshots.PutMCPDiscoverySnapshot(
					ctx,
					sessionID,
					threadID,
					server,
					discovered,
				)
				if err != nil {
					return nil, err
				}
			}
		} else {
			discovered, err = a.discoverMCP(ctx, sessionID, server)
			if err != nil {
				failure := mcpConnectionFailureEvent(server)
				if mcpclient.IsAuthenticationError(err) {
					failure = mcpAuthenticationFailureEvent(server)
				}
				setupEvents = append(
					setupEvents,
					failure,
				)
				continue
			}
		}
		for _, remoteTool := range discovered {
			enabled, policy := configured.ToolEnabled(remoteTool.Name)
			if !enabled {
				continue
			}
			if policy.Type != "always_allow" && policy.Type != "always_ask" {
				return nil, domain.Validation(fmt.Sprintf(
					"mcp tool %s/%s has unsupported permission %q",
					server.Name,
					remoteTool.Name,
					policy.Type,
				))
			}
			alias := mcpModelToolName(server.Name, remoteTool.Name)
			if _, duplicate := aliases[alias]; duplicate {
				return nil, domain.Validation(fmt.Sprintf(
					"MCP model tool name collision %q",
					alias,
				))
			}
			aliases[alias] = struct{}{}
			result.Request.Tools = append(
				result.Request.Tools,
				model.ToolSchema{
					Name:        alias,
					Description: remoteTool.Description,
					InputSchema: remoteTool.InputSchema,
				},
			)
			result.Tools = append(result.Tools, TurnTool{
				Name:        alias,
				Kind:        TurnToolMCP,
				Permission:  policy,
				MCPServer:   server,
				MCPToolName: remoteTool.Name,
			})
		}
	}
	for name := range servers {
		if _, ok := referenced[name]; !ok {
			return nil, domain.Validation(fmt.Sprintf(
				"MCP server %q has no matching mcp_toolset",
				name,
			))
		}
	}
	return setupEvents, nil
}

func (a *Activities) discoverMCP(
	ctx context.Context,
	sessionID string,
	server domain.MCPServer,
) ([]mcpclient.Tool, error) {
	if authenticated, ok := a.mcp.(mcpclient.AuthenticatedClient); ok {
		return authenticated.DiscoverAuthenticated(ctx, sessionID, server, a.mcpAuth)
	}
	return a.mcp.Discover(ctx, server)
}

func mcpConnectionFailureEvent(server domain.MCPServer) domain.EventDraft {
	return domain.EventDraft{
		Type: domain.EvSessionError,
		Payload: map[string]any{
			"error": map[string]any{
				"type":            "mcp_connection_failed_error",
				"message":         "Could not connect to MCP server " + server.Name + ".",
				"mcp_server_name": server.Name,
				"retry_status": map[string]any{
					"type": "exhausted",
				},
			},
		},
	}
}

func mcpAuthenticationFailureEvent(server domain.MCPServer) domain.EventDraft {
	return domain.EventDraft{
		Type: domain.EvSessionError,
		Payload: map[string]any{
			"error": map[string]any{
				"type":            "mcp_authentication_failed_error",
				"message":         "Authentication failed for MCP server " + server.Name + ".",
				"mcp_server_name": server.Name,
				"retry_status": map[string]any{
					"type": "exhausted",
				},
			},
		},
	}
}

func mcpModelToolName(serverName, toolName string) string {
	sanitize := func(value string) string {
		var b strings.Builder
		for _, r := range value {
			switch {
			case r >= 'a' && r <= 'z',
				r >= 'A' && r <= 'Z',
				r >= '0' && r <= '9',
				r == '_', r == '-':
				b.WriteRune(r)
			default:
				b.WriteByte('_')
			}
		}
		return b.String()
	}
	name := "mcp__" + sanitize(serverName) + "__" + sanitize(toolName)
	if len(name) <= 64 {
		return name
	}
	sum := sha256.Sum256([]byte(serverName + "\x00" + toolName))
	suffix := "_" + hex.EncodeToString(sum[:6])
	return name[:64-len(suffix)] + suffix
}

func transcriptCoversPriorTurns(
	transcript domain.ProviderTranscript,
	history []domain.Event,
	currentTriggerID string,
	currentResolutionIDs []string,
) bool {
	represented := make(map[string]struct{}, len(transcript.TriggerEventIDs))
	for _, id := range transcript.TriggerEventIDs {
		represented[id] = struct{}{}
	}
	current := make(map[string]struct{}, len(currentResolutionIDs)+1)
	current[currentTriggerID] = struct{}{}
	for _, id := range currentResolutionIDs {
		current[id] = struct{}{}
	}
	for _, event := range history {
		if _, ok := current[event.ID]; ok {
			continue
		}
		if event.TurnEventID != nil {
			// A model-driving event can also be an internally consumed output of a
			// prior turn. Advisor advice uses this shape because the ordinary private
			// tool_result already delivered it to that turn. It therefore does not
			// require a separate provider transcript trigger row.
			continue
		}
		if !drivesPreparedModelTurn(event.Type) {
			continue
		}
		if _, ok := represented[event.ID]; !ok {
			return false
		}
	}
	return true
}

func drivesPreparedModelTurn(eventType string) bool {
	switch eventType {
	case domain.EvUserMessage,
		domain.EvUserDefineOutcome,
		domain.EvUserCustomToolResult,
		domain.EvUserToolResult,
		domain.EvUserToolConfirmation,
		domain.EvAgentThreadMessageReceived:
		return true
	default:
		return false
	}
}

const outcomeGraderSystem = "You are an independent outcome grader for a managed agent harness."

// EvaluateOutcome uses a separate model context from the working agent. It
// returns only a compact verdict that deterministic Workflow code can use to
// decide whether to revise or finish.
func (a *Activities) EvaluateOutcome(
	ctx context.Context,
	in EvaluateOutcomeInput,
) (EvaluateOutcomeResult, error) {
	if a.modelClient == nil {
		return EvaluateOutcomeResult{}, fmt.Errorf("temporal: model client is not configured")
	}
	stopHeartbeat := heartbeatActivity(ctx)
	defer stopHeartbeat()
	prompt, err := outcomeEvaluationPrompt(in.Outcome, in.Candidate, in.Iteration)
	if err != nil {
		return EvaluateOutcomeResult{FatalError: err.Error()}, nil
	}
	graderRequest := model.Request{
		Model: in.Model, Effort: in.Effort, Speed: in.Speed,
		System: outcomeGraderSystem + " Return exactly one JSON object with " +
			`{"result":"satisfied|needs_revision|failed","explanation":"..."}.`,
		MaxTokens: 1024,
		Messages: []domain.Message{{Role: domain.RoleUser, Content: []domain.ContentBlock{{
			Type: "text", Text: prompt,
		}}}},
	}
	preparedContext, err := prepareRequestContext(graderRequest, false, 0)
	if err != nil {
		return EvaluateOutcomeResult{FatalError: err.Error()}, nil
	}
	response, err := a.modelClient.CreateMessage(ctx, preparedContext.Request)
	if err != nil {
		var apiErr *model.APIError
		if errors.As(err, &apiErr) && !apiErr.Retryable() {
			return EvaluateOutcomeResult{FatalError: apiErr.Error()}, nil
		}
		return EvaluateOutcomeResult{}, err
	}
	verdict, explanation, err := parseOutcomeVerdict(response.Content)
	if err != nil {
		return EvaluateOutcomeResult{
			Usage:      response.Usage,
			StopReason: response.StopReason,
			FatalError: "grader returned an invalid verdict: " + err.Error(),
		}, nil
	}
	if in.FinalCycle && verdict == "needs_revision" {
		verdict = "max_iterations_reached"
	}
	startEventID := in.StartEventID
	if startEventID == "" {
		startEventID = a.ids.NewID(domain.PrefixEvent)
	}
	endEventID := in.EndEventID
	if endEventID == "" {
		endEventID = a.ids.NewID(domain.PrefixEvent)
	}
	return EvaluateOutcomeResult{
		StartEventID: startEventID,
		EndEventID:   endEventID,
		Result:       verdict, Explanation: explanation,
		Usage: response.Usage, StopReason: response.StopReason,
	}, nil
}

func outcomeEvaluationPrompt(
	outcome domain.OutcomeSpec,
	candidate []domain.Message,
	iteration int,
) (string, error) {
	rubric, err := json.Marshal(outcome.Rubric)
	if err != nil {
		return "", fmt.Errorf("encode outcome rubric: %w", err)
	}
	transcript, err := json.Marshal(candidate)
	if err != nil {
		return "", fmt.Errorf("encode outcome candidate: %w", err)
	}
	return fmt.Sprintf(
		"Evaluate revision cycle %d.\n\nOutcome:\n%s\n\nRubric:\n%s\n\nAgent transcript and deliverable evidence:\n%s",
		iteration, outcome.Description, rubric, transcript,
	), nil
}

func parseOutcomeVerdict(
	content []domain.ContentBlock,
) (string, string, error) {
	var text strings.Builder
	for _, block := range content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	raw := text.String()
	start := strings.IndexByte(raw, '{')
	end := strings.LastIndexByte(raw, '}')
	if start < 0 || end < start {
		return "", "", fmt.Errorf("response did not contain a JSON object")
	}
	var parsed struct {
		Result      string `json:"result"`
		Explanation string `json:"explanation"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &parsed); err != nil {
		return "", "", err
	}
	switch parsed.Result {
	case "satisfied", "needs_revision", "failed":
	default:
		return "", "", fmt.Errorf("unknown result %q", parsed.Result)
	}
	if strings.TrimSpace(parsed.Explanation) == "" {
		return "", "", fmt.Errorf("explanation is required")
	}
	return parsed.Result, parsed.Explanation, nil
}

func intValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func pendingBarrierContainsTrigger(pending []domain.PendingAction, triggerEventID string) bool {
	for _, action := range pending {
		if action.ResolvingEventID != nil && *action.ResolvingEventID == triggerEventID {
			return true
		}
	}
	return false
}

func (a *Activities) prepareResumeActions(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	resolutionEventIDs []string,
	pending []domain.PendingAction,
) ([]ResumeAction, error) {
	expected := make(map[string]struct{}, len(resolutionEventIDs))
	for _, id := range resolutionEventIDs {
		if id == "" {
			return nil, domain.Validation("resolution event id is required")
		}
		if _, duplicate := expected[id]; duplicate {
			return nil, domain.Validation("duplicate resolution event id")
		}
		expected[id] = struct{}{}
	}
	if _, ok := expected[triggerEventID]; !ok {
		return nil, domain.Validation("resume trigger is not in the pending-action barrier")
	}
	if len(pending) != len(expected) {
		return nil, domain.Validation("resolution events do not match the complete pending-action barrier")
	}

	type orderedAction struct {
		seq    int64
		resume ResumeAction
	}
	ordered := make([]orderedAction, 0, len(pending))
	for _, row := range pending {
		if row.ResolvingEventID == nil {
			return nil, domain.Validation("pending-action barrier is not fully claimed")
		}
		resolutionID := *row.ResolvingEventID
		if _, ok := expected[resolutionID]; !ok {
			return nil, domain.Validation("resolution events do not match the complete pending-action barrier")
		}
		action, err := a.source.GetEvent(ctx, sessionID, row.ActionEventID)
		if err != nil {
			return nil, err
		}
		kind, ok := domain.PendingActionKindForEvent(action.Type, action.Payload)
		var approvalSequence int64
		if row.ApprovalEventID != nil {
			if !ok || kind != domain.PendingToolConfirmation ||
				row.Kind != domain.PendingToolResult || !domain.IsSelfHostedToolCall(action.Type, action.Payload) {
				return nil, domain.Validation("approval does not match an external pending action")
			}
			approval, err := a.source.GetEvent(ctx, sessionID, *row.ApprovalEventID)
			if err != nil {
				return nil, err
			}
			ref, approvalKind, valid := domain.ResolutionReference(approval.Type, approval.Payload)
			if !valid || approvalKind != domain.PendingToolConfirmation || approval.Payload["result"] != "allow" ||
				(ref != action.ID && ref != row.ClientActionEventID) || approval.ThreadID != row.ThreadID ||
				approval.ProcessedAt == nil || approval.Sequence <= action.Sequence {
				return nil, domain.Validation("external tool has no valid committed approval")
			}
			kind = domain.PendingToolResult
			approvalSequence = approval.Sequence
		}
		if !ok || kind != row.Kind {
			return nil, domain.Validation("pending action no longer matches its server event")
		}
		name, _ := action.Payload["name"].(string)
		input, inputOK := action.Payload["input"].(map[string]any)
		if name == "" || !inputOK {
			return nil, domain.Validation("pending action has invalid tool name or input")
		}
		if serverName, _ := action.Payload["mcp_server_name"].(string); serverName != "" {
			name = mcpModelToolName(serverName, name)
		}
		resolution, err := a.source.GetEvent(ctx, sessionID, resolutionID)
		if err != nil {
			return nil, err
		}
		if row.ApprovalEventID != nil && resolution.Sequence <= approvalSequence {
			return nil, domain.Validation("external tool result predates its approval")
		}
		refID, refKind, ok := domain.ResolutionReference(resolution.Type, resolution.Payload)
		expectedClientID := row.ClientActionEventID
		if expectedClientID == "" {
			expectedClientID = action.ID
		}
		if !ok || (refID != expectedClientID && refID != action.ID) ||
			refKind != row.Kind {
			return nil, domain.Validation("client result does not match its pending action")
		}
		if routed, _ := resolution.Payload["session_thread_id"].(string); routed != "" &&
			routed != row.ThreadID {
			return nil, domain.Validation("client result names the wrong session Thread")
		}

		resume := ResumeAction{
			ActionEventID: action.ID,
			// Carry the durable type forward: the result event that answers this
			// park must pair with what the ledger actually holds, whatever naming
			// scheme the resuming Workflow execution would choose for a new call.
			ActionEventType:   action.Type,
			Kind:              row.Kind,
			ToolName:          name,
			Input:             input,
			ResolutionEventID: resolution.ID,
		}
		if row.ApprovalEventID != nil {
			resume.Confirmation = "allow"
		}
		switch row.Kind {
		case domain.PendingCustomToolResult, domain.PendingToolResult:
			if raw, present := resolution.Payload["content"]; present {
				content, ok := raw.([]any)
				if !ok {
					return nil, domain.Validation("custom tool result content must be an array")
				}
				resume.Content = content
			}
			resume.IsError, _ = resolution.Payload["is_error"].(bool)
		case domain.PendingToolConfirmation:
			resume.Confirmation, _ = resolution.Payload["result"].(string)
			if resume.Confirmation != "allow" && resume.Confirmation != "deny" {
				return nil, domain.Validation("tool confirmation must be allow or deny")
			}
			resume.DenyMessage, _ = resolution.Payload["deny_message"].(string)
			if resume.Confirmation == "allow" {
				resume.ToolStepID = a.ids.NewID(domain.PrefixToolStep)
			}
		default:
			return nil, domain.Validation("unknown pending action kind")
		}
		ordered = append(ordered, orderedAction{seq: action.Sequence, resume: resume})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].seq == ordered[j].seq {
			return ordered[i].resume.ActionEventID < ordered[j].resume.ActionEventID
		}
		return ordered[i].seq < ordered[j].seq
	})
	out := make([]ResumeAction, 0, len(ordered))
	for _, item := range ordered {
		out = append(out, item.resume)
	}
	return out, nil
}

func withoutResumeEvents(history []domain.Event, actions []ResumeAction) []domain.Event {
	excluded := make(map[string]struct{}, len(actions)*2)
	for _, action := range actions {
		excluded[action.ActionEventID] = struct{}{}
		excluded[action.ResolutionEventID] = struct{}{}
	}
	filtered := make([]domain.Event, 0, len(history))
	for _, event := range history {
		if _, drop := excluded[event.ID]; !drop {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

// CallModel performs exactly one model call. Its full normalized response is
// returned to Temporal, which durably records the text/tool round structure.
func (a *Activities) CallModel(ctx context.Context, in CallModelInput) (CallModelResult, error) {
	if a.modelClient == nil {
		return CallModelResult{}, fmt.Errorf("temporal: model client is not configured")
	}
	stopHeartbeat := heartbeatActivity(ctx)
	defer stopHeartbeat()
	messageEventID := a.ids.NewID(domain.PrefixEvent)
	modelRequestStartID := in.ModelRequestStartID
	if modelRequestStartID == "" {
		modelRequestStartID = a.ids.NewID(domain.PrefixEvent)
	}
	modelRequestEndID := in.ModelRequestEndID
	if modelRequestEndID == "" {
		modelRequestEndID = a.ids.NewID(domain.PrefixEvent)
	}
	startedPreview := false
	startedThinkingPreview := false
	thinkingEventID := ""
	var previewMu sync.Mutex
	callbacks := model.StreamCallbacks{OnTextDelta: func(index int, text string) {
		if a.previews == nil || text == "" {
			return
		}
		previewMu.Lock()
		defer previewMu.Unlock()
		if !startedPreview {
			_ = a.previews.PublishPreview(ctx, in.SessionID, domain.PreviewFrame{
				ThreadID:            in.ThreadID,
				Kind:                domain.PreviewEventStart,
				EventID:             messageEventID,
				EventType:           domain.EvAgentMessage,
				ModelRequestStartID: modelRequestStartID,
			})
			startedPreview = true
		}
		_ = a.previews.PublishPreview(ctx, in.SessionID, domain.PreviewFrame{
			ThreadID:            in.ThreadID,
			Kind:                domain.PreviewEventDelta,
			EventID:             messageEventID,
			EventType:           domain.EvAgentMessage,
			ModelRequestStartID: modelRequestStartID,
			Index:               index,
			Text:                text,
		})
	}, OnThinkingStart: func() {
		if a.previews == nil {
			return
		}
		previewMu.Lock()
		defer previewMu.Unlock()
		if startedThinkingPreview {
			return
		}
		thinkingEventID = a.ids.NewID(domain.PrefixEvent)
		_ = a.previews.PublishPreview(ctx, in.SessionID, domain.PreviewFrame{
			ThreadID:            in.ThreadID,
			Kind:                domain.PreviewEventStart,
			EventID:             thinkingEventID,
			EventType:           domain.EvAgentThinking,
			ModelRequestStartID: modelRequestStartID,
		})
		startedThinkingPreview = true
	}}
	var response model.Response
	var err error
	if client, ok := a.modelClient.(model.RichStreamingClient); ok {
		response, err = client.CreateMessageStreamWithCallbacks(ctx, in.Request, callbacks)
	} else {
		response, err = a.modelClient.CreateMessageStream(ctx, in.Request, callbacks.OnTextDelta)
	}
	if err != nil {
		var apiErr *model.APIError
		if errors.As(err, &apiErr) && apiErr.Kind == model.ErrorRequestTooLarge {
			return CallModelResult{
				ModelRequestStartID:  modelRequestStartID,
				ModelRequestEndID:    modelRequestEndID,
				ContextOverflow:      true,
				ContextOverflowError: apiErr.Error(),
			}, nil
		}
		if errors.As(err, &apiErr) && apiErr.Retryable() && in.HandleRetryableErrors {
			return CallModelResult{
				ModelRequestStartID: modelRequestStartID,
				ModelRequestEndID:   modelRequestEndID,
				RetryError: &ModelRetryError{
					Type:             modelRetryErrorType(apiErr.Kind),
					Message:          apiErr.Error(),
					RetryAfterMillis: apiErr.RetryAfter.Milliseconds(),
				},
			}, nil
		}
		if errors.As(err, &apiErr) && !apiErr.Retryable() {
			// Permanent provider failures cannot succeed while the immutable turn
			// input and worker configuration remain unchanged. Return them through
			// the existing successful-result terminal channel so the Workflow
			// commits session.error and status_terminated instead of retrying the
			// Activity forever.
			return CallModelResult{
				ModelRequestStartID: modelRequestStartID,
				ModelRequestEndID:   modelRequestEndID,
				FatalError:          apiErr.Error(),
				FatalErrorType:      terminalModelErrorType(apiErr.Kind),
			}, nil
		}
		return CallModelResult{}, err
	}
	result := CallModelResult{
		Response:            response,
		ModelRequestStartID: modelRequestStartID,
		ModelRequestEndID:   modelRequestEndID,
	}
	normalized := append([]domain.ContentBlock(nil), response.Content...)
	hasText := false
	hasThinking := false
	for i := range normalized {
		switch normalized[i].Type {
		case "text":
			if normalized[i].Text != "" {
				hasText = true
			}
		case "tool_use":
			if normalized[i].ToolName == "" {
				result.FatalError = "model returned a tool_use without a name"
				return result, nil
			}
			if normalized[i].Input == nil {
				result.FatalError = "model returned a tool_use without an input object"
				return result, nil
			}
			publicEventID := a.ids.NewID(domain.PrefixEvent)
			result.ToolSteps = append(result.ToolSteps, PlannedToolStep{
				ToolUseEventID:    publicEventID,
				ProviderToolUseID: normalized[i].ToolUseID,
				ToolStepID:        a.ids.NewID(domain.PrefixToolStep),
			})
		case "thinking", "redacted_thinking":
			hasThinking = true
		}
	}
	result.Response.Content = normalized
	if hasText {
		result.MessageEventID = messageEventID
	}
	if hasThinking {
		if thinkingEventID == "" {
			thinkingEventID = a.ids.NewID(domain.PrefixEvent)
		}
		result.ThinkingEventID = thinkingEventID
	}
	return result, nil
}

// AccountModelRequest commits provider usage independently of public turn
// completion. This preserves billed usage when a later tool, grader, interrupt,
// or terminal error prevents the turn itself from completing.
func (a *Activities) AccountModelRequest(
	ctx context.Context,
	in AccountModelRequestInput,
) error {
	source, ok := a.source.(ModelRequestUsageSource)
	if !ok {
		return fmt.Errorf("temporal: model request usage accounting is not configured")
	}
	return source.AccountModelRequest(
		ctx,
		in.SessionID,
		in.ThreadID,
		in.RequestEventID,
		in.Model,
		in.Usage,
		in.StopReason,
	)
}

func (a *Activities) AdmitModelRequest(
	ctx context.Context,
	in AdmitModelRequestInput,
) (AdmitModelRequestResult, error) {
	source, ok := a.source.(ModelRequestAdmissionSource)
	if !ok {
		return AdmitModelRequestResult{}, fmt.Errorf(
			"temporal: model request budget admission is not configured",
		)
	}
	allowed, err := source.AdmitModelRequest(ctx, in.SessionID, in.ThreadID)
	return AdmitModelRequestResult{Allowed: allowed}, err
}

func terminalModelErrorType(kind model.ErrorKind) string {
	if kind == model.ErrorBilling {
		return "billing_error"
	}
	return "model_request_failed_error"
}

func modelRetryErrorType(kind model.ErrorKind) string {
	switch kind {
	case model.ErrorRateLimit:
		return "model_rate_limited_error"
	case model.ErrorOverloaded:
		return "model_overloaded_error"
	default:
		return "model_request_failed_error"
	}
}

// ExecuteTool runs one always-allow built-in behind the durable per-step
// journal. A completed step is returned without re-execution; a started step
// without a result is classified ambiguous and reported as a successful result
// so the Workflow terminates rather than retrying the side effect.
func (a *Activities) ExecuteTool(ctx context.Context, in ExecuteToolInput) (ExecuteToolResult, error) {
	if a.journal == nil {
		return ExecuteToolResult{}, fmt.Errorf("temporal: tool execution requires a journal")
	}
	stopHeartbeat := heartbeatActivity(ctx)
	defer stopHeartbeat()
	if err := a.journal.EnsureAttempt(
		ctx, in.SessionID, in.TriggerEventID, in.AttemptID,
	); err != nil {
		return ExecuteToolResult{}, err
	}
	step, err := a.journal.EnsureToolStep(
		ctx,
		in.AttemptID,
		in.ToolStepID,
		in.Ordinal,
		in.ToolUseEventID,
		in.ToolName,
		in.Input,
	)
	if err != nil {
		return ExecuteToolResult{}, err
	}
	out := ExecuteToolResult{}
	kind := in.ToolKind
	if kind == "" {
		kind = TurnToolBuiltin
	}
	retrySafeStarted := false
	switch step.State {
	case domain.ToolStepCompleted:
		if step.Result == nil {
			return ExecuteToolResult{}, fmt.Errorf("temporal: completed tool step %s has no result", step.ID)
		}
		out.Events = append([]domain.EventDraft(nil), step.Result.Events...)
		out.Result = workflowToolResult(*step.Result)
		return out, nil
	case domain.ToolStepAmbiguous:
		out.Ambiguous = true
		return out, nil
	case domain.ToolStepStarted:
		if kind == TurnToolAdvisor {
			result := advisorErrorResult(
				"Advisor execution was interrupted after the model request began; " +
					"the request will not be repeated to avoid duplicate usage.",
			)
			consultation := in.AdvisorConsultation
			consultation.StopReason = "interrupted"
			consultation.UsageKnown = false
			if err := a.completeAdvisorToolStep(
				ctx,
				in,
				result,
				consultation,
			); err != nil {
				return ExecuteToolResult{}, err
			}
			out.Result = workflowToolResult(result)
			return out, nil
		}
		if in.ToolKind == TurnToolRuntimeSkill {
			// Skill activation is a read-only, deterministic context load. If the
			// worker disappeared after crossing Start but before persisting the
			// result, re-reading the immutable Session pin is safe and required to
			// avoid losing an activated Skill on restart.
			retrySafeStarted = true
			break
		}
		dctx, cancel := durableCtx(ctx)
		err := a.journal.MarkToolStepAmbiguous(dctx, step.ID)
		cancel()
		if err != nil {
			return ExecuteToolResult{}, err
		}
		out.Ambiguous = true
		return out, nil
	case domain.ToolStepPrepared:
		// Continue below.
	default:
		return ExecuteToolResult{}, fmt.Errorf("temporal: invalid tool step state %q", step.State)
	}

	if kind == TurnToolCoordinator {
		source, ok := a.source.(CoordinatorToolSource)
		if !ok {
			return ExecuteToolResult{}, fmt.Errorf(
				"temporal: event source cannot execute coordinator tools",
			)
		}
		result, err := source.ExecuteCoordinatorToolStep(
			ctx, in.SessionID, in.ThreadID, in.TriggerEventID,
			in.ToolStepID, in.ToolName, in.Input,
		)
		if err != nil {
			return ExecuteToolResult{}, err
		}
		return ExecuteToolResult{Result: workflowToolResult(result)}, nil
	}
	if kind == TurnToolAdvisor {
		return a.executeAdvisorTool(ctx, in, step.ID)
	}
	if kind == TurnToolRuntimeSkill {
		session, err := a.source.GetSession(ctx, in.SessionID)
		if err != nil {
			return ExecuteToolResult{}, err
		}
		return a.executeRuntimeSkill(
			workspace.WithScope(ctx, session.WorkspaceID), in, step.ID,
			retrySafeStarted,
		)
	}
	if kind != TurnToolMCP {
		out.FatalError = "tool execution owner is not server-executable: " + string(kind)
		return out, nil
	}
	if a.mcp == nil || in.MCPServer.Name == "" ||
		in.MCPServer.URL == "" || in.MCPToolName == "" {
		out.FatalError = "MCP tool execution is missing its pinned server definition"
		return out, nil
	}
	session, err := a.source.GetSession(ctx, in.SessionID)
	if err != nil {
		return ExecuteToolResult{}, err
	}
	ctx = workspace.WithScope(ctx, session.WorkspaceID)
	if !retrySafeStarted {
		dctx, cancel := durableCtx(ctx)
		err = a.journal.StartToolStep(dctx, step.ID)
		cancel()
		if err != nil {
			return ExecuteToolResult{}, err
		}
	}

	// Crossing StartToolStep is the side-effect uncertainty boundary. A
	// transport failure after this point may have happened after the remote
	// server executed the tool, so the Activity error intentionally becomes
	// ambiguous on retry rather than blindly calling the MCP tool again.
	var called mcpclient.Result
	if authenticated, ok := a.mcp.(mcpclient.AuthenticatedClient); ok {
		called, err = authenticated.CallAuthenticated(
			ctx, in.SessionID, in.MCPServer, in.MCPToolName, in.Input, a.mcpAuth,
		)
	} else {
		called, err = a.mcp.Call(ctx, in.MCPServer, in.MCPToolName, in.Input)
	}
	if err != nil {
		if mcpclient.IsAuthenticationError(err) {
			out.Events = append(out.Events, mcpAuthenticationFailureEvent(in.MCPServer))
			out.Result = domain.ToolStepResult{
				Content: []any{map[string]any{
					"type": "text",
					"text": "Authentication failed for MCP server " + in.MCPServer.Name + ".",
				}},
				IsError: true,
			}
		} else {
			return ExecuteToolResult{}, err
		}
	} else {
		executed, raw, projectErr := tools.ProjectMCPResult(called)
		if projectErr != nil {
			executed = tools.Result{
				Content: []any{map[string]any{
					"type": "text",
					"text": projectErr.Error(),
				}},
				IsError: true,
			}
		}
		out.Result = domain.ToolStepResult{
			Content: executed.Content,
			IsError: executed.IsError,
			Raw:     raw,
		}
	}
	if len(out.Events) > 0 {
		out.Result.Events = append([]domain.EventDraft(nil), out.Events...)
	}
	if err := completeToolResultDurably(ctx, a.journal, step.ID, out.Result); err != nil {
		return ExecuteToolResult{}, err
	}
	out.Result = workflowToolResult(out.Result)
	return out, nil
}

func (a *Activities) executeRuntimeSkill(
	ctx context.Context,
	in ExecuteToolInput,
	stepID string,
	retrySafeStarted bool,
) (ExecuteToolResult, error) {
	out := ExecuteToolResult{}
	if a.skillInstructions == nil {
		out.FatalError = "custom Skill instruction loading is unavailable"
		return out, nil
	}
	skillSource, hasSkills := a.source.(SessionSkillSource)
	threadSkillSource, hasThreadSkills := a.source.(SessionThreadSkillSource)
	if !hasSkills && !hasThreadSkills {
		out.FatalError = "custom Skill runtime metadata is unavailable"
		return out, nil
	}
	if !retrySafeStarted {
		dctx, cancel := durableCtx(ctx)
		err := a.journal.StartToolStep(dctx, stepID)
		cancel()
		if err != nil {
			return ExecuteToolResult{}, err
		}
	}

	name, inputErr := agentruntime.RuntimeSkillName(in.Input)
	switch {
	case inputErr != nil:
		out.Result = domain.ToolStepResult{
			Content: []any{map[string]any{"type": "text", "text": inputErr.Error()}},
			IsError: true,
		}
	case in.SkillAlreadyLoaded:
		out.Result = domain.ToolStepResult{Content: []any{map[string]any{
			"type": "text", "text": "Skill " + name + " is already loaded",
		}}}
	default:
		runtime := domain.SkillRuntime{Root: domain.SessionSkillsRoot}
		var err error
		if hasThreadSkills && in.ThreadID != "" {
			runtime, err = threadSkillSource.SessionThreadSkillRuntime(
				ctx, in.SessionID, in.ThreadID,
			)
		} else if hasSkills {
			runtime.Versions, err = skillSource.SessionSkillsForRuntime(ctx, in.SessionID)
		} else {
			out.FatalError = "custom Skill runtime metadata is unavailable"
			return out, nil
		}
		if err != nil {
			return ExecuteToolResult{}, err
		}
		var valid bool
		runtime, valid = selfHostedSkillRuntime(runtime)
		if !valid {
			out.FatalError = "custom Skill runtime path is outside the Skill root"
			return out, nil
		}
		if in.SkillRuntimeRoot != "" && in.SkillRuntimeRoot != runtime.Root {
			out.FatalError = "custom Skill runtime scope changed after turn preparation"
			return out, nil
		}
		var selected *domain.SkillVersion
		for index := range runtime.Versions {
			if runtime.Versions[index].Name == name {
				selected = &runtime.Versions[index]
				break
			}
		}
		if selected == nil {
			out.Result = domain.ToolStepResult{
				Content: []any{map[string]any{
					"type": "text", "text": "Skill: unknown or unavailable Skill " + name,
				}},
				IsError: true,
			}
		} else {
			runtimePath := runtime.SkillPath(name)
			expectedRoot := domain.SessionSkillsRelativeRoot
			if !strings.HasPrefix(runtimePath, expectedRoot+"/") {
				out.FatalError = "custom Skill runtime path is outside the Skill root"
				return out, nil
			}
			body, err := a.skillInstructions.LoadSkillInstructions(ctx, *selected)
			if err != nil {
				var domainErr *domain.DomainError
				if errors.As(err, &domainErr) && domainErr.Kind == domain.KindValidation {
					out.FatalError = err.Error()
					return out, nil
				}
				return ExecuteToolResult{}, err
			}
			if !utf8.Valid(body) {
				out.Result = domain.ToolStepResult{
					Content: []any{map[string]any{
						"type": "text", "text": "Skill: SKILL.md is not valid UTF-8",
					}},
					IsError: true,
				}
			} else {
				out.Result = domain.ToolStepResult{
					Content: []any{map[string]any{
						"type": "text", "text": "Launching skill: " + name,
					}},
					InjectedContent: []domain.ContentBlock{
						agentruntime.RuntimeSkillInjectionAt(runtime.Root, name, body),
					},
				}
			}
		}
	}
	if err := completeToolResultDurably(ctx, a.journal, stepID, out.Result); err != nil {
		return ExecuteToolResult{}, err
	}
	out.Result = workflowToolResult(out.Result)
	return out, nil
}

func selfHostedSkillRuntime(runtime domain.SkillRuntime) (domain.SkillRuntime, bool) {
	prefix := domain.SessionWorkspaceRoot + "/"
	relative, ok := strings.CutPrefix(runtime.Root, prefix)
	if !ok || (relative != domain.SessionSkillsRelativeRoot &&
		!strings.HasPrefix(relative, domain.SessionSkillsRelativeRoot+"/.agents/")) {
		return domain.SkillRuntime{}, false
	}
	runtime.Root = relative
	return runtime, true
}

func (a *Activities) executeAdvisorTool(
	ctx context.Context,
	in ExecuteToolInput,
	stepID string,
) (ExecuteToolResult, error) {
	if a.modelClient == nil {
		return ExecuteToolResult{}, fmt.Errorf("temporal: advisor requires a model client")
	}
	if strings.TrimSpace(in.AdvisorRequest.Model) == "" ||
		strings.TrimSpace(in.AdvisorConsultation.ThreadID) == "" ||
		len(in.AdvisorConsultation.LifecycleIDs) != 9 {
		return ExecuteToolResult{}, domain.Validation(
			"advisor execution metadata is incomplete",
		)
	}
	dctx, cancel := durableCtx(ctx)
	err := a.journal.StartToolStep(dctx, stepID)
	cancel()
	if err != nil {
		return ExecuteToolResult{}, err
	}
	preparedContext, contextErr := prepareRequestContext(in.AdvisorRequest, false, 0)
	if contextErr != nil {
		consultation := in.AdvisorConsultation
		consultation.Model = in.AdvisorRequest.Model
		consultation.UsageModel = in.AdvisorRequest.Model
		consultation.StopReason = "context_limit"
		consultation.UsageKnown = false
		result := advisorErrorResult("Advisor request was too large: " + contextErr.Error())
		if err := a.completeAdvisorToolStep(ctx, in, result, consultation); err != nil {
			return ExecuteToolResult{}, err
		}
		return ExecuteToolResult{Result: workflowToolResult(result)}, nil
	}
	in.AdvisorRequest = preparedContext.Request

	response, err := a.modelClient.CreateMessage(ctx, in.AdvisorRequest)
	consultation := in.AdvisorConsultation
	consultation.Model = in.AdvisorRequest.Model
	consultation.UsageModel = in.AdvisorRequest.Model
	var result domain.ToolStepResult
	if err != nil && ctx.Err() != nil {
		result = advisorErrorResult(
			"Advisor execution was interrupted after the model request began; " +
				"the request will not be repeated to avoid duplicate usage.",
		)
		consultation.StopReason = "interrupted"
		consultation.UsageKnown = false
	} else if err != nil {
		result = advisorErrorResult("Advisor request failed: " + err.Error())
		consultation.StopReason = "error"
		consultation.UsageKnown = false
	} else {
		consultation.Usage = response.Usage
		consultation.UsageKnown = true
		consultation.StopReason = response.StopReason
		consultation.PublicContent = agentruntime.TextBlocksToContent(response.Content)
		advice := agentruntime.FlattenResultText(consultation.PublicContent)
		if strings.TrimSpace(advice) == "" {
			result = advisorErrorResult("Advisor returned no review text.")
		} else {
			consultation.AdviceDelivered = true
			result = domain.ToolStepResult{Content: consultation.PublicContent}
		}
	}
	if err := a.completeAdvisorToolStep(ctx, in, result, consultation); err != nil {
		return ExecuteToolResult{}, err
	}
	return ExecuteToolResult{Result: workflowToolResult(result)}, nil
}

func (a *Activities) completeAdvisorToolStep(
	ctx context.Context,
	in ExecuteToolInput,
	result domain.ToolStepResult,
	consultation domain.AdvisorConsultation,
) error {
	source, ok := a.source.(AdvisorToolSource)
	if !ok {
		return fmt.Errorf("temporal: advisor persistence is not configured")
	}
	dctx, cancel := durableCtx(ctx)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < toolResultWriteAttempts; attempt++ {
		if err := source.CompleteAdvisorToolStep(
			dctx,
			in.SessionID,
			in.ThreadID,
			in.TriggerEventID,
			in.ToolStepID,
			result,
			consultation,
		); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt+1 == toolResultWriteAttempts {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-timer.C:
		case <-dctx.Done():
			timer.Stop()
			return lastErr
		}
	}
	return lastErr
}

func advisorErrorResult(message string) domain.ToolStepResult {
	return domain.ToolStepResult{
		Content: []any{map[string]any{"type": "text", "text": message}},
		IsError: true,
	}
}

// workflowToolResult is the bounded model/public projection returned through
// Temporal. Executor-native Raw/RawPath stay in the PostgreSQL journal and do
// not need to inflate Workflow history.
func workflowToolResult(result domain.ToolStepResult) domain.ToolStepResult {
	result.Raw = nil
	result.RawPath = ""
	result.Events = nil
	return result
}

func completeToolResultDurably(
	ctx context.Context,
	journal JournalStore,
	stepID string,
	result domain.ToolStepResult,
) error {
	dctx, cancel := durableCtx(ctx)
	defer cancel()

	var lastErr error
	for attempt := 0; attempt < toolResultWriteAttempts; attempt++ {
		if err := journal.CompleteToolStep(dctx, stepID, result); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt+1 == toolResultWriteAttempts {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-timer.C:
		case <-dctx.Done():
			timer.Stop()
			return lastErr
		}
	}
	return lastErr
}

// CompleteWorkflowTurn commits the Workflow's durable output and optional tool
// attempt through one idempotent PostgreSQL transaction.
func (a *Activities) CompleteWorkflowTurn(
	ctx context.Context,
	in CompleteWorkflowTurnInput,
) (RunTurnResult, error) {
	dctx, cancel := durableCtx(ctx)
	defer cancel()
	var completion TurnCompletionResult
	var err error
	usage := in.Usage
	if in.UsageAlreadyAccounted {
		usage = domain.TokenUsage{}
	}
	if in.IsChild {
		source, ok := a.source.(ThreadCompletionSource)
		if !ok {
			return RunTurnResult{}, fmt.Errorf(
				"temporal: event source cannot complete child Thread turns",
			)
		}
		completion, err = source.CompleteThreadWorkflowTurn(
			dctx, in.SessionID, in.ThreadID, in.TriggerEventID,
			in.Output, in.Status, in.AttemptID, in.AttemptState,
			in.AttemptError, in.PendingActionEventIDs,
			in.ResolutionEventIDs, in.TranscriptDelta,
			in.ToolUseMappings, usage,
		)
	} else if source, ok := a.source.(ProviderTranscriptUsageCompletionSource); ok {
		completion, err = source.CompleteWorkflowTurnWithTranscriptAndUsage(
			dctx,
			in.SessionID,
			in.TriggerEventID,
			in.Output,
			in.Status,
			in.AttemptID,
			in.AttemptState,
			in.AttemptError,
			in.PendingActionEventIDs,
			in.ResolutionEventIDs,
			in.TranscriptDelta,
			in.ToolUseMappings,
			usage,
		)
	} else if source, ok := a.source.(ProviderTranscriptCompletionSource); ok {
		completion, err = source.CompleteWorkflowTurnWithTranscript(
			dctx,
			in.SessionID,
			in.TriggerEventID,
			in.Output,
			in.Status,
			in.AttemptID,
			in.AttemptState,
			in.AttemptError,
			in.PendingActionEventIDs,
			in.ResolutionEventIDs,
			in.TranscriptDelta,
			in.ToolUseMappings,
		)
	} else if source, ok := a.source.(UsageCompletionSource); ok {
		completion, err = source.CompleteWorkflowTurnWithUsage(
			dctx,
			in.SessionID,
			in.TriggerEventID,
			in.Output,
			in.Status,
			in.AttemptID,
			in.AttemptState,
			in.AttemptError,
			in.PendingActionEventIDs,
			in.ResolutionEventIDs,
			usage,
		)
	} else {
		completion, err = a.source.CompleteWorkflowTurn(
			dctx,
			in.SessionID,
			in.TriggerEventID,
			in.Output,
			in.Status,
			in.AttemptID,
			in.AttemptState,
			in.AttemptError,
			in.PendingActionEventIDs,
			in.ResolutionEventIDs,
		)
	}
	if err != nil {
		return RunTurnResult{}, err
	}
	switch {
	case completion.Status == domain.StatusTerminated:
		return RunTurnResult{Disposition: TurnTerminated}, nil
	case completion.Parked != nil && *completion.Parked:
		return RunTurnResult{Disposition: TurnParked}, nil
	case completion.Parked == nil && len(in.PendingActionEventIDs) > 0:
		// Replay of a pre-interrupt Activity result. At that time the requested
		// pending set was the disposition source because PG could not override a
		// park with an interrupt.
		return RunTurnResult{Disposition: TurnParked}, nil
	default:
		return RunTurnResult{Disposition: TurnCompleted}, nil
	}
}

const activityHeartbeatInterval = 500 * time.Millisecond

// heartbeatActivity makes long model/tool Activities promptly observe a
// Workflow cancellation request. Temporal delivers remote Activity
// cancellation through heartbeat responses; without this loop, an interrupt
// could remain buffered until a long provider or sandbox call returned.
func heartbeatActivity(ctx context.Context) func() {
	if !activity.IsActivity(ctx) {
		return func() {}
	}
	done := make(chan struct{})
	activity.RecordHeartbeat(ctx)
	go func() {
		ticker := time.NewTicker(activityHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				activity.RecordHeartbeat(ctx)
			case <-done:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { close(done) })
	}
}

// durableWriteTimeout bounds a durable write that must run even after the
// Activity context is canceled (e.g. a tool side effect already happened and its
// fact must be recorded). It is deliberately generous but finite.
const durableWriteTimeout = 30 * time.Second

// durableCtx returns a context detached from the caller's cancellation
// (context.WithoutCancel preserves values like tracing metadata) with a fresh
// bounded timeout. It is created per durable write, never once before a long
// runtime call, so the timeout cannot expire mid-run. This lets an interrupt
// reach a tool executor while still giving the result a bounded opportunity to
// commit.
func durableCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), durableWriteTimeout)
}
