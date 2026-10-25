package pg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/blob"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

// Two independent pools share only the test's PostgreSQL schema and S3 bucket.
// The gate pauses an actual upload at the storage boundary, after its intent.
func uploadRecoveryServices(t *testing.T) (*Store, *Store, app.BlobStore) {
	t.Helper()
	if os.Getenv("MANGO_TEST_S3_ENDPOINT") == "" {
		t.Skip("S3 service not configured")
	}
	first := testStore(t)
	pool, err := pgxpool.NewWithConfig(context.Background(), first.pool.Config().Copy())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	second := NewDefaultWorkspaceStore(pool, domain.NewRandomIDGen(), fixedClock{})
	blobs, err := blob.NewS3Store(context.Background(), blob.S3Config{
		Endpoint: os.Getenv("MANGO_TEST_S3_ENDPOINT"), Region: "us-east-1",
		Bucket: os.Getenv("MANGO_TEST_S3_BUCKET"), AccessKey: os.Getenv("MANGO_TEST_S3_ACCESS_KEY"),
		SecretKey: os.Getenv("MANGO_TEST_S3_SECRET_KEY"), UsePathStyle: true,
		CreateBucket: true, UploadTempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return first, second, blobs
}

func TestBlobCleanup_PostgresRevisionAndWorkspaceFence(t *testing.T) {
	store := testStore(t)
	repo := NewFileRepository(store)
	ctx := context.Background()
	first, err := repo.RetainBlobCleanup(ctx, "files/late", true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := repo.RetainBlobCleanup(ctx, "files/late", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveBlobCleanup(ctx, first); err != nil {
		t.Fatal(err)
	}
	other := workspace.WithScope(ctx, "wrkspc_other")
	if err := repo.RemoveBlobCleanup(other, second); err != nil {
		t.Fatal(err)
	}
	if items, err := repo.ListBlobCleanup(other); err != nil || len(items) != 0 {
		t.Fatalf("cross-Workspace cleanup visible: %+v %v", items, err)
	}
	items, err := repo.ListBlobCleanup(ctx)
	if err != nil || len(items) != 1 || items[0] != second || second.Revision <= first.Revision {
		t.Fatalf("new receipt cleared by stale acknowledgement: %+v %v", items, err)
	}
	if err := repo.RemoveBlobCleanup(ctx, second); err != nil {
		t.Fatal(err)
	}
	if items, err := repo.ListBlobCleanup(ctx); err != nil || len(items) != 0 {
		t.Fatalf("cleanup receipt not removed: %+v %v", items, err)
	}
	fresh, err := repo.RetainBlobCleanup(ctx, "files/late", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveBlobCleanup(ctx, first); err != nil {
		t.Fatal(err)
	}
	if items, err := repo.ListBlobCleanup(ctx); err != nil || len(items) != 1 || items[0] != fresh {
		t.Fatalf("deleted/recreated receipt lost to stale acknowledgement: %+v %v", items, err)
	}
}

func TestBlobCleanup_PostgresUnconfirmedGuardsRotateAndRemain(t *testing.T) {
	store := testStore(t)
	repo := NewFileRepository(store)
	ctx := context.Background()
	for i := 0; i < 101; i++ {
		if _, err := repo.RetainBlobCleanup(ctx, fmt.Sprintf("files/%03d", i), false); err != nil {
			t.Fatal(err)
		}
	}
	first, err := repo.ListBlobCleanup(ctx)
	if err != nil || len(first) != 100 {
		t.Fatalf("scan not bounded: %d %v", len(first), err)
	}
	for _, intent := range first {
		if err := repo.TouchBlobCleanup(ctx, intent); err != nil {
			t.Fatal(err)
		}
		if err := repo.RemoveBlobCleanup(ctx, intent); err != nil {
			t.Fatal(err)
		}
	}
	next, err := repo.ListBlobCleanup(ctx)
	if err != nil || len(next) != 100 || next[0].BlobKey != "files/100" {
		t.Fatalf("unvisited key starved behind permanent guards: %+v %v", next, err)
	}
	var count int
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM blob_cleanup_intents").Scan(&count); err != nil || count != 101 {
		t.Fatalf("unconfirmed guards removed: %d %v", count, err)
	}
	confirmed, err := repo.RetainBlobCleanup(ctx, first[0].BlobKey, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.RemoveBlobCleanup(ctx, first[0]); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM blob_cleanup_intents").Scan(&count); err != nil || count != 101 {
		t.Fatalf("old acknowledgement removed confirmation: %d %v", count, err)
	}
	if err := repo.RemoveBlobCleanup(ctx, confirmed); err != nil {
		t.Fatal(err)
	}
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM blob_cleanup_intents").Scan(&count); err != nil || count != 100 {
		t.Fatalf("confirmed guard not collected: %d %v", count, err)
	}
}

// The writer disappears after the real object write, without calling any
// service completion or cleanup hook. Only another API's durable claim can
// preserve the key across this crash window.
func TestUpload_PostgresS3LateWriteCrashRetainsCleanupGuard(t *testing.T) {
	for _, kind := range []string{"File", "Skill"} {
		t.Run(kind, func(t *testing.T) {
			first, second, blobs := uploadRecoveryServices(t)
			ctx := context.Background()
			id := domain.NewRandomIDGen().NewID("late_")
			key := "crash/" + id
			defer func() { _ = blobs.Delete(ctx, key) }()
			now := time.Now().UTC()
			var reconcile func(context.Context) error
			if kind == "File" {
				pending := domain.File{ID: id, Filename: "late.txt", MimeType: "text/plain", CreatedAt: now, UpdatedAt: now, BlobKey: key, State: domain.FileStateUploading}
				if err := NewFileRepository(first).BeginUpload(ctx, pending); err != nil {
					t.Fatal(err)
				}
				if _, err := first.pool.Exec(ctx, "UPDATE files SET upload_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", id); err != nil {
					t.Fatal(err)
				}
				reconcile = app.NewFileService(NewFileRepository(second), blobs, domain.NewRandomIDGen(), fixedClock{}).Reconcile
			} else {
				skill := domain.Skill{ID: id, CreatedAt: now, UpdatedAt: now, DisplayTitle: "late", Source: "custom"}
				version := domain.SkillVersion{SkillID: id, Version: "1", CreatedAt: now, Name: "late", Directory: "late", BlobKey: key, State: domain.SkillVersionUploading, Initial: true}
				if err := NewSkillRepository(first).BeginSkill(ctx, skill, version); err != nil {
					t.Fatal(err)
				}
				if _, err := first.pool.Exec(ctx, "UPDATE skill_versions SET upload_expires_at=clock_timestamp()-interval '1 second' WHERE skill_id=$1", id); err != nil {
					t.Fatal(err)
				}
				reconcile = app.NewSkillService(NewSkillRepository(second), blobs, domain.NewRandomIDGen(), fixedClock{}).Reconcile
			}
			if err := reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if _, err := blobs.Put(ctx, key, "text/plain", bytes.NewBufferString("late committed write"), 100); err != nil {
				t.Fatal(err)
			}
			if err := reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			if body, err := blobs.Open(ctx, key); err == nil {
				_ = body.Close()
				t.Fatal("late write survived restart after writer disappeared")
			}
			var count int
			if err := first.pool.QueryRow(ctx, "SELECT count(*) FROM blob_cleanup_intents WHERE blob_key=$1", key).Scan(&count); err != nil || count != 1 {
				t.Fatalf("unconfirmed writer guard lost: %d %v", count, err)
			}
		})
	}
}

type keySpecificDeleteFailure struct {
	app.BlobStore
	key string
}

func (b keySpecificDeleteFailure) Delete(ctx context.Context, key string) error {
	if key == b.key {
		return errors.New("object-specific deletion failure")
	}
	return b.BlobStore.Delete(ctx, key)
}

func TestUpload_PostgresS3MetadataFailureDoesNotStarveGuard(t *testing.T) {
	for _, kind := range []string{"File", "Skill"} {
		t.Run(kind, func(t *testing.T) {
			first, _, blobs := uploadRecoveryServices(t)
			ctx := context.Background()
			key := "failed/" + domain.NewRandomIDGen().NewID("")
			late := "late/" + domain.NewRandomIDGen().NewID("")
			defer func() { _ = blobs.Delete(ctx, late) }()
			if _, err := blobs.Put(ctx, late, "text/plain", bytes.NewBufferString("late bytes"), 100); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			id := domain.NewRandomIDGen().NewID("pending_")
			var reconcile func(context.Context) error
			faulty := keySpecificDeleteFailure{BlobStore: blobs, key: key}
			if kind == "File" {
				repo := NewFileRepository(first)
				if err := repo.BeginUpload(ctx, domain.File{ID: id, Filename: "pending.txt", MimeType: "text/plain", CreatedAt: now, UpdatedAt: now, BlobKey: key, State: domain.FileStateUploading}); err != nil {
					t.Fatal(err)
				}
				if _, err := first.pool.Exec(ctx, "UPDATE files SET upload_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1", id); err != nil {
					t.Fatal(err)
				}
				if _, err := repo.RetainBlobCleanup(ctx, late, true); err != nil {
					t.Fatal(err)
				}
				reconcile = app.NewFileService(repo, faulty, domain.NewRandomIDGen(), fixedClock{}).Reconcile
			} else {
				repo := NewSkillRepository(first)
				skill := domain.Skill{ID: id, CreatedAt: now, UpdatedAt: now, DisplayTitle: "pending", Source: "custom"}
				version := domain.SkillVersion{SkillID: id, Version: "1", CreatedAt: now, Name: "pending", Directory: "pending", BlobKey: key, State: domain.SkillVersionUploading, Initial: true}
				if err := repo.BeginSkill(ctx, skill, version); err != nil {
					t.Fatal(err)
				}
				if _, err := first.pool.Exec(ctx, "UPDATE skill_versions SET upload_expires_at=clock_timestamp()-interval '1 second' WHERE skill_id=$1", id); err != nil {
					t.Fatal(err)
				}
				if _, err := repo.RetainBlobCleanup(ctx, late, true); err != nil {
					t.Fatal(err)
				}
				reconcile = app.NewSkillService(repo, faulty, domain.NewRandomIDGen(), fixedClock{}).Reconcile
			}
			if err := reconcile(ctx); err == nil {
				t.Fatal("metadata failure was hidden")
			}
			if body, err := blobs.Open(ctx, late); err == nil {
				_ = body.Close()
				t.Fatal("metadata failure blocked independent guard cleanup")
			}
		})
	}
}

type gatedUploadStore struct {
	app.BlobStore
	entered  chan string
	release  chan struct{}
	afterPut bool
	once     sync.Once
}

type unavailableCleanupStore struct{ app.BlobStore }

func (unavailableCleanupStore) Delete(context.Context, string) error {
	return errors.New("temporary object deletion outage")
}

func (s *gatedUploadStore) unblock() { s.once.Do(func() { close(s.release) }) }

func (s *gatedUploadStore) Put(ctx context.Context, key, mime string, body io.Reader, limit int64) (app.BlobInfo, error) {
	var info app.BlobInfo
	var err error
	if s.afterPut {
		info, err = s.BlobStore.Put(ctx, key, mime, body, limit)
		if err != nil {
			return info, err
		}
	}
	s.entered <- key
	select {
	case <-s.release:
	case <-ctx.Done():
		return app.BlobInfo{}, ctx.Err()
	}
	if s.afterPut {
		return info, nil
	}
	return s.BlobStore.Put(ctx, key, mime, body, limit)
}

func TestFileUpload_PostgresS3OtherAPIReconcilePreservesActiveUpload(t *testing.T) {
	first, second, blobs := uploadRecoveryServices(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gate := &gatedUploadStore{BlobStore: blobs, entered: make(chan string, 1), release: make(chan struct{})}
	defer gate.unblock()
	service := app.NewFileService(NewFileRepository(first), gate, domain.NewRandomIDGen(), fixedClock{})
	reconciler := app.NewFileService(NewFileRepository(second), blobs, domain.NewRandomIDGen(), fixedClock{})
	result := make(chan error, 1)
	go func() {
		_, err := service.Upload(ctx, app.FileUploadInput{Filename: "active.txt", MimeType: "text/plain", Body: bytes.NewBufferString("complete output")})
		result <- err
	}()
	var key string
	select {
	case key = <-gate.entered:
	case <-ctx.Done():
		t.Fatal("upload did not reach blob storage")
	}
	defer func() { _ = blobs.Delete(context.Background(), key) }()
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Error(err)
	}
	gate.unblock()
	if err := <-result; err != nil {
		t.Errorf("other API cleaned an active upload: %v", err)
	}
	var count int
	if err := first.pool.QueryRow(ctx, `SELECT count(*) FROM files WHERE blob_key=$1 AND state='ready'`, key).Scan(&count); err != nil || count != 1 {
		t.Errorf("completed metadata missing: count=%d err=%v", count, err)
	}
}

func TestSkillUpload_PostgresS3OtherAPIReconcilePreservesActiveUpload(t *testing.T) {
	first, second, blobs := uploadRecoveryServices(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gate := &gatedUploadStore{BlobStore: blobs, entered: make(chan string, 1), release: make(chan struct{}), afterPut: true}
	defer gate.unblock()
	service := app.NewSkillService(NewSkillRepository(first), gate, domain.NewRandomIDGen(), fixedClock{})
	reconciler := app.NewSkillService(NewSkillRepository(second), blobs, domain.NewRandomIDGen(), fixedClock{})
	archive := postgresSkillArchive(t)
	result := make(chan error, 1)
	go func() {
		_, err := service.Create(ctx, app.SkillCreateInput{Files: []app.SkillUploadFile{{Body: archive, Filename: "database-audit.zip"}}})
		result <- err
	}()
	var key string
	select {
	case key = <-gate.entered:
	case <-ctx.Done():
		t.Fatal("upload did not reach blob storage")
	}
	defer func() { _ = blobs.Delete(context.Background(), key) }()
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Error(err)
	}
	gate.unblock()
	if err := <-result; err != nil {
		t.Errorf("other API cleaned an active Skill upload: %v", err)
	}
	download, err := blobs.Open(ctx, key)
	if err != nil {
		t.Errorf("completed archive lost: %v", err)
	} else {
		_ = download.Close()
	}
}

func TestFileUpload_PostgresS3ExpiredWriterCannotPublishOrLeaveBytes(t *testing.T) {
	first, second, blobs := uploadRecoveryServices(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	gate := &gatedUploadStore{BlobStore: unavailableCleanupStore{blobs}, entered: make(chan string, 1), release: make(chan struct{})}
	defer gate.unblock()
	repo := NewFileRepository(first)
	service := app.NewFileService(repo, gate, domain.NewRandomIDGen(), fixedClock{})
	result := make(chan error, 1)
	go func() {
		_, err := service.Upload(ctx, app.FileUploadInput{Filename: "late.txt", MimeType: "text/plain", Body: bytes.NewBufferString("late bytes")})
		result <- err
	}()
	var key, id string
	select {
	case key = <-gate.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer func() { _ = blobs.Delete(context.Background(), key) }()
	if err := first.pool.QueryRow(ctx, `UPDATE files SET upload_expires_at=now()-interval '1 second' WHERE blob_key=$1 RETURNING id`, key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := repo.RenewUpload(ctx, id); !errors.Is(err, app.ErrUploadLeaseLost) {
		t.Fatalf("expired lease revived: %v", err)
	}
	if err := app.NewFileService(NewFileRepository(second), blobs, domain.NewRandomIDGen(), fixedClock{}).Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	gate.unblock()
	if err := <-result; !errors.Is(err, app.ErrUploadLeaseLost) {
		t.Fatalf("expired writer published: %v", err)
	}
	if err := app.NewFileService(NewFileRepository(second), blobs, domain.NewRandomIDGen(), fixedClock{}).Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if download, err := blobs.Open(ctx, key); err == nil {
		_ = download.Close()
		t.Fatal("late upload left orphan bytes")
	}
}

func TestSkillUpload_PostgresS3ExpiredWriterCannotAffectReusedVersion(t *testing.T) {
	first, second, blobs := uploadRecoveryServices(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	apiClock := domain.FixedClock{T: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	archive := postgresSkillArchive(t)
	input := []app.SkillUploadFile{{Filename: "database-audit.zip", Body: archive}}
	seed := app.NewSkillService(NewSkillRepository(first), blobs, domain.NewRandomIDGen(), apiClock)
	skill, err := seed.Create(ctx, app.SkillCreateInput{Files: input})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		rows, _ := first.pool.Query(context.Background(), `SELECT blob_key FROM skill_versions WHERE skill_id=$1`, skill.ID)
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var key string
				if rows.Scan(&key) == nil {
					_ = blobs.Delete(context.Background(), key)
				}
			}
		}
	}()
	oldGate := &gatedUploadStore{BlobStore: unavailableCleanupStore{blobs}, entered: make(chan string, 1), release: make(chan struct{})}
	newGate := &gatedUploadStore{BlobStore: blobs, entered: make(chan string, 1), release: make(chan struct{}), afterPut: true}
	defer oldGate.unblock()
	defer newGate.unblock()
	oldService := app.NewSkillService(NewSkillRepository(first), oldGate, domain.NewRandomIDGen(), apiClock)
	newService := app.NewSkillService(NewSkillRepository(second), newGate, domain.NewRandomIDGen(), apiClock)
	oldResult, newResult := make(chan error, 1), make(chan error, 1)
	go func() { _, err := oldService.CreateVersion(ctx, skill.ID, input); oldResult <- err }()
	var oldKey, newKey, version string
	select {
	case oldKey = <-oldGate.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer func() { _ = blobs.Delete(context.Background(), oldKey) }()
	if err := first.pool.QueryRow(ctx, `UPDATE skill_versions SET upload_expires_at=now()-interval '1 second' WHERE blob_key=$1 RETURNING version`, oldKey).Scan(&version); err != nil {
		t.Fatal(err)
	}
	reconciler := app.NewSkillService(NewSkillRepository(second), blobs, domain.NewRandomIDGen(), apiClock)
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	// Fixed API clocks deliberately produce the same public Version again.
	go func() { _, err := newService.CreateVersion(ctx, skill.ID, input); newResult <- err }()
	select {
	case newKey = <-newGate.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer func() { _ = blobs.Delete(context.Background(), newKey) }()
	var newVersion string
	if err := first.pool.QueryRow(ctx, `SELECT version FROM skill_versions WHERE blob_key=$1`, newKey).Scan(&newVersion); err != nil || newVersion != version {
		t.Fatalf("Version reuse not exercised: %q %q %v", version, newVersion, err)
	}
	oldGate.unblock()
	if err := <-oldResult; !errors.Is(err, app.ErrUploadLeaseLost) {
		t.Fatalf("old writer completed new upload: %v", err)
	}
	newGate.unblock()
	if err := <-newResult; err != nil {
		t.Fatalf("old writer removed or released new upload: %v", err)
	}
	ready, err := NewSkillRepository(second).GetVersion(ctx, skill.ID, newVersion)
	if err != nil || ready.BlobKey != newKey {
		t.Fatalf("wrong archive published: %+v %v", ready, err)
	}
	if download, err := blobs.Open(ctx, newKey); err != nil {
		t.Fatal(err)
	} else {
		_ = download.Close()
	}
	if err := reconciler.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if download, err := blobs.Open(ctx, oldKey); err == nil {
		_ = download.Close()
		t.Fatal("old archive not cleaned")
	}
}

type lostFileCompletion struct{ app.FileRepository }

func (r lostFileCompletion) CompleteUpload(ctx context.Context, id string, info app.BlobInfo) (domain.File, error) {
	file, err := r.FileRepository.CompleteUpload(ctx, id, info)
	if err == nil {
		err = errors.New("completion response lost after commit")
	}
	return file, err
}

type lostSkillCompletion struct{ app.SkillRepository }

func (r lostSkillCompletion) CompleteVersion(ctx context.Context, id, version, key string, info app.BlobInfo) (domain.Skill, domain.SkillVersion, error) {
	skill, item, err := r.SkillRepository.CompleteVersion(ctx, id, version, key, info)
	if err == nil {
		err = errors.New("completion response lost after commit")
	}
	return skill, item, err
}

func TestUpload_PostgresS3LostCompletionResponsePreservesReadyBytes(t *testing.T) {
	for _, kind := range []string{"File", "Skill"} {
		t.Run(kind, func(t *testing.T) {
			first, second, blobs := uploadRecoveryServices(t)
			ctx := context.Background()
			var key string
			if kind == "File" {
				service := app.NewFileService(lostFileCompletion{NewFileRepository(first)}, blobs, domain.NewRandomIDGen(), fixedClock{})
				if _, err := service.Upload(ctx, app.FileUploadInput{Filename: "lost.txt", MimeType: "text/plain", Body: bytes.NewBufferString("keep ready bytes")}); err == nil {
					t.Fatal("lost acknowledgement not exercised")
				}
				if err := app.NewFileService(NewFileRepository(second), blobs, domain.NewRandomIDGen(), fixedClock{}).Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				if err := first.pool.QueryRow(ctx, `SELECT blob_key FROM files WHERE state='ready'`).Scan(&key); err != nil {
					t.Fatal(err)
				}
			} else {
				service := app.NewSkillService(lostSkillCompletion{NewSkillRepository(first)}, blobs, domain.NewRandomIDGen(), fixedClock{})
				if _, err := service.Create(ctx, app.SkillCreateInput{Files: []app.SkillUploadFile{{Filename: "database-audit.zip", Body: postgresSkillArchive(t)}}}); err == nil {
					t.Fatal("lost acknowledgement not exercised")
				}
				if err := app.NewSkillService(NewSkillRepository(second), blobs, domain.NewRandomIDGen(), fixedClock{}).Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				if err := first.pool.QueryRow(ctx, `SELECT blob_key FROM skill_versions WHERE state='ready'`).Scan(&key); err != nil {
					t.Fatal(err)
				}
			}
			defer func() { _ = blobs.Delete(ctx, key) }()
			download, err := blobs.Open(ctx, key)
			if err != nil {
				t.Fatal("committed bytes were deleted:", err)
			}
			defer func() { _ = download.Close() }()
			data, err := io.ReadAll(download)
			if err != nil || len(data) == 0 {
				t.Fatalf("invalid ready bytes: len=%d err=%v", len(data), err)
			}
		})
	}
}
