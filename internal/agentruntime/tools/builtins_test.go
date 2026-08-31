package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/yanpgwang/mango/internal/sandbox"
	"github.com/yanpgwang/mango/internal/sandbox/sandboxtest"
)

func newSB(t *testing.T) sandbox.Sandbox {
	t.Helper()
	return sandboxtest.Docker(t)
}

func TestBuiltins_WriteReadEditBash(t *testing.T) {
	sb := newSB(t)
	reg := Registry()
	if r := reg["write"](context.Background(), sb, map[string]any{"path": "x.txt", "file_text": "hello world"}); r.IsError {
		t.Fatalf("write: %+v", r)
	}
	if r := reg["read"](context.Background(), sb, map[string]any{"path": "x.txt"}); r.IsError || !contains(r, "hello world") {
		t.Fatalf("read: %+v", r)
	}
	if r := reg["edit"](context.Background(), sb, map[string]any{"path": "x.txt", "old_str": "world", "new_str": "gophers"}); r.IsError {
		t.Fatalf("edit: %+v", r)
	}
	if r := reg["bash"](context.Background(), sb, map[string]any{"command": "cat x.txt"}); r.IsError || !contains(r, "hello gophers") {
		t.Fatalf("bash: %+v", r)
	}
}

func TestBuiltins_ReadViewRange(t *testing.T) {
	sb := newSB(t)
	reg := Registry()
	if r := reg["write"](context.Background(), sb, map[string]any{
		"path": "lines.txt", "file_text": "line1\nline2\nline3",
	}); r.IsError {
		t.Fatalf("write: %+v", r)
	}
	tests := []struct {
		name    string
		rangeIn any
		want    string
		wantErr bool
	}{
		{name: "inclusive", rangeIn: []any{float64(2), float64(2)}, want: "line2"},
		{name: "through eof", rangeIn: []any{float64(2), float64(0)}, want: "line2\nline3"},
		{name: "inverted", rangeIn: []any{float64(3), float64(1)}, want: ""},
		{name: "past eof", rangeIn: []any{float64(10), float64(12)}, want: ""},
		{name: "wrong arity", rangeIn: []any{float64(2)}, wantErr: true},
		{name: "fractional", rangeIn: []any{1.5, float64(2)}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := reg["read"](context.Background(), sb, map[string]any{
				"path": "lines.txt", "view_range": tt.rangeIn,
			})
			if r.IsError != tt.wantErr {
				t.Fatalf("read = %+v, want error %v", r, tt.wantErr)
			}
			if !tt.wantErr && resultText(t, r) != tt.want {
				t.Fatalf("read text = %q, want %q", resultText(t, r), tt.want)
			}
		})
	}
}

func TestBuiltins_ReadRejectsOversizedFileWithoutReturningItsContents(t *testing.T) {
	sb := newSB(t)
	full := strings.Repeat("secret", MaxReadFileBytes/6+1)
	if err := sb.WriteFile(context.Background(), "large.txt", []byte(full)); err != nil {
		t.Fatal(err)
	}
	r := Registry()["read"](context.Background(), sb, map[string]any{
		"path": "large.txt", "view_range": []any{float64(1), float64(1)},
	})
	if !r.IsError || !contains(r, "use bash") || contains(r, "secret") {
		t.Fatalf("oversized read = %#v", r)
	}
}

func TestBuiltinSchemasOnlyAdvertiseImplementedSemantics(t *testing.T) {
	bashProperties := Schema("bash")["properties"].(map[string]any)
	if _, advertised := bashProperties["restart"]; advertised {
		t.Fatal("bash schema advertises a persistent-shell restart that the executor does not implement")
	}
	readProperties := Schema("read")["properties"].(map[string]any)
	if _, advertised := readProperties["view_range"]; !advertised {
		t.Fatal("read schema omitted the implemented view_range")
	}
}

func TestBuiltins_EditMissingStringIsError(t *testing.T) {
	sb := newSB(t)
	Registry()["write"](context.Background(), sb, map[string]any{"path": "y.txt", "file_text": "abc"})
	r := Registry()["edit"](context.Background(), sb, map[string]any{"path": "y.txt", "old_str": "zzz", "new_str": "q"})
	if !r.IsError {
		t.Fatal("edit on missing old_str must be is_error")
	}
}

func TestBuiltins_NotImplemented(t *testing.T) {
	r := Registry()["web_fetch"](context.Background(), sandboxtest.Inert(t), map[string]any{"url": "http://x"})
	if !r.IsError {
		t.Fatal("web_fetch should report not implemented as is_error")
	}
}

func TestBuiltins_Glob(t *testing.T) {
	sb := newSB(t)
	reg := Registry()
	for _, f := range []string{"a.go", "b.txt", "sub/c.go", "sub/deep/d.go"} {
		if r := reg["write"](context.Background(), sb, map[string]any{"path": f, "file_text": "x"}); r.IsError {
			t.Fatalf("setup write %s: %+v", f, r)
		}
	}
	// "*.go" matches only top-level .go files (no '/').
	r := reg["glob"](context.Background(), sb, map[string]any{"pattern": "*.go"})
	if r.IsError || !contains(r, "a.go") || containsAny(r, "c.go", "d.go", "b.txt") {
		t.Fatalf("glob *.go = %+v", r)
	}
	// "**/*.go" matches .go at any depth.
	r = reg["glob"](context.Background(), sb, map[string]any{"pattern": "**/*.go"})
	if r.IsError || !contains(r, "a.go") || !contains(r, "sub/c.go") || !contains(r, "sub/deep/d.go") || containsAny(r, "b.txt") {
		t.Fatalf("glob **/*.go = %+v", r)
	}
	// No match is not an error.
	r = reg["glob"](context.Background(), sb, map[string]any{"pattern": "*.rs"})
	if r.IsError {
		t.Fatalf("glob no-match should not be error: %+v", r)
	}
	// Missing pattern is an error.
	if r := reg["glob"](context.Background(), sb, map[string]any{}); !r.IsError {
		t.Fatal("glob without pattern must be is_error")
	}
}

func TestBuiltins_Grep(t *testing.T) {
	sb := newSB(t)
	reg := Registry()
	reg["write"](context.Background(), sb, map[string]any{"path": "a.txt", "file_text": "alpha\nbeta\ngamma"})
	reg["write"](context.Background(), sb, map[string]any{"path": "sub/b.txt", "file_text": "delta\nbeta"})
	// Matches across files, recursive; not an error.
	r := reg["grep"](context.Background(), sb, map[string]any{"pattern": "beta"})
	if r.IsError || !contains(r, "a.txt") || !contains(r, "b.txt") {
		t.Fatalf("grep beta = %+v", r)
	}
	// No match is not an error.
	r = reg["grep"](context.Background(), sb, map[string]any{"pattern": "zzz"})
	if r.IsError {
		t.Fatalf("grep no-match should not be error: %+v", r)
	}
	// Missing pattern is an error.
	if r := reg["grep"](context.Background(), sb, map[string]any{}); !r.IsError {
		t.Fatal("grep without pattern must be is_error")
	}
}

func TestGlobToRegexp(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"*.go", "a.go", true},
		{"*.go", "sub/a.go", false},
		{"**/*.go", "a.go", true},
		{"**/*.go", "sub/a.go", true},
		{"**/*.go", "sub/deep/a.go", true},
		{"**/*.go", "a.txt", false},
		{"sub/*.go", "sub/a.go", true},
		{"sub/*.go", "a.go", false},
		{"?.go", "a.go", true},
		{"?.go", "ab.go", false},
	}
	for _, c := range cases {
		re, err := globToRegexp(c.pattern)
		if err != nil {
			t.Fatalf("globToRegexp(%q): %v", c.pattern, err)
		}
		if got := re.MatchString(c.path); got != c.want {
			t.Errorf("glob %q vs %q = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func containsAny(r Result, subs ...string) bool {
	for _, s := range subs {
		if contains(r, s) {
			return true
		}
	}
	return false
}

func contains(r Result, s string) bool {
	for _, b := range r.Content {
		if m, ok := b.(map[string]any); ok {
			if txt, _ := m["text"].(string); strings.Contains(txt, s) {
				return true
			}
		}
	}
	return false
}

func resultText(t *testing.T, r Result) string {
	t.Helper()
	if len(r.Content) != 1 {
		t.Fatalf("result content = %#v", r.Content)
	}
	block, ok := r.Content[0].(map[string]any)
	if !ok {
		t.Fatalf("result block = %#v", r.Content[0])
	}
	text, ok := block["text"].(string)
	if !ok {
		t.Fatalf("result text = %#v", block["text"])
	}
	return text
}
