package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"

	"github.com/yanpgwang/mango/internal/domain"
)

// SessionSkillMountRepository exposes the immutable Version metadata retained
// by a Session's relational pins.
type SessionSkillMountRepository interface {
	SessionSkillsForRuntime(context.Context, string) ([]domain.SkillVersion, error)
}

type SessionThreadSkillMountRepository interface {
	SessionThreadSkillRuntime(
		context.Context,
		string,
		string,
	) (domain.SkillRuntime, error)
}

// SessionSkillMaterializer validates and reads immutable custom Skill archives
// for model-context injection. The external worker independently prepares the
// same pinned bundles in its own workspace.
type SessionSkillMaterializer struct {
	skills SessionSkillMountRepository
	blobs  BlobStore
}

func NewSessionSkillMaterializer(
	skills SessionSkillMountRepository,
	blobs BlobStore,
) *SessionSkillMaterializer {
	return &SessionSkillMaterializer{skills: skills, blobs: blobs}
}

// SupportsSkillRuntime reports whether immutable Skill instructions can load.
func (m *SessionSkillMaterializer) SupportsSkillRuntime() bool {
	return m != nil && m.skills != nil && m.blobs != nil
}

// LoadSkillInstructions reads the immutable SKILL.md directly from Mango's
// canonical bundle. The orchestration layer owns the Agent loop, so activating
// a Skill must not require
// reverse access to the worker's filesystem. The worker independently
// materializes the same pinned bundle for later read/bash access to supporting
// files.
func (m *SessionSkillMaterializer) LoadSkillInstructions(
	ctx context.Context,
	version domain.SkillVersion,
) ([]byte, error) {
	if !m.SupportsSkillRuntime() {
		return nil, errors.New("session Skill loader is not configured")
	}
	body, err := m.blobs.Open(ctx, version.BlobKey)
	if err != nil {
		return nil, err
	}
	defer body.Close() //nolint:errcheck // the read/validation error below remains authoritative

	limited := &io.LimitedReader{R: body, N: version.SizeBytes + 1}
	archive, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("session Skill: read archive: %w", err)
	}
	if int64(len(archive)) != version.SizeBytes {
		return nil, domain.Validation(
			"session Skill: stored archive size does not match its immutable metadata",
		)
	}
	digest := sha256.Sum256(archive)
	if hex.EncodeToString(digest[:]) != version.ChecksumSHA256 {
		return nil, domain.Validation(
			"session Skill: stored archive checksum does not match its immutable metadata",
		)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(reader.File) == 0 || len(reader.File) > MaxSkillFiles {
		return nil, domain.Validation("session Skill: stored archive is invalid")
	}
	want := path.Join(version.Directory, "SKILL.md")
	for _, entry := range reader.File {
		if entry.Name != want {
			continue
		}
		if !entry.Mode().IsRegular() || entry.UncompressedSize64 >= uint64(MaxSkillUploadBytes) {
			return nil, domain.Validation("session Skill: stored SKILL.md is invalid")
		}
		opened, err := entry.Open()
		if err != nil {
			return nil, domain.Validation("session Skill: stored archive is invalid")
		}
		instructions, readErr := io.ReadAll(io.LimitReader(opened, MaxSkillUploadBytes))
		closeErr := opened.Close()
		if readErr != nil || closeErr != nil || int64(len(instructions)) != int64(entry.UncompressedSize64) {
			return nil, domain.Validation("session Skill: stored SKILL.md is invalid")
		}
		return instructions, nil
	}
	return nil, domain.Validation("session Skill: stored archive has no root SKILL.md")
}
