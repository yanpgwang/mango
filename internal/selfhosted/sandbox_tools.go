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
	if os.Getenv("MANGO_SANDBOXED") != "1" {
		return nil, errors.New("selfhosted: sandbox tools refuse to execute outside a launcher-provided sandbox")
	}
	return agenttoolset.New(agenttoolset.Context{Workdir: workdir})
}

// CloseSandboxTools releases stateful tool processes. The Environment worker
// intentionally borrows its tool slice, so the sandbox entrypoint that creates
// the tools also owns this cleanup.
func CloseSandboxTools(tools []mango.SessionTool) error {
	return agenttoolset.CloseAll(tools)
}
