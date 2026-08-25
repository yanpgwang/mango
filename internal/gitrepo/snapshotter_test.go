package gitrepo

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestMapCloneErrorSeparatesPermanentAndTemporaryFailures(t *testing.T) {
	missing := mapCloneError(context.Background(), transport.ErrRepositoryNotFound)
	var classified interface{ DeploymentRunErrorType() string }
	var domainErr *domain.DomainError
	if !errors.As(missing, &classified) ||
		classified.DeploymentRunErrorType() != "session_resource_not_found_error" ||
		!errors.As(missing, &domainErr) || domainErr.Kind != domain.KindValidation ||
		domainErr.Code != "" {
		t.Fatalf("mapped missing repository error = %#v", missing)
	}

	for _, cause := range []error{
		errors.New("connection reset"),
		transport.ErrAuthorizationFailed,
	} {
		temporary := mapCloneError(context.Background(), cause)
		classified = nil
		domainErr = nil
		if errors.As(temporary, &classified) || errors.As(temporary, &domainErr) {
			t.Fatalf("temporary clone error was made permanent = %#v", temporary)
		}
	}
}

func TestWriteSnapshotArchivePreservesGitMetadataAndExecutableFiles(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(t.TempDir(), "snapshot.tar")
	if err := writeSnapshotArchive(context.Background(), root, archivePath); err != nil {
		t.Fatal(err)
	}
	archive, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close() //nolint:errcheck
	reader := tar.NewReader(archive)
	entries := map[string]os.FileMode{}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = header.FileInfo().Mode()
	}
	if mode, ok := entries[".git/HEAD"]; !ok || !mode.IsRegular() {
		t.Fatalf(".git/HEAD archive mode = %v, present=%v", mode, ok)
	}
	if mode, ok := entries["run.sh"]; !ok || mode.Perm()&0o111 == 0 {
		t.Fatalf("run.sh archive mode = %v, present=%v", mode, ok)
	}
}

func TestWriteSnapshotArchiveRejectsEscapingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions differ on Windows")
	}
	root := filepath.Join(t.TempDir(), "repository")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../outside", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	err := writeSnapshotArchive(
		context.Background(), root, filepath.Join(t.TempDir(), "snapshot.tar"),
	)
	if err == nil {
		t.Fatal("escaping symlink was accepted")
	}
}
