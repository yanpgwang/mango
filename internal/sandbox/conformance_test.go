package sandbox_test

import (
	"os"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/sandbox"
	"github.com/yanpgwang/mango/internal/sandbox/sandboxtest"
)

func TestLocalProviderConformance(t *testing.T) {
	sandboxtest.Run(t, sandboxtest.Config{
		NewProvider: func(*testing.T) sandbox.Provider {
			return sandbox.NewLocalProvider()
		},
		Spec: sandbox.Spec{Timeout: 30 * time.Second},
	})
}

func TestDockerProviderConformance(t *testing.T) {
	cfg := sandboxtest.Config{
		NewProvider: func(t *testing.T) sandbox.Provider {
			t.Helper()
			provider, err := sandbox.NewDockerProvider(sandbox.DockerConfig{
				DefaultImage: "alpine:latest",
			})
			if err != nil {
				if os.Getenv("MANGO_TEST_DOCKER") == "1" {
					t.Fatalf("required Docker Engine not reachable: %v", err)
				}
				t.Skipf("Docker Engine not reachable: %v", err)
			}
			return provider
		},
		Spec: sandbox.Spec{Timeout: 30 * time.Second},
	}
	sandboxtest.Run(t, cfg)
	sandboxtest.RunFileResources(t, cfg)
	sandboxtest.RunGitRepositories(t, cfg)
	sandboxtest.RunSessionOutputs(t, cfg)
}
