package app

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/sandbox"
)

type staticSessionSkillRepository struct {
	versions []domain.SkillVersion
	runtime  domain.SkillRuntime
}

func (r staticSessionSkillRepository) SessionThreadSkillRuntime(
	context.Context,
	string,
	string,
) (domain.SkillRuntime, error) {
	runtime := r.runtime
	runtime.Versions = append([]domain.SkillVersion(nil), runtime.Versions...)
	return runtime, nil
}

func (r staticSessionSkillRepository) SessionSkillsForRuntime(
	context.Context,
	string,
) ([]domain.SkillVersion, error) {
	return append([]domain.SkillVersion(nil), r.versions...), nil
}

type trackingSkillSandbox struct {
	sandbox.Sandbox
	mount   sandbox.ReadOnlySkillMount
	archive []byte
}

func (s *trackingSkillSandbox) HasReadOnlySkill(
	_ context.Context,
	mount sandbox.ReadOnlySkillMount,
) (bool, error) {
	return s.mount == mount && len(s.archive) > 0, nil
}

func (s *trackingSkillSandbox) ImportReadOnlySkill(
	_ context.Context,
	mount sandbox.ReadOnlySkillMount,
	archive io.Reader,
) error {
	body, err := io.ReadAll(archive)
	if err != nil {
		return err
	}
	s.mount = mount
	s.archive = body
	return nil
}

func TestSessionSkillMaterializerUsesPinnedMetadataAndIsIdempotent(t *testing.T) {
	bundle, err := prepareSkillBundle([]SkillUploadFile{{
		Filename: "reports/SKILL.md",
		Body:     []byte("---\nname: reports\ndescription: Analyze reports\n---\nRead inputs.\n"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	info := ComputeBlobInfo(bundle.Archive)
	version := domain.SkillVersion{
		SkillID: "skill_reports", Version: "100", Name: bundle.Name,
		Description: bundle.Description, Directory: bundle.Directory,
		BlobKey: "skills/skill_reports/100.zip", SizeBytes: info.SizeBytes,
		UncompressedSizeBytes: bundle.UncompressedSizeBytes,
		ChecksumSHA256:        info.ChecksumSHA256, State: domain.SkillVersionReady,
	}
	blobs := newMemoryBlobStore()
	blobs.objects[version.BlobKey] = append([]byte(nil), bundle.Archive...)
	box := &trackingSkillSandbox{}
	materializer := NewSessionSkillMaterializer(
		staticSessionSkillRepository{versions: []domain.SkillVersion{version}}, blobs,
	)
	if err := materializer.Reconcile(context.Background(), "sesn_1", box); err != nil {
		t.Fatal(err)
	}
	if box.mount.Name != "reports" || box.mount.ArchiveRoot != "reports" ||
		box.mount.Identity != "skill_reports@100" ||
		!bytes.Equal(box.archive, bundle.Archive) {
		t.Fatalf("materialized Skill = mount=%+v archive=%d bytes", box.mount, len(box.archive))
	}
	blobs.objects[version.BlobKey] = []byte("must not reopen")
	if err := materializer.Reconcile(context.Background(), "sesn_1", box); err != nil {
		t.Fatalf("idempotent reconcile: %v", err)
	}
	if !bytes.Equal(box.archive, bundle.Archive) {
		t.Fatal("idempotent reconcile replaced a present Skill")
	}
}

func TestSessionSkillMaterializerLoadsInstructionsFromImmutableBundle(t *testing.T) {
	instructions := []byte("---\nname: reports\ndescription: Analyze reports\n---\nRead inputs.\n")
	bundle, err := prepareSkillBundle([]SkillUploadFile{{
		Filename: "reports/SKILL.md", Body: instructions,
	}, {
		Filename: "reports/scripts/run.sh", Body: []byte("#!/bin/sh\n"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	info := ComputeBlobInfo(bundle.Archive)
	version := domain.SkillVersion{
		SkillID: "skill_reports", Version: "100", Name: bundle.Name,
		Directory: bundle.Directory, BlobKey: "skills/skill_reports/100.zip",
		SizeBytes: info.SizeBytes, ChecksumSHA256: info.ChecksumSHA256,
	}
	blobs := newMemoryBlobStore()
	blobs.objects[version.BlobKey] = append([]byte(nil), bundle.Archive...)
	materializer := NewSessionSkillMaterializer(staticSessionSkillRepository{}, blobs)

	loaded, err := materializer.LoadSkillInstructions(context.Background(), version)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, instructions) {
		t.Fatalf("instructions = %q", loaded)
	}
	composed := NewSessionRuntimeMaterializer(nil, materializer)
	loaded, err = composed.LoadSkillInstructions(context.Background(), version)
	if err != nil || !bytes.Equal(loaded, instructions) {
		t.Fatalf("composed instructions = %q, %v", loaded, err)
	}

	version.ChecksumSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := materializer.LoadSkillInstructions(context.Background(), version); !sandbox.IsPermanent(err) {
		t.Fatalf("corrupt metadata error = %v, want permanent", err)
	}
}

func TestSessionSkillMaterializerRejectsUnsupportedSandboxPermanently(t *testing.T) {
	version := domain.SkillVersion{
		SkillID: "skill_reports", Version: "100", Name: "reports",
		Directory: "reports", SizeBytes: 1, UncompressedSizeBytes: 1,
		ChecksumSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}
	materializer := NewSessionSkillMaterializer(
		staticSessionSkillRepository{versions: []domain.SkillVersion{version}},
		newMemoryBlobStore(),
	)
	err := materializer.Reconcile(
		context.Background(), "sesn_1", struct{ sandbox.Sandbox }{},
	)
	if !sandbox.IsPermanent(err) {
		t.Fatalf("Reconcile error = %v, want permanent", err)
	}
}

func TestSessionSkillMaterializerUsesThreadAgentRuntimePath(t *testing.T) {
	bundle, err := prepareSkillBundle([]SkillUploadFile{{
		Filename: "reports/SKILL.md",
		Body: []byte(
			"---\nname: reports\ndescription: Analyze reports\n---\nRead inputs.\n",
		),
	}})
	if err != nil {
		t.Fatal(err)
	}
	info := ComputeBlobInfo(bundle.Archive)
	version := domain.SkillVersion{
		SkillID: "skill_reports", Version: "200", Name: bundle.Name,
		Description: bundle.Description, Directory: bundle.Directory,
		BlobKey: "skills/skill_reports/200.zip", SizeBytes: info.SizeBytes,
		UncompressedSizeBytes: bundle.UncompressedSizeBytes,
		ChecksumSHA256:        info.ChecksumSHA256, State: domain.SkillVersionReady,
	}
	root := domain.SessionSkillsRoot + "/.agents/0123456789abcdef01234567"
	blobs := newMemoryBlobStore()
	blobs.objects[version.BlobKey] = append([]byte(nil), bundle.Archive...)
	box := &trackingSkillSandbox{}
	materializer := NewSessionSkillMaterializer(
		staticSessionSkillRepository{runtime: domain.SkillRuntime{
			Root: root, Versions: []domain.SkillVersion{version},
		}},
		blobs,
	)
	if err := materializer.ReconcileThread(
		context.Background(), "sesn_1", "sthr_child", box,
	); err != nil {
		t.Fatal(err)
	}
	if box.mount.RuntimePath != root+"/reports" ||
		box.mount.Name != "reports" {
		t.Fatalf("child Skill mount = %+v", box.mount)
	}
}
