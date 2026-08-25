package app

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestDeploymentReconcilerContinuesAfterOneClaimFails(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("first occurrence failed")
	runner := &scheduledDeploymentRunnerFake{
		claims: []DeploymentScheduleClaim{
			{DeploymentID: "depl_first", ScheduledAt: time.Unix(1, 0)},
			{DeploymentID: "depl_second", ScheduledAt: time.Unix(2, 0)},
			{DeploymentID: "depl_third", ScheduledAt: time.Unix(3, 0)},
		},
		failures: map[string]error{"depl_first": wantErr},
	}

	err := NewDeploymentReconciler(runner).reconcile(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("reconcile error = %v, want %v", err, wantErr)
	}
	got := runner.recordedRuns()
	sort.Strings(got)
	want := []string{"depl_first", "depl_second", "depl_third"}
	if !equalStrings(got, want) {
		t.Fatalf("scheduled runs = %v, want %v", got, want)
	}
}

func TestDeploymentReconcilerRunsClaimBatchConcurrently(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	started := make(chan string, 3)
	runner := &scheduledDeploymentRunnerFake{
		claims: []DeploymentScheduleClaim{
			{DeploymentID: "depl_first", ScheduledAt: time.Unix(1, 0)},
			{DeploymentID: "depl_second", ScheduledAt: time.Unix(2, 0)},
			{DeploymentID: "depl_third", ScheduledAt: time.Unix(3, 0)},
		},
		started: started, release: release,
	}
	reconciler := NewDeploymentReconciler(runner)
	reconciler.renewInterval = time.Hour
	done := make(chan error, 1)
	go func() { done <- reconciler.reconcile(context.Background()) }()

	seen := map[string]bool{}
	for range 3 {
		select {
		case id := <-started:
			seen[id] = true
		case <-time.After(time.Second):
			t.Fatalf("only started concurrent claims: %v", seen)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDeploymentReconcilerRenewsLongRunningClaim(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	runner := &scheduledDeploymentRunnerFake{
		claims: []DeploymentScheduleClaim{{
			DeploymentID: "depl_slow", ScheduledAt: time.Unix(1, 0), Token: "dclaim_slow",
		}},
		started: make(chan string, 1), release: release,
		renewed: make(chan struct{}, 1),
	}
	reconciler := NewDeploymentReconciler(runner)
	reconciler.renewInterval = 5 * time.Millisecond
	done := make(chan error, 1)
	go func() { done <- reconciler.reconcile(context.Background()) }()
	<-runner.started
	select {
	case <-runner.renewed:
	case <-time.After(time.Second):
		t.Fatal("long-running claim was not renewed")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestDeploymentReconcilerCancelsRunAfterClaimOwnershipIsLost(t *testing.T) {
	t.Parallel()
	wantErr := errors.New("claim token changed")
	runner := &scheduledDeploymentRunnerFake{
		claims: []DeploymentScheduleClaim{{
			DeploymentID: "depl_lost", ScheduledAt: time.Unix(1, 0), Token: "dclaim_old",
		}},
		started: make(chan string, 1), release: make(chan struct{}), renewErr: wantErr,
	}
	reconciler := NewDeploymentReconciler(runner)
	reconciler.renewInterval = 5 * time.Millisecond
	err := reconciler.reconcile(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("lost claim reconcile error = %v", err)
	}
}

type scheduledDeploymentRunnerFake struct {
	mu       sync.Mutex
	claims   []DeploymentScheduleClaim
	failures map[string]error
	runs     []string
	started  chan string
	release  chan struct{}
	renewed  chan struct{}
	renewErr error
}

func (f *scheduledDeploymentRunnerFake) ClaimDue(
	context.Context,
	int,
) ([]DeploymentScheduleClaim, error) {
	return f.claims, nil
}

func (f *scheduledDeploymentRunnerFake) RenewClaim(
	context.Context,
	DeploymentScheduleClaim,
) error {
	if f.renewed != nil {
		select {
		case f.renewed <- struct{}{}:
		default:
		}
	}
	return f.renewErr
}

func (f *scheduledDeploymentRunnerFake) RunScheduled(
	ctx context.Context,
	id string,
	_ time.Time,
) (domain.DeploymentRun, error) {
	f.mu.Lock()
	f.runs = append(f.runs, id)
	err := f.failures[id]
	f.mu.Unlock()
	if f.started != nil {
		f.started <- id
	}
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return domain.DeploymentRun{}, ctx.Err()
		}
	}
	return domain.DeploymentRun{}, err
}

func (f *scheduledDeploymentRunnerFake) recordedRuns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.runs...)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
