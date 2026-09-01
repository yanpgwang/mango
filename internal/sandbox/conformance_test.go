package sandbox_test

import (
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/sandbox"
	"github.com/yanpgwang/mango/internal/sandbox/sandboxtest"
)

func TestOpenSandboxProviderConformance(t *testing.T) {
	cfg := sandboxtest.Config{
		NewProvider: func(t *testing.T) sandbox.Provider {
			return sandboxtest.OpenSandboxProvider(t)
		},
		Spec: sandbox.Spec{Timeout: 30 * time.Second},
	}
	t.Run("lifecycle", func(t *testing.T) { sandboxtest.Run(t, cfg) })
	t.Run("file-resources", func(t *testing.T) { sandboxtest.RunFileResources(t, cfg) })
	t.Run("git-repositories", func(t *testing.T) { sandboxtest.RunGitRepositories(t, cfg) })
	t.Run("session-outputs", func(t *testing.T) { sandboxtest.RunSessionOutputs(t, cfg) })
}
