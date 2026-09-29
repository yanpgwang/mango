package selfhosted

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	mango "github.com/yanpgwang/mango/sdk/go"
)

// SandboxHealthcheck proves subprocess execution and workspace read/write.
// Its program is fixed, receives no caller input, emits no process output, and
// inherits no credentials. The Docker launcher supplies an ephemeral workspace.
func SandboxHealthcheck(ctx context.Context, workdir string) (retErr error) {
	if os.Getenv("MANGO_SANDBOXED") != "1" {
		return errors.New("healthcheck requires a launcher-provided sandbox")
	}
	ctx, cancel := context.WithTimeout(ctx, mango.HealthcheckExecutionTimeout)
	defer cancel()
	dir, err := os.MkdirTemp(workdir, ".mango-healthcheck-")
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, os.RemoveAll(dir)) }()
	path := filepath.Join(dir, "probe")
	command := exec.CommandContext(ctx, "/bin/bash", "--noprofile", "--norc", "-c", `printf 'mango-healthcheck\n' > "$1"`, "mango-healthcheck", path)
	command.Dir = dir
	command.Env = []string{"PATH=/usr/local/bin:/usr/bin:/bin", "HOME=/tmp"}
	command.WaitDelay = time.Second
	if err := command.Run(); err != nil {
		return err
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if string(content) != "mango-healthcheck\n" {
		return errors.New("healthcheck workspace readback did not match")
	}
	return nil
}
