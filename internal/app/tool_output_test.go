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

func (r outputFileRepository) EnsureToolOutputUpload(ctx context.Context, file domain.File) (domain.File, error) {
	r.mu.Lock()
	existing, ok := r.files[file.ID]
	r.mu.Unlock()
	if ok {
		return existing, nil
	}
	if err := r.BeginUpload(ctx, file); err != nil {
		return domain.File{}, err
	}
	return file, nil
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
