package mango

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultSessionSkillArchiveLimit   int64 = 30_000_000
	defaultSessionSkillFileLimit            = 1000
	defaultSessionSkillCountLimit           = 500
	defaultSessionSkillTotalByteLimit       = 500 << 20
	defaultSessionSkillTotalFileLimit       = 10_000
)

// SessionSkillSetup owns the complete Skill tree downloaded for one Session.
// Cleanup removes only that tree, allowing a long-lived worker process or
// persistent Session volume to be reused without leaking inputs into unrelated
// work.
type SessionSkillSetup struct {
	root string
}

type sessionSkillBudget struct {
	archives       int
	compressed     int64
	expanded       int64
	files          int
	archiveLimit   int64
	fileLimit      int
	countLimit     int
	totalByteLimit int64
	totalFileLimit int
}

func defaultSessionSkillBudget() *sessionSkillBudget {
	return &sessionSkillBudget{
		archiveLimit:   defaultSessionSkillArchiveLimit,
		fileLimit:      defaultSessionSkillFileLimit,
		countLimit:     defaultSessionSkillCountLimit,
		totalByteLimit: defaultSessionSkillTotalByteLimit,
		totalFileLimit: defaultSessionSkillTotalFileLimit,
	}
}

// PrepareSessionSkills downloads every immutable custom Skill pin in the
// Session snapshot before tool dispatch begins. Primary-Agent Skills use
// {workdir}/skills/<name>; roster Agents use stable, Agent-scoped directories
// below {workdir}/skills/.agents so equal names from different Agents cannot
// collide. Mango canonicalizes uploaded Skill bundles as zip archives.
func PrepareSessionSkills(
	ctx context.Context,
	client *Client,
	session Session,
	workdir string,
) (*SessionSkillSetup, error) {
	return prepareSessionSkills(ctx, client, session, workdir, defaultSessionSkillBudget())
}

func prepareSessionSkills(
	ctx context.Context,
	client *Client,
	session Session,
	workdir string,
	budget *sessionSkillBudget,
) (_ *SessionSkillSetup, retErr error) {
	if client == nil {
		return nil, errors.New("mango: PrepareSessionSkills client is required")
	}
	root, err := filepath.Abs(workdir)
	if err != nil {
		return nil, fmt.Errorf("mango: resolve Skill workdir: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("mango: resolve Skill workdir: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("mango: inspect Skill workdir: %w", err)
	}
	if !info.IsDir() {
		return nil, &os.PathError{
			Op:   "inspect Skill workdir",
			Path: root,
			Err:  errors.New("not a directory"),
		}
	}
	finalRoot := filepath.Join(root, "skills")
	stagingRoot, err := os.MkdirTemp(root, ".mango-skills-")
	if err != nil {
		return nil, fmt.Errorf("mango: create Session Skill staging directory: %w", err)
	}
	setup := &SessionSkillSetup{root: finalRoot}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stagingRoot)
		}
		if retErr != nil {
			_ = setup.Cleanup()
		}
	}()

	type scope struct {
		root   string
		skills []SkillReferenceResponse
	}
	scopes := []scope{{root: stagingRoot, skills: session.Agent.Skills}}
	if topology := session.Agent.Multiagent.SessionResolvedMultiagent; topology != nil {
		for _, member := range topology.Agents {
			agent := member.ManagedAgentThreadAgent
			if agent == nil || len(agent.Skills) == 0 {
				continue
			}
			scopes = append(scopes, scope{
				root:   sessionAgentSkillScopeRoot(stagingRoot, session.Agent.ID, session.Agent.Version, agent.ID, agent.Version),
				skills: agent.Skills,
			})
		}
	}

	seenPins := make(map[string]struct{})
	seenNames := make(map[string]struct{})
	for _, current := range scopes {
		for _, reference := range current.skills {
			resolved := reference.ResolvedSkillReference
			if resolved == nil || resolved.SkillID == "" || resolved.Version == "" {
				return nil, errors.New("mango: Session contains an unresolved custom Skill reference")
			}
			pin := current.root + "\x00" + resolved.SkillID + "\x00" + resolved.Version
			if _, duplicate := seenPins[pin]; duplicate {
				continue
			}
			seenPins[pin] = struct{}{}
			budget.archives++
			if budget.archives > budget.countLimit {
				return nil, errors.New("mango: Session custom Skills exceed the 500-bundle limit")
			}

			version, err := client.GetSkillVersion(ctx, resolved.SkillID, resolved.Version)
			if err != nil {
				return nil, fmt.Errorf("mango: retrieve Skill %s@%s: %w", resolved.SkillID, resolved.Version, err)
			}
			if version.SkillID != resolved.SkillID || version.Version != resolved.Version {
				return nil, fmt.Errorf("mango: Skill %s@%s metadata identity mismatch", resolved.SkillID, resolved.Version)
			}
			if !validSessionSkillName(version.Name) || !validSessionSkillDirectory(version.Directory, version.Name) {
				return nil, fmt.Errorf("mango: Skill %s@%s has unsafe runtime metadata", resolved.SkillID, resolved.Version)
			}
			if version.SizeBytes < 0 || !validSessionSkillChecksum(version.ChecksumSHA256) {
				return nil, fmt.Errorf("mango: Skill %s@%s has invalid integrity metadata", resolved.SkillID, resolved.Version)
			}
			nameKey := current.root + "\x00" + version.Name
			if _, duplicate := seenNames[nameKey]; duplicate {
				return nil, fmt.Errorf("mango: Session has conflicting Skill name %q", version.Name)
			}
			seenNames[nameKey] = struct{}{}

			destination := filepath.Join(current.root, version.Name)
			if err := downloadAndExtractSessionSkill(
				ctx, client, resolved.SkillID, resolved.Version,
				version.Directory, version.SizeBytes, version.ChecksumSHA256,
				destination, stagingRoot, budget,
			); err != nil {
				return nil, fmt.Errorf("mango: prepare Skill %s@%s: %w", resolved.SkillID, resolved.Version, err)
			}
		}
	}
	if budget.archives == 0 {
		if err := os.RemoveAll(finalRoot); err != nil {
			return nil, fmt.Errorf("mango: clear prior Session Skill tree: %w", err)
		}
		setup.root = ""
		return setup, nil
	}
	// Publish the complete immutable tree at one trusted boundary. A prior
	// Session can replace `skills` (or anything below it) with a symlink, but
	// RemoveAll never follows a symlink passed as its root. Staging every scope
	// under a fresh direct child of the canonical workdir avoids traversing any
	// path an earlier tool process controlled.
	if err := os.RemoveAll(finalRoot); err != nil {
		return nil, fmt.Errorf("mango: replace prior Session Skill tree: %w", err)
	}
	if err := os.Rename(stagingRoot, finalRoot); err != nil {
		return nil, fmt.Errorf("mango: publish Session Skill tree: %w", err)
	}
	published = true
	return setup, nil
}

// Cleanup removes the complete Session-owned Skill tree. RemoveAll unlinks a
// replacement root symlink without traversing its target.
func (s *SessionSkillSetup) Cleanup() error {
	if s == nil || s.root == "" {
		return nil
	}
	err := os.RemoveAll(s.root)
	s.root = ""
	return err
}

func sessionAgentSkillScopeRoot(root, primaryID string, primaryVersion int64, agentID string, agentVersion int64) string {
	if agentID == primaryID && agentVersion == primaryVersion {
		return root
	}
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", agentID, agentVersion)))
	return filepath.Join(root, ".agents", hex.EncodeToString(digest[:12]))
}

func downloadAndExtractSessionSkill(
	ctx context.Context,
	client *Client,
	skillID string,
	version string,
	archiveRoot string,
	expectedSize int64,
	expectedChecksum string,
	destination string,
	stagingRoot string,
	budget *sessionSkillBudget,
) (retErr error) {
	if budget.compressed > budget.totalByteLimit-expectedSize {
		return errors.New("Session custom Skills exceed the 500 MiB compressed-size limit")
	}
	download, err := client.DownloadSkillVersion(ctx, skillID, version)
	if err != nil {
		return err
	}
	defer download.Close() // extraction/download errors remain authoritative

	archive, err := os.CreateTemp(stagingRoot, ".mango-skill-*.zip")
	if err != nil {
		return fmt.Errorf("create archive spool: %w", err)
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath) //nolint:errcheck // best-effort spool cleanup
	limited := &io.LimitedReader{R: download, N: budget.archiveLimit + 1}
	hasher := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(archive, hasher), limited)
	closeErr := archive.Close()
	if copyErr != nil {
		return fmt.Errorf("download archive: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("flush archive: %w", closeErr)
	}
	if written > budget.archiveLimit {
		return errors.New("Skill archive exceeds the 30 MB limit")
	}
	if written != expectedSize {
		return fmt.Errorf("Skill archive size mismatch: received %d bytes, expected %d", written, expectedSize)
	}
	actualChecksum := hex.EncodeToString(hasher.Sum(nil))
	if actualChecksum != expectedChecksum {
		return errors.New("Skill archive checksum mismatch")
	}
	budget.compressed += written

	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create Skill root: %w", err)
	}
	if err := os.Mkdir(destination, 0o755); err != nil {
		return fmt.Errorf("create Skill directory: %w", err)
	}
	if err := extractSessionSkillArchive(ctx, archivePath, archiveRoot, destination, budget); err != nil {
		return err
	}
	return nil
}

func extractSessionSkillArchive(
	ctx context.Context,
	archivePath, archiveRoot, destination string,
	budget *sessionSkillBudget,
) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return errors.New("stored Skill archive is invalid")
	}
	defer reader.Close()
	if len(reader.File) == 0 || len(reader.File) > budget.fileLimit {
		return errors.New("stored Skill archive has an invalid file count")
	}
	if budget.files > budget.totalFileLimit-len(reader.File) {
		return errors.New("Session custom Skills exceed the 10000-file limit")
	}
	seen := make(map[string]struct{}, len(reader.File))
	var expanded int64
	foundInstructions := false
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := sessionSkillRelativePath(entry.Name, archiveRoot)
		if err != nil {
			return err
		}
		if _, duplicate := seen[relative]; duplicate {
			return errors.New("stored Skill archive contains duplicate paths")
		}
		seen[relative] = struct{}{}
		if entry.FileInfo().IsDir() || !entry.Mode().IsRegular() {
			return errors.New("stored Skill archive contains a non-regular file")
		}
		if entry.UncompressedSize64 > uint64(budget.archiveLimit) ||
			expanded > budget.archiveLimit-int64(entry.UncompressedSize64) {
			return errors.New("stored Skill archive exceeds the expanded-size limit")
		}
		if entry.UncompressedSize64 > uint64(budget.totalByteLimit) ||
			budget.expanded > budget.totalByteLimit-int64(entry.UncompressedSize64) {
			return errors.New("Session custom Skills exceed the 500 MiB expanded-size limit")
		}
		target := filepath.Join(destination, filepath.FromSlash(relative))
		if target == destination || !strings.HasPrefix(target, destination+string(os.PathSeparator)) {
			return errors.New("stored Skill archive contains an unsafe path")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("create Skill directory: %w", err)
		}
		input, err := entry.Open()
		if err != nil {
			return errors.New("stored Skill archive is invalid")
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o555)
		if err != nil {
			input.Close() //nolint:errcheck // create error is authoritative
			return fmt.Errorf("create Skill file: %w", err)
		}
		written, copyErr := io.Copy(output, io.LimitReader(input, int64(entry.UncompressedSize64)+1))
		inputCloseErr := input.Close()
		outputCloseErr := output.Close()
		if copyErr != nil || inputCloseErr != nil || outputCloseErr != nil || written != int64(entry.UncompressedSize64) {
			return errors.New("stored Skill archive is invalid")
		}
		expanded += written
		budget.expanded += written
		foundInstructions = foundInstructions || relative == "SKILL.md"
	}
	if !foundInstructions {
		return errors.New("stored Skill archive has no root SKILL.md")
	}
	budget.files += len(reader.File)
	return nil
}

func validSessionSkillChecksum(checksum string) bool {
	if len(checksum) != sha256.Size*2 {
		return false
	}
	for _, character := range checksum {
		if (character >= '0' && character <= '9') ||
			(character >= 'a' && character <= 'f') {
			continue
		}
		return false
	}
	return true
}

func sessionSkillRelativePath(raw, archiveRoot string) (string, error) {
	if raw == "" || strings.Contains(raw, "\\") || filepath.IsAbs(raw) {
		return "", errors.New("stored Skill archive contains an unsafe path")
	}
	prefix := archiveRoot + "/"
	if !strings.HasPrefix(raw, prefix) {
		return "", errors.New("stored Skill archive escaped its canonical root")
	}
	relative := strings.TrimPrefix(raw, prefix)
	if relative == "" || filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative))) != relative ||
		strings.HasPrefix(relative, "../") || strings.ContainsRune(relative, '\x00') {
		return "", errors.New("stored Skill archive contains an unsafe path")
	}
	return relative, nil
}

func validSessionSkillName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, character := range name {
		if (character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validSessionSkillDirectory(directory, name string) bool {
	if directory == "" || len(directory) > 64 || strings.ContainsAny(directory, "/\\") {
		return false
	}
	for _, character := range directory {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' {
			continue
		}
		return false
	}
	return strings.ReplaceAll(strings.ToLower(directory), "_", "-") == name
}
