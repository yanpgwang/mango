package selfhosted

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mango "github.com/yanpgwang/mango/sdk/go"
)

func TestSandboxToolsRequireLauncherMarkerAndScrubCredentials(t *testing.T) {
	t.Setenv("MANGO_SANDBOXED", "")
	if _, err := SandboxTools(t.TempDir()); err == nil {
		t.Fatal("SandboxTools accepted a host process without MANGO_SANDBOXED")
	}

	t.Setenv("MANGO_SANDBOXED", "1")
	t.Setenv("MANGO_API_KEY", "workspace-secret")
	t.Setenv("MANGO_WORK_SECRET", "work-secret")
	t.Setenv("MANGO_SESSION_ID", "session-secret")
	t.Setenv("SAFE_VALUE", "visible")
	toolset, err := SandboxTools(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bash := findTool(t, toolset, "bash")
	blocks, err := bash.Execute(context.Background(), mango.SessionToolCall{
		Name:  "bash",
		Input: json.RawMessage(`{"command":"printf '%s|%s|%s|%s' \"${MANGO_API_KEY:-}\" \"${MANGO_WORK_SECRET:-}\" \"${MANGO_SESSION_ID:-}\" \"${SAFE_VALUE:-}\""}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := blocks[0].TextBlockInput.Text; got != "|||visible" {
		t.Fatalf("bash environment = %q, want only SAFE_VALUE", got)
	}
}

func TestSandboxToolsReadWriteEditGlobAndGrep(t *testing.T) {
	t.Setenv("MANGO_SANDBOXED", "1")
	root := t.TempDir()
	toolset, err := SandboxTools(root)
	if err != nil {
		t.Fatal(err)
	}
	execute := func(name, input string) (string, error) {
		t.Helper()
		blocks, err := findTool(t, toolset, name).Execute(context.Background(), mango.SessionToolCall{
			Name: name, Input: json.RawMessage(input),
		})
		if err != nil {
			return "", err
		}
		return blocks[0].TextBlockInput.Text, nil
	}
	if _, err := execute("write", `{"path":"src/note.txt","file_text":"alpha\nbeta\n"}`); err != nil {
		t.Fatal(err)
	}
	if got, err := execute("read", `{"path":"src/note.txt","view_range":[2,2]}`); err != nil || got != "beta" {
		t.Fatalf("read = %q, %v", got, err)
	}
	if _, err := execute("edit", `{"path":"src/note.txt","old_str":"beta","new_str":"gamma"}`); err != nil {
		t.Fatal(err)
	}
	if got, err := execute("glob", `{"pattern":"**/*.txt"}`); err != nil || got != "src/note.txt" {
		t.Fatalf("glob = %q, %v", got, err)
	}
	if got, err := execute("grep", `{"pattern":"gamma","path":"src"}`); err != nil || !strings.Contains(got, "src/note.txt:2:gamma") {
		t.Fatalf("grep = %q, %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(root, "src", "note.txt")); err != nil || string(got) != "alpha\ngamma\n" {
		t.Fatalf("workspace file = %q, %v", got, err)
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
