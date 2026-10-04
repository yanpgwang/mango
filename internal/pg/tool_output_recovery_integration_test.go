package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/blob"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

// The gateway accepts the first real S3 request but reports an unknown outcome.
// That accepted request reaches SeaweedFS only after a later publication or
// deletion. No callback from the disappeared writer performs cleanup.
func TestToolOutput_PostgresS3UnknownWriteSurvivesPublicationAndDeletion(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		t.Run(fmt.Sprintf("deleted=%v", deleted), func(t *testing.T) {
			first, second, objects := uploadRecoveryServices(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			ctx = workspace.WithScope(ctx, workspace.DefaultID)
			if _, err := first.CreateSession(ctx, newSession("sesn_unknown_output"), nil); err != nil {
				t.Fatal(err)
			}
			upstream, err := url.Parse(os.Getenv("MANGO_TEST_S3_ENDPOINT"))
			if err != nil {
				t.Fatal(err)
			}
			proxy := httputil.NewSingleHostReverseProxy(upstream)
			var writes atomic.Int32
			accepted := make(chan *http.Request, 1)
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPut || writes.Add(1) != 1 {
					proxy.ServeHTTP(w, r)
					return
				}
				body, readErr := io.ReadAll(r.Body)
				if readErr != nil {
					http.Error(w, "read accepted upload", http.StatusInternalServerError)
					return
				}
				request := r.Clone(ctx)
				request.URL.Scheme, request.URL.Host = upstream.Scheme, upstream.Host
				request.RequestURI = ""
				request.Body = io.NopCloser(bytes.NewReader(body))
				accepted <- request
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = io.WriteString(w, "<Error><Code>SlowDown</Code><Message>accepted write outcome unknown</Message></Error>")
			}))
			defer gateway.Close()
			uncertain, err := blob.NewS3Store(ctx, blob.S3Config{
				Endpoint: gateway.URL, Region: "us-east-1", Bucket: os.Getenv("MANGO_TEST_S3_BUCKET"),
				AccessKey: os.Getenv("MANGO_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("MANGO_TEST_S3_SECRET_KEY"),
				UsePathStyle: true, UploadTempDir: t.TempDir(),
			})
			if err != nil {
				t.Fatal(err)
			}
			id := domain.NewRandomIDGen().NewID(domain.PrefixFile)
			text := strings.Repeat("complete output\n", 8000) + "tail-proof"
			writer := app.NewFileService(NewFileRepository(first), uncertain, domain.NewRandomIDGen(), fixedClock{})
			if err := writer.StoreToolOutput(ctx, "sesn_unknown_output", id, text); err == nil {
				t.Fatal("unknown write was accepted as ready")
			}
			var late *http.Request
			select {
			case late = <-accepted:
			case <-ctx.Done():
				t.Fatal("S3 write did not reach gateway")
			}
			key := strings.TrimPrefix(late.URL.Path, "/"+os.Getenv("MANGO_TEST_S3_BUCKET")+"/")
			t.Cleanup(func() { _ = objects.Delete(context.Background(), key) })
			// A replacement process publishes only the already durable text.
			service := app.NewFileService(NewFileRepository(second), objects, domain.NewRandomIDGen(), fixedClock{})
			if err := service.StoreToolOutput(ctx, "sesn_unknown_output", id, text); err != nil {
				t.Fatal(err)
			}
			ready, err := service.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = objects.Delete(context.Background(), ready.BlobKey) })
			if err := service.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if deleted {
				if err := first.DeleteSession(ctx, "sesn_unknown_output"); err != nil {
					t.Fatal(err)
				}
				if _, err := service.Delete(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			response, err := http.DefaultClient.Do(late)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("late remote commit status=%d", response.StatusCode)
			}
			outage := app.NewFileService(NewFileRepository(first), keySpecificDeleteFailure{BlobStore: objects, key: key}, domain.NewRandomIDGen(), fixedClock{})
			if err := outage.Reconcile(ctx); err == nil {
				t.Fatal("object deletion outage was not exercised")
			}
			if body, err := objects.Open(ctx, key); err != nil {
				t.Fatalf("outage did not preserve late bytes for retry: %v", err)
			} else {
				_ = body.Close()
			}
			// A fresh process must still know the abandoned write after File
			// publication and, in the second case, after all File metadata is gone.
			restarted := app.NewFileService(NewFileRepository(first), objects, domain.NewRandomIDGen(), fixedClock{})
			if err := restarted.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if orphan, err := objects.Open(ctx, key); err == nil {
				_ = orphan.Close()
				t.Fatal("late unknown write left bytes after publication/deletion and restart")
			}
			if !deleted {
				download, err := service.Download(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(download.Body)
				_ = download.Body.Close()
				if err != nil || string(body) != text {
					t.Fatalf("cleanup damaged published output: %v", err)
				}
			}
			var guards int
			if err := first.pool.QueryRow(ctx, "SELECT count(*) FROM blob_cleanup_intents WHERE kind='file' AND blob_key=$1 AND NOT writer_finished", key).Scan(&guards); err != nil || guards != 1 {
				t.Fatalf("unknown writer guard not retained: count=%d err=%v", guards, err)
			}
		})
	}
}

type lostToolOutputCompletion struct{ *FileRepository }

func (r lostToolOutputCompletion) CompleteToolOutputUpload(ctx context.Context, file domain.File) (domain.File, error) {
	ready, err := r.FileRepository.CompleteToolOutputUpload(ctx, file)
	if err == nil {
		err = errors.New("generated File commit acknowledgement lost")
	}
	return ready, err
}

func (r lostToolOutputCompletion) Get(context.Context, string) (domain.File, error) {
	return domain.File{}, errors.New("database read unavailable after lost acknowledgement")
}

type countedToolOutputWrites struct {
	app.BlobStore
	writes atomic.Int32
}

func (b *countedToolOutputWrites) Put(ctx context.Context, key, mime string, body io.Reader, limit int64) (app.BlobInfo, error) {
	b.writes.Add(1)
	return b.BlobStore.Put(ctx, key, mime, body, limit)
}

func TestToolOutput_PostgresS3LostCommitAndReadResponsePreservesReadyBytes(t *testing.T) {
	first, second, objects := uploadRecoveryServices(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := first.CreateSession(ctx, newSession("sesn_lost_output"), nil); err != nil {
		t.Fatal(err)
	}
	counted := &countedToolOutputWrites{BlobStore: objects}
	id := domain.NewRandomIDGen().NewID(domain.PrefixFile)
	text := "completed receipt must survive both lost database responses"
	writer := app.NewFileService(lostToolOutputCompletion{NewFileRepository(first)}, counted, domain.NewRandomIDGen(), fixedClock{})
	if err := writer.StoreToolOutput(ctx, "sesn_lost_output", id, text); err == nil {
		t.Fatal("lost commit and failed fallback read were not exercised")
	}
	service := app.NewFileService(NewFileRepository(second), counted, domain.NewRandomIDGen(), fixedClock{})
	ready, err := service.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = objects.Delete(context.Background(), ready.BlobKey) })
	if err := service.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var before, after int
	if err := first.pool.QueryRow(ctx, "SELECT count(*) FROM blob_cleanup_intents").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := service.StoreToolOutput(ctx, "sesn_lost_output", id, text); err != nil {
		t.Fatalf("ready receipt was not resumed: %v", err)
	}
	if err := first.pool.QueryRow(ctx, "SELECT count(*) FROM blob_cleanup_intents").Scan(&after); err != nil || before != after || counted.writes.Load() != 1 {
		t.Fatalf("ready retry created a write or guard: writes=%d guards=%d->%d err=%v", counted.writes.Load(), before, after, err)
	}
	download, err := service.Download(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(download.Body)
	_ = download.Body.Close()
	if err != nil || string(body) != text {
		t.Fatalf("ready bytes damaged after uncertain commit: %v", err)
	}
}

func TestToolOutput_PostgresS3SupersededWriterPreservesWinningFile(t *testing.T) {
	first, second, objects := uploadRecoveryServices(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ctx = workspace.WithScope(ctx, workspace.DefaultID)
	if _, err := first.CreateSession(ctx, newSession("sesn_concurrent_output"), nil); err != nil {
		t.Fatal(err)
	}
	id := domain.NewRandomIDGen().NewID(domain.PrefixFile)
	text := "immutable output with a complete tail"
	gate := &gatedUploadStore{BlobStore: objects, entered: make(chan string, 1), release: make(chan struct{}), afterPut: true}
	defer gate.unblock()
	writer := app.NewFileService(NewFileRepository(first), gate, domain.NewRandomIDGen(), fixedClock{})
	result := make(chan error, 1)
	go func() { result <- writer.StoreToolOutput(ctx, "sesn_concurrent_output", id, text) }()
	var oldKey string
	select {
	case oldKey = <-gate.entered:
	case <-ctx.Done():
		t.Fatal("first upload never reached completion boundary")
	}
	t.Cleanup(func() { _ = objects.Delete(context.Background(), oldKey) })
	service := app.NewFileService(NewFileRepository(second), objects, domain.NewRandomIDGen(), fixedClock{})
	if err := service.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	// A live, currently referenced attempt cannot be deleted by guard scanning.
	body, err := objects.Open(ctx, oldKey)
	if err != nil {
		t.Fatalf("reconciliation deleted current uploading bytes: %v", err)
	}
	_ = body.Close()
	if err := service.StoreToolOutput(ctx, "sesn_concurrent_output", id, text); err != nil {
		t.Fatal(err)
	}
	winning, err := service.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = objects.Delete(context.Background(), winning.BlobKey) })
	gate.unblock()
	if err := <-result; err != nil {
		t.Fatalf("matching immutable publication was not recovered: %v", err)
	}
	if err := service.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if old, err := objects.Open(ctx, oldKey); err == nil {
		_ = old.Close()
		t.Fatal("superseded writer's bytes survived cleanup")
	}
	download, err := service.Download(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(download.Body)
	_ = download.Body.Close()
	if err != nil || string(data) != text {
		t.Fatalf("superseded cleanup damaged winning output: %v", err)
	}
	var guards int
	if err := first.pool.QueryRow(ctx, "SELECT count(*) FROM blob_cleanup_intents WHERE kind='file' AND blob_key=$1", oldKey).Scan(&guards); err != nil || guards != 0 {
		t.Fatalf("confirmed superseded writer guard remains: count=%d err=%v", guards, err)
	}
}
