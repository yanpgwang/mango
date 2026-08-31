package sandbox

import (
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
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/yanpgwang/mango/internal/domain"
)

const (
	dockerResourceFilesDir   = "files"
	dockerResourceOutputsDir = "outputs"
	dockerResourceSkillsDir  = "skills"
	dockerResourceMemoryDir  = "memory"
	dockerResourceStateDir   = "state"
)

func dockerResourceRootPrefix(sessionKey string) string {
	sum := sha256.Sum256([]byte(sessionKey))
	return fmt.Sprintf("session-%x-", sum[:16])
}

func (p *dockerProvider) auditResourceRoots() {
	now := time.Now()
	p.resourceAuditMu.Lock()
	if !p.resourceAuditAt.IsZero() && now.Sub(p.resourceAuditAt) < dockerResourceAuditEvery {
		p.resourceAuditMu.Unlock()
		return
	}
	p.resourceAuditAt = now
	p.resourceAuditMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = p.reapStaleResourceRoots(ctx, now.Add(-dockerResourceReapGrace))
}

func (p *dockerProvider) reapStaleResourceRoots(
	ctx context.Context,
	staleBefore time.Time,
) error {
	base, err := canonicalHostPath(p.resourceBaseDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := os.Stat(base); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	active, err := p.activeDockerResourceRoots(ctx, base)
	if err != nil {
		// The active set must be complete before deleting anything. A daemon or
		// inspect failure therefore makes the entire audit a no-op.
		return err
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !isDockerResourceRootName(entry.Name()) {
			continue
		}
		root := filepath.Join(base, entry.Name())
		if _, mounted := active[root]; mounted {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.ModTime().Before(staleBefore) {
			continue
		}
		if err := os.RemoveAll(root); err != nil {
			return fmt.Errorf("sandbox: reap stale Docker File Resource directory: %w", err)
		}
	}
	return nil
}

func (p *dockerProvider) activeDockerResourceRoots(
	ctx context.Context,
	base string,
) (map[string]struct{}, error) {
	filters := make(client.Filters).Add(
		"label", dockerManagedLabel+"=true",
	)
	listed, err := p.engine.ContainerList(ctx, client.ContainerListOptions{
		All: true, Filters: filters,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: list managed Docker containers: %w", err)
	}
	active := make(map[string]struct{})
	for _, item := range listed.Items {
		mounts, err := p.inspectContainerMounts(ctx, item.ID)
		if err != nil {
			return nil, fmt.Errorf("sandbox: inspect managed Docker container: %w", err)
		}
		for _, mount := range mounts {
			directory := ""
			switch mount.Destination {
			case SessionUploadsRoot:
				directory = dockerResourceFilesDir
			case domain.SessionSkillsRoot:
				directory = dockerResourceSkillsDir
			default:
				continue
			}
			mountedRoot, err := canonicalHostPath(mount.Source)
			if err != nil || filepath.Base(mountedRoot) != directory {
				continue
			}
			root := filepath.Dir(mountedRoot)
			relative, err := filepath.Rel(base, root)
			if err == nil && isDockerResourceRootName(relative) &&
				!strings.Contains(relative, string(filepath.Separator)) {
				active[root] = struct{}{}
			}
		}
	}
	return active, nil
}

func (p *dockerProvider) inspectContainerMounts(
	ctx context.Context,
	cid string,
) ([]container.MountPoint, error) {
	result, err := p.engine.ContainerInspect(ctx, cid, client.ContainerInspectOptions{})
	if err != nil {
		return nil, err
	}
	return result.Container.Mounts, nil
}

func isDockerResourceRootName(name string) bool {
	const prefix = "session-"
	if !strings.HasPrefix(name, prefix) || len(name) <= len(prefix)+32+1 ||
		name[len(prefix)+32] != '-' {
		return false
	}
	_, err := hex.DecodeString(name[len(prefix) : len(prefix)+32])
	return err == nil
}

func (p *dockerProvider) ensureResourceRoot(
	sessionKey string,
) (string, string, string, string, string, error) {
	if strings.Contains(p.resourceBaseDir, ",") {
		return "", "", "", "", "", errors.New("sandbox: docker resource directory cannot contain a comma")
	}
	if _, err := os.Stat(p.resourceBaseDir); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(p.resourceBaseDir, 0o700); err != nil {
			return "", "", "", "", "", fmt.Errorf("sandbox: create docker resource base directory: %w", err)
		}
		if err := os.Chmod(p.resourceBaseDir, 0o700); err != nil {
			return "", "", "", "", "", fmt.Errorf("sandbox: protect docker resource base directory: %w", err)
		}
	} else if err != nil {
		return "", "", "", "", "", fmt.Errorf("sandbox: inspect docker resource base directory: %w", err)
	}
	base, err := filepath.EvalSymlinks(p.resourceBaseDir)
	if err != nil {
		return "", "", "", "", "", fmt.Errorf("sandbox: resolve docker resource base directory: %w", err)
	}
	// A new generation for each newly provisioned container prevents stale
	// Docker Desktop bind-mount lookups after an earlier sandbox was destroyed.
	// The winning generation is recovered from the container mount on Attach.
	root, err := os.MkdirTemp(base, dockerResourceRootPrefix(sessionKey))
	if err != nil {
		return "", "", "", "", "", fmt.Errorf("sandbox: create docker resource directory: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(root)
		}
	}()
	if err := os.MkdirAll(filepath.Join(root, dockerResourceFilesDir), 0o755); err != nil {
		return "", "", "", "", "", fmt.Errorf("sandbox: create docker resource files directory: %w", err)
	}
	if err := os.Chmod(filepath.Join(root, dockerResourceFilesDir), 0o755); err != nil {
		return "", "", "", "", "", fmt.Errorf("sandbox: set docker resource files permissions: %w", err)
	}
	// Output files are produced by the container user, which is provider-image
	// specific. The directory is private to one Session resource root, so broad
	// write permission does not cross a Session boundary.
	if err := os.MkdirAll(filepath.Join(root, dockerResourceOutputsDir), 0o777); err != nil {
		return "", "", "", "", "", fmt.Errorf("sandbox: create docker output directory: %w", err)
	}
	if err := os.Chmod(filepath.Join(root, dockerResourceOutputsDir), 0o777); err != nil {
		return "", "", "", "", "", fmt.Errorf("sandbox: set docker output permissions: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, dockerResourceSkillsDir), 0o755); err != nil {
		return "", "", "", "", "", fmt.Errorf("sandbox: create docker Skill directory: %w", err)
	}
	if err := os.Chmod(filepath.Join(root, dockerResourceSkillsDir), 0o755); err != nil {
		return "", "", "", "", "", fmt.Errorf("sandbox: set docker Skill directory permissions: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, dockerResourceMemoryDir), 0o755); err != nil {
		return "", "", "", "", "", fmt.Errorf("sandbox: create Docker Memory Store directory: %w", err)
	}
	if err := os.Chmod(filepath.Join(root, dockerResourceMemoryDir), 0o755); err != nil {
		return "", "", "", "", "", fmt.Errorf("sandbox: set Docker Memory Store permissions: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(root, dockerResourceStateDir), 0o700); err != nil {
		return "", "", "", "", "", fmt.Errorf("sandbox: create docker resource state directory: %w", err)
	}
	if err := os.Chmod(filepath.Join(root, dockerResourceStateDir), 0o700); err != nil {
		return "", "", "", "", "", fmt.Errorf("sandbox: set docker resource state permissions: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", "", "", "", fmt.Errorf("sandbox: resolve docker resource directory: %w", err)
	}
	cleanup = false
	return resolved,
		filepath.Join(resolved, dockerResourceFilesDir),
		filepath.Join(resolved, dockerResourceOutputsDir),
		filepath.Join(resolved, dockerResourceSkillsDir),
		filepath.Join(resolved, dockerResourceMemoryDir),
		nil
}

func (p *dockerProvider) inspectSkillMount(
	ctx context.Context,
	cid string,
	resourceRoot string,
) (bool, error) {
	mounts, err := p.inspectContainerMounts(ctx, cid)
	if err != nil {
		return false, fmt.Errorf("sandbox: inspect Docker Skill mount: %w", err)
	}
	var actual *container.MountPoint
	for index := range mounts {
		if mounts[index].Destination == domain.SessionSkillsRoot {
			actual = &mounts[index]
			break
		}
	}
	if actual == nil {
		return false, nil
	}
	if actual.RW || resourceRoot == "" {
		return false, Permanent(errors.New(
			"sandbox: Docker Skill mount is not a recognized read-only bind mount",
		))
	}
	skillsRoot, err := canonicalHostPath(actual.Source)
	if err != nil {
		return false, fmt.Errorf("sandbox: resolve Docker Skill mount: %w", err)
	}
	if filepath.Base(skillsRoot) != dockerResourceSkillsDir ||
		filepath.Dir(skillsRoot) != resourceRoot {
		return false, Permanent(errors.New(
			"sandbox: Docker Skill mount source is not provider-owned",
		))
	}
	return true, nil
}

func (p *dockerProvider) inspectOutputMount(
	ctx context.Context,
	cid string,
	resourceRoot string,
) (bool, error) {
	mounts, err := p.inspectContainerMounts(ctx, cid)
	if err != nil {
		return false, fmt.Errorf("sandbox: inspect Docker output mount: %w", err)
	}
	var actual *container.MountPoint
	for index := range mounts {
		if mounts[index].Destination == SessionOutputsRoot {
			actual = &mounts[index]
			break
		}
	}
	if actual == nil {
		// Sandboxes created before output publication remain attachable for
		// ordinary tools, but cannot claim the deliverable capability.
		return false, nil
	}
	if !actual.RW || resourceRoot == "" {
		return false, Permanent(errors.New(
			"sandbox: Docker output mount is not a recognized writable bind mount",
		))
	}
	outputsRoot, err := canonicalHostPath(actual.Source)
	if err != nil {
		return false, fmt.Errorf("sandbox: resolve Docker output mount: %w", err)
	}
	if filepath.Base(outputsRoot) != dockerResourceOutputsDir ||
		filepath.Dir(outputsRoot) != resourceRoot {
		return false, Permanent(errors.New(
			"sandbox: Docker output mount source is not provider-owned",
		))
	}
	return true, nil
}

func (p *dockerProvider) inspectResourceMount(
	ctx context.Context,
	cid string,
	sessionKey string,
) (string, bool, error) {
	mounts, err := p.inspectContainerMounts(ctx, cid)
	if err != nil {
		return "", false, fmt.Errorf("sandbox: inspect Docker File Resource mount: %w", err)
	}
	var actual *container.MountPoint
	for index := range mounts {
		if mounts[index].Destination == SessionUploadsRoot {
			actual = &mounts[index]
			break
		}
	}
	if actual == nil {
		// Containers created before File Resources were introduced remain valid
		// for ordinary tools. Active resources fail explicitly, while deleting
		// resources reconcile as a no-op because no mount could contain them.
		return "", false, nil
	}
	if actual.RW {
		return "", false, Permanent(errors.New(
			"sandbox: Docker File Resource mount is not a recognized read-only bind mount",
		))
	}
	filesRoot, err := canonicalHostPath(actual.Source)
	if err != nil {
		return "", false, fmt.Errorf("sandbox: resolve Docker File Resource mount: %w", err)
	}
	if filepath.Base(filesRoot) != dockerResourceFilesDir {
		return "", false, Permanent(errors.New(
			"sandbox: Docker File Resource mount source is not provider-owned",
		))
	}
	resourceRoot := filepath.Dir(filesRoot)
	base, err := canonicalHostPath(p.resourceBaseDir)
	if err != nil {
		return "", false, fmt.Errorf("sandbox: resolve Docker File Resource base directory: %w", err)
	}
	relative, err := filepath.Rel(base, resourceRoot)
	if err != nil || relative == "." || strings.Contains(relative, string(filepath.Separator)) ||
		!strings.HasPrefix(relative, dockerResourceRootPrefix(sessionKey)) {
		return "", false, Permanent(errors.New(
			"sandbox: Docker File Resource mount source is outside the provider directory",
		))
	}
	return resourceRoot, true, nil
}

func validateFileResourceMount(mount FileResourceMount) error {
	if err := validateResourceIdentity(mount.Identity); err != nil {
		return err
	}
	if mount.SizeBytes < 0 {
		return errors.New("sandbox: file resource size cannot be negative")
	}
	decoded, err := hex.DecodeString(mount.ChecksumSHA256)
	if err != nil || len(decoded) != sha256.Size || strings.ToLower(mount.ChecksumSHA256) != mount.ChecksumSHA256 {
		return errors.New("sandbox: file resource checksum must be a lowercase SHA-256 digest")
	}
	_, err = resourceRelativePath(mount.RuntimePath)
	return err
}

func validateResourceIdentity(identity string) error {
	if identity == "" || len(identity) > 255 || !utf8.ValidString(identity) {
		return errors.New("sandbox: file resource identity is invalid")
	}
	for _, character := range identity {
		if unicode.IsControl(character) {
			return errors.New("sandbox: file resource identity contains a control character")
		}
	}
	return nil
}

func resourceRelativePath(runtimePath string) (string, error) {
	if len(runtimePath) > domain.MaxSessionFileMountPathBytes {
		return "", errors.New("sandbox: file resource path exceeds 1024 bytes")
	}
	if !utf8.ValidString(runtimePath) {
		return "", errors.New("sandbox: file resource path must be valid UTF-8")
	}
	for _, character := range runtimePath {
		if unicode.IsControl(character) {
			return "", errors.New("sandbox: file resource path contains a control character")
		}
	}
	clean := path.Clean(runtimePath)
	if clean == SessionUploadsRoot || !strings.HasPrefix(clean, SessionUploadsRoot+"/") {
		return "", fmt.Errorf(
			"sandbox: file resource path %q must be beneath %s",
			runtimePath,
			SessionUploadsRoot,
		)
	}
	relative := strings.TrimPrefix(clean, SessionUploadsRoot+"/")
	for _, component := range strings.Split(relative, "/") {
		if len(component) > domain.MaxSessionFileMountComponentBytes {
			return "", errors.New("sandbox: file resource path component exceeds 255 bytes")
		}
	}
	return relative, nil
}

func (s *dockerSandbox) resourcePaths(runtimePath string) (string, string, error) {
	if !s.resourceMountReady || s.resourceRoot == "" {
		return "", "", Permanent(errors.New(
			"sandbox: Docker container has no read-only File Resource mount",
		))
	}
	relative, err := resourceRelativePath(runtimePath)
	if err != nil {
		return "", "", Permanent(err)
	}
	target := filepath.Join(
		s.resourceRoot,
		dockerResourceFilesDir,
		filepath.FromSlash(relative),
	)
	sum := sha256.Sum256([]byte(path.Clean(runtimePath)))
	marker := filepath.Join(
		s.resourceRoot,
		dockerResourceStateDir,
		hex.EncodeToString(sum[:]),
	)
	return target, marker, nil
}

func resourceMarker(mount FileResourceMount) string {
	return mount.Identity + "\n" + strconv.FormatInt(mount.SizeBytes, 10) + "\n" +
		mount.ChecksumSHA256 + "\n"
}

func resourceMarkerIdentity(marker []byte) string {
	identity, _, _ := strings.Cut(string(marker), "\n")
	return identity
}

type dockerResourceSyncContextKey struct{}

func resourceSyncLockHeld(ctx context.Context) bool {
	held, _ := ctx.Value(dockerResourceSyncContextKey{}).(bool)
	return held
}

func (s *dockerSandbox) acquireResourceLockMode(
	ctx context.Context,
	mode int,
	wait bool,
) (func(), bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if s.resourceRoot == "" {
		return nil, false, Permanent(errors.New(
			"sandbox: Docker container has no provider-owned Resource root",
		))
	}
	lockPath := filepath.Join(s.resourceRoot, dockerResourceStateDir, ".lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, fmt.Errorf("sandbox: open Resource lock: %w", err)
	}
	for {
		err = syscall.Flock(int(lock.Fd()), mode|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
				_ = lock.Close()
			}, true, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = lock.Close()
			return nil, false, fmt.Errorf("sandbox: lock Resource state: %w", err)
		}
		if !wait {
			_ = lock.Close()
			return func() {}, false, nil
		}
		select {
		case <-ctx.Done():
			_ = lock.Close()
			return nil, false, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (s *dockerSandbox) acquireResourceLock(ctx context.Context) (func(), error) {
	unlock, _, err := s.acquireResourceLockMode(ctx, syscall.LOCK_EX, true)
	return unlock, err
}

func (s *dockerSandbox) acquireResourceReadLock(ctx context.Context) (func(), error) {
	unlock, _, err := s.acquireResourceLockMode(ctx, syscall.LOCK_SH, true)
	return unlock, err
}

func (s *dockerSandbox) LockResourceOperation(ctx context.Context) (func(), error) {
	return s.acquireResourceReadLock(ctx)
}

func (s *dockerSandbox) TryLockResourceSync(
	ctx context.Context,
) (context.Context, func(), bool, error) {
	unlock, acquired, err := s.acquireResourceLockMode(ctx, syscall.LOCK_EX, false)
	if err != nil || !acquired {
		return ctx, unlock, acquired, err
	}
	return context.WithValue(ctx, dockerResourceSyncContextKey{}, true), unlock, true, nil
}

func (s *dockerSandbox) LockResourceSync(
	ctx context.Context,
) (context.Context, func(), error) {
	unlock, err := s.acquireResourceLock(ctx)
	if err != nil {
		return ctx, nil, err
	}
	return context.WithValue(ctx, dockerResourceSyncContextKey{}, true), unlock, nil
}

func (s *dockerSandbox) HasFileResource(
	ctx context.Context,
	mount FileResourceMount,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := validateFileResourceMount(mount); err != nil {
		return false, Permanent(err)
	}
	if !s.resourceMountReady || s.resourceRoot == "" {
		return false, nil
	}
	target, marker, err := s.resourcePaths(mount.RuntimePath)
	if err != nil {
		return false, err
	}
	unlock, err := s.acquireResourceReadLock(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sandbox: inspect File Resource: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o222 != 0 || info.Size() != mount.SizeBytes {
		return false, nil
	}
	stored, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sandbox: read File Resource marker: %w", err)
	}
	return string(stored) == resourceMarker(mount), nil
}

func (s *dockerSandbox) ImportFileResource(
	ctx context.Context,
	mount FileResourceMount,
	content io.Reader,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if content == nil {
		return errors.New("sandbox: File Resource content is required")
	}
	if err := validateFileResourceMount(mount); err != nil {
		return Permanent(err)
	}
	target, marker, err := s.resourcePaths(mount.RuntimePath)
	if err != nil {
		return err
	}
	unlock, err := s.acquireResourceLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	filesRoot := filepath.Join(s.resourceRoot, dockerResourceFilesDir)
	if err := secureMkdirAll(filesRoot, filepath.Dir(target)); err != nil {
		return err
	}

	temp, err := os.CreateTemp(filepath.Dir(target), ".mango-resource-*")
	if err != nil {
		return fmt.Errorf("sandbox: create File Resource temp file: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName) //nolint:errcheck // rename makes cleanup a no-op

	hash := sha256.New()
	limited := &io.LimitedReader{R: content, N: mount.SizeBytes + 1}
	written, copyErr := io.Copy(io.MultiWriter(temp, hash), limited)
	if copyErr != nil {
		temp.Close() //nolint:errcheck // copy error is authoritative
		return fmt.Errorf("sandbox: stream File Resource: %w", copyErr)
	}
	if written != mount.SizeBytes {
		temp.Close() //nolint:errcheck // size mismatch is authoritative
		return fmt.Errorf(
			"sandbox: File Resource size mismatch: received %d bytes, expected %d",
			written,
			mount.SizeBytes,
		)
	}
	if checksum := hex.EncodeToString(hash.Sum(nil)); checksum != mount.ChecksumSHA256 {
		temp.Close() //nolint:errcheck // checksum mismatch is authoritative
		return errors.New("sandbox: File Resource checksum mismatch")
	}
	if err := temp.Sync(); err != nil {
		temp.Close() //nolint:errcheck // sync error is authoritative
		return fmt.Errorf("sandbox: sync File Resource: %w", err)
	}
	if err := temp.Chmod(0o444); err != nil {
		temp.Close() //nolint:errcheck // chmod error is authoritative
		return fmt.Errorf("sandbox: make File Resource read-only: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("sandbox: close File Resource: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tempName, target); err != nil {
		return fmt.Errorf("sandbox: publish File Resource: %w", err)
	}
	if err := syncDirectory(filepath.Dir(target)); err != nil {
		return err
	}
	if err := writeResourceMarker(marker, resourceMarker(mount)); err != nil {
		return err
	}
	return nil
}

func (s *dockerSandbox) RemoveFileResource(
	ctx context.Context,
	runtimePath string,
	identity string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.resourceMountReady || s.resourceRoot == "" {
		return nil
	}
	if err := validateResourceIdentity(identity); err != nil {
		return Permanent(err)
	}
	target, marker, err := s.resourcePaths(runtimePath)
	if err != nil {
		return err
	}
	unlock, err := s.acquireResourceLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	stored, err := os.ReadFile(marker)
	if err == nil && resourceMarkerIdentity(stored) != identity {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sandbox: read File Resource marker: %w", err)
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sandbox: remove File Resource: %w", err)
	}
	if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sandbox: remove File Resource marker: %w", err)
	}
	filesRoot := filepath.Join(s.resourceRoot, dockerResourceFilesDir)
	if err := pruneEmptyResourceParents(filesRoot, filepath.Dir(target)); err != nil {
		return err
	}
	return syncDirectory(filesRoot)
}

func pruneEmptyResourceParents(filesRoot string, directory string) error {
	for directory != filesRoot {
		err := os.Remove(directory)
		switch {
		case err == nil, errors.Is(err, os.ErrNotExist):
			directory = filepath.Dir(directory)
		case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
			return nil
		default:
			return fmt.Errorf("sandbox: prune File Resource directory: %w", err)
		}
	}
	return nil
}

func secureMkdirAll(root string, directory string) error {
	relative, err := filepath.Rel(root, directory)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("sandbox: File Resource directory escapes provider root")
	}
	current := root
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		if err := os.Mkdir(current, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return fmt.Errorf("sandbox: create File Resource directory: %w", err)
		}
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("sandbox: inspect File Resource directory: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("sandbox: File Resource parent is not a provider-owned directory")
		}
		if err := os.Chmod(current, 0o755); err != nil {
			return fmt.Errorf("sandbox: set File Resource directory permissions: %w", err)
		}
	}
	return nil
}

func writeResourceMarker(path string, content string) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".mango-marker-*")
	if err != nil {
		return fmt.Errorf("sandbox: create File Resource marker: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName) //nolint:errcheck // rename makes cleanup a no-op
	if _, err := io.WriteString(temp, content); err != nil {
		temp.Close() //nolint:errcheck // write error is authoritative
		return fmt.Errorf("sandbox: write File Resource marker: %w", err)
	}
	if err := temp.Sync(); err != nil {
		temp.Close() //nolint:errcheck // sync error is authoritative
		return fmt.Errorf("sandbox: sync File Resource marker: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("sandbox: close File Resource marker: %w", err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("sandbox: publish File Resource marker: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(directory string) error {
	handle, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("sandbox: open File Resource directory: %w", err)
	}
	defer handle.Close() //nolint:errcheck // sync result is authoritative
	if err := handle.Sync(); err != nil {
		return fmt.Errorf("sandbox: sync File Resource directory: %w", err)
	}
	return nil
}
