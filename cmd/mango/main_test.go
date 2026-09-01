package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/sandbox"
)

func TestRetryingSessionResourceReconcilerRecoversAndCaches(t *testing.T) {
	attempts := 0
	reconciler := &retryingSessionResourceReconciler{
		resolve: func(context.Context) (*resolvedFiles, error) {
			attempts++
			if attempts == 1 {
				return nil, errors.New("temporary object-store outage")
			}
			return &resolvedFiles{}, nil
		},
	}
	if _, err := reconciler.resolveMaterializer(context.Background()); err == nil {
		t.Fatal("first resolve unexpectedly succeeded")
	}
	first, err := reconciler.resolveMaterializer(context.Background())
	if err != nil || first == nil {
		t.Fatalf("second resolve = %v, %v", first, err)
	}
	second, err := reconciler.resolveMaterializer(context.Background())
	if err != nil || second != first {
		t.Fatalf("cached resolve = %v, %v; want %v", second, err, first)
	}
	if attempts != 2 {
		t.Fatalf("resolver attempts = %d, want 2", attempts)
	}
}

func TestResolveSandboxProvider_RequiresOpenSandbox(t *testing.T) {
	t.Setenv(openSandboxDomainEnv, "")
	provider, err := resolveSandboxProvider()
	if provider != nil || err == nil || !strings.Contains(err.Error(), openSandboxDomainEnv) {
		t.Fatalf("provider = %v, %v; want missing %s", provider, err, openSandboxDomainEnv)
	}
}

func TestOpenSandboxCapabilities(t *testing.T) {
	want := sandboxCapabilities{
		PackageSetup: true, LimitedNetwork: true, FileResources: true,
		SessionOutputs: true, SkillBundles: true, MemoryStores: true,
		GitRepositories: true,
	}
	if got := openSandboxCapabilities(); got != want {
		t.Fatalf("OpenSandbox capabilities = %+v, want %+v", got, want)
	}
}

func TestResolveSandboxProvider_RejectsInvalidOpenSandboxConfig(t *testing.T) {
	t.Setenv(openSandboxDomainEnv, "http://127.0.0.1:8090")
	t.Setenv(openSandboxUseProxyEnv, "sometimes")
	_, err := resolveSandboxProvider()
	if err == nil || !strings.Contains(err.Error(), openSandboxUseProxyEnv) {
		t.Fatalf("resolveSandboxProvider error = %v, want invalid %s", err, openSandboxUseProxyEnv)
	}
}

func TestSandboxEnvironmentParsersRejectInvalidValues(t *testing.T) {
	t.Setenv(openSandboxUseProxyEnv, "sometimes")
	if _, err := envBool(openSandboxUseProxyEnv); err == nil {
		t.Fatalf("envBool accepted invalid %s", openSandboxUseProxyEnv)
	}
}

func TestDefaultOpenSandboxRuntime_PythonReattachAndCleanup(t *testing.T) {
	if os.Getenv("MANGO_TEST_OPENSANDBOX") != "1" {
		t.Skip("set MANGO_TEST_OPENSANDBOX=1 to require real OpenSandbox execution")
	}
	t.Setenv("MANGO_MODEL_API_KEY", "worker-only-test-marker")
	provider, err := resolveSandboxProvider()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	key := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano())
	spec := sandbox.Spec{Timeout: 10 * time.Second}
	ref, box, err := provider.Create(ctx, key, spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		if err := box.Destroy(cleanupCtx); err != nil {
			t.Errorf("cleanup OpenSandbox sandbox: %v", err)
		}
	})
	if ref.Provider != sandbox.OpenSandboxProviderName || box.Root() != "/workspace" {
		t.Fatalf("default sandbox = %+v, root %q", ref, box.Root())
	}
	result, err := box.Exec(ctx, sandbox.Command{Path: "python3", Args: []string{"-c",
		"import os; from pathlib import Path; assert 'MANGO_MODEL_API_KEY' not in os.environ; assert not Path('/var/run/docker.sock').exists(); Path('marker.txt').write_text('survives'); Path('/mnt/session/outputs/result.txt').write_text('artifact')",
	}})
	if err != nil || result.ExitCode != 0 || result.TimedOut {
		t.Fatalf("default Python execution = %+v, %v", result, err)
	}
	restarted, err := resolveSandboxProvider()
	if err != nil {
		t.Fatal(err)
	}
	attached, err := restarted.Attach(ctx, key, ref, spec)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"marker.txt": "survives", "/mnt/session/outputs/result.txt": "artifact"} {
		got, err := attached.ReadFile(ctx, path)
		if err != nil || string(got) != want {
			t.Fatalf("read %s after restart = %q, %v", path, got, err)
		}
	}
	if err := attached.Destroy(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Attach(ctx, key, ref, spec); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("attach after deletion = %v, want ErrNotFound", err)
	}
}

// TestDefaultAddr_BindsLoopback asserts the serve default listen address binds
// to loopback so a fresh serve never exposes the unauthenticated API on all
// interfaces.
func TestDefaultAddr_BindsLoopback(t *testing.T) {
	if defaultAddr != "127.0.0.1:8080" {
		t.Fatalf("defaultAddr = %q; want 127.0.0.1:8080", defaultAddr)
	}
}

// TestNewHTTPServer_Timeouts asserts the serving server sets slow-header and
// idle bounds and a header-size cap, and deliberately leaves WriteTimeout unset
// so long-lived SSE streams are not aborted mid-response.
func TestNewHTTPServer_Timeouts(t *testing.T) {
	srv := newHTTPServer("127.0.0.1:0", http.NewServeMux())
	if srv.ReadHeaderTimeout != 10*time.Second {
		t.Errorf("ReadHeaderTimeout = %v; want 10s", srv.ReadHeaderTimeout)
	}
	if srv.IdleTimeout != 120*time.Second {
		t.Errorf("IdleTimeout = %v; want 120s", srv.IdleTimeout)
	}
	if srv.MaxHeaderBytes != 1<<20 {
		t.Errorf("MaxHeaderBytes = %d; want %d", srv.MaxHeaderBytes, 1<<20)
	}
	if srv.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v; want 0 (unset, so SSE streams are not aborted)", srv.WriteTimeout)
	}
}

// TestResolveModelClient_ReportsRealModelWithEnv proves model selection reports
// realModel=true when both the model base URL and API key are configured. It
// performs no network call: construction does not contact the endpoint.
func TestResolveModelClient_ReportsRealModelWithEnv(t *testing.T) {
	t.Setenv("MANGO_MODEL_BASE_URL", "https://model.invalid")
	t.Setenv("MANGO_MODEL_API_KEY", "sk-test")
	client, realModel, err := resolveModelClient()
	if err != nil {
		t.Fatal(err)
	}
	if !realModel {
		t.Fatalf("resolveModelClient realModel=false with model env configured; want true")
	}
	if client == nil {
		t.Fatal("resolveModelClient returned a nil client")
	}
}

func TestResolveModelClient_UsesFakeWithoutEnv(t *testing.T) {
	t.Setenv("MANGO_MODEL_BASE_URL", "")
	t.Setenv("MANGO_MODEL_API_KEY", "")
	client, realModel, err := resolveModelClient()
	if err != nil {
		t.Fatal(err)
	}
	if realModel {
		t.Fatal("resolveModelClient realModel=true without model configuration")
	}
	if client == nil {
		t.Fatal("resolveModelClient returned a nil fake client")
	}
}
