package temporal

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"

	"go.temporal.io/sdk/workflow"

	"github.com/yanpgwang/mango/internal/agentruntime"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

const (
	// maxWorkflowToolRounds is the deterministic safety bound for one public
	// turn. It prevents a model that continually requests tools from growing
	// Workflow history forever.
	maxWorkflowToolRounds = 20

	// Provider continuations are successful Messages API responses, not
	// transport retries. Bound them independently so a provider that never
	// reaches a natural stop cannot grow Workflow history forever.
	maxPauseTurnContinuations = 5
	maxOutputContinuations    = 3

	// maxModelRequestAttempts bounds provider-level retries for one immutable
	// model request. Infrastructure failures remain Activity errors and retain
	// Temporal's unbounded recovery policy.
	maxModelRequestAttempts = 3
	modelRetryInitialDelay  = time.Second
	modelRetryMaximumDelay  = time.Minute
)

const (
	liveModelSpanStartChangeID = "live-model-request-span-start"
	liveModelSpanStartVersion  = 1

	// mcpToolEventsChangeID gates the wire-breaking move of MCP tool calls from
	// agent.tool_use/agent.tool_result onto the documented
	// agent.mcp_tool_use/agent.mcp_tool_result pair. A Workflow execution that
	// recorded no marker for this change keeps writing new MCP calls as
	// agent.tool_use, so an in-flight turn replays deterministically.
	//
	// The gate governs exactly one decision: which type a *new* tool use is
	// written as. It cannot govern anything more, because workflow.GetVersion
	// memoizes per Workflow execution while the public event ledger is
	// cross-execution. SessionWorkflow continues-as-new (see
	// continueAsNewThreshold), which starts a fresh history whose gate re-resolves
	// to the current version on an upgraded worker, while PostgreSQL still holds
	// every event the previous executions published.
	//
	// General principle: a version gate guarantees code-branch consistency within
	// one execution; it cannot guarantee consistency of cross-execution persisted
	// state. Any semantic that must survive Continue-As-New has to be derived from
	// durable state. Here the tool-result variant is derived from the committed
	// tool-use event type (ResumeAction.ActionEventType for a resumed barrier,
	// plannedToolUse.useEventType within a round), never from this gate.
	mcpToolEventsChangeID = "mcp-tool-event-types"
	mcpToolEventsVersion  = 1

	// terminalSessionErrorChangeID gates the public transition from the legacy
	// HTTP-style api_error payload to the documented Session Event error union.
	// Existing Workflow histories retain their recorded command payload; new
	// executions publish unknown_error with an explicit terminal retry status.
	terminalSessionErrorChangeID = "terminal-session-error-contract"
	terminalSessionErrorVersion  = 1

	outcomeEvaluationHeartbeatChangeID = "outcome-evaluation-heartbeats"
	outcomeEvaluationHeartbeatVersion  = 1

	modelRetryLifecycleChangeID = "public-model-retry-lifecycle"
	modelRetryLifecycleVersion  = 1

	contextCompactionEventChangeID = "thread-context-compaction-event"
	contextCompactionEventVersion  = 1

	providerStopReasonChangeID = "provider-stop-reason-state-machine"
	providerStopReasonVersion  = 1

	modelRequestAccountingChangeID = "per-model-request-usage-accounting"
	modelRequestAccountingVersion  = 1

	cumulativePauseContinuationChangeID = "cumulative-pause-turn-continuations"
	cumulativePauseContinuationVersion  = 1
)

type providerResponseDisposition uint8

const (
	providerResponseComplete providerResponseDisposition = iota
	providerResponseExecuteTools
	providerResponseContinuePause
	providerResponseContinueOutput
)

// runWorkflowTurn owns the plan-act-observe loop in deterministic Workflow
// code. Every model call and every tool call is an Activity, so each completed
// response/result is independently recorded in Temporal history and replay
// resumes at the next unfinished step.
func runWorkflowTurn(
	actx workflow.Context,
	sessionID string,
	triggerEventID string,
) (RunTurnResult, error) {
	return runWorkflowTurnInternal(
		actx,
		sessionID,
		triggerEventID,
		nil,
		nil,
	)
}

func runWorkflowTurnWithResolutions(
	actx workflow.Context,
	sessionID string,
	triggerEventID string,
	resolutionEventIDs []string,
) (RunTurnResult, error) {
	return runWorkflowTurnInternal(
		actx,
		sessionID,
		triggerEventID,
		resolutionEventIDs,
		nil,
	)
}

func runWorkflowTurnInternal(
	actx workflow.Context,
	sessionID string,
	triggerEventID string,
	resolutionEventIDs []string,
	interrupts *turnInterruptWatcher,
) (RunTurnResult, error) {
	var prepared PrepareTurnResult
	if err := workflow.ExecuteActivity(actx, ActivityPrepareTurn, PrepareTurnInput{
		SessionID:          sessionID,
		TriggerEventID:     triggerEventID,
		ResolutionEventIDs: resolutionEventIDs,
	}).Get(actx, &prepared); err != nil {
		return RunTurnResult{}, err
	}
	if prepared.AlreadyCompleted {
		if prepared.Terminated {
			return RunTurnResult{Disposition: TurnTerminated}, nil
		}
		return RunTurnResult{Disposition: TurnCompleted}, nil
	}
	liveModelSpanStarts := workflow.GetVersion(
		actx,
		liveModelSpanStartChangeID,
		workflow.DefaultVersion,
		liveModelSpanStartVersion,
	) == liveModelSpanStartVersion
	// Evaluated once before the tool loop so every model round of this turn writes
	// new tool uses under one naming scheme. Its position in the command stream is
	// part of replay history and must not change. Resumed barriers deliberately do
	// not consult it; they follow the type their parked event already carries.
	mcpToolEvents := workflow.GetVersion(
		actx,
		mcpToolEventsChangeID,
		workflow.DefaultVersion,
		mcpToolEventsVersion,
	) == mcpToolEventsVersion
	terminalSessionErrors := workflow.GetVersion(
		actx,
		terminalSessionErrorChangeID,
		workflow.DefaultVersion,
		terminalSessionErrorVersion,
	) == terminalSessionErrorVersion
	outcomeEvaluationHeartbeats := workflow.GetVersion(
		actx,
		outcomeEvaluationHeartbeatChangeID,
		workflow.DefaultVersion,
		outcomeEvaluationHeartbeatVersion,
	) == outcomeEvaluationHeartbeatVersion
	modelRetryLifecycle := workflow.GetVersion(
		actx,
		modelRetryLifecycleChangeID,
		workflow.DefaultVersion,
		modelRetryLifecycleVersion,
	) == modelRetryLifecycleVersion
	contextCompactionEvents := workflow.GetVersion(
		actx,
		contextCompactionEventChangeID,
		workflow.DefaultVersion,
		contextCompactionEventVersion,
	) == contextCompactionEventVersion
	providerStopReasons := workflow.GetVersion(
		actx,
		providerStopReasonChangeID,
		workflow.DefaultVersion,
		providerStopReasonVersion,
	) == providerStopReasonVersion
	modelRequestAccounting := workflow.GetVersion(
		actx,
		modelRequestAccountingChangeID,
		workflow.DefaultVersion,
		modelRequestAccountingVersion,
	) == modelRequestAccountingVersion
	cumulativePauseContinuations := workflow.GetVersion(
		actx,
		cumulativePauseContinuationChangeID,
		workflow.DefaultVersion,
		cumulativePauseContinuationVersion,
	) == cumulativePauseContinuationVersion
	initialOutput := append([]domain.EventDraft(nil), prepared.PreludeEvents...)
	if contextCompactionEvents && prepared.ContextProjection.Compacted {
		initialOutput = append(initialOutput, domain.EventDraft{
			Type:    domain.EvAgentThreadContextCompacted,
			Payload: map[string]any{},
		})
	}

	turn := &workflowTurnState{
		actx:                     actx,
		sessionID:                sessionID,
		threadID:                 prepared.ThreadID,
		isChild:                  prepared.IsChild,
		skillRuntimeRoot:         prepared.SkillRuntimeRoot,
		triggerEventID:           triggerEventID,
		resolutionEventIDs:       resolutionEventIDs,
		interrupts:               interrupts,
		terminalSessionErrors:    terminalSessionErrors,
		outcomeHeartbeats:        outcomeEvaluationHeartbeats,
		outcomeHeartbeatInterval: outcomeEvaluationHeartbeatInterval,
		usesProviderTranscript:   prepared.UsesProviderTranscript,
		output:                   initialOutput,
		transcriptDelta: append(
			[]domain.Message(nil),
			prepared.TranscriptDelta...,
		),
		loadedSkills:              agentruntime.LoadedRuntimeSkills(prepared.Request.Messages),
		perRequestUsageAccounting: modelRequestAccounting,
		sessionOutputsEnabled:     prepared.SessionOutputsEnabled,
	}
	if prepared.FatalError != "" {
		return turn.terminate(failTurn(prepared.FatalError))
	}

	toolsByName := indexTurnTools(prepared.Tools)
	messages, interrupted, failure, err := resumeWorkflowTurn(
		turn,
		prepared,
		toolsByName,
		append([]domain.Message(nil), prepared.Request.Messages...),
	)
	if err != nil {
		return RunTurnResult{}, err
	}
	if interrupted {
		return turn.completeInterrupted()
	}
	if failure != "" {
		return turn.terminate(failure)
	}

	maxRounds := maxWorkflowToolRounds
	if prepared.Outcome != nil {
		// Each evaluation cycle may consume a full tool loop, and
		// max_iterations_reached is followed by one final acknowledgment turn.
		maxRounds *= prepared.Outcome.MaxIterations + 1
	}
	outcomeIteration := 0
	outcomeFinished := false
	pauseTurnContinuations := 0
	outputContinuations := 0
	pauseChainActive := false
	var pauseMessagesBase []domain.Message
	var pauseTranscriptBase []domain.Message
	for round := 0; round < maxRounds; round++ {
		// A later model request must never overtake completed public progress
		// from the preceding tool/outcome round in PostgreSQL receipt order.
		if liveModelSpanStarts {
			if err := turn.flushOutput(); err != nil {
				return RunTurnResult{}, err
			}
		}
		request := prepared.Request
		request.Messages = messages
		preparedContext, err := prepareRequestContext(request, false, 0)
		if err != nil {
			return turn.terminate(failTurn(err.Error()))
		}
		request = preparedContext.Request
		if preparedContext.Projection.Compacted {
			messages = request.Messages
			turn.output = append(turn.output, domain.EventDraft{
				Type: domain.EvAgentThreadContextCompacted, Payload: map[string]any{},
			})
		}
		mappingCheckpoint := len(turn.toolUseMappings)
		var called CallModelResult
		var activityOutcome interruptibleActivityOutcome
		recoveredContextOverflow := false
		for attempt := 0; ; attempt++ {
			modelRequestStartID, modelRequestEndID := modelRequestAttemptSpanIDs(
				sessionID,
				triggerEventID,
				round,
				attempt,
			)
			if modelRequestAccounting {
				if err := turn.awaitModelRequestAdmission(); err != nil {
					return RunTurnResult{}, err
				}
			}
			if liveModelSpanStarts {
				if err := turn.startModelRequest(modelRequestStartID); err != nil {
					return RunTurnResult{}, err
				}
			}

			var err error
			previewThreadID := ""
			if prepared.IsChild {
				previewThreadID = prepared.ThreadID
			}
			called, activityOutcome, err = turn.callModel(CallModelInput{
				SessionID:             sessionID,
				ThreadID:              previewThreadID,
				ModelRequestStartID:   modelRequestStartID,
				ModelRequestEndID:     modelRequestEndID,
				HandleRetryableErrors: modelRetryLifecycle,
				Request:               request,
			})
			if err != nil {
				return RunTurnResult{}, err
			}
			if modelRequestAccounting && activityOutcome.Completed && hasTokenUsage(called.Response.Usage) {
				if err := turn.accountModelRequest(
					called.ModelRequestEndID,
					request,
					called.Response.Usage,
					called.Response.StopReason,
				); err != nil {
					return RunTurnResult{}, err
				}
			}
			if activityOutcome.Interrupted && !activityOutcome.Completed {
				cancelled := CallModelResult{
					ModelRequestStartID: modelRequestStartID,
					ModelRequestEndID:   modelRequestEndID,
				}
				if !liveModelSpanStarts {
					if start := modelRequestStartDraft(cancelled); start != nil {
						turn.output = append(turn.output, *start)
					}
				}
				if end := modelRequestEndDraft(cancelled, true); end != nil {
					turn.output = append(turn.output, *end)
				}
				return turn.completeInterrupted()
			}
			if !liveModelSpanStarts {
				if start := modelRequestStartDraft(called); start != nil {
					turn.output = append(turn.output, *start)
				}
			}
			if called.ContextOverflow {
				if end := modelRequestEndDraft(called, true); end != nil {
					turn.output = append(turn.output, *end)
				}
				if activityOutcome.Interrupted {
					return turn.completeInterrupted()
				}
				if recoveredContextOverflow {
					message := called.ContextOverflowError
					if message == "" {
						message = "model request exceeded the provider context window after compaction"
					}
					return turn.terminateTyped("model_request_failed_error", message)
				}
				recovered, recoveryErr := prepareRequestContext(request, true, 0)
				if recoveryErr != nil {
					return turn.terminateTyped(
						"model_request_failed_error", recoveryErr.Error(),
					)
				}
				request = recovered.Request
				messages = request.Messages
				recoveredContextOverflow = true
				turn.output = append(turn.output, domain.EventDraft{
					Type:    domain.EvAgentThreadContextCompacted,
					Payload: map[string]any{},
				})
				if err := turn.flushOutput(); err != nil {
					return RunTurnResult{}, err
				}
				continue
			}
			if called.FatalError != "" {
				if end := modelRequestEndDraft(called, true); end != nil {
					turn.output = append(turn.output, *end)
				}
				if activityOutcome.Interrupted {
					return turn.completeInterrupted()
				}
				return turn.terminateTyped(called.FatalErrorType, called.FatalError)
			}
			if called.RetryError == nil {
				break
			}
			if end := modelRequestEndDraft(called, true); end != nil {
				turn.output = append(turn.output, *end)
			}
			if activityOutcome.Interrupted {
				return turn.completeInterrupted()
			}
			if attempt+1 >= maxModelRequestAttempts {
				return turn.exhaustModelRetries(*called.RetryError)
			}
			if err := turn.flushOutput(); err != nil {
				return RunTurnResult{}, err
			}
			errorEventID, rescheduledEventID, runningEventID := modelRetryEventIDs(
				sessionID, triggerEventID, round, attempt,
			)
			if err := turn.recordModelRetry(
				*called.RetryError, errorEventID, rescheduledEventID,
			); err != nil {
				return RunTurnResult{}, err
			}
			interrupted, err := turn.waitModelRetry(
				modelRetryDelay(*called.RetryError, attempt),
			)
			if err != nil {
				return RunTurnResult{}, err
			}
			if interrupted {
				return turn.completeInterrupted()
			}
			if err := turn.resumeModelRetry(runningEventID); err != nil {
				return RunTurnResult{}, err
			}
		}
		assistantMessage := model.AnchoredAssistantMessage(request, called.Response)
		var toolUses []domain.ContentBlock
		for _, block := range called.Response.Content {
			if block.Type == "tool_use" {
				toolUses = append(toolUses, block)
			}
		}
		disposition := providerResponseComplete
		providerFailure := ""
		if providerStopReasons {
			disposition, providerFailure = classifyProviderResponse(
				called.Response.StopReason,
				len(toolUses),
			)
		}

		if prepared.UsesProviderTranscript {
			if !cumulativePauseContinuations && providerStopReasons &&
				disposition == providerResponseContinuePause {
				if pauseChainActive {
					turn.transcriptDelta = append(
						[]domain.Message(nil),
						pauseTranscriptBase...,
					)
				} else {
					pauseTranscriptBase = append(
						[]domain.Message(nil),
						turn.transcriptDelta...,
					)
				}
			}
			turn.transcriptDelta = agentruntime.AppendMerging(
				turn.transcriptDelta,
				[]domain.Message{assistantMessage},
			)
			for _, planned := range called.ToolSteps {
				if providerStopReasons && disposition != providerResponseExecuteTools {
					break
				}
				providerID := planned.ProviderToolUseID
				if providerID == "" {
					providerID = planned.ToolUseEventID
				}
				toolName := toolNameForProviderID(
					called.Response.Content,
					providerID,
				)
				kind := toolsByName[toolName].Kind
				if kind == TurnToolCoordinator || kind == TurnToolAdvisor {
					// Private runtime tools have no generic public tool-use event. Their
					// provider ids are already preserved in the private transcript and
					// must not map to a synthetic public id.
					continue
				}
				turn.toolUseMappings = append(
					turn.toolUseMappings,
					domain.ProviderToolUseMapping{
						PublicEventID:     planned.ToolUseEventID,
						ProviderToolUseID: providerID,
						ToolName:          toolName,
					},
				)
			}
		}
		if !cumulativePauseContinuations && providerStopReasons &&
			disposition != providerResponseContinuePause {
			pauseChainActive = false
		}
		if called.ThinkingEventID != "" {
			turn.output = append(turn.output, domain.EventDraft{
				ID: called.ThinkingEventID, Type: domain.EvAgentThinking,
				Payload: map[string]any{},
			})
		}

		if content := agentruntime.TextBlocksToContent(called.Response.Content); len(content) > 0 {
			if called.MessageEventID == "" {
				if activityOutcome.Interrupted {
					return turn.completeInterrupted()
				}
				return turn.terminate(failTurn(
					"model response text has no durable public event id",
				))
			}
			turn.output = append(turn.output, domain.EventDraft{
				ID:      called.MessageEventID,
				Type:    domain.EvAgentMessage,
				Payload: map[string]any{"content": content},
			})
		}

		if providerFailure != "" {
			if end := modelRequestEndDraft(called, false); end != nil {
				turn.output = append(turn.output, *end)
			}
			if activityOutcome.Interrupted {
				return turn.completeInterrupted()
			}
			return turn.terminateTyped(
				"model_request_failed_error",
				providerFailure,
			)
		}
		if providerStopReasons {
			switch disposition {
			case providerResponseContinuePause:
				if end := modelRequestEndDraft(called, false); end != nil {
					turn.output = append(turn.output, *end)
				}
				if activityOutcome.Interrupted {
					return turn.completeInterrupted()
				}
				if pauseTurnContinuations >= maxPauseTurnContinuations {
					return turn.terminateTyped(
						"model_request_failed_error",
						"model exceeded the pause_turn continuation limit",
					)
				}
				pauseTurnContinuations++
				if !cumulativePauseContinuations {
					if pauseChainActive {
						messages = append([]domain.Message(nil), pauseMessagesBase...)
					} else {
						pauseMessagesBase = append([]domain.Message(nil), messages...)
					}
				}
				messages = agentruntime.AppendMerging(
					messages, []domain.Message{assistantMessage},
				)
				if !cumulativePauseContinuations {
					pauseChainActive = true
				}
				continue
			case providerResponseContinueOutput:
				if end := modelRequestEndDraft(called, false); end != nil {
					turn.output = append(turn.output, *end)
				}
				if activityOutcome.Interrupted {
					return turn.completeInterrupted()
				}
				if outputContinuations >= maxOutputContinuations {
					return turn.terminateTyped(
						"model_request_failed_error",
						"model exceeded the max_tokens continuation limit",
					)
				}
				outputContinuations++
				recoveryMessage := domain.Message{
					Role: domain.RoleUser,
					Content: []domain.ContentBlock{{
						Type: "text",
						Text: "Output token limit reached. Continue directly from where you stopped without apologizing or recapping.",
					}},
				}
				messages = agentruntime.AppendMerging(messages, []domain.Message{
					assistantMessage,
					recoveryMessage,
				})
				if prepared.UsesProviderTranscript {
					turn.transcriptDelta = agentruntime.AppendMerging(
						turn.transcriptDelta,
						[]domain.Message{recoveryMessage},
					)
				}
				continue
			}
		}
		if len(toolUses) == 0 {
			if end := modelRequestEndDraft(called, false); end != nil {
				turn.output = append(turn.output, *end)
			}
			if prepared.Outcome == nil || outcomeFinished {
				return turn.complete(nil)
			}
			candidate := agentruntime.AppendMerging(
				messages, []domain.Message{assistantMessage},
			)
			finalCycle := outcomeIteration+1 >= prepared.Outcome.MaxIterations
			evaluationStartID, evaluationEndID := outcomeEvaluationSpanIDs(
				sessionID,
				triggerEventID,
				outcomeIteration,
			)
			if modelRequestAccounting {
				if err := turn.flushOutput(); err != nil {
					return RunTurnResult{}, err
				}
				if err := turn.awaitModelRequestAdmission(); err != nil {
					return RunTurnResult{}, err
				}
			}
			if outcomeEvaluationHeartbeats {
				turn.output = append(turn.output, outcomeEvaluationStartDraft(
					*prepared.Outcome,
					outcomeIteration,
					evaluationStartID,
				))
				if err := turn.flushOutput(); err != nil {
					return RunTurnResult{}, err
				}
				turn.activeOutcomeEvaluationStartID = evaluationStartID
				turn.activeOutcomeIteration = outcomeIteration
			}
			evaluated, evaluationOutcome, err := turn.evaluateOutcome(
				EvaluateOutcomeInput{
					SessionID:    sessionID,
					StartEventID: evaluationStartID,
					EndEventID:   evaluationEndID,
					Model:        prepared.Request.Model,
					Effort:       prepared.Request.Effort,
					Speed:        prepared.Request.Speed,
					Outcome:      *prepared.Outcome,
					Candidate:    candidate,
					Iteration:    outcomeIteration,
					FinalCycle:   finalCycle,
				},
			)
			if err != nil {
				return RunTurnResult{}, err
			}
			if evaluationOutcome.Interrupted && !evaluationOutcome.Completed {
				return turn.completeInterrupted()
			}
			if evaluated.FatalError != "" {
				if evaluationOutcome.Interrupted {
					return turn.completeInterrupted()
				}
				if outcomeEvaluationHeartbeats {
					turn.output = append(turn.output, outcomeEvaluationFailureDraft(
						*prepared.Outcome,
						outcomeIteration,
						evaluationStartID,
						evaluationEndID,
						evaluated,
					))
					turn.activeOutcomeEvaluationStartID = ""
				}
				return turn.terminate(failTurn(evaluated.FatalError))
			}
			if outcomeEvaluationHeartbeats {
				turn.output = append(turn.output, outcomeEvaluationEndDraft(
					*prepared.Outcome,
					outcomeIteration,
					evaluated,
				))
				if !evaluationOutcome.Interrupted {
					turn.activeOutcomeEvaluationStartID = ""
				}
			} else {
				turn.output = append(
					turn.output,
					outcomeEvaluationDrafts(
						*prepared.Outcome,
						outcomeIteration,
						evaluated,
					)...,
				)
			}
			if evaluationOutcome.Interrupted {
				return turn.completeInterrupted()
			}
			switch evaluated.Result {
			case "satisfied", "failed":
				return turn.complete(nil)
			case "needs_revision", "max_iterations_reached":
				feedback := "Independent outcome evaluation: " + evaluated.Explanation
				if evaluated.Result == "max_iterations_reached" {
					feedback += "\nThe evaluation budget is exhausted. Provide one final acknowledgment of the best available result."
					outcomeFinished = true
				} else {
					feedback += "\nRevise the deliverable to address this feedback, then present the updated result."
					outcomeIteration++
				}
				feedbackMessage := domain.Message{
					Role:    domain.RoleUser,
					Content: []domain.ContentBlock{{Type: "text", Text: feedback}},
				}
				messages = agentruntime.AppendMerging(candidate, []domain.Message{feedbackMessage})
				if prepared.UsesProviderTranscript {
					turn.transcriptDelta = agentruntime.AppendMerging(
						turn.transcriptDelta,
						[]domain.Message{feedbackMessage},
					)
				}
				continue
			default:
				return turn.terminate(failTurn("grader returned an unsupported outcome result"))
			}
		}
		if end := modelRequestEndDraft(called, false); end != nil {
			turn.output = append(turn.output, *end)
		}
		if prepared.AttemptID == "" {
			if activityOutcome.Interrupted {
				closeInterruptedProviderToolRound(
					turn,
					toolUses,
					nil,
					mappingCheckpoint,
				)
				return turn.completeInterrupted()
			}
			return turn.terminate(failTurn("tool-using turn has no durable attempt id"))
		}

		stepsByProviderID := make(
			map[string]PlannedToolStep,
			len(called.ToolSteps),
		)
		for _, planned := range called.ToolSteps {
			providerID := planned.ProviderToolUseID
			if providerID == "" {
				providerID = planned.ToolUseEventID
			}
			stepsByProviderID[providerID] = planned
		}
		plan, failure := planToolBatch(
			toolUses,
			toolsByName,
			stepsByProviderID,
			mcpToolEvents,
		)
		if failure != "" {
			if activityOutcome.Interrupted {
				closeInterruptedProviderToolRound(
					turn,
					toolUses,
					nil,
					mappingCheckpoint,
				)
				return turn.completeInterrupted()
			}
			return turn.terminate(failure)
		}
		turn.output = append(turn.output, plan.actionDrafts...)
		if activityOutcome.Interrupted {
			closeInterruptedProviderToolRound(
				turn,
				toolUses,
				nil,
				mappingCheckpoint,
			)
			return turn.completeInterrupted()
		}

		executed, interrupted, failure, err := executeToolBatch(
			turn,
			prepared.AttemptID,
			plan,
			request,
			called.Response.Content,
		)
		if err != nil {
			return RunTurnResult{}, err
		}
		turn.output = append(turn.output, executed.resultDrafts...)
		if interrupted {
			closeInterruptedProviderToolRound(
				turn,
				toolUses,
				executed.resultBlocks,
				mappingCheckpoint,
			)
			return turn.completeInterrupted()
		}
		if failure != "" {
			return turn.terminate(failure)
		}

		// A model may request an executable tool and an approval/custom tool in
		// the same assistant block. Persist completed executable results before
		// parking so every tool_use already executed has a matching tool_result
		// when the remaining pending actions resume.
		if prepared.UsesProviderTranscript && len(executed.resultBlocks) > 0 {
			turn.transcriptDelta = agentruntime.AppendMerging(
				turn.transcriptDelta,
				[]domain.Message{{
					Role:    domain.RoleUser,
					Content: executed.resultBlocks,
				}},
			)
		}
		if len(plan.pendingActionEventIDs) > 0 {
			return turn.complete(plan.pendingActionEventIDs)
		}

		// Preserve the model's exact assistant round, including text emitted
		// alongside tool_use blocks, then append the paired tool results.
		messages = agentruntime.AppendMerging(messages, []domain.Message{
			assistantMessage,
			{Role: domain.RoleUser, Content: executed.resultBlocks},
		})
	}

	if providerStopReasons {
		return turn.terminateTyped(
			"model_request_failed_error",
			"agent loop exceeded the per-turn round limit",
		)
	}
	// Preserve the legacy replay outcome for Workflow histories that crossed
	// the safety bound before stop-reason handling was introduced.
	return turn.complete(nil)
}

func classifyProviderResponse(
	stopReason string,
	toolUseCount int,
) (providerResponseDisposition, string) {
	switch stopReason {
	case "end_turn", "stop_sequence", "refusal", "model_context_window_exceeded":
		if toolUseCount > 0 {
			return providerResponseComplete,
				"model returned " + stopReason + " with a client tool_use block"
		}
		return providerResponseComplete, ""
	case "tool_use":
		if toolUseCount == 0 {
			return providerResponseComplete,
				"model returned tool_use without a client tool_use block"
		}
		return providerResponseExecuteTools, ""
	case "pause_turn":
		if toolUseCount > 0 {
			return providerResponseComplete,
				"model returned pause_turn with a client tool_use block"
		}
		return providerResponseContinuePause, ""
	case "max_tokens":
		if toolUseCount > 0 {
			return providerResponseComplete,
				"model returned max_tokens with a potentially incomplete client tool_use block"
		}
		return providerResponseContinueOutput, ""
	case "":
		return providerResponseComplete, "model response has no stop_reason"
	default:
		return providerResponseComplete,
			"model returned unsupported stop_reason " + strconv.Quote(stopReason)
	}
}

// modelRequestSpanIDs are deterministic Workflow-owned operation ids. Owning
// them before the interruptible model Activity starts lets an interrupt commit
// the terminal span.model_request_end that closes any best-effort preview, even
// when cancellation prevents the Activity result from being recorded.
func modelRequestSpanIDs(sessionID, triggerEventID string, round int) (string, string) {
	makeID := func(kind string) string {
		sum := sha256.Sum256([]byte(
			sessionID + "\x00" + triggerEventID + "\x00" + strconv.Itoa(round) + "\x00" + kind,
		))
		return domain.PrefixEvent + hex.EncodeToString(sum[:12])
	}
	return makeID("model_start"), makeID("model_end")
}

func advisorConsultationIDs(stepID string) domain.AdvisorConsultation {
	makeID := func(prefix, kind string) string {
		sum := sha256.Sum256([]byte(stepID + "\x00advisor\x00" + kind))
		return prefix + hex.EncodeToString(sum[:12])
	}
	consultation := domain.AdvisorConsultation{
		ThreadID:       makeID(domain.PrefixSessionThread, "thread"),
		UsageRequestID: makeID(domain.PrefixEvent, "usage"),
		LifecycleIDs:   make([]string, 9),
	}
	for index := range consultation.LifecycleIDs {
		consultation.LifecycleIDs[index] = makeID(
			domain.PrefixEvent,
			"lifecycle_"+strconv.Itoa(index),
		)
	}
	return consultation
}

func advisorRequestForTool(
	definition TurnTool,
	executor model.Request,
	assistant []domain.ContentBlock,
) (model.Request, domain.AdvisorConsultation, error) {
	request, err := agentruntime.AdvisorRequest(
		definition.Model,
		executor,
		assistant,
	)
	return request, domain.AdvisorConsultation{Model: definition.Model}, err
}

func modelRequestAttemptSpanIDs(
	sessionID string,
	triggerEventID string,
	round int,
	attempt int,
) (string, string) {
	if attempt == 0 {
		return modelRequestSpanIDs(sessionID, triggerEventID, round)
	}
	makeID := func(kind string) string {
		sum := sha256.Sum256([]byte(
			sessionID + "\x00" + triggerEventID + "\x00" + strconv.Itoa(round) +
				"\x00retry\x00" + strconv.Itoa(attempt) + "\x00" + kind,
		))
		return domain.PrefixEvent + hex.EncodeToString(sum[:12])
	}
	return makeID("model_start"), makeID("model_end")
}

func modelRetryEventIDs(
	sessionID string,
	triggerEventID string,
	round int,
	attempt int,
) (string, string, string) {
	makeID := func(kind string) string {
		sum := sha256.Sum256([]byte(
			sessionID + "\x00" + triggerEventID + "\x00" + strconv.Itoa(round) +
				"\x00retry_state\x00" + strconv.Itoa(attempt) + "\x00" + kind,
		))
		return domain.PrefixEvent + hex.EncodeToString(sum[:12])
	}
	return makeID("error"), makeID("rescheduled"), makeID("running")
}

func modelRetryDelay(retry ModelRetryError, attempt int) time.Duration {
	if retry.RetryAfterMillis > 0 {
		delay := time.Duration(retry.RetryAfterMillis) * time.Millisecond
		if delay > modelRetryMaximumDelay {
			return modelRetryMaximumDelay
		}
		return delay
	}
	delay := modelRetryInitialDelay * time.Duration(1<<attempt)
	if delay > modelRetryMaximumDelay {
		return modelRetryMaximumDelay
	}
	return delay
}

func outcomeEvaluationSpanIDs(
	sessionID string,
	triggerEventID string,
	iteration int,
) (string, string) {
	makeID := func(kind string) string {
		sum := sha256.Sum256([]byte(
			sessionID + "\x00" + triggerEventID + "\x00outcome\x00" +
				strconv.Itoa(iteration) + "\x00" + kind,
		))
		return domain.PrefixEvent + hex.EncodeToString(sum[:12])
	}
	return makeID("evaluation_start"), makeID("evaluation_end")
}

func outcomeEvaluationHeartbeatID(startEventID string, ordinal int) string {
	sum := sha256.Sum256([]byte(
		startEventID + "\x00heartbeat\x00" + strconv.Itoa(ordinal),
	))
	return domain.PrefixEvent + hex.EncodeToString(sum[:12])
}

func workflowProgressEventID(
	sessionID string,
	triggerEventID string,
	ordinal int,
) string {
	sum := sha256.Sum256([]byte(
		sessionID + "\x00" + triggerEventID + "\x00progress\x00" +
			strconv.Itoa(ordinal),
	))
	return domain.PrefixEvent + hex.EncodeToString(sum[:12])
}

func toolNameForProviderID(
	blocks []domain.ContentBlock,
	providerID string,
) string {
	for _, block := range blocks {
		if block.Type == "tool_use" && block.ToolUseID == providerID {
			return block.ToolName
		}
	}
	return ""
}
