package pg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

func pendingToolOutput(id, session, key string) domain.File {
	now := time.Now().UTC()
	info := app.ComputeBlobInfo([]byte("durable receipt"))
	return domain.File{ID: id, SessionID: session, Filename: id + ".txt", MimeType: "text/plain", BlobKey: key,
		SizeBytes: info.SizeBytes, ChecksumSHA256: info.ChecksumSHA256, State: domain.FileStateUploading, CreatedAt: now, UpdatedAt: now}
}

func TestToolOutput_PostgresReceiptAndAttemptFences(t *testing.T) {
	store := testStore(t)
	ctx := workspace.WithScope(context.Background(), workspace.DefaultID)
	for _, id := range []string{"sesn_owner", "sesn_other"} {
		if _, err := store.CreateSession(ctx, newSession(id), nil); err != nil {
			t.Fatal(err)
		}
	}
	repo := NewFileRepository(store)
	first := pendingToolOutput("file_receipt", "sesn_owner", "files/first")
	if _, err := repo.BeginToolOutputUpload(ctx, first); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*domain.File){
		func(f *domain.File) { f.SessionID = "sesn_other" },
		func(f *domain.File) { f.Filename = "other.txt" },
		func(f *domain.File) { f.MimeType = "application/json" },
		func(f *domain.File) { f.SizeBytes++ },
		func(f *domain.File) { f.ChecksumSHA256 = app.ComputeBlobInfo([]byte("different")).ChecksumSHA256 },
	} {
		wrong := first
		wrong.BlobKey = "files/rejected"
		mutate(&wrong)
		if _, err := repo.BeginToolOutputUpload(ctx, wrong); err == nil {
			t.Fatal("changed immutable receipt admitted")
		}
		if _, err := repo.CompleteToolOutputUpload(ctx, wrong); err == nil {
			t.Fatal("changed immutable receipt published")
		}
	}
	other := workspace.WithScope(ctx, "wrkspc_other")
	if _, err := repo.BeginToolOutputUpload(other, first); err == nil {
		t.Fatal("cross-Workspace admission succeeded")
	}
	if _, err := repo.CompleteToolOutputUpload(other, first); err == nil {
		t.Fatal("cross-Workspace publication succeeded")
	}
	var current string
	var guards int
	if err := store.pool.QueryRow(ctx, "SELECT blob_key FROM files WHERE id=$1", first.ID).Scan(&current); err != nil || current != first.BlobKey {
		t.Fatalf("rejected request changed current attempt: %q %v", current, err)
	}
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM blob_cleanup_intents").Scan(&guards); err != nil || guards != 1 {
		t.Fatalf("admission guard was missing or rejected request committed: %d %v", guards, err)
	}
	second := first
	second.BlobKey = "files/second"
	if _, err := repo.BeginToolOutputUpload(ctx, second); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CompleteToolOutputUpload(ctx, first); !errors.Is(err, app.ErrUploadLeaseLost) {
		t.Fatalf("superseded attempt published pending replacement: %v", err)
	}
	if ready, err := repo.CompleteToolOutputUpload(ctx, second); err != nil || ready.BlobKey != second.BlobKey {
		t.Fatalf("current attempt failed: %+v %v", ready, err)
	}
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM blob_cleanup_intents WHERE blob_key=$1", second.BlobKey).Scan(&guards); err != nil || guards != 0 {
		t.Fatalf("published attempt guard not removed atomically: %d %v", guards, err)
	}
}

func TestToolOutput_PostgresOrdinaryCompletionCannotPublishGeneratedFile(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	if _, err := store.CreateSession(ctx, newSession("sesn_generated"), nil); err != nil {
		t.Fatal(err)
	}
	repo := NewFileRepository(store)
	file := pendingToolOutput("file_generated", "sesn_generated", "files/generated")
	if _, err := repo.BeginToolOutputUpload(ctx, file); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CompleteUpload(ctx, file.ID, app.ComputeBlobInfo([]byte("different bytes"))); !errors.Is(err, app.ErrUploadLeaseLost) {
		t.Fatalf("ID-only completion bypassed generated attempt fence: %v", err)
	}
}

func TestToolOutput_PostgresDeletionFencesWaitingAdmissionAndPublication(t *testing.T) {
	for _, operation := range []string{"Begin", "Complete"} {
		t.Run(operation, func(t *testing.T) {
			store := testStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := store.CreateSession(ctx, newSession("sesn_deleting_output"), nil); err != nil {
				t.Fatal(err)
			}
			file := pendingToolOutput("file_deleting_output", "sesn_deleting_output", "files/deleting-output")
			if operation == "Complete" {
				if _, err := NewFileRepository(store).BeginToolOutputUpload(ctx, file); err != nil {
					t.Fatal(err)
				}
			}
			cfg := store.pool.Config().Copy()
			name := domain.NewRandomIDGen().NewID("tool_output_wait_")
			cfg.ConnConfig.RuntimeParams["application_name"] = name
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			repo := NewFileRepository(NewDefaultWorkspaceStore(pool, domain.NewRandomIDGen(), fixedClock{}))
			tx, err := store.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			// Hold the same Session lock as PrepareSessionDeletion. Publication
			// must re-evaluate the deletion fence after this transaction commits.
			if _, err := tx.Exec(ctx, "UPDATE sessions SET deleting_at=clock_timestamp() WHERE id=$1", file.SessionID); err != nil {
				t.Fatal(err)
			}
			result := make(chan error, 1)
			go func() {
				if operation == "Begin" {
					_, err := repo.BeginToolOutputUpload(ctx, file)
					result <- err
				} else {
					_, err := repo.CompleteToolOutputUpload(ctx, file)
					result <- err
				}
			}()
			for {
				var waiting bool
				if err := store.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=$1 AND wait_event_type='Lock')", name).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err := <-result:
					t.Fatalf("operation bypassed Session lock: %v", err)
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				case <-time.After(5 * time.Millisecond):
				}
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-result; err == nil {
				t.Fatal("operation passed committed Session deletion fence")
			}
			if err := store.FinalizeSessionDeletion(ctx, file.SessionID); err != nil {
				t.Fatal(err)
			}
			items, err := NewFileRepository(store).ListIncomplete(ctx)
			if err != nil || (operation == "Complete" && len(items) != 1) || (operation == "Begin" && len(items) != 0) {
				t.Fatalf("deletion did not preserve only the previously admitted attempt: %+v %v", items, err)
			}
		})
	}
}
