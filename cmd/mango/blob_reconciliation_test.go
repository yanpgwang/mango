package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestBlobReconciliationRetriesAndStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var calls atomic.Int32
	finished := make(chan struct{}, 1)
	stop := startBlobReconciliation(ctx, time.Millisecond, func(context.Context) error {
		if calls.Add(1) == 1 {
			return errors.New("temporary storage outage")
		}
		select {
		case finished <- struct{}{}:
		default:
		}
		return nil
	}, nil)
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("reconciliation did not retry")
	}
	stop()
	if calls.Load() < 2 {
		t.Fatal("failed scan not retried")
	}
}
