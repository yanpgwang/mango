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
	sandboxtest.Run(t, cfg)
	sandboxtest.RunFileResources(t, cfg)
	sandboxtest.RunGitRepositories(t, cfg)
	sandboxtest.RunSessionOutputs(t, cfg)
}
