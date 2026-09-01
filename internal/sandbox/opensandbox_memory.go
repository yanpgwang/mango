package sandbox

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	openSandboxMemoryControlRoot = "/var/lib/mango/memory-control"
	openSandboxMemoryStateRoot   = "/var/lib/mango/memory-state"
	openSandboxMemoryFileLimit   = 102400
	openSandboxMemoryFilesLimit  = 2000
	openSandboxManifestLimit     = 1 << 20
)

func openSandboxMemoryIdentity(identity string) string {
	sum := sha256.Sum256([]byte("opensandbox-memory-mount\x00" + identity))
	return hex.EncodeToString(sum[:8])
}

func openSandboxMemoryControlPath(mount MemoryStoreMount) string {
	return path.Join(openSandboxMemoryControlRoot, openSandboxMemoryIdentity(mount.Identity))
}

func openSandboxMemoryManifestPath(mount MemoryStoreMount) string {
	return path.Join(
		openSandboxMemoryStateRoot,
		"memory-"+openSandboxMemoryIdentity(mount.Identity)+".json",
	)
}

func (s *openSandboxBox) ensureMemoryLayout(ctx context.Context) error {
	if len(s.memoryStores) == 0 {
		return nil
	}
	for _, directory := range []struct {
		path string
		mode int
	}{
		{openSandboxMemoryControlRoot, 0o700},
		{openSandboxMemoryStateRoot, 0o700},
	} {
		if err := s.resources.ensureDirectory(ctx, directory.path, directory.mode); err != nil {
			return err
		}
	}
	paths := []string{openSandboxMemoryControlRoot, openSandboxMemoryStateRoot}
	for _, mount := range s.memoryStores {
		for _, mountPath := range []string{
			mount.RuntimePath,
			openSandboxMemoryControlPath(mount),
		} {
			info, err := s.remote.ResourceStat(ctx, mountPath)
			if err != nil {
				return fmt.Errorf("sandbox: opensandbox inspect Memory mount %s: %w", mountPath, err)
			}
			if !info.Directory {
				return Permanent(fmt.Errorf(
					"sandbox: opensandbox Memory mount %s is not a directory",
					mountPath,
				))
			}
		}
		paths = append(paths, openSandboxMemoryControlPath(mount))
	}
	result, err := s.execMaintenance(ctx, Command{
		Path: "chmod",
		Args: append([]string{"0700"}, paths...),
	}, remoteOperationCommandTimeout(ctx, s.timeout))
	if err != nil {
		return fmt.Errorf("sandbox: opensandbox protect Memory control paths: %w", err)
	}
	if result == nil || result.ExitCode != 0 {
		return Permanent(fmt.Errorf(
			"sandbox: opensandbox cannot protect Memory control paths: %s",
			remoteCommandFailure(result),
		))
	}
	// The private parent remains 0700. Each child is the same volume root that
	// agents see at the public mount, so it must be traversable there. A
	// read-only public mount still enforces the write boundary at the VFS layer.
	for _, mount := range s.memoryStores {
		result, err = s.execMaintenance(ctx, Command{
			Path: "chmod", Args: []string{"0777", openSandboxMemoryControlPath(mount)},
		}, remoteOperationCommandTimeout(ctx, s.timeout))
		if err != nil {
			return fmt.Errorf("sandbox: opensandbox prepare Memory mount: %w", err)
		}
		if result == nil || result.ExitCode != 0 {
			return Permanent(fmt.Errorf(
				"sandbox: opensandbox cannot prepare Memory mount: %s",
				remoteCommandFailure(result),
			))
		}
	}
	return nil
}

func (s *openSandboxBox) memoryStorePaths(
	mount MemoryStoreMount,
) (string, string, error) {
	if err := validateMemoryStoreMount(mount); err != nil {
		return "", "", Permanent(err)
	}
	expected, ok := s.memoryStores[mount.Identity]
	if !ok || expected != mount {
		return "", "", Permanent(fmt.Errorf(
			"sandbox: Memory Store mount %s is unavailable",
			mount.RuntimePath,
		))
	}
	return openSandboxMemoryControlPath(mount), openSandboxMemoryManifestPath(mount), nil
}

func (s *openSandboxBox) ReadMemoryStore(
	ctx context.Context,
	mount MemoryStoreMount,
) (MemoryStoreSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return MemoryStoreSnapshot{}, err
	}
	root, manifestPath, err := s.memoryStorePaths(mount)
	if err != nil {
		return MemoryStoreSnapshot{}, err
	}
	unlock := func() {}
	if !resourceSyncLockHeld(ctx) {
		ctx, unlock, err = s.sync.LockResourceSync(ctx)
		if err != nil {
			return MemoryStoreSnapshot{}, err
		}
	}
	defer unlock()

	snapshot, err := s.readMemoryBaseline(ctx, mount, manifestPath)
	if err != nil {
		return MemoryStoreSnapshot{}, err
	}
	current, err := s.readMemoryTree(ctx, root)
	if err != nil {
		return MemoryStoreSnapshot{}, err
	}
	snapshot.Current = current
	sort.Slice(snapshot.Baseline, func(i, j int) bool {
		return snapshot.Baseline[i].Path < snapshot.Baseline[j].Path
	})
	return snapshot, nil
}

func (s *openSandboxBox) readMemoryBaseline(
	ctx context.Context,
	mount MemoryStoreMount,
	manifestPath string,
) (MemoryStoreSnapshot, error) {
	info, err := s.remote.ResourceStat(ctx, manifestPath)
	if s.remote.ResourceIsNotFound(err) {
		return MemoryStoreSnapshot{}, nil
	}
	if err != nil {
		return MemoryStoreSnapshot{}, fmt.Errorf("sandbox: read Memory Store baseline: %w", err)
	}
	if !info.Regular || info.SizeBytes > openSandboxManifestLimit {
		return MemoryStoreSnapshot{}, Permanent(errors.New(
			"sandbox: OpenSandbox Memory Store baseline is invalid",
		))
	}
	reader, err := s.remote.ResourceOpen(ctx, manifestPath)
	if err != nil {
		return MemoryStoreSnapshot{}, fmt.Errorf("sandbox: open Memory Store baseline: %w", err)
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, openSandboxManifestLimit+1))
	closeErr := reader.Close()
	if readErr != nil {
		return MemoryStoreSnapshot{}, fmt.Errorf("sandbox: read Memory Store baseline: %w", readErr)
	}
	if closeErr != nil {
		return MemoryStoreSnapshot{}, fmt.Errorf("sandbox: close Memory Store baseline: %w", closeErr)
	}
	var manifest memoryStoreManifest
	if len(body) > openSandboxManifestLimit || json.Unmarshal(body, &manifest) != nil ||
		manifest.Version != memoryStoreManifestVersion || manifest.StoreID != mount.StoreID {
		return MemoryStoreSnapshot{}, Permanent(errors.New(
			"sandbox: OpenSandbox Memory Store baseline is invalid",
		))
	}
	snapshot := MemoryStoreSnapshot{
		Initialized: true,
		Baseline:    make([]MemoryStoreFile, 0, len(manifest.Files)),
	}
	for _, file := range manifest.Files {
		if file.MemoryID == "" {
			return MemoryStoreSnapshot{}, Permanent(errors.New(
				"sandbox: OpenSandbox Memory Store baseline is invalid",
			))
		}
		if _, err := validateMemoryStoreFilePath(file.Path); err != nil {
			return MemoryStoreSnapshot{}, Permanent(err)
		}
		snapshot.Baseline = append(snapshot.Baseline, MemoryStoreFile{
			MemoryID:      file.MemoryID,
			Path:          file.Path,
			ContentSHA256: file.ContentSHA256,
		})
	}
	return snapshot, nil
}

func (s *openSandboxBox) readMemoryTree(
	ctx context.Context,
	root string,
) ([]MemoryStoreContent, error) {
	archivePath, err := openSandboxMemoryArchivePath()
	if err != nil {
		return nil, err
	}
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), remoteDefaultPeriod)
		defer cancel()
		removeErr := s.remote.ResourceRemoveFile(cleanupCtx, archivePath)
		if removeErr != nil && !s.remote.ResourceIsNotFound(removeErr) {
			// The primary snapshot result remains authoritative. A later refresh
			// can safely overwrite this random control-plane artifact.
			return
		}
	}
	defer cleanup()
	result, err := s.execMaintenance(ctx, Command{
		Path: "tar", Args: []string{"-C", root, "-cf", archivePath, "."},
	}, remoteOperationCommandTimeout(ctx, s.timeout))
	if err != nil {
		return nil, fmt.Errorf("sandbox: opensandbox archive Memory Store: %w", err)
	}
	if result == nil || result.ExitCode != 0 {
		return nil, Permanent(fmt.Errorf(
			"sandbox: opensandbox cannot archive Memory Store: %s",
			remoteCommandFailure(result),
		))
	}
	reader, err := s.remote.ResourceOpen(ctx, archivePath)
	if err != nil {
		return nil, fmt.Errorf("sandbox: opensandbox open Memory Store archive: %w", err)
	}
	current, readErr := readOpenSandboxMemoryArchive(reader)
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, fmt.Errorf("sandbox: opensandbox close Memory Store archive: %w", closeErr)
	}
	return current, nil
}

func openSandboxMemoryArchivePath() (string, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("sandbox: allocate Memory Store snapshot: %w", err)
	}
	return path.Join(
		openSandboxMemoryStateRoot,
		"snapshot-"+hex.EncodeToString(nonce[:])+".tar",
	), nil
}

func readOpenSandboxMemoryArchive(reader io.Reader) ([]MemoryStoreContent, error) {
	archive := tar.NewReader(reader)
	current := make([]MemoryStoreContent, 0)
	seen := make(map[string]struct{})
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("sandbox: read Memory Store archive: %w", err)
		}
		name := strings.TrimPrefix(header.Name, "./")
		if header.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if name == "" || name == "." {
			if header.Typeflag == tar.TypeDir {
				continue
			}
			return nil, Permanent(errors.New("sandbox: Memory Store archive has an invalid root entry"))
		}
		if path.IsAbs(name) || path.Clean(name) != name || strings.HasPrefix(name, "../") {
			return nil, Permanent(errors.New("sandbox: Memory Store archive path is invalid"))
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg {
			return nil, Permanent(errors.New("sandbox: Memory Store contains a non-regular file"))
		}
		if header.Size < 0 || header.Size > openSandboxMemoryFileLimit {
			return nil, Permanent(errors.New("sandbox: Memory content exceeds 102400 bytes"))
		}
		memoryPath := "/" + name
		if _, err := validateMemoryStoreFilePath(memoryPath); err != nil {
			return nil, Permanent(err)
		}
		if _, duplicate := seen[memoryPath]; duplicate {
			return nil, Permanent(errors.New("sandbox: Memory Store contains duplicate paths"))
		}
		seen[memoryPath] = struct{}{}
		body, err := io.ReadAll(io.LimitReader(archive, openSandboxMemoryFileLimit+1))
		if err != nil {
			return nil, fmt.Errorf("sandbox: read Memory content: %w", err)
		}
		if int64(len(body)) != header.Size || len(body) > openSandboxMemoryFileLimit {
			return nil, Permanent(errors.New("sandbox: Memory content size is invalid"))
		}
		if !utf8.Valid(body) {
			return nil, Permanent(errors.New("sandbox: Memory content must be valid UTF-8"))
		}
		current = append(current, MemoryStoreContent{Path: memoryPath, Content: body})
		if len(current) > openSandboxMemoryFilesLimit {
			return nil, Permanent(errors.New("sandbox: Memory Store exceeds 2000 files"))
		}
	}
	sort.Slice(current, func(i, j int) bool { return current[i].Path < current[j].Path })
	return current, nil
}

func (s *openSandboxBox) ReplaceMemoryStore(
	ctx context.Context,
	mount MemoryStoreMount,
	files []MemoryStoreFile,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(files) > openSandboxMemoryFilesLimit {
		return Permanent(errors.New("sandbox: Memory Store exceeds 2000 files"))
	}
	root, manifestPath, err := s.memoryStorePaths(mount)
	if err != nil {
		return err
	}
	ordered := append([]MemoryStoreFile(nil), files...)
	seen := make(map[string]struct{}, len(ordered))
	manifest := memoryStoreManifest{
		Version: memoryStoreManifestVersion,
		StoreID: mount.StoreID,
		Files:   make([]memoryStoreManifestFile, 0, len(ordered)),
	}
	for _, file := range ordered {
		if file.MemoryID == "" {
			return Permanent(errors.New("sandbox: Memory baseline is missing a Memory ID"))
		}
		if _, err := validateMemoryStoreFilePath(file.Path); err != nil {
			return Permanent(err)
		}
		if _, duplicate := seen[file.Path]; duplicate {
			return Permanent(errors.New("sandbox: Memory baseline contains duplicate paths"))
		}
		seen[file.Path] = struct{}{}
		if len(file.Content) > openSandboxMemoryFileLimit || !utf8.Valid(file.Content) {
			return Permanent(errors.New("sandbox: Memory content is invalid"))
		}
		sum := sha256.Sum256(file.Content)
		if hex.EncodeToString(sum[:]) != file.ContentSHA256 {
			return Permanent(errors.New("sandbox: Memory content checksum mismatch"))
		}
		manifest.Files = append(manifest.Files, memoryStoreManifestFile{
			MemoryID:      file.MemoryID,
			Path:          file.Path,
			ContentSHA256: file.ContentSHA256,
		})
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	sort.Slice(manifest.Files, func(i, j int) bool {
		return manifest.Files[i].Path < manifest.Files[j].Path
	})
	manifestBody, err := json.Marshal(manifest)
	if err != nil {
		return err
	}

	unlock := func() {}
	if !resourceSyncLockHeld(ctx) {
		ctx, unlock, err = s.sync.LockResourceSync(ctx)
		if err != nil {
			return err
		}
	}
	defer unlock()
	if err := s.removeMemoryFileIfPresent(ctx, manifestPath); err != nil {
		return fmt.Errorf("sandbox: clear Memory Store baseline: %w", err)
	}
	result, err := s.execMaintenance(ctx, Command{
		Path: "sh",
		Args: []string{
			"-c",
			`find "$1" -mindepth 1 -maxdepth 1 -exec rm -rf -- {} +`,
			"mango-memory-clear",
			root,
		},
	}, remoteOperationCommandTimeout(ctx, s.timeout))
	if err != nil {
		return fmt.Errorf("sandbox: clear OpenSandbox Memory Store: %w", err)
	}
	if result == nil || result.ExitCode != 0 {
		return Permanent(fmt.Errorf(
			"sandbox: cannot clear OpenSandbox Memory Store: %s",
			remoteCommandFailure(result),
		))
	}
	for _, file := range ordered {
		relative, _ := validateMemoryStoreFilePath(file.Path)
		target := path.Join(root, relative)
		if err := s.ensureMemoryParent(ctx, root, path.Dir(relative)); err != nil {
			return err
		}
		if err := s.remote.ResourceUpload(
			ctx,
			target,
			strings.NewReader(string(file.Content)),
			remoteFilePermissions{Mode: 0o666},
		); err != nil {
			return fmt.Errorf("sandbox: upload OpenSandbox Memory content: %w", err)
		}
	}
	if err := s.remote.ResourceUpload(
		ctx,
		manifestPath,
		strings.NewReader(string(manifestBody)),
		remoteFilePermissions{Mode: 0o600},
	); err != nil {
		return fmt.Errorf("sandbox: write OpenSandbox Memory baseline: %w", err)
	}
	return nil
}

func (s *openSandboxBox) ensureMemoryParent(
	ctx context.Context,
	root string,
	relative string,
) error {
	parent := root
	for _, component := range strings.Split(relative, "/") {
		if component == "" || component == "." {
			continue
		}
		parent = path.Join(parent, component)
		if err := s.resources.ensureDirectory(ctx, parent, 0o777); err != nil {
			return err
		}
	}
	return nil
}

func (s *openSandboxBox) removeMemoryFileIfPresent(
	ctx context.Context,
	filePath string,
) error {
	err := s.remote.ResourceRemoveFile(ctx, filePath)
	if err == nil || s.remote.ResourceIsNotFound(err) {
		return nil
	}
	return err
}

func remoteCommandFailure(result *Result) string {
	if result == nil {
		return "command returned no result"
	}
	message := strings.TrimSpace(string(result.Stderr))
	if message == "" {
		message = strings.TrimSpace(string(result.Stdout))
	}
	if message == "" {
		message = fmt.Sprintf("command exited with code %d", result.ExitCode)
	}
	return message
}

var _ MemoryStoreSandbox = (*openSandboxBox)(nil)
