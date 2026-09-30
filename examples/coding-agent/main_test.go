package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	mango "github.com/yanpgwang/mango/sdk/go"
)

func TestTurnRequiresWorkerExecution(t *testing.T) {
	end := mango.SessionEvent{SessionStatusIdleEvent: &mango.SessionStatusIdleEvent{
		StopReason: mango.SessionStopReason{SessionEndTurn: &mango.SessionEndTurn{Type: "end_turn"}},
	}}
	p := newTurnEvidence()
	if done, err := p.observe(end); done || err == nil {
		t.Fatal("accepted an end_turn without execution")
	}
	call := mango.SessionEvent{AgentToolUseEvent: &mango.AgentToolUseEvent{
		ID: "bash-current", Name: "bash", EvaluatedPermission: mango.Some(mango.EvaluatedPermission("allow")),
	}}
	if _, err := p.observe(call); err != nil {
		t.Fatal(err)
	}
	waiting := mango.SessionEvent{SessionStatusIdleEvent: &mango.SessionStatusIdleEvent{
		StopReason: mango.SessionStopReason{SessionRequiresAction: &mango.SessionRequiresAction{Type: "requires_action", EventIDs: []string{"bash-current"}}},
	}}
	if done, err := p.observe(waiting); done || err != nil {
		t.Fatalf("allowed worker barrier: done=%v err=%v", done, err)
	}
	for _, result := range []mango.PersistedUserToolResultEvent{
		{ToolUseID: "bash-from-previous-turn"},
		{ToolUseID: "bash-current", IsError: mango.Some(true)},
	} {
		if _, err := p.observe(mango.SessionEvent{PersistedUserToolResultEvent: &result}); err != nil {
			t.Fatal(err)
		}
		if done, err := p.observe(end); done || err == nil {
			t.Fatal("accepted an unrelated or failed result")
		}
	}
	result := mango.SessionEvent{PersistedUserToolResultEvent: &mango.PersistedUserToolResultEvent{ToolUseID: "bash-current"}}
	if _, err := p.observe(result); err != nil {
		t.Fatal(err)
	}
	if done, err := p.observe(end); !done || err != nil {
		t.Fatalf("completed worker turn: done=%v err=%v", done, err)
	}
	waiting.SessionStatusIdleEvent.StopReason.SessionRequiresAction.EventIDs = []string{"human-action"}
	if _, err := p.observe(waiting); err == nil {
		t.Fatal("accepted an unknown action barrier")
	}
}

func TestVerificationRequiresAllPristineTests(t *testing.T) {
	// A module can exit during import with status zero before unittest runs.
	for _, output := range []string{"", "\nRan 0 tests in 0.001s\n\nOK\n", "\nRan 4 tests in 0.001s\n\nOK (skipped=1)\n", "\nRan 4 tests in 0.001s\n\nFAILED (failures=1)\n"} {
		if err := verifySuccess([]byte(output), nil); err == nil {
			t.Fatalf("accepted incomplete verification %q", output)
		}
	}
	success := []byte("\nRan 4 tests in 0.001s\n\nOK\n")
	if err := verifySuccess(success, nil); err != nil {
		t.Fatal(err)
	}
	if err := verifySuccess(success, errors.New("docker failed")); err == nil {
		t.Fatal("ignored container failure")
	}
}

func TestArtifactReaderRejectsNonSourceFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "invoice.py"), []byte("# source\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("invoice.py", filepath.Join(dir, "link.py")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "large.py"), make([]byte, 1<<20+1), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if data, err := readSource(root, "invoice.py"); err != nil || string(data) != "# source\n" {
		t.Fatalf("source = %q, %v", data, err)
	}
	for _, path := range []string{"link.py", "large.py", ".", "../outside.py"} {
		if _, err := readSource(root, path); err == nil {
			t.Errorf("accepted %s", path)
		}
	}
}
