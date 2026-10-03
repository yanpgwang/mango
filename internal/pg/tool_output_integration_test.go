package pg

import (
	"context"
	"crypto/sha256"
	"fmt"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/blob"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/workspace"
	mango "github.com/yanpgwang/mango/sdk/go"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestToolOutput_PostgresS3RecoveryScopeAndSessionRelease(t *testing.T) {
	endpoint := os.Getenv("MANGO_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3 service not configured")
	}
	store := testStore(t)
	ctx := workspace.WithScope(context.Background(), workspace.DefaultID)
	trigger := journalTurn(t, store, "sesn_output")
	blobs, err := blob.NewS3Store(ctx, blob.S3Config{Endpoint: endpoint, Region: "us-east-1", Bucket: os.Getenv("MANGO_TEST_S3_BUCKET"), AccessKey: os.Getenv("MANGO_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("MANGO_TEST_S3_SECRET_KEY"), UsePathStyle: true, CreateBucket: true, UploadTempDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	repo := NewFileRepository(store)
	service := app.NewFileService(repo, blobs, domain.NewSeqIDGen(), fixedClock{})
	text := strings.Repeat("中", 100001) + "\x00tail-proof"
	info := app.ComputeBlobInfo([]byte(text))
	id := domain.NewRandomIDGen().NewID(domain.PrefixFile)
	defer func() { _ = blobs.Delete(ctx, workspace.BlobKey(ctx, "files/"+id)) }()
	pending := domain.File{ID: id, SessionID: "sesn_output", Filename: id + ".txt", MimeType: "text/plain", SizeBytes: info.SizeBytes, ChecksumSHA256: info.ChecksumSHA256, BlobKey: workspace.BlobKey(ctx, "files/"+id), State: domain.FileStateUploading}
	if _, err := repo.EnsureToolOutputUpload(ctx, pending); err != nil {
		t.Fatal(err)
	}
	// Startup reconciliation must leave the orchestrator's resumable intent alone.
	if err := service.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	restarted := app.NewFileService(repo, blobs, domain.NewSeqIDGen(), fixedClock{})
	for range 2 {
		if err := restarted.StoreToolOutput(ctx, "sesn_output", id, text); err != nil {
			t.Fatal(err)
		}
	}
	handler := httpapi.NewServer(httpapi.Deps{Files: restarted}, httpapi.Config{}).Handler()
	for _, test := range []struct {
		name string
		ctx  context.Context
		want int
	}{
		{"owner", workspace.WithSessionScope(ctx, workspace.DefaultID, workspace.SessionScope{SessionID: "sesn_output"}), 200},
		{"other Session", workspace.WithSessionScope(ctx, workspace.DefaultID, workspace.SessionScope{SessionID: "sesn_other"}), 404},
		{"other Workspace", workspace.WithScope(ctx, "wrkspc_other"), 404},
	} {
		request := httptest.NewRequest(http.MethodGet, "/v1/files/"+id+"/content", nil).WithContext(test.ctx)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Fatalf("%s status=%d body=%s", test.name, response.Code, response.Body.String())
		}
		if test.want == 200 && response.Body.String() != text {
			t.Fatal("full content differs")
		}
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	client, err := mango.New(mango.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := client.Files.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	sum, ok := metadata.ChecksumSHA256.Get()
	if !ok || sum != fmt.Sprintf("%x", sha256.Sum256([]byte(text))) {
		t.Fatal("SDK lost File checksum")
	}
	download, err := client.Files.Download(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(download)
	_ = download.Close()
	if err != nil || string(data) != text {
		t.Fatal("SDK download lost full text")
	}
	if _, err := service.Delete(ctx, id); err == nil {
		t.Fatal("pinned File deleted")
	}
	// Real journal publication clears its private full receipt after File success.
	attempt, err := store.BeginAttempt(ctx, "sesn_output", trigger)
	if err != nil {
		t.Fatal(err)
	}
	stepID, err := store.PrepareToolStep(ctx, attempt.ID, 0, "sevt_output", "mcp__get", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.StartToolStep(ctx, stepID); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteToolStep(ctx, stepID, domain.ToolStepResult{Content: []any{map[string]any{"type": "text", "text": "preview"}}, FileID: id, FullOutput: []byte(text)}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := store.MarkToolOutputPublished(ctx, stepID, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.MarkToolOutputPublished(ctx, stepID, "file_wrong"); err == nil {
		t.Fatal("wrong publication accepted")
	}
	var raw string
	if err := store.pool.QueryRow(ctx, "SELECT result::text FROM tool_steps WHERE id=$1", stepID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "full_output") {
		t.Fatal("temporary receipt not cleared")
	}
	if _, err := store.CompleteWorkflowTurn(ctx, "sesn_output", trigger, []domain.EventDraft{{Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}}}}, domain.StatusIdle, attempt.ID, domain.RunAttemptCompleted, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSession(ctx, "sesn_output"); err != nil {
		t.Fatal(err)
	}
	released, err := service.Get(ctx, id)
	if err != nil || released.SessionID != "" {
		t.Fatalf("release=%+v err=%v", released, err)
	}
	if _, err := service.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := service.StoreToolOutput(ctx, "sesn_output", id, text); err == nil {
		t.Fatal("deleted Session resurrected File")
	}
}
