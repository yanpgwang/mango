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
	WorkflowRunWakeups(context.Context) ([]pg.WorkflowRunWakeup, error)
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
	wakes, err := r.store.WorkflowRunWakeups(ctx)
	if err != nil {
		return 0, err
	}
	removed := 0
	var failures []error
	for _, wake := range wakes[:min(len(wakes), r.cfg.BatchSize)] {
		if err := r.deliverer.WakeWorkflowRun(ctx, wake); err != nil {
			failures = append(failures, err)
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
		// Failed deliveries leave their durable rows. The caller can also drive
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
	_, err := s.client.SignalWithStartWorkflow(ctx, id, WorkflowRunWakeSignal, wake, client.StartWorkflowOptions{
		ID: id, TaskQueue: s.taskQueue,
		// Rejects closed IDs only while Temporal retains their history. This is
		// not a durable PG chain-generation/reset fence; expired-history recovery
		// remains a gate before this private relay can be enabled in the product.
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
	}, WorkflowRunWorkflowType, WorkflowRunWorkflowInput{SessionID: wake.SessionID, RunID: wake.RunID})
	return err
}
