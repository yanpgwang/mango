package httpapi

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http/httptest"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestSkillsSDK_AllNineCustomOperations(t *testing.T) {
	service := newSDKSkillService()
	server := httptest.NewServer(NewServer(Deps{Skills: service}, Config{
		RequireAuth: true,
	}).Handler())
	defer server.Close()
	client := anthropic.NewClient(option.WithBaseURL(server.URL), option.WithAuthToken("sk-test"))
	ctx := context.Background()
	archive := sdkSkillZip(t, "reviewing-code")

	created, err := client.Beta.Skills.New(ctx, anthropic.BetaSkillNewParams{
		Files: []io.Reader{&sdkNamedSkillReader{
			Reader: bytes.NewReader(archive), filename: "reviewing-code.zip",
		}},
		DisplayTitle: anthropic.String("Code Review"),
	})
	if err != nil {
		t.Fatalf("Create Skill: %v", err)
	}
	if created.ID != "skill_sdk" || created.DisplayTitle != "Code Review" ||
		created.LatestVersion != "100" || created.Source != "custom" {
		t.Fatalf("created = %s", created.RawJSON())
	}

	got, err := client.Beta.Skills.Get(ctx, created.ID, anthropic.BetaSkillGetParams{})
	if err != nil || got.ID != created.ID {
		t.Fatalf("Get Skill = %+v, %v", got, err)
	}
	listed, err := client.Beta.Skills.List(ctx, anthropic.BetaSkillListParams{
		Source: anthropic.String("custom"), Limit: anthropic.Int(20),
	})
	if err != nil || len(listed.Data) != 1 || listed.Data[0].ID != created.ID {
		t.Fatalf("List Skills = %+v, %v", listed, err)
	}

	second, err := client.Beta.Skills.Versions.New(ctx, created.ID, anthropic.BetaSkillVersionNewParams{
		Files: []io.Reader{&sdkNamedSkillReader{
			Reader: bytes.NewReader(archive), filename: "reviewing-code.zip",
		}},
	})
	if err != nil || second.Version != "200" || second.SkillID != created.ID {
		t.Fatalf("Create Skill Version = %+v, %v", second, err)
	}
	version, err := client.Beta.Skills.Versions.Get(
		ctx, second.Version, anthropic.BetaSkillVersionGetParams{SkillID: created.ID},
	)
	if err != nil || version.Name != "reviewing-code" {
		t.Fatalf("Get Skill Version = %+v, %v", version, err)
	}
	versions, err := client.Beta.Skills.Versions.List(
		ctx, created.ID, anthropic.BetaSkillVersionListParams{Limit: anthropic.Int(20)},
	)
	if err != nil || len(versions.Data) != 2 {
		t.Fatalf("List Skill Versions = %+v, %v", versions, err)
	}
	download, err := client.Beta.Skills.Versions.Download(
		ctx, second.Version, anthropic.BetaSkillVersionDownloadParams{SkillID: created.ID},
	)
	if err != nil {
		t.Fatalf("Download Skill Version: %v", err)
	}
	downloaded, readErr := io.ReadAll(download.Body)
	closeErr := download.Body.Close()
	if readErr != nil || closeErr != nil || !bytes.Equal(downloaded, archive) {
		t.Fatalf("download = %d bytes, read=%v close=%v", len(downloaded), readErr, closeErr)
	}

	for _, value := range []string{"100", "200"} {
		deleted, err := client.Beta.Skills.Versions.Delete(
			ctx, value, anthropic.BetaSkillVersionDeleteParams{SkillID: created.ID},
		)
		if err != nil || deleted.ID != value || deleted.Type != "skill_version_deleted" {
			t.Fatalf("Delete Skill Version %s = %+v, %v", value, deleted, err)
		}
	}
	deleted, err := client.Beta.Skills.Delete(ctx, created.ID, anthropic.BetaSkillDeleteParams{})
	if err != nil || deleted.ID != created.ID || deleted.Type != "skill_deleted" {
		t.Fatalf("Delete Skill = %+v, %v", deleted, err)
	}
}

type sdkNamedSkillReader struct {
	*bytes.Reader
	filename string
}

func (r *sdkNamedSkillReader) Filename() string { return r.filename }

func sdkSkillZip(t *testing.T, name string) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	part, err := writer.Create(name + "/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("---\nname: " + name +
		"\ndescription: Reviews code when a user requests feedback.\n---\n# Review\n")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

type sdkSkillService struct {
	skill    domain.Skill
	versions map[string]domain.SkillVersion
	archives map[string][]byte
	next     int
}

func newSDKSkillService() *sdkSkillService {
	return &sdkSkillService{
		versions: make(map[string]domain.SkillVersion), archives: make(map[string][]byte), next: 100,
	}
}

func (s *sdkSkillService) Create(_ context.Context, input app.SkillCreateInput) (domain.Skill, error) {
	if len(input.Files) != 1 || input.Files[0].Filename != "reviewing-code.zip" || input.DisplayTitle == nil {
		return domain.Skill{}, domain.Validation("unexpected SDK multipart request")
	}
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	version := strconv.Itoa(s.next)
	s.skill = domain.Skill{
		ID: "skill_sdk", CreatedAt: now, UpdatedAt: now, DisplayTitle: *input.DisplayTitle,
		LatestVersion: version, Source: "custom", Ready: true,
	}
	s.versions[version] = sdkVersion(s.skill.ID, version, now, input.Files[0].Body)
	s.archives[version] = append([]byte(nil), input.Files[0].Body...)
	s.next += 100
	return s.skill, nil
}

func (s *sdkSkillService) Get(_ context.Context, id string) (domain.Skill, error) {
	if s.skill.ID != id {
		return domain.Skill{}, domain.NotFound("skill not found")
	}
	return s.skill, nil
}

func (s *sdkSkillService) List(_ context.Context, _ app.SkillListQuery) (app.SkillListPage, error) {
	items := []domain.Skill{}
	if s.skill.ID != "" {
		items = append(items, s.skill)
	}
	return app.SkillListPage{Skills: items}, nil
}

func (s *sdkSkillService) Delete(_ context.Context, id string) (domain.Skill, error) {
	if s.skill.ID != id {
		return domain.Skill{}, domain.NotFound("skill not found")
	}
	if len(s.versions) != 0 {
		return domain.Skill{}, domain.Validation("delete versions first")
	}
	item := s.skill
	s.skill = domain.Skill{}
	return item, nil
}

func (s *sdkSkillService) CreateVersion(
	_ context.Context, skillID string, files []app.SkillUploadFile,
) (domain.SkillVersion, error) {
	if skillID != s.skill.ID || len(files) != 1 {
		return domain.SkillVersion{}, domain.Validation("unexpected SDK multipart request")
	}
	version := strconv.Itoa(s.next)
	item := sdkVersion(skillID, version, s.skill.CreatedAt.Add(time.Second), files[0].Body)
	s.versions[version] = item
	s.archives[version] = append([]byte(nil), files[0].Body...)
	s.skill.LatestVersion = version
	s.skill.UpdatedAt = item.CreatedAt
	s.next += 100
	return item, nil
}

func (s *sdkSkillService) GetVersion(
	_ context.Context, skillID, version string,
) (domain.SkillVersion, error) {
	item, ok := s.versions[version]
	if !ok || item.SkillID != skillID {
		return domain.SkillVersion{}, domain.NotFound("Skill Version not found")
	}
	return item, nil
}

func (s *sdkSkillService) ListVersions(
	_ context.Context, skillID string, _ app.SkillVersionListQuery,
) (app.SkillVersionListPage, error) {
	items := make([]domain.SkillVersion, 0, len(s.versions))
	for _, item := range s.versions {
		if item.SkillID == skillID {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Version < items[j].Version })
	return app.SkillVersionListPage{Versions: items}, nil
}

func (s *sdkSkillService) DeleteVersion(
	_ context.Context, skillID, version string,
) (domain.SkillVersion, error) {
	item, err := s.GetVersion(context.Background(), skillID, version)
	if err != nil {
		return domain.SkillVersion{}, err
	}
	delete(s.versions, version)
	delete(s.archives, version)
	return item, nil
}

func (s *sdkSkillService) Download(
	_ context.Context, skillID, version string,
) (app.SkillVersionDownload, error) {
	item, err := s.GetVersion(context.Background(), skillID, version)
	if err != nil {
		return app.SkillVersionDownload{}, err
	}
	return app.SkillVersionDownload{
		Version: item, Body: io.NopCloser(bytes.NewReader(s.archives[version])),
	}, nil
}

func sdkVersion(skillID, version string, createdAt time.Time, body []byte) domain.SkillVersion {
	checksum := sha256.Sum256(body)
	return domain.SkillVersion{
		ID: version, SkillID: skillID, Version: version, CreatedAt: createdAt,
		Description: "Reviews code when a user requests feedback.",
		Directory:   "reviewing-code", Name: "reviewing-code", SizeBytes: int64(len(body)),
		ChecksumSHA256: fmt.Sprintf("%x", checksum), State: domain.SkillVersionReady,
	}
}
