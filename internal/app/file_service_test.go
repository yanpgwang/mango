package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestFileService_UploadDeleteAndReconcile(t *testing.T) {
	repo := newMemoryFileRepository()
	blobs := newMemoryBlobStore()
	service := NewFileService(repo, blobs, domain.NewSeqIDGen(), domain.FixedClock{
		T: time.Date(2026, 8, 4, 1, 2, 3, 0, time.UTC),
	})

	created, err := service.Upload(context.Background(), FileUploadInput{
		Filename: "report.txt", MimeType: "text/plain; charset=utf-8",
		Body: bytes.NewBufferString("hello"),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if created.ID != "file_1" || created.SizeBytes != 5 || created.MimeType != "text/plain" {
		t.Fatalf("created = %+v", created)
	}
	if created.State != domain.FileStateReady {
		t.Fatalf("uploaded state = %+v", created)
	}
	download, err := service.Download(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(download.Body)
	_ = download.Body.Close()
	if err != nil || string(data) != "hello" {
		t.Fatalf("download = %q, %v", data, err)
	}

	if _, err := service.Delete(context.Background(), created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := service.Get(context.Background(), created.ID); err == nil {
		t.Fatal("deleted file remains visible")
	}
	if _, present := blobs.objects[created.BlobKey]; present {
		t.Fatal("deleted file bytes remain")
	}

	pending := domain.File{
		ID: "file_pending", Filename: "pending.txt", MimeType: "text/plain",
		BlobKey: "files/file_pending", State: domain.FileStateUploading,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := repo.BeginUpload(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	blobs.objects[pending.BlobKey] = []byte("orphan")
	if err := service.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, present := repo.files[pending.ID]; present {
		t.Fatal("pending metadata remains after reconciliation")
	}
	if _, present := blobs.objects[pending.BlobKey]; present {
		t.Fatal("pending blob remains after reconciliation")
	}
}

func TestFileService_ValidatesUploadAndMapsStreamingLimit(t *testing.T) {
	tests := []FileUploadInput{
		{Filename: "", MimeType: "text/plain", Body: bytes.NewReader(nil)},
		{Filename: "../secret", MimeType: "text/plain", Body: bytes.NewReader(nil)},
		{Filename: "ok.txt", MimeType: "not a mime", Body: bytes.NewReader(nil)},
		{Filename: "ok.txt", MimeType: "text/plain"},
	}
	for _, input := range tests {
		service := NewFileService(newMemoryFileRepository(), newMemoryBlobStore(),
			domain.NewSeqIDGen(), domain.FixedClock{})
		if _, err := service.Upload(context.Background(), input); err == nil {
			t.Errorf("Upload(%+v) succeeded", input)
		}
	}

	repo := newMemoryFileRepository()
	blobs := newMemoryBlobStore()
	blobs.putErr = ErrBlobTooLarge
	service := NewFileService(repo, blobs, domain.NewSeqIDGen(), domain.FixedClock{})
	_, err := service.Upload(context.Background(), FileUploadInput{
		Filename: "large.bin", MimeType: "application/octet-stream", Body: bytes.NewReader(nil),
	})
	var domainErr *domain.DomainError
	if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindTooLarge {
		t.Fatalf("oversize error = %v", err)
	}
	if len(repo.files) != 0 {
		t.Fatalf("failed upload left rows: %+v", repo.files)
	}
}

func TestFileService_ReadOutcomeRubricValidatesBoundedTextAndIntegrity(t *testing.T) {
	newFixture := func(t *testing.T, data []byte) (*FileService, *memoryFileRepository, *memoryBlobStore, domain.File) {
		t.Helper()
		repo := newMemoryFileRepository()
		blobs := newMemoryBlobStore()
		service := NewFileService(repo, blobs, domain.NewSeqIDGen(), domain.FixedClock{})
		file, err := service.Upload(context.Background(), FileUploadInput{
			Filename: "rubric.md", MimeType: "application/octet-stream",
			Body: bytes.NewReader(data),
		})
		if err != nil {
			t.Fatalf("upload rubric: %v", err)
		}
		return service, repo, blobs, file
	}

	valid := "# Rubric\n- cites evidence"
	service, _, _, file := newFixture(t, []byte(valid))
	got, err := service.ReadOutcomeRubric(context.Background(), file.ID)
	if err != nil || got != valid {
		t.Fatalf("ReadOutcomeRubric = %q, %v", got, err)
	}
	maxRunes := strings.Repeat("界", domain.MaxOutcomeRubricCharacters)
	service, _, _, file = newFixture(t, []byte(maxRunes))
	if got, err := service.ReadOutcomeRubric(context.Background(), file.ID); err != nil || got != maxRunes {
		t.Fatalf("exact character limit: len=%d err=%v", len(got), err)
	}
	if _, err := service.ReadOutcomeRubric(context.Background(), "file_missing"); err == nil {
		t.Fatal("missing rubric File was accepted")
	}
	service, repo, _, file := newFixture(t, []byte(valid))
	if _, err := repo.BeginDelete(context.Background(), file.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReadOutcomeRubric(context.Background(), file.ID); err == nil {
		t.Fatal("deleting rubric File was accepted")
	}

	tests := []struct {
		name   string
		data   []byte
		mutate func(*memoryFileRepository, *memoryBlobStore, domain.File)
		want   string
	}{
		{name: "empty", data: nil, want: "must not be empty"},
		{name: "invalid UTF-8", data: []byte{0xff}, want: "valid UTF-8"},
		{
			name: "too many characters",
			data: []byte(strings.Repeat("x", domain.MaxOutcomeRubricCharacters+1)),
			want: "at most 262144 characters",
		},
		{
			name: "metadata precheck", data: []byte("valid"),
			mutate: func(repo *memoryFileRepository, _ *memoryBlobStore, file domain.File) {
				repo.mu.Lock()
				stored := repo.files[file.ID]
				stored.SizeBytes = maxOutcomeRubricBytes + 1
				repo.files[file.ID] = stored
				repo.mu.Unlock()
			},
			want: "at most 262144 characters",
		},
		{
			name: "integrity mismatch", data: []byte("valid"),
			mutate: func(_ *memoryFileRepository, blobs *memoryBlobStore, file domain.File) {
				blobs.objects[file.BlobKey] = []byte("other")
			},
			want: "integrity verification",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, repo, blobs, file := newFixture(t, test.data)
			if test.mutate != nil {
				test.mutate(repo, blobs, file)
			}
			_, err := service.ReadOutcomeRubric(context.Background(), file.ID)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ReadOutcomeRubric error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestFileService_ReadMessageFileValidatesTextScopeBoundsAndIntegrity(t *testing.T) {
	newFixture := func(
		t *testing.T,
		data []byte,
		mediaType string,
	) (*FileService, *memoryFileRepository, *memoryBlobStore, domain.File) {
		t.Helper()
		repo := newMemoryFileRepository()
		blobs := newMemoryBlobStore()
		service := NewFileService(repo, blobs, domain.NewSeqIDGen(), domain.FixedClock{})
		file, err := service.Upload(context.Background(), FileUploadInput{
			Filename: "notes.md", MimeType: mediaType, Body: bytes.NewReader(data),
		})
		if err != nil {
			t.Fatalf("upload message File: %v", err)
		}
		return service, repo, blobs, file
	}

	const valid = "# Notes\n\n你好 from the uploaded file"
	for _, mediaType := range []string{"text/markdown", "application/octet-stream"} {
		service, _, _, file := newFixture(t, []byte(valid), mediaType)
		got, err := service.ReadMessageFile(context.Background(), file.ID)
		if err != nil || got.FileID != file.ID || got.Filename != "notes.md" ||
			got.MimeType != mediaType || got.Content != valid {
			t.Fatalf("ReadMessageFile(%s) = %+v, %v", mediaType, got, err)
		}
	}
	service := NewFileService(
		newMemoryFileRepository(), newMemoryBlobStore(),
		domain.NewSeqIDGen(), domain.FixedClock{},
	)
	genericPDF, err := service.Upload(context.Background(), FileUploadInput{
		Filename: "scan.pdf", MimeType: "application/octet-stream",
		Body: bytes.NewBufferString("%PDF-1.4"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReadMessageFile(context.Background(), genericPDF.ID); err == nil ||
		!strings.Contains(err.Error(), "UTF-8 text files only") {
		t.Fatalf("generic PDF error = %v", err)
	}

	tests := []struct {
		name      string
		data      []byte
		mediaType string
		mutate    func(*memoryFileRepository, *memoryBlobStore, domain.File)
		want      string
	}{
		{name: "empty", mediaType: "text/plain", want: "must not be empty"},
		{
			name: "invalid UTF-8", data: []byte{0xff}, mediaType: "text/plain",
			want: "UTF-8 text files only",
		},
		{
			name: "NUL byte", data: []byte("text\x00binary"), mediaType: "text/plain",
			want: "UTF-8 text files only",
		},
		{
			name: "PDF media type", data: []byte("%PDF-1.4"), mediaType: "application/pdf",
			want: "UTF-8 text files only",
		},
		{
			name:      "too many characters",
			data:      []byte(strings.Repeat("x", domain.MaxFileMessageCharacters+1)),
			mediaType: "text/plain", want: "at most 262144 characters",
		},
		{
			name: "integrity mismatch", data: []byte("valid"), mediaType: "text/plain",
			mutate: func(_ *memoryFileRepository, blobs *memoryBlobStore, file domain.File) {
				blobs.objects[file.BlobKey] = []byte("other")
			},
			want: "integrity verification",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, repo, blobs, file := newFixture(t, test.data, test.mediaType)
			if test.mutate != nil {
				test.mutate(repo, blobs, file)
			}
			_, err := service.ReadMessageFile(context.Background(), file.ID)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("ReadMessageFile error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestFileService_PreservesBlobWhenCompletionResultIsAmbiguous(t *testing.T) {
	repo := newMemoryFileRepository()
	repo.completeErrAfterCommit = errors.New("database connection lost after commit")
	blobs := newMemoryBlobStore()
	service := NewFileService(repo, blobs, domain.NewSeqIDGen(), domain.FixedClock{})

	_, err := service.Upload(context.Background(), FileUploadInput{
		Filename: "committed.txt", MimeType: "text/plain", Body: bytes.NewBufferString("safe"),
	})
	if !errors.Is(err, repo.completeErrAfterCommit) {
		t.Fatalf("Upload error = %v", err)
	}
	file, err := repo.Get(context.Background(), "file_1")
	if err != nil || file.State != domain.FileStateReady {
		t.Fatalf("committed File = %+v, %v", file, err)
	}
	if got := string(blobs.objects[file.BlobKey]); got != "safe" {
		t.Fatalf("committed blob = %q, want safe", got)
	}
}

func TestFileService_CleanupOutlivesCanceledRequest(t *testing.T) {
	repo := newMemoryFileRepository()
	repo.rejectCanceledCleanup = true
	blobs := newMemoryBlobStore()
	blobs.rejectCanceledCleanup = true
	service := NewFileService(repo, blobs, domain.NewSeqIDGen(), domain.FixedClock{})
	pending := domain.File{
		ID: "file_pending_cancel", Filename: "pending.txt", MimeType: "text/plain",
		BlobKey: "files/file_pending_cancel", State: domain.FileStateUploading,
	}
	if err := repo.BeginUpload(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	blobs.objects[pending.BlobKey] = []byte("partial")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	service.cleanupIncomplete(ctx, pending)
	if len(repo.files) != 0 || len(blobs.objects) != 0 {
		t.Fatalf("cleanup left row or blob: files=%+v objects=%+v", repo.files, blobs.objects)
	}
}

type memoryFileRepository struct {
	mu                     sync.Mutex
	files                  map[string]domain.File
	completeErrAfterCommit error
	rejectCanceledCleanup  bool
}

func newMemoryFileRepository() *memoryFileRepository {
	return &memoryFileRepository{files: make(map[string]domain.File)}
}

func (r *memoryFileRepository) BeginUpload(_ context.Context, file domain.File) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.files[file.ID] = file
	return nil
}

func (r *memoryFileRepository) CompleteUpload(
	_ context.Context,
	id string,
	info BlobInfo,
) (domain.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	file, present := r.files[id]
	if !present || file.State != domain.FileStateUploading {
		return domain.File{}, domain.Conflict("not pending")
	}
	file.SizeBytes = info.SizeBytes
	file.ChecksumSHA256 = info.ChecksumSHA256
	file.State = domain.FileStateReady
	r.files[id] = file
	if r.completeErrAfterCommit != nil {
		return domain.File{}, r.completeErrAfterCommit
	}
	return file, nil
}

func (r *memoryFileRepository) Get(_ context.Context, id string) (domain.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	file, present := r.files[id]
	if !present || file.State != domain.FileStateReady {
		return domain.File{}, domain.NotFound("file not found")
	}
	return file, nil
}

func (r *memoryFileRepository) List(_ context.Context, query FileListQuery) (FileListPage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	files := make([]domain.File, 0, len(r.files))
	for _, file := range r.files {
		if file.State == domain.FileStateReady {
			files = append(files, file)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].CreatedAt.Equal(files[j].CreatedAt) {
			return files[i].ID > files[j].ID
		}
		return files[i].CreatedAt.After(files[j].CreatedAt)
	})
	if len(files) > query.Limit {
		return FileListPage{Files: files[:query.Limit], HasMore: true}, nil
	}
	return FileListPage{Files: files}, nil
}

func (r *memoryFileRepository) BeginDelete(_ context.Context, id string) (domain.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	file, present := r.files[id]
	if !present || file.State != domain.FileStateReady {
		return domain.File{}, domain.NotFound("file not found")
	}
	file.State = domain.FileStateDeleting
	r.files[id] = file
	return file, nil
}

func (r *memoryFileRepository) RemoveIncomplete(ctx context.Context, id string) error {
	if r.rejectCanceledCleanup && ctx.Err() != nil {
		return ctx.Err()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if file, present := r.files[id]; present && file.State != domain.FileStateReady {
		delete(r.files, id)
	}
	return nil
}

func (r *memoryFileRepository) ListIncomplete(context.Context) ([]domain.File, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	files := make([]domain.File, 0)
	for _, file := range r.files {
		if file.State != domain.FileStateReady {
			files = append(files, file)
		}
	}
	return files, nil
}

type memoryBlobStore struct {
	objects               map[string][]byte
	putCalls              int
	putErr                error
	rejectCanceledCleanup bool
}

func newMemoryBlobStore() *memoryBlobStore {
	return &memoryBlobStore{objects: map[string][]byte{}}
}

func (s *memoryBlobStore) Put(
	_ context.Context,
	key string,
	_ string,
	body io.Reader,
	maxBytes int64,
) (BlobInfo, error) {
	s.putCalls++
	if s.putErr != nil {
		return BlobInfo{}, s.putErr
	}
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return BlobInfo{}, err
	}
	if int64(len(data)) > maxBytes {
		return BlobInfo{}, ErrBlobTooLarge
	}
	s.objects[key] = data
	return ComputeBlobInfo(data), nil
}

func (s *memoryBlobStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	data, present := s.objects[key]
	if !present {
		return nil, errors.New("missing blob")
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (s *memoryBlobStore) Delete(ctx context.Context, key string) error {
	if s.rejectCanceledCleanup && ctx.Err() != nil {
		return ctx.Err()
	}
	delete(s.objects, key)
	return nil
}
