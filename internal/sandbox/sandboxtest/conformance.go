// Package sandboxtest provides the lifecycle conformance suite every Mango
// sandbox provider must pass.
package sandboxtest

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/sandbox"
)

// RunFileResources exercises the provider-neutral File Resource materialization
// contract. It is intentionally separate from Run because providers must opt in
// to the absolute uploads path and runtime add/delete lifecycle.
func RunFileResources(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.NewProvider == nil {
		t.Fatal("sandboxtest: NewProvider is required")
	}
	if cfg.Spec.Timeout == 0 {
		cfg.Spec.Timeout = 30 * time.Second
	}
	if cfg.ShellPath == "" {
		cfg.ShellPath = "/bin/sh"
	}
	ctx := context.Background()
	provider := cfg.NewProvider(t)
	capability, ok := provider.(sandbox.FileResourceProvider)
	if !ok || !capability.SupportsFileResources() {
		t.Fatalf("provider %q does not advertise File Resources", provider.Name())
	}
	session := sessionKey(t)
	ref, box, err := provider.Create(ctx, session, cfg.Spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Destroy(context.Background()) })
	resources, ok := box.(sandbox.FileResourceSandbox)
	if !ok {
		t.Fatalf("provider %q sandbox does not expose File Resources", provider.Name())
	}

	content := []byte("portable File Resource\n")
	mount := fileResourceMount("/mnt/session/uploads/nested/data.txt", "sesrsc_first", content)
	present, err := resources.HasFileResource(ctx, mount)
	if err != nil || present {
		t.Fatalf("initial HasFileResource = %t, %v", present, err)
	}
	if err := resources.ImportFileResource(ctx, mount, bytes.NewReader(content)); err != nil {
		t.Fatalf("ImportFileResource: %v", err)
	}
	assertMountedFile(t, ctx, box, resources, mount, content, cfg.ShellPath)

	replacementContent := []byte("replacement\n")
	replacement := fileResourceMount(mount.RuntimePath, "sesrsc_second", replacementContent)
	if err := resources.ImportFileResource(
		ctx, replacement, &failingReader{data: []byte("partial")},
	); err == nil {
		t.Fatal("partial replacement unexpectedly succeeded")
	}
	if err := resources.RemoveFileResource(
		ctx, replacement.RuntimePath, mount.Identity,
	); err != nil {
		t.Fatalf("stale removal during interrupted replacement: %v", err)
	}
	if err := resources.RemoveFileResource(
		ctx, replacement.RuntimePath, replacement.Identity,
	); err != nil {
		t.Fatalf("remove interrupted replacement: %v", err)
	}
	if err := resources.ImportFileResource(
		ctx, replacement, bytes.NewReader(replacementContent),
	); err != nil {
		t.Fatalf("replace File Resource: %v", err)
	}
	if err := resources.RemoveFileResource(
		ctx, replacement.RuntimePath, mount.Identity,
	); err != nil {
		t.Fatalf("stale identity removal: %v", err)
	}
	assertMountedFile(t, ctx, box, resources, replacement, replacementContent, cfg.ShellPath)

	restarted := cfg.NewProvider(t)
	attached, err := restarted.Attach(ctx, session, ref, cfg.Spec)
	if err != nil {
		t.Fatalf("Attach File Resource sandbox: %v", err)
	}
	attachedResources, ok := attached.(sandbox.FileResourceSandbox)
	if !ok {
		t.Fatalf("attached provider %q sandbox lost File Resources", provider.Name())
	}
	assertMountedFile(
		t, ctx, attached, attachedResources, replacement, replacementContent, cfg.ShellPath,
	)
	if err := attachedResources.RemoveFileResource(
		ctx, replacement.RuntimePath, replacement.Identity,
	); err != nil {
		t.Fatalf("RemoveFileResource: %v", err)
	}
	if err := attachedResources.RemoveFileResource(
		ctx, replacement.RuntimePath, replacement.Identity,
	); err != nil {
		t.Fatalf("idempotent RemoveFileResource: %v", err)
	}
	present, err = attachedResources.HasFileResource(ctx, replacement)
	if err != nil || present {
		t.Fatalf("HasFileResource after removal = %t, %v", present, err)
	}

	invalid := fileResourceMount("/mnt/session/uploads/../escape", "sesrsc_invalid", content)
	if err := attachedResources.ImportFileResource(
		ctx, invalid, bytes.NewReader(content),
	); err == nil || !sandbox.IsPermanent(err) {
		t.Fatalf("invalid File Resource path error = %v, want permanent", err)
	}
}

// RunGitRepositories exercises the durable, writable, offline repository
// restore contract shared by every capable provider.
func RunGitRepositories(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.NewProvider == nil {
		t.Fatal("sandboxtest: NewProvider is required")
	}
	if cfg.Spec.Timeout == 0 {
		cfg.Spec.Timeout = 30 * time.Second
	}
	if cfg.ShellPath == "" {
		cfg.ShellPath = "/bin/sh"
	}
	ctx := context.Background()
	provider := cfg.NewProvider(t)
	capability, ok := provider.(sandbox.GitRepositoryProvider)
	if !ok || !capability.SupportsGitRepositories() {
		t.Fatalf("provider %q does not advertise Git repositories", provider.Name())
	}
	session := sessionKey(t)
	ref, box, err := provider.Create(ctx, session, cfg.Spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Destroy(context.Background()) })
	repositories, ok := box.(sandbox.GitRepositorySandbox)
	if !ok {
		t.Fatalf("provider %q sandbox does not expose Git repositories", provider.Name())
	}
	archive := gitRepositoryArchive(t, map[string]string{
		".git/HEAD": "ref: refs/heads/main\n",
		"README.md": "canonical\n",
	})
	checksum := sha256.Sum256(archive)
	mount := sandbox.GitRepositoryMount{
		Identity: "sesrsc_repository", RuntimePath: "/workspace/mango-conformance-repo",
		ResolvedCommit: "0123456789abcdef0123456789abcdef01234567",
		SizeBytes:      int64(len(archive)), ChecksumSHA256: fmt.Sprintf("%x", checksum),
	}
	present, err := repositories.HasGitRepository(ctx, mount)
	if err != nil || present {
		t.Fatalf("initial HasGitRepository = %t, %v", present, err)
	}
	if err := repositories.ImportGitRepository(ctx, mount, bytes.NewReader(archive)); err != nil {
		t.Fatalf("ImportGitRepository: %v", err)
	}
	assertGitRepository(t, ctx, box, repositories, mount, "canonical\n", cfg.ShellPath)
	if err := box.WriteFile(ctx, mount.RuntimePath+"/README.md", []byte("agent edit\n")); err != nil {
		t.Fatalf("edit Git repository: %v", err)
	}
	if err := repositories.ImportGitRepository(ctx, mount, bytes.NewReader(archive)); err != nil {
		t.Fatalf("retry Git repository import: %v", err)
	}
	assertGitRepository(t, ctx, box, repositories, mount, "agent edit\n", cfg.ShellPath)

	restarted := cfg.NewProvider(t)
	attached, err := restarted.Attach(ctx, session, ref, cfg.Spec)
	if err != nil {
		t.Fatalf("Attach Git repository sandbox: %v", err)
	}
	attachedRepositories, ok := attached.(sandbox.GitRepositorySandbox)
	if !ok {
		t.Fatalf("attached provider %q sandbox lost Git repositories", provider.Name())
	}
	assertGitRepository(
		t, ctx, attached, attachedRepositories, mount, "agent edit\n", cfg.ShellPath,
	)
	if err := attachedRepositories.RemoveGitRepository(
		ctx, mount.RuntimePath, "sesrsc_stale",
	); err != nil {
		t.Fatalf("stale Git repository removal: %v", err)
	}
	assertGitRepository(
		t, ctx, attached, attachedRepositories, mount, "agent edit\n", cfg.ShellPath,
	)
	if err := attachedRepositories.RemoveGitRepository(
		ctx, mount.RuntimePath, mount.Identity,
	); err != nil {
		t.Fatalf("RemoveGitRepository: %v", err)
	}
	present, err = attachedRepositories.HasGitRepository(ctx, mount)
	if err != nil || present {
		t.Fatalf("HasGitRepository after removal = %t, %v", present, err)
	}
}

func assertGitRepository(
	t *testing.T,
	ctx context.Context,
	box sandbox.Sandbox,
	repositories sandbox.GitRepositorySandbox,
	mount sandbox.GitRepositoryMount,
	want string,
	shellPath string,
) {
	t.Helper()
	present, err := repositories.HasGitRepository(ctx, mount)
	if err != nil || !present {
		t.Fatalf("HasGitRepository = %t, %v", present, err)
	}
	content, err := box.ReadFile(ctx, mount.RuntimePath+"/README.md")
	if err != nil || string(content) != want {
		t.Fatalf("repository README = %q, %v", content, err)
	}
	result, err := box.Exec(ctx, sandbox.Command{
		Path: shellPath,
		Args: []string{"-c", "test -f " + mount.RuntimePath + "/.git/HEAD && cat " + mount.RuntimePath + "/README.md"},
	})
	if err != nil || result.ExitCode != 0 || string(result.Stdout) != want {
		t.Fatalf("shell read Git repository: result=%+v err=%v", result, err)
	}
}

func gitRepositoryArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var archive bytes.Buffer
	w := tar.NewWriter(&archive)
	if err := w.WriteHeader(&tar.Header{
		Name: ".git/", Typeflag: tar.TypeDir, Mode: 0o755,
	}); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := w.WriteHeader(&tar.Header{
			Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(content)),
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes()
}

func assertMountedFile(
	t *testing.T,
	ctx context.Context,
	box sandbox.Sandbox,
	resources sandbox.FileResourceSandbox,
	mount sandbox.FileResourceMount,
	want []byte,
	shellPath string,
) {
	t.Helper()
	present, err := resources.HasFileResource(ctx, mount)
	if err != nil || !present {
		t.Fatalf("HasFileResource = %t, %v", present, err)
	}
	got, err := box.ReadFile(ctx, mount.RuntimePath)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("ReadFile mounted resource = %q, %v", got, err)
	}
	result, err := box.Exec(ctx, sandbox.Command{
		Path: shellPath, Args: []string{"-c", "cat " + mount.RuntimePath},
	})
	if err != nil || result.ExitCode != 0 || !bytes.Equal(result.Stdout, want) {
		t.Fatalf("shell read mounted resource: result=%+v err=%v", result, err)
	}
}

func fileResourceMount(
	runtimePath string,
	identity string,
	content []byte,
) sandbox.FileResourceMount {
	checksum := sha256.Sum256(content)
	return sandbox.FileResourceMount{
		Identity: identity, RuntimePath: runtimePath,
		SizeBytes: int64(len(content)), ChecksumSHA256: fmt.Sprintf("%x", checksum[:]),
	}
}

type failingReader struct {
	data []byte
	done bool
}

func (r *failingReader) Read(buffer []byte) (int, error) {
	if !r.done {
		r.done = true
		return copy(buffer, r.data), nil
	}
	return 0, errors.New("injected stream failure")
}

// Factory returns a fresh client for the same provider deployment. It may call
// t.Skip when an optional daemon or credential is unavailable.
type Factory func(t *testing.T) sandbox.Provider

// Config describes the portable POSIX surface exercised by the suite.
type Config struct {
	NewProvider Factory
	Spec        sandbox.Spec
	ShellPath   string
}

// RunSessionOutputs exercises the provider-neutral contract required before a
// provider may advertise SessionOutputs. Adapters that cannot pass this suite
// must remain fail-closed in the capability registry.
func RunSessionOutputs(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.NewProvider == nil {
		t.Fatal("sandboxtest: NewProvider is required")
	}
	if cfg.Spec.Timeout == 0 {
		cfg.Spec.Timeout = 30 * time.Second
	}
	if cfg.ShellPath == "" {
		cfg.ShellPath = "/bin/sh"
	}
	ctx := context.Background()
	provider := cfg.NewProvider(t)
	capability, ok := provider.(sandbox.SessionOutputProvider)
	if !ok || !capability.SupportsSessionOutputs() {
		t.Fatalf("provider %q does not advertise Session outputs", provider.Name())
	}
	_, box, err := provider.Create(ctx, sessionKey(t), cfg.Spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Destroy(context.Background()) })
	exporter, ok := box.(sandbox.SessionOutputSandbox)
	if !ok {
		t.Fatalf("provider %q sandbox does not export Session outputs", provider.Name())
	}
	locker, ok := box.(sandbox.ResourceSynchronizationSandbox)
	if !ok {
		t.Fatalf("provider %q sandbox cannot lock output snapshots", provider.Name())
	}
	if err := box.WriteFile(
		ctx, sandbox.SessionOutputsRoot+"/nested/tool.txt", []byte("tool"),
	); err != nil {
		t.Fatalf("write output through tool boundary: %v", err)
	}
	result, err := box.Exec(ctx, sandbox.Command{
		Path: cfg.ShellPath,
		Args: []string{"-c", "printf shell > /mnt/session/outputs/shell.txt"},
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("write output through shell boundary: result=%+v err=%v", result, err)
	}
	unlock, err := locker.LockResourceOperation(ctx)
	if err != nil {
		t.Fatalf("lock output snapshot: %v", err)
	}
	defer unlock()
	first := readSessionOutputArchive(t, ctx, exporter)
	second := readSessionOutputArchive(t, ctx, exporter)
	want := map[string]string{"nested/tool.txt": "tool", "shell.txt": "shell"}
	if !maps.Equal(first, want) || !maps.Equal(second, want) {
		t.Fatalf("repeatable output snapshots = first:%v second:%v want:%v", first, second, want)
	}
}

func readSessionOutputArchive(
	t *testing.T,
	ctx context.Context,
	exporter sandbox.SessionOutputSandbox,
) map[string]string {
	t.Helper()
	stream, err := exporter.OpenSessionOutputs(ctx)
	if err != nil {
		t.Fatalf("OpenSessionOutputs: %v", err)
	}
	defer func() { _ = stream.Close() }()
	files := make(map[string]string)
	reader := tar.NewReader(stream)
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			return files
		}
		if err != nil {
			t.Fatalf("read output archive: %v", err)
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != 0 {
			t.Fatalf("output archive entry %q type = %d", header.Name, header.Typeflag)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("read output %q: %v", header.Name, err)
		}
		files[strings.TrimPrefix(header.Name, "./")] = string(body)
	}
}

// Run exercises the provider behavior required by SessionManager and the
// built-in tool runtime. Provider-specific isolation and capability tests remain
// alongside the adapter.
func Run(t *testing.T, cfg Config) {
	t.Helper()
	if cfg.NewProvider == nil {
		t.Fatal("sandboxtest: NewProvider is required")
	}
	if cfg.Spec.Timeout == 0 {
		cfg.Spec.Timeout = 30 * time.Second
	}
	if cfg.ShellPath == "" {
		cfg.ShellPath = "/bin/sh"
	}

	t.Run("stable_identity", func(t *testing.T) {
		first := cfg.NewProvider(t)
		second := cfg.NewProvider(t)
		if first == nil || second == nil {
			t.Fatal("provider factory returned nil")
		}
		if first.Name() == "" {
			t.Fatal("provider name is empty")
		}
		if second.Name() != first.Name() {
			t.Fatalf(
				"provider name changed across clients: %q != %q",
				first.Name(),
				second.Name(),
			)
		}
	})

	t.Run("execution_and_files", func(t *testing.T) {
		ctx := context.Background()
		provider := cfg.NewProvider(t)
		_, box, err := provider.Create(ctx, sessionKey(t), cfg.Spec)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = box.Destroy(context.Background()) })

		if box.Root() == "" {
			t.Fatal("sandbox root is empty")
		}
		content := []byte{'d', 'u', 'r', 'a', 'b', 'l', 'e', 0, '\n'}
		if err := box.WriteFile(ctx, "nested/state.bin", content); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, err := box.ReadFile(ctx, "nested/state.bin")
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !bytes.Equal(got, content) {
			t.Fatalf("file round trip = %q, want %q", got, content)
		}
		bounded, ok := box.(sandbox.BoundedFileReader)
		if !ok {
			t.Fatal("sandbox does not implement bounded file reads")
		}
		prefix, truncated, err := bounded.ReadFileBounded(ctx, "nested/state.bin", 4)
		if err != nil {
			t.Fatalf("ReadFileBounded: %v", err)
		}
		if !truncated || !bytes.Equal(prefix, content[:4]) {
			t.Fatalf("bounded file read = %q, %v; want %q, true", prefix, truncated, content[:4])
		}
		exact, truncated, err := bounded.ReadFileBounded(
			ctx, "nested/state.bin", int64(len(content)),
		)
		if err != nil {
			t.Fatalf("exact ReadFileBounded: %v", err)
		}
		if truncated || !bytes.Equal(exact, content) {
			t.Fatalf("exact bounded file read = %q, %v; want %q, false", exact, truncated, content)
		}
		if _, _, err := bounded.ReadFileBounded(ctx, "../escape", 4); err == nil {
			t.Fatal("ReadFileBounded accepted a path outside the workspace")
		}

		result, err := box.Exec(ctx, sandbox.Command{
			Path: cfg.ShellPath,
			Args: []string{"-c", "printf conformance-exec"},
		})
		if err != nil {
			t.Fatalf("Exec: %v", err)
		}
		if result.ExitCode != 0 || string(result.Stdout) != "conformance-exec" {
			t.Fatalf("Exec result = %+v", result)
		}
		result, err = box.Exec(ctx, sandbox.Command{
			Path: cfg.ShellPath,
			Args: []string{"-c", "exit 7"},
		})
		if err != nil {
			t.Fatalf("Exec non-zero exit: %v", err)
		}
		if result.ExitCode != 7 {
			t.Fatalf("non-zero exit code = %d, want 7", result.ExitCode)
		}

		if _, err := box.ReadFile(ctx, "../escape"); err == nil {
			t.Fatal("ReadFile accepted a path outside the workspace")
		}
		if err := box.WriteFile(ctx, "../escape", []byte("x")); err == nil {
			t.Fatal("WriteFile accepted a path outside the workspace")
		}

		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := box.Exec(cancelled, sandbox.Command{
			Path: cfg.ShellPath,
			Args: []string{"-c", "true"},
		}); !errors.Is(err, context.Canceled) {
			t.Fatalf("Exec with cancelled context = %v, want context.Canceled", err)
		}
	})

	t.Run("idempotent_create_and_restart_attach", func(t *testing.T) {
		ctx := context.Background()
		session := sessionKey(t)
		firstProvider := cfg.NewProvider(t)
		firstRef, first, err := firstProvider.Create(ctx, session, cfg.Spec)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = first.Destroy(context.Background()) })
		if firstRef.Provider != firstProvider.Name() || firstRef.ID == "" {
			t.Fatalf("invalid durable reference: %+v", firstRef)
		}
		if err := first.WriteFile(ctx, "restart.txt", []byte("preserved")); err != nil {
			t.Fatal(err)
		}

		restartedProvider := cfg.NewProvider(t)
		sameRef, same, err := restartedProvider.Create(ctx, session, cfg.Spec)
		if err != nil {
			t.Fatalf("repeated Create: %v", err)
		}
		if sameRef != firstRef {
			t.Fatalf("repeated Create ref = %+v, want %+v", sameRef, firstRef)
		}
		assertFile(t, ctx, same, "restart.txt", "preserved")

		attached, err := restartedProvider.Attach(ctx, session, firstRef, cfg.Spec)
		if err != nil {
			t.Fatalf("Attach: %v", err)
		}
		assertFile(t, ctx, attached, "restart.txt", "preserved")
	})

	t.Run("ownership_and_missing_reference", func(t *testing.T) {
		ctx := context.Background()
		session := sessionKey(t)
		provider := cfg.NewProvider(t)
		ref, box, err := provider.Create(ctx, session, cfg.Spec)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = box.Destroy(context.Background()) })

		if _, err := provider.Attach(
			ctx,
			session+"-other",
			ref,
			cfg.Spec,
		); err == nil || !sandbox.IsPermanent(err) {
			t.Fatalf("cross-session Attach = %v, want permanent ownership error", err)
		}
		if _, err := provider.Attach(
			ctx,
			session,
			sandbox.Ref{Provider: "wrong-provider", ID: ref.ID},
			cfg.Spec,
		); err == nil || !sandbox.IsPermanent(err) {
			t.Fatalf("wrong-provider Attach = %v, want permanent error", err)
		}

	})

	t.Run("idempotent_destroy", func(t *testing.T) {
		ctx := context.Background()
		session := sessionKey(t)
		provider := cfg.NewProvider(t)
		ref, box, err := provider.Create(ctx, session, cfg.Spec)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = box.Destroy(context.Background()) })

		if err := box.Destroy(ctx); err != nil {
			t.Fatalf("first Destroy: %v", err)
		}
		if err := box.Destroy(ctx); err != nil {
			t.Fatalf("repeated Destroy: %v", err)
		}
		restartedProvider := cfg.NewProvider(t)
		if _, err := restartedProvider.Attach(
			ctx,
			session,
			ref,
			cfg.Spec,
		); !errors.Is(err, sandbox.ErrNotFound) {
			t.Fatalf("Attach after Destroy = %v, want ErrNotFound", err)
		}
	})
}

func assertFile(
	t *testing.T,
	ctx context.Context,
	box sandbox.Sandbox,
	path string,
	want string,
) {
	t.Helper()
	got, err := box.ReadFile(ctx, path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	if string(got) != want {
		t.Fatalf("ReadFile(%q) = %q, want %q", path, got, want)
	}
}

func sessionKey(t *testing.T) string {
	t.Helper()
	name := strings.NewReplacer(
		"/", "-",
		" ", "-",
		"_", "-",
	).Replace(strings.ToLower(t.Name()))
	return fmt.Sprintf("sesn-conformance-%s-%d", name, time.Now().UnixNano())
}
