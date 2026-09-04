//go:build linux

package main

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// protectProcessCredentials prevents same-UID tool subprocesses from using
// ptrace-gated /proc files, process_vm_readv, or ptrace to inspect the trusted
// item runner. The runner calls this before reading its scoped Work secret.
func protectProcessCredentials() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("protect item runner credentials: %w", err)
	}
	return nil
}
