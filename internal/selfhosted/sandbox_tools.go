package selfhosted

import (
	"errors"
	"os"

	mango "github.com/yanpgwang/mango/sdk/go"
	"github.com/yanpgwang/mango/sdk/go/tools/agenttoolset"
)

// SandboxTools builds the SDK's provider-neutral agent toolset only after a
// launcher has marked this process as sandboxed. It prevents the first-party
// Docker command from accidentally becoming a host-process fallback.
func SandboxTools(workdir string) ([]mango.SessionTool, error) {
	return SandboxToolsForSession(mango.EnvironmentWorkerToolContext{Workdir: workdir})
}

// SandboxToolsForSession binds the core toolset after EnvironmentWorker has
// materialized this Session's Memory Stores and supplied their access policy.
func SandboxToolsForSession(session mango.EnvironmentWorkerToolContext) ([]mango.SessionTool, error) {
	if os.Getenv("MANGO_SANDBOXED") != "1" {
		return nil, errors.New("selfhosted: sandbox tools refuse to execute outside a launcher-provided sandbox")
	}
	return agenttoolset.New(agenttoolset.Context{
		Workdir: session.Workdir, AllowedRoots: session.AllowedRoots,
		ReadOnlyRoots: session.ReadOnlyRoots,
	})
}

// CloseSandboxTools releases stateful tools created directly with SandboxTools.
// EnvironmentWorker owns and closes tools returned by SandboxToolsForSession.
func CloseSandboxTools(tools []mango.SessionTool) error {
	return agenttoolset.CloseAll(tools)
}
