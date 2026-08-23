package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandGitRepositoriesRestoresWritableSnapshotIdempotently(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	control := filepath.Join(root, "control")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	mapPath := func(value string) string {
		value = strings.ReplaceAll(value, gitRepositoryControlRoot, control)
		value = strings.ReplaceAll(value, domainWorkspaceRootForTest, workspace)
		return value
	}
	loseRestoreAcknowledgement := true
	execute := func(ctx context.Context, command Command) (*Result, error) {
		restore := len(command.Args) == 2 && strings.Contains(command.Args[1], "tar -xf ")
		args := append([]string(nil), command.Args...)
		for index := range args {
			args[index] = mapPath(args[index])
		}
		process := exec.CommandContext(ctx, command.Path, args...)
		process.Stdin = bytes.NewReader(command.Stdin)
		var stdout, stderr bytes.Buffer
		process.Stdout, process.Stderr = &stdout, &stderr
		err := process.Run()
		exitCode := 0
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return nil, err
			}
			exitCode = exitErr.ExitCode()
		}
		if restore && exitCode == 0 && loseRestoreAcknowledgement {
			loseRestoreAcknowledgement = false
			return nil, errors.New("simulated lost restore acknowledgement")
		}
		return &Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitCode}, nil
	}
	upload := func(_ context.Context, destination string, content io.Reader, _ int64) error {
		destination = mapPath(destination)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		file, err := os.Create(destination)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, content)
		return errors.Join(copyErr, file.Close())
	}

	archive := repositoryArchiveForTest(t, map[string]string{
		".git/HEAD": "ref: refs/heads/main\n",
		"README.md": "hello\n",
	})
	sum := sha256.Sum256(archive)
	mount := GitRepositoryMount{
		Identity: "sesrsc_repository", RuntimePath: "/workspace/repository",
		ResolvedCommit: "0123456789abcdef0123456789abcdef01234567",
		SizeBytes:      int64(len(archive)), ChecksumSHA256: fmt.Sprintf("%x", sum),
	}
	repositories := newCommandGitRepositories("test", execute, upload)
	if err := repositories.ImportGitRepository(
		context.Background(), mount, bytes.NewReader(archive),
	); err == nil {
		t.Fatal("restore unexpectedly acknowledged its first publication")
	}
	if err := repositories.ImportGitRepository(
		context.Background(), mount, bytes.NewReader(archive),
	); err != nil {
		t.Fatalf("adopt published repository after lost acknowledgement: %v", err)
	}
	present, err := repositories.HasGitRepository(context.Background(), mount)
	if err != nil || !present {
		t.Fatalf("HasGitRepository = %v, %v", present, err)
	}
	readme := filepath.Join(workspace, "repository", "README.md")
	if err := os.WriteFile(readme, []byte("agent edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repositories.ImportGitRepository(
		context.Background(), mount, bytes.NewReader(archive),
	); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(readme)
	if err != nil || string(content) != "agent edit\n" {
		t.Fatalf("retry replaced writable worktree: %q, %v", content, err)
	}
	if err := repositories.RemoveGitRepository(
		context.Background(), mount.RuntimePath, "sesrsc_newer",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(readme); err != nil {
		t.Fatalf("stale removal removed repository: %v", err)
	}
	if err := repositories.RemoveGitRepository(
		context.Background(), mount.RuntimePath, mount.Identity,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "repository")); !os.IsNotExist(err) {
		t.Fatalf("repository remains after removal: %v", err)
	}
}

func TestCommandGitRepositoriesCleansStagingAfterTargetCollision(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	control := filepath.Join(root, "control")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	mapPath := func(value string) string {
		value = strings.ReplaceAll(value, gitRepositoryControlRoot, control)
		return strings.ReplaceAll(value, domainWorkspaceRootForTest, workspace)
	}
	execute := func(ctx context.Context, command Command) (*Result, error) {
		args := append([]string(nil), command.Args...)
		for index := range args {
			args[index] = mapPath(args[index])
		}
		if len(command.Args) == 2 && strings.Contains(command.Args[1], "tar -xf ") {
			if err := os.WriteFile(filepath.Join(workspace, "repository"), []byte("collision\n"), 0o644); err != nil {
				return nil, err
			}
		}
		process := exec.CommandContext(ctx, command.Path, args...)
		process.Stdin = bytes.NewReader(command.Stdin)
		var stdout, stderr bytes.Buffer
		process.Stdout, process.Stderr = &stdout, &stderr
		err := process.Run()
		exitCode := 0
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return nil, err
			}
			exitCode = exitErr.ExitCode()
		}
		return &Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitCode}, nil
	}
	upload := func(_ context.Context, destination string, content io.Reader, _ int64) error {
		destination = mapPath(destination)
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		file, err := os.Create(destination)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, content)
		return errors.Join(copyErr, file.Close())
	}

	archive := repositoryArchiveForTest(t, map[string]string{
		".git/HEAD": "ref: refs/heads/main\n",
		"README.md": "hello\n",
	})
	sum := sha256.Sum256(archive)
	mount := GitRepositoryMount{
		Identity: "sesrsc_repository", RuntimePath: "/workspace/repository",
		ResolvedCommit: "0123456789abcdef0123456789abcdef01234567",
		SizeBytes:      int64(len(archive)), ChecksumSHA256: fmt.Sprintf("%x", sum),
	}
	repositories := newCommandGitRepositories("test", execute, upload)
	err := repositories.ImportGitRepository(context.Background(), mount, bytes.NewReader(archive))
	if err == nil || !IsPermanent(err) {
		t.Fatalf("collision error = %v, want permanent", err)
	}
	archivePath, stagingPath := gitRepositoryStagingPaths(mount.RuntimePath, mount.Identity)
	for _, candidate := range []string{archivePath, stagingPath} {
		if _, err := os.Lstat(mapPath(candidate)); !os.IsNotExist(err) {
			t.Fatalf("staging artifact %s remains after collision: %v", candidate, err)
		}
	}
	content, err := os.ReadFile(filepath.Join(workspace, "repository"))
	if err != nil || string(content) != "collision\n" {
		t.Fatalf("collision target changed: %q, %v", content, err)
	}
}

func TestGitRepositoryStagingTreeIsTargetSibling(t *testing.T) {
	const target = "/workspace/projects/repository"
	archivePath, stagingPath := gitRepositoryStagingPaths(target, "sesrsc_repository")
	if path.Dir(stagingPath) != path.Dir(target) {
		t.Fatalf("staging tree %q is not a target sibling", stagingPath)
	}
	if !strings.HasPrefix(archivePath, gitRepositoryStagingRoot+"/") {
		t.Fatalf("archive path %q escaped the private staging root", archivePath)
	}
	if strings.HasPrefix(stagingPath, gitRepositoryStagingRoot+"/") {
		t.Fatalf("staging tree %q remained under the private archive root", stagingPath)
	}
}

func TestValidateGitRepositoryArchiveRejectsEscapingSymlink(t *testing.T) {
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	for _, header := range []*tar.Header{
		{Name: ".git/HEAD", Typeflag: tar.TypeReg, Mode: 0o644, Size: 5},
		{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "../../etc", Mode: 0o777},
	} {
		if err := w.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size > 0 {
			if _, err := w.Write([]byte("HEAD\n")); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := validateGitRepositoryArchive(context.Background(), bytes.NewReader(archive.Bytes())); err == nil || !IsPermanent(err) {
		t.Fatalf("validation error = %v, want permanent", err)
	}
}

func TestCommandGitRepositoriesRejectsSymlinkedWorkspaceAncestor(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	control := filepath.Join(root, "control")
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "linked")); err != nil {
		t.Fatal(err)
	}
	mapPath := func(value string) string {
		value = strings.ReplaceAll(value, gitRepositoryControlRoot, control)
		return strings.ReplaceAll(value, domainWorkspaceRootForTest, workspace)
	}
	execute := func(ctx context.Context, command Command) (*Result, error) {
		args := append([]string(nil), command.Args...)
		for index := range args {
			args[index] = mapPath(args[index])
		}
		process := exec.CommandContext(ctx, command.Path, args...)
		process.Stdin = bytes.NewReader(command.Stdin)
		var stdout, stderr bytes.Buffer
		process.Stdout, process.Stderr = &stdout, &stderr
		err := process.Run()
		exitCode := 0
		if err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return nil, err
			}
			exitCode = exitErr.ExitCode()
		}
		return &Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitCode}, nil
	}
	archive := repositoryArchiveForTest(t, map[string]string{
		".git/HEAD": "ref: refs/heads/main\n",
	})
	sum := sha256.Sum256(archive)
	mount := GitRepositoryMount{
		Identity: "sesrsc_repository", RuntimePath: "/workspace/linked/nested/repository",
		ResolvedCommit: "0123456789abcdef0123456789abcdef01234567",
		SizeBytes:      int64(len(archive)), ChecksumSHA256: fmt.Sprintf("%x", sum),
	}
	repositories := newCommandGitRepositories(
		"test", execute,
		func(context.Context, string, io.Reader, int64) error {
			t.Fatal("unsafe repository reached the upload boundary")
			return nil
		},
	)
	err := repositories.ImportGitRepository(context.Background(), mount, bytes.NewReader(archive))
	if err == nil || !IsPermanent(err) {
		t.Fatalf("symlinked ancestor error = %v, want permanent", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "nested")); !os.IsNotExist(err) {
		t.Fatalf("restore wrote through the workspace symlink: %v", err)
	}
}

func repositoryArchiveForTest(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	for _, directory := range []string{".git/", ""} {
		if directory == "" {
			continue
		}
		if err := w.WriteHeader(&tar.Header{
			Name: directory, Typeflag: tar.TypeDir, Mode: 0o755,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for name, value := range files {
		if err := w.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(value)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(value)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

const domainWorkspaceRootForTest = "/workspace"
