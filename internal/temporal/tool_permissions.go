package temporal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/permission"
)

const (
	ActivityEnsureToolPermissionAttempt = "EnsureToolPermissionAttempt"
	ActivityEvaluateToolPermission      = "EvaluateToolPermission"
)

// PermissionSource keeps durable judgment facts separate from current action
// authority. PostgreSQL implements both checks without holding locks over I/O.
type PermissionSource interface {
	EnsureToolPermissionAttempt(context.Context, domain.ToolPermissionOwner) error
	PermissionIntentThrough(context.Context, string, string) (domain.PermissionIntent, error)
	GetToolPermissionReceipt(context.Context, domain.ToolPermissionReceipt) (*domain.ToolPermissionReceipt, error)
	AdmitToolPermissionEvaluation(context.Context, domain.ToolPermissionReceipt) (bool, error)
	RecordToolPermissionReceipt(context.Context, domain.ToolPermissionReceipt) (domain.ToolPermissionReceipt, error)
	StartToolStepWithPermission(context.Context, string, string) error
}

type EnsureToolPermissionAttemptResult struct {
	Established bool `json:"established"`
}
type EvaluateToolPermissionInput struct {
	Owner          domain.ToolPermissionOwner `json:"owner"`
	ToolUseEventID string                     `json:"tool_use_event_id"`
	Model          domain.Model               `json:"model"`
	Input          permission.Input           `json:"input"`
}
type EvaluateToolPermissionResult struct {
	Decision     domain.ToolPermissionDecision `json:"decision"`
	BudgetPaused bool                          `json:"budget_paused"`
	Stopped      bool                          `json:"stopped"`
}

func (a *Activities) WithPermissionEvaluator(evaluator permission.Evaluator) *Activities {
	a.permissionEvaluator = evaluator
	return a
}

func (a *Activities) EnsureToolPermissionAttempt(ctx context.Context, owner domain.ToolPermissionOwner) (EnsureToolPermissionAttemptResult, error) {
	source, ok := a.source.(PermissionSource)
	if !ok {
		return EnsureToolPermissionAttemptResult{}, fmt.Errorf("temporal: automatic permissions require durable permission storage")
	}
	err := source.EnsureToolPermissionAttempt(ctx, owner)
	if permissionOwnerStopped(err) {
		return EnsureToolPermissionAttemptResult{}, nil
	}
	return EnsureToolPermissionAttemptResult{Established: err == nil}, err
}

func permissionOwnerStopped(err error) bool {
	var domainErr *domain.DomainError
	return errors.As(err, &domainErr) && (domainErr.Kind == domain.KindConflict || domainErr.Kind == domain.KindNotFound)
}

// EvaluateToolPermission bills only a committed response, including a response
// received after cancellation. The immutable decision is never execution authority.
func (a *Activities) EvaluateToolPermission(ctx context.Context, in EvaluateToolPermissionInput) (EvaluateToolPermissionResult, error) {
	source, ok := a.source.(PermissionSource)
	if !ok {
		return EvaluateToolPermissionResult{}, fmt.Errorf("temporal: automatic permissions require durable permission storage")
	}
	stopHeartbeat := heartbeatActivity(ctx)
	defer stopHeartbeat()
	invocationHash, err := permission.FingerprintInvocation(in.Input.Call.ToolName, in.Input.Call.Input)
	if err != nil {
		return EvaluateToolPermissionResult{}, err
	}
	contextHash, err := permission.FingerprintInput(in.Input)
	if err != nil {
		return EvaluateToolPermissionResult{}, err
	}
	candidate := domain.ToolPermissionReceipt{ToolPermissionOwner: in.Owner, ToolUseEventID: in.ToolUseEventID, ToolName: in.Input.Call.ToolName, InvocationHash: invocationHash, ContextHash: contextHash, Model: in.Model}
	admitted, err := source.AdmitToolPermissionEvaluation(ctx, candidate)
	if permissionOwnerStopped(err) {
		return EvaluateToolPermissionResult{Stopped: true}, nil
	}
	if err != nil {
		return EvaluateToolPermissionResult{}, err
	}
	if !admitted {
		return EvaluateToolPermissionResult{BudgetPaused: true}, nil
	}
	cached, err := source.GetToolPermissionReceipt(ctx, candidate)
	if err != nil {
		return EvaluateToolPermissionResult{}, err
	}
	if cached != nil {
		return EvaluateToolPermissionResult{Decision: cached.Decision}, nil
	}
	result := permission.Result{Decision: domain.ToolPermissionDecision{Type: "ask", ReasonCode: "indeterminate"}}
	if a.permissionEvaluator != nil {
		result, err = a.permissionEvaluator.Evaluate(ctx, in.Model, in.Input)
	}
	if !result.Decision.Valid() || (err != nil && ctx.Err() == nil) {
		result.Decision = domain.ToolPermissionDecision{Type: "ask", ReasonCode: "indeterminate"}
	}
	if ctx.Err() != nil && !result.ResponseReceived {
		return EvaluateToolPermissionResult{}, ctx.Err()
	}
	candidate.Decision = result.Decision
	candidate.Usage = result.Usage
	candidate.StopReason = result.StopReason
	candidate.ResponseReceived = result.ResponseReceived
	dctx, cancel := durableCtx(ctx)
	defer cancel()
	var winner domain.ToolPermissionReceipt
	var writeErr error
	for attempt := 0; attempt < 3; attempt++ {
		winner, writeErr = source.RecordToolPermissionReceipt(dctx, candidate)
		if writeErr == nil {
			break
		}
		var domainErr *domain.DomainError
		if errors.As(writeErr, &domainErr) || attempt == 2 {
			break
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 100 * time.Millisecond)
		select {
		case <-timer.C:
		case <-dctx.Done():
			timer.Stop()
			return EvaluateToolPermissionResult{}, writeErr
		}
	}
	if writeErr != nil {
		return EvaluateToolPermissionResult{}, writeErr
	}
	if ctx.Err() != nil {
		return EvaluateToolPermissionResult{}, ctx.Err()
	}
	return EvaluateToolPermissionResult{Decision: winner.Decision}, nil
}
