package temporal

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/yanpgwang/mango/internal/pg"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"
)

type WorkflowRunOutboxStore interface {
	ClaimWorkflowRunWakeups(context.Context, int) ([]pg.WorkflowRunWakeup, error)
	PrepareWorkflowRunStart(context.Context, pg.WorkflowRunWakeup) (bool, error)
	DeferWorkflowRunWakeup(context.Context, pg.WorkflowRunWakeup) error
	DeleteWorkflowRunWakeupIfUnchanged(context.Context, pg.WorkflowRunWakeup) (bool, error)
}

type WorkflowRunWakeupDeliverer interface {
	WakeWorkflowRun(context.Context, pg.WorkflowRunWakeup) error
}

// WorkflowRunRelay is opt-in and drains the existing versioned Run outbox,
// never ordinary primary child reports. No application startup wires it yet.
type WorkflowRunRelay struct {
	store     WorkflowRunOutboxStore
	deliverer WorkflowRunWakeupDeliverer
	cfg       RelayConfig
}

func NewWorkflowRunRelay(store WorkflowRunOutboxStore, deliverer WorkflowRunWakeupDeliverer, cfg RelayConfig) *WorkflowRunRelay {
	return &WorkflowRunRelay{store: store, deliverer: deliverer, cfg: cfg.withDefaults()}
}

func (r *WorkflowRunRelay) RunOnce(ctx context.Context) (int, error) {
	removed := 0
	var failures []error
	for range r.cfg.BatchSize {
		wakes, err := r.store.ClaimWorkflowRunWakeups(ctx, 1)
		if err != nil {
			return removed, errors.Join(append(failures, err)...)
		}
		if len(wakes) == 0 {
			break
		}
		wake := wakes[0]
		wake.StartAllowed, err = r.store.PrepareWorkflowRunStart(ctx, wake)
		if err != nil {
			failures = append(failures, err, r.store.DeferWorkflowRunWakeup(ctx, wake))
			continue
		}
		deliveryCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = r.deliverer.WakeWorkflowRun(deliveryCtx, wake)
		cancel()
		if err != nil {
			failures = append(failures, err, r.store.DeferWorkflowRunWakeup(ctx, wake))
			continue
		}
		deleted, err := r.store.DeleteWorkflowRunWakeupIfUnchanged(ctx, wake)
		if err != nil {
			failures = append(failures, err)
		} else if deleted {
			removed++
		}
	}
	return removed, errors.Join(failures...)
}

func (r *WorkflowRunRelay) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.cfg.PollInterval)
	defer ticker.Stop()
	for {
		// Failed deliveries defer their durable rows with bounded retries. The caller can also drive
		// RunOnce directly to observe errors; this loop retries until canceled.
		if _, err := r.RunOnce(ctx); err != nil && ctx.Err() == nil {
			// No raw transport/database error text: it is not a public log API.
			log.Print("workflow_run_relay: delivery deferred; retrying")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func workflowRunWorkflowID(runID string) string { return "mango.workflow-run." + runID }

func (s *Signaler) WakeWorkflowRun(ctx context.Context, wake pg.WorkflowRunWakeup) error {
	id := workflowRunWorkflowID(wake.RunID)
	hint := pg.WorkflowRunWakeup{SessionID: wake.SessionID, RunID: wake.RunID, Version: wake.Version}
	if !wake.StartAllowed {
		return s.client.SignalWorkflow(ctx, id, "", WorkflowRunWakeSignal, hint)
	}
	_, err := s.client.SignalWithStartWorkflow(ctx, id, WorkflowRunWakeSignal, hint, client.StartWorkflowOptions{
		ID: id, TaskQueue: s.taskQueue,
		// Retained history rejects a closed start. The PG binding independently
		// fences a fresh chain after history expires, before evaluator access.
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}, WorkflowRunWorkflowType, WorkflowRunWorkflowInput{SessionID: wake.SessionID, RunID: wake.RunID})
	return err
}
