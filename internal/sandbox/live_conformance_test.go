package sandbox_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/sandbox"
	"github.com/yanpgwang/mango/internal/sandbox/sandboxtest"
)

func TestOpenSandboxLiveConformance(t *testing.T) {
	requireLive(t, "MANGO_LIVE_OPENSANDBOX")
	factory := newLiveOpenSandboxProvider
	t.Run("lifecycle", func(t *testing.T) { runLiveConformance(t, factory) })
	t.Run("file-resources", func(t *testing.T) { runLiveFileResourceConformance(t, factory) })
	t.Run("session-outputs", func(t *testing.T) { runLiveSessionOutputConformance(t, factory) })
	t.Run("skill-bundles", func(t *testing.T) { runLiveSkillBundleConformance(t, factory) })
	t.Run("git-repositories", func(t *testing.T) { runLiveGitRepositoryConformance(t, factory) })
	t.Run("memory-stores", func(t *testing.T) { runLiveMemoryStoreConformance(t, factory) })
}

// TestOpenSandboxKataLiveQualification verifies the provider and egress parts
// of the production-backend gate. The companion operator command inspects the
// Kubernetes RuntimeClass, BatchSandbox, and live Pod without introducing a
// host process executor into the sandbox runtime packages.
func TestOpenSandboxKataLiveQualification(t *testing.T) {
	requireLive(t, "MANGO_LIVE_OPENSANDBOX_KATA")
	allowedURL := requireLiveURL(t, "OPEN_SANDBOX_KATA_ALLOWED_URL")
	blockedURL := requireLiveURL(t, "OPEN_SANDBOX_KATA_BLOCKED_URL")
	if allowedURL.Hostname() == blockedURL.Hostname() {
		t.Fatalf(
			"OPEN_SANDBOX_KATA_ALLOWED_URL and OPEN_SANDBOX_KATA_BLOCKED_URL " +
				"must use different hosts",
		)
	}
	// The ordinary lifecycle and resource suites remain part of qualification;
	// Kata is an isolation implementation, not a replacement contract.
	t.Run("lifecycle", func(t *testing.T) { runLiveConformance(t, newLiveOpenSandboxProvider) })
	t.Run("file-resources", func(t *testing.T) { runLiveFileResourceConformance(t, newLiveOpenSandboxProvider) })
	t.Run("session-outputs", func(t *testing.T) { runLiveSessionOutputConformance(t, newLiveOpenSandboxProvider) })
	t.Run("skill-bundles", func(t *testing.T) { runLiveSkillBundleConformance(t, newLiveOpenSandboxProvider) })
	t.Run("git-repositories", func(t *testing.T) { runLiveGitRepositoryConformance(t, newLiveOpenSandboxProvider) })

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	provider := newLiveOpenSandboxProvider(t)

	// Establish that both targets are reachable without a policy. Otherwise a
	// blocked-target failure would be ambiguous evidence.
	bridgeRef, bridgeBox, err := provider.Create(ctx, liveSessionKey("bridge"), sandbox.Spec{
		Timeout: 2 * time.Minute,
		Network: "bridge",
	})
	if err != nil {
		t.Fatalf("create unrestricted OpenSandbox probe %s: %v", bridgeRef.ID, err)
	}
	t.Cleanup(func() { _ = bridgeBox.Destroy(context.Background()) })
	assertLiveHTTPProbe(t, ctx, bridgeBox, allowedURL.String(), true)
	assertLiveHTTPProbe(t, ctx, bridgeBox, blockedURL.String(), true)
	if err := bridgeBox.Destroy(ctx); err != nil {
		t.Fatalf("destroy unrestricted OpenSandbox probe: %v", err)
	}

	_, limitedBox, err := provider.Create(ctx, liveSessionKey("limited"), sandbox.Spec{
		Timeout:             2 * time.Minute,
		Network:             "limited",
		NetworkAllowedHosts: []string{allowedURL.Hostname()},
	})
	if err != nil {
		t.Fatalf("create limited OpenSandbox probe: %v", err)
	}
	t.Cleanup(func() { _ = limitedBox.Destroy(context.Background()) })
	assertLiveHTTPProbe(t, ctx, limitedBox, allowedURL.String(), true)
	assertLiveHTTPProbe(t, ctx, limitedBox, blockedURL.String(), false)
}

func newLiveOpenSandboxProvider(t *testing.T) sandbox.Provider {
	t.Helper()
	provider, err := sandbox.NewOpenSandboxProvider(sandbox.OpenSandboxConfig{
		BaseURL:  os.Getenv("OPEN_SANDBOX_DOMAIN"),
		APIKey:   os.Getenv("OPEN_SANDBOX_API_KEY"),
		Image:    os.Getenv("OPEN_SANDBOX_IMAGE"),
		UseProxy: liveEnvBool("OPEN_SANDBOX_USE_SERVER_PROXY"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider
}

func liveSessionKey(kind string) string {
	return fmt.Sprintf("mango-opensandbox-kata-%s-%d", kind, time.Now().UnixNano())
}

func assertLiveHTTPProbe(
	t *testing.T,
	ctx context.Context,
	box sandbox.Sandbox,
	target string,
	wantReachable bool,
) {
	t.Helper()
	const probe = `import sys, urllib.request
try:
    with urllib.request.urlopen(sys.argv[1], timeout=10) as response:
        if response.status >= 400:
            raise RuntimeError("HTTP status %d" % response.status)
except Exception as exc:
    print("%s: %s" % (type(exc).__name__, exc), file=sys.stderr)
    sys.exit(1)
`
	result, err := box.Exec(ctx, sandbox.Command{
		Path: "python",
		Args: []string{"-c", probe, target},
	})
	if err != nil {
		t.Fatalf("probe %s: %v", target, err)
	}
	reachable := result.ExitCode == 0 && !result.TimedOut
	if reachable != wantReachable {
		t.Fatalf(
			"probe %s reachable = %t, want %t (exit=%d timeout=%t stderr=%q)",
			target, reachable, wantReachable, result.ExitCode, result.TimedOut,
			strings.TrimSpace(string(result.Stderr)),
		)
	}
}

func requireLiveValue(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		t.Fatalf("%s is required for live qualification", name)
	}
	return value
}

func requireLiveURL(t *testing.T, name string) *url.URL {
	t.Helper()
	value := requireLiveValue(t, name)
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		t.Fatalf("%s must be an absolute HTTP(S) URL, got %q", name, value)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		t.Fatalf("%s scheme = %q, want http or https", name, parsed.Scheme)
	}
	return parsed
}

func runLiveFileResourceConformance(t *testing.T, factory sandboxtest.Factory) {
	t.Helper()
	sandboxtest.RunFileResources(t, sandboxtest.Config{
		NewProvider: factory,
		Spec:        sandbox.Spec{Timeout: 2 * time.Minute, Network: "bridge"},
		ShellPath:   "/bin/sh",
	})
}

func runLiveSessionOutputConformance(t *testing.T, factory sandboxtest.Factory) {
	t.Helper()
	sandboxtest.RunSessionOutputs(t, sandboxtest.Config{
		NewProvider: factory,
		Spec:        sandbox.Spec{Timeout: 2 * time.Minute, Network: "bridge"},
		ShellPath:   "/bin/sh",
	})
}

func runLiveSkillBundleConformance(t *testing.T, factory sandboxtest.Factory) {
	t.Helper()
	sandboxtest.RunSkillBundles(t, sandboxtest.Config{
		NewProvider: factory,
		Spec:        sandbox.Spec{Timeout: 2 * time.Minute, Network: "bridge"},
		ShellPath:   "/bin/sh",
	})
}

func runLiveGitRepositoryConformance(t *testing.T, factory sandboxtest.Factory) {
	t.Helper()
	sandboxtest.RunGitRepositories(t, sandboxtest.Config{
		NewProvider: factory,
		Spec:        sandbox.Spec{Timeout: 2 * time.Minute, Network: "bridge"},
		ShellPath:   "/bin/sh",
	})
}

func runLiveMemoryStoreConformance(t *testing.T, factory sandboxtest.Factory) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	provider := factory(t)
	readWrite := sandbox.MemoryStoreMount{
		Identity: "sesrsc_memory_rw", StoreID: "memstore_rw",
		RuntimePath: "/mnt/memory/project", Access: domain.MemoryAccessReadWrite,
	}
	readOnly := sandbox.MemoryStoreMount{
		Identity: "sesrsc_memory_ro", StoreID: "memstore_ro",
		RuntimePath: "/mnt/memory/reference", Access: domain.MemoryAccessReadOnly,
	}
	spec := sandbox.Spec{
		Timeout:      2 * time.Minute,
		Network:      "bridge",
		MemoryStores: []sandbox.MemoryStoreMount{readWrite, readOnly},
	}
	session := liveSessionKey("memory")
	ref, box, err := provider.Create(ctx, session, spec)
	if err != nil {
		t.Fatalf("create Memory Store sandbox: %v", err)
	}
	t.Cleanup(func() { _ = box.Destroy(context.Background()) })
	memoryBox, ok := box.(sandbox.MemoryStoreSandbox)
	if !ok {
		t.Fatalf("OpenSandbox sandbox does not expose Memory Stores: %T", box)
	}
	file := func(id, memoryPath, content string) sandbox.MemoryStoreFile {
		sum := sha256.Sum256([]byte(content))
		return sandbox.MemoryStoreFile{
			MemoryID: id, Path: memoryPath, Content: []byte(content),
			ContentSHA256: hex.EncodeToString(sum[:]),
		}
	}
	if err := memoryBox.ReplaceMemoryStore(ctx, readWrite, []sandbox.MemoryStoreFile{
		file("mem_note", "/notes/a.md", "initial"),
	}); err != nil {
		t.Fatalf("materialize read-write Memory Store: %v", err)
	}
	if err := memoryBox.ReplaceMemoryStore(ctx, readOnly, []sandbox.MemoryStoreFile{
		file("mem_policy", "/policy.md", "fixed"),
	}); err != nil {
		t.Fatalf("materialize read-only Memory Store: %v", err)
	}
	if err := box.WriteFile(ctx, readWrite.RuntimePath+"/notes/a.md", []byte("file-tool")); err != nil {
		t.Fatalf("write read-write Memory through file tool: %v", err)
	}
	if err := box.WriteFile(ctx, readOnly.RuntimePath+"/policy.md", []byte("changed")); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("read-only Memory file-tool error = %v", err)
	}
	result, err := box.Exec(ctx, sandbox.Command{
		Path: "sh",
		Args: []string{"-c", `test "$(id -u)" = 1000 && test ! -x /var/lib/mango/memory-control && printf updated > /mnt/memory/project/notes/a.md && printf new > /mnt/memory/project/new.md`},
	})
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("write read-write Memory as unprivileged agent: result=%+v err=%v", result, err)
	}
	result, err = box.Exec(ctx, sandbox.Command{
		Path: "sh", Args: []string{"-c", `printf changed > /mnt/memory/reference/policy.md`},
	})
	if err != nil {
		t.Fatalf("probe read-only Memory: %v", err)
	}
	if result == nil || result.ExitCode == 0 {
		t.Fatalf("read-only Memory accepted an agent write: %+v", result)
	}
	snapshot, err := memoryBox.ReadMemoryStore(ctx, readWrite)
	if err != nil || !snapshot.Initialized || len(snapshot.Baseline) != 1 || len(snapshot.Current) != 2 {
		t.Fatalf("read-write Memory snapshot = %+v, %v", snapshot, err)
	}
	contents := map[string]string{}
	for _, current := range snapshot.Current {
		contents[current.Path] = string(current.Content)
	}
	if contents["/notes/a.md"] != "updated" || contents["/new.md"] != "new" {
		t.Fatalf("read-write Memory contents = %#v", contents)
	}
	readOnlySnapshot, err := memoryBox.ReadMemoryStore(ctx, readOnly)
	if err != nil || len(readOnlySnapshot.Current) != 1 ||
		string(readOnlySnapshot.Current[0].Content) != "fixed" {
		t.Fatalf("read-only Memory snapshot = %+v, %v", readOnlySnapshot, err)
	}
	restarted := factory(t)
	attached, err := restarted.Attach(ctx, session, ref, spec)
	if err != nil {
		t.Fatalf("attach Memory Store sandbox: %v", err)
	}
	attachedSnapshot, err := attached.(sandbox.MemoryStoreSandbox).ReadMemoryStore(ctx, readWrite)
	if err != nil || len(attachedSnapshot.Current) != 2 {
		t.Fatalf("attached Memory snapshot = %+v, %v", attachedSnapshot, err)
	}
}

func runLiveConformance(t *testing.T, factory sandboxtest.Factory) {
	t.Helper()
	sandboxtest.Run(t, sandboxtest.Config{
		NewProvider: factory,
		// Provider lifecycle conformance does not require egress. "bridge"
		// avoids requiring an optional policy sidecar in local OpenSandbox
		// installations; provider-specific tests cover network policy mapping.
		Spec:      sandbox.Spec{Timeout: 2 * time.Minute, Network: "bridge"},
		ShellPath: "/bin/sh",
	})
}

func requireLive(t *testing.T, name string) {
	t.Helper()
	if !liveEnvBool(name) {
		t.Skipf("set %s=1 to run provider live conformance", name)
	}
}

func liveEnvBool(name string) bool {
	value := strings.TrimSpace(os.Getenv(name))
	parsed, err := strconv.ParseBool(value)
	return err == nil && parsed
}
