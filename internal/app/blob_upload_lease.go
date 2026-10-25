package app

import (
	"context"
	"errors"
	"time"
)

// UploadLeaseDuration is measured by PostgreSQL, not an API process's clock.
// Each public upload has a unique immutable resource identity and one writer.
const UploadLeaseDuration = time.Minute

// ErrUploadLeaseLost is a definite ownership failure, distinct from an
// uncertain completion response that may already have committed.
var ErrUploadLeaseLost = errors.New("upload ownership was lost")

// startUploadLease renews ownership while blob I/O is in progress. A renewal
// failure cancels I/O; completion also checks ownership atomically in SQL.
// Release only makes a still-pending intent eligible for later cleanup.
func startUploadLease(ctx context.Context, interval time.Duration, renew, release func(context.Context) error) (context.Context, func()) {
	owned, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-owned.Done():
				return
			case <-ticker.C:
				request, stop := context.WithTimeout(owned, 3*time.Second)
				err := renew(request)
				stop()
				if err != nil {
					cancel(err)
					return
				}
			}
		}
	}()
	return owned, func() {
		cancel(nil)
		<-done
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer stop()
		_ = release(cleanup) // A failed release expires naturally.
	}
}
