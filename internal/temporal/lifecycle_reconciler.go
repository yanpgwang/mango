package temporal

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"
)

const lifecycleDrainDelay = 100 * time.Millisecond

// DeletionStore is the worker-side view of fenced Session deletion state.
// PostgreSQL remains authoritative; the reconciler only resumes the external
// cleanup and finalization that a crashed API process may have left unfinished.
type DeletionStore interface {
	ListDeletingSessionIDs(ctx context.Context, limit int) ([]string, error)
	FinalizeSessionMemoryResources(ctx context.Context, sessionID string) error
	FinalizeSessionDeletion(ctx context.Context, sessionID string) error
}

type SessionTerminator interface {
	TerminateSession(ctx context.Context, sessionID string) error
}

type LifecycleReconcilerConfig struct {
	// PollInterval is the idle delay between scans. A scan that completes work
	// immediately repeats so a backlog drains without one interval per batch.
	PollInterval time.Duration
	// BatchSize bounds deleting-session scans.
	BatchSize int
	// AttemptTimeout prevents one unavailable provider from starving the rest of
	// the batch. The deterministic cleanup Workflow continues after this local
	// wait expires and a later scan joins it.
	AttemptTimeout time.Duration
}

func (c LifecycleReconcilerConfig) withDefaults() LifecycleReconcilerConfig {
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 100
	}
	if c.AttemptTimeout <= 0 {
		c.AttemptTimeout = 30 * time.Second
	}
	return c
}

// LifecycleReconcileResult reports successfully discharged durable obligations.
type LifecycleReconcileResult struct {
	Deletions int
}

func (r LifecycleReconcileResult) total() int {
	return r.Deletions
}

// LifecycleReconciler closes the process-crash window between a durable
// Session deletion fence and Workflow termination/finalization.
type LifecycleReconciler struct {
	store      DeletionStore
	terminator SessionTerminator
	cfg        LifecycleReconcilerConfig
}

func NewLifecycleReconciler(
	store DeletionStore,
	terminator SessionTerminator,
	cfg LifecycleReconcilerConfig,
) *LifecycleReconciler {
	return &LifecycleReconciler{
		store:      store,
		terminator: terminator,
		cfg:        cfg.withDefaults(),
	}
}

func (r *LifecycleReconciler) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}

		result, err := r.RunOnce(ctx)
		if err != nil && ctx.Err() == nil {
			log.Printf("lifecycle reconciler: cycle error: %v", err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		wait := r.nextDelay(result)
		timer.Reset(wait)
	}
}

func (r *LifecycleReconciler) nextDelay(result LifecycleReconcileResult) time.Duration {
	wait := r.cfg.PollInterval
	if result.total() > 0 && wait > lifecycleDrainDelay {
		wait = lifecycleDrainDelay
	}
	return wait
}

func (r *LifecycleReconciler) RunOnce(
	ctx context.Context,
) (LifecycleReconcileResult, error) {
	var (
		result LifecycleReconcileResult
		errs   []error
	)

	if r.store == nil || r.terminator == nil {
		return result, errors.Join(errs...)
	}
	sessionIDs, err := r.store.ListDeletingSessionIDs(ctx, r.cfg.BatchSize)
	if err != nil {
		errs = append(errs, fmt.Errorf("list deleting sessions: %w", err))
		return result, errors.Join(errs...)
	}
	for _, sessionID := range sessionIDs {
		if ctx.Err() != nil {
			break
		}
		attemptCtx, cancel := context.WithTimeout(ctx, r.cfg.AttemptTimeout)
		err := r.terminator.TerminateSession(attemptCtx, sessionID)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"resume cleanup for session %s: %w",
				sessionID,
				err,
			))
			continue
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(ctx, r.cfg.AttemptTimeout)
		err = r.store.FinalizeSessionMemoryResources(cleanupCtx, sessionID)
		cleanupCancel()
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"finalize Memory resources for session %s: %w", sessionID, err,
			))
			continue
		}

		finalizeCtx, cancel := context.WithTimeout(ctx, r.cfg.AttemptTimeout)
		err = r.store.FinalizeSessionDeletion(finalizeCtx, sessionID)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"finalize deletion for session %s: %w",
				sessionID,
				err,
			))
			continue
		}
		result.Deletions++
	}
	return result, errors.Join(errs...)
}
