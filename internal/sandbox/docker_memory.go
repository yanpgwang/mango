package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yanpgwang/mango/internal/domain"
)

const dockerMemoryManifestVersion = 1

type dockerMemoryManifest struct {
	Version int                        `json:"version"`
	StoreID string                     `json:"store_id"`
	Files   []dockerMemoryManifestFile `json:"files"`
}

type dockerMemoryManifestFile struct {
	MemoryID      string `json:"memory_id"`
	Path          string `json:"path"`
	ContentSHA256 string `json:"content_sha256"`
}

func validateMemoryStoreMount(mount MemoryStoreMount) error {
	if err := validateResourceIdentity(mount.Identity); err != nil {
		return err
	}
	if mount.StoreID == "" {
		return errors.New("sandbox: Memory Store ID is required")
	}
	if mount.Access != domain.MemoryAccessReadWrite &&
		mount.Access != domain.MemoryAccessReadOnly {
		return errors.New("sandbox: Memory Store access must be read_write or read_only")
	}
	if !utf8.ValidString(mount.RuntimePath) {
		return errors.New("sandbox: Memory Store mount path must be valid UTF-8")
	}
	for _, character := range mount.RuntimePath {
		if unicode.IsControl(character) {
			return errors.New("sandbox: Memory Store mount path contains a control character")
		}
	}
	clean := path.Clean(mount.RuntimePath)
	if path.Dir(clean) != domain.SessionMemoryRoot ||
		clean == domain.SessionMemoryRoot || len(path.Base(clean)) > 255 {
		return fmt.Errorf(
			"sandbox: Memory Store mount path %q must be one directory beneath %s",
			mount.RuntimePath,
			domain.SessionMemoryRoot,
		)
	}
	return nil
}

func prepareDockerMemoryMounts(
	memoryRoot string,
	mounts []MemoryStoreMount,
) (map[string]string, error) {
	result := make(map[string]string, len(mounts))
	paths := make(map[string]struct{}, len(mounts))
	for _, mount := range mounts {
		if err := validateMemoryStoreMount(mount); err != nil {
			return nil, Permanent(err)
		}
		if _, exists := result[mount.Identity]; exists {
			return nil, Permanent(errors.New("sandbox: duplicate Memory Store mount identity"))
		}
		if _, exists := paths[mount.RuntimePath]; exists {
			return nil, Permanent(errors.New("sandbox: duplicate Memory Store mount path"))
		}
		paths[mount.RuntimePath] = struct{}{}
		source := filepath.Join(memoryRoot, filepath.FromSlash(path.Base(mount.RuntimePath)))
		if err := os.Mkdir(source, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("sandbox: create Docker Memory Store mount: %w", err)
		}
		info, err := os.Lstat(source)
		if err != nil {
			return nil, fmt.Errorf("sandbox: inspect Docker Memory Store mount: %w", err)
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, Permanent(errors.New(
				"sandbox: Docker Memory Store mount is not a provider-owned directory",
			))
		}
		result[mount.Identity] = source
	}
	return result, nil
}

func (p *dockerProvider) inspectMemoryMounts(
	ctx context.Context,
	cid string,
	resourceRoot string,
	expected []MemoryStoreMount,
) (map[string]string, error) {
	if len(expected) == 0 {
		return map[string]string{}, nil
	}
	if resourceRoot == "" {
		return nil, Permanent(errors.New(
			"sandbox: Docker container has no provider-owned Memory Store root",
		))
	}
	actual, err := p.inspectContainerMounts(ctx, cid)
	if err != nil {
		return nil, fmt.Errorf("sandbox: inspect Docker Memory Store mounts: %w", err)
	}
	byDestination := make(map[string]struct {
		Source string
		RW     bool
	}, len(actual))
	for _, mount := range actual {
		byDestination[mount.Destination] = struct {
			Source string
			RW     bool
		}{Source: mount.Source, RW: mount.RW}
	}
	result := make(map[string]string, len(expected))
	for _, mount := range expected {
		if err := validateMemoryStoreMount(mount); err != nil {
			return nil, Permanent(err)
		}
		actualMount, ok := byDestination[mount.RuntimePath]
		if !ok {
			return nil, Permanent(fmt.Errorf(
				"sandbox: Docker Memory Store mount %s is missing",
				mount.RuntimePath,
			))
		}
		wantWritable := mount.Access == domain.MemoryAccessReadWrite
		if actualMount.RW != wantWritable {
			return nil, Permanent(fmt.Errorf(
				"sandbox: Docker Memory Store mount %s has the wrong access mode",
				mount.RuntimePath,
			))
		}
		source, err := canonicalHostPath(actualMount.Source)
		if err != nil {
			return nil, fmt.Errorf("sandbox: resolve Docker Memory Store mount: %w", err)
		}
		memoryRoot := filepath.Join(resourceRoot, dockerResourceMemoryDir)
		if filepath.Dir(source) != memoryRoot ||
			filepath.Base(source) != path.Base(mount.RuntimePath) {
			return nil, Permanent(errors.New(
				"sandbox: Docker Memory Store mount source is not provider-owned",
			))
		}
		result[mount.Identity] = source
	}
	return result, nil
}

func (s *dockerSandbox) memoryStorePaths(
	mount MemoryStoreMount,
) (string, string, error) {
	if err := validateMemoryStoreMount(mount); err != nil {
		return "", "", Permanent(err)
	}
	root, ok := s.memoryMounts[mount.Identity]
	if !ok || root == "" {
		return "", "", Permanent(fmt.Errorf(
			"sandbox: Memory Store mount %s is unavailable",
			mount.RuntimePath,
		))
	}
	sum := sha256.Sum256([]byte("memory\x00" + mount.Identity))
	manifest := filepath.Join(
		s.resourceRoot,
		dockerResourceStateDir,
		"memory-"+hex.EncodeToString(sum[:])+".json",
	)
	return root, manifest, nil
}

func validateMemoryStoreFilePath(value string) (string, error) {
	if !utf8.ValidString(value) || value == "" || value == "/" ||
		!strings.HasPrefix(value, "/") || len(value) > 1024 {
		return "", errors.New("sandbox: invalid Memory path")
	}
	clean := path.Clean(value)
	if clean != value {
		return "", errors.New("sandbox: Memory path is not canonical")
	}
	for _, character := range value {
		if unicode.IsControl(character) || unicode.Is(unicode.Cf, character) {
			return "", errors.New("sandbox: Memory path contains a control or format character")
		}
	}
	return strings.TrimPrefix(value, "/"), nil
}

func (s *dockerSandbox) ReadMemoryStore(
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
		unlock, err = s.acquireResourceLock(ctx)
		if err != nil {
			return MemoryStoreSnapshot{}, err
		}
	}
	defer unlock()

	snapshot := MemoryStoreSnapshot{}
	manifestBody, err := os.ReadFile(manifestPath)
	if err == nil {
		var manifest dockerMemoryManifest
		if jsonErr := json.Unmarshal(manifestBody, &manifest); jsonErr != nil ||
			manifest.Version != dockerMemoryManifestVersion || manifest.StoreID != mount.StoreID {
			return MemoryStoreSnapshot{}, Permanent(errors.New(
				"sandbox: Docker Memory Store baseline is invalid",
			))
		}
		snapshot.Initialized = true
		snapshot.Baseline = make([]MemoryStoreFile, 0, len(manifest.Files))
		for _, file := range manifest.Files {
			snapshot.Baseline = append(snapshot.Baseline, MemoryStoreFile{
				MemoryID:      file.MemoryID,
				Path:          file.Path,
				ContentSHA256: file.ContentSHA256,
			})
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return MemoryStoreSnapshot{}, fmt.Errorf("sandbox: read Memory Store baseline: %w", err)
	}

	err = filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return Permanent(errors.New("sandbox: Memory Store contains a symbolic link"))
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return Permanent(errors.New("sandbox: Memory Store contains a non-regular file"))
		}
		if info.Size() > 102400 {
			return Permanent(errors.New("sandbox: Memory content exceeds 102400 bytes"))
		}
		relative, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		memoryPath := "/" + filepath.ToSlash(relative)
		if _, err := validateMemoryStoreFilePath(memoryPath); err != nil {
			return Permanent(err)
		}
		body, err := os.ReadFile(filePath)
		if err != nil {
			return err
		}
		if !utf8.Valid(body) {
			return Permanent(errors.New("sandbox: Memory content must be valid UTF-8"))
		}
		snapshot.Current = append(snapshot.Current, MemoryStoreContent{
			Path:    memoryPath,
			Content: body,
		})
		if len(snapshot.Current) > 2000 {
			return Permanent(errors.New("sandbox: Memory Store exceeds 2000 files"))
		}
		return nil
	})
	if err != nil {
		return MemoryStoreSnapshot{}, err
	}
	sort.Slice(snapshot.Baseline, func(i, j int) bool {
		return snapshot.Baseline[i].Path < snapshot.Baseline[j].Path
	})
	sort.Slice(snapshot.Current, func(i, j int) bool {
		return snapshot.Current[i].Path < snapshot.Current[j].Path
	})
	return snapshot, nil
}

func (s *dockerSandbox) ReplaceMemoryStore(
	ctx context.Context,
	mount MemoryStoreMount,
	files []MemoryStoreFile,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(files) > 2000 {
		return Permanent(errors.New("sandbox: Memory Store exceeds 2000 files"))
	}
	root, manifestPath, err := s.memoryStorePaths(mount)
	if err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(files))
	manifest := dockerMemoryManifest{
		Version: dockerMemoryManifestVersion,
		StoreID: mount.StoreID,
		Files:   make([]dockerMemoryManifestFile, 0, len(files)),
	}
	for _, file := range files {
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
		if len(file.Content) > 102400 || !utf8.Valid(file.Content) {
			return Permanent(errors.New("sandbox: Memory content is invalid"))
		}
		sum := sha256.Sum256(file.Content)
		if hex.EncodeToString(sum[:]) != file.ContentSHA256 {
			return Permanent(errors.New("sandbox: Memory content checksum mismatch"))
		}
		manifest.Files = append(manifest.Files, dockerMemoryManifestFile{
			MemoryID:      file.MemoryID,
			Path:          file.Path,
			ContentSHA256: file.ContentSHA256,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	sort.Slice(manifest.Files, func(i, j int) bool {
		return manifest.Files[i].Path < manifest.Files[j].Path
	})
	manifestBody, err := json.Marshal(manifest)
	if err != nil {
		return err
	}

	unlock := func() {}
	if !resourceSyncLockHeld(ctx) {
		unlock, err = s.acquireResourceLock(ctx)
		if err != nil {
			return err
		}
	}
	defer unlock()
	// Missing manifest means an interrupted refresh is never mistaken for agent
	// edits against an old baseline.
	if err := os.Remove(manifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sandbox: clear Memory Store baseline: %w", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("sandbox: list Memory Store mount: %w", err)
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return fmt.Errorf("sandbox: clear Memory Store mount: %w", err)
		}
	}
	for _, file := range files {
		relative, _ := validateMemoryStoreFilePath(file.Path)
		target := filepath.Join(root, filepath.FromSlash(relative))
		if err := secureMkdirAll(root, filepath.Dir(target)); err != nil {
			return err
		}
		temp, err := os.CreateTemp(filepath.Dir(target), ".mango-memory-*")
		if err != nil {
			return fmt.Errorf("sandbox: create Memory temp file: %w", err)
		}
		tempName := temp.Name()
		if _, err := temp.Write(file.Content); err != nil {
			temp.Close() //nolint:errcheck
			_ = os.Remove(tempName)
			return fmt.Errorf("sandbox: write Memory file: %w", err)
		}
		if err := temp.Sync(); err != nil {
			temp.Close() //nolint:errcheck
			_ = os.Remove(tempName)
			return fmt.Errorf("sandbox: sync Memory file: %w", err)
		}
		if err := temp.Chmod(0o644); err != nil {
			temp.Close() //nolint:errcheck
			_ = os.Remove(tempName)
			return fmt.Errorf("sandbox: chmod Memory file: %w", err)
		}
		if err := temp.Close(); err != nil {
			_ = os.Remove(tempName)
			return fmt.Errorf("sandbox: close Memory file: %w", err)
		}
		if err := os.Rename(tempName, target); err != nil {
			_ = os.Remove(tempName)
			return fmt.Errorf("sandbox: publish Memory file: %w", err)
		}
	}
	if err := syncDirectory(root); err != nil {
		return err
	}
	return writeResourceMarker(manifestPath, string(manifestBody))
}
