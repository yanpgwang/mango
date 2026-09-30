package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestStateUpdatesReplaceCompletePrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "gate.json")
	initial := gateState{BaseURL: "http://localhost:8080", Decisions: map[string]decision{}}
	if err := writeState(path, initial, true); err != nil {
		t.Fatal(err)
	}
	old, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = old.Close() }()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	next := initial
	next.SessionID = "session_1"
	if err := writeState(path, next, false); err != nil {
		t.Fatal(err)
	}
	retained, err := io.ReadAll(old)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, retained) {
		t.Fatal("update mutated the old file instead of atomically replacing it")
	}
	state, err := readState(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.SessionID != "session_1" {
		t.Fatal("new state was not published")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions: %o", info.Mode().Perm())
	}
	if err := writeState(path, initial, true); !os.IsExist(err) {
		t.Fatalf("initial creation replaced an existing journal: %v", err)
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("temporary state files remain: %v", files)
	}
}

func TestStateRejectsSymlinkAndPublicPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	state := gateState{BaseURL: "http://localhost:8080", Decisions: map[string]decision{}}
	if err := writeState(path, state, true); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readState(link); err == nil {
		t.Fatal("read symlink state")
	}
	if err := writeState(link, state, false); err == nil {
		t.Fatal("replaced symlink state")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readState(path); err == nil {
		t.Fatal("read nonprivate state")
	}
	if err := writeState(path, state, false); err == nil {
		t.Fatal("updated nonprivate state")
	}
}
