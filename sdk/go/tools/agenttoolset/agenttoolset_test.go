package agenttoolset

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mango "github.com/yanpgwang/mango/sdk/go"
)

func TestCoreToolsetAndCredentialScrubbing(t *testing.T) {
	root := t.TempDir()
	tools, err := New(Context{
		Workdir: root,
		Environment: []string{
			"MANGO_API_KEY=workspace-secret", "MANGO_WORK_SECRET=work-secret",
			"MANGO_SESSION_ID=session-secret", "MANGO_FUTURE_CREDENTIAL=future-secret", "SAFE_VALUE=visible",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	execute := func(name, input string) string {
		t.Helper()
		blocks, err := findTool(t, tools, name).Execute(context.Background(), mango.SessionToolCall{
			Name: name, Input: json.RawMessage(input),
		})
		if err != nil {
			t.Fatal(err)
		}
		return blocks[0].TextBlockInput.Text
	}
	if got := execute("bash", `{"command":"printf '%s|%s|%s|%s|%s' \"${MANGO_API_KEY:-}\" \"${MANGO_WORK_SECRET:-}\" \"${MANGO_SESSION_ID:-}\" \"${MANGO_FUTURE_CREDENTIAL:-}\" \"${SAFE_VALUE:-}\""}`); got != "||||visible" {
		t.Fatalf("bash environment = %q", got)
	}
	if got := execute("bash", `{"command":"set -o pipefail; values=(alpha beta); [[ ${values[1]} == beta ]] && printf bash"}`); got != "bash" {
		t.Fatalf("Bash-only syntax result = %q", got)
	}
	execute("write", `{"path":"src/note.txt","file_text":"alpha\nbeta\n"}`)
	if got := execute("read", `{"path":"src/note.txt","view_range":[2,2]}`); got != "beta" {
		t.Fatalf("read = %q", got)
	}
	execute("edit", `{"path":"src/note.txt","old_str":"beta","new_str":"gamma"}`)
	if got := execute("glob", `{"pattern":"**/*.txt"}`); got != "src/note.txt" {
		t.Fatalf("glob = %q", got)
	}
	if got := execute("grep", `{"pattern":"gamma","path":"src"}`); !strings.Contains(got, "src/note.txt:2:gamma") {
		t.Fatalf("grep = %q", got)
	}
}

func TestToolsetRejectsTraversalAndEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	tools, err := New(Context{Workdir: root})
	if err != nil {
		t.Fatal(err)
	}
	write := findTool(t, tools, "write")
	if _, err := write.Execute(context.Background(), mango.SessionToolCall{
		Name: "write", Input: json.RawMessage(`{"path":"../escape","file_text":"bad"}`),
	}); err == nil {
		t.Fatal("write accepted parent traversal")
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := write.Execute(context.Background(), mango.SessionToolCall{
		Name: "write", Input: json.RawMessage(`{"path":"link/escape","file_text":"bad"}`),
	}); err == nil {
		t.Fatal("write accepted an escaping symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "escape")); !os.IsNotExist(err) {
		t.Fatalf("outside file exists or stat failed unexpectedly: %v", err)
	}
}

func TestToolsetEnforcesByteLimits(t *testing.T) {
	root := t.TempDir()
	tools, err := New(Context{Workdir: root, MaxReadBytes: 4, MaxEditBytes: 5, MaxOutputBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	write := findTool(t, tools, "write")
	if _, err := write.Execute(context.Background(), mango.SessionToolCall{
		Name: "write", Input: json.RawMessage(`{"path":"x","file_text":"123456"}`),
	}); err == nil {
		t.Fatal("write accepted content over limit")
	}
	if err := os.WriteFile(filepath.Join(root, "x"), []byte("12345"), 0o644); err != nil {
		t.Fatal(err)
	}
	read := findTool(t, tools, "read")
	if _, err := read.Execute(context.Background(), mango.SessionToolCall{
		Name: "read", Input: json.RawMessage(`{"path":"x"}`),
	}); err == nil {
		t.Fatal("read accepted file over limit")
	}
	bash := findTool(t, tools, "bash")
	blocks, err := bash.Execute(context.Background(), mango.SessionToolCall{
		Name: "bash", Input: json.RawMessage(`{"command":"printf 123456"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := blocks[0].TextBlockInput.Text; !strings.HasPrefix(got, "123") || !strings.Contains(got, "truncated") {
		t.Fatalf("bounded bash output = %q", got)
	}
}

func TestBashCancellationTerminatesTheCommandGroup(t *testing.T) {
	tools, err := New(Context{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = findTool(t, tools, "bash").Execute(ctx, mango.SessionToolCall{
		Name: "bash", Input: json.RawMessage(`{"command":"sleep 30 & wait"}`),
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bash cancellation error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("bash cancellation took %s", elapsed)
	}
}

func findTool(t *testing.T, tools []mango.SessionTool, name string) mango.SessionTool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name() == name {
			return tool
		}
	}
	t.Fatalf("tool %q not found", name)
	return nil
}
