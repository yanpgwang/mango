package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"time"
)

// Skill is the stable identity for a custom Skill. LatestVersion is empty when
// every immutable Version has been deleted.
type Skill struct {
	ID            string
	CreatedAt     time.Time
	UpdatedAt     time.Time
	DisplayTitle  string
	LatestVersion string
	Source        string
	TitleExplicit bool
	Ready         bool
}

// SkillVersionState is internal lifecycle state around object-store I/O. Only
// ready Versions cross the public API boundary.
type SkillVersionState string

const (
	SkillVersionUploading SkillVersionState = "uploading"
	SkillVersionReady     SkillVersionState = "ready"
	SkillVersionDeleting  SkillVersionState = "deleting"
)

// SkillVersion is immutable public metadata plus the private archive pointer
// and reconciliation state needed to keep PostgreSQL and object storage in
// agreement.
type SkillVersion struct {
	ID          string
	SkillID     string
	Version     string
	CreatedAt   time.Time
	Description string
	Directory   string
	Name        string
	BlobKey     string
	SizeBytes   int64
	// UncompressedSizeBytes is the exact byte footprint of the validated
	// canonical bundle before zip compression, or UnknownSkillUncompressedSize
	// for Versions created before this metadata existed. Runtime admission uses
	// it to bound per-Session staging independently of compression ratio.
	UncompressedSizeBytes int64
	ChecksumSHA256        string
	State                 SkillVersionState
	Initial               bool
}

// SkillRuntime is the immutable custom Skill surface selected for one Session
// Agent execution scope. Multiple Threads running the same resolved Agent may
// share this bundle; their conversation context remains independent.
type SkillRuntime struct {
	Root     string
	Versions []SkillVersion
}

// SessionAgentSkillRoot preserves the Claude Code-compatible flat path for the
// coordinator and self copies. External roster Agents use a stable opaque
// namespace so equal Skill names with different Versions cannot collide in the
// shared Session sandbox.
func SessionAgentSkillRoot(
	sessionAgentID string,
	sessionAgentVersion int,
	agent Agent,
) string {
	if agent.ID == sessionAgentID && agent.Version == sessionAgentVersion {
		return SessionSkillsRoot
	}
	identity := agent.ID + "\x00" + strconv.Itoa(agent.Version)
	digest := sha256.Sum256([]byte(identity))
	return SessionSkillsRoot + "/.agents/" + hex.EncodeToString(digest[:12])
}

func (r SkillRuntime) SkillPath(name string) string {
	return r.Root + "/" + name
}

const (
	// SessionSkillsRoot is the provider-independent runtime directory containing
	// immutable-source custom Skills. Each Skill is re-rooted beneath its
	// validated frontmatter name, independent of the uploaded archive
	// directory's original casing.
	SessionSkillsRoot = "/workspace/skills"

	// SessionSkillsRelativeRoot is the model-visible root for self-hosted
	// workers. Their concrete Workdir is launcher-owned and intentionally absent
	// from the Environment API, so the control plane must describe Skill files
	// relative to that root rather than assume /workspace.
	SessionSkillsRelativeRoot = "skills"

	// UnknownSkillUncompressedSize marks Versions created before the runtime
	// started persisting exact expanded archive sizes. Their archive checksum is
	// still authoritative; extraction applies the normal per-bundle upper bound.
	UnknownSkillUncompressedSize int64 = -1
)
