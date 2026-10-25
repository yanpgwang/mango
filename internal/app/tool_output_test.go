package app

import (
	"context"
	"errors"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
	"io"
	"testing"
)

type outputFileRepository struct{ *memoryFileRepository }

func (r outputFileRepository) BeginToolOutputUpload(ctx context.Context, file domain.File) (domain.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.files[file.ID]
	if ok {
		if existing.SessionID != file.SessionID || existing.SizeBytes != file.SizeBytes || existing.ChecksumSHA256 != file.ChecksumSHA256 {
			return domain.File{}, domain.Conflict("receipt differs")
		}
		if existing.State == domain.FileStateReady {
			return existing, nil
		}
	}
	if _, err := r.RetainBlobCleanup(ctx, file.BlobKey, false); err != nil {
		return domain.File{}, err
	}
	r.files[file.ID] = file
	return file, nil
}

func (r outputFileRepository) CompleteToolOutputUpload(_ context.Context, expected domain.File) (domain.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	file, ok := r.files[expected.ID]
	if !ok || file.State != domain.FileStateUploading || file.BlobKey != expected.BlobKey {
		return domain.File{}, ErrUploadLeaseLost
	}
	file.State = domain.FileStateReady
	r.files[file.ID] = file
	r.memoryBlobCleanupRepository.mu.Lock()
	delete(r.intents, file.BlobKey)
	r.memoryBlobCleanupRepository.mu.Unlock()
	return file, r.completeErrAfterCommit
}

func (r outputFileRepository) ListBlobCleanup(ctx context.Context) ([]BlobCleanupIntent, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	intents, err := r.memoryBlobCleanupRepository.ListBlobCleanup(ctx)
	var eligible []BlobCleanupIntent
	for _, intent := range intents {
		current := false
		for _, file := range r.files {
			current = current || (file.BlobKey == intent.BlobKey && file.State != domain.FileStateDeleting)
		}
		if !current {
			eligible = append(eligible, intent)
		}
	}
	return eligible, err
}
func TestToolOutputStorage_ResumesAndPinsImmutableFile(t *testing.T) {
	ctx := workspace.WithScope(context.Background(), workspace.DefaultID)
	repo := outputFileRepository{newMemoryFileRepository()}
	blobs := newMemoryBlobStore()
	service := NewFileService(repo, blobs, domain.NewSeqIDGen(), domain.FixedClock{})
	blobs.putErr = errors.New("storage unavailable")
	if err := service.StoreToolOutput(ctx, "sesn_one", "file_result", "full tail"); err == nil {
		t.Fatal("unavailable storage was accepted")
	}
	blobs.putErr = nil
	for range 2 {
		if err := service.StoreToolOutput(ctx, "sesn_one", "file_result", "full tail"); err != nil {
			t.Fatal(err)
		}
	}
	if len(repo.files) != 1 {
		t.Fatal("duplicate Files")
	}
	if err := service.StoreToolOutput(ctx, "sesn_other", "file_result", "full tail"); err == nil {
		t.Fatal("ID reuse changed the owner")
	}
	if err := service.StoreToolOutput(ctx, "sesn_one", "file_result", "changed"); err == nil {
		t.Fatal("ID reuse changed content")
	}
	if _, err := service.Delete(ctx, "file_result"); err == nil {
		t.Fatal("Session-pinned File was deleted")
	}
	own := workspace.WithSessionScope(ctx, workspace.DefaultID, workspace.SessionScope{SessionID: "sesn_one"})
	download, err := service.Download(own, "file_result")
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(download.Body)
	_ = download.Body.Close()
	if string(data) != "full tail" {
		t.Fatal("full text lost")
	}
	other := workspace.WithSessionScope(ctx, workspace.DefaultID, workspace.SessionScope{SessionID: "sesn_other"})
	if _, err := service.Download(other, "file_result"); err == nil {
		t.Fatal("other Session accessed result")
	}
}
