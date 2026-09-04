package agenttoolset

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	mango "github.com/yanpgwang/mango/sdk/go"
)

const bashFramingAllowance = int64(256)

var (
	errBashClosed     = errors.New("bash: tool is closed")
	errBashTerminated = errors.New("bash: shell terminated")
	errBashTimedOut   = errors.New("bash: command timed out")
	bashANSI          = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
)

// BashInput is the self-hosted bash tool's input contract. Command is optional
// only when Restart is true. TimeoutMs zero uses Context.BashTimeout.
type BashInput struct {
	Command   string `json:"command,omitempty"`
	Restart   bool   `json:"restart,omitempty"`
	TimeoutMs int64  `json:"timeout_ms,omitempty"`
}

// CloseAll closes stateful tools created by New and joins any cleanup errors.
// SessionToolRunner deliberately does not own tool lifetime, so the isolation
// process that created the toolset should defer this function.
func CloseAll(tools []mango.SessionTool) (result error) {
	for _, sessionTool := range tools {
		closer, ok := sessionTool.(io.Closer)
		if !ok {
			continue
		}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					result = errors.Join(result, fmt.Errorf("agenttoolset: close %s: panic: %v", sessionTool.Name(), recovered))
				}
			}()
			if err := closer.Close(); err != nil {
				result = errors.Join(result, fmt.Errorf("agenttoolset: close %s: %w", sessionTool.Name(), err))
			}
		}()
	}
	return result
}

type bashTool struct {
	workspace *workspace

	callMu sync.Mutex
	state  sync.Mutex
	shell  *bashSession
	closed bool
}

func newBashTool(workspace *workspace) *bashTool {
	return &bashTool{workspace: workspace}
}

func (t *bashTool) Name() string { return "bash" }

func (t *bashTool) Execute(ctx context.Context, call mango.SessionToolCall) ([]mango.ResultContentInput, error) {
	if call.Name != "" && call.Name != t.Name() {
		return nil, fmt.Errorf("agenttoolset: bash executor received %q", call.Name)
	}
	var input BashInput
	if err := json.Unmarshal(call.Input, &input); err != nil {
		return nil, fmt.Errorf("bash: decode input: %w", err)
	}
	if input.TimeoutMs < 0 {
		return nil, errors.New("bash: timeout_ms must be non-negative")
	}
	if input.TimeoutMs > int64((time.Duration(1<<63-1))/time.Millisecond) {
		return nil, errors.New("bash: timeout_ms exceeds duration range")
	}

	t.callMu.Lock()
	defer t.callMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if input.Restart {
		if err := t.restart(); err != nil {
			return nil, fmt.Errorf("bash: restart shell: %w", err)
		}
		if strings.TrimSpace(input.Command) == "" {
			return textResult("bash session restarted"), nil
		}
	}
	if strings.TrimSpace(input.Command) == "" {
		return nil, errors.New("bash: command is required")
	}

	shell, err := t.session()
	if err != nil {
		return nil, err
	}
	timeout := t.workspace.bashTO
	if input.TimeoutMs > 0 {
		timeout = time.Duration(input.TimeoutMs) * time.Millisecond
	}
	output, exitCode, runErr := shell.execute(ctx, input.Command, timeout, true)
	if runErr != nil {
		restartErr := t.restart()
		if errors.Is(runErr, errBashTimedOut) {
			message := appendBashStatus(output, "[timed out]", "session restarted after timeout")
			if restartErr != nil {
				message = appendBashStatus(message, "restart error: "+restartErr.Error())
			}
			return nil, errors.New(message)
		}
		if restartErr != nil {
			return nil, fmt.Errorf("bash: %w", errors.Join(runErr, fmt.Errorf("restart shell: %w", restartErr)))
		}
		return nil, fmt.Errorf("bash: %w", runErr)
	}
	if exitCode != 0 {
		return nil, errors.New(appendBashStatus(output, fmt.Sprintf("exit code: %d", exitCode)))
	}
	return textResult(output), nil
}

func (t *bashTool) session() (*bashSession, error) {
	t.state.Lock()
	defer t.state.Unlock()
	if t.closed {
		return nil, errBashClosed
	}
	if t.shell != nil {
		return t.shell, nil
	}
	shell, err := startBashSession(
		t.workspace.root,
		t.workspace.env,
		t.workspace.maxOutput,
		t.workspace.closeTO,
	)
	if err != nil {
		return nil, fmt.Errorf("bash: start shell: %w", err)
	}
	t.shell = shell
	return shell, nil
}

func (t *bashTool) restart() error {
	t.state.Lock()
	shell := t.shell
	t.shell = nil
	closed := t.closed
	t.state.Unlock()
	if closed && shell == nil {
		return errBashClosed
	}
	if shell == nil {
		return nil
	}
	return shell.Close()
}

func (t *bashTool) Close() error {
	t.state.Lock()
	if t.closed {
		t.state.Unlock()
		return nil
	}
	t.closed = true
	shell := t.shell
	t.shell = nil
	t.state.Unlock()
	if shell == nil {
		return nil
	}
	return shell.Close()
}

type bashSession struct {
	terminal *os.File
	command  *exec.Cmd

	outputMu  sync.Mutex
	output    []byte
	truncated bool
	maxOutput int64
	notify    chan struct{}
	drainDone chan struct{}

	processDone chan struct{}
	waitMu      sync.Mutex
	waitErr     error

	closeMu      sync.Mutex
	closed       bool
	closeTimeout time.Duration
}

func startBashSession(workdir string, environment []string, maxOutput int64, closeTimeout time.Duration) (*bashSession, error) {
	command := exec.Command("/bin/bash", "--noprofile", "--norc")
	command.Dir = workdir
	command.Env = overlayEnvironment(environment, map[string]string{"PS1": "", "PS2": "", "TERM": "dumb"})
	terminal, err := pty.Start(command)
	if err != nil {
		return nil, err
	}
	session := &bashSession{
		terminal: terminal, command: command, maxOutput: maxOutput,
		notify: make(chan struct{}, 1), drainDone: make(chan struct{}),
		processDone: make(chan struct{}), closeTimeout: closeTimeout,
	}
	go session.drain()
	go session.reap()
	if _, _, err := session.execute(context.Background(), "stty -echo 2>/dev/null; set +m", 5*time.Second, false); err != nil {
		closeErr := session.Close()
		return nil, errors.Join(fmt.Errorf("initialize terminal: %w", err), closeErr)
	}
	return session, nil
}

func (s *bashSession) drain() {
	defer close(s.drainDone)
	buffer := make([]byte, 4096)
	for {
		count, err := s.terminal.Read(buffer)
		if count > 0 {
			s.outputMu.Lock()
			s.output = append(s.output, buffer[:count]...)
			limit := s.maxOutput + bashFramingAllowance
			if int64(len(s.output)) > limit {
				over := int64(len(s.output)) - limit
				s.output = append([]byte(nil), s.output[over:]...)
				s.truncated = true
			}
			s.outputMu.Unlock()
			select {
			case s.notify <- struct{}{}:
			default:
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *bashSession) reap() {
	err := s.command.Wait()
	s.waitMu.Lock()
	s.waitErr = err
	s.waitMu.Unlock()
	close(s.processDone)
}

func (s *bashSession) execute(ctx context.Context, command string, timeout time.Duration, redirectStdin bool) (string, int, error) {
	if timeout <= 0 {
		return "", -1, errors.New("bash execution timeout must be positive")
	}
	s.closeMu.Lock()
	closed := s.closed
	s.closeMu.Unlock()
	if closed {
		return "", -1, errBashClosed
	}

	s.outputMu.Lock()
	s.output = s.output[:0]
	s.truncated = false
	s.outputMu.Unlock()

	marker, err := bashMarker()
	if err != nil {
		return "", -1, err
	}
	redirect := ""
	if redirectStdin {
		redirect = " </dev/null"
	}
	half := len(marker) / 2
	framed := fmt.Sprintf(
		"{\n%s\n}%s 2>&1\nbuiltin printf '%%s%%s%%d\\n' '%s' '%s' \"$?\"\n",
		command, redirect, marker[:half], marker[half:],
	)
	if _, err := io.WriteString(s.terminal, framed); err != nil {
		return "", -1, fmt.Errorf("write command: %w", err)
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", -1, ctx.Err()
		case <-timer.C:
			output, truncated := s.snapshot()
			return cleanBashOutput(output, truncated, s.maxOutput), -1, errBashTimedOut
		case <-s.processDone:
			return "", -1, errBashTerminated
		case <-s.drainDone:
			return "", -1, errBashTerminated
		case <-s.notify:
		}

		output, truncated := s.snapshot()
		body, exitCode, complete := findBashCompletion(output, marker)
		if !complete {
			continue
		}
		return cleanBashOutput(body, truncated, s.maxOutput), exitCode, nil
	}
}

func findBashCompletion(output []byte, marker string) ([]byte, int, bool) {
	markerBytes := []byte(marker)
	for offset := 0; offset < len(output); {
		relative := bytes.Index(output[offset:], markerBytes)
		if relative < 0 {
			return nil, 0, false
		}
		index := offset + relative
		tail := output[index+len(markerBytes):]
		lineEnd := bytes.IndexByte(tail, '\n')
		if lineEnd < 0 {
			return nil, 0, false
		}
		exitCode, err := strconv.ParseInt(strings.TrimSpace(string(tail[:lineEnd])), 10, 32)
		if err == nil {
			return output[:index], int(exitCode), true
		}
		// Ignore any marker-like output that is not followed by an exit status.
		// The unpredictable full marker is never present in the input framing.
		offset = index + len(markerBytes)
	}
	return nil, 0, false
}

func (s *bashSession) snapshot() ([]byte, bool) {
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	return append([]byte(nil), s.output...), s.truncated
}

func (s *bashSession) Close() error {
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return nil
	}
	s.closed = true
	s.closeMu.Unlock()

	var result error
	if err := killProcessGroup(s.command.Process); err != nil && !errors.Is(err, os.ErrProcessDone) {
		result = errors.Join(result, fmt.Errorf("terminate process group: %w", err))
	}
	if err := s.terminal.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
		result = errors.Join(result, fmt.Errorf("close terminal: %w", err))
	}
	timer := time.NewTimer(s.closeTimeout)
	defer timer.Stop()
	select {
	case <-s.processDone:
		s.waitMu.Lock()
		waitErr := s.waitErr
		s.waitMu.Unlock()
		var exitErr *exec.ExitError
		if waitErr != nil && !errors.As(waitErr, &exitErr) {
			result = errors.Join(result, fmt.Errorf("reap shell: %w", waitErr))
		}
	case <-timer.C:
		result = errors.Join(result, fmt.Errorf("reap shell exceeded %s", s.closeTimeout))
	}
	return result
}

func bashMarker() (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("create completion marker: %w", err)
	}
	return "__MANGO_BASH_" + hex.EncodeToString(random) + "__", nil
}

func cleanBashOutput(output []byte, truncated bool, limit int64) string {
	clean := bashANSI.ReplaceAll(output, nil)
	clean = bytes.ReplaceAll(clean, []byte("\r\n"), []byte("\n"))
	clean = bytes.ReplaceAll(clean, []byte("\r"), nil)
	if int64(len(clean)) > limit {
		clean = clean[int64(len(clean))-limit:]
		truncated = true
	}
	text := string(clean)
	if truncated {
		return fmt.Sprintf("[output truncated at %d bytes]\n%s", limit, text)
	}
	return text
}

func overlayEnvironment(base []string, values map[string]string) []string {
	result := make([]string, 0, len(base)+len(values))
	for _, entry := range base {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, replaced := values[name]; !replaced {
			result = append(result, entry)
		}
	}
	for _, name := range []string{"PS1", "PS2", "TERM"} {
		result = append(result, name+"="+values[name])
	}
	return result
}

func appendBashStatus(output string, status ...string) string {
	parts := make([]string, 0, len(status)+1)
	if strings.TrimSpace(output) != "" {
		parts = append(parts, strings.TrimRight(output, "\n"))
	}
	parts = append(parts, status...)
	return strings.Join(parts, "\n")
}

var (
	_ mango.SessionTool = (*bashTool)(nil)
	_ io.Closer         = (*bashTool)(nil)
)
