package app

import (
	"context"
	"errors"
	"io"
	"sort"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestSkillService_CustomLifecycleAndReconciliation(t *testing.T) {
	ctx := context.Background()
	repo := newMemorySkillRepository()
	blobs := newMemoryBlobStore()
	service := NewSkillService(repo, blobs, domain.NewSeqIDGen(), domain.FixedClock{
		T: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC),
	})
	manifest := []byte("---\nname: report-analysis\ndescription: Analyzes reports when a user supplies business data.\n---\n# Workflow\n")
	bundle := []SkillUploadFile{{Filename: "report-analysis.zip", Body: makeSkillZip(t, []zipTestEntry{
		{name: "report-analysis/SKILL.md", body: manifest, mode: 0o644},
		{name: "report-analysis/reference.md", body: []byte("reference"), mode: 0o644},
	})}}

	created, err := service.Create(ctx, SkillCreateInput{Files: bundle})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID != "skill_1" || created.DisplayTitle != "report-analysis" ||
		created.LatestVersion == "" || created.Source != "custom" {
		t.Fatalf("created = %+v", created)
	}
	first, err := service.GetVersion(ctx, created.ID, created.LatestVersion)
	if err != nil {
		t.Fatal(err)
	}
	download, err := service.Download(ctx, created.ID, first.Version)
	if err != nil {
		t.Fatal(err)
	}
	archive, err := io.ReadAll(download.Body)
	_ = download.Body.Close()
	if err != nil || string(readZipFile(t, archive, "report-analysis/SKILL.md")) != string(manifest) {
		t.Fatalf("download archive = %d bytes, %v", len(archive), err)
	}

	second, err := service.CreateVersion(ctx, created.ID, bundle)
	if err != nil {
		t.Fatalf("CreateVersion: %v", err)
	}
	if second.Version == first.Version {
		t.Fatal("immutable Versions reused an identifier")
	}
	if _, err := service.Delete(ctx, created.ID); err == nil {
		t.Fatal("Skill with Versions was deleted")
	}
	for _, version := range []string{first.Version, second.Version} {
		if _, err := service.DeleteVersion(ctx, created.ID, version); err != nil {
			t.Fatalf("DeleteVersion %s: %v", version, err)
		}
	}
	if _, err := service.Delete(ctx, created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	pendingSkill := domain.Skill{
		ID: "skill_pending", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		DisplayTitle: "pending", Source: "custom",
	}
	pendingVersion := domain.SkillVersion{
		ID: "1", SkillID: pendingSkill.ID, Version: "1", CreatedAt: time.Now(),
		Name: "pending", Description: "pending", Directory: "pending",
		BlobKey: "skills/skill_pending/1.zip", State: domain.SkillVersionUploading, Initial: true,
	}
	if err := repo.BeginSkill(ctx, pendingSkill, pendingVersion); err != nil {
		t.Fatal(err)
	}
	blobs.objects[pendingVersion.BlobKey] = []byte("orphan")
	if err := service.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if _, present := repo.skills[pendingSkill.ID]; present {
		t.Fatal("initial incomplete Skill row remains")
	}
	if _, present := blobs.objects[pendingVersion.BlobKey]; present {
		t.Fatal("initial incomplete Skill archive remains")
	}
}

func TestSkillService_CleansFailedInitialUpload(t *testing.T) {
	repo := newMemorySkillRepository()
	blobs := newMemoryBlobStore()
	blobs.putErr = errors.New("object store unavailable")
	service := NewSkillService(repo, blobs, domain.NewSeqIDGen(), domain.FixedClock{})
	_, err := service.Create(context.Background(), SkillCreateInput{Files: []SkillUploadFile{{
		Filename: "safe-skill/SKILL.md",
		Body:     []byte("---\nname: safe-skill\ndescription: Handles safe work when requested.\n---\n"),
	}}})
	if err == nil {
		t.Fatal("failed blob upload returned nil")
	}
	if len(repo.skills) != 0 || len(repo.versions) != 0 {
		t.Fatalf("failed initial upload left metadata: skills=%v versions=%v", repo.skills, repo.versions)
	}
}

func TestSkillService_FailedUploadRetainsCleanupUntilStorageRecovers(t *testing.T) {
	for _, initial := range []bool{true, false} {
		name := "new_version"
		if initial {
			name = "initial_version"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			repo := newMemorySkillRepository()
			blobs := newMemoryBlobStore()
			service := NewSkillService(repo, blobs, domain.NewSeqIDGen(), domain.FixedClock{})
			files := []SkillUploadFile{{
				Filename: "safe-skill/SKILL.md",
				Body:     []byte("---\nname: safe-skill\ndescription: Handles safe work when requested.\n---\n"),
			}}
			var existing domain.Skill
			var err error
			if !initial {
				existing, err = service.Create(ctx, SkillCreateInput{Files: files})
				if err != nil {
					t.Fatal(err)
				}
			}
			blobs.putErrAfterCommit = errors.New("upload response lost after archive was stored")
			blobs.deleteErr = errors.New("archive deletion unavailable")
			if initial {
				_, err = service.Create(ctx, SkillCreateInput{Files: files})
			} else {
				_, err = service.CreateVersion(ctx, existing.ID, files)
			}
			if !errors.Is(err, blobs.putErrAfterCommit) {
				t.Fatalf("upload error = %v, want original upload error", err)
			}
			pending, err := repo.ListIncompleteVersions(ctx)
			if err != nil || len(pending) != 1 {
				t.Fatalf("cleanup intent after failed archive deletion = %+v, %v", pending, err)
			}
			if _, err := service.GetVersion(ctx, pending[0].SkillID, pending[0].Version); err == nil {
				t.Fatal("failed Version upload became visible")
			}
			restarted := NewSkillService(repo, blobs, domain.NewSeqIDGen(), domain.FixedClock{})
			if err := restarted.Reconcile(ctx); !errors.Is(err, blobs.deleteErr) {
				t.Fatalf("Reconcile during outage = %v, want deletion error", err)
			}
			blobs.deleteErr = nil
			if err := restarted.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile after recovery: %v", err)
			}
			if pending, err := repo.ListIncompleteVersions(ctx); err != nil || len(pending) != 0 {
				t.Fatalf("cleanup intents remain after recovery: %+v, %v", pending, err)
			}
			if initial {
				if len(repo.skills) != 0 || len(blobs.objects) != 0 {
					t.Fatalf("failed initial Skill remains: skills=%v objects=%v", repo.skills, blobs.objects)
				}
			} else {
				current, err := restarted.Get(ctx, existing.ID)
				if err != nil || current.LatestVersion != existing.LatestVersion || len(blobs.objects) != 1 {
					t.Fatalf("existing Skill changed after cleanup: %+v, %v; objects=%v", current, err, blobs.objects)
				}
				download, err := restarted.Download(ctx, existing.ID, existing.LatestVersion)
				if err != nil {
					t.Fatalf("download previous Version: %v", err)
				}
				archive, err := io.ReadAll(download.Body)
				_ = download.Body.Close()
				if err != nil || string(readZipFile(t, archive, "safe-skill/SKILL.md")) != string(files[0].Body) {
					t.Fatalf("previous Version bytes changed: %v", err)
				}
			}
		})
	}
}

type memorySkillRepository struct {
	memoryBlobCleanupRepository
	skills   map[string]domain.Skill
	versions map[string]map[string]domain.SkillVersion
}

func newMemorySkillRepository() *memorySkillRepository {
	return &memorySkillRepository{
		skills: make(map[string]domain.Skill), versions: make(map[string]map[string]domain.SkillVersion),
	}
}

func (r *memorySkillRepository) BeginSkill(_ context.Context, skill domain.Skill, version domain.SkillVersion) error {
	if _, exists := r.skills[skill.ID]; exists {
		return domain.Conflict("skill exists")
	}
	r.skills[skill.ID] = skill
	r.versions[skill.ID] = map[string]domain.SkillVersion{version.Version: version}
	return nil
}

func (r *memorySkillRepository) BeginVersion(_ context.Context, version domain.SkillVersion) error {
	if _, exists := r.skills[version.SkillID]; !exists {
		return domain.NotFound("skill not found")
	}
	r.versions[version.SkillID][version.Version] = version
	return nil
}

func (r *memorySkillRepository) CompleteVersion(
	_ context.Context, skillID, version, blobKey string, info BlobInfo,
) (domain.Skill, domain.SkillVersion, error) {
	item := r.versions[skillID][version]
	if item.BlobKey != blobKey {
		return domain.Skill{}, domain.SkillVersion{}, ErrUploadLeaseLost
	}
	item.State = domain.SkillVersionReady
	item.SizeBytes, item.ChecksumSHA256 = info.SizeBytes, info.ChecksumSHA256
	r.versions[skillID][version] = item
	skill := r.skills[skillID]
	skill.Ready, skill.LatestVersion, skill.UpdatedAt = true, version, item.CreatedAt
	r.skills[skillID] = skill
	return skill, item, nil
}

func (r *memorySkillRepository) GetSkill(_ context.Context, id string) (domain.Skill, error) {
	item, ok := r.skills[id]
	if !ok || !item.Ready {
		return domain.Skill{}, domain.NotFound("skill not found")
	}
	return item, nil
}

func (r *memorySkillRepository) ListSkills(_ context.Context, query SkillListQuery) (SkillListPage, error) {
	items := make([]domain.Skill, 0)
	for _, item := range r.skills {
		if item.Ready {
			items = append(items, item)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreatedAt.After(items[j].CreatedAt) })
	return SkillListPage{Skills: items}, nil
}

func (r *memorySkillRepository) GetVersion(_ context.Context, skillID, version string) (domain.SkillVersion, error) {
	item, ok := r.versions[skillID][version]
	if !ok || item.State != domain.SkillVersionReady {
		return domain.SkillVersion{}, domain.NotFound("Skill Version not found")
	}
	return item, nil
}

func (r *memorySkillRepository) ListVersions(
	_ context.Context, skillID string, _ SkillVersionListQuery,
) (SkillVersionListPage, error) {
	if _, ok := r.skills[skillID]; !ok {
		return SkillVersionListPage{}, domain.NotFound("skill not found")
	}
	items := make([]domain.SkillVersion, 0)
	for _, item := range r.versions[skillID] {
		if item.State == domain.SkillVersionReady {
			items = append(items, item)
		}
	}
	return SkillVersionListPage{Versions: items}, nil
}

func (r *memorySkillRepository) BeginDeleteVersion(
	_ context.Context, skillID, version string,
) (domain.SkillVersion, error) {
	item, ok := r.versions[skillID][version]
	if !ok || item.State != domain.SkillVersionReady {
		return domain.SkillVersion{}, domain.NotFound("Skill Version not found")
	}
	item.State = domain.SkillVersionDeleting
	r.versions[skillID][version] = item
	skill := r.skills[skillID]
	skill.LatestVersion = ""
	for candidate, value := range r.versions[skillID] {
		if value.State == domain.SkillVersionReady && candidate > skill.LatestVersion {
			skill.LatestVersion = candidate
		}
	}
	r.skills[skillID] = skill
	return item, nil
}

func (r *memorySkillRepository) RemoveIncompleteVersion(_ context.Context, skillID, version, blobKey string) error {
	if item, ok := r.versions[skillID][version]; ok && item.State != domain.SkillVersionReady && item.BlobKey == blobKey {
		delete(r.versions[skillID], version)
	}
	return nil
}

func (r *memorySkillRepository) ListIncompleteVersions(_ context.Context) ([]domain.SkillVersion, error) {
	var items []domain.SkillVersion
	for _, versions := range r.versions {
		for _, item := range versions {
			if item.State != domain.SkillVersionReady {
				items = append(items, item)
			}
		}
	}
	return items, nil
}

func (r *memorySkillRepository) RenewUpload(context.Context, string, string, string) error {
	return nil
}
func (r *memorySkillRepository) ReleaseUpload(context.Context, string, string, string, bool) error {
	return nil
}
func (r *memorySkillRepository) ClaimIncompleteVersion(_ context.Context, id, version string) (domain.SkillVersion, bool, error) {
	item, ok := r.versions[id][version]
	if !ok || item.State == domain.SkillVersionReady {
		return domain.SkillVersion{}, false, nil
	}
	item.State = domain.SkillVersionDeleting
	r.versions[id][version] = item
	return item, true, nil
}

func (r *memorySkillRepository) DeleteEmptySkill(_ context.Context, id string) error {
	if skill, ok := r.skills[id]; ok && !skill.Ready && len(r.versions[id]) == 0 {
		delete(r.skills, id)
		delete(r.versions, id)
	}
	return nil
}

func (r *memorySkillRepository) DeleteSkill(_ context.Context, id string) (domain.Skill, error) {
	item, ok := r.skills[id]
	if !ok || !item.Ready {
		return domain.Skill{}, domain.NotFound("skill not found")
	}
	if len(r.versions[id]) != 0 {
		return domain.Skill{}, domain.Validation("delete versions first")
	}
	delete(r.skills, id)
	delete(r.versions, id)
	return item, nil
}
