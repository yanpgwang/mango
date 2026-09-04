// Package agenttoolset provides provider-neutral local implementations of
// Mango's core coding tools. It is intended to run inside an isolation boundary
// supplied by the caller; it does not create or claim to be a sandbox.
package agenttoolset

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	mango "github.com/yanpgwang/mango/sdk/go"
)

const (
	defaultMaxReadBytes   = int64(64 << 10)
	defaultMaxEditBytes   = int64(1 << 20)
	defaultMaxOutputBytes = int64(100_000)
	defaultMaxWalkEntries = 50_000
	defaultMaxGlobResults = 200
	defaultBashTimeout    = 120 * time.Second
	defaultBashCloseWait  = 2 * time.Second
)

// Context configures a core agent toolset rooted at Workdir. Workdir must be
// an existing directory inside an isolation boundary owned by the caller.
// Environment defaults to os.Environ. Mango credentials are always removed
// before the persistent bash process is started. File tools are confined to
// Workdir; bash is unrestricted within the caller's isolation boundary.
type Context struct {
	Workdir        string
	Environment    []string
	MaxReadBytes   int64
	MaxEditBytes   int64
	MaxOutputBytes int64
	// BashTimeout bounds a bash invocation when its input omits timeout_ms or
	// sets it to zero. Zero uses 120 seconds.
	BashTimeout time.Duration
	// BashCloseTimeout bounds process reaping when the toolset is closed or a
	// damaged shell is restarted. Zero uses two seconds.
	BashCloseTimeout time.Duration
}

// New returns bash, read, write, edit, glob, and grep executors in stable order.
// The package deliberately does not launch a sandbox or execute server-side
// tools such as web_search and web_fetch.
func New(options Context) ([]mango.SessionTool, error) {
	workspace, err := newWorkspace(options)
	if err != nil {
		return nil, err
	}
	return []mango.SessionTool{
		newBashTool(workspace),
		&tool{name: "read", workspace: workspace},
		&tool{name: "write", workspace: workspace},
		&tool{name: "edit", workspace: workspace},
		&tool{name: "glob", workspace: workspace},
		&tool{name: "grep", workspace: workspace},
	}, nil
}

type tool struct {
	name      string
	workspace *workspace
}

func (t *tool) Name() string { return t.name }

func (t *tool) Execute(ctx context.Context, call mango.SessionToolCall) ([]mango.ResultContentInput, error) {
	if call.Name != "" && call.Name != t.name {
		return nil, fmt.Errorf("agenttoolset: %s executor received %q", t.name, call.Name)
	}
	input := make(map[string]json.RawMessage)
	decoder := json.NewDecoder(bytes.NewReader(call.Input))
	decoder.UseNumber()
	if err := decoder.Decode(&input); err != nil {
		return nil, fmt.Errorf("%s: decode input: %w", t.name, err)
	}
	var output string
	var err error
	switch t.name {
	case "read":
		output, err = t.workspace.read(ctx, input)
	case "write":
		output, err = t.workspace.write(ctx, stringInput(input, "path"), stringInput(input, "file_text"))
	case "edit":
		output, err = t.workspace.edit(ctx, stringInput(input, "path"), stringInput(input, "old_str"), stringInput(input, "new_str"))
	case "glob":
		output, err = t.workspace.glob(ctx, stringInput(input, "pattern"), stringInput(input, "path"))
	case "grep":
		output, err = t.workspace.grep(ctx, stringInput(input, "pattern"), stringInput(input, "path"))
	default:
		err = fmt.Errorf("agenttoolset: unsupported tool %q", t.name)
	}
	if err != nil {
		return nil, err
	}
	return textResult(output), nil
}

type workspace struct {
	root      string
	env       []string
	maxRead   int64
	maxEdit   int64
	maxOutput int64
	bashTO    time.Duration
	closeTO   time.Duration
}

func newWorkspace(options Context) (*workspace, error) {
	if strings.TrimSpace(options.Workdir) == "" {
		return nil, errors.New("agenttoolset: workdir is required")
	}
	root, err := filepath.Abs(options.Workdir)
	if err != nil {
		return nil, fmt.Errorf("agenttoolset: resolve workdir: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, fmt.Errorf("agenttoolset: resolve workdir symlinks: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("agenttoolset: stat workdir: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("agenttoolset: workdir must be a directory")
	}
	if options.MaxReadBytes < 0 || options.MaxEditBytes < 0 || options.MaxOutputBytes < 0 {
		return nil, errors.New("agenttoolset: byte limits must be non-negative")
	}
	if options.MaxOutputBytes > int64(^uint64(0)>>1)-bashFramingAllowance {
		return nil, errors.New("agenttoolset: max output bytes exceeds supported range")
	}
	if options.BashTimeout < 0 || options.BashCloseTimeout < 0 {
		return nil, errors.New("agenttoolset: bash timeouts must be non-negative")
	}
	if options.MaxReadBytes == 0 {
		options.MaxReadBytes = defaultMaxReadBytes
	}
	if options.MaxEditBytes == 0 {
		options.MaxEditBytes = defaultMaxEditBytes
	}
	if options.MaxOutputBytes == 0 {
		options.MaxOutputBytes = defaultMaxOutputBytes
	}
	if options.BashTimeout == 0 {
		options.BashTimeout = defaultBashTimeout
	}
	if options.BashCloseTimeout == 0 {
		options.BashCloseTimeout = defaultBashCloseWait
	}
	environment := options.Environment
	if environment == nil {
		environment = os.Environ()
	}
	return &workspace{
		root: root, env: scrubCredentials(environment), maxRead: options.MaxReadBytes,
		maxEdit: options.MaxEditBytes, maxOutput: options.MaxOutputBytes,
		bashTO: options.BashTimeout, closeTO: options.BashCloseTimeout,
	}, nil
}

func (w *workspace) read(ctx context.Context, input map[string]json.RawMessage) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path := stringInput(input, "path")
	if path == "" {
		return "", errors.New("read: path is required")
	}
	resolved, err := w.resolveExisting(path)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	data, err := readBounded(resolved, w.maxRead)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	start, end, present, err := viewRange(input["view_range"])
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	if !present {
		return string(data), nil
	}
	lines := strings.Split(string(data), "\n")
	if start < 1 {
		start = 1
	}
	if start > len(lines) {
		return "", nil
	}
	last := len(lines)
	if end > 0 && end < last {
		last = end
	}
	if last < start {
		return "", nil
	}
	return strings.Join(lines[start-1:last], "\n"), nil
}

func (w *workspace) write(ctx context.Context, path, contents string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if path == "" {
		return "", errors.New("write: path is required")
	}
	if int64(len(contents)) > w.maxEdit {
		return "", fmt.Errorf("write: content exceeds the %d-byte limit", w.maxEdit)
	}
	resolved, err := w.resolveForWrite(path)
	if err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	if err := atomicWrite(resolved, []byte(contents)); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	return "wrote " + path, nil
}

func (w *workspace) edit(ctx context.Context, path, oldValue, newValue string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if path == "" {
		return "", errors.New("edit: path is required")
	}
	if oldValue == "" {
		return "", errors.New("edit: old_str is required")
	}
	resolved, err := w.resolveExisting(path)
	if err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	data, err := readBounded(resolved, w.maxEdit)
	if err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	count := strings.Count(string(data), oldValue)
	if count == 0 {
		return "", errors.New("edit: old_str not found")
	}
	if count != 1 {
		return "", errors.New("edit: old_str is not unique")
	}
	updated := strings.Replace(string(data), oldValue, newValue, 1)
	if int64(len(updated)) > w.maxEdit {
		return "", fmt.Errorf("edit: result exceeds the %d-byte limit", w.maxEdit)
	}
	if err := atomicWrite(resolved, []byte(updated)); err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	return "edited " + path, nil
}

func (w *workspace) glob(ctx context.Context, pattern, root string) (string, error) {
	if pattern == "" {
		return "", errors.New("glob: pattern is required")
	}
	matcher, err := globRegexp(filepath.ToSlash(filepath.Clean(pattern)))
	if err != nil {
		return "", fmt.Errorf("glob: invalid pattern: %w", err)
	}
	if root == "" {
		root = "."
	}
	searchRoot, err := w.resolveExisting(root)
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	var matches []string
	visited := 0
	err = filepath.WalkDir(searchRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		visited++
		if visited > defaultMaxWalkEntries {
			return errors.New("workspace walk exceeds the 50000-entry limit")
		}
		if entry.Type()&os.ModeSymlink != 0 && entry.IsDir() {
			return filepath.SkipDir
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		relative, err := filepath.Rel(w.root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if matcher.MatchString(relative) {
			matches = append(matches, relative)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("glob: %w", err)
	}
	sort.Strings(matches)
	if len(matches) > defaultMaxGlobResults {
		matches = matches[:defaultMaxGlobResults]
	}
	return limitText(strings.Join(matches, "\n"), w.maxOutput), nil
}

func (w *workspace) grep(ctx context.Context, pattern, root string) (string, error) {
	if pattern == "" {
		return "", errors.New("grep: pattern is required")
	}
	matcher, err := regexp.Compile(pattern)
	if err != nil {
		return "", fmt.Errorf("grep: invalid pattern: %w", err)
	}
	if root == "" {
		root = "."
	}
	searchRoot, err := w.resolveExisting(root)
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	info, err := os.Stat(searchRoot)
	if err != nil {
		return "", fmt.Errorf("grep: %w", err)
	}
	paths := []string{searchRoot}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return "", errors.New("grep: path is not a regular file or directory")
	}
	if info.IsDir() {
		paths = nil
		visited := 0
		err = filepath.WalkDir(searchRoot, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			visited++
			if visited > defaultMaxWalkEntries {
				return errors.New("workspace walk exceeds the 50000-entry limit")
			}
			if entry.Type()&os.ModeSymlink != 0 && entry.IsDir() {
				return filepath.SkipDir
			}
			if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				entryInfo, infoErr := entry.Info()
				if infoErr == nil && entryInfo.Mode().IsRegular() && entryInfo.Size() <= w.maxEdit {
					paths = append(paths, path)
				}
			}
			return nil
		})
		if err != nil {
			return "", fmt.Errorf("grep: %w", err)
		}
	}
	var output strings.Builder
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		file, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("grep: %w", err)
		}
		relative, err := filepath.Rel(w.root, path)
		if err != nil {
			file.Close()
			return "", fmt.Errorf("grep: %w", err)
		}
		scanner := bufio.NewScanner(io.LimitReader(file, w.maxEdit+1))
		scanner.Buffer(make([]byte, 64<<10), int(w.maxEdit))
		line := 0
		for scanner.Scan() {
			line++
			value := scanner.Text()
			if matcher.MatchString(value) {
				fmt.Fprintf(&output, "%s:%d:%s\n", filepath.ToSlash(relative), line, value)
				if int64(output.Len()) > w.maxOutput {
					break
				}
			}
		}
		scanErr := scanner.Err()
		file.Close()
		if scanErr != nil {
			return "", fmt.Errorf("grep: scan %s: %w", filepath.ToSlash(relative), scanErr)
		}
		if int64(output.Len()) > w.maxOutput {
			break
		}
	}
	return strings.TrimSuffix(limitText(output.String(), w.maxOutput), "\n"), nil
}

func (w *workspace) resolveExisting(path string) (string, error) {
	candidate, err := w.lexicalPath(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", err
	}
	if !pathWithin(w.root, resolved) {
		return "", errors.New("path escapes the workspace")
	}
	return resolved, nil
}

func (w *workspace) resolveForWrite(path string) (string, error) {
	candidate, err := w.lexicalPath(path)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(candidate); err == nil {
		if !pathWithin(w.root, resolved) {
			return "", errors.New("path escapes the workspace")
		}
		return resolved, nil
	}
	parent := filepath.Dir(candidate)
	for {
		resolvedParent, evalErr := filepath.EvalSymlinks(parent)
		if evalErr == nil {
			if !pathWithin(w.root, resolvedParent) {
				return "", errors.New("path escapes the workspace")
			}
			break
		}
		next := filepath.Dir(parent)
		if next == parent || !pathWithin(w.root, next) {
			return "", evalErr
		}
		parent = next
	}
	return candidate, nil
}

func (w *workspace) lexicalPath(path string) (string, error) {
	if filepath.IsAbs(path) {
		return "", errors.New("absolute paths are not allowed")
	}
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes the workspace")
	}
	candidate := filepath.Join(w.root, clean)
	if !pathWithin(w.root, candidate) {
		return "", errors.New("path escapes the workspace")
	}
	return candidate, nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("path is not a regular file")
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("file exceeds the %d-byte limit", limit)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds the %d-byte limit", limit)
	}
	return data, nil
}

func atomicWrite(path string, data []byte) error {
	if info, err := os.Stat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("destination is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".mango-write-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

func stringInput(input map[string]json.RawMessage, name string) string {
	var value string
	_ = json.Unmarshal(input[name], &value)
	return value
}

func viewRange(raw json.RawMessage) (start, end int, present bool, err error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return 0, 0, false, nil
	}
	var values []json.Number
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&values); err != nil || len(values) != 2 {
		return 0, 0, true, errors.New("view_range must be [start_line, end_line]")
	}
	start64, err := values[0].Int64()
	if err != nil {
		return 0, 0, true, errors.New("view_range start_line must be an integer")
	}
	end64, err := values[1].Int64()
	if err != nil {
		return 0, 0, true, errors.New("view_range end_line must be an integer")
	}
	start, end = int(start64), int(end64)
	if int64(start) != start64 || int64(end) != end64 {
		return 0, 0, true, errors.New("view_range values exceed integer range")
	}
	return start, end, true, nil
}

func globRegexp(pattern string) (*regexp.Regexp, error) {
	var expression strings.Builder
	expression.WriteByte('^')
	for i := 0; i < len(pattern); {
		switch pattern[i] {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i += 2
				if i < len(pattern) && pattern[i] == '/' {
					expression.WriteString("(?:.*/)?")
					i++
				} else {
					expression.WriteString(".*")
				}
			} else {
				expression.WriteString("[^/]*")
				i++
			}
		case '?':
			expression.WriteString("[^/]")
			i++
		default:
			expression.WriteString(regexp.QuoteMeta(string(pattern[i])))
			i++
		}
	}
	expression.WriteByte('$')
	return regexp.Compile(expression.String())
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func scrubCredentials(environment []string) []string {
	clean := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if !strings.HasPrefix(name, "MANGO_") {
			clean = append(clean, entry)
		}
	}
	return clean
}

func textResult(value string) []mango.ResultContentInput {
	return []mango.ResultContentInput{{TextBlockInput: &mango.TextBlockInput{Type: "text", Text: value}}}
}

func limitText(value string, limit int64) string {
	if int64(len(value)) <= limit {
		return value
	}
	return value[:limit] + fmt.Sprintf("\n[output truncated at %d bytes]", limit)
}

var _ mango.SessionTool = (*tool)(nil)
