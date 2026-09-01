package sandbox

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/yanpgwang/mango/internal/domain"
)

const (
	maxSkillArchiveBytes int64 = 30_000_000
	maxSkillFiles        int   = 1000
)

func validateReadOnlySkillMount(mount ReadOnlySkillMount) error {
	if err := validateResourceIdentity(mount.Identity); err != nil {
		return err
	}
	if !validSkillRuntimeName(mount.Name) {
		return errors.New("sandbox: Skill runtime name is invalid")
	}
	if !validSkillRuntimePath(resolvedSkillRuntimePath(mount), mount.Name) {
		return errors.New("sandbox: Skill runtime path is invalid")
	}
	if !validSkillArchiveRoot(mount.ArchiveRoot, mount.Name) {
		return errors.New("sandbox: Skill archive root is invalid")
	}
	if mount.SizeBytes < 0 || mount.SizeBytes >= maxSkillArchiveBytes {
		return errors.New("sandbox: Skill archive size is invalid")
	}
	if mount.UncompressedSizeBytes != domain.UnknownSkillUncompressedSize &&
		(mount.UncompressedSizeBytes < 0 || mount.UncompressedSizeBytes >= maxSkillArchiveBytes) {
		return errors.New("sandbox: Skill expanded size is invalid")
	}
	decoded, err := hex.DecodeString(mount.ChecksumSHA256)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(mount.ChecksumSHA256) != mount.ChecksumSHA256 {
		return errors.New("sandbox: Skill checksum must be a lowercase SHA-256 digest")
	}
	return nil
}

func resolvedSkillRuntimePath(mount ReadOnlySkillMount) string {
	if mount.RuntimePath != "" {
		return mount.RuntimePath
	}
	return domain.SessionSkillsRoot + "/" + mount.Name
}

func validSkillRuntimePath(runtimePath, name string) bool {
	prefix := domain.SessionSkillsRoot + "/"
	if !strings.HasPrefix(runtimePath, prefix) || path.Clean(runtimePath) != runtimePath ||
		strings.ContainsRune(runtimePath, '\x00') {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(runtimePath, prefix), "/")
	if len(parts) == 1 {
		return parts[0] == name
	}
	if len(parts) != 3 || parts[0] != ".agents" || parts[2] != name || len(parts[1]) != 24 {
		return false
	}
	_, err := hex.DecodeString(parts[1])
	return err == nil
}

func validSkillRuntimeName(name string) bool {
	if name == "" || len(name) > 64 || !utf8.ValidString(name) {
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

func validSkillArchiveRoot(root, name string) bool {
	if root == "" || len(root) > 64 || !utf8.ValidString(root) || strings.ContainsAny(root, "/\\") {
		return false
	}
	for _, character := range root {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' {
			continue
		}
		return false
	}
	return strings.ReplaceAll(strings.ToLower(root), "_", "-") == name
}

func skillMarker(mount ReadOnlySkillMount) string {
	return mount.Identity + "\n" + mount.Name + "\n" +
		resolvedSkillRuntimePath(mount) + "\n" + mount.ArchiveRoot + "\n" +
		strconv.FormatInt(mount.SizeBytes, 10) + "\n" +
		strconv.FormatInt(mount.UncompressedSizeBytes, 10) + "\n" +
		mount.ChecksumSHA256 + "\n"
}

func storeVerifiedSkillArchive(
	archive *os.File,
	mount ReadOnlySkillMount,
	content io.Reader,
) (int64, error) {
	hash := sha256.New()
	limited := &io.LimitedReader{R: content, N: mount.SizeBytes + 1}
	written, err := io.Copy(io.MultiWriter(archive, hash), limited)
	if err != nil {
		return 0, fmt.Errorf("sandbox: stream Skill archive: %w", err)
	}
	if written != mount.SizeBytes {
		return 0, fmt.Errorf(
			"sandbox: Skill archive size mismatch: received %d bytes, expected %d",
			written, mount.SizeBytes,
		)
	}
	if checksum := hex.EncodeToString(hash.Sum(nil)); checksum != mount.ChecksumSHA256 {
		return 0, errors.New("sandbox: Skill archive checksum mismatch")
	}
	if err := archive.Sync(); err != nil {
		return 0, fmt.Errorf("sandbox: sync Skill archive: %w", err)
	}
	return written, nil
}

func extractCanonicalSkill(
	ctx context.Context,
	archive *os.File,
	size int64,
	staging string,
	mount ReadOnlySkillMount,
) error {
	reader, err := zip.NewReader(archive, size)
	if err != nil {
		return errors.New("sandbox: stored Skill archive is invalid")
	}
	if len(reader.File) == 0 || len(reader.File) > maxSkillFiles {
		return errors.New("sandbox: stored Skill archive has an invalid file count")
	}
	seen := make(map[string]struct{}, len(reader.File))
	expandedLimit := mount.UncompressedSizeBytes
	if expandedLimit == domain.UnknownSkillUncompressedSize {
		expandedLimit = maxSkillArchiveBytes - 1
	}
	var total int64
	foundSkillMD := false
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		relative, err := skillArchiveRelativePath(entry.Name, mount.ArchiveRoot)
		if err != nil {
			return err
		}
		if _, exists := seen[relative]; exists {
			return errors.New("sandbox: stored Skill archive contains duplicate paths")
		}
		seen[relative] = struct{}{}
		if entry.FileInfo().IsDir() || !entry.Mode().IsRegular() {
			return errors.New("sandbox: stored Skill archive contains a non-regular file")
		}
		if entry.UncompressedSize64 > uint64(expandedLimit-total) {
			return errors.New("sandbox: stored Skill archive expanded size mismatch")
		}
		target := filepath.Join(staging, filepath.FromSlash(relative))
		if err := secureSkillMkdirAll(staging, filepath.Dir(target)); err != nil {
			return err
		}
		opened, err := entry.Open()
		if err != nil {
			return errors.New("sandbox: stored Skill archive is invalid")
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			_ = opened.Close()
			return fmt.Errorf("sandbox: create extracted Skill file: %w", err)
		}
		written, copyErr := io.Copy(output, io.LimitReader(opened, int64(entry.UncompressedSize64)+1))
		closeInputErr := opened.Close()
		if copyErr != nil || closeInputErr != nil || written != int64(entry.UncompressedSize64) {
			_ = output.Close()
			return errors.New("sandbox: stored Skill archive is invalid")
		}
		if err := output.Sync(); err != nil {
			_ = output.Close()
			return fmt.Errorf("sandbox: sync extracted Skill file: %w", err)
		}
		mode := os.FileMode(0o444) | (entry.Mode().Perm() & 0o111)
		if err := output.Chmod(mode); err != nil {
			_ = output.Close()
			return fmt.Errorf("sandbox: make extracted Skill file read-only: %w", err)
		}
		if err := output.Close(); err != nil {
			return fmt.Errorf("sandbox: close extracted Skill file: %w", err)
		}
		total += written
		foundSkillMD = foundSkillMD || relative == "SKILL.md"
	}
	if !foundSkillMD ||
		(mount.UncompressedSizeBytes != domain.UnknownSkillUncompressedSize && total != mount.UncompressedSizeBytes) {
		return errors.New("sandbox: stored Skill archive does not match its metadata")
	}
	return nil
}

func secureSkillMkdirAll(root, directory string) error {
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("sandbox: Skill directory escapes validation root")
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := os.Mkdir(current, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("sandbox: create Skill directory: %w", err)
		}
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("sandbox: inspect Skill directory: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("sandbox: Skill parent is not a validation directory")
		}
	}
	return nil
}

func skillArchiveRelativePath(raw, archiveRoot string) (string, error) {
	if raw == "" || !utf8.ValidString(raw) || strings.Contains(raw, "\\") ||
		strings.HasPrefix(raw, "/") || strings.ContainsRune(raw, '\x00') {
		return "", errors.New("sandbox: stored Skill archive contains an unsafe path")
	}
	cleaned := path.Clean(raw)
	if cleaned != raw || strings.HasSuffix(raw, "/") {
		return "", errors.New("sandbox: stored Skill archive contains an unsafe path")
	}
	prefix := archiveRoot + "/"
	if !strings.HasPrefix(cleaned, prefix) {
		return "", errors.New("sandbox: stored Skill archive root does not match metadata")
	}
	relative := strings.TrimPrefix(cleaned, prefix)
	if relative == "" || relative == "." || strings.HasPrefix(relative, "../") ||
		strings.Contains(relative, "/../") {
		return "", errors.New("sandbox: stored Skill archive contains an unsafe path")
	}
	return relative, nil
}
