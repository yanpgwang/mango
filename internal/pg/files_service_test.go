package pg

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/blob"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	mango "github.com/yanpgwang/mango/sdk/go"
)

func TestFileService_PostgresS3RestartReconciliation(t *testing.T) {
	endpoint := os.Getenv("MANGO_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("MANGO_TEST_S3_ENDPOINT not set; skipping Files service conformance")
	}
	store := testStore(t)
	repo := NewFileRepository(store)
	blobs, err := blob.NewS3Store(context.Background(), blob.S3Config{
		Endpoint: endpoint, Region: "us-east-1",
		Bucket:       os.Getenv("MANGO_TEST_S3_BUCKET"),
		AccessKey:    os.Getenv("MANGO_TEST_S3_ACCESS_KEY"),
		SecretKey:    os.Getenv("MANGO_TEST_S3_SECRET_KEY"),
		UsePathStyle: true, CreateBucket: true, UploadTempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	ctx := context.Background()
	service := app.NewFileService(repo, blobs, domain.NewSeqIDGen(), fixedClock{})

	created, err := service.Upload(ctx, app.FileUploadInput{
		Filename: "input.txt", MimeType: "text/plain", Body: bytes.NewBufferString("input"),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if created.SizeBytes != 5 {
		t.Fatalf("created = %+v", created)
	}
	rubric, err := service.ReadOutcomeRubric(ctx, created.ID)
	if err != nil || rubric != "input" {
		t.Fatalf("ReadOutcomeRubric through PostgreSQL/S3 = %q, %v", rubric, err)
	}

	output, err := service.Upload(ctx, app.FileUploadInput{
		Filename: "output.txt", MimeType: "text/plain", Body: bytes.NewBufferString("output"),
	})
	if err != nil {
		t.Fatal(err)
	}
	download, err := service.Download(ctx, output.ID)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	data, readErr := io.ReadAll(download.Body)
	if closeErr := download.Body.Close(); readErr == nil {
		readErr = closeErr
	}
	if readErr != nil || string(data) != "output" {
		t.Fatalf("download = %q, %v", data, readErr)
	}

	pending := domain.File{
		ID: "file_pending_service", Filename: "pending.txt", MimeType: "text/plain",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		BlobKey: "files/file_pending_service", State: domain.FileStateUploading,
	}
	if err := repo.BeginUpload(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Put(ctx, pending.BlobKey, pending.MimeType,
		bytes.NewBufferString("orphan"), app.MaxFileBytes); err != nil {
		t.Fatal(err)
	}

	deleting, err := repo.BeginDelete(ctx, created.ID)
	if err != nil || deleting.State != domain.FileStateDeleting {
		t.Fatalf("BeginDelete = %+v, %v", deleting, err)
	}
	restarted := app.NewFileService(repo, blobs, domain.NewSeqIDGen(), fixedClock{})
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, file := range []domain.File{pending, deleting} {
		if _, err := blobs.Open(ctx, file.BlobKey); err == nil {
			t.Errorf("incomplete blob %s remains", file.BlobKey)
		}
	}
	if incomplete, err := repo.ListIncomplete(ctx); err != nil || len(incomplete) != 0 {
		t.Fatalf("incomplete rows = %+v, %v", incomplete, err)
	}

	_, _ = service.Delete(ctx, output.ID)
}

func TestFileHTTP_PostgresS3SDKLifecycle(t *testing.T) {
	endpoint := os.Getenv("MANGO_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("MANGO_TEST_S3_ENDPOINT not set; skipping Files HTTP service conformance")
	}
	store := testStore(t)
	blobs, err := blob.NewS3Store(context.Background(), blob.S3Config{
		Endpoint: endpoint, Region: "us-east-1",
		Bucket:       os.Getenv("MANGO_TEST_S3_BUCKET"),
		AccessKey:    os.Getenv("MANGO_TEST_S3_ACCESS_KEY"),
		SecretKey:    os.Getenv("MANGO_TEST_S3_SECRET_KEY"),
		UsePathStyle: true, CreateBucket: true, UploadTempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	repo := NewFileRepository(store)
	ids := domain.NewSeqIDGen()
	service := app.NewFileService(repo, blobs, ids, fixedClock{})
	server := httptest.NewServer(httpapi.NewServer(httpapi.Deps{Files: service}, httpapi.Config{
		RequireAuth: true,
	}).Handler())
	defer server.Close()
	client, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	uploaded, err := client.Files.Upload(ctx, mango.FileUploadRequest{
		File: mango.Upload{Filename: "service.txt", ContentType: "text/plain", Reader: bytes.NewReader([]byte("service"))},
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if uploaded.SizeBytes != 7 || uploaded.Filename != "service.txt" {
		t.Fatalf("uploaded = %+v", uploaded)
	}
	metadata, err := client.Files.Get(ctx, uploaded.ID)
	if err != nil || metadata.ID != uploaded.ID {
		t.Fatalf("GetMetadata = %+v, %v", metadata, err)
	}
	page, err := client.Files.List(ctx, mango.ListFilesParams{})
	if err != nil || len(page.Data) != 1 || page.Data[0].ID != uploaded.ID {
		t.Fatalf("List = %+v, %v", page, err)
	}
	download, err := client.Files.Download(ctx, uploaded.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(download)
	_ = download.Close()
	if err != nil || string(data) != "service" {
		t.Fatalf("download = %q, %v", data, err)
	}
	deleted, err := client.Files.Delete(ctx, uploaded.ID)
	if err != nil || deleted.ID != uploaded.ID {
		t.Fatalf("Delete = %+v, %v", deleted, err)
	}
	if _, err := client.Files.Get(ctx, uploaded.ID); err == nil {
		t.Fatal("deleted File remains visible")
	}
}

func TestFileService_PostgresS3ConcurrentLifecycle(t *testing.T) {
	endpoint := os.Getenv("MANGO_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("MANGO_TEST_S3_ENDPOINT not set; skipping concurrent Files conformance")
	}
	store := testStore(t)
	blobs, err := blob.NewS3Store(context.Background(), blob.S3Config{
		Endpoint: endpoint, Region: "us-east-1",
		Bucket:       os.Getenv("MANGO_TEST_S3_BUCKET"),
		AccessKey:    os.Getenv("MANGO_TEST_S3_ACCESS_KEY"),
		SecretKey:    os.Getenv("MANGO_TEST_S3_SECRET_KEY"),
		UsePathStyle: true, CreateBucket: true, UploadTempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewS3Store: %v", err)
	}
	service := app.NewFileService(
		NewFileRepository(store), blobs, domain.NewSeqIDGen(), fixedClock{},
	)
	ctx := context.Background()
	const count = 8
	files := make(chan domain.File, count)
	errs := make(chan error, count)
	var uploads sync.WaitGroup
	for index := 0; index < count; index++ {
		uploads.Add(1)
		go func(index int) {
			defer uploads.Done()
			created, uploadErr := service.Upload(ctx, app.FileUploadInput{
				Filename: fmt.Sprintf("concurrent-%d.bin", index),
				MimeType: "application/octet-stream",
				Body:     bytes.NewReader(bytes.Repeat([]byte{byte('a' + index)}, 32<<10)),
			})
			if uploadErr != nil {
				errs <- uploadErr
				return
			}
			files <- created
		}(index)
	}
	uploads.Wait()
	close(files)
	close(errs)
	for uploadErr := range errs {
		t.Errorf("concurrent Upload: %v", uploadErr)
	}
	created := make([]domain.File, 0, count)
	for file := range files {
		created = append(created, file)
	}
	if len(created) != count {
		t.Fatalf("created %d Files, want %d", len(created), count)
	}
	t.Cleanup(func() {
		for _, file := range created {
			_, _ = service.Delete(context.Background(), file.ID)
		}
	})
	page, err := service.List(ctx, app.FileListQuery{Limit: 100})
	if err != nil || len(page.Files) != count {
		t.Fatalf("List after concurrent upload = %d Files, %v", len(page.Files), err)
	}

	errs = make(chan error, count)
	var deletes sync.WaitGroup
	for _, file := range created {
		deletes.Add(1)
		go func(file domain.File) {
			defer deletes.Done()
			if _, deleteErr := service.Delete(ctx, file.ID); deleteErr != nil {
				errs <- deleteErr
			}
		}(file)
	}
	deletes.Wait()
	close(errs)
	for deleteErr := range errs {
		t.Errorf("concurrent Delete: %v", deleteErr)
	}
	page, err = service.List(ctx, app.FileListQuery{Limit: 100})
	if err != nil || len(page.Files) != 0 {
		t.Fatalf("List after concurrent delete = %d Files, %v", len(page.Files), err)
	}
}

type serviceNamedReader struct {
	*bytes.Reader
}

func (*serviceNamedReader) Name() string        { return "service.txt" }
func (*serviceNamedReader) ContentType() string { return "text/plain" }
