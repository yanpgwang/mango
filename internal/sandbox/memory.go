package sandbox

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/yanpgwang/mango/internal/domain"
)

type resourceSyncContextKey struct{}

func resourceSyncLockHeld(ctx context.Context) bool {
	held, _ := ctx.Value(resourceSyncContextKey{}).(bool)
	return held
}

func resourceSyncContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, resourceSyncContextKey{}, true)
}

const memoryStoreManifestVersion = 1

type memoryStoreManifest struct {
	Version int                       `json:"version"`
	StoreID string                    `json:"store_id"`
	Files   []memoryStoreManifestFile `json:"files"`
}

type memoryStoreManifestFile struct {
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

func validateMemoryStoreMounts(mounts []MemoryStoreMount) error {
	identities := make(map[string]struct{}, len(mounts))
	paths := make(map[string]struct{}, len(mounts))
	for _, mount := range mounts {
		if err := validateMemoryStoreMount(mount); err != nil {
			return err
		}
		if _, duplicate := identities[mount.Identity]; duplicate {
			return errors.New("sandbox: duplicate Memory Store mount identity")
		}
		identities[mount.Identity] = struct{}{}
		if _, duplicate := paths[mount.RuntimePath]; duplicate {
			return errors.New("sandbox: duplicate Memory Store mount path")
		}
		paths[mount.RuntimePath] = struct{}{}
	}
	return nil
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
