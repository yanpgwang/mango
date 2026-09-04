//go:build windows

package agenttoolset

import "os"

func killProcessGroup(process *os.Process) error {
	if process == nil {
		return nil
	}
	return process.Kill()
}
