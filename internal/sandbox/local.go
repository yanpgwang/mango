package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// maxOutput caps the bytes captured from stdout and stderr, independently.
const maxOutput = 100_000

// localProvider provisions sandboxes backed by the host filesystem and a plain
// child process. See the package doc: DEV-GRADE GUARDRAIL, not a security
// boundary. baseDir is stable across provider instances so a new worker process
// can attach to the workspace named by a persisted reference.
type localProvider struct {
	baseDir string
}

// NewLocalProvider returns a Provider that runs commands as local child
// processes confined (best-effort) to a working directory.
//
// This is a dev-grade guardrail, NOT a security boundary: it shares the host
// kernel and filesystem namespace, and offers no network isolation. Do NOT run
// untrusted code with it.
func NewLocalProvider() Provider {
	return &localProvider{baseDir: filepath.Join(os.TempDir(), "mango-sandboxes")}
}

func (p *localProvider) Name() string { return LocalProviderName }

// Package installation is intentionally disabled: local execution shares the
// worker host and must never mutate its system or language package stores.
func (*localProvider) SupportsPackageSetup() bool { return false }

func (p *localProvider) Create(
	ctx context.Context,
	sessionKey string,
	spec Spec,
) (Ref, Sandbox, error) {
	if err := ctx.Err(); err != nil {
		return Ref{}, nil, err
	}
	if sessionKey == "" {
		return Ref{}, nil, errors.New("sandbox: session key is required")
	}
	root := p.rootFor(sessionKey, spec)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Ref{}, nil, fmt.Errorf("sandbox: create root: %w", err)
	}
	box, err := p.attachRoot(root, spec)
	if err != nil {
		return Ref{}, nil, err
	}
	return Ref{Provider: p.Name(), ID: box.Root()}, box, nil
}

func (p *localProvider) Attach(
	ctx context.Context,
	sessionKey string,
	ref Ref,
	spec Spec,
) (Sandbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sessionKey == "" {
		return nil, Permanent(errors.New("sandbox: session key is required"))
	}
	if err := ref.validate(); err != nil {
		return nil, Permanent(err)
	}
	if ref.Provider != p.Name() {
		return nil, Permanent(fmt.Errorf(
			"sandbox: local provider cannot attach reference for %q",
			ref.Provider,
		))
	}
	expectedRoot, err := canonicalLocalPath(p.rootFor(sessionKey, spec))
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve expected local workspace: %w", err)
	}
	referencedRoot, err := canonicalLocalPath(ref.ID)
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve referenced local workspace: %w", err)
	}
	if referencedRoot != expectedRoot {
		return nil, Permanent(fmt.Errorf(
			"sandbox: local workspace %q belongs to another session",
			ref.ID,
		))
	}
	info, err := os.Stat(ref.ID)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: local workspace %q", ErrNotFound, ref.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("sandbox: stat local workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("sandbox: local workspace %q is not a directory", ref.ID)
	}
	return p.attachRoot(ref.ID, spec)
}

// canonicalLocalPath resolves symlinks through the deepest existing ancestor.
// This lets Attach validate session ownership before reporting a missing
// deterministic workspace, even when an operator removed the whole local
// provider base directory.
func canonicalLocalPath(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	current := absolute
	var missing []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func (p *localProvider) rootFor(sessionKey string, spec Spec) string {
	if spec.WorkDir != "" {
		return spec.WorkDir
	}
	sum := sha256.Sum256([]byte(sessionKey))
	return filepath.Join(p.baseDir, fmt.Sprintf("session-%x", sum[:16]))
}

func (p *localProvider) attachRoot(root string, spec Spec) (Sandbox, error) {
	// Resolve symlinks so confinement checks compare canonical paths (e.g. on
	// darwin /tmp is a symlink to /private/tmp).
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve root: %w", err)
	}
	return &localSandbox{
		root:    resolved,
		timeout: spec.Timeout,
	}, nil
}

type localSandbox struct {
	root    string
	timeout time.Duration
}

func (s *localSandbox) Root() string { return s.root }

// resolve joins path onto root and verifies the cleaned result stays within
// root, rejecting ".." escapes and absolute paths that point outside root.
func (s *localSandbox) resolve(path string) (string, error) {
	clean := filepath.Clean(filepath.Join(s.root, path))
	sep := string(filepath.Separator)
	if clean != s.root && !strings.HasPrefix(clean+sep, s.root+sep) {
		return "", fmt.Errorf("sandbox: path %q escapes root", path)
	}
	return clean, nil
}

func (s *localSandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	full, err := s.resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

func (s *localSandbox) ReadFileBounded(
	ctx context.Context,
	path string,
	maxBytes int64,
) ([]byte, bool, error) {
	full, err := s.resolve(path)
	if err != nil {
		return nil, false, err
	}
	return readFileBoundedByCommand(ctx, s, full, maxBytes)
}

func (s *localSandbox) WriteFile(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	full, err := s.resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return err
	}
	return os.WriteFile(full, data, 0o600)
}

func (s *localSandbox) Exec(ctx context.Context, cmd Command) (*Result, error) {
	// Bound the command by the sandbox timeout, while still honoring an
	// already-cancelled parent context.
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}

	// exec.CommandContext kills the child process when ctx is done, so
	// cancellation and timeout both terminate the subprocess.
	c := exec.CommandContext(ctx, cmd.Path, cmd.Args...)
	c.Dir = s.root
	// Minimal, cleared environment: nothing from the host is inherited.
	c.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + s.root}
	if len(cmd.Stdin) > 0 {
		c.Stdin = bytes.NewReader(cmd.Stdin)
	}
	stdout := &cappedBuffer{cap: maxOutput}
	stderr := &cappedBuffer{cap: maxOutput}
	c.Stdout = stdout
	c.Stderr = stderr

	runErr := c.Run()

	res := &Result{
		Stdout: stdout.Bytes(),
		Stderr: stderr.Bytes(),
	}

	// Timeout / cancellation: the context deadline elapsing means the child was
	// killed by CommandContext.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		res.TimedOut = true
		res.ExitCode = -1
		return res, nil
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return res, ctx.Err()
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			res.ExitCode = exitErr.ExitCode()
			return res, nil
		}
		return res, fmt.Errorf("sandbox: exec: %w", runErr)
	}
	res.ExitCode = c.ProcessState.ExitCode()
	return res, nil
}

func (s *localSandbox) Destroy(ctx context.Context) error {
	// Idempotent: RemoveAll returns nil if root is already gone.
	return os.RemoveAll(s.root)
}

// cappedBuffer accumulates up to cap bytes, then drops the rest and records a
// truncation note appended once at the tail.
type cappedBuffer struct {
	buf       bytes.Buffer
	cap       int
	truncated bool
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	if remaining := w.cap - w.buf.Len(); remaining > 0 {
		if len(p) <= remaining {
			return w.buf.Write(p)
		}
		w.buf.Write(p[:remaining])
		w.truncated = true
	} else {
		w.truncated = true
	}
	// Report the full length as consumed so the child process is not blocked by
	// a short write once the cap is reached.
	return len(p), nil
}

func (w *cappedBuffer) Bytes() []byte {
	if w.truncated {
		return append(w.buf.Bytes(), []byte("\n[output truncated]")...)
	}
	return w.buf.Bytes()
}
