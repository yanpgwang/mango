// Package gitrepo resolves public Git remotes into immutable, bounded Session
// snapshots. Network access stays in the control plane; sandbox adapters only
// receive the resulting provider-neutral tar archive.
package gitrepo

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	gitclient "github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitstorage "github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpegress"
)

const (
	cloneWriteBudget = 1 << 30
	cloneEntryBudget = 200_000
	cloneTimeout     = 5 * time.Minute
)

var (
	errCloneWriteBudget = errors.New("git repository clone exceeds its write budget")
	installHTTPSOnce    sync.Once
)

type Snapshotter struct {
	tempDir string
}

// NewSnapshotter installs one process-wide go-git HTTPS transport backed by
// Mango's public-only dialer. The control plane constructs this once before it
// starts serving concurrent requests.
func NewSnapshotter(tempDir string) *Snapshotter {
	installHTTPSOnce.Do(func() {
		client := httpegress.NewPublicClient(cloneTimeout)
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many Git HTTP redirects")
			}
			if req.URL.Scheme != "https" || req.URL.User != nil {
				return errors.New("git HTTP redirect must remain anonymous HTTPS")
			}
			return nil
		}
		gitclient.InstallProtocol("https", githttp.NewClient(client))
	})
	return &Snapshotter{tempDir: tempDir}
}

func (s *Snapshotter) OpenSnapshot(
	ctx context.Context,
	request app.GitRepositorySnapshotRequest,
) (app.GitRepositorySnapshot, error) {
	if err := domain.ValidateGitRepositoryURL(request.URL); err != nil {
		return app.GitRepositorySnapshot{}, err
	}
	checkoutType, checkoutValue, err := domain.NormalizeGitRepositoryCheckout(
		request.CheckoutType, request.CheckoutValue,
	)
	if err != nil {
		return app.GitRepositorySnapshot{}, err
	}
	snapshotCtx, cancel := context.WithTimeout(ctx, cloneTimeout)
	defer cancel()
	root, err := os.MkdirTemp(s.tempDir, "mango-git-snapshot-")
	if err != nil {
		return app.GitRepositorySnapshot{}, fmt.Errorf("git repository: create snapshot workspace: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(root) }
	repositoryDir := filepath.Join(root, "repository")
	if err := os.Mkdir(repositoryDir, 0o700); err != nil {
		cleanup()
		return app.GitRepositorySnapshot{}, fmt.Errorf("git repository: create clone directory: %w", err)
	}

	budget := &writeBudget{
		remaining: cloneWriteBudget, entriesRemaining: cloneEntryBudget,
	}
	worktree := &budgetFilesystem{
		Filesystem: osfs.New(repositoryDir, osfs.WithBoundOS()),
		budget:     budget,
	}
	if err := worktree.MkdirAll(".git", 0o700); err != nil {
		cleanup()
		return app.GitRepositorySnapshot{}, fmt.Errorf("git repository: create metadata directory: %w", err)
	}
	dotGit, err := worktree.Chroot(".git")
	if err != nil {
		cleanup()
		return app.GitRepositorySnapshot{}, fmt.Errorf("git repository: isolate metadata directory: %w", err)
	}
	storage := gitstorage.NewStorageWithOptions(
		dotGit,
		cache.NewObjectLRUDefault(),
		gitstorage.Options{LargeObjectThreshold: 64 << 20},
	)
	defer func() { _ = storage.Close() }()

	options := &git.CloneOptions{URL: request.URL}
	switch checkoutType {
	case domain.GitRepositoryCheckoutBranch:
		options.ReferenceName = plumbing.NewBranchReferenceName(checkoutValue)
		options.SingleBranch = true
	case domain.GitRepositoryCheckoutCommit:
		options.NoCheckout = true
	}
	repository, err := git.CloneContext(snapshotCtx, storage, worktree, options)
	if err != nil {
		cleanup()
		return app.GitRepositorySnapshot{}, mapCloneError(snapshotCtx, err)
	}

	var resolved plumbing.Hash
	if checkoutType == domain.GitRepositoryCheckoutCommit {
		resolved = plumbing.NewHash(checkoutValue)
		if _, err := repository.CommitObject(resolved); err != nil {
			cleanup()
			return app.GitRepositorySnapshot{}, domain.SessionResourceNotFound(
				"checkout.sha is not reachable from the repository's advertised refs",
			)
		}
		worktreeHandle, err := repository.Worktree()
		if err != nil {
			cleanup()
			return app.GitRepositorySnapshot{}, fmt.Errorf("git repository: open worktree: %w", err)
		}
		if err := worktreeHandle.Checkout(&git.CheckoutOptions{Hash: resolved}); err != nil {
			cleanup()
			return app.GitRepositorySnapshot{}, fmt.Errorf("git repository: checkout commit: %w", err)
		}
	} else {
		head, err := repository.Head()
		if err != nil {
			cleanup()
			return app.GitRepositorySnapshot{}, domain.SessionResourceNotFound(
				"repository has no resolvable default commit",
			)
		}
		resolved = head.Hash()
	}

	archivePath := filepath.Join(root, "snapshot.tar")
	if err := writeSnapshotArchive(snapshotCtx, repositoryDir, archivePath); err != nil {
		cleanup()
		if errors.Is(err, app.ErrBlobTooLarge) {
			return app.GitRepositorySnapshot{}, domain.TooLarge(
				"Git repository snapshot exceeds the 500 MB Session Resource limit",
			)
		}
		return app.GitRepositorySnapshot{}, err
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		cleanup()
		return app.GitRepositorySnapshot{}, fmt.Errorf("git repository: open snapshot archive: %w", err)
	}
	return app.GitRepositorySnapshot{
		ResolvedCommit: resolved.String(),
		Archive:        &cleanupReadCloser{ReadCloser: archive, cleanup: cleanup},
	}, nil
}

func mapCloneError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errCloneWriteBudget) {
		return domain.TooLarge("Git repository exceeds the clone safety limit")
	}
	return domain.SessionResourceNotFound(
		"public Git repository could not be cloned: " + err.Error(),
	)
}

func writeSnapshotArchive(ctx context.Context, root, archivePath string) error {
	archive, err := os.OpenFile(archivePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("git repository: create snapshot archive: %w", err)
	}
	limited := &archiveLimitWriter{writer: archive, remaining: app.MaxSessionResourceBytes}
	w := tar.NewWriter(limited)
	entryCount := 0
	walkErr := filepath.WalkDir(root, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if filePath == root {
			return nil
		}
		entryCount++
		if entryCount > 100_000 {
			return app.ErrBlobTooLarge
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("git repository: unsupported filesystem entry %q", filePath)
		}
		name, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		name = filepath.ToSlash(name)
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(filePath)
			if err != nil {
				return err
			}
			resolved := filepath.Clean(filepath.Join(filepath.Dir(name), link))
			if filepath.IsAbs(link) || resolved == ".." || strings.HasPrefix(
				filepath.ToSlash(resolved), "../",
			) {
				return fmt.Errorf("git repository: symlink %q escapes the repository", name)
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = name
		if info.IsDir() {
			header.Name += "/"
		}
		header.Uid, header.Gid = 0, 0
		header.Uname, header.Gname = "", ""
		header.ModTime = time.Unix(0, 0).UTC()
		header.AccessTime, header.ChangeTime = time.Time{}, time.Time{}
		if err := w.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(filePath)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(w, file)
		closeErr := file.Close()
		return errors.Join(copyErr, closeErr)
	})
	closeTarErr := w.Close()
	closeArchiveErr := archive.Close()
	if err := errors.Join(walkErr, closeTarErr, closeArchiveErr); err != nil {
		_ = os.Remove(archivePath)
		return fmt.Errorf("git repository: build snapshot archive: %w", err)
	}
	return nil
}

type cleanupReadCloser struct {
	io.ReadCloser
	cleanup func()
}

func (r *cleanupReadCloser) Close() error {
	err := r.ReadCloser.Close()
	r.cleanup()
	return err
}

type archiveLimitWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *archiveLimitWriter) Write(value []byte) (int, error) {
	if int64(len(value)) > w.remaining {
		return 0, app.ErrBlobTooLarge
	}
	n, err := w.writer.Write(value)
	w.remaining -= int64(n)
	return n, err
}

type writeBudget struct {
	mu               sync.Mutex
	remaining        int64
	entriesRemaining int64
}

func (b *writeBudget) consumeEntry() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.entriesRemaining <= 0 {
		return errCloneWriteBudget
	}
	b.entriesRemaining--
	return nil
}

func (b *writeBudget) consume(size int64) error {
	if size < 0 {
		return errCloneWriteBudget
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if size > b.remaining {
		return errCloneWriteBudget
	}
	b.remaining -= size
	return nil
}

type budgetFilesystem struct {
	billy.Filesystem
	budget *writeBudget
}

func (f *budgetFilesystem) Create(name string) (billy.File, error) {
	if err := f.budget.consumeEntry(); err != nil {
		return nil, err
	}
	file, err := f.Filesystem.Create(name)
	return f.wrap(file), err
}

func (f *budgetFilesystem) OpenFile(name string, flag int, perm os.FileMode) (billy.File, error) {
	if flag&os.O_CREATE != 0 {
		if err := f.budget.consumeEntry(); err != nil {
			return nil, err
		}
	}
	file, err := f.Filesystem.OpenFile(name, flag, perm)
	return f.wrap(file), err
}

func (f *budgetFilesystem) TempFile(dir, prefix string) (billy.File, error) {
	if err := f.budget.consumeEntry(); err != nil {
		return nil, err
	}
	file, err := f.Filesystem.TempFile(dir, prefix)
	return f.wrap(file), err
}

func (f *budgetFilesystem) Symlink(target, link string) error {
	if err := f.budget.consumeEntry(); err != nil {
		return err
	}
	return f.Filesystem.Symlink(target, link)
}

func (f *budgetFilesystem) MkdirAll(name string, perm os.FileMode) error {
	if err := f.budget.consumeEntry(); err != nil {
		return err
	}
	return f.Filesystem.MkdirAll(name, perm)
}

func (f *budgetFilesystem) Chroot(path string) (billy.Filesystem, error) {
	nested, err := f.Filesystem.Chroot(path)
	if err != nil {
		return nil, err
	}
	return &budgetFilesystem{Filesystem: nested, budget: f.budget}, nil
}

func (f *budgetFilesystem) wrap(file billy.File) billy.File {
	if file == nil {
		return nil
	}
	return &budgetFile{File: file, budget: f.budget}
}

type budgetFile struct {
	billy.File
	budget *writeBudget
}

func (f *budgetFile) Write(value []byte) (int, error) {
	if err := f.budget.consume(int64(len(value))); err != nil {
		return 0, err
	}
	return f.File.Write(value)
}

func (f *budgetFile) Truncate(size int64) error {
	if err := f.budget.consume(size); err != nil {
		return err
	}
	return f.File.Truncate(size)
}

var _ app.GitRepositorySnapshotter = (*Snapshotter)(nil)

// Compile-time checks keep the wrappers aligned with go-billy upgrades.
var _ billy.Filesystem = (*budgetFilesystem)(nil)
var _ billy.File = (*budgetFile)(nil)
