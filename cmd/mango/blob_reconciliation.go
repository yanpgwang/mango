package main

import (
	"context"
	"log"
	"time"
)

// A restart before a crashed writer's lease expires must skip that intent.
// Periodic reconciliation ensures it is eventually collected without another
// process restart, and retries storage outages without disabling the API.
func startBlobReconciliation(ctx context.Context, interval time.Duration, files, skills func(context.Context) error) func() {
	owned, cancel := context.WithCancel(ctx)
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
				for _, item := range []struct {
					name      string
					reconcile func(context.Context) error
				}{{"Files", files}, {"Skills", skills}} {
					if item.reconcile == nil {
						continue
					}
					request, stop := context.WithTimeout(owned, 10*time.Second)
					err := item.reconcile(request)
					stop()
					if err != nil && owned.Err() == nil {
						log.Printf("serve: %s reconciliation will retry: %v", item.name, err)
					}
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}
