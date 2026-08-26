package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

const (
	defaultDeploymentPollInterval = time.Second
	defaultDeploymentClaimBatch   = 4
	defaultDeploymentClaimRenewal = ScheduleClaimLease / 3
)

type ScheduledDeploymentRunner interface {
	ClaimDue(context.Context, int) ([]DeploymentScheduleClaim, error)
	RenewClaim(context.Context, DeploymentScheduleClaim) error
	RunScheduled(context.Context, string, time.Time) (domain.DeploymentRun, error)
}

type DeploymentReconciler struct {
	runner        ScheduledDeploymentRunner
	pollInterval  time.Duration
	batchSize     int
	renewInterval time.Duration
}

func NewDeploymentReconciler(runner ScheduledDeploymentRunner) *DeploymentReconciler {
	return &DeploymentReconciler{
		runner: runner, pollInterval: defaultDeploymentPollInterval,
		batchSize: defaultDeploymentClaimBatch, renewInterval: defaultDeploymentClaimRenewal,
	}
}

// Run claims due cron occurrences through PostgreSQL and creates their
// Sessions. Token-fenced claims are renewed during admission and scheduled run
// rows are unique per occurrence, so multiple worker replicas can run this loop
// safely and a crashed worker is retried without exposing duplicate successful
// Sessions.
func (r *DeploymentReconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.pollInterval)
	defer ticker.Stop()
	for {
		if err := r.reconcile(ctx); err != nil && ctx.Err() == nil {
			// A failed scan must not terminate the worker role. The leased claim
			// becomes eligible again and the next tick also retries database
			// connectivity.
			log.Printf("deployments: scheduled run reconciliation failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *DeploymentReconciler) reconcile(ctx context.Context) error {
	claims, err := r.runner.ClaimDue(ctx, r.batchSize)
	if err != nil {
		return err
	}
	results := make(chan error, len(claims))
	for _, claim := range claims {
		claim := claim
		go func() {
			claimCtx := workspace.WithScope(ctx, claim.WorkspaceID)
			results <- r.runClaim(claimCtx, claim)
		}()
	}
	var combined error
	for range claims {
		combined = errors.Join(combined, <-results)
	}
	return combined
}

func (r *DeploymentReconciler) runClaim(
	ctx context.Context,
	claim DeploymentScheduleClaim,
) error {
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	heartbeatDone := make(chan error, 1)
	stopHeartbeat := make(chan struct{})
	go func() {
		ticker := time.NewTicker(r.renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				heartbeatDone <- nil
				return
			case <-stopHeartbeat:
				heartbeatDone <- nil
				return
			case <-ticker.C:
				if err := r.runner.RenewClaim(runCtx, claim); err != nil {
					cancelRun()
					heartbeatDone <- fmt.Errorf(
						"renew Deployment %s schedule claim: %w", claim.DeploymentID, err,
					)
					return
				}
			}
		}
	}()
	_, runErr := r.runner.RunScheduled(
		runCtx, claim.DeploymentID, claim.ScheduledAt,
	)
	close(stopHeartbeat)
	heartbeatErr := <-heartbeatDone
	return errors.Join(runErr, heartbeatErr)
}
