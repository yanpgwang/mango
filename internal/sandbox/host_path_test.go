package sandbox

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCanonicalHostPathResolvesMissingDescendantsWithoutCreatingThem(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	got, err := canonicalHostPath(filepath.Join(alias, "missing", "files"))
	if err != nil || got != filepath.Join(real, "missing", "files") {
		t.Fatalf("canonical path = %q, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(real, "missing")); !os.IsNotExist(err) {
		t.Fatalf("canonicalization created a path: %v", err)
	}
}
