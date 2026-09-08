package pg

import (
	"context"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestFileRepository_LifecycleAndBidirectionalPaging(t *testing.T) {
	store := testStore(t)
	repo := NewFileRepository(store)
	ctx := context.Background()
	base := time.Date(2026, 8, 4, 0, 0, 0, 0, time.UTC)

	created := make([]domain.File, 0, 5)
	for index := 1; index <= 5; index++ {
		file := domain.File{
			ID:        "file_" + itoa(int64(index)),
			CreatedAt: base.Add(time.Duration(index) * time.Second),
			UpdatedAt: base.Add(time.Duration(index) * time.Second),
			Filename:  "file.txt", MimeType: "text/plain",
			BlobKey: "files/file_" + itoa(int64(index)), State: domain.FileStateUploading,
		}
		if index == 3 {
			file.Scope = &domain.FileScope{ID: "sesn_scope", Type: "session"}
			file.Downloadable = true
		}
		if err := repo.BeginUpload(ctx, file); err != nil {
			t.Fatalf("BeginUpload %d: %v", index, err)
		}
		ready, err := repo.CompleteUpload(ctx, file.ID, app.BlobInfo{
			SizeBytes: int64(index), ChecksumSHA256: "sum",
		})
		if err != nil {
			t.Fatalf("CompleteUpload %d: %v", index, err)
		}
		created = append(created, ready)
	}

	first, err := repo.List(ctx, app.FileListQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertFileIDs(t, first.Files, "file_5", "file_4")
	if !first.HasMore {
		t.Fatal("first page missing has_more")
	}
	second, err := repo.List(ctx, app.FileListQuery{AfterID: "file_4", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertFileIDs(t, second.Files, "file_3", "file_2")
	if !second.HasMore {
		t.Fatal("second page missing has_more")
	}
	last, err := repo.List(ctx, app.FileListQuery{AfterID: "file_2", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertFileIDs(t, last.Files, "file_1")
	if last.HasMore {
		t.Fatal("last page has_more = true")
	}

	previous, err := repo.List(ctx, app.FileListQuery{BeforeID: "file_2", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertFileIDs(t, previous.Files, "file_4", "file_3")
	if !previous.HasMore {
		t.Fatal("backward page missing has_more")
	}
	newest, err := repo.List(ctx, app.FileListQuery{BeforeID: "file_4", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	assertFileIDs(t, newest.Files, "file_5")
	if newest.HasMore {
		t.Fatal("newest backward page has_more = true")
	}

	scoped, err := repo.List(ctx, app.FileListQuery{ScopeID: "sesn_scope", Limit: 20})
	if err != nil {
		t.Fatalf("scope filter = %+v, %v", scoped, err)
	}
	assertFileIDs(t, scoped.Files, "file_3")
	missingScope, err := repo.List(ctx, app.FileListQuery{ScopeID: "sesn_missing", Limit: 20})
	if err != nil || len(missingScope.Files) != 0 {
		t.Fatalf("missing scope filter = %+v, %v", missingScope, err)
	}
	if _, err := repo.List(ctx, app.FileListQuery{AfterID: "file_missing", Limit: 2}); err == nil {
		t.Fatal("missing cursor accepted")
	}

	deleting, err := repo.BeginDelete(ctx, created[2].ID)
	if err != nil || deleting.State != domain.FileStateDeleting {
		t.Fatalf("BeginDelete = %+v, %v", deleting, err)
	}
	if _, err := repo.Get(ctx, deleting.ID); err == nil {
		t.Fatal("deleting file remains visible")
	}
	incomplete, err := repo.ListIncomplete(ctx)
	if err != nil || len(incomplete) != 1 || incomplete[0].ID != deleting.ID {
		t.Fatalf("incomplete = %+v, %v", incomplete, err)
	}
	if err := repo.RemoveIncomplete(ctx, deleting.ID); err != nil {
		t.Fatal(err)
	}
	if incomplete, err = repo.ListIncomplete(ctx); err != nil || len(incomplete) != 0 {
		t.Fatalf("incomplete after remove = %+v, %v", incomplete, err)
	}
}

func assertFileIDs(t *testing.T, files []domain.File, expected ...string) {
	t.Helper()
	if len(files) != len(expected) {
		t.Fatalf("file count = %d, want %d: %+v", len(files), len(expected), files)
	}
	for index, id := range expected {
		if files[index].ID != id {
			t.Fatalf("file[%d] = %s, want %s", index, files[index].ID, id)
		}
	}
}
