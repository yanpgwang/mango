package temporal

import (
	"context"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/mcpclient"
	"github.com/yanpgwang/mango/internal/pg"
)

func (s storeSource) GetMCPDiscoverySnapshot(
	ctx context.Context,
	sessionID string,
	threadID string,
	server domain.MCPServer,
) ([]mcpclient.Tool, bool, error) {
	return s.store.GetMCPDiscoverySnapshot(ctx, sessionID, threadID, server)
}

func (s storeSource) PutMCPDiscoverySnapshot(
	ctx context.Context,
	sessionID string,
	threadID string,
	server domain.MCPServer,
	tools []mcpclient.Tool,
) ([]mcpclient.Tool, error) {
	return s.store.PutMCPDiscoverySnapshot(ctx, sessionID, threadID, server, tools)
}

// storeSource adapts *pg.Store to the EventSource interface the Activities
// depend on. It exists so the temporal package depends on a narrow interface
// rather than the pg package's concrete completion type, keeping the domain
// boundary clean (no Temporal wire types leak into pg, no pg types into the
// workflow contract).
type storeSource struct{ store *pg.Store }

func (s storeSource) SessionSkillsForRuntime(
	ctx context.Context,
	sessionID string,
) ([]domain.SkillVersion, error) {
	return s.store.SessionSkillsForRuntime(ctx, sessionID)
}

func (s storeSource) SessionThreadSkillRuntime(
	ctx context.Context,
	sessionID string,
	threadID string,
) (domain.SkillRuntime, error) {
	return s.store.SessionThreadSkillRuntime(ctx, sessionID, threadID)
}

func (s storeSource) GetSessionThread(
	ctx context.Context,
	sessionID string,
	threadID string,
) (domain.SessionThread, error) {
	return s.store.GetSessionThread(ctx, sessionID, threadID)
}

// NewStoreSource wraps a PostgreSQL store as an Activity EventSource.
func NewStoreSource(store *pg.Store) EventSource { return storeSource{store: store} }

func (s storeSource) AccountModelRequest(
	ctx context.Context,
	sessionID string,
	threadID string,
	requestEventID string,
	model domain.Model,
	usage domain.TokenUsage,
	stopReason string,
) error {
	return s.store.AccountModelRequest(
		ctx, sessionID, threadID, requestEventID, model, usage, stopReason,
	)
}

func (s storeSource) CompleteAdvisorToolStep(
	ctx context.Context,
	sessionID string,
	primaryThreadID string,
	triggerEventID string,
	stepID string,
	result domain.ToolStepResult,
	consultation domain.AdvisorConsultation,
) error {
	return s.store.CompleteAdvisorToolStep(
		ctx, sessionID, primaryThreadID, triggerEventID, stepID, result, consultation,
	)
}

func (s storeSource) AdmitModelRequest(
	ctx context.Context,
	sessionID string,
	threadID string,
) (bool, error) {
	return s.store.AdmitModelRequest(ctx, sessionID, threadID)
}

func (s storeSource) EventsAfter(ctx context.Context, sessionID string, cursor int64, limit int) ([]domain.Event, error) {
	return s.store.EventsAfter(ctx, sessionID, cursor, limit)
}

func (s storeSource) ThreadEventsAfter(
	ctx context.Context,
	sessionID string,
	threadID string,
	cursor int64,
	limit int,
) ([]domain.Event, error) {
	return s.store.ThreadEventsAfter(ctx, sessionID, threadID, cursor, limit)
}

func (s storeSource) ExecuteCoordinatorToolStep(
	ctx context.Context,
	sessionID string,
	parentThreadID string,
	triggerEventID string,
	stepID string,
	toolName string,
	input map[string]any,
) (domain.ToolStepResult, error) {
	executed, err := s.store.ExecuteCoordinatorToolStep(
		ctx, sessionID, parentThreadID, triggerEventID,
		stepID, toolName, input,
	)
	return executed.Result, err
}

func (s storeSource) RecordThreadWorkflowRetry(
	ctx context.Context,
	sessionID string,
	threadID string,
	triggerEventID string,
	errorEventID string,
	statusEventID string,
	errorPayload map[string]any,
) error {
	return s.store.RecordThreadWorkflowRetry(
		ctx, sessionID, threadID, triggerEventID,
		errorEventID, statusEventID, errorPayload,
	)
}

func (s storeSource) ResumeThreadWorkflowRetry(
	ctx context.Context,
	sessionID string,
	threadID string,
	triggerEventID string,
	statusEventID string,
) error {
	return s.store.ResumeThreadWorkflowRetry(
		ctx, sessionID, threadID, triggerEventID, statusEventID,
	)
}

func (s storeSource) FirstUnprocessedInterruptAfter(
	ctx context.Context,
	sessionID string,
	afterSeq int64,
) (*domain.Event, error) {
	return s.store.FirstUnprocessedInterruptAfter(ctx, sessionID, afterSeq)
}

func (s storeSource) FirstUnprocessedThreadInterruptAfter(
	ctx context.Context,
	sessionID string,
	threadID string,
	afterSeq int64,
) (*domain.Event, error) {
	return s.store.FirstUnprocessedThreadInterruptAfter(
		ctx, sessionID, threadID, afterSeq,
	)
}

func (s storeSource) HistoryThrough(ctx context.Context, sessionID, triggerEventID string, limit int) ([]domain.Event, error) {
	return s.store.HistoryThrough(ctx, sessionID, triggerEventID, limit)
}

func (s storeSource) GetSession(ctx context.Context, id string) (domain.Session, error) {
	return s.store.GetSession(ctx, id)
}

func (s storeSource) GetEvent(ctx context.Context, sessionID, id string) (domain.Event, error) {
	return s.store.GetEvent(ctx, sessionID, id)
}

func (s storeSource) UnresolvedPendingActions(
	ctx context.Context,
	sessionID string,
) ([]domain.PendingAction, error) {
	return s.store.UnresolvedPendingActions(ctx, sessionID)
}

func (s storeSource) UnresolvedThreadPendingActions(
	ctx context.Context,
	sessionID string,
	threadID string,
) ([]domain.PendingAction, error) {
	return s.store.UnresolvedThreadPendingActions(ctx, sessionID, threadID)
}

func (s storeSource) AppendWorkflowEvents(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	drafts []domain.EventDraft,
) error {
	return s.store.AppendWorkflowEvents(ctx, sessionID, triggerEventID, drafts)
}

func (s storeSource) RecordWorkflowRetry(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	errorEventID string,
	statusEventID string,
	errorPayload map[string]any,
) error {
	return s.store.RecordWorkflowRetry(
		ctx,
		sessionID,
		triggerEventID,
		errorEventID,
		statusEventID,
		errorPayload,
	)
}

func (s storeSource) ResumeWorkflowRetry(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	statusEventID string,
) error {
	return s.store.ResumeWorkflowRetry(
		ctx,
		sessionID,
		triggerEventID,
		statusEventID,
	)
}

func (s storeSource) LoadProviderTranscript(
	ctx context.Context,
	sessionID string,
) (domain.ProviderTranscript, error) {
	return s.store.LoadProviderTranscript(ctx, sessionID)
}

func (s storeSource) LoadThreadProviderTranscript(
	ctx context.Context,
	sessionID string,
	threadID string,
) (domain.ProviderTranscript, error) {
	return s.store.LoadThreadProviderTranscript(ctx, sessionID, threadID)
}

func (s storeSource) PutThreadContextSnapshot(
	ctx context.Context,
	sessionID string,
	threadID string,
	triggerEventID string,
	transcriptTriggerEventIDs []string,
	messages []domain.Message,
	projection domain.ContextProjection,
) (domain.ContextSnapshot, error) {
	return s.store.PutThreadContextSnapshot(
		ctx,
		sessionID,
		threadID,
		triggerEventID,
		transcriptTriggerEventIDs,
		messages,
		projection,
	)
}

func (s storeSource) GetThreadContextSnapshotForTrigger(
	ctx context.Context,
	sessionID string,
	threadID string,
	triggerEventID string,
) (domain.ContextSnapshot, bool, error) {
	return s.store.GetThreadContextSnapshotForTrigger(
		ctx,
		sessionID,
		threadID,
		triggerEventID,
	)
}

func (s storeSource) CompleteWorkflowTurn(
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
) (TurnCompletionResult, error) {
	return s.CompleteWorkflowTurnWithUsage(
		ctx,
		sessionID,
		triggerEventID,
		output,
		status,
		attemptID,
		attemptState,
		attemptError,
		pendingActionEventIDs,
		resolutionEventIDs,
		domain.TokenUsage{},
	)
}

func (s storeSource) CompleteWorkflowTurnWithUsage(
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
) (TurnCompletionResult, error) {
	res, err := s.store.CompleteWorkflowTurnWithUsage(
		ctx,
		sessionID,
		triggerEventID,
		output,
		status,
		attemptID,
		attemptState,
		attemptError,
		pendingActionEventIDs,
		resolutionEventIDs,
		usage,
	)
	if err != nil {
		return TurnCompletionResult{}, err
	}
	parked := res.Parked
	return TurnCompletionResult{
		Events:  res.Events,
		Applied: res.Applied,
		Status:  res.Session.Status,
		Parked:  &parked,
	}, nil
}

func (s storeSource) CompleteWorkflowTurnWithTranscript(
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
) (TurnCompletionResult, error) {
	return s.CompleteWorkflowTurnWithTranscriptAndUsage(
		ctx,
		sessionID,
		triggerEventID,
		output,
		status,
		attemptID,
		attemptState,
		attemptError,
		pendingActionEventIDs,
		resolutionEventIDs,
		transcriptDelta,
		toolUseMappings,
		domain.TokenUsage{},
	)
}

func (s storeSource) CompleteWorkflowTurnWithTranscriptAndUsage(
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
) (TurnCompletionResult, error) {
	res, err := s.store.CompleteWorkflowTurnWithTranscriptAndUsage(
		ctx,
		sessionID,
		triggerEventID,
		output,
		status,
		attemptID,
		attemptState,
		attemptError,
		pendingActionEventIDs,
		resolutionEventIDs,
		transcriptDelta,
		toolUseMappings,
		usage,
	)
	if err != nil {
		return TurnCompletionResult{}, err
	}
	parked := res.Parked
	return TurnCompletionResult{
		Events:  res.Events,
		Applied: res.Applied,
		Status:  res.Session.Status,
		Parked:  &parked,
	}, nil
}

func (s storeSource) CompleteThreadWorkflowTurn(
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
) (TurnCompletionResult, error) {
	res, err := s.store.CompleteThreadWorkflowTurn(
		ctx, sessionID, threadID, triggerEventID, output, status,
		attemptID, attemptState, attemptError, pendingActionEventIDs,
		resolutionEventIDs, transcriptDelta, toolUseMappings, usage,
	)
	if err != nil {
		return TurnCompletionResult{}, err
	}
	parked := res.Parked
	completionStatus := res.Session.Status
	if res.ThreadStatus != "" {
		completionStatus = res.ThreadStatus
	}
	return TurnCompletionResult{
		Events: res.Events, Applied: res.Applied,
		Status: completionStatus, Parked: &parked,
	}, nil
}

// storeSource also satisfies JournalStore; the methods below delegate directly.

func (s storeSource) EnsureAttempt(ctx context.Context, sessionID, triggerEventID, attemptID string) error {
	_, err := s.store.EnsureAttempt(ctx, sessionID, triggerEventID, attemptID)
	return err
}

func (s storeSource) EnsureToolStep(
	ctx context.Context,
	attemptID string,
	stepID string,
	ordinal int,
	toolUseEventID string,
	toolName string,
	input map[string]any,
) (domain.ToolStep, error) {
	return s.store.EnsureToolStep(ctx, attemptID, stepID, ordinal, toolUseEventID, toolName, input)
}

func (s storeSource) StartToolStep(ctx context.Context, stepID string) error {
	return s.store.StartToolStep(ctx, stepID)
}

func (s storeSource) CompleteToolStep(ctx context.Context, stepID string, result domain.ToolStepResult) error {
	return s.store.CompleteToolStep(ctx, stepID, result)
}

func (s storeSource) MarkToolStepAmbiguous(ctx context.Context, stepID string) error {
	return s.store.MarkToolStepAmbiguous(ctx, stepID)
}

func (s storeSource) MarkToolOutputPublished(ctx context.Context, stepID, fileID string) error {
	return s.store.MarkToolOutputPublished(ctx, stepID, fileID)
}

func (s storeSource) EnsureToolPermissionAttempt(ctx context.Context, owner domain.ToolPermissionOwner) error {
	_, err := s.store.EnsureToolPermissionAttempt(ctx, owner)
	return err
}
func (s storeSource) PermissionIntentThrough(ctx context.Context, sessionID, triggerID string) (domain.PermissionIntent, error) {
	return s.store.PermissionIntentThrough(ctx, sessionID, triggerID)
}
func (s storeSource) GetToolPermissionReceipt(ctx context.Context, candidate domain.ToolPermissionReceipt) (*domain.ToolPermissionReceipt, error) {
	return s.store.GetToolPermissionReceipt(ctx, candidate)
}
func (s storeSource) AdmitToolPermissionEvaluation(ctx context.Context, candidate domain.ToolPermissionReceipt) (bool, error) {
	return s.store.AdmitToolPermissionEvaluation(ctx, candidate)
}
func (s storeSource) RecordToolPermissionReceipt(ctx context.Context, candidate domain.ToolPermissionReceipt) (domain.ToolPermissionReceipt, error) {
	return s.store.RecordToolPermissionReceipt(ctx, candidate)
}
func (s storeSource) StartToolStepWithPermission(ctx context.Context, stepID, toolUseID string) error {
	return s.store.StartToolStepWithPermission(ctx, stepID, toolUseID)
}
