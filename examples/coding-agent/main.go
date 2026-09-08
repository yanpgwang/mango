// Command coding-agent fixes a small Python project using Mango's public SDK
// and the standalone Docker supervisor. Run against a real Mango deployment.
package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	mango "github.com/yanpgwang/mango/sdk/go"
)

//go:embed fixture/*.py
var fixture embed.FS

type demo struct {
	client                                         *mango.Client
	root, baseURL, sandboxURL, image, worker, user string
	environmentID, agentID, sessionID              string
	files                                          []string
	supervisor                                     *exec.Cmd
	supervisorDone                                 chan error
	log                                            *os.File
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "Coding agent demo failed:", err)
		os.Exit(1)
	}
}

func run() (result error) {
	modelID := os.Getenv("MANGO_EXAMPLE_MODEL_ID")
	if modelID == "" || os.Getenv("MANGO_API_KEY") == "" {
		return errors.New("set MANGO_EXAMPLE_MODEL_ID and MANGO_API_KEY for a running Mango deployment with Files enabled")
	}
	if os.Getuid() == 0 {
		return errors.New("run this local Docker demo as your normal, non-root user")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, timeout := context.WithTimeout(ctx, 10*time.Minute)
	defer timeout()
	d := &demo{
		baseURL:    envOr("MANGO_EXAMPLE_BASE_URL", "http://localhost:8080"),
		sandboxURL: envOr("MANGO_DOCKER_BASE_URL", "http://host.docker.internal:8080"),
		image:      envOr("MANGO_WORKER_IMAGE", "mango-self-hosted-worker:local"),
		worker:     envOr("MANGO_EXAMPLE_WORKER", "bin/mango-worker"),
		user:       fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
	}
	var err error
	d.client, err = mango.New(mango.Config{BaseURL: d.baseURL, APIKey: os.Getenv("MANGO_API_KEY")})
	if err != nil {
		return err
	}
	// Use a fresh output directory every time; never overwrite a user's checkout.
	parent := envOr("MANGO_EXAMPLE_OUTPUT_DIR", os.TempDir())
	d.root, err = os.MkdirTemp(parent, "mango-coding-agent-")
	if err != nil {
		return err
	}
	d.root, err = filepath.Abs(d.root)
	if err != nil {
		return err
	}
	d.root, err = filepath.EvalSymlinks(d.root)
	if err != nil {
		return err
	}
	fmt.Println("Artifacts:", d.root)
	d.log, err = os.Create(filepath.Join(d.root, "worker.log"))
	if err != nil {
		return err
	}
	defer func() {
		result = errors.Join(result, d.stopWorker(), d.cleanup(result != nil), d.log.Close())
	}()
	if err := exec.CommandContext(ctx, "docker", "image", "inspect", d.image).Run(); err != nil {
		return fmt.Errorf("build the worker image first (see docs/examples/coding-agent.md): %w", err)
	}
	env, err := d.client.Environments.New(ctx, mango.EnvironmentCreateRequest{Name: "Coding agent demo"})
	if err != nil {
		return err
	}
	d.environmentID = env.ID
	agent, err := d.client.Agents.New(ctx, mango.AgentCreateRequest{
		Name: "Invoice debugger", Model: mango.ModelID(modelID),
		System: mango.SomePtr("You repair Python code. Run the supplied tests first, inspect failures, fix the implementation, and rerun until green. Do not change tests. Work in /workspace. Use Python's standard library; no dependency installation is needed."),
		Tools: mango.Some([]mango.AgentTool{{BuiltinToolset: &mango.BuiltinToolset{
			Type: "agent_toolset_20260401",
			Configs: mango.Some([]mango.BuiltinToolsetConfigsItem{
				{Name: "bash", Enabled: mango.Some(true)}, {Name: "read", Enabled: mango.Some(true)},
				{Name: "write", Enabled: mango.Some(true)}, {Name: "edit", Enabled: mango.Some(true)},
				{Name: "glob", Enabled: mango.Some(true)}, {Name: "grep", Enabled: mango.Some(true)},
			}),
			DefaultConfig: mango.Some(mango.ToolDefaultConfig{Enabled: mango.Some(false), PermissionPolicy: mango.Some(mango.PermissionPolicy{Type: "always_allow"})}),
		}}}),
	})
	if err != nil {
		return err
	}
	d.agentID = agent.ID
	session, err := d.client.Sessions.New(ctx, mango.SessionCreateRequest{
		Agent: mango.AgentVersion(agent.ID, agent.Version), EnvironmentID: env.ID, Title: mango.Some("Fix invoice calculations"),
	})
	if err != nil {
		return err
	}
	d.sessionID = session.ID
	if err := d.saveIDs(); err != nil {
		return err
	}
	workspace := filepath.Join(d.root, session.ID)
	if err := os.Mkdir(workspace, 0755); err != nil {
		return err
	}
	fmt.Println("Session:", session.ID)
	for _, name := range []string{"invoice.py", "test_invoice.py"} {
		data, err := fixture.ReadFile("fixture/" + name)
		if err != nil {
			return err
		}
		// The application owns Files transfer; Work credentials never access Files.
		downloaded, err := d.transfer(ctx, name, data)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(workspace, name), downloaded, 0644); err != nil {
			return err
		}
	}
	original, err := fixture.ReadFile("fixture/invoice.py")
	if err != nil {
		return err
	}
	baseline, err := d.verify(ctx, "before", original)
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || !bytes.Contains(baseline, []byte("FAILED (failures=3)")) {
		return fmt.Errorf("expected three baseline test failures, got %v: %s", err, baseline)
	}
	fmt.Println("Baseline: 4 tests, 3 failures.")
	if err := d.startWorker(); err != nil {
		return err
	}
	if err := d.turn(ctx, "Run python3 -B -m unittest -v in /workspace. Repair invoice.py so all supplied tests pass. Keep test_invoice.py unchanged and explain the bugs you fixed."); err != nil {
		return err
	}
	if err := d.stopWorker(); err != nil {
		return err
	}
	fmt.Println("Worker stopped; starting a replacement for the same Session...")
	if err := d.startWorker(); err != nil {
		return err
	}
	if err := d.turn(ctx, "Continue from the existing /workspace files. Rerun python3 -B -m unittest -v against your final invoice.py, then show its content and summarize the result. Do not change the tests."); err != nil {
		return err
	}
	if err := d.stopWorker(); err != nil {
		return err
	}
	workspaceFiles, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = workspaceFiles.Close() }()
	final, err := readSource(workspaceFiles, "invoice.py")
	if err != nil {
		return err
	}
	tests, err := readSource(workspaceFiles, "test_invoice.py")
	if err != nil {
		return err
	}
	pristine, err := fixture.ReadFile("fixture/test_invoice.py")
	if err != nil {
		return err
	}
	if !bytes.Equal(tests, pristine) {
		return errors.New("agent changed the supplied tests")
	}
	verified, err := d.verify(ctx, "after", final)
	fmt.Print(string(verified))
	if err := verifySuccess(verified, err); err != nil {
		return fmt.Errorf("independent verification: %w", err)
	}
	if bytes.Equal(final, original) {
		return errors.New("agent did not change invoice.py")
	}
	downloaded, err := d.transfer(ctx, "invoice-fixed.py", final)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(d.root, "invoice-fixed.py"), downloaded, 0644); err != nil {
		return err
	}
	if err := d.saveHistory(ctx); err != nil {
		return err
	}
	fmt.Println("Verified: 4 pristine tests pass in a fresh container; File upload/download bytes match.")
	fmt.Println("Source, tests, events.json, resources.json and worker.log:", d.root)
	return nil
}

func (d *demo) transfer(ctx context.Context, name string, data []byte) ([]byte, error) {
	file, err := d.client.Files.Upload(ctx, mango.FileUploadRequest{File: mango.Upload{Filename: name, Reader: bytes.NewReader(data), ContentType: "text/x-python"}})
	if err != nil {
		return nil, err
	}
	d.files = append(d.files, file.ID)
	if err := d.saveIDs(); err != nil {
		return nil, err
	}
	body, err := d.client.Files.Download(ctx, file.ID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = body.Close() }()
	downloaded, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(data, downloaded) {
		return nil, fmt.Errorf("file %s round trip changed bytes", file.ID)
	}
	return downloaded, nil
}

// Verify only the selected source plus embedded pristine tests in a new,
// read-only container: no agent-supplied scripts or local Python execution.
func (d *demo) verify(ctx context.Context, stage string, source []byte) ([]byte, error) {
	dir := filepath.Join(d.root, stage)
	if err := os.Mkdir(dir, 0755); err != nil {
		return nil, err
	}
	tests, err := fixture.ReadFile("fixture/test_invoice.py")
	if err != nil {
		return nil, err
	}
	for name, data := range map[string][]byte{"invoice.py": source, "test_invoice.py": tests} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			return nil, err
		}
	}
	name := "mango-demo-verify-" + d.sessionID + "-" + stage
	command := exec.CommandContext(ctx, "docker", "run", "--rm", "--name", name,
		"--network=none", "--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges",
		"--memory=256m", "--pids-limit=64", "--user", d.user,
		"--mount", "type=bind,src="+dir+",dst=/workspace,readonly", "--workdir", "/workspace",
		"--entrypoint", "python3", d.image, "-B", "-m", "unittest", "-v")
	output, runErr := command.CombinedOutput()
	if ctx.Err() != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanup, "docker", "rm", "-f", name).Run()
	}
	saveErr := os.WriteFile(filepath.Join(d.root, stage+"-tests.txt"), output, 0644)
	return output, errors.Join(runErr, saveErr)
}

func (d *demo) startWorker() error {
	command := exec.Command(d.worker, "docker", "--environment-id", d.environmentID,
		"--base-url", d.baseURL, "--sandbox-base-url", d.sandboxURL,
		"--image", d.image, "--workspace-root", d.root, "--user", d.user, "--max-idle", "1m")
	// The supervisor needs the application key; no model credentials are needed.
	for _, key := range []string{"PATH", "HOME", "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH", "MANGO_API_KEY"} {
		if value, ok := os.LookupEnv(key); ok {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	command.Stdout, command.Stderr = d.log, d.log
	if err := command.Start(); err != nil {
		return err
	}
	d.supervisor, d.supervisorDone = command, make(chan error, 1)
	go func() { d.supervisorDone <- command.Wait() }()
	return nil
}

func (d *demo) stopWorker() error {
	if d.supervisor == nil {
		return nil
	}
	command, done := d.supervisor, d.supervisorDone
	d.supervisor = nil
	if err := command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	timer := time.NewTimer(150 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		_ = command.Process.Kill()
		<-done
		return errors.New("worker shutdown exceeded 150 seconds; inspect worker.log and Docker containers")
	}
}

func (d *demo) turn(ctx context.Context, prompt string) error {
	stream, err := d.client.Sessions.Events.Stream(ctx, d.sessionID, mango.StreamSessionEventsParams{})
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	sent, err := d.client.Sessions.Events.Send(ctx, d.sessionID, mango.SendSessionEventsRequest{Events: []mango.ClientSessionEventInput{mango.UserMessage(prompt)}})
	if err != nil {
		return err
	} // Never blindly resend an uncertain write.
	if len(sent.Data) != 1 || sent.Data[0].PersistedUserMessageEvent == nil {
		return errors.New("missing admitted user message")
	}
	inputID := sent.Data[0].PersistedUserMessageEvent.ID
	started := false
	seen := map[string]bool{}
	evidence := newTurnEvidence()
	consume := func(event mango.SessionEvent) (bool, error) {
		// Decode only the common envelope; event-specific payloads remain SDK types.
		encoded, err := json.Marshal(event)
		if err != nil {
			return false, err
		}
		var envelope struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(encoded, &envelope); err != nil {
			return false, err
		}
		if seen[envelope.ID] {
			return false, nil
		}
		seen[envelope.ID] = true
		if envelope.ID == inputID {
			started = true
		}
		if !started {
			return false, nil
		}
		if done, err := evidence.observe(event); done || err != nil {
			return done, err
		}
		if message := event.AgentMessageEvent; message != nil {
			for _, block := range message.Content {
				fmt.Println(block.Text)
			}
		}
		if call := event.AgentToolUseEvent; call != nil {
			fmt.Printf("[%s] %s\n", call.Name, mustJSON(call.Input))
		}

		return false, nil
	}
	for stream.Next() {
		var frame mango.EventStreamFrame
		if err := stream.Event().Decode(&frame); err != nil {
			return err
		}
		if frame.SessionEvent != nil {
			done, err := consume(*frame.SessionEvent)
			if done || err != nil {
				return err
			}
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fmt.Println("Live stream disconnected; continuing from durable history...")
	// Polling is sufficient for this small tutorial. Do not resend the message.
	// Restart the history cursor so a dropped input event cannot hide completion.
	started, seen = false, map[string]bool{}
	evidence = newTurnEvidence()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		history := d.client.Sessions.Events.ListAutoPaging(ctx, d.sessionID, mango.ListSessionEventsParams{Order: mango.Some("asc"), Limit: mango.Some(int64(100))})
		for history.Next() {
			done, err := consume(history.Value())
			if done || err != nil {
				return err
			}
		}
		if err := history.Err(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (d *demo) saveHistory(ctx context.Context) error {
	history := d.client.Sessions.Events.ListAutoPaging(ctx, d.sessionID, mango.ListSessionEventsParams{Order: mango.Some("asc"), Limit: mango.Some(int64(100))})
	var events []mango.SessionEvent
	for history.Next() {
		events = append(events, history.Value())
	}
	if err := history.Err(); err != nil {
		return err
	}
	return writeJSON(filepath.Join(d.root, "events.json"), events)
}

func (d *demo) saveIDs() error {
	return writeJSON(filepath.Join(d.root, "resources.json"), map[string]any{
		"base_url": d.baseURL, "environment_id": d.environmentID, "agent_id": d.agentID,
		"session_id": d.sessionID, "file_ids": d.files,
	})
}

func (d *demo) cleanup(failed bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var result error
	if d.sessionID != "" {
		// Save partial history before deleting an unfinished Session on failure.
		result = errors.Join(result, d.saveHistory(ctx))
		if failed {
			_, err := d.client.Sessions.Delete(ctx, d.sessionID)
			result = errors.Join(result, err)
		} else {
			_, err := d.client.Sessions.Archive(ctx, d.sessionID)
			result = errors.Join(result, err)
		}
	}
	if d.agentID != "" {
		_, err := d.client.Agents.Archive(ctx, d.agentID)
		result = errors.Join(result, err)
	}
	if d.environmentID != "" {
		_, err := d.client.Environments.Archive(ctx, d.environmentID)
		result = errors.Join(result, err)
	}
	for _, id := range d.files {
		_, err := d.client.Files.Delete(ctx, id)
		result = errors.Join(result, err)
	}
	return errors.Join(result, d.saveIDs())
}

func writeJSON(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0644)
}
func mustJSON(value any) string { data, _ := json.Marshal(value); return string(data) }
func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

// An end_turn alone cannot prove that a replacement worker executed anything.
// requires_action is normal for allowed worker-owned calls; other barriers need
// application or human attention and must not silently hang this tutorial.
type turnEvidence struct {
	allowed       map[string]string
	bashSucceeded bool
}

func newTurnEvidence() *turnEvidence { return &turnEvidence{allowed: map[string]string{}} }
func (p *turnEvidence) observe(event mango.SessionEvent) (bool, error) {
	if call := event.AgentToolUseEvent; call != nil {
		if permission, _ := call.EvaluatedPermission.Get(); permission == "allow" {
			p.allowed[call.ID] = call.Name
		}
	}
	if result := event.PersistedUserToolResultEvent; result != nil {
		failed, _ := result.IsError.Get()
		if p.allowed[result.ToolUseID] == "bash" && !failed {
			p.bashSucceeded = true
		}
	}
	if result := event.AgentToolResultEvent; result != nil {
		failed, _ := result.IsError.Get()
		if p.allowed[result.ToolUseID] == "bash" && !failed {
			p.bashSucceeded = true
		}
	}
	if idle := event.SessionStatusIdleEvent; idle != nil {
		if idle.StopReason.SessionEndTurn != nil {
			if !p.bashSucceeded {
				return false, errors.New("turn ended without a successful Bash result from the worker")
			}
			return true, nil
		}
		if action := idle.StopReason.SessionRequiresAction; action != nil && len(action.EventIDs) > 0 {
			for _, id := range action.EventIDs {
				if p.allowed[id] == "" {
					return false, fmt.Errorf("session needs an external action: %s", id)
				}
			}
			return false, nil
		}
		return false, fmt.Errorf("session needs attention: %s", mustJSON(idle.StopReason))
	}
	if event.SessionStatusTerminatedEvent != nil {
		return false, errors.New("session terminated before end_turn")
	}
	return false, nil
}

func verifySuccess(output []byte, runErr error) error {
	if runErr != nil {
		return runErr
	}
	if !bytes.Contains(output, []byte("\nRan 4 tests in ")) || !bytes.HasSuffix(bytes.TrimSpace(output), []byte("\nOK")) {
		return errors.New("verification did not report all four pristine tests passing")
	}
	return nil
}

// Read only small regular source files after the worker has stopped. In
// particular, do not follow an agent-created symlink or block on a FIFO.
func readSource(root *os.Root, name string) ([]byte, error) {
	const limit = 1 << 20
	info, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("%s must be a regular source file no larger than 1 MiB", name)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%s exceeds 1 MiB", name)
	}
	return data, nil
}
