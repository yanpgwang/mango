package sandbox_test

import (
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/sandbox"
	"github.com/yanpgwang/mango/internal/sandbox/sandboxtest"
)

func TestDockerProviderConformance(t *testing.T) {
	resourceDir := t.TempDir()
	cfg := sandboxtest.Config{
		NewProvider: func(t *testing.T) sandbox.Provider {
			return sandboxtest.DockerProvider(t, sandbox.DockerConfig{ResourceBaseDir: resourceDir})
		},
		Spec: sandbox.Spec{Timeout: 30 * time.Second},
	}
	sandboxtest.Run(t, cfg)
	sandboxtest.RunFileResources(t, cfg)
	sandboxtest.RunGitRepositories(t, cfg)
	sandboxtest.RunSessionOutputs(t, cfg)
}
