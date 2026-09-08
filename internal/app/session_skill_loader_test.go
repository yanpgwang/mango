package app

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
)

type emptySessionSkillRepository struct{}

func (emptySessionSkillRepository) SessionSkillsForRuntime(
	context.Context,
	string,
) ([]domain.SkillVersion, error) {
	return nil, nil
}

func TestSessionSkillMaterializerLoadsValidatedInstructions(t *testing.T) {
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
	loader := NewSessionSkillMaterializer(emptySessionSkillRepository{}, blobs)

	loaded, err := loader.LoadSkillInstructions(context.Background(), version)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(loaded, instructions) {
		t.Fatalf("instructions = %q", loaded)
	}

	version.ChecksumSHA256 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	_, err = loader.LoadSkillInstructions(context.Background(), version)
	var domainErr *domain.DomainError
	if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindValidation {
		t.Fatalf("corrupt metadata error = %v, want validation", err)
	}
}
