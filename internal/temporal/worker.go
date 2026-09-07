package temporal

import (
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// NewWorker builds a Temporal worker that serves the SessionWorkflow and its
// Activities on the session task queue. The caller runs it (worker.Run) and owns
// the client's lifecycle.
//
// Activities are registered under stable names so a Go method rename cannot
// silently break workflow replay.
func NewWorker(c client.Client, acts *Activities) worker.Worker {
	return NewWorkerOnTaskQueue(c, acts, TaskQueue)
}

func NewWorkerOnTaskQueue(
	c client.Client,
	acts *Activities,
	taskQueue string,
) worker.Worker {
	if taskQueue == "" {
		taskQueue = TaskQueue
	}
	w := worker.New(c, taskQueue, worker.Options{})
	w.RegisterWorkflow(SessionWorkflow)
	w.RegisterWorkflowWithOptions(
		SessionThreadWorkflow,
		workflow.RegisterOptions{Name: SessionThreadWorkflowType},
	)
	w.RegisterActivityWithOptions(acts.LoadEvents, activity.RegisterOptions{Name: ActivityLoadEvents})
	w.RegisterActivityWithOptions(acts.LoadInterrupt, activity.RegisterOptions{Name: ActivityLoadInterrupt})
	w.RegisterActivityWithOptions(acts.LoadPendingActions, activity.RegisterOptions{Name: ActivityLoadPendingActions})
	w.RegisterActivityWithOptions(acts.PrepareTurn, activity.RegisterOptions{Name: ActivityPrepareTurn})
	w.RegisterActivityWithOptions(acts.AdmitModelRequest, activity.RegisterOptions{Name: ActivityAdmitModelRequest})
	w.RegisterActivityWithOptions(acts.StartModelRequest, activity.RegisterOptions{Name: ActivityStartModelRequest})
	w.RegisterActivityWithOptions(acts.AppendWorkflowEvents, activity.RegisterOptions{Name: ActivityAppendWorkflowEvents})
	w.RegisterActivityWithOptions(acts.RecordModelRetry, activity.RegisterOptions{Name: ActivityRecordModelRetry})
	w.RegisterActivityWithOptions(acts.ResumeModelRetry, activity.RegisterOptions{Name: ActivityResumeModelRetry})
	w.RegisterActivityWithOptions(acts.CallModel, activity.RegisterOptions{Name: ActivityCallModel})
	w.RegisterActivityWithOptions(acts.AccountModelRequest, activity.RegisterOptions{Name: ActivityAccountModelRequest})
	w.RegisterActivityWithOptions(acts.EvaluateOutcome, activity.RegisterOptions{Name: ActivityEvaluateOutcome})
	w.RegisterActivityWithOptions(acts.ExecuteTool, activity.RegisterOptions{Name: ActivityExecuteTool})
	w.RegisterActivityWithOptions(acts.CompleteWorkflowTurn, activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn})
	return w
}
