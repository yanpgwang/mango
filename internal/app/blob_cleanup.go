package app

import (
	"context"
	"errors"
	"time"
)

// BlobCleanupIntent survives removal or reuse of the upload's metadata row.
// Revision fences an older cleanup acknowledgement from a newer late write.
type BlobCleanupIntent struct {
	BlobKey  string
	Revision int64
}

type BlobCleanupRepository interface {
	RetainBlobCleanup(context.Context, string, bool) (BlobCleanupIntent, error)
	TouchBlobCleanup(context.Context, BlobCleanupIntent) error
	ListBlobCleanup(context.Context) ([]BlobCleanupIntent, error)
	RemoveBlobCleanup(context.Context, BlobCleanupIntent) error
}

func cleanupBlob(ctx context.Context, repo BlobCleanupRepository, blobs BlobStore, key string, writeFinished bool) error {
	intent, err := repo.RetainBlobCleanup(ctx, key, writeFinished)
	if err != nil {
		return err
	}
	if err := blobs.Delete(ctx, key); err != nil {
		return err
	}
	return repo.RemoveBlobCleanup(ctx, intent)
}

func reconcileBlobCleanup(ctx context.Context, repo BlobCleanupRepository, blobs BlobStore) error {
	intents, err := repo.ListBlobCleanup(ctx)
	if err != nil {
		return err
	}
	var failures error
	for _, intent := range intents {
		if err := repo.TouchBlobCleanup(ctx, intent); err != nil {
			return errors.Join(failures, err)
		}
		if err := blobs.Delete(ctx, intent.BlobKey); err != nil {
			failures = errors.Join(failures, err)
			if ctx.Err() != nil {
				return failures
			}
			continue
		}
		if err := repo.RemoveBlobCleanup(ctx, intent); err != nil {
			failures = errors.Join(failures, err)
		}
	}
	return failures
}

// Separate budgets ensure a failing metadata candidate cannot consume every
// periodic scan before independent late-write guards are reached.
func reconcileBlobOperations(ctx context.Context, budget time.Duration, repo BlobCleanupRepository, blobs BlobStore, metadata func(context.Context) error) error {
	request, cancel := context.WithTimeout(ctx, budget)
	metadataErr := metadata(request)
	cancel()
	request, cancel = context.WithTimeout(ctx, budget)
	defer cancel()
	return errors.Join(metadataErr, reconcileBlobCleanup(request, repo, blobs))
}
