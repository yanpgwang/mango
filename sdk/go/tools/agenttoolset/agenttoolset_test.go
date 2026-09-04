package agenttoolset

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	t.Cleanup(func() {
		if err := CloseAll(tools); err != nil {
			t.Errorf("close toolset: %v", err)
		}
	})
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
	t.Cleanup(func() { _ = CloseAll(tools) })
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
	t.Cleanup(func() { _ = CloseAll(tools) })
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
	if got := blocks[0].TextBlockInput.Text; !strings.HasSuffix(got, "456") || !strings.Contains(got, "truncated") {
		t.Fatalf("bounded bash output = %q", got)
	}
}

func TestBashCancellationTerminatesTheCommandGroup(t *testing.T) {
	tools, err := New(Context{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = CloseAll(tools) })
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

func TestBashPersistsStateAndRestarts(t *testing.T) {
	root := t.TempDir()
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := New(Context{Workdir: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = CloseAll(tools) })
	bash := findTool(t, tools, "bash")
	execute := func(input string) string {
		t.Helper()
		blocks, err := bash.Execute(context.Background(), mango.SessionToolCall{
			Name: "bash", Input: json.RawMessage(input),
		})
		if err != nil {
			t.Fatal(err)
		}
		return blocks[0].TextBlockInput.Text
	}

	if got := execute(`{"command":"mkdir child; cd child; export MANGO_BASH_STATE=kept; printf ready"}`); got != "ready" {
		t.Fatalf("first command = %q", got)
	}
	wantDir := filepath.Join(root, "child")
	if got := execute(`{"command":"printf '%s|%s' \"$MANGO_BASH_STATE\" \"$PWD\""}`); got != "kept|"+wantDir {
		t.Fatalf("persistent state = %q", got)
	}
	execute(`{"command":"(sleep 0.05; printf background > bg.txt) &"}`)
	if got := execute(`{"command":"for _ in {1..100}; do [[ -f bg.txt ]] && break; sleep 0.01; done; cat bg.txt"}`); !strings.Contains(got, "background") {
		t.Fatalf("background job result = %q", got)
	}
	if got := execute(`{"restart":true}`); got != "bash session restarted" {
		t.Fatalf("restart result = %q", got)
	}
	if got := execute(`{"command":"printf '[%s]|%s' \"$MANGO_BASH_STATE\" \"$PWD\""}`); got != "[]|"+root {
		t.Fatalf("fresh state = %q", got)
	}
	if got := execute(`{"restart":true,"command":"printf '[%s]|%s' \"$MANGO_BASH_STATE\" \"$PWD\""}`); got != "[]|"+root {
		t.Fatalf("restart with command = %q", got)
	}
}

func TestBashNonZeroExitIncludesOutputAndCode(t *testing.T) {
	tools, err := New(Context{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = CloseAll(tools) })
	_, err = findTool(t, tools, "bash").Execute(context.Background(), mango.SessionToolCall{
		Name: "bash", Input: json.RawMessage(`{"command":"printf failure; (exit 7)"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "failure") || !strings.Contains(err.Error(), "exit code: 7") {
		t.Fatalf("non-zero error = %v", err)
	}
}

func TestBashTimeoutAndCancellationRestartTheShell(t *testing.T) {
	tools, err := New(Context{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = CloseAll(tools) })
	bash := findTool(t, tools, "bash")
	_, err = bash.Execute(context.Background(), mango.SessionToolCall{
		Name: "bash", Input: json.RawMessage(`{"command":"export LEAK=timeout; printf before; sleep 30","timeout_ms":100}`),
	})
	if err == nil || !strings.Contains(err.Error(), "before") || !strings.Contains(err.Error(), "[timed out]") || !strings.Contains(err.Error(), "session restarted after timeout") {
		t.Fatalf("timeout error = %v", err)
	}
	assertFreshBash(t, bash, "timeout")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err = bash.Execute(ctx, mango.SessionToolCall{
		Name: "bash", Input: json.RawMessage(`{"command":"export LEAK=cancel; sleep 30","timeout_ms":30000}`),
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancellation error = %v", err)
	}
	assertFreshBash(t, bash, "cancel")
}

func TestBashRedirectsCommandStdinAndCannotSpoofFraming(t *testing.T) {
	tools, err := New(Context{Workdir: t.TempDir(), BashTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = CloseAll(tools) })
	blocks, err := findTool(t, tools, "bash").Execute(context.Background(), mango.SessionToolCall{
		Name: "bash", Input: json.RawMessage(`{"command":"cat; printf '__MANGO_BASH_DONE__7|after'"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := blocks[0].TextBlockInput.Text; got != "__MANGO_BASH_DONE__7|after" {
		t.Fatalf("bash output = %q", got)
	}
}

func TestBashValidatesInputAndCloseIsIdempotent(t *testing.T) {
	for _, contextOptions := range []Context{
		{Workdir: t.TempDir(), BashTimeout: -time.Second},
		{Workdir: t.TempDir(), BashCloseTimeout: -time.Second},
	} {
		if _, err := New(contextOptions); err == nil {
			t.Fatal("New accepted a negative bash timeout")
		}
	}

	tools, err := New(Context{Workdir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	bash := findTool(t, tools, "bash")
	for _, input := range []string{`{}`, `{"command":"echo no","timeout_ms":-1}`, `{"command":"echo no","timeout_ms":1.5}`} {
		if _, err := bash.Execute(context.Background(), mango.SessionToolCall{Name: "bash", Input: json.RawMessage(input)}); err == nil {
			t.Fatalf("bash accepted invalid input %s", input)
		}
	}
	if _, err := bash.Execute(context.Background(), mango.SessionToolCall{Name: "bash", Input: json.RawMessage(`{"command":"printf started"}`)}); err != nil {
		t.Fatal(err)
	}
	closer, ok := bash.(io.Closer)
	if !ok {
		t.Fatal("bash tool is not closable")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := bash.Execute(context.Background(), mango.SessionToolCall{Name: "bash", Input: json.RawMessage(`{"command":"echo no"}`)}); !errors.Is(err, errBashClosed) {
		t.Fatalf("execute after close = %v", err)
	}
}

func TestBashCloseInterruptsInFlightCommand(t *testing.T) {
	root := t.TempDir()
	tools, err := New(Context{Workdir: root, BashCloseTimeout: 500 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	bash := findTool(t, tools, "bash")
	done := make(chan error, 1)
	go func() {
		_, err := bash.Execute(context.Background(), mango.SessionToolCall{
			Name: "bash", Input: json.RawMessage(`{"command":"printf started > close-started; sleep 30"}`),
		})
		done <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "close-started")); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if time.Now().After(deadline) {
			t.Fatal("bash command did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	startedAt := time.Now()
	if err := CloseAll(tools); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(startedAt); elapsed > 2*time.Second {
		t.Fatalf("CloseAll took %s", elapsed)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("in-flight command succeeded after close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight command did not stop")
	}
}

func TestCloseAllContinuesAfterErrorsAndPanics(t *testing.T) {
	first := &closeProbe{name: "first"}
	panicker := &closeProbe{name: "panicker", close: func() error { panic("close failed") }}
	errored := &closeProbe{name: "errored", close: func() error { return errors.New("reap failed") }}
	last := &closeProbe{name: "last"}
	err := CloseAll([]mango.SessionTool{first, panicker, errored, last})
	if err == nil || !strings.Contains(err.Error(), "close failed") || !strings.Contains(err.Error(), "reap failed") {
		t.Fatalf("CloseAll error = %v", err)
	}
	for _, tool := range []*closeProbe{first, panicker, errored, last} {
		if !tool.closed {
			t.Fatalf("tool %q was not closed", tool.name)
		}
	}
}

func assertFreshBash(t *testing.T, bash mango.SessionTool, previous string) {
	t.Helper()
	blocks, err := bash.Execute(context.Background(), mango.SessionToolCall{
		Name: "bash", Input: json.RawMessage(`{"command":"printf '[%s]|after' \"$LEAK\""}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	got := blocks[0].TextBlockInput.Text
	if got != "[]|after" || strings.Contains(got, previous) || strings.Contains(got, "__MANGO_BASH_") {
		t.Fatalf("fresh shell output = %q", got)
	}
}

type closeProbe struct {
	name   string
	closed bool
	close  func() error
}

func (p *closeProbe) Name() string { return p.name }

func (p *closeProbe) Execute(context.Context, mango.SessionToolCall) ([]mango.ResultContentInput, error) {
	return nil, nil
}

func (p *closeProbe) Close() error {
	p.closed = true
	if p.close != nil {
		return p.close()
	}
	return nil
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
