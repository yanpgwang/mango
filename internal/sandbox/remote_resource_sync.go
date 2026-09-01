package sandbox

import (
	"context"
	"sync"
	"time"
)

const remoteResourceLockPollInterval = 10 * time.Millisecond

// remoteResourceSynchronization coordinates tools and resource snapshots that
// share one provider client. Cross-process ownership still depends on Mango's
// provider-aware task routing, which remains a deployment requirement for all
// remote sandbox adapters.
type remoteResourceSynchronization struct {
	mu sync.RWMutex
}

func (s *remoteResourceSynchronization) LockResourceOperation(
	ctx context.Context,
) (func(), error) {
	for {
		if s.mu.TryRLock() {
			return s.mu.RUnlock, nil
		}
		if err := waitForRemoteResourceLock(ctx); err != nil {
			return nil, err
		}
	}
}

func (s *remoteResourceSynchronization) TryLockResourceSync(
	ctx context.Context,
) (context.Context, func(), bool, error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, false, err
	}
	if !s.mu.TryLock() {
		return ctx, func() {}, false, nil
	}
	return resourceSyncContext(ctx), s.mu.Unlock, true, nil
}

func (s *remoteResourceSynchronization) LockResourceSync(
	ctx context.Context,
) (context.Context, func(), error) {
	for {
		if s.mu.TryLock() {
			return resourceSyncContext(ctx), s.mu.Unlock, nil
		}
		if err := waitForRemoteResourceLock(ctx); err != nil {
			return ctx, nil, err
		}
	}
}

func waitForRemoteResourceLock(ctx context.Context) error {
	timer := time.NewTimer(remoteResourceLockPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
