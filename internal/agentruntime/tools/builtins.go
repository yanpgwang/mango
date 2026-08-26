package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/yanpgwang/mango/internal/sandbox"
)

const MaxReadFileBytes = 64 << 10

// execBash runs a shell command inside the sandbox and returns its combined
// stdout+stderr as text. A non-zero exit code is reported as a tool error.
func execBash(ctx context.Context, sb sandbox.Sandbox, in map[string]any) Result {
	cmd, _ := in["command"].(string)
	if cmd == "" {
		return textResult("bash: command is required", true)
	}
	res, err := sb.Exec(ctx, sandbox.Command{Path: "/bin/sh", Args: []string{"-c", cmd}})
	if err != nil {
		return textResult("bash: "+err.Error(), true)
	}
	out := string(res.Stdout) + string(res.Stderr)
	if res.TimedOut {
		out += "\n[timed out]"
	}
	return textResult(out, res.ExitCode != 0)
}

// execRead reads a file from the sandbox and returns its contents as text.
func execRead(ctx context.Context, sb sandbox.Sandbox, in map[string]any) Result {
	path, _ := in["path"].(string)
	if path == "" {
		return textResult("read: path is required", true)
	}
	reader, ok := sb.(sandbox.BoundedFileReader)
	if !ok {
		return textResult("read: sandbox provider does not support bounded file reads", true)
	}
	data, truncated, err := reader.ReadFileBounded(ctx, path, MaxReadFileBytes)
	if err != nil {
		return textResult("read: "+err.Error(), true)
	}
	if truncated {
		return textResult(fmt.Sprintf(
			"read: file exceeds the %d-byte read limit; use bash with dd, head, tail, or sed to print a bounded slice",
			MaxReadFileBytes,
		), true)
	}
	startLine, endLine, ranged, err := parseViewRange(in["view_range"])
	if err != nil {
		return textResult("read: "+err.Error(), true)
	}
	if !ranged {
		return textResult(string(data), false)
	}
	lines := strings.Split(string(data), "\n")
	start := 0
	if startLine > 1 {
		start = startLine - 1
	}
	if start >= len(lines) {
		return textResult("", false)
	}
	end := len(lines)
	if endLine > 0 && endLine < end {
		end = endLine
	}
	if end < start {
		return textResult("", false)
	}
	return textResult(strings.Join(lines[start:end], "\n"), false)
}

func parseViewRange(raw any) (start, end int, present bool, err error) {
	if raw == nil {
		return 0, 0, false, nil
	}
	values, ok := raw.([]any)
	if !ok || len(values) != 2 {
		return 0, 0, true, fmt.Errorf("view_range must be [start_line, end_line]")
	}
	start, err = inputInteger(values[0])
	if err != nil {
		return 0, 0, true, fmt.Errorf("view_range start_line: %w", err)
	}
	end, err = inputInteger(values[1])
	if err != nil {
		return 0, 0, true, fmt.Errorf("view_range end_line: %w", err)
	}
	return start, end, true, nil
}

func inputInteger(raw any) (int, error) {
	switch value := raw.(type) {
	case int:
		return value, nil
	case int64:
		converted := int(value)
		if int64(converted) != value {
			return 0, fmt.Errorf("must fit in an integer")
		}
		return converted, nil
	case json.Number:
		value64, err := value.Int64()
		if err != nil {
			return 0, fmt.Errorf("must be an integer")
		}
		return inputInteger(value64)
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) || math.Trunc(value) != value {
			return 0, fmt.Errorf("must be an integer")
		}
		converted := int(value)
		if float64(converted) != value {
			return 0, fmt.Errorf("must fit in an integer")
		}
		return converted, nil
	default:
		return 0, fmt.Errorf("must be an integer")
	}
}

// execWrite writes file_text to a file in the sandbox, creating or truncating.
func execWrite(ctx context.Context, sb sandbox.Sandbox, in map[string]any) Result {
	path, _ := in["path"].(string)
	if path == "" {
		return textResult("write: path is required", true)
	}
	text, _ := in["file_text"].(string)
	if err := sb.WriteFile(ctx, path, []byte(text)); err != nil {
		return textResult("write: "+err.Error(), true)
	}
	return textResult("wrote "+path, false)
}

// execEdit replaces a single, unique occurrence of old_str with new_str in the
// named file. It is a tool error if old_str is empty, not found, or not unique.
func execEdit(ctx context.Context, sb sandbox.Sandbox, in map[string]any) Result {
	path, _ := in["path"].(string)
	if path == "" {
		return textResult("edit: path is required", true)
	}
	oldS, _ := in["old_str"].(string)
	newS, _ := in["new_str"].(string)
	data, err := sb.ReadFile(ctx, path)
	if err != nil {
		return textResult("edit: "+err.Error(), true)
	}
	s := string(data)
	switch {
	case oldS == "" || strings.Count(s, oldS) == 0:
		return textResult("edit: old_str not found", true)
	case strings.Count(s, oldS) > 1:
		return textResult("edit: old_str is not unique", true)
	}
	if err := sb.WriteFile(ctx, path, []byte(strings.Replace(s, oldS, newS, 1))); err != nil {
		return textResult("edit: "+err.Error(), true)
	}
	return textResult("edited "+path, false)
}

// execGlob lists files under an optional root and returns those whose
// sandbox-relative path matches the glob pattern. Enumeration runs in the
// sandbox via `find`; matching is done in Go so the semantics are testable.
// `*` and `?` match within a path segment, `**` matches across segments.
func execGlob(ctx context.Context, sb sandbox.Sandbox, in map[string]any) Result {
	pattern, _ := in["pattern"].(string)
	if pattern == "" {
		return textResult("glob: pattern is required", true)
	}
	re, err := globToRegexp(pattern)
	if err != nil {
		return textResult("glob: invalid pattern: "+err.Error(), true)
	}
	root, _ := in["path"].(string)
	if root == "" {
		root = "."
	}
	files, res, ok := listFiles(ctx, sb, root)
	if !ok {
		return res
	}
	var matched []string
	for _, f := range files {
		if re.MatchString(f) {
			matched = append(matched, f)
		}
	}
	sort.Strings(matched)
	return textResult(strings.Join(matched, "\n"), false)
}

// execGrep searches file contents for a regular expression, recursively from an
// optional root. Command arguments are passed as argv (no shell), so the
// pattern cannot be interpreted as shell syntax. A no-match result (grep exit
// code 1) is not an error; only a real grep failure (exit >= 2) is.
func execGrep(ctx context.Context, sb sandbox.Sandbox, in map[string]any) Result {
	pattern, _ := in["pattern"].(string)
	if pattern == "" {
		return textResult("grep: pattern is required", true)
	}
	path, _ := in["path"].(string)
	if path == "" {
		path = "."
	}
	res, err := sb.Exec(ctx, sandbox.Command{
		Path: "grep",
		Args: []string{"-rnE", "--", pattern, path},
	})
	if err != nil {
		return textResult("grep: "+err.Error(), true)
	}
	if res.TimedOut {
		return textResult("grep: timed out", true)
	}
	// grep exit codes: 0 = matches, 1 = no matches, >=2 = error.
	if res.ExitCode >= 2 {
		return textResult("grep: "+strings.TrimSpace(string(res.Stderr)), true)
	}
	return textResult(string(res.Stdout), false)
}

// listFiles enumerates regular files under root inside the sandbox using
// `find`, returning sandbox-relative paths (the leading "./" is stripped). On a
// find failure it returns ok=false with an error Result for the caller.
func listFiles(ctx context.Context, sb sandbox.Sandbox, root string) ([]string, Result, bool) {
	res, err := sb.Exec(ctx, sandbox.Command{
		Path: "find",
		Args: []string{root, "-type", "f"},
	})
	if err != nil {
		return nil, textResult("glob: "+err.Error(), true), false
	}
	if res.TimedOut {
		return nil, textResult("glob: timed out", true), false
	}
	if res.ExitCode != 0 {
		return nil, textResult("glob: "+strings.TrimSpace(string(res.Stderr)), true), false
	}
	var files []string
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		files = append(files, strings.TrimPrefix(line, "./"))
	}
	return files, Result{}, true
}

// globToRegexp compiles a glob pattern into an anchored regexp. Rules:
//   - `**` matches any characters including path separators (any depth);
//   - `*` matches any run of non-separator characters within one segment;
//   - `?` matches a single non-separator character;
//   - all other characters are matched literally.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				// `**` — any depth, including separators.
				b.WriteString(".*")
				i++
				// Swallow a following separator so "**/x" also matches "x".
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					b.WriteString("(?:/)?")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
