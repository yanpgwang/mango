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
	Owner                 pg.WorkflowCoordinatorClaim
}

type WorkflowRunPumpResult struct {
	Cursor, Epoch, Version int64
	Waiting                bool
	Terminal               bool
}

type WorkflowRunStore interface {
	BindWorkflowCoordinator(context.Context, string, string, pg.WorkflowCoordinatorClaim) error
	ClaimWorkflowCoordinatorEvaluator(context.Context, string, string, string, int64, string) (pg.WorkflowEvaluatorClaim, error)
	LoadWorkflowPumpSnapshot(context.Context, string, string, pg.WorkflowPumpRequest) (pg.WorkflowPumpSnapshot, error)
	CommitWorkflowPump(context.Context, string, string, pg.WorkflowPumpRequest) (pg.WorkflowPumpDecision, error)
}

// WorkflowRunEvaluator is an unexposed integration boundary, not a qualified
// language/runtime ABI. No production implementation is shipped. nil command
// means host wait, including a persisted program-value checkpoint; it
// neither closes a Run nor publishes a terminal result.
type WorkflowRunEvaluator interface {
	Next(context.Context, pg.WorkflowPumpSnapshot) (*pg.WorkflowPumpCommand, error)
}

type WorkflowRunTerminalStore interface {
	RecoverWorkflowRunTerminal(context.Context, string, string, pg.WorkflowCoordinatorClaim) (*pg.WorkflowRunTerminal, error)
	FinalizeWorkflowRunCandidate(context.Context, string, string, pg.WorkflowPumpRequest) (pg.WorkflowRunTerminal, error)
}
type WorkflowRunActivities struct {
	// Explicit internal closure gate; public admission and default app stay off.
	FinalizeResults bool
	Store           WorkflowRunStore
	Evaluator       WorkflowRunEvaluator
}

func workflowPumpID(cursor int64) string { return fmt.Sprintf("pump/%d", cursor) }

func (a *WorkflowRunActivities) Pump(ctx context.Context, in WorkflowRunPumpInput) (WorkflowRunPumpResult, error) {
	var out WorkflowRunPumpResult
	if a.Store == nil || a.Evaluator == nil || in.Cursor < 0 || in.Cursor > 256 || in.PreviousEpoch < 0 {
		return out, temporal.NewNonRetryableApplicationError("internal Workflow pump is unavailable or invalid", "WorkflowPumpConfiguration", nil)
	}
	if !activity.IsActivity(ctx) || in.Owner.ExecutionID != activity.GetInfo(ctx).WorkflowExecution.RunID ||
		in.Owner.Cursor != in.Cursor || in.Owner.PreviousEpoch != in.PreviousEpoch {
		return out, workflowPumpError(domain.Conflict("untrusted Workflow coordinator identity"))
	}
	if a.FinalizeResults {
		terminalStore, ok := a.Store.(WorkflowRunTerminalStore)
		if !ok {
			return out, workflowPumpError(domain.Conflict("Workflow terminal store is unavailable"))
		}
		terminal, err := terminalStore.RecoverWorkflowRunTerminal(ctx, in.SessionID, in.RunID, in.Owner)
		if err != nil {
			return out, workflowPumpError(err)
		}
		if terminal != nil {
			return WorkflowRunPumpResult{Cursor: terminal.Cursor, Epoch: terminal.Epoch, Version: terminal.Version, Terminal: true}, nil
		}
	}
	if err := a.Store.BindWorkflowCoordinator(ctx, in.SessionID, in.RunID, in.Owner); err != nil {
		return out, workflowPumpError(err)
	}
	claim, err := a.Store.ClaimWorkflowCoordinatorEvaluator(ctx, in.SessionID, in.RunID, workflowPumpID(in.Cursor), in.PreviousEpoch, in.Owner.ExecutionID)
	if err != nil {
		return out, workflowPumpError(err)
	}
	request := pg.WorkflowPumpRequest{RequestID: workflowPumpID(in.Cursor), Epoch: claim.Epoch, ExpectedCursor: in.Cursor, CoordinatorExecutionID: in.Owner.ExecutionID}
	snapshot, err := a.Store.LoadWorkflowPumpSnapshot(ctx, in.SessionID, in.RunID, request)
	if err != nil {
		return out, workflowPumpError(err)
	}
	out = WorkflowRunPumpResult{Cursor: in.Cursor, Epoch: claim.Epoch, Version: snapshot.Version}
	if snapshot.Committed != nil {
		out.Cursor = snapshot.Committed.Cursor
		return out, nil
	}
	if a.FinalizeResults && len(snapshot.Journal) > 0 && snapshot.Journal[len(snapshot.Journal)-1].Decision.Type == "return" {
		terminalStore := a.Store.(WorkflowRunTerminalStore)
		terminal, err := terminalStore.FinalizeWorkflowRunCandidate(ctx, in.SessionID, in.RunID, request)
		if err != nil {
			return out, workflowPumpError(err)
		}
		return WorkflowRunPumpResult{Cursor: terminal.Cursor, Epoch: terminal.Epoch, Version: terminal.Version, Terminal: true}, nil
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
	info := workflow.GetInfo(ctx)
	// FirstRunID also spans Reset. OriginalRunID differs from the physical RunID
	// on Reset; such histories must never replay PG business decisions.
	if info.FirstRunID == "" || info.OriginalRunID != info.WorkflowExecution.RunID || info.Attempt != 1 || info.CronSchedule != "" {
		return temporal.NewNonRetryableApplicationError("Workflow coordinator history requires reconciliation", "WorkflowPumpFence", nil)
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
			Owner: pg.WorkflowCoordinatorClaim{ChainID: info.FirstRunID, ExecutionID: info.WorkflowExecution.RunID, PredecessorID: info.ContinuedExecutionRunID, Cursor: in.Cursor, PreviousEpoch: in.PreviousEpoch},
		}).Get(actx, &result); err != nil {
			return err
		}
		if result.Terminal {
			if result.Waiting || result.Cursor != in.Cursor || result.Epoch != in.PreviousEpoch+1 || result.Version < 1 {
				return errors.New("invalid Workflow terminal checkpoint")
			}
			return nil
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
