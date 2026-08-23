package sandbox

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/yanpgwang/mango/internal/domain"
)

const (
	gitRepositoryControlRoot           = "/var/lib/mango/repositories"
	gitRepositoryMarkerRoot            = gitRepositoryControlRoot + "/markers"
	gitRepositoryStagingRoot           = gitRepositoryControlRoot + "/staging"
	maxGitRepositoryEntries            = 100_000
	maxGitRepositoryArchiveBytes int64 = 500 << 20
)

type repositoryExecute func(context.Context, Command) (*Result, error)
type repositoryUpload func(context.Context, string, io.Reader, int64) error

type commandGitRepositories struct {
	provider string
	execute  repositoryExecute
	upload   repositoryUpload
	mu       sync.Mutex
}

type gitRepositoryMarker struct {
	Identity       string `json:"identity"`
	ResolvedCommit string `json:"resolved_commit"`
	ChecksumSHA256 string `json:"checksum_sha256"`
	State          string `json:"state"`
}

type preparedGitRepositoryArchive struct {
	file *os.File
	size int64
}

func newCommandGitRepositories(
	provider string,
	execute repositoryExecute,
	upload repositoryUpload,
) *commandGitRepositories {
	return &commandGitRepositories{provider: provider, execute: execute, upload: upload}
}

func (r *commandGitRepositories) HasGitRepository(
	ctx context.Context,
	mount GitRepositoryMount,
) (bool, error) {
	if err := validateGitRepositoryMount(mount); err != nil {
		return false, Permanent(err)
	}
	if r == nil || r.execute == nil || r.upload == nil {
		return false, errors.New("sandbox: Git repository data plane is required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.has(ctx, mount)
}

func (r *commandGitRepositories) has(
	ctx context.Context,
	mount GitRepositoryMount,
) (bool, error) {
	markerPath := gitRepositoryMarkerPath(mount.RuntimePath)
	target := path.Clean(mount.RuntimePath)
	script := gitRepositoryExistingLayoutGuard(target) +
		"if [ -L " + shellQuote(target) + " ] || " +
		"{ [ -e " + shellQuote(target) + " ] && [ ! -d " + shellQuote(target) + " ]; }; then exit 42; fi\n" +
		"if [ ! -d " + shellQuote(target) + " ]; then exit 44; fi\n" +
		"if [ -L " + shellQuote(markerPath) + " ] || [ ! -f " + shellQuote(markerPath) + " ]; then exit 45; fi\n" +
		"cat " + shellQuote(markerPath) + "\n"
	result, err := r.execute(ctx, Command{Path: "/bin/sh", Args: []string{"-c", script}})
	if err != nil {
		return false, fmt.Errorf("sandbox: %s inspect Git repository: %w", r.provider, err)
	}
	if result == nil {
		return false, errors.New("sandbox: Git repository inspection returned no result")
	}
	switch result.ExitCode {
	case 44:
		return false, nil
	case 42:
		return false, Permanent(fmt.Errorf(
			"sandbox: Git repository target %s is not a directory", target,
		))
	case 45:
		return false, Permanent(fmt.Errorf(
			"sandbox: Git repository target %s already exists without its Mango marker", target,
		))
	case 46:
		return false, Permanent(fmt.Errorf(
			"sandbox: Git repository path %s contains an unsafe directory", target,
		))
	case 0:
	default:
		return false, fmt.Errorf(
			"sandbox: %s inspect Git repository failed with exit %d: %s",
			r.provider, result.ExitCode, strings.TrimSpace(string(result.Stderr)),
		)
	}
	var marker gitRepositoryMarker
	if len(result.Stdout) > 16*1024 || json.Unmarshal(result.Stdout, &marker) != nil {
		return false, Permanent(errors.New("sandbox: Git repository marker is invalid"))
	}
	if !repositoryMarkerMatches(marker, mount) {
		return false, Permanent(fmt.Errorf(
			"sandbox: Git repository target %s belongs to another resource", target,
		))
	}
	if marker.State == "pending" {
		marker.State = "ready"
		if err := r.writeMarker(ctx, mount.RuntimePath, marker); err != nil {
			return false, err
		}
	}
	return marker.State == "ready", nil
}

func (r *commandGitRepositories) ImportGitRepository(
	ctx context.Context,
	mount GitRepositoryMount,
	content io.Reader,
) error {
	if content == nil {
		return errors.New("sandbox: Git repository archive content is required")
	}
	if err := validateGitRepositoryMount(mount); err != nil {
		return Permanent(err)
	}
	prepared, cleanup, err := prepareGitRepositoryArchive(ctx, mount, content)
	if err != nil {
		return err
	}
	defer cleanup()

	r.mu.Lock()
	defer r.mu.Unlock()
	present, err := r.has(ctx, mount)
	if err != nil {
		return err
	}
	if present {
		return nil
	}
	if err := r.ensureLayout(ctx, mount.RuntimePath); err != nil {
		return err
	}
	marker := gitRepositoryMarker{
		Identity: mount.Identity, ResolvedCommit: mount.ResolvedCommit,
		ChecksumSHA256: mount.ChecksumSHA256, State: "pending",
	}
	if err := r.writeMarker(ctx, mount.RuntimePath, marker); err != nil {
		return err
	}
	archivePath, stagingPath := gitRepositoryStagingPaths(mount.RuntimePath, mount.Identity)
	if err := r.prepareArchiveUpload(ctx, archivePath); err != nil {
		return err
	}
	if _, err := prepared.file.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("sandbox: rewind Git repository archive: %w", err)
	}
	if err := r.upload(ctx, archivePath, prepared.file, prepared.size); err != nil {
		return fmt.Errorf("sandbox: %s transfer Git repository archive: %w", r.provider, err)
	}
	target := path.Clean(mount.RuntimePath)
	script := "set -eu\n" +
		"rm -rf " + shellQuote(stagingPath) + "\n" +
		"mkdir -p " + shellQuote(stagingPath) + "\n" +
		"tar -xf " + shellQuote(archivePath) + " -C " + shellQuote(stagingPath) + "\n" +
		"test -f " + shellQuote(path.Join(stagingPath, ".git/HEAD")) + "\n" +
		"if [ -e " + shellQuote(target) + " ] || [ -L " + shellQuote(target) + " ]; then exit 73; fi\n" +
		"mv " + shellQuote(stagingPath) + " " + shellQuote(target) + "\n" +
		"rm -f " + shellQuote(archivePath) + "\n"
	result, err := r.execute(ctx, Command{Path: "/bin/sh", Args: []string{"-c", script}})
	if err != nil {
		return fmt.Errorf("sandbox: %s restore Git repository: %w", r.provider, err)
	}
	if result == nil || (result.ExitCode != 0 && result.ExitCode != 73) {
		diagnostic := "command returned no result"
		if result != nil {
			diagnostic = fmt.Sprintf("exit %d: %s", result.ExitCode, strings.TrimSpace(string(result.Stderr)))
		}
		return fmt.Errorf("sandbox: %s restore Git repository failed: %s", r.provider, diagnostic)
	}
	if result.ExitCode == 73 {
		if err := r.cleanupStaging(ctx, archivePath, stagingPath); err != nil {
			return err
		}
		present, err := r.has(ctx, mount)
		if err != nil || !present {
			if err != nil {
				return err
			}
			return Permanent(fmt.Errorf("sandbox: Git repository target %s already exists", target))
		}
		return nil
	}
	marker.State = "ready"
	if err := r.writeMarker(ctx, mount.RuntimePath, marker); err != nil {
		return err
	}
	present, err = r.has(ctx, mount)
	if err != nil {
		return err
	}
	if !present {
		return errors.New("sandbox: provider did not persist the complete Git repository")
	}
	return nil
}

func (r *commandGitRepositories) cleanupStaging(
	ctx context.Context,
	archivePath string,
	stagingPath string,
) error {
	script := "set -eu\n" + gitRepositoryDirectoryGuardScript(gitRepositoryStagingRoot) +
		"rm -rf " + shellQuote(stagingPath) + "\n" +
		"rm -f " + shellQuote(archivePath) + "\n"
	result, err := r.execute(ctx, Command{Path: "/bin/sh", Args: []string{"-c", script}})
	if err != nil {
		return fmt.Errorf("sandbox: %s clean Git repository staging: %w", r.provider, err)
	}
	if result == nil || result.ExitCode != 0 {
		return fmt.Errorf("sandbox: %s clean Git repository staging failed", r.provider)
	}
	return nil
}

func (r *commandGitRepositories) RemoveGitRepository(
	ctx context.Context,
	runtimePath string,
	identity string,
) error {
	if err := validateResourceIdentity(identity); err != nil {
		return Permanent(err)
	}
	if err := validateGitRepositoryRuntimePath(runtimePath); err != nil {
		return Permanent(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	markerPath := gitRepositoryMarkerPath(runtimePath)
	script := gitRepositoryExistingLayoutGuard(runtimePath) +
		"if [ ! -f " + shellQuote(markerPath) + " ] || [ -L " + shellQuote(markerPath) + " ]; then exit 44; fi\n" +
		"cat " + shellQuote(markerPath) + "\n"
	result, err := r.execute(ctx, Command{Path: "/bin/sh", Args: []string{"-c", script}})
	if err != nil {
		return err
	}
	if result == nil || result.ExitCode == 44 {
		return nil
	}
	if result.ExitCode == 46 {
		return Permanent(fmt.Errorf(
			"sandbox: Git repository path %s contains an unsafe directory",
			path.Clean(runtimePath),
		))
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("sandbox: %s read Git repository marker failed", r.provider)
	}
	var marker gitRepositoryMarker
	if json.Unmarshal(result.Stdout, &marker) != nil || marker.Identity != identity {
		return nil
	}
	archivePath, stagingPath := gitRepositoryStagingPaths(runtimePath, identity)
	remove := "rm -rf " + shellQuote(path.Clean(runtimePath)) + " " + shellQuote(stagingPath) + "\n" +
		"rm -f " + shellQuote(archivePath) + " " + shellQuote(markerPath) + "\n"
	result, err = r.execute(ctx, Command{Path: "/bin/sh", Args: []string{"-c", remove}})
	if err != nil {
		return err
	}
	if result == nil || result.ExitCode != 0 {
		return fmt.Errorf("sandbox: %s remove Git repository failed", r.provider)
	}
	return nil
}

func (r *commandGitRepositories) ensureLayout(ctx context.Context, runtimePath string) error {
	script := "set -eu\n"
	for _, directory := range []string{
		gitRepositoryControlRoot, gitRepositoryMarkerRoot, gitRepositoryStagingRoot,
	} {
		script += gitRepositoryEnsureDirectoryScript(directory)
	}
	for _, directory := range gitRepositoryWorkspaceDirectories(runtimePath) {
		script += gitRepositoryEnsureDirectoryScript(directory)
	}
	result, err := r.execute(ctx, Command{Path: "/bin/sh", Args: []string{"-c", script}})
	if err != nil {
		return err
	}
	if result == nil || result.ExitCode != 0 {
		return Permanent(fmt.Errorf("sandbox: %s cannot prepare Git repository directories", r.provider))
	}
	return nil
}

func (r *commandGitRepositories) prepareArchiveUpload(
	ctx context.Context,
	archivePath string,
) error {
	quoted := shellQuote(archivePath)
	script := "set -eu\n" + gitRepositoryDirectoryGuardScript(gitRepositoryStagingRoot) +
		"if [ -L " + quoted + " ] || [ -f " + quoted + " ]; then rm -f " + quoted +
		"; elif [ -e " + quoted + " ]; then exit 46; fi\n"
	result, err := r.execute(ctx, Command{Path: "/bin/sh", Args: []string{"-c", script}})
	if err != nil {
		return err
	}
	if result == nil || result.ExitCode != 0 {
		return Permanent(fmt.Errorf(
			"sandbox: %s cannot prepare the Git repository transfer path", r.provider,
		))
	}
	return nil
}

func (r *commandGitRepositories) writeMarker(
	ctx context.Context,
	runtimePath string,
	marker gitRepositoryMarker,
) error {
	raw, err := json.Marshal(marker)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	markerPath := gitRepositoryMarkerPath(runtimePath)
	temporary := markerPath + ".tmp"
	quotedTemporary := shellQuote(temporary)
	script := "set -eu\n" +
		"umask 077\n" +
		"if [ -L " + quotedTemporary + " ] || [ -f " + quotedTemporary +
		" ]; then rm -f " + quotedTemporary +
		"; elif [ -e " + quotedTemporary + " ]; then exit 46; fi\n" +
		"cat > " + quotedTemporary + "\n" +
		"if [ -L " + quotedTemporary + " ] || [ ! -f " + quotedTemporary + " ]; then exit 46; fi\n" +
		"mv " + quotedTemporary + " " + shellQuote(markerPath) + "\n"
	result, err := r.execute(ctx, Command{
		Path: "/bin/sh", Args: []string{"-c", script}, Stdin: raw,
	})
	if err != nil {
		return fmt.Errorf("sandbox: %s write Git repository marker: %w", r.provider, err)
	}
	if result == nil || result.ExitCode != 0 {
		return fmt.Errorf("sandbox: %s write Git repository marker failed", r.provider)
	}
	return nil
}

func gitRepositoryExistingLayoutGuard(runtimePath string) string {
	var script strings.Builder
	for _, directory := range []string{gitRepositoryControlRoot, gitRepositoryMarkerRoot} {
		script.WriteString(gitRepositoryDirectoryGuardScript(directory))
	}
	for _, directory := range gitRepositoryWorkspaceDirectories(runtimePath) {
		script.WriteString(gitRepositoryDirectoryGuardScript(directory))
	}
	return script.String()
}

func gitRepositoryEnsureDirectoryScript(directory string) string {
	quoted := shellQuote(directory)
	return gitRepositoryDirectoryGuardScript(directory) +
		"mkdir -p " + quoted + "\n" +
		gitRepositoryDirectoryGuardScript(directory)
}

func gitRepositoryDirectoryGuardScript(directory string) string {
	quoted := shellQuote(directory)
	return "if [ -L " + quoted + " ]; then exit 46; fi\n" +
		"if [ -e " + quoted + " ] && [ ! -d " + quoted + " ]; then exit 46; fi\n"
}

func gitRepositoryWorkspaceDirectories(runtimePath string) []string {
	parent := path.Dir(path.Clean(runtimePath))
	directories := []string{domain.SessionRepositoryRoot}
	relative := strings.TrimPrefix(parent, domain.SessionRepositoryRoot)
	current := domain.SessionRepositoryRoot
	for _, component := range strings.Split(strings.TrimPrefix(relative, "/"), "/") {
		if component == "" || component == "." {
			continue
		}
		current = path.Join(current, component)
		directories = append(directories, current)
	}
	return directories
}

func prepareGitRepositoryArchive(
	ctx context.Context,
	mount GitRepositoryMount,
	content io.Reader,
) (preparedGitRepositoryArchive, func(), error) {
	file, err := os.CreateTemp("", "mango-repository-*.tar")
	if err != nil {
		return preparedGitRepositoryArchive{}, func() {}, err
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	hash := sha256.New()
	limited := &io.LimitedReader{R: content, N: mount.SizeBytes + 1}
	written, err := io.Copy(io.MultiWriter(file, hash), limited)
	if err != nil {
		cleanup()
		return preparedGitRepositoryArchive{}, func() {}, err
	}
	if written != mount.SizeBytes {
		cleanup()
		return preparedGitRepositoryArchive{}, func() {}, fmt.Errorf(
			"sandbox: Git repository archive size mismatch: received %d bytes, expected %d",
			written, mount.SizeBytes,
		)
	}
	if hex.EncodeToString(hash.Sum(nil)) != mount.ChecksumSHA256 {
		cleanup()
		return preparedGitRepositoryArchive{}, func() {}, errors.New(
			"sandbox: Git repository archive checksum mismatch",
		)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return preparedGitRepositoryArchive{}, func() {}, err
	}
	if err := validateGitRepositoryArchive(ctx, file); err != nil {
		cleanup()
		return preparedGitRepositoryArchive{}, func() {}, err
	}
	return preparedGitRepositoryArchive{file: file, size: written}, cleanup, nil
}

func validateGitRepositoryArchive(ctx context.Context, reader io.Reader) error {
	t := tar.NewReader(reader)
	seen := make(map[string]struct{})
	symlinks := make(map[string]struct{})
	head := false
	for count := 0; ; count++ {
		if count >= maxGitRepositoryEntries {
			return Permanent(errors.New("sandbox: Git repository archive has too many entries"))
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := t.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Permanent(fmt.Errorf("sandbox: read Git repository archive: %w", err))
		}
		name := path.Clean(strings.TrimSuffix(header.Name, "/"))
		if name == "." || path.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || len(name) > 4096 {
			return Permanent(fmt.Errorf("sandbox: unsafe Git repository archive path %q", header.Name))
		}
		if _, duplicate := seen[name]; duplicate {
			return Permanent(fmt.Errorf("sandbox: duplicate Git repository archive path %q", name))
		}
		for parent := path.Dir(name); parent != "."; parent = path.Dir(parent) {
			if _, linked := symlinks[parent]; linked {
				return Permanent(fmt.Errorf("sandbox: Git repository archive writes through symlink %q", parent))
			}
		}
		switch header.Typeflag {
		case tar.TypeReg, 0, tar.TypeDir:
		case tar.TypeSymlink:
			target := path.Clean(path.Join(path.Dir(name), header.Linkname))
			if path.IsAbs(header.Linkname) || target == ".." || strings.HasPrefix(target, "../") {
				return Permanent(fmt.Errorf("sandbox: Git repository symlink %q escapes the repository", name))
			}
			symlinks[name] = struct{}{}
		default:
			return Permanent(fmt.Errorf("sandbox: unsupported Git repository archive entry %q", name))
		}
		seen[name] = struct{}{}
		if name == ".git/HEAD" && (header.Typeflag == tar.TypeReg || header.Typeflag == 0) {
			head = true
		}
	}
	if !head {
		return Permanent(errors.New("sandbox: Git repository archive is missing .git/HEAD"))
	}
	return nil
}

func validateGitRepositoryMount(mount GitRepositoryMount) error {
	if err := validateResourceIdentity(mount.Identity); err != nil {
		return err
	}
	if err := validateGitRepositoryRuntimePath(mount.RuntimePath); err != nil {
		return err
	}
	if len(mount.ResolvedCommit) != 40 {
		return errors.New("sandbox: Git repository resolved commit is invalid")
	}
	if _, err := hex.DecodeString(mount.ResolvedCommit); err != nil {
		return errors.New("sandbox: Git repository resolved commit is invalid")
	}
	if mount.SizeBytes <= 0 || mount.SizeBytes > maxGitRepositoryArchiveBytes {
		return errors.New("sandbox: Git repository archive size is invalid")
	}
	if !validSHA256(mount.ChecksumSHA256) {
		return errors.New("sandbox: Git repository archive checksum is invalid")
	}
	return nil
}

func validateGitRepositoryRuntimePath(value string) error {
	clean := path.Clean(value)
	if clean == domain.SessionRepositoryRoot ||
		!strings.HasPrefix(clean, domain.SessionRepositoryRoot+"/") {
		return errors.New("sandbox: Git repository path must be a child of /workspace")
	}
	if domain.SessionFileMountPathsConflict(clean, domain.SessionSkillsRoot) {
		return errors.New("sandbox: Git repository path overlaps the Skill directory")
	}
	return nil
}

func repositoryMarkerMatches(marker gitRepositoryMarker, mount GitRepositoryMount) bool {
	return marker.Identity == mount.Identity &&
		marker.ResolvedCommit == mount.ResolvedCommit &&
		marker.ChecksumSHA256 == mount.ChecksumSHA256 &&
		(marker.State == "pending" || marker.State == "ready")
}

func gitRepositoryMarkerPath(runtimePath string) string {
	sum := sha256.Sum256([]byte(path.Clean(runtimePath)))
	return path.Join(gitRepositoryMarkerRoot, hex.EncodeToString(sum[:]))
}

// gitRepositoryStagingPaths keeps the uploaded archive under Mango's private
// control root while placing the extracted tree beside its final target. The
// final mv is therefore a same-filesystem rename even when /workspace is a
// separate sandbox volume. Resource identity makes the hidden sibling unique
// without exposing user-controlled path components in its name.
func gitRepositoryStagingPaths(runtimePath string, identity string) (string, string) {
	target := path.Clean(runtimePath)
	sum := sha256.Sum256([]byte(identity + "\x00" + target))
	name := hex.EncodeToString(sum[:16])
	return path.Join(gitRepositoryStagingRoot, name+".tar"),
		path.Join(path.Dir(target), ".mango-repository-"+name+".tree")
}
