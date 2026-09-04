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
	"github.com/yanpgwang/mango/internal/sandbox"
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

// SessionSkillMaterializer converges canonical custom Skill archives into the
// provider's read-only runtime tree. Version pins are immutable for a Session,
// so reconciliation only needs idempotent presence/repair, not per-Skill delete.
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

// SupportsSkillRuntime lets orchestration distinguish a Skill-aware
// reconciler from a File-only resource reconciler before advertising the
// private Skill dispatcher to the model.
func (m *SessionSkillMaterializer) SupportsSkillRuntime() bool {
	return m != nil && m.skills != nil && m.blobs != nil
}

// LoadSkillInstructions reads the immutable SKILL.md directly from Mango's
// canonical bundle. The orchestration layer owns the Agent loop for both
// managed and self-hosted execution, so activating a Skill must not require
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
		return nil, sandbox.Permanent(errors.New(
			"session Skill: stored archive size does not match its immutable metadata",
		))
	}
	digest := sha256.Sum256(archive)
	if hex.EncodeToString(digest[:]) != version.ChecksumSHA256 {
		return nil, sandbox.Permanent(errors.New(
			"session Skill: stored archive checksum does not match its immutable metadata",
		))
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil || len(reader.File) == 0 || len(reader.File) > MaxSkillFiles {
		return nil, sandbox.Permanent(errors.New("session Skill: stored archive is invalid"))
	}
	want := path.Join(version.Directory, "SKILL.md")
	for _, entry := range reader.File {
		if entry.Name != want {
			continue
		}
		if !entry.Mode().IsRegular() || entry.UncompressedSize64 >= uint64(MaxSkillUploadBytes) {
			return nil, sandbox.Permanent(errors.New("session Skill: stored SKILL.md is invalid"))
		}
		opened, err := entry.Open()
		if err != nil {
			return nil, sandbox.Permanent(errors.New("session Skill: stored archive is invalid"))
		}
		instructions, readErr := io.ReadAll(io.LimitReader(opened, MaxSkillUploadBytes))
		closeErr := opened.Close()
		if readErr != nil || closeErr != nil || int64(len(instructions)) != int64(entry.UncompressedSize64) {
			return nil, sandbox.Permanent(errors.New("session Skill: stored SKILL.md is invalid"))
		}
		return instructions, nil
	}
	return nil, sandbox.Permanent(errors.New("session Skill: stored archive has no root SKILL.md"))
}

func (m *SessionSkillMaterializer) Reconcile(
	ctx context.Context,
	sessionID string,
	box sandbox.Sandbox,
) error {
	versions, err := m.skills.SessionSkillsForRuntime(ctx, sessionID)
	if err != nil {
		return err
	}
	return m.reconcileRuntime(ctx, sessionID, domain.SkillRuntime{
		Root: domain.SessionSkillsRoot, Versions: versions,
	}, box)
}

// ReconcileThread materializes only the Skill bundle selected by the Thread's
// resolved Agent execution scope. Session Files and Memory remain shared.
func (m *SessionSkillMaterializer) ReconcileThread(
	ctx context.Context,
	sessionID string,
	threadID string,
	box sandbox.Sandbox,
) error {
	source, ok := m.skills.(SessionThreadSkillMountRepository)
	if !ok {
		return sandbox.Permanent(fmt.Errorf(
			"sandbox: Thread Skill runtime metadata is unavailable for Session %s",
			sessionID,
		))
	}
	runtime, err := source.SessionThreadSkillRuntime(ctx, sessionID, threadID)
	if err != nil {
		return err
	}
	return m.reconcileRuntime(ctx, sessionID, runtime, box)
}

func (m *SessionSkillMaterializer) reconcileRuntime(
	ctx context.Context,
	sessionID string,
	runtime domain.SkillRuntime,
	box sandbox.Sandbox,
) error {
	if len(runtime.Versions) == 0 {
		return nil
	}
	if runtime.Root == "" {
		return sandbox.Permanent(fmt.Errorf(
			"sandbox: Session %s Skill runtime root is missing",
			sessionID,
		))
	}
	mounter, supported := box.(sandbox.SkillBundleSandbox)
	if !supported {
		return sandbox.Permanent(fmt.Errorf(
			"sandbox: provider cannot materialize custom Skills for Session %s",
			sessionID,
		))
	}
	seenNames := make(map[string]struct{}, len(runtime.Versions))
	var expandedBytes int64
	for _, version := range runtime.Versions {
		if _, exists := seenNames[version.Name]; exists {
			return sandbox.Permanent(fmt.Errorf(
				"sandbox: Session %s has conflicting Skill runtime name %q",
				sessionID, version.Name,
			))
		}
		seenNames[version.Name] = struct{}{}
		expanded, valid := SkillExpandedBudgetBytes(version.UncompressedSizeBytes)
		if !valid || expanded > MaxSessionSkillBytes-expandedBytes {
			return sandbox.Permanent(fmt.Errorf(
				"sandbox: Session %s Skills exceed the expanded-size limit",
				sessionID,
			))
		}
		expandedBytes += expanded
		mount := sandbox.ReadOnlySkillMount{
			Identity:              version.SkillID + "@" + version.Version,
			Name:                  version.Name,
			RuntimePath:           runtime.SkillPath(version.Name),
			ArchiveRoot:           version.Directory,
			SizeBytes:             version.SizeBytes,
			UncompressedSizeBytes: version.UncompressedSizeBytes,
			ChecksumSHA256:        version.ChecksumSHA256,
		}
		present, err := mounter.HasReadOnlySkill(ctx, mount)
		if err != nil {
			return err
		}
		if present {
			continue
		}
		body, err := m.blobs.Open(ctx, version.BlobKey)
		if err != nil {
			return err
		}
		importErr := mounter.ImportReadOnlySkill(ctx, mount, body)
		closeErr := body.Close()
		if importErr != nil {
			return importErr
		}
		if closeErr != nil {
			return fmt.Errorf("session Skill: close archive body: %w", closeErr)
		}
	}
	return nil
}

// SessionRuntimeMaterializer composes File Resource and custom Skill
// reconciliation behind Temporal's single pre-tool hook.
type SessionRuntimeMaterializer struct {
	files   *SessionResourceMaterializer
	skills  *SessionSkillMaterializer
	memory  *SessionMemoryMaterializer
	outputs *SessionOutputPublisher
}

func NewSessionRuntimeMaterializer(
	files *SessionResourceMaterializer,
	skills *SessionSkillMaterializer,
	memory ...*SessionMemoryMaterializer,
) *SessionRuntimeMaterializer {
	materializer := &SessionRuntimeMaterializer{files: files, skills: skills}
	if len(memory) > 0 {
		materializer.memory = memory[0]
	}
	return materializer
}

func (m *SessionRuntimeMaterializer) WithSessionOutputPublisher(
	outputs *SessionOutputPublisher,
) *SessionRuntimeMaterializer {
	m.outputs = outputs
	return m
}

func (m *SessionRuntimeMaterializer) SupportsSkillRuntime() bool {
	return m != nil && m.skills != nil && m.skills.SupportsSkillRuntime()
}

// LoadSkillInstructions preserves the control-plane instruction loader through
// the composed runtime resource boundary used by Temporal.
func (m *SessionRuntimeMaterializer) LoadSkillInstructions(
	ctx context.Context,
	version domain.SkillVersion,
) ([]byte, error) {
	if m == nil || m.skills == nil {
		return nil, errors.New("session Skill loader is not configured")
	}
	return m.skills.LoadSkillInstructions(ctx, version)
}

func (m *SessionRuntimeMaterializer) SupportsSessionOutputs() bool {
	return m != nil && m.outputs != nil
}

func (m *SessionRuntimeMaterializer) PublishSessionOutputs(
	ctx context.Context,
	sessionID string,
	box sandbox.Sandbox,
) error {
	if m == nil || m.outputs == nil {
		return sandbox.Permanent(fmt.Errorf(
			"session output publication is not configured",
		))
	}
	return m.outputs.Publish(ctx, sessionID, box)
}

func (m *SessionRuntimeMaterializer) Reconcile(
	ctx context.Context,
	sessionID string,
	box sandbox.Sandbox,
) error {
	if m.files != nil {
		if err := m.files.Reconcile(ctx, sessionID, box); err != nil {
			return err
		}
	}
	if m.skills != nil {
		if err := m.skills.Reconcile(ctx, sessionID, box); err != nil {
			return err
		}
	}
	if m.memory != nil {
		return m.memory.Reconcile(ctx, sessionID, box)
	}
	return nil
}

// ReconcileThread keeps Session-shared resources converged while selecting
// custom Skills from the current Thread's resolved Agent scope.
func (m *SessionRuntimeMaterializer) ReconcileThread(
	ctx context.Context,
	sessionID string,
	threadID string,
	box sandbox.Sandbox,
) error {
	if m.files != nil {
		if err := m.files.Reconcile(ctx, sessionID, box); err != nil {
			return err
		}
	}
	if m.skills != nil {
		if err := m.skills.ReconcileThread(ctx, sessionID, threadID, box); err != nil {
			return err
		}
	}
	if m.memory != nil {
		return m.memory.Reconcile(ctx, sessionID, box)
	}
	return nil
}

func (m *SessionRuntimeMaterializer) Writeback(
	ctx context.Context,
	sessionID string,
	box sandbox.Sandbox,
) error {
	if m == nil || m.memory == nil {
		return nil
	}
	return m.memory.Writeback(ctx, sessionID, box)
}

func (m *SessionRuntimeMaterializer) WritebackForRelease(
	ctx context.Context,
	sessionID string,
	box sandbox.Sandbox,
) error {
	if m == nil || m.memory == nil {
		return nil
	}
	return m.memory.WritebackForRelease(ctx, sessionID, box)
}

func (m *SessionRuntimeMaterializer) MemoryStoreMountsForRelease(
	ctx context.Context,
	sessionID string,
) ([]sandbox.MemoryStoreMount, error) {
	if m == nil || m.memory == nil {
		return nil, nil
	}
	return m.memory.MemoryStoreMountsForRelease(ctx, sessionID)
}

func (m *SessionRuntimeMaterializer) CleanupSession(
	ctx context.Context,
	sessionID string,
) error {
	if m.outputs != nil {
		if err := m.outputs.CleanupSession(ctx, sessionID); err != nil {
			return err
		}
	}
	if m.files != nil {
		if err := m.files.CleanupSession(ctx, sessionID); err != nil {
			return err
		}
	}
	if m.memory != nil {
		return m.memory.CleanupSession(ctx, sessionID)
	}
	return nil
}
