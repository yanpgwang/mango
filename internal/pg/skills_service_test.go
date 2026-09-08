package pg

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/blob"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	mango "github.com/yanpgwang/mango/sdk/go"
)

func TestSkillService_PostgresS3SDKLifecycleAndRestartReconciliation(t *testing.T) {
	endpoint := os.Getenv("MANGO_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("MANGO_TEST_S3_ENDPOINT not set; skipping Skills service conformance")
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
	repo := NewSkillRepository(store)
	service := app.NewSkillService(repo, blobs, domain.NewSeqIDGen(), fixedClock{})
	server := httptestServerForSkills(t, service)
	defer server.Close()
	client, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	archive := postgresSkillArchive(t)

	created, err := client.Skills.New(ctx, mango.SkillUploadRequest{
		Files: []mango.Upload{{Reader: bytes.NewReader(archive), Filename: "database-audit.zip", ContentType: "application/zip"}},
	})
	if err != nil {
		t.Fatalf("Create Skill: %v", err)
	}
	if created.ID == "" || created.LatestVersion == nil || created.DisplayTitle != "database-audit" {
		t.Fatalf("created = %+v", created)
	}
	download, err := client.Skills.Versions.Download(
		ctx, created.ID, *created.LatestVersion,
	)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	downloaded, readErr := io.ReadAll(download)
	_ = download.Close()
	if readErr != nil || !zipContains(t, downloaded, "database-audit/SKILL.md") {
		t.Fatalf("downloaded archive = %d bytes, %v", len(downloaded), readErr)
	}

	second, err := client.Skills.Versions.New(
		ctx, created.ID, mango.SkillVersionUploadRequest{Files: []mango.Upload{{Reader: bytes.NewReader(archive), Filename: "database-audit.zip", ContentType: "application/zip"}}},
	)
	if err != nil || second.Version == *created.LatestVersion {
		t.Fatalf("Create Version = %+v, %v", second, err)
	}
	if _, err := client.Skills.Delete(ctx, created.ID); err == nil {
		t.Fatal("Skill with Versions was deleted")
	}
	for _, version := range []string{*created.LatestVersion, second.Version} {
		if _, err := client.Skills.Versions.Delete(
			ctx, created.ID, version,
		); err != nil {
			t.Fatalf("Delete Version %s: %v", version, err)
		}
	}
	empty, err := client.Skills.Get(ctx, created.ID)
	if err != nil || empty.LatestVersion != nil {
		t.Fatalf("empty Skill = %+v, %v", empty, err)
	}
	if _, err := client.Skills.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete Skill: %v", err)
	}

	pendingSkill := domain.Skill{
		ID: "skill_restart_pending", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		DisplayTitle: "restart-pending", Source: "custom",
	}
	pendingVersion := repositorySkillVersion(
		pendingSkill.ID, "900", pendingSkill.CreatedAt, true,
	)
	pendingVersion.BlobKey = "skills/skill_restart_pending/900.zip"
	if err := repo.BeginSkill(ctx, pendingSkill, pendingVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := blobs.Put(
		ctx, pendingVersion.BlobKey, "application/zip", bytes.NewReader(archive), app.MaxSkillUploadBytes,
	); err != nil {
		t.Fatal(err)
	}
	restarted := app.NewSkillService(repo, blobs, domain.NewSeqIDGen(), fixedClock{})
	if err := restarted.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if incomplete, err := repo.ListIncompleteVersions(ctx); err != nil || len(incomplete) != 0 {
		t.Fatalf("incomplete Versions = %+v, %v", incomplete, err)
	}
	if _, err := blobs.Open(ctx, pendingVersion.BlobKey); err == nil {
		t.Fatal("orphan Skill archive remains after reconciliation")
	}
}

func httptestServerForSkills(t *testing.T, service *app.SkillService) *httptest.Server {
	t.Helper()
	return httptest.NewServer(httpapi.NewServer(httpapi.Deps{Skills: service}, httpapi.Config{
		RequireAuth: true,
	}).Handler())
}

type postgresSkillReader struct {
	*bytes.Reader
	filename string
}

func (r *postgresSkillReader) Filename() string { return r.filename }

func postgresSkillArchive(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	part, err := writer.Create("database-audit/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("---\nname: database-audit\n" +
		"description: Audits database changes when schema work needs review.\n---\n# Audit\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func zipContains(t *testing.T, body []byte, name string) bool {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	for _, file := range reader.File {
		if file.Name == name {
			return true
		}
	}
	return false
}
