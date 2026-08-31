package sandboxtest

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/sandbox"
	"github.com/yanpgwang/mango/internal/testutil/dockertest"
)

// DockerProvider tracks created resources for bounded cleanup even when a test
// fails before its Session deletion path. This is a real Mango Docker provider.
func DockerProvider(t testing.TB, configs ...sandbox.DockerConfig) sandbox.Provider {
	t.Helper()
	dockertest.Require(t)
	cfg := sandbox.DockerConfig{}
	if len(configs) > 1 {
		t.Fatal("at most one Docker test configuration is supported")
	}
	if len(configs) == 1 {
		cfg = configs[0]
	}
	if cfg.ResourceBaseDir == "" {
		cfg.ResourceBaseDir = t.TempDir()
	}
	provider, err := sandbox.NewDockerProvider(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tracked := &trackedProvider{Provider: provider}
	t.Cleanup(func() {
		tracked.mu.Lock()
		defer tracked.mu.Unlock()
		for _, box := range tracked.boxes {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			if err := box.Destroy(ctx); err != nil {
				t.Errorf("clean up Docker sandbox: %v", err)
			}
			cancel()
		}
	})
	return tracked
}

type trackedProvider struct {
	sandbox.Provider
	mu    sync.Mutex
	boxes []sandbox.Sandbox
}

func (p *trackedProvider) Create(ctx context.Context, key string, spec sandbox.Spec) (sandbox.Ref, sandbox.Sandbox, error) {
	ref, box, err := p.Provider.Create(ctx, key, spec)
	if err == nil {
		p.mu.Lock()
		p.boxes = append(p.boxes, box)
		p.mu.Unlock()
	}
	return ref, box, err
}

func (p *trackedProvider) SupportsPackageSetup() bool    { return true }
func (p *trackedProvider) SupportsFileResources() bool   { return true }
func (p *trackedProvider) SupportsSessionOutputs() bool  { return true }
func (p *trackedProvider) SupportsSkillBundles() bool    { return true }
func (p *trackedProvider) SupportsMemoryStores() bool    { return true }
func (p *trackedProvider) SupportsGitRepositories() bool { return true }

func Docker(t testing.TB) sandbox.Sandbox {
	t.Helper()
	provider := DockerProvider(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, box, err := provider.Create(ctx, fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()), sandbox.Spec{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return box
}

// MountSkill publishes an immutable fixture through the real provider boundary.
func MountSkill(t testing.TB, box sandbox.Sandbox, runtimePath, body string) {
	t.Helper()
	name := path.Base(runtimePath)
	var archive bytes.Buffer
	w := zip.NewWriter(&archive)
	f, err := w.Create(name + "/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(archive.Bytes())
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := box.(sandbox.SkillBundleSandbox).ImportReadOnlySkill(ctx, sandbox.ReadOnlySkillMount{
		Identity: "test-skill-" + name, Name: name, RuntimePath: runtimePath, ArchiveRoot: name,
		SizeBytes: int64(archive.Len()), UncompressedSizeBytes: int64(len(body)), ChecksumSHA256: fmt.Sprintf("%x", sum),
	}, bytes.NewReader(archive.Bytes())); err != nil {
		t.Fatal(err)
	}
}

// NoProvision makes text-only/custom-tool tests fail if they accidentally
// acquire a sandbox. It cannot execute commands or create a workspace.
func NoProvision(t testing.TB) sandbox.Provider { return noProvision{t: t} }

type noProvision struct{ t testing.TB }

func (noProvision) Name() string { return "no-sandbox-test" }
func (p noProvision) Create(context.Context, string, sandbox.Spec) (sandbox.Ref, sandbox.Sandbox, error) {
	p.t.Error("unexpected sandbox provisioning in a non-executing test")
	return sandbox.Ref{}, nil, fmt.Errorf("sandbox provisioning forbidden by test")
}
func (p noProvision) Attach(context.Context, string, sandbox.Ref, sandbox.Spec) (sandbox.Sandbox, error) {
	p.t.Error("unexpected sandbox attachment in a non-executing test")
	return nil, fmt.Errorf("sandbox attachment forbidden by test")
}

// Inert supplies only sandbox identity to protocol tests. Unexpected command or
// file operations fail the test, even if the caller handles the returned error.
func Inert(t testing.TB) sandbox.Sandbox { return inertSandbox{t: t} }

type inertSandbox struct{ t testing.TB }

func (s inertSandbox) unexpected() error {
	s.t.Error("unexpected sandbox I/O in a non-executing test")
	return fmt.Errorf("sandbox I/O forbidden by test")
}
func (s inertSandbox) Exec(context.Context, sandbox.Command) (*sandbox.Result, error) {
	return nil, s.unexpected()
}
func (s inertSandbox) ReadFile(context.Context, string) ([]byte, error) { return nil, s.unexpected() }
func (s inertSandbox) WriteFile(context.Context, string, []byte) error  { return s.unexpected() }
func (inertSandbox) Root() string                                       { return "/workspace" }
func (inertSandbox) Destroy(context.Context) error                      { return nil }
