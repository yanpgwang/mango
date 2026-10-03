package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type memoryBlobCleanupRepository struct {
	mu       sync.Mutex
	intents  map[string]BlobCleanupIntent
	finished map[string]bool
	revision int64
}

func (r *memoryBlobCleanupRepository) RetainBlobCleanup(_ context.Context, key string, writeFinished bool) (BlobCleanupIntent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.intents == nil {
		r.intents = map[string]BlobCleanupIntent{}
		r.finished = map[string]bool{}
	}
	intent := r.intents[key]
	intent.BlobKey = key
	r.revision++
	intent.Revision = r.revision
	r.finished[key] = r.finished[key] || writeFinished
	r.intents[key] = intent
	return intent, nil
}

func (r *memoryBlobCleanupRepository) ListBlobCleanup(context.Context) ([]BlobCleanupIntent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []BlobCleanupIntent
	for _, intent := range r.intents {
		result = append(result, intent)
	}
	return result, nil
}

func (r *memoryBlobCleanupRepository) RemoveBlobCleanup(_ context.Context, intent BlobCleanupIntent) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.finished[intent.BlobKey] && r.intents[intent.BlobKey].Revision == intent.Revision {
		delete(r.intents, intent.BlobKey)
	}
	return nil
}

func (*memoryBlobCleanupRepository) TouchBlobCleanup(context.Context, BlobCleanupIntent) error {
	return nil
}

func TestBlobCleanupMetadataTimeoutDoesNotStarveGuardQueue(t *testing.T) {
	repo := &memoryBlobCleanupRepository{}
	blobs := newMemoryBlobStore()
	blobs.objects["files/late"] = []byte("late")
	if _, err := repo.RetainBlobCleanup(context.Background(), "files/late", true); err != nil {
		t.Fatal(err)
	}
	err := reconcileBlobOperations(context.Background(), 10*time.Millisecond, repo, blobs, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("metadata error lost: %v", err)
	}
	if len(blobs.objects) != 0 {
		t.Fatal("timed-out metadata prevented guard cleanup")
	}
	if intents, err := repo.ListBlobCleanup(context.Background()); err != nil || len(intents) != 0 {
		t.Fatalf("guard not acknowledged: %+v %v", intents, err)
	}
}
