package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUploadLeaseRenewalFailureCancelsWriterAndReleasesIntent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	released := false
	owned, stop := startUploadLease(ctx, time.Millisecond,
		func(context.Context) error { return ErrUploadLeaseLost },
		func(cleanup context.Context) error {
			if cleanup.Err() != nil {
				t.Error("release inherited canceled I/O context")
			}
			released = true
			return nil
		})
	<-owned.Done()
	if !errors.Is(context.Cause(owned), ErrUploadLeaseLost) {
		t.Fatal(context.Cause(owned))
	}
	stop()
	if !released {
		t.Fatal("pending intent not released")
	}
}
