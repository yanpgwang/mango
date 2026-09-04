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
	defaultSessionSkillArchiveLimit int64 = 30_000_000
	defaultSessionSkillFileLimit          = 1000
)

// SessionSkillSetup owns the Skill directories downloaded for one Session.
// Cleanup removes only those directories, allowing a long-lived worker
// process or persistent Session volume to be reused without leaking inputs
// into unrelated work.
type SessionSkillSetup struct {
	directories []string
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
) (_ *SessionSkillSetup, retErr error) {
	if client == nil {
		return nil, errors.New("mango: PrepareSessionSkills client is required")
	}
	root, err := filepath.Abs(workdir)
	if err != nil {
		return nil, fmt.Errorf("mango: resolve Skill workdir: %w", err)
	}
	setup := &SessionSkillSetup{}
	defer func() {
		if retErr != nil {
			_ = setup.Cleanup()
		}
	}()

	type scope struct {
		root   string
		skills []SkillReferenceResponse
	}
	scopes := []scope{{root: filepath.Join(root, "skills"), skills: session.Agent.Skills}}
	if topology := session.Agent.Multiagent.SessionResolvedMultiagent; topology != nil {
		for _, member := range topology.Agents {
			agent := member.ManagedAgentThreadAgent
			if agent == nil || len(agent.Skills) == 0 {
				continue
			}
			scopes = append(scopes, scope{
				root:   sessionAgentSkillRoot(root, session.Agent.ID, session.Agent.Version, agent.ID, agent.Version),
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
			nameKey := current.root + "\x00" + version.Name
			if _, duplicate := seenNames[nameKey]; duplicate {
				return nil, fmt.Errorf("mango: Session has conflicting Skill name %q", version.Name)
			}
			seenNames[nameKey] = struct{}{}

			destination := filepath.Join(current.root, version.Name)
			if err := downloadAndExtractSessionSkill(
				ctx, client, resolved.SkillID, resolved.Version,
				version.Directory, destination,
			); err != nil {
				return nil, fmt.Errorf("mango: prepare Skill %s@%s: %w", resolved.SkillID, resolved.Version, err)
			}
			setup.directories = append(setup.directories, destination)
		}
	}
	return setup, nil
}

// Cleanup removes the per-Skill directories created by PrepareSessionSkills.
func (s *SessionSkillSetup) Cleanup() error {
	if s == nil {
		return nil
	}
	var errs []error
	for index := len(s.directories) - 1; index >= 0; index-- {
		if err := os.RemoveAll(s.directories[index]); err != nil {
			errs = append(errs, err)
		}
	}
	s.directories = nil
	return errors.Join(errs...)
}

func sessionAgentSkillRoot(workdir, primaryID string, primaryVersion int64, agentID string, agentVersion int64) string {
	root := filepath.Join(workdir, "skills")
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
	destination string,
) (retErr error) {
	download, err := client.DownloadSkillVersion(ctx, skillID, version)
	if err != nil {
		return err
	}
	defer download.Close() // extraction/download errors remain authoritative

	archive, err := os.CreateTemp("", "mango-skill-*.zip")
	if err != nil {
		return fmt.Errorf("create archive spool: %w", err)
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath) //nolint:errcheck // best-effort spool cleanup
	limited := &io.LimitedReader{R: download, N: defaultSessionSkillArchiveLimit}
	written, copyErr := io.Copy(archive, limited)
	closeErr := archive.Close()
	if copyErr != nil {
		return fmt.Errorf("download archive: %w", copyErr)
	}
	if closeErr != nil {
		return fmt.Errorf("flush archive: %w", closeErr)
	}
	if written >= defaultSessionSkillArchiveLimit || limited.N == 0 {
		return errors.New("Skill archive exceeds the 30 MB limit")
	}

	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create Skill root: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".mango-skill-")
	if err != nil {
		return fmt.Errorf("create Skill staging directory: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := extractSessionSkillArchive(ctx, archivePath, archiveRoot, staging); err != nil {
		return err
	}
	if err := os.RemoveAll(destination); err != nil {
		return fmt.Errorf("replace prior Skill directory: %w", err)
	}
	if err := os.Rename(staging, destination); err != nil {
		return fmt.Errorf("publish Skill directory: %w", err)
	}
	published = true
	return nil
}

func extractSessionSkillArchive(ctx context.Context, archivePath, archiveRoot, destination string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return errors.New("stored Skill archive is invalid")
	}
	defer reader.Close()
	if len(reader.File) == 0 || len(reader.File) > defaultSessionSkillFileLimit {
		return errors.New("stored Skill archive has an invalid file count")
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
		if entry.UncompressedSize64 >= uint64(defaultSessionSkillArchiveLimit) ||
			expanded >= defaultSessionSkillArchiveLimit-int64(entry.UncompressedSize64) {
			return errors.New("stored Skill archive exceeds the expanded-size limit")
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
		foundInstructions = foundInstructions || relative == "SKILL.md"
	}
	if !foundInstructions {
		return errors.New("stored Skill archive has no root SKILL.md")
	}
	return nil
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
