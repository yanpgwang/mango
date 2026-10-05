package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVersionCommandReportsLinkedIdentityWithoutServices(t *testing.T) {
	// sudo's secure_path can select an older system Go than this test binary.
	tools := t.TempDir()
	if err := os.WriteFile(filepath.Join(tools, "go"), []byte("#!/bin/sh\necho wrong-system-toolchain >&2\nexit 73\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	binary := filepath.Join(t.TempDir(), "mango-worker")
	flags := "-X github.com/yanpgwang/mango/internal/buildinfo.Version=0.1.0-alpha.2 -X github.com/yanpgwang/mango/internal/buildinfo.Revision=0123456789abcdef0123456789abcdef01234567"
	build := exec.Command(filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-ldflags", flags, "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, output)
	}
	command := exec.Command(binary, "version")
	command.Env = append(os.Environ(), "DOCKER_HOST=not-a-docker-url", "MANGO_BASE_URL=not-an-http-url")
	output, err := command.Output()
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	var info map[string]string
	if err := json.Unmarshal(output, &info); err != nil {
		t.Fatal(err)
	}
	if len(info) != 2 || info["version"] != "0.1.0-alpha.2" || info["revision"] != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("release identity = %#v", info)
	}
}
