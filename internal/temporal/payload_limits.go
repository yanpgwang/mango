package temporal

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/proto"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

// Leave room below Temporal's default 2 MiB payload / 4 MiB transaction limits
// for envelopes and the small commands accompanying a content Activity.
const maxContentPayloadBytes = 1024 * 1024

type payloadCapacityError struct {
	operation string
	bytes     int
}

func (e *payloadCapacityError) Error() string {
	if e.bytes < 0 {
		return "cannot encode " + e.operation + " for runtime execution"
	}
	return fmt.Sprintf("%s exceeds runtime content capacity (%d encoded bytes; limit %d)",
		e.operation, e.bytes, maxContentPayloadBytes)
}

// Measure the actual converter output, including JSON escaping and protobuf
// metadata. Semantic token estimates and raw HTTP lengths are different limits.
func checkContentPayload(operation string, value any) error {
	payload, err := converter.GetDefaultDataConverter().ToPayload(value)
	if err != nil {
		return &payloadCapacityError{operation: operation, bytes: -1}
	}
	size := proto.Size(payload)
	if size > maxContentPayloadBytes {
		return &payloadCapacityError{operation: operation, bytes: size}
	}
	return nil
}

// PrepareTurn returns either bounded immutable context or a terminal failure.
func (a *Activities) PrepareTurn(ctx context.Context, in PrepareTurnInput) (PrepareTurnResult, error) {
	result, err := a.prepareTurn(ctx, in)
	if err == nil {
		if capacity := checkContentPayload("prepared context", result); capacity != nil {
			// Child routing must survive even when no context can enter history.
			failure := PrepareTurnResult{ThreadID: result.ThreadID, IsChild: result.IsChild,
				FatalError: capacity.Error()}
			if result.Outcome != nil {
				// Admission already made this Outcome active. Keep only its
				// identity so failure can settle that durable projection.
				failure.PreludeEvents = []domain.EventDraft{outcomeEvaluationFailureDraft(
					domain.OutcomeSpec{OutcomeID: result.Outcome.OutcomeID}, 0, "", "",
					EvaluateOutcomeResult{FatalError: capacity.Error()},
				)}
			}
			return failure, nil
		}
	}
	return result, err
}

func boundedResponseAccounting(response model.Response) model.Response {
	response.Content = nil
	// These provider strings are not needed to preserve numerical usage. Do not
	// let an invalid provider value make the failure receipt oversized as well.
	if len(response.StopReason) > 128 {
		response.StopReason = ""
	}
	if len(response.Usage.Speed) > 128 {
		response.Usage.Speed = ""
	}
	if len(response.Usage.ProviderRegion) > 128 {
		response.Usage.ProviderRegion = ""
	}
	return response
}

// CallModel bounds the response without retrying completed inference.
func (a *Activities) CallModel(ctx context.Context, in CallModelInput) (CallModelResult, error) {
	result, err := a.callModel(ctx, in)
	if err == nil {
		if capacity := checkContentPayload("model response", result); capacity != nil {
			// Return a successful, small terminal result so completed inference is
			// not retried. The Workflow accounts this usage before terminating.
			return CallModelResult{Response: boundedResponseAccounting(result.Response),
				ModelRequestStartID: result.ModelRequestStartID, ModelRequestEndID: result.ModelRequestEndID,
				FatalError: capacity.Error(), FatalErrorType: "model_request_failed_error"}, nil
		}
	}
	return result, err
}

// EvaluateOutcome preserves usage when the grader result cannot be transported.
func (a *Activities) EvaluateOutcome(ctx context.Context, in EvaluateOutcomeInput) (EvaluateOutcomeResult, error) {
	result, err := a.evaluateOutcome(ctx, in)
	if err == nil {
		if capacity := checkContentPayload("outcome evaluation", result); capacity != nil {
			response := boundedResponseAccounting(model.Response{Usage: result.Usage, StopReason: result.StopReason})
			return EvaluateOutcomeResult{StartEventID: result.StartEventID, EndEventID: result.EndEventID,
				Usage: response.Usage, StopReason: response.StopReason, FatalError: capacity.Error()}, nil
		}
	}
	return result, err
}

// ExecuteTool bounds transport after preserving the durable tool journal.
func (a *Activities) ExecuteTool(ctx context.Context, in ExecuteToolInput) (ExecuteToolResult, error) {
	result, err := a.executeTool(ctx, in)
	if err == nil {
		if capacity := checkContentPayload("tool result", result); capacity != nil {
			// The durable journal (and Advisor usage, when applicable) is already
			// committed. Reject transport without executing the side effect again.
			return ExecuteToolResult{FatalError: capacity.Error()}, nil
		}
	}
	return result, err
}

func (t *workflowTurnState) commitTurn(input CompleteWorkflowTurnInput) (RunTurnResult, error) {
	if err := checkContentPayload("turn completion", input); err != nil {
		return t.terminateCapacity(err)
	}
	var result RunTurnResult
	err := workflow.ExecuteActivity(t.actx, ActivityCompleteWorkflowTurn, input).Get(t.actx, &result)
	return result, err
}

func (t *workflowTurnState) terminalOutcomeFailure(message string) domain.EventDraft {
	failure := outcomeEvaluationFailureDraft(domain.OutcomeSpec{OutcomeID: t.outcomeID},
		t.activeOutcomeIteration, t.activeOutcomeEvaluationStartID, "",
		EvaluateOutcomeResult{FatalError: message})
	// A completed grader's verdict may still be in the oversized completion
	// batch. Retain its small correlation/accounting fields, not its large
	// explanation or the candidate transcript.
	for i := len(t.output) - 1; i >= 0; i-- {
		draft := t.output[i]
		if draft.Type != domain.EvSpanOutcomeEvaluationEnd || draft.Payload["outcome_id"] != t.outcomeID {
			continue
		}
		failure.ID = draft.ID
		for _, key := range []string{"outcome_evaluation_start_id", "iteration"} {
			failure.Payload[key] = draft.Payload[key]
		}
		if usage, ok := draft.Payload["usage"].(map[string]any); ok {
			bounded := map[string]any{}
			for _, key := range []string{"cache_creation_input_tokens", "cache_read_input_tokens", "input_tokens", "output_tokens"} {
				bounded[key] = usage[key]
			}
			speed, _ := usage["speed"].(string)
			if len(speed) > 128 {
				speed = ""
			}
			bounded["speed"] = nullableSpeed(speed)
			failure.Payload["usage"] = bounded
		}
		break
	}
	return failure
}

// This path deliberately drops only uncommitted turn content. It never carries
// that oversized content into the error receipt. Already committed events,
// tool journal records and independently accounted usage remain authoritative.
func (t *workflowTurnState) terminateCapacity(capacity error) (RunTurnResult, error) {
	message := capacity.Error()
	output := []domain.EventDraft{}
	statusPayload := map[string]any{}
	if t.outcomeID != "" {
		failure := t.terminalOutcomeFailure(message)
		output = append(output, failure)
		// Completion's PostgreSQL transaction still decides whether an earlier
		// interrupt wins. It needs this correlation even when it removes the
		// competing failed verdict and emits its authoritative interrupted end.
		statusPayload[domain.InternalOutcomeEvaluationStart] = failure.Payload["outcome_evaluation_start_id"]
		statusPayload[domain.InternalOutcomeIteration] = failure.Payload["iteration"]
	}
	output = append(output,
		domain.EventDraft{Type: domain.EvSessionError, Payload: map[string]any{"error": map[string]any{
			"type": "unknown_error", "message": message,
			"retry_status": map[string]any{"type": "terminal"},
		}}},
		domain.EventDraft{Type: domain.EvSessionStatusTerminated, Payload: statusPayload},
	)
	input := CompleteWorkflowTurnInput{
		SessionID: t.sessionID, ThreadID: t.threadID, IsChild: t.isChild,
		TriggerEventID: t.triggerEventID, AttemptID: t.attemptID,
		ResolutionEventIDs: t.resolutionEventIDs, Status: domain.StatusTerminated,
		Usage: t.usage, UsageAlreadyAccounted: t.perRequestUsageAccounting,
		Output: output,
	}
	if t.attemptID != "" {
		input.AttemptState = domain.RunAttemptFailed
		input.AttemptError = &message
	}
	var result RunTurnResult
	err := workflow.ExecuteActivity(t.actx, ActivityCompleteWorkflowTurn, input).Get(t.actx, &result)
	return result, err
}
