package temporal

import (
	"encoding/json"
	"fmt"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/yanpgwang/mango/internal/agentruntime"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

const (
	outcomeEvaluationHeartbeatInterval     = 30 * time.Second
	resourceMaterializationTimeoutChangeID = "session-resource-materialization-timeout"
	resourceMaterializationTimeoutVersion  = 1
	resourceMaterializationToolTimeout     = 30 * time.Minute
)

// turnFailure is a typed terminal reason produced by deterministic turn logic.
// It is deliberately distinct from error: errors retry an Activity, while a
// turnFailure is committed as an honest terminal Session outcome.
type turnFailure string

func failTurn(message string) turnFailure {
	return turnFailure(message)
}

// workflowTurnState owns the mutable, deterministic state threaded through one
// Workflow-owned turn. Capturing it here prevents every terminal branch from
// manually passing the same session, trigger, output, attempt, and resolution
// arguments in positional order.
type workflowTurnState struct {
	actx                           workflow.Context
	sessionID                      string
	triggerEventID                 string
	threadID                       string
	isChild                        bool
	skillRuntimeRoot               string
	resolutionEventIDs             []string
	output                         []domain.EventDraft
	attemptID                      string
	ordinal                        int
	interrupts                     *turnInterruptWatcher
	terminalSessionErrors          bool
	outcomeHeartbeats              bool
	outcomeHeartbeatInterval       time.Duration
	activeOutcomeEvaluationStartID string
	activeOutcomeIteration         int
	usesProviderTranscript         bool
	transcriptDelta                []domain.Message
	toolUseMappings                []domain.ProviderToolUseMapping
	loadedSkills                   map[string]struct{}
	usage                          domain.TokenUsage
	perRequestUsageAccounting      bool
	sessionOutputsEnabled          bool
	flushedEventCount              int
}

func (t *workflowTurnState) startModelRequest(startID string) error {
	return workflow.ExecuteActivity(
		t.actx,
		ActivityStartModelRequest,
		StartModelRequestInput{
			SessionID:           t.sessionID,
			TriggerEventID:      t.triggerEventID,
			ModelRequestStartID: startID,
		},
	).Get(t.actx, nil)
}

func (t *workflowTurnState) awaitModelRequestAdmission() error {
	for {
		var admitted AdmitModelRequestResult
		if err := workflow.ExecuteActivity(
			t.actx,
			ActivityAdmitModelRequest,
			AdmitModelRequestInput{
				SessionID: t.sessionID,
				ThreadID:  t.threadID,
			},
		).Get(t.actx, &admitted); err != nil {
			return err
		}
		if admitted.Allowed {
			return nil
		}
		if t.interrupts == nil {
			return fmt.Errorf("budget-paused turn has no workflow wakeup channel")
		}
		t.interrupts.waitForWakeup()
	}
}

func (t *workflowTurnState) flushOutput() error {
	if len(t.output) == 0 {
		return nil
	}
	drafts := append([]domain.EventDraft(nil), t.output...)
	for i := range drafts {
		if drafts[i].ID == "" {
			drafts[i].ID = workflowProgressEventID(
				t.sessionID,
				t.triggerEventID,
				t.flushedEventCount+i,
			)
		}
	}
	if err := workflow.ExecuteActivity(
		t.actx,
		ActivityAppendWorkflowEvents,
		AppendWorkflowEventsInput{
			SessionID:      t.sessionID,
			TriggerEventID: t.triggerEventID,
			Events:         drafts,
		},
	).Get(t.actx, nil); err != nil {
		return err
	}
	t.flushedEventCount += len(drafts)
	t.output = nil
	return nil
}

func (t *workflowTurnState) callModel(
	input CallModelInput,
) (CallModelResult, interruptibleActivityOutcome, error) {
	var called CallModelResult
	if t.interrupts == nil {
		err := workflow.ExecuteActivity(
			t.actx,
			ActivityCallModel,
			input,
		).Get(t.actx, &called)
		if err == nil {
			t.usage.Add(called.Response.Usage)
		}
		return called, interruptibleActivityOutcome{Completed: err == nil}, err
	}
	outcome, err := t.interrupts.executeActivity(
		ActivityCallModel,
		input,
		&called,
	)
	if err == nil && outcome.Completed {
		t.usage.Add(called.Response.Usage)
	}
	return called, outcome, err
}

func (t *workflowTurnState) recordModelRetry(
	retry ModelRetryError,
	errorEventID string,
	statusEventID string,
) error {
	return workflow.ExecuteActivity(
		t.actx,
		ActivityRecordModelRetry,
		RecordModelRetryInput{
			SessionID:      t.sessionID,
			ThreadID:       t.threadID,
			IsChild:        t.isChild,
			TriggerEventID: t.triggerEventID,
			ErrorEventID:   errorEventID,
			StatusEventID:  statusEventID,
			Error:          retry,
		},
	).Get(t.actx, nil)
}

func (t *workflowTurnState) resumeModelRetry(statusEventID string) error {
	return workflow.ExecuteActivity(
		t.actx,
		ActivityResumeModelRetry,
		ResumeModelRetryInput{
			SessionID:      t.sessionID,
			ThreadID:       t.threadID,
			IsChild:        t.isChild,
			TriggerEventID: t.triggerEventID,
			StatusEventID:  statusEventID,
		},
	).Get(t.actx, nil)
}

func (t *workflowTurnState) waitModelRetry(delay time.Duration) (bool, error) {
	if t.interrupts != nil {
		return t.interrupts.wait(delay)
	}
	if err := workflow.NewTimer(t.actx, delay).Get(t.actx, nil); err != nil {
		return false, err
	}
	return false, nil
}

func (t *workflowTurnState) evaluateOutcome(
	input EvaluateOutcomeInput,
) (EvaluateOutcomeResult, interruptibleActivityOutcome, error) {
	var evaluated EvaluateOutcomeResult
	if t.outcomeHeartbeats {
		heartbeatInterval := t.outcomeHeartbeatInterval
		if heartbeatInterval <= 0 {
			heartbeatInterval = outcomeEvaluationHeartbeatInterval
		}
		heartbeatOrdinal := 0
		emitHeartbeat := func() error {
			err := t.appendOutcomeEvaluationHeartbeat(input, heartbeatOrdinal)
			heartbeatOrdinal++
			return err
		}
		if t.interrupts == nil {
			err := t.evaluateOutcomeWithHeartbeats(
				input,
				&evaluated,
				heartbeatInterval,
				emitHeartbeat,
			)
			if err == nil {
				evaluated.StartEventID = input.StartEventID
				evaluated.EndEventID = input.EndEventID
				t.usage.Add(evaluated.Usage)
				if t.perRequestUsageAccounting && hasTokenUsage(evaluated.Usage) {
					err = t.accountOutcomeEvaluation(input, evaluated)
				}
			}
			return evaluated, interruptibleActivityOutcome{Completed: err == nil}, err
		}
		outcome, err := t.interrupts.executeActivityWithPulse(
			ActivityEvaluateOutcome,
			input,
			&evaluated,
			heartbeatInterval,
			emitHeartbeat,
		)
		if err == nil && outcome.Completed {
			evaluated.StartEventID = input.StartEventID
			evaluated.EndEventID = input.EndEventID
			t.usage.Add(evaluated.Usage)
			if t.perRequestUsageAccounting && hasTokenUsage(evaluated.Usage) {
				err = t.accountOutcomeEvaluation(input, evaluated)
			}
		}
		return evaluated, outcome, err
	}
	if t.interrupts == nil {
		err := workflow.ExecuteActivity(
			t.actx,
			ActivityEvaluateOutcome,
			input,
		).Get(t.actx, &evaluated)
		if err == nil {
			t.usage.Add(evaluated.Usage)
			if t.perRequestUsageAccounting && hasTokenUsage(evaluated.Usage) {
				err = t.accountOutcomeEvaluation(input, evaluated)
			}
		}
		return evaluated, interruptibleActivityOutcome{Completed: err == nil}, err
	}
	outcome, err := t.interrupts.executeActivity(
		ActivityEvaluateOutcome,
		input,
		&evaluated,
	)
	if err == nil && outcome.Completed {
		t.usage.Add(evaluated.Usage)
		if t.perRequestUsageAccounting && hasTokenUsage(evaluated.Usage) {
			err = t.accountOutcomeEvaluation(input, evaluated)
		}
	}
	return evaluated, outcome, err
}

func (t *workflowTurnState) accountOutcomeEvaluation(
	input EvaluateOutcomeInput,
	evaluated EvaluateOutcomeResult,
) error {
	return t.accountModelRequest(
		input.EndEventID,
		model.Request{
			Model: input.Model, Effort: input.Effort, Speed: input.Speed,
		},
		evaluated.Usage,
		evaluated.StopReason,
	)
}

func (t *workflowTurnState) accountModelRequest(
	requestEventID string,
	request model.Request,
	usage domain.TokenUsage,
	stopReason string,
) error {
	return workflow.ExecuteActivity(
		t.actx,
		ActivityAccountModelRequest,
		AccountModelRequestInput{
			SessionID: t.sessionID, ThreadID: t.threadID,
			RequestEventID: requestEventID,
			Model: domain.Model{
				ID: request.Model, Effort: request.Effort, Speed: request.Speed,
			},
			Usage: usage, StopReason: stopReason,
		},
	).Get(t.actx, nil)
}

func hasTokenUsage(usage domain.TokenUsage) bool {
	return usage.InputTokens != 0 || usage.OutputTokens != 0 ||
		usage.CacheReadInputTokens != 0 ||
		usage.CacheCreation.Ephemeral1hInputTokens != 0 ||
		usage.CacheCreation.Ephemeral5mInputTokens != 0 ||
		usage.ServerToolUse.WebFetchRequests != 0 ||
		usage.ServerToolUse.WebSearchRequests != 0
}

func (t *workflowTurnState) evaluateOutcomeWithHeartbeats(
	input EvaluateOutcomeInput,
	evaluated *EvaluateOutcomeResult,
	heartbeatInterval time.Duration,
	emitHeartbeat func() error,
) error {
	future := workflow.ExecuteActivity(t.actx, ActivityEvaluateOutcome, input)
	for {
		if future.IsReady() {
			return future.Get(t.actx, evaluated)
		}
		timerCtx, cancelTimer := workflow.WithCancel(t.actx)
		timer := workflow.NewTimer(timerCtx, heartbeatInterval)
		selector := workflow.NewSelector(t.actx)
		timerReady := false
		selector.AddFuture(future, func(workflow.Future) {})
		selector.AddFuture(timer, func(workflow.Future) { timerReady = true })
		selector.Select(t.actx)
		if future.IsReady() {
			cancelTimer()
			return future.Get(t.actx, evaluated)
		}
		if timerReady {
			if err := emitHeartbeat(); err != nil {
				return err
			}
		}
	}
}

func (t *workflowTurnState) appendOutcomeEvaluationHeartbeat(
	input EvaluateOutcomeInput,
	ordinal int,
) error {
	return workflow.ExecuteActivity(
		t.actx,
		ActivityAppendWorkflowEvents,
		AppendWorkflowEventsInput{
			SessionID:      t.sessionID,
			TriggerEventID: t.triggerEventID,
			Events: []domain.EventDraft{{
				ID:   outcomeEvaluationHeartbeatID(input.StartEventID, ordinal),
				Type: domain.EvSpanOutcomeEvaluationOngoing,
				Payload: map[string]any{
					"outcome_id": input.Outcome.OutcomeID,
					"iteration":  input.Iteration,
				},
			}},
		},
	).Get(t.actx, nil)
}

func outcomeEvaluationDrafts(
	outcome domain.OutcomeSpec,
	iteration int,
	evaluated EvaluateOutcomeResult,
) []domain.EventDraft {
	return []domain.EventDraft{
		outcomeEvaluationStartDraft(outcome, iteration, evaluated.StartEventID),
		outcomeEvaluationEndDraft(outcome, iteration, evaluated),
	}
}

func outcomeEvaluationStartDraft(
	outcome domain.OutcomeSpec,
	iteration int,
	startEventID string,
) domain.EventDraft {
	return domain.EventDraft{
		ID: startEventID, Type: domain.EvSpanOutcomeEvaluationStart,
		Payload: map[string]any{
			"outcome_id": outcome.OutcomeID, "iteration": iteration,
		},
	}
}

func outcomeEvaluationEndDraft(
	outcome domain.OutcomeSpec,
	iteration int,
	evaluated EvaluateOutcomeResult,
) domain.EventDraft {
	cacheCreation := evaluated.Usage.CacheCreation.Ephemeral1hInputTokens +
		evaluated.Usage.CacheCreation.Ephemeral5mInputTokens
	usage := map[string]any{
		"cache_creation_input_tokens": cacheCreation,
		"cache_read_input_tokens":     evaluated.Usage.CacheReadInputTokens,
		"input_tokens":                evaluated.Usage.InputTokens,
		"output_tokens":               evaluated.Usage.OutputTokens,
		"speed":                       nullableSpeed(evaluated.Usage.Speed),
	}
	return domain.EventDraft{
		ID: evaluated.EndEventID, Type: domain.EvSpanOutcomeEvaluationEnd,
		Payload: map[string]any{
			"outcome_evaluation_start_id": evaluated.StartEventID,
			"outcome_id":                  outcome.OutcomeID, "iteration": iteration,
			"result": evaluated.Result, "explanation": evaluated.Explanation,
			"usage": usage,
		},
	}
}

func outcomeEvaluationFailureDraft(
	outcome domain.OutcomeSpec,
	iteration int,
	startEventID string,
	endEventID string,
	evaluated EvaluateOutcomeResult,
) domain.EventDraft {
	evaluated.StartEventID = startEventID
	evaluated.EndEventID = endEventID
	evaluated.Result = "failed"
	evaluated.Explanation = evaluated.FatalError
	return outcomeEvaluationEndDraft(outcome, iteration, evaluated)
}

// modelRequestStartDraft is retained only for histories recorded before the
// live-model-request-span-start Workflow patch. New histories publish the same
// event through StartModelRequest before CallModel begins.
func modelRequestStartDraft(called CallModelResult) *domain.EventDraft {
	if called.ModelRequestStartID == "" {
		return nil
	}
	return &domain.EventDraft{
		ID: called.ModelRequestStartID, Type: domain.EvSpanModelRequestStart,
		Payload: map[string]any{},
	}
}

func modelRequestEndDraft(
	called CallModelResult,
	isError bool,
) *domain.EventDraft {
	if called.ModelRequestEndID == "" || called.ModelRequestStartID == "" {
		return nil
	}
	usage := called.Response.Usage
	return &domain.EventDraft{
		ID: called.ModelRequestEndID, Type: domain.EvSpanModelRequestEnd,
		Payload: map[string]any{
			"model_request_start_id": called.ModelRequestStartID,
			"is_error":               isError,
			"model_usage": map[string]any{
				"cache_creation_input_tokens": usage.CacheCreation.Ephemeral1hInputTokens +
					usage.CacheCreation.Ephemeral5mInputTokens,
				"cache_read_input_tokens": usage.CacheReadInputTokens,
				"input_tokens":            usage.InputTokens,
				"output_tokens":           usage.OutputTokens,
				"speed":                   nullableSpeed(usage.Speed),
			},
		},
	}
}

func nullableSpeed(speed string) any {
	if speed == "" {
		return nil
	}
	return speed
}

func (t *workflowTurnState) executeTool(
	attemptID string,
	use domain.ContentBlock,
	publicEventID string,
	stepID string,
	definition TurnTool,
	advisorRequest model.Request,
	advisorConsultation domain.AdvisorConsultation,
) (ExecuteToolResult, interruptibleActivityOutcome, error) {
	t.attemptID = attemptID
	input := ExecuteToolInput{
		SessionID:           t.sessionID,
		ThreadID:            t.threadID,
		TriggerEventID:      t.triggerEventID,
		AttemptID:           attemptID,
		Ordinal:             t.ordinal,
		ToolUseEventID:      publicEventID,
		ToolStepID:          stepID,
		ToolName:            use.ToolName,
		ToolKind:            definition.Kind,
		MCPServer:           definition.MCPServer,
		MCPToolName:         definition.MCPToolName,
		Input:               use.Input,
		SkillRuntimeRoot:    t.skillRuntimeRoot,
		AdvisorRequest:      advisorRequest,
		AdvisorConsultation: advisorConsultation,
	}
	if definition.Kind == TurnToolRuntimeSkill {
		if name, err := agentruntime.RuntimeSkillName(use.Input); err == nil {
			_, input.SkillAlreadyLoaded = t.loadedSkills[name]
		}
	}
	toolActx := t.actx
	if workflow.GetVersion(
		t.actx,
		resourceMaterializationTimeoutChangeID,
		workflow.DefaultVersion,
		resourceMaterializationTimeoutVersion,
	) == resourceMaterializationTimeoutVersion {
		options := workflow.GetActivityOptions(t.actx)
		options.StartToCloseTimeout = resourceMaterializationToolTimeout
		toolActx = workflow.WithActivityOptions(t.actx, options)
	}
	var executed ExecuteToolResult
	if t.interrupts == nil {
		err := workflow.ExecuteActivity(
			toolActx,
			ActivityExecuteTool,
			input,
		).Get(toolActx, &executed)
		if err != nil {
			return ExecuteToolResult{}, interruptibleActivityOutcome{}, err
		}
		t.rememberLoadedSkill(definition, use, executed)
		t.ordinal++
		return executed, interruptibleActivityOutcome{Completed: true}, nil
	}
	outcome, err := t.interrupts.executeActivityOn(
		toolActx,
		ActivityExecuteTool,
		input,
		&executed,
	)
	if err != nil {
		return ExecuteToolResult{}, interruptibleActivityOutcome{}, err
	}
	if outcome.Completed {
		t.rememberLoadedSkill(definition, use, executed)
		t.ordinal++
	}
	return executed, outcome, nil
}

func (t *workflowTurnState) rememberLoadedSkill(
	definition TurnTool,
	use domain.ContentBlock,
	executed ExecuteToolResult,
) {
	if definition.Kind != TurnToolRuntimeSkill || executed.Result.IsError {
		return
	}
	name, err := agentruntime.RuntimeSkillName(use.Input)
	if err != nil {
		return
	}
	if t.loadedSkills == nil {
		t.loadedSkills = make(map[string]struct{})
	}
	t.loadedSkills[name] = struct{}{}
}

func (t *workflowTurnState) complete(
	pendingActionEventIDs []string,
) (RunTurnResult, error) {
	return t.completeTurn(pendingActionEventIDs, true)
}

// completeInterrupted commits the interrupt promptly. Session output
// publication is an idle-transition side effect for completed work; it must not
// make an explicit interrupt wait behind a potentially long sandbox snapshot.
func (t *workflowTurnState) completeInterrupted() (RunTurnResult, error) {
	return t.completeTurn(nil, false)
}

func (t *workflowTurnState) completeTurn(
	pendingActionEventIDs []string,
	publishSessionOutputs bool,
) (RunTurnResult, error) {
	if publishSessionOutputs {
		if fatal, err := t.publishSessionOutputs(); err != nil {
			return RunTurnResult{}, err
		} else if fatal != "" {
			t.output = append(t.output, sessionOutputErrorDraft(fatal))
		}
	}
	stopReason := map[string]any{"type": "end_turn"}
	if len(pendingActionEventIDs) > 0 {
		stopReason = map[string]any{
			"type":      "requires_action",
			"event_ids": pendingActionEventIDs,
		}
	}
	statusType := domain.EvSessionStatusIdle
	statusPayload := map[string]any{"stop_reason": stopReason}
	if t.isChild {
		statusType = domain.EvSessionThreadStatusIdle
		statusPayload["session_thread_id"] = t.threadID
	}
	output := append(t.output, domain.EventDraft{
		Type: statusType, Payload: statusPayload,
	})
	if t.activeOutcomeEvaluationStartID != "" {
		output[len(output)-1].Payload[domain.InternalOutcomeEvaluationStart] =
			t.activeOutcomeEvaluationStartID
		output[len(output)-1].Payload[domain.InternalOutcomeIteration] =
			t.activeOutcomeIteration
	}
	input := CompleteWorkflowTurnInput{
		SessionID:             t.sessionID,
		ThreadID:              t.threadID,
		IsChild:               t.isChild,
		TriggerEventID:        t.triggerEventID,
		Output:                output,
		Status:                domain.StatusIdle,
		AttemptID:             t.attemptID,
		PendingActionEventIDs: pendingActionEventIDs,
		ResolutionEventIDs:    t.resolutionEventIDs,
		Usage:                 t.usage,
		UsageAlreadyAccounted: t.perRequestUsageAccounting,
	}
	if t.usesProviderTranscript {
		input.TranscriptDelta = t.transcriptDelta
		input.ToolUseMappings = t.toolUseMappings
	}
	if t.attemptID != "" {
		input.AttemptState = domain.RunAttemptCompleted
	}
	var result RunTurnResult
	err := workflow.ExecuteActivity(
		t.actx,
		ActivityCompleteWorkflowTurn,
		input,
	).Get(t.actx, &result)
	return result, err
}

func (t *workflowTurnState) exhaustModelRetries(
	retry ModelRetryError,
) (RunTurnResult, error) {
	if fatal, err := t.publishSessionOutputs(); err != nil {
		return RunTurnResult{}, err
	} else if fatal != "" {
		t.output = append(t.output, sessionOutputErrorDraft(fatal))
	}
	output := append(t.output,
		domain.EventDraft{Type: domain.EvSessionError, Payload: map[string]any{
			"error": map[string]any{
				"type":    retry.Type,
				"message": retry.Message,
				"retry_status": map[string]any{
					"type": "exhausted",
				},
			},
		}},
		domain.EventDraft{Type: domain.EvSessionStatusIdle, Payload: map[string]any{
			"stop_reason": map[string]any{"type": "retries_exhausted"},
		}},
	)
	input := CompleteWorkflowTurnInput{
		SessionID:             t.sessionID,
		ThreadID:              t.threadID,
		IsChild:               t.isChild,
		TriggerEventID:        t.triggerEventID,
		Output:                output,
		Status:                domain.StatusIdle,
		AttemptID:             t.attemptID,
		ResolutionEventIDs:    t.resolutionEventIDs,
		Usage:                 t.usage,
		UsageAlreadyAccounted: t.perRequestUsageAccounting,
	}
	if t.usesProviderTranscript {
		input.TranscriptDelta = t.transcriptDelta
		input.ToolUseMappings = t.toolUseMappings
	}
	if t.attemptID != "" {
		message := retry.Message
		input.AttemptState = domain.RunAttemptFailed
		input.AttemptError = &message
	}
	var result RunTurnResult
	err := workflow.ExecuteActivity(
		t.actx,
		ActivityCompleteWorkflowTurn,
		input,
	).Get(t.actx, &result)
	return result, err
}

func (t *workflowTurnState) publishSessionOutputs() (string, error) {
	if !t.sessionOutputsEnabled || t.isChild {
		return "", nil
	}
	options := workflow.GetActivityOptions(t.actx)
	options.StartToCloseTimeout = resourceMaterializationToolTimeout
	outputActx := workflow.WithActivityOptions(t.actx, options)
	var published PublishSessionOutputsResult
	err := workflow.ExecuteActivity(
		outputActx,
		ActivityPublishSessionOutputs,
		PublishSessionOutputsInput{SessionID: t.sessionID},
	).Get(outputActx, &published)
	return published.FatalError, err
}

func sessionOutputErrorDraft(message string) domain.EventDraft {
	return domain.EventDraft{Type: domain.EvSessionError, Payload: map[string]any{
		"error": map[string]any{
			"type":    "unknown_error",
			"message": "Session output publication failed: " + message,
			"retry_status": map[string]any{
				"type": "exhausted",
			},
		},
	}}
}

func (t *workflowTurnState) terminate(
	failure turnFailure,
) (RunTurnResult, error) {
	return t.terminateTyped("unknown_error", string(failure))
}

func (t *workflowTurnState) terminateTyped(
	errorType string,
	message string,
) (RunTurnResult, error) {
	errorPayload := map[string]any{"type": "api_error", "message": message}
	if t.terminalSessionErrors {
		if errorType == "" {
			errorType = "unknown_error"
		}
		errorPayload = map[string]any{
			"type":    errorType,
			"message": message,
			"retry_status": map[string]any{
				"type": "terminal",
			},
		}
	}
	output := append(t.output,
		domain.EventDraft{Type: domain.EvSessionError, Payload: map[string]any{
			"error": errorPayload,
		}},
		domain.EventDraft{Type: domain.EvSessionStatusTerminated, Payload: map[string]any{}},
	)
	input := CompleteWorkflowTurnInput{
		SessionID:             t.sessionID,
		ThreadID:              t.threadID,
		IsChild:               t.isChild,
		TriggerEventID:        t.triggerEventID,
		Output:                output,
		Status:                domain.StatusTerminated,
		AttemptID:             t.attemptID,
		ResolutionEventIDs:    t.resolutionEventIDs,
		Usage:                 t.usage,
		UsageAlreadyAccounted: t.perRequestUsageAccounting,
	}
	if t.usesProviderTranscript {
		input.TranscriptDelta = t.transcriptDelta
		input.ToolUseMappings = t.toolUseMappings
	}
	if t.attemptID != "" {
		input.AttemptState = domain.RunAttemptFailed
		input.AttemptError = &message
	}
	var result RunTurnResult
	err := workflow.ExecuteActivity(
		t.actx,
		ActivityCompleteWorkflowTurn,
		input,
	).Get(t.actx, &result)
	return result, err
}

func indexTurnTools(tools []TurnTool) map[string]TurnTool {
	toolsByName := make(map[string]TurnTool, len(tools))
	for _, tool := range tools {
		// Built-ins are listed before custom tools. Preserve the first owner when
		// names collide, matching the public built-in-first dispatch contract.
		if _, exists := toolsByName[tool.Name]; !exists {
			toolsByName[tool.Name] = tool
		}
	}
	return toolsByName
}

// resumeWorkflowTurn reconstructs a fully claimed pending-action barrier as one
// logical tool-result round. It remains Workflow-scoped because an allowed
// confirmation executes the original built-in through an Activity.
func resumeWorkflowTurn(
	turn *workflowTurnState,
	prepared PrepareTurnResult,
	toolsByName map[string]TurnTool,
	messages []domain.Message,
) ([]domain.Message, bool, turnFailure, error) {
	if len(prepared.ResumeActions) == 0 {
		return messages, false, "", nil
	}

	actionBlocks := make([]domain.ContentBlock, 0, len(prepared.ResumeActions))
	resultBlocks := make([]domain.ContentBlock, 0, len(prepared.ResumeActions))
	for _, action := range prepared.ResumeActions {
		definition, ok := toolsByName[action.ToolName]
		if !ok {
			return nil, false, failTurn(
				"pending action names a tool that is not enabled: " + action.ToolName,
			), nil
		}
		providerToolUseID := action.ProviderToolUseID
		if providerToolUseID == "" {
			providerToolUseID = action.ActionEventID
		}
		if !prepared.UsesProviderTranscript {
			actionBlocks = append(actionBlocks, domain.ContentBlock{
				Type:      "tool_use",
				ToolUseID: providerToolUseID,
				ToolName:  action.ToolName,
				Input:     action.Input,
			})
		}

		content := action.Content
		isError := action.IsError
		switch action.Kind {
		case domain.PendingCustomToolResult:
			if definition.Kind != TurnToolCustom {
				return nil, false, failTurn(
					"custom tool result does not reference a custom tool",
				), nil
			}
		case domain.PendingToolResult:
			if definition.Kind != TurnToolSelfHosted {
				return nil, false, failTurn(
					"tool result does not reference a self-hosted built-in tool",
				), nil
			}
		case domain.PendingToolConfirmation:
			if (definition.Kind != TurnToolBuiltin &&
				definition.Kind != TurnToolMCP) ||
				definition.Permission.Type != "always_ask" {
				return nil, false, failTurn(
					"tool confirmation does not reference an always_ask built-in",
				), nil
			}
			if action.Confirmation == "deny" {
				text := "Tool call denied by user."
				if action.DenyMessage != "" {
					text += " " + action.DenyMessage
				}
				content = []any{map[string]any{"type": "text", "text": text}}
				isError = true
			} else {
				if action.ToolStepID == "" || prepared.AttemptID == "" {
					return nil, false, failTurn(
						"allowed confirmation has no durable operation id",
					), nil
				}
				executed, activityOutcome, err := turn.executeTool(
					prepared.AttemptID,
					domain.ContentBlock{
						Type:      "tool_use",
						ToolUseID: providerToolUseID,
						ToolName:  action.ToolName,
						Input:     action.Input,
					},
					action.ActionEventID,
					action.ToolStepID,
					definition,
					model.Request{},
					domain.AdvisorConsultation{},
				)
				if err != nil {
					return nil, false, "", err
				}
				if activityOutcome.Interrupted && !activityOutcome.Completed {
					return nil, true, "", nil
				}
				if executed.FatalError != "" {
					if activityOutcome.Interrupted {
						return nil, true, "", nil
					}
					return nil, false, failTurn(executed.FatalError), nil
				}
				if executed.Ambiguous {
					if activityOutcome.Interrupted {
						return nil, true, "", nil
					}
					return nil, false, failTurn(
						"a confirmed tool began executing but no trustworthy result was recorded; " +
							"the side effect will not be retried",
					), nil
				}
				turn.output = append(turn.output, executed.Events...)
				content = executed.Result.Content
				isError = executed.Result.IsError
				if activityOutcome.Interrupted {
					turn.output = append(turn.output, toolResultDraft(
						action.ActionEventType,
						action.ActionEventID,
						content,
						isError,
					))
					return nil, true, "", nil
				}
			}
			turn.output = append(turn.output, toolResultDraft(
				action.ActionEventType,
				action.ActionEventID,
				content,
				isError,
			))
		default:
			return nil, false, failTurn("unknown pending action kind"), nil
		}
		resultBlocks = append(resultBlocks, domain.ContentBlock{
			Type:          "tool_result",
			ToolResultFor: providerToolUseID,
			Text:          agentruntime.FlattenResultText(content),
			IsError:       isError,
			ResultContent: rawResultContent(content),
		})
	}

	var added []domain.Message
	if len(actionBlocks) > 0 {
		added = append(added, domain.Message{
			Role: domain.RoleAssistant, Content: actionBlocks,
		})
	}
	added = append(added, domain.Message{
		Role: domain.RoleUser, Content: resultBlocks,
	})
	if prepared.UsesProviderTranscript {
		turn.transcriptDelta = agentruntime.AppendMerging(
			turn.transcriptDelta,
			[]domain.Message{{Role: domain.RoleUser, Content: resultBlocks}},
		)
	}
	return agentruntime.AppendMerging(messages, added), false, "", nil
}

type plannedToolUse struct {
	use           domain.ContentBlock
	publicEventID string
	// useEventType is the type of the tool-use draft this plan committed for the
	// call. The result event is derived from it rather than recomputed, so the
	// pair can never disagree.
	useEventType string
	stepID       string
	definition   TurnTool
}

type toolBatchPlan struct {
	actionDrafts          []domain.EventDraft
	executable            []plannedToolUse
	pendingActionEventIDs []string
}

// serverToolUseType picks the documented tool-use variant for a server-executed
// call. An MCP call is its own event type carrying a required mcp_server_name;
// every other server-executed tool stays on agent.tool_use. mcpToolEvents is the
// Workflow version gate: a history recorded before the change keeps the legacy
// spelling so replay stays deterministic. It is the only decision the gate
// makes, because it is the only one that is not already fixed by durable state.
func serverToolUseType(definition TurnTool, mcpToolEvents bool) string {
	if mcpToolEvents && definition.Kind == TurnToolMCP {
		return domain.EvAgentMcpToolUse
	}
	return domain.EvAgentToolUse
}

// toolResultDraft builds the result event that answers one server-executed tool
// call, deriving the variant from the tool-use event that was actually
// committed. agent.mcp_tool_result correlates through mcp_tool_use_id and
// carries no mcp_server_name: upstream documents attribution as a join back to
// the agent.mcp_tool_use event, so inventing a server name here would be a field
// the contract does not have.
//
// The use type is read from state rather than recomputed from the tool
// definition and the version gate, because a parked call is answered by whatever
// execution happens to resume it. That execution can be running newer code than
// the one that wrote the call, and an mcp_tool_use_id pointing at an
// agent.tool_use event would be an unreadable public ledger.
func toolResultDraft(
	toolUseEventType string,
	toolUseEventID string,
	content []any,
	isError bool,
) domain.EventDraft {
	if domain.AgentToolResultTypeFor(toolUseEventType) == domain.EvAgentMcpToolResult {
		return domain.EventDraft{
			Type: domain.EvAgentMcpToolResult,
			Payload: map[string]any{
				"mcp_tool_use_id": toolUseEventID,
				"content":         content,
				"is_error":        isError,
			},
		}
	}
	return domain.EventDraft{
		Type: domain.EvAgentToolResult,
		Payload: map[string]any{
			"tool_use_id": toolUseEventID,
			"content":     content,
			"is_error":    isError,
		},
	}
}

// planToolBatch is the pure classification boundary for one model tool-use
// round. It validates the complete batch before any side effect and then
// separates server-executed built-ins from client-action barriers.
func planToolBatch(
	toolUses []domain.ContentBlock,
	toolsByName map[string]TurnTool,
	stepsByProviderID map[string]PlannedToolStep,
	mcpToolEvents bool,
) (toolBatchPlan, turnFailure) {
	for _, use := range toolUses {
		if stepsByProviderID[use.ToolUseID].ToolStepID == "" {
			return toolBatchPlan{}, failTurn(
				"model tool request has no durable operation id",
			)
		}
		definition, ok := toolsByName[use.ToolName]
		if !ok {
			return toolBatchPlan{}, failTurn(
				"model requested a tool that is not enabled: " + use.ToolName,
			)
		}
		if definition.Kind == TurnToolAdvisor {
			if definition.Model == "" {
				return toolBatchPlan{}, failTurn("advisor tool has no configured model")
			}
			if len(use.Input) != 0 {
				return toolBatchPlan{}, failTurn("advisor tool does not accept input fields")
			}
		}
		if definition.Kind == TurnToolBuiltin &&
			definition.Permission.Type != "always_allow" &&
			definition.Permission.Type != "always_ask" {
			return toolBatchPlan{}, failTurn(
				"built-in tool has unsupported permission policy: " +
					definition.Permission.Type,
			)
		}
	}

	plan := toolBatchPlan{
		actionDrafts: make([]domain.EventDraft, 0, len(toolUses)),
	}
	for _, use := range toolUses {
		definition := toolsByName[use.ToolName]
		planned := stepsByProviderID[use.ToolUseID]
		if definition.Kind == TurnToolCoordinator || definition.Kind == TurnToolAdvisor {
			// Coordinator and Advisor primitives are private model tools. Their
			// durable public projection is the official Thread lifecycle/message
			// event set, never a generic agent.tool_use / agent.tool_result pair.
			plan.executable = append(plan.executable, plannedToolUse{
				use: use, publicEventID: planned.ToolUseEventID,
				stepID: planned.ToolStepID, definition: definition,
			})
			continue
		}
		draft := domain.EventDraft{
			ID: planned.ToolUseEventID,
			Payload: map[string]any{
				"name":  use.ToolName,
				"input": use.Input,
			},
		}
		if definition.Kind == TurnToolMCP {
			// The public event reports the bare tool name the server published.
			// use.ToolName is the namespaced model-facing alias and stays private
			// to the provider request; mcp_server_name is what lets the alias be
			// reconstructed on resume.
			draft.Payload["name"] = definition.MCPToolName
			draft.Payload["mcp_server_name"] = definition.MCPServer.Name
		}
		switch {
		case definition.Kind == TurnToolCustom:
			draft.Type = domain.EvAgentCustomToolUse
			plan.pendingActionEventIDs = append(
				plan.pendingActionEventIDs,
				planned.ToolUseEventID,
			)
		case definition.Kind == TurnToolSelfHosted:
			draft.Type = domain.EvAgentToolUse
			draft.Payload["evaluated_permission"] = "allow"
			draft.Payload[domain.InternalToolExecutionOwner] = "self_hosted"
			plan.pendingActionEventIDs = append(
				plan.pendingActionEventIDs,
				planned.ToolUseEventID,
			)
		case definition.Permission.Type == "always_ask":
			draft.Type = serverToolUseType(definition, mcpToolEvents)
			draft.Payload["evaluated_permission"] = "ask"
			plan.pendingActionEventIDs = append(
				plan.pendingActionEventIDs,
				planned.ToolUseEventID,
			)
		default:
			draft.Type = serverToolUseType(definition, mcpToolEvents)
			// The public field reports the result of permission evaluation, not
			// merely a barrier. An always_allow call has already evaluated to
			// allow even though it proceeds directly to server execution.
			draft.Payload["evaluated_permission"] = "allow"
			plan.executable = append(plan.executable, plannedToolUse{
				use:           use,
				publicEventID: planned.ToolUseEventID,
				useEventType:  draft.Type,
				stepID:        planned.ToolStepID,
				definition:    definition,
			})
		}
		plan.actionDrafts = append(plan.actionDrafts, draft)
	}
	return plan, ""
}

type toolBatchExecution struct {
	resultDrafts []domain.EventDraft
	resultBlocks []domain.ContentBlock
}

// closeInterruptedProviderToolRound keeps the private transcript legal when an
// interrupt wins after a provider response containing client tool_use blocks.
// Completed tools retain their real result; every remaining provider tool id
// receives a synthetic error result. Public output still follows the interrupt
// projection and drops uncompleted tool-use events.
func closeInterruptedProviderToolRound(
	turn *workflowTurnState,
	toolUses []domain.ContentBlock,
	completed []domain.ContentBlock,
	mappingCheckpoint int,
) {
	if !turn.usesProviderTranscript {
		return
	}
	byProviderID := make(map[string]domain.ContentBlock, len(completed))
	for _, block := range completed {
		if block.Type == "tool_result" && block.ToolResultFor != "" {
			byProviderID[block.ToolResultFor] = block
		}
	}
	results := make([]domain.ContentBlock, 0, len(toolUses))
	for _, use := range toolUses {
		if block, ok := byProviderID[use.ToolUseID]; ok {
			results = append(results, block)
			continue
		}
		results = append(results, domain.ContentBlock{
			Type:          "tool_result",
			ToolResultFor: use.ToolUseID,
			Text:          "Tool execution was interrupted before a result was committed.",
			IsError:       true,
		})
	}
	if len(results) > 0 {
		turn.transcriptDelta = agentruntime.AppendMerging(
			turn.transcriptDelta,
			[]domain.Message{{
				Role: domain.RoleUser, Content: results,
			}},
		)
	}

	if mappingCheckpoint < 0 || mappingCheckpoint > len(turn.toolUseMappings) {
		mappingCheckpoint = len(turn.toolUseMappings)
	}
	filtered := append(
		[]domain.ProviderToolUseMapping(nil),
		turn.toolUseMappings[:mappingCheckpoint]...,
	)
	for _, mapping := range turn.toolUseMappings[mappingCheckpoint:] {
		if _, completed := byProviderID[mapping.ProviderToolUseID]; completed {
			filtered = append(filtered, mapping)
		}
	}
	turn.toolUseMappings = filtered
}

// executeToolBatch remains Workflow-scoped: each tool execution is a durable
// Activity and its command order and ordinal are part of replay history.
func executeToolBatch(
	turn *workflowTurnState,
	attemptID string,
	plan toolBatchPlan,
	executorRequest model.Request,
	assistantContent []domain.ContentBlock,
) (toolBatchExecution, bool, turnFailure, error) {
	execution := toolBatchExecution{
		resultDrafts: make([]domain.EventDraft, 0, len(plan.executable)),
		resultBlocks: make([]domain.ContentBlock, 0, len(plan.executable)),
	}
	injectedBlocks := make([]domain.ContentBlock, 0, len(plan.executable))
	for _, planned := range plan.executable {
		var advisorRequest model.Request
		var consultation domain.AdvisorConsultation
		if planned.definition.Kind == TurnToolAdvisor {
			if turn.perRequestUsageAccounting {
				if err := turn.flushOutput(); err != nil {
					return toolBatchExecution{}, false, "", err
				}
				if err := turn.awaitModelRequestAdmission(); err != nil {
					return toolBatchExecution{}, false, "", err
				}
			}
			var err error
			advisorRequest, consultation, err = advisorRequestForTool(
				planned.definition,
				executorRequest,
				assistantContent,
			)
			if err != nil {
				return toolBatchExecution{}, false, failTurn(err.Error()), nil
			}
			ids := advisorConsultationIDs(planned.stepID)
			consultation.ThreadID = ids.ThreadID
			consultation.UsageRequestID = ids.UsageRequestID
			consultation.LifecycleIDs = ids.LifecycleIDs
		}
		executed, activityOutcome, err := turn.executeTool(
			attemptID,
			planned.use,
			planned.publicEventID,
			planned.stepID,
			planned.definition,
			advisorRequest,
			consultation,
		)
		if err != nil {
			return toolBatchExecution{}, false, "", err
		}
		if activityOutcome.Interrupted && !activityOutcome.Completed {
			execution.resultBlocks = append(execution.resultBlocks, injectedBlocks...)
			return execution, true, "", nil
		}
		if executed.FatalError != "" {
			if activityOutcome.Interrupted {
				execution.resultBlocks = append(execution.resultBlocks, injectedBlocks...)
				return execution, true, "", nil
			}
			return toolBatchExecution{}, false, failTurn(executed.FatalError), nil
		}
		if executed.Ambiguous {
			if activityOutcome.Interrupted {
				execution.resultBlocks = append(execution.resultBlocks, injectedBlocks...)
				return execution, true, "", nil
			}
			return toolBatchExecution{}, false, failTurn(
				"a tool began executing but no trustworthy result was recorded; " +
					"the side effect will not be retried",
			), nil
		}
		execution.resultDrafts = append(execution.resultDrafts, executed.Events...)
		if planned.definition.Kind != TurnToolCoordinator &&
			planned.definition.Kind != TurnToolAdvisor {
			execution.resultDrafts = append(execution.resultDrafts, toolResultDraft(
				planned.useEventType,
				planned.publicEventID,
				executed.Result.Content,
				executed.Result.IsError,
			))
		}
		execution.resultBlocks = append(execution.resultBlocks, domain.ContentBlock{
			Type:          "tool_result",
			ToolResultFor: planned.use.ToolUseID,
			Text:          agentruntime.FlattenResultText(executed.Result.Content),
			IsError:       executed.Result.IsError,
			ResultContent: rawResultContent(executed.Result.Content),
		})
		injectedBlocks = append(injectedBlocks, executed.Result.InjectedContent...)
		if activityOutcome.Interrupted {
			execution.resultBlocks = append(execution.resultBlocks, injectedBlocks...)
			return execution, true, "", nil
		}
	}
	execution.resultBlocks = append(execution.resultBlocks, injectedBlocks...)
	return execution, false, "", nil
}

func rawResultContent(content []any) []json.RawMessage {
	hasRichContent := false
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok || block["type"] != "text" {
			hasRichContent = true
			break
		}
	}
	if !hasRichContent {
		return nil
	}
	out := make([]json.RawMessage, 0, len(content))
	for _, item := range content {
		raw, err := json.Marshal(item)
		if err == nil {
			out = append(out, json.RawMessage(raw))
		}
	}
	return out
}
