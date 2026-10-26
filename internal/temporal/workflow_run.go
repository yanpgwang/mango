package temporal

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

const (
	WorkflowRunWorkflowType = "WorkflowRunWorkflow"
	WorkflowRunWakeSignal   = "workflow_run_wakeup"
	ActivityWorkflowRunPump = "WorkflowRunPump"
)

type WorkflowRunWorkflowInput struct {
	SessionID, RunID                   string
	Cursor, PreviousEpoch, HintVersion int64
}

type WorkflowRunPumpInput struct {
	SessionID, RunID      string
	Cursor, PreviousEpoch int64
}

type WorkflowRunPumpResult struct {
	Cursor, Epoch, Version int64
	Waiting                bool
}

type WorkflowRunStore interface {
	ClaimWorkflowEvaluator(context.Context, string, string, string, int64) (pg.WorkflowEvaluatorClaim, error)
	LoadWorkflowPumpSnapshot(context.Context, string, string, pg.WorkflowPumpRequest) (pg.WorkflowPumpSnapshot, error)
	CommitWorkflowPump(context.Context, string, string, pg.WorkflowPumpRequest) (pg.WorkflowPumpDecision, error)
}

// WorkflowRunEvaluator is an unexposed integration boundary, not a qualified
// language/runtime ABI. No production implementation is shipped. nil command
// means host wait, including the test double's quiescent final checkpoint; it
// neither closes a Run nor publishes a terminal result.
type WorkflowRunEvaluator interface {
	Next(context.Context, pg.WorkflowPumpSnapshot) (*pg.WorkflowPumpCommand, error)
}

type WorkflowRunActivities struct {
	Store     WorkflowRunStore
	Evaluator WorkflowRunEvaluator
}

func workflowPumpID(cursor int64) string { return fmt.Sprintf("pump/%d", cursor) }

func (a *WorkflowRunActivities) Pump(ctx context.Context, in WorkflowRunPumpInput) (WorkflowRunPumpResult, error) {
	var out WorkflowRunPumpResult
	if a.Store == nil || a.Evaluator == nil || in.Cursor < 0 || in.Cursor > 256 || in.PreviousEpoch < 0 {
		return out, temporal.NewNonRetryableApplicationError("internal Workflow pump is unavailable or invalid", "WorkflowPumpConfiguration", nil)
	}
	claim, err := a.Store.ClaimWorkflowEvaluator(ctx, in.SessionID, in.RunID, workflowPumpID(in.Cursor), in.PreviousEpoch)
	if err != nil {
		return out, workflowPumpError(err)
	}
	request := pg.WorkflowPumpRequest{RequestID: workflowPumpID(in.Cursor), Epoch: claim.Epoch, ExpectedCursor: in.Cursor}
	snapshot, err := a.Store.LoadWorkflowPumpSnapshot(ctx, in.SessionID, in.RunID, request)
	if err != nil {
		return out, workflowPumpError(err)
	}
	out = WorkflowRunPumpResult{Cursor: in.Cursor, Epoch: claim.Epoch, Version: snapshot.Version}
	if snapshot.Committed != nil {
		out.Cursor = snapshot.Committed.Cursor
		return out, nil
	}
	command, err := a.Evaluator.Next(ctx, snapshot)
	if err != nil {
		return out, workflowPumpError(err)
	}
	if command == nil {
		out.Waiting = true
		return out, nil
	}
	request.Command = *command
	decision, err := a.Store.CommitWorkflowPump(ctx, in.SessionID, in.RunID, request)
	if err != nil {
		return out, workflowPumpError(err)
	}
	out.Cursor = decision.Cursor
	return out, nil
}

// Infrastructure failures retry the same pump identity. Ownership/control and
// protocol errors fail closed; lifecycle parking/resume is a separate gate.
func workflowPumpError(err error) error {
	if errors.Is(err, context.Canceled) {
		return temporal.NewCanceledError()
	}
	var domainErr *domain.DomainError
	if errors.As(err, &domainErr) {
		return temporal.NewNonRetryableApplicationError("Workflow pump requires ownership/control reconciliation", "WorkflowPumpFence", nil)
	}
	var applicationErr *temporal.ApplicationError
	if errors.As(err, &applicationErr) && applicationErr.NonRetryable() {
		return temporal.NewNonRetryableApplicationError("Workflow evaluator requires reconciliation", "WorkflowPumpEvaluator", nil)
	}
	// Error messages, nested causes and details can include source or receipts.
	// Preserve retry classification without copying any of them into history.
	return temporal.NewApplicationError("Workflow pump infrastructure unavailable", "WorkflowPumpRetry")
}

// RegisterWorkflowRunCoordinator is opt-in infrastructure registration. The
// default worker/application and ordinary relay do not register or start it.
func RegisterWorkflowRunCoordinator(w worker.Worker, acts *WorkflowRunActivities) {
	w.RegisterWorkflowWithOptions(WorkflowRunWorkflow, workflow.RegisterOptions{Name: WorkflowRunWorkflowType})
	w.RegisterActivityWithOptions(acts.Pump, activity.RegisterOptions{Name: ActivityWorkflowRunPump})
}

func WorkflowRunWorkflow(ctx workflow.Context, in WorkflowRunWorkflowInput) error {
	return workflowRunWorkflow(ctx, in, continueAsNewThreshold, time.Minute)
}

func workflowRunWorkflow(ctx workflow.Context, in WorkflowRunWorkflowInput, commandThreshold int, reconcileAfter time.Duration) error {
	if in.SessionID == "" || in.RunID == "" || in.Cursor < 0 || in.Cursor > 256 || in.PreviousEpoch < 0 || in.HintVersion < 0 || commandThreshold < 1 || reconcileAfter <= 0 {
		return errors.New("invalid internal Workflow coordinator input")
	}
	actx := workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: time.Minute,
		RetryPolicy:         &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: time.Minute},
	})
	wakes := workflow.GetSignalChannel(ctx, WorkflowRunWakeSignal)
	drain := func() {
		var wake pg.WorkflowRunWakeup
		for wakes.ReceiveAsync(&wake) {
			if wake.SessionID == in.SessionID && wake.RunID == in.RunID {
				in.HintVersion = max(in.HintVersion, wake.Version)
			}
		}
	}
	cycles, commands := 0, 0
	for {
		var result WorkflowRunPumpResult
		if err := workflow.ExecuteActivity(actx, ActivityWorkflowRunPump, WorkflowRunPumpInput{
			SessionID: in.SessionID, RunID: in.RunID, Cursor: in.Cursor, PreviousEpoch: in.PreviousEpoch,
		}).Get(actx, &result); err != nil {
			return err
		}
		cycles++
		if result.Epoch != in.PreviousEpoch+1 || result.Version < 1 ||
			(result.Waiting && result.Cursor != in.Cursor) || (!result.Waiting && result.Cursor != in.Cursor+1) {
			return errors.New("workflow pump returned an invalid checkpoint")
		}
		if !result.Waiting {
			in.Cursor, in.PreviousEpoch = result.Cursor, result.Epoch
			commands++
		}
		drain()
		// A signal accepted while the Activity was finishing wins before wait.
		// A new history always re-reads PG before waiting, even if a hint vanished.
		if commands >= commandThreshold || cycles >= continueAsNewThreshold || workflow.GetInfo(ctx).GetContinueAsNewSuggested() {
			drain()
			return workflow.NewContinueAsNewError(ctx, WorkflowRunWorkflowType, in)
		}
		if !result.Waiting || in.HintVersion > result.Version {
			continue
		}
		waitctx, cancel := workflow.WithCancel(ctx)
		selector := workflow.NewSelector(ctx)
		selector.AddFuture(workflow.NewTimer(waitctx, reconcileAfter), func(workflow.Future) {})
		selector.AddReceive(wakes, func(channel workflow.ReceiveChannel, _ bool) {
			var wake pg.WorkflowRunWakeup
			channel.Receive(ctx, &wake)
			if wake.SessionID == in.SessionID && wake.RunID == in.RunID {
				in.HintVersion = max(in.HintVersion, wake.Version)
			}
		})
		selector.Select(ctx)
		cancel()
		drain()
	}
}
