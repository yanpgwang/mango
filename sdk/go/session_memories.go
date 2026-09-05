package mango

// Self-hosted Session Memory Store materialization and synchronization.
//
// The control plane remains authoritative. A worker downloads the frozen
// Session attachments, exposes their mount paths to its file tools, then
// reconciles local files against Memory content SHA-256 preconditions. This
// helper owns only directories it created for the current Work item.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	DefaultMemorySyncInterval = 15 * time.Second
	MinMemorySyncInterval     = 5 * time.Second
	MemoryFlushTimeout        = 30 * time.Second
	MemoryDeleteConfirmDelay  = 30 * time.Second
	MemoryMarkerPath          = ".mango-memory-store"

	maxSessionMemoryFiles = 2000
	maxSessionMemoryBytes = int64(102400)
	fullMemoryPageSize    = int64(20)
	basicMemoryPageSize   = int64(100)
	minimumDeleteBatch    = 8
	maximumDeleteBatch    = 50
)

var ErrMemorySyncIntervalTooShort = fmt.Errorf(
	"mango: memory sync interval must be at least %s, zero for the default, or negative to disable",
	MinMemorySyncInterval,
)

// MemoryDeleteMode controls whether a locally deleted file can delete the
// corresponding server Memory. Log-only performs every safety check but does
// not issue the delete request.
type MemoryDeleteMode uint8

const (
	MemorySyncDeletionsEnabled MemoryDeleteMode = iota
	MemorySyncDeletionsLogOnly
	MemorySyncDeletionsDisabled
)

// SessionMemoryError identifies a Store that could not be materialized before
// tool dispatch. Running without a mount named by the Session prompt would be
// a partial and misleading execution, so EnvironmentWorker treats this as an
// input-preparation failure.
type SessionMemoryError struct {
	MemoryStoreID string
	Err           error
}

func (e *SessionMemoryError) Error() string {
	return fmt.Sprintf("mango: memory store %s: %v", e.MemoryStoreID, e.Err)
}

func (e *SessionMemoryError) Unwrap() error { return e.Err }

type SessionMemoryStoresOptions struct {
	Workdir       string
	SyncInterval  time.Duration
	SyncDeletions MemoryDeleteMode
	Logger        *slog.Logger
}

// SessionMemoryStores owns the local copies of all Memory Stores attached to
// one claimed Session. It is intentionally sequential: EnvironmentWorker calls
// it between tool dispatches and once during teardown.
type SessionMemoryStores struct {
	client        *Client
	workdir       string
	syncInterval  time.Duration
	syncDeletions MemoryDeleteMode
	logger        *slog.Logger
	now           func() time.Time
	lastSync      time.Time
	finished      bool
	stores        []*mountedMemoryStore
}

type mountedMemoryStore struct {
	storeID       string
	mountPath     string
	readOnly      bool
	root          *os.Root
	identity      fs.FileInfo
	marker        []byte
	baseline      map[string]string
	refused       map[string]string
	pendingDelete map[string]time.Time
}

func NewSessionMemoryStores(client *Client, options SessionMemoryStoresOptions) (*SessionMemoryStores, error) {
	if client == nil {
		return nil, errors.New("mango: SessionMemoryStores client is required")
	}
	if options.SyncInterval > 0 && options.SyncInterval < MinMemorySyncInterval {
		return nil, fmt.Errorf("%w: got %s", ErrMemorySyncIntervalTooShort, options.SyncInterval)
	}
	if options.SyncInterval == 0 {
		options.SyncInterval = DefaultMemorySyncInterval
	}
	if options.SyncDeletions > MemorySyncDeletionsDisabled {
		return nil, errors.New("mango: invalid Memory deletion sync mode")
	}
	workdir, err := filepath.Abs(options.Workdir)
	if err != nil {
		return nil, fmt.Errorf("mango: resolve Memory workdir: %w", err)
	}
	workdir, err = filepath.EvalSymlinks(workdir)
	if err != nil {
		return nil, fmt.Errorf("mango: resolve Memory workdir: %w", err)
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := time.Now
	return &SessionMemoryStores{
		client: client, workdir: workdir, syncInterval: options.SyncInterval,
		syncDeletions: options.SyncDeletions, logger: logger, now: now,
		lastSync: now(),
	}, nil
}

// Roots returns the exact directories to add to the worker file tools.
func (s *SessionMemoryStores) Roots() []string {
	result := make([]string, 0, len(s.stores))
	for _, store := range s.stores {
		result = append(result, store.mountPath)
	}
	return result
}

// ReadOnlyRoots returns the subset whose Session attachment is read_only.
func (s *SessionMemoryStores) ReadOnlyRoots() []string {
	result := make([]string, 0, len(s.stores))
	for _, store := range s.stores {
		if store.readOnly {
			result = append(result, store.mountPath)
		}
	}
	return result
}

// Download materializes every attached Memory Store from one already-fetched
// Session snapshot. An error leaves no directory created by this call behind.
func (s *SessionMemoryStores) Download(ctx context.Context, session Session) (retErr error) {
	if len(s.stores) != 0 {
		return errors.New("mango: Session Memory Stores were already downloaded")
	}
	seenStores := map[string]struct{}{}
	seenMounts := map[string]struct{}{}
	defer func() {
		if retErr != nil {
			if err := s.Dispose(); err != nil {
				retErr = errors.Join(retErr, err)
			}
		}
	}()
	for _, resource := range session.Resources {
		memory := resource.MemoryStoreSessionResource
		if memory == nil {
			continue
		}
		if len(seenStores) == 8 {
			return errors.New("mango: Session contains more than 8 Memory Stores")
		}
		if memory.MemoryStoreID == "" {
			return &SessionMemoryError{Err: errors.New("Session resource has no memory_store_id")}
		}
		if _, duplicate := seenStores[memory.MemoryStoreID]; duplicate {
			return &SessionMemoryError{MemoryStoreID: memory.MemoryStoreID, Err: errors.New("Store is attached more than once")}
		}
		seenStores[memory.MemoryStoreID] = struct{}{}
		mountPath, err := s.validateMount(*memory)
		if err != nil {
			return &SessionMemoryError{MemoryStoreID: memory.MemoryStoreID, Err: err}
		}
		if _, duplicate := seenMounts[mountPath]; duplicate {
			return &SessionMemoryError{MemoryStoreID: memory.MemoryStoreID, Err: errors.New("mount_path conflicts with another Store")}
		}
		seenMounts[mountPath] = struct{}{}
		store, err := s.downloadStore(ctx, *memory, mountPath)
		if err != nil {
			return &SessionMemoryError{MemoryStoreID: memory.MemoryStoreID, Err: err}
		}
		s.stores = append(s.stores, store)
	}
	s.lastSync = s.now()
	return nil
}

func (s *SessionMemoryStores) validateMount(resource MemoryStoreSessionResource) (string, error) {
	if resource.Access != "read_write" && resource.Access != "read_only" {
		return "", fmt.Errorf("invalid access %q", resource.Access)
	}
	if !filepath.IsAbs(resource.MountPath) || filepath.Clean(resource.MountPath) != resource.MountPath {
		return "", fmt.Errorf("mount_path is not a clean absolute path: %q", resource.MountPath)
	}
	mountPath := filepath.Clean(resource.MountPath)
	if pathWithinSDK(s.workdir, mountPath) || pathWithinSDK(mountPath, s.workdir) {
		return "", errors.New("Memory mount_path cannot overlap the Session workdir")
	}
	parent := filepath.Dir(mountPath)
	parentInfo, err := os.Lstat(parent)
	if err != nil {
		return "", fmt.Errorf("inspect Memory mount parent %s: %w", parent, err)
	}
	if !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("Memory mount parent %s must be a directory, not a symlink", parent)
	}
	if _, err := os.Lstat(mountPath); err == nil {
		return "", fmt.Errorf("something already exists at Memory mount_path %s", mountPath)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("inspect Memory mount_path %s: %w", mountPath, err)
	}
	return mountPath, nil
}

func (s *SessionMemoryStores) downloadStore(
	ctx context.Context,
	resource MemoryStoreSessionResource,
	mountPath string,
) (_ *mountedMemoryStore, retErr error) {
	heads, err := s.listMemories(ctx, resource.MemoryStoreID, true)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(mountPath, 0o700); err != nil {
		return nil, fmt.Errorf("create Memory mount_path %s: %w", mountPath, err)
	}
	created := true
	defer func() {
		if retErr != nil && created {
			_ = os.RemoveAll(mountPath)
		}
	}()
	identity, err := os.Lstat(mountPath)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(mountPath)
	if err != nil {
		return nil, fmt.Errorf("open Memory mount_path %s: %w", mountPath, err)
	}
	store := &mountedMemoryStore{
		storeID: resource.MemoryStoreID, mountPath: mountPath,
		readOnly: resource.Access == "read_only", root: root, identity: identity,
		marker: markerContents(resource.MemoryStoreID), baseline: map[string]string{},
		refused: map[string]string{}, pendingDelete: map[string]time.Time{},
	}
	if err := store.writeFile(MemoryMarkerPath, store.marker); err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("write Memory Store marker: %w", err)
	}
	for _, head := range heads {
		if err := store.writeRemote(head); err != nil {
			_ = root.Close()
			return nil, err
		}
		store.baseline[memoryRelativePath(head.Path)] = string(head.ContentSHA256)
	}
	created = false
	s.logger.Info("downloaded Session Memory Store", "memory_store_id", store.storeID,
		"count", len(store.baseline), "mount_path", store.mountPath)
	return store, nil
}

func markerContents(storeID string) []byte {
	return []byte("mango-memory-store-v1\n" + storeID + "\n")
}

func memoryRelativePath(value MemoryPath) string {
	return strings.TrimPrefix(string(value), "/")
}

func validateMemoryRelativePath(value string) error {
	if value == "" || value == "." || filepath.IsAbs(value) || filepath.Clean(value) != value ||
		value == ".." || strings.HasPrefix(value, ".."+string(filepath.Separator)) {
		return fmt.Errorf("invalid Memory path %q", value)
	}
	if filepath.ToSlash(value) == MemoryMarkerPath {
		return fmt.Errorf("Memory path %q is reserved by the worker", value)
	}
	return nil
}

func (s *SessionMemoryStores) listMemories(ctx context.Context, storeID string, full bool) ([]Memory, error) {
	view, limit := "basic", basicMemoryPageSize
	if full {
		view, limit = "full", fullMemoryPageSize
	}
	iterator := s.client.ListMemoriesAutoPaging(ctx, storeID, ListMemoriesParams{
		Depth: Some[int64](0), Limit: Some(limit), View: Some(view),
	})
	result := make([]Memory, 0)
	seenPaths := map[string]struct{}{}
	for iterator.Next() {
		item := iterator.Value()
		if item.Memory == nil {
			continue
		}
		memory := *item.Memory
		if memory.ID == "" || memory.MemoryStoreID != storeID {
			return nil, errors.New("mango: Memory listing returned an invalid identity")
		}
		relative := memoryRelativePath(memory.Path)
		if filepath.ToSlash(relative) == MemoryMarkerPath {
			s.logger.Warn("server listed the reserved Memory Store marker; skipping it",
				"memory_store_id", storeID, "path", memory.Path)
			continue
		}
		if err := validateMemoryRelativePath(relative); err != nil {
			return nil, err
		}
		if _, duplicate := seenPaths[relative]; duplicate {
			return nil, fmt.Errorf("mango: Memory listing contains duplicate path %q", memory.Path)
		}
		seenPaths[relative] = struct{}{}
		if full {
			if memory.Content == nil || !utf8.ValidString(*memory.Content) || int64(len(*memory.Content)) > maxSessionMemoryBytes {
				return nil, fmt.Errorf("mango: Memory %s has invalid full content", memory.ID)
			}
			if memorySHA([]byte(*memory.Content)) != string(memory.ContentSHA256) {
				return nil, fmt.Errorf("mango: Memory %s content checksum mismatch", memory.ID)
			}
		}
		result = append(result, memory)
		if len(result) > maxSessionMemoryFiles {
			return nil, errors.New("mango: Memory Store exceeds the 2000-file limit")
		}
	}
	if err := iterator.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// SyncIfDue performs a best-effort two-way reconciliation after a tool call.
// Individual Store failures are logged so one unavailable Store does not stop
// Session execution.
func (s *SessionMemoryStores) SyncIfDue(ctx context.Context) {
	if s.syncInterval < 0 || s.now().Sub(s.lastSync) < s.syncInterval {
		return
	}
	for _, store := range s.stores {
		if err := s.syncStore(ctx, store, false); err != nil {
			s.logger.Warn("Session Memory Store sync failed", "memory_store_id", store.storeID, "error", err)
		}
	}
	s.lastSync = s.now()
}

// Finish runs the final full reconciliation. The delete confirmation delay is
// waived because no later periodic pass is guaranteed.
func (s *SessionMemoryStores) Finish(ctx context.Context) {
	if s.finished {
		return
	}
	s.finished = true
	for _, store := range s.stores {
		if err := s.syncStore(ctx, store, true); err != nil {
			s.logger.Warn("final Session Memory Store sync failed", "memory_store_id", store.storeID, "error", err)
		}
	}
}

// FlushWrites is the cancellation rescue pass. It uploads new and changed
// files but intentionally performs no pulls, local removals, or server deletes.
func (s *SessionMemoryStores) FlushWrites(ctx context.Context) {
	for _, store := range s.stores {
		if err := s.flushStore(ctx, store); err != nil {
			s.logger.Warn("Session Memory Store flush failed", "memory_store_id", store.storeID, "error", err)
		}
	}
}

// Cleanup closes the Memory lifecycle after the caller has closed its tools.
// Fresh timeout contexts are used because the Session execution context is
// commonly already cancelled during teardown.
func (s *SessionMemoryStores) Cleanup(cleanEnd bool) {
	if cleanEnd {
		ctx, cancel := context.WithTimeout(context.Background(), MemoryFlushTimeout)
		s.Finish(ctx)
		if ctx.Err() != nil {
			s.logger.Warn("final Session Memory Store sync reached its timeout", "timeout", MemoryFlushTimeout)
		}
		cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), MemoryFlushTimeout)
	s.FlushWrites(ctx)
	if ctx.Err() != nil {
		s.logger.Warn("Session Memory Store flush reached its timeout", "timeout", MemoryFlushTimeout)
	}
	cancel()
	if err := s.Dispose(); err != nil {
		s.logger.Warn("Session Memory Store cleanup failed", "error", err)
	}
}

func (s *SessionMemoryStores) syncStore(ctx context.Context, store *mountedMemoryStore, final bool) error {
	local, trusted, err := store.scan()
	if err != nil {
		return err
	}
	if !trusted {
		if len(local) != 0 {
			s.logger.Warn("Memory Store marker is missing or altered; leaving the directory untouched",
				"memory_store_id", store.storeID, "mount_path", store.mountPath)
			return nil
		}
		return s.rebuildStore(ctx, store)
	}
	if len(local) == 0 && len(store.baseline) > 1 {
		return s.rebuildStore(ctx, store)
	}
	remote, err := s.listMemories(ctx, store.storeID, false)
	if err != nil {
		return err
	}
	remoteByPath := make(map[string]Memory, len(remote))
	for _, item := range remote {
		remoteByPath[memoryRelativePath(item.Path)] = item
	}
	deleteLimit := max(minimumDeleteBatch, min(maximumDeleteBatch, len(store.baseline)/4))
	deletesAttempted := 0
	for _, relative := range memoryPathUnion(store.baseline, local, remoteByPath) {
		if err := ctx.Err(); err != nil {
			return err
		}
		baseSHA, hadBase := store.baseline[relative]
		localSHA, hasLocal := local[relative]
		remoteItem, hasRemote := remoteByPath[relative]
		remoteSHA := string(remoteItem.ContentSHA256)

		if hasLocal {
			delete(store.pendingDelete, relative)
		}
		if hasLocal && hasRemote && localSHA == remoteSHA {
			store.baseline[relative] = remoteSHA
			delete(store.refused, relative)
			continue
		}
		if !hasLocal && hadBase && hasRemote && remoteSHA == baseSHA {
			if !store.readOnly && s.syncLocalDeletion(ctx, store, relative, remoteItem, baseSHA, final, &deletesAttempted, deleteLimit) {
				delete(store.baseline, relative)
			}
			continue
		}
		if !hasRemote {
			switch {
			case !hasLocal:
				delete(store.baseline, relative)
			case hadBase && localSHA == baseSHA:
				if store.removeUnchanged(relative, baseSHA) {
					delete(store.baseline, relative)
				}
			case store.readOnly:
				// A local edit in a read-only Store stays local but is never
				// uploaded. Forget the deleted remote baseline so a later remote
				// recreation is recognized even if it has the old checksum.
				delete(store.baseline, relative)
			case store.refused[relative] == localSHA:
			default:
				if created, ok := s.uploadMemory(ctx, store, relative, localSHA, nil); ok {
					store.baseline[relative] = string(created.ContentSHA256)
				}
			}
			continue
		}

		remoteChanged := !hadBase || remoteSHA != baseSHA
		localChanged := hasLocal && (!hadBase || localSHA != baseSHA)
		switch {
		case !hasLocal:
			if pulled, ok := s.pullMemory(ctx, store, remoteItem); ok {
				store.baseline[relative] = pulled
			}
		case remoteChanged:
			if localChanged {
				s.logger.Warn("Memory changed both locally and remotely; keeping the Store version",
					"memory_store_id", store.storeID, "path", "/"+relative)
			}
			if pulled, ok := s.pullMemory(ctx, store, remoteItem); ok {
				store.baseline[relative] = pulled
			}
		case localChanged && !store.readOnly && store.refused[relative] != localSHA:
			if updated, ok := s.uploadMemory(ctx, store, relative, localSHA, &remoteItem); ok {
				store.baseline[relative] = string(updated.ContentSHA256)
			}
		default:
			store.baseline[relative] = remoteSHA
		}
	}
	return nil
}

func (s *SessionMemoryStores) syncLocalDeletion(
	ctx context.Context,
	store *mountedMemoryStore,
	relative string,
	remote Memory,
	baseSHA string,
	final bool,
	attempted *int,
	limit int,
) bool {
	if s.syncDeletions == MemorySyncDeletionsDisabled {
		return false
	}
	first, observed := store.pendingDelete[relative]
	if !observed {
		first = s.now()
		store.pendingDelete[relative] = first
	}
	if !final && s.now().Sub(first) < MemoryDeleteConfirmDelay {
		return false
	}
	_, present, err := store.hashFile(relative)
	if err != nil || present || !store.markerValid() {
		return false
	}
	if *attempted >= limit {
		return false
	}
	(*attempted)++
	if s.syncDeletions == MemorySyncDeletionsLogOnly {
		s.logger.Info("Memory deletion sync is log-only", "memory_store_id", store.storeID, "path", "/"+relative)
		return false
	}
	_, err = s.client.DeleteMemory(ctx, store.storeID, remote.ID, DeleteMemoryParams{
		ExpectedContentSHA256: Some(baseSHA),
	})
	if err == nil || isAPIStatus(err, 404) {
		delete(store.pendingDelete, relative)
		return true
	}
	if isAPIStatus(err, 409) || isAPIStatus(err, 412) {
		s.logger.Warn("Memory was deleted locally but changed remotely; keeping the Store version",
			"memory_store_id", store.storeID, "path", "/"+relative)
		return false
	}
	s.logger.Warn("Memory deletion upload failed", "memory_store_id", store.storeID,
		"path", "/"+relative, "error", err)
	return false
}

func (s *SessionMemoryStores) flushStore(ctx context.Context, store *mountedMemoryStore) error {
	if store.readOnly {
		return nil
	}
	local, trusted, err := store.scan()
	if err != nil || !trusted {
		if err != nil {
			return err
		}
		return errors.New("Memory Store marker is missing or altered")
	}
	remote, err := s.listMemories(ctx, store.storeID, false)
	if err != nil {
		return err
	}
	remoteByPath := make(map[string]Memory, len(remote))
	for _, item := range remote {
		remoteByPath[memoryRelativePath(item.Path)] = item
	}
	paths := make([]string, 0, len(local))
	for relative := range local {
		paths = append(paths, relative)
	}
	sort.Strings(paths)
	for _, relative := range paths {
		localSHA := local[relative]
		if store.refused[relative] == localSHA || store.baseline[relative] == localSHA {
			continue
		}
		remoteItem, exists := remoteByPath[relative]
		if exists && string(remoteItem.ContentSHA256) == localSHA {
			store.baseline[relative] = localSHA
			continue
		}
		baseSHA, hadBase := store.baseline[relative]
		if exists && (!hadBase || string(remoteItem.ContentSHA256) != baseSHA) {
			s.logger.Warn("Memory changed both locally and remotely; cancellation flush keeps the Store version",
				"memory_store_id", store.storeID, "path", "/"+relative)
			continue
		}
		var existing *Memory
		if exists {
			existing = &remoteItem
		}
		if uploaded, ok := s.uploadMemory(ctx, store, relative, localSHA, existing); ok {
			store.baseline[relative] = string(uploaded.ContentSHA256)
		}
	}
	return nil
}

func (s *SessionMemoryStores) uploadMemory(
	ctx context.Context,
	store *mountedMemoryStore,
	relative, expectedLocalSHA string,
	existing *Memory,
) (Memory, bool) {
	content, sha, present, err := store.readFile(relative)
	if err != nil || !present || sha != expectedLocalSHA {
		if err != nil {
			if present {
				store.refused[relative] = expectedLocalSHA
			}
			s.logger.Warn("read Memory file for upload failed", "memory_store_id", store.storeID,
				"path", "/"+relative, "error", err)
		}
		return Memory{}, false
	}
	if !utf8.Valid(content) {
		store.refused[relative] = sha
		s.logger.Warn("Memory file is not UTF-8 and will not be retried until it changes",
			"memory_store_id", store.storeID, "path", "/"+relative)
		return Memory{}, false
	}
	var result Memory
	if existing == nil {
		result, err = s.client.CreateMemory(ctx, store.storeID, CreateMemoryParams{View: Some("full")}, MemoryCreateRequest{
			Path: MemoryPath("/" + filepath.ToSlash(relative)), Content: string(content),
		})
	} else {
		result, err = s.client.UpdateMemory(ctx, store.storeID, existing.ID, UpdateMemoryParams{View: Some("full")}, MemoryUpdateRequest{
			Content: Some(string(content)), Precondition: Some(MemoryPrecondition{
				Type: "content_sha256", ContentSHA256: existing.ContentSHA256,
			}),
		})
	}
	if err != nil {
		if isAPIStatus(err, 400) || isAPIStatus(err, 413) {
			store.refused[relative] = sha
			s.logger.Warn("Store rejected a Memory file; it will not be retried until it changes",
				"memory_store_id", store.storeID, "path", "/"+relative, "error", err)
		} else if isAPIStatus(err, 409) || isAPIStatus(err, 412) {
			s.logger.Warn("Memory upload lost a concurrent Store update",
				"memory_store_id", store.storeID, "path", "/"+relative)
		} else {
			s.logger.Warn("Memory upload failed", "memory_store_id", store.storeID,
				"path", "/"+relative, "error", err)
		}
		return Memory{}, false
	}
	delete(store.refused, relative)
	return result, true
}

func (s *SessionMemoryStores) pullMemory(ctx context.Context, store *mountedMemoryStore, listed Memory) (string, bool) {
	full, err := s.client.GetMemory(ctx, store.storeID, listed.ID, GetMemoryParams{View: Some("full")})
	if err != nil {
		s.logger.Warn("Memory download failed", "memory_store_id", store.storeID,
			"path", listed.Path, "error", err)
		return "", false
	}
	if full.ID != listed.ID || full.MemoryStoreID != store.storeID || full.Path != listed.Path ||
		full.ContentSHA256 != listed.ContentSHA256 ||
		full.Content == nil || memorySHA([]byte(*full.Content)) != string(full.ContentSHA256) {
		s.logger.Warn("Memory changed during download; retrying on the next sync",
			"memory_store_id", store.storeID, "path", listed.Path)
		return "", false
	}
	if err := store.writeRemote(full); err != nil {
		s.logger.Warn("write downloaded Memory failed", "memory_store_id", store.storeID,
			"path", listed.Path, "error", err)
		return "", false
	}
	return string(full.ContentSHA256), true
}

func (s *SessionMemoryStores) rebuildStore(ctx context.Context, store *mountedMemoryStore) error {
	if err := store.ensureMounted(); err != nil {
		return err
	}
	local, _, err := store.scan()
	if err != nil {
		return err
	}
	if len(local) != 0 {
		return errors.New("Memory Store directory is not empty and cannot be rebuilt safely")
	}
	heads, err := s.listMemories(ctx, store.storeID, true)
	if err != nil {
		return err
	}
	if err := store.writeFile(MemoryMarkerPath, store.marker); err != nil {
		return err
	}
	store.baseline = map[string]string{}
	store.pendingDelete = map[string]time.Time{}
	for _, head := range heads {
		if err := store.writeRemote(head); err != nil {
			return err
		}
		store.baseline[memoryRelativePath(head.Path)] = string(head.ContentSHA256)
	}
	return nil
}

func (store *mountedMemoryStore) scan() (map[string]string, bool, error) {
	if err := store.ensureMounted(); err != nil {
		return nil, false, err
	}
	files := map[string]string{}
	markerOK := false
	err := fs.WalkDir(store.root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == "." || entry.IsDir() {
			return nil
		}
		if path == MemoryMarkerPath {
			content, err := readRootFile(store.root, path)
			if err != nil {
				return err
			}
			markerOK = string(content) == string(store.marker)
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("Memory path %q is a symbolic link", path)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("Memory path %q is not a regular file", path)
		}
		sha, present, err := store.hashFile(path)
		if err != nil {
			return err
		}
		if present {
			files[path] = sha
		}
		if len(files) > maxSessionMemoryFiles {
			return errors.New("Memory Store exceeds the 2000-file limit")
		}
		return nil
	})
	return files, markerOK, err
}

func (store *mountedMemoryStore) markerValid() bool {
	content, err := readRootFile(store.root, MemoryMarkerPath)
	return err == nil && string(content) == string(store.marker)
}

func (store *mountedMemoryStore) ensureMounted() error {
	current, err := os.Lstat(store.mountPath)
	if os.IsNotExist(err) {
		if closeErr := store.root.Close(); closeErr != nil {
			return closeErr
		}
		if err := os.Mkdir(store.mountPath, 0o700); err != nil {
			return fmt.Errorf("recreate Memory mount_path: %w", err)
		}
		current, err = os.Lstat(store.mountPath)
		if err != nil {
			return err
		}
		root, err := os.OpenRoot(store.mountPath)
		if err != nil {
			return err
		}
		store.root, store.identity = root, current
		return nil
	}
	if err != nil {
		return err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, store.identity) {
		return errors.New("Memory mount_path was replaced during Session execution")
	}
	return nil
}

func (store *mountedMemoryStore) readFile(relative string) ([]byte, string, bool, error) {
	if err := validateMemoryRelativePath(relative); err != nil {
		return nil, "", false, err
	}
	info, err := store.root.Lstat(relative)
	if os.IsNotExist(err) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	if !info.Mode().IsRegular() {
		return nil, "", false, fmt.Errorf("Memory path %q is not a regular file", relative)
	}
	if info.Size() > maxSessionMemoryBytes {
		return nil, "", true, fmt.Errorf("Memory file %q exceeds the 102400-byte limit", relative)
	}
	file, err := store.root.Open(relative)
	if err != nil {
		return nil, "", true, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxSessionMemoryBytes+1))
	closeErr := file.Close()
	if readErr != nil {
		return nil, "", true, readErr
	}
	if closeErr != nil {
		return nil, "", true, closeErr
	}
	if int64(len(data)) > maxSessionMemoryBytes {
		return nil, "", true, fmt.Errorf("Memory file %q exceeds the 102400-byte limit", relative)
	}
	return data, memorySHA(data), true, nil
}

func (store *mountedMemoryStore) hashFile(relative string) (string, bool, error) {
	if err := validateMemoryRelativePath(relative); err != nil {
		return "", false, err
	}
	info, err := store.root.Lstat(relative)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !info.Mode().IsRegular() {
		return "", false, fmt.Errorf("Memory path %q is not a regular file", relative)
	}
	file, err := store.root.Open(relative)
	if err != nil {
		return "", true, err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, file)
	closeErr := file.Close()
	if copyErr != nil {
		return "", true, copyErr
	}
	if closeErr != nil {
		return "", true, closeErr
	}
	return hex.EncodeToString(hash.Sum(nil)), true, nil
}

func (store *mountedMemoryStore) removeUnchanged(relative, expectedSHA string) bool {
	sha, present, err := store.hashFile(relative)
	if err != nil {
		return false
	}
	if !present {
		return true
	}
	if sha != expectedSHA {
		return false
	}
	return store.root.Remove(relative) == nil
}

func (store *mountedMemoryStore) writeRemote(memory Memory) error {
	if memory.Content == nil {
		return fmt.Errorf("Memory %s has no content", memory.ID)
	}
	relative := memoryRelativePath(memory.Path)
	if err := validateMemoryRelativePath(relative); err != nil {
		return err
	}
	content := []byte(*memory.Content)
	if !utf8.Valid(content) || int64(len(content)) > maxSessionMemoryBytes {
		return fmt.Errorf("Memory %s has invalid full content", memory.ID)
	}
	if memorySHA(content) != string(memory.ContentSHA256) {
		return fmt.Errorf("Memory %s content checksum mismatch", memory.ID)
	}
	return store.writeFile(relative, content)
}

func (store *mountedMemoryStore) writeFile(relative string, data []byte) error {
	if err := validateMemoryRelativePath(relative); err != nil && relative != MemoryMarkerPath {
		return err
	}
	directory := filepath.Dir(relative)
	if directory != "." {
		if err := mkdirAllRoot(store.root, directory, 0o700); err != nil {
			return err
		}
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return err
	}
	temporary := filepath.Join(directory, ".mango-memory-"+hex.EncodeToString(random))
	file, err := store.root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr == nil {
		writeErr = closeErr
	}
	if writeErr != nil {
		_ = store.root.Remove(temporary)
		return writeErr
	}
	if err := replaceRootFile(store.root, temporary, relative); err != nil {
		_ = store.root.Remove(temporary)
		return err
	}
	return nil
}

func readRootFile(root *os.Root, name string) ([]byte, error) {
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(file)
	closeErr := file.Close()
	return data, errors.Join(readErr, closeErr)
}

func mkdirAllRoot(root *os.Root, name string, mode fs.FileMode) error {
	if name == "." {
		return nil
	}
	parent := filepath.Dir(name)
	if parent != "." {
		if err := mkdirAllRoot(root, parent, mode); err != nil {
			return err
		}
	}
	if err := root.Mkdir(name, mode); err != nil {
		info, statErr := root.Lstat(name)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return err
		}
	}
	return nil
}

// Dispose removes only mount directories that are still the same filesystem
// objects created by Download. A replaced path is left for the operator.
func (s *SessionMemoryStores) Dispose() (result error) {
	for index := len(s.stores) - 1; index >= 0; index-- {
		store := s.stores[index]
		current, statErr := os.Lstat(store.mountPath)
		same := statErr == nil && current.IsDir() && current.Mode()&os.ModeSymlink == 0 && os.SameFile(current, store.identity)
		if same {
			local, trusted, scanErr := store.scan()
			if scanErr != nil {
				result = errors.Join(result, scanErr)
				same = false
			} else if !trusted && len(local) != 0 {
				s.logger.Warn("Memory Store marker is missing or altered; leaving the directory on disk",
					"memory_store_id", store.storeID, "mount_path", store.mountPath)
				same = false
			}
		}
		if err := store.root.Close(); err != nil {
			result = errors.Join(result, err)
		}
		if same {
			if err := os.RemoveAll(store.mountPath); err != nil {
				result = errors.Join(result, err)
			}
		} else if statErr != nil && !os.IsNotExist(statErr) {
			result = errors.Join(result, statErr)
		}
	}
	s.stores = nil
	return result
}

func memoryPathUnion(base, local map[string]string, remote map[string]Memory) []string {
	seen := make(map[string]struct{}, len(base)+len(local)+len(remote))
	for value := range base {
		seen[value] = struct{}{}
	}
	for value := range local {
		seen[value] = struct{}{}
	}
	for value := range remote {
		seen[value] = struct{}{}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func memorySHA(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

func pathWithinSDK(root, value string) bool {
	relative, err := filepath.Rel(root, value)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}
