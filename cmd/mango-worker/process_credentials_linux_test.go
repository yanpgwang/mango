//go:build linux

package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

const (
	credentialTargetHelper = "MANGO_TEST_CREDENTIAL_TARGET"
	credentialReaderHelper = "MANGO_TEST_CREDENTIAL_READER"
)

func TestProtectProcessCredentialsDeniesSameUIDProcRead(t *testing.T) {
	if target := os.Getenv(credentialReaderHelper); target != "" {
		_, err := os.ReadFile("/proc/" + target + "/environ")
		if !errors.Is(err, fs.ErrPermission) {
			t.Fatalf("read protected parent environment: %v", err)
		}
		return
	}
	if os.Getenv(credentialTargetHelper) == "1" {
		if err := protectProcessCredentials(); err != nil {
			t.Fatal(err)
		}
		reader := exec.Command(os.Args[0], "-test.run=^TestProtectProcessCredentialsDeniesSameUIDProcRead$")
		reader.Env = []string{credentialReaderHelper + "=" + strconv.Itoa(os.Getpid())}
		if output, err := reader.CombinedOutput(); err != nil {
			t.Fatalf("credential reader: %v\n%s", err, output)
		}
		return
	}

	target := exec.Command(os.Args[0], "-test.run=^TestProtectProcessCredentialsDeniesSameUIDProcRead$")
	target.Env = append(os.Environ(),
		credentialTargetHelper+"=1",
		"MANGO_WORK_SECRET=review-secret",
	)
	if output, err := target.CombinedOutput(); err != nil {
		t.Fatalf("protected target: %v\n%s", err, output)
	}
}
