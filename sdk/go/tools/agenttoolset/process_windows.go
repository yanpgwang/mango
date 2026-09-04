//go:build windows

package agenttoolset

import (
	"os/exec"
	"time"
)

func configureCommandCancellation(command *exec.Cmd) {
	command.WaitDelay = 2 * time.Second
}
