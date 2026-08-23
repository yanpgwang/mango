package temporal_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/controlplane"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
	"github.com/yanpgwang/mango/internal/sandbox"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
	"go.temporal.io/sdk/client"
)

const (
	iterateFixtureDirectory = "testdata/coding_agent_iterate"
	iterateOutputPath       = "calc.py"
)

// TestVerticalSlice_DockerIterateFixFailingTestsEndToEnd exercises Mango's
// coding-agent iterate workflow as a Mango-owned product scenario.
// A retry-safe deterministic model drives the real PostgreSQL + Temporal +
// Docker path through two observed failures, fixes the fixture, and publishes
// the final source through Mango's Session output lifecycle. It never calls an
// external model or GitHub service.
func TestVerticalSlice_DockerIterateFixFailingTestsEndToEnd(t *testing.T) {
	requireIterateServices(t)
	provider := iterateDockerProvider(t, dockerOptional)
	runIterateFixFailingTests(t, iterateScenarioCase{
		provider:        provider,
		modelClient:     iterateProbeModel{},
		modelID:         "fake",
		timeout:         90 * time.Second,
		requireRecovery: true,
		tools:           bashOnlyToolset(t),
	})
}

// TestVerticalSlice_LiveModelIterateFixFailingTestsEndToEnd runs the same
// observable scenario with the explicitly configured Messages endpoint. It is
// opt-in because it makes a credentialed, potentially billable model call.
func TestVerticalSlice_LiveModelIterateFixFailingTestsEndToEnd(t *testing.T) {
	modelClient, modelID := liveModelForTest(t, "iterate coding-agent scenario")
	provider := iterateDockerProvider(t, dockerRequired)
	runIterateFixFailingTests(t, iterateScenarioCase{
		provider:        provider,
		modelClient:     modelClient,
		modelID:         modelID,
		timeout:         5 * time.Minute,
		requireRecovery: true,
		tools:           iterateCodingToolset(t),
	})
}

type iterateScenarioCase struct {
	provider        sandbox.Provider
	modelClient     model.Client
	modelID         string
	tools           []any
	timeout         time.Duration
	requireRecovery bool
}

func runIterateFixFailingTests(t *testing.T, tc iterateScenarioCase) {
	t.Helper()
	if tc.provider == nil || tc.modelClient == nil || tc.modelID == "" || tc.timeout <= 0 {
		t.Fatal("iterate scenario requires a provider, model, model ID, and timeout")
	}
	databaseURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	temporalAddress := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if databaseURL == "" || temporalAddress == "" {
		t.Skip("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT to run the iterate scenario")
	}

	ctx := context.Background()
	store, cleanup := integrationStore(t, databaseURL)
	defer cleanup()

	temporalClient, err := client.Dial(client.Options{HostPort: temporalAddress})
	if err != nil {
		t.Skipf("temporal unreachable at %s: %v", temporalAddress, err)
	}
	defer temporalClient.Close()

	ids := domain.NewRandomIDGen()
	clock := realClock{}
	blobs := newIntegrationBlobStore()
	fileRepository := pg.NewFileRepository(store)
	files := app.NewFileService(fileRepository, blobs, ids, clock)
	resourceService := controlplane.NewSessionResourceService(
		store, fileRepository, blobs, ids, clock, true,
	)
	resources := app.NewSessionRuntimeMaterializer(
		app.NewSessionResourceMaterializer(store, fileRepository, blobs),
		nil,
	).WithSessionOutputPublisher(
		app.NewSessionOutputPublisher(fileRepository, blobs, ids, clock),
	)

	runtime := temporalpkg.NewRuntime(temporalpkg.RuntimeConfig{
		TemporalClient:  temporalClient,
		Store:           store,
		ModelClient:     tc.modelClient,
		SandboxProvider: tc.provider,
		IDGenerator:     ids,
		RelayConfig:     temporalpkg.RelayConfig{PollInterval: 200 * time.Millisecond},
		TaskQueue:       "mango-iterate-" + ids.NewID(""),
		Resources:       resources,
	})
	if err := runtime.Worker.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer runtime.Worker.Stop()
	relayCtx, stopRelay := context.WithCancel(ctx)
	defer stopRelay()
	go func() { _ = runtime.Relay.Run(relayCtx) }()

	sessionID := "sesn_iterate_" + ids.NewID("")
	defer terminateIntegrationWorkflow(t, temporalClient, sessionID)
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := runtime.Sandbox.Release(releaseCtx, sessionID); err != nil {
			t.Errorf("release iterate sandbox: %v", err)
		}
	}()

	system := "You are a debugging agent with filesystem and shell tools. You must use the tools to work inside the Sandbox; never answer with instructions or a proposed patch. Run the failing checks, read the failures, fix the code, and repeat until every assertion passes."
	session := domain.Session{
		ID:                sessionID,
		AgentID:           "agent_iterate",
		AgentVersion:      1,
		EnvironmentID:     "env_iterate",
		EnvironmentType:   "cloud",
		EnvironmentConfig: map[string]any{"type": "cloud"},
		Status:            domain.StatusIdle,
		Metadata:          map[string]any{},
		AgentSnapshot: domain.Agent{
			ID: "agent_iterate", Version: 1, Model: domain.Model{ID: tc.modelID},
			System: &system, Tools: tc.tools,
		},
		CreatedAt: clock.Now().UTC(),
		UpdatedAt: clock.Now().UTC(),
	}
	orchestrator := runtime.Orchestrator()
	if _, _, err := orchestrator.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create iterate Session: %v", err)
	}

	for _, name := range []string{"calc.py", "test_calc.py"} {
		content := iterateFixture(t, name)
		uploaded, err := files.Upload(ctx, app.FileUploadInput{
			Filename: name, MimeType: "text/x-python", Body: strings.NewReader(content),
		})
		if err != nil {
			t.Fatalf("upload %s: %v", name, err)
		}
		mountPath := domain.SessionUploadsRoot + "/" + name
		if _, err := resourceService.Add(ctx, sessionID, app.FileSessionResourceInput{
			FileID: uploaded.ID, MountPath: &mountPath,
		}); err != nil {
			t.Fatalf("attach %s: %v", name, err)
		}
	}

	prompt := "Use your Sandbox tools now. The checks in /mnt/session/uploads/test_calc.py for /mnt/session/uploads/calc.py are failing. First copy both files into /workspace/iterate and run the assertions so you observe the existing failure. Then inspect and edit the writable calc.py, re-running the assertions until add, divide, and mean all behave as test_calc.py requires. Pytest is not installed, so run equivalent assertions directly with python3. Do not install packages or access the network. Write the verified final calc.py to /mnt/session/outputs/calc.py. Do not reply until the file exists and every assertion has passed."
	if _, err := orchestrator.Admit(ctx, sessionID, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{map[string]any{
			"type": "text", "text": prompt,
		}}},
	}}); err != nil {
		t.Fatalf("admit iterate task: %v", err)
	}

	events := waitForIterateCompletion(t, store, sessionID, tc.timeout)
	if tc.requireRecovery && !hasErroredToolResult(events) {
		t.Fatalf("iterate scenario never observed a failing check; events=%s", typeList(events))
	}
	if len(eventsOfType(events, domain.EvAgentToolUse)) == 0 {
		t.Fatalf(
			"iterate scenario completed without a tool call; agent_messages=%q events=%s",
			iterateAgentMessages(events),
			typeList(events),
		)
	}

	box, found, err := runtime.Sandbox.AcquireExisting(ctx, sessionID, sandbox.Spec{})
	if err != nil || !found {
		t.Fatalf("attach completed iterate sandbox: found=%v err=%v", found, err)
	}
	verification, err := box.Exec(ctx, sandbox.Command{
		Path: "/bin/sh",
		Args: []string{"-c", iterateVerificationCommand("/mnt/session/outputs/calc.py")},
	})
	if err != nil {
		t.Fatalf("independently verify iterate output: %v", err)
	}
	if verification.ExitCode != 0 {
		t.Fatalf("iterate output verification failed: stdout=%s stderr=%s", verification.Stdout, verification.Stderr)
	}

	page, err := files.List(ctx, app.FileListQuery{ScopeID: sessionID, Limit: 100})
	if err != nil {
		t.Fatalf("list iterate Session outputs: %v", err)
	}
	var output *domain.File
	for index := range page.Files {
		if page.Files[index].OutputPath == iterateOutputPath {
			output = &page.Files[index]
			break
		}
	}
	if output == nil {
		t.Fatalf("published outputs = %+v, want %s", page.Files, iterateOutputPath)
	}
	download, err := files.Download(ctx, output.ID)
	if err != nil {
		t.Fatalf("download iterate output: %v", err)
	}
	defer download.Body.Close() //nolint:errcheck // test reports the content read
	published, err := io.ReadAll(download.Body)
	if err != nil {
		t.Fatalf("read iterate output: %v", err)
	}
	runtimeOutput, err := box.ReadFile(ctx, "/mnt/session/outputs/calc.py")
	if err != nil {
		t.Fatalf("read runtime iterate output: %v", err)
	}
	if string(published) != string(runtimeOutput) {
		t.Fatal("published calc.py differs from the verified runtime output")
	}

	final, err := store.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatalf("get iterate Session: %v", err)
	}
	if final.Status != domain.StatusIdle {
		t.Fatalf("iterate Session status = %s, want idle", final.Status)
	}
}

func requireIterateServices(t *testing.T) {
	t.Helper()
	if os.Getenv("MANGO_TEST_DATABASE_URL") == "" ||
		os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT") == "" {
		t.Skip("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT to run the iterate scenario")
	}
}

func waitForIterateCompletion(
	t *testing.T,
	store *pg.Store,
	sessionID string,
	timeout time.Duration,
) []domain.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events, err := store.EventsAfter(context.Background(), sessionID, 0, 500)
		if err != nil {
			t.Fatalf("list iterate events: %v", err)
		}
		if failure, ok := firstFailureEvent(events); ok {
			t.Fatalf("iterate workflow failed with %s: %#v; events=%s", failure.Type, failure.Payload, typeList(events))
		}
		if hasType(events, domain.EvAgentMessage) && hasType(events, domain.EvSessionStatusIdle) {
			return events
		}
		time.Sleep(250 * time.Millisecond)
	}
	events, _ := store.EventsAfter(context.Background(), sessionID, 0, 500)
	t.Fatalf("timed out after %s waiting for iterate completion; events=%s", timeout, typeList(events))
	return nil
}

func hasErroredToolResult(events []domain.Event) bool {
	for _, event := range eventsOfType(events, domain.EvAgentToolResult) {
		_, isError, ok := eventText(event)
		if ok && isError {
			return true
		}
	}
	return false
}

func iterateAgentMessages(events []domain.Event) string {
	var messages []string
	for _, event := range eventsOfType(events, domain.EvAgentMessage) {
		text, _, ok := eventText(event)
		if ok && strings.TrimSpace(text) != "" {
			messages = append(messages, strings.TrimSpace(text))
		}
	}
	return strings.Join(messages, " | ")
}

func iterateFixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(iterateFixtureDirectory, name))
	if err != nil {
		t.Fatalf("read iterate fixture %s: %v", name, err)
	}
	return string(data)
}

func iterateDockerProvider(t *testing.T, requirement dockerRequirement) sandbox.Provider {
	t.Helper()
	provider, err := sandbox.NewDockerProvider(sandbox.DockerConfig{
		DefaultImage: "python:3.12-alpine",
	})
	if err != nil {
		if requirement == dockerRequired {
			t.Fatalf("Docker Engine is required for the live iterate scenario: %v", err)
		}
		t.Skipf("Docker Engine unreachable: %v", err)
	}
	return provider
}

func iterateCodingToolset(t *testing.T) []any {
	t.Helper()
	enabledNames := map[string]bool{
		"bash": true, "read": true, "write": true,
		"edit": true, "glob": true, "grep": true,
	}
	configs := make([]any, 0, len(enabledNames))
	for _, name := range domain.BuiltinToolNames {
		if !enabledNames[name] {
			continue
		}
		configs = append(configs, map[string]any{
			"name": name, "enabled": true,
			"permission_policy": map[string]any{"type": "always_allow"},
		})
	}
	raw := []any{map[string]any{
		"type": domain.BuiltinToolsetType,
		"default_config": map[string]any{
			"enabled":           false,
			"permission_policy": map[string]any{"type": "always_allow"},
		},
		"configs": configs,
	}}
	parsed, err := domain.ParseTools(raw)
	if err != nil {
		t.Fatalf("parse iterate coding toolset: %v", err)
	}
	for _, name := range domain.BuiltinToolNames {
		enabled, policy := parsed.BuiltinEnabled(name)
		if enabled != enabledNames[name] {
			t.Fatalf("iterate coding tool %q enabled = %v, want %v", name, enabled, enabledNames[name])
		}
		if enabled && policy.Type != "always_allow" {
			t.Fatalf("iterate coding tool %q policy = %q, want always_allow", name, policy.Type)
		}
	}
	return raw
}

// iterateProbeModel derives its next action entirely from committed tool
// results, making every provider Activity retry return the same response.
type iterateProbeModel struct{}

func (iterateProbeModel) CreateMessage(
	_ context.Context,
	request model.Request,
) (model.Response, error) {
	results := make([]domain.ContentBlock, 0, 3)
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_result" {
				results = append(results, block)
			}
		}
	}
	if len(results) > 3 {
		return model.Response{}, fmt.Errorf("iterate probe received %d tool results", len(results))
	}
	if len(results) >= 1 && (!results[0].IsError || !strings.Contains(results[0].Text, "AssertionError")) {
		return model.Response{}, errors.New("iterate probe did not observe the planted add failure")
	}
	if len(results) >= 2 && (!results[1].IsError || !strings.Contains(results[1].Text, "ZeroDivisionError")) {
		return model.Response{}, errors.New("iterate probe did not observe the planted divide failure")
	}
	if len(results) == 3 {
		if results[2].IsError || !strings.Contains(results[2].Text, "all assertions passed") {
			return model.Response{}, errors.New("iterate probe did not observe the repaired test suite")
		}
		return model.Response{
			Content:    []domain.ContentBlock{{Type: "text", Text: "All assertions pass and calc.py was published."}},
			StopReason: "end_turn",
		}, nil
	}

	commands := []string{
		"set -eu; mkdir -p /workspace/iterate; cp /mnt/session/uploads/calc.py /mnt/session/uploads/test_calc.py /workspace/iterate/; " + iterateVerificationCommand("/workspace/iterate/calc.py"),
		`set -eu; cd /workspace/iterate; python3 - <<'PY'
from pathlib import Path
p = Path("calc.py")
p.write_text(p.read_text().replace("    return a + b + 1  # BUG: off by one", "    return a + b"))
PY
` + iterateVerificationCommand("/workspace/iterate/calc.py"),
		`set -eu; cd /workspace/iterate; python3 - <<'PY'
from pathlib import Path
p = Path("calc.py")
p.write_text(p.read_text().replace(
    "def divide(a, b):\n    return a / b  # BUG: no zero check",
    "def divide(a, b):\n    if b == 0:\n        raise ValueError(\"cannot divide by zero\")\n    return a / b",
))
PY
` + iterateVerificationCommand("/workspace/iterate/calc.py") + `
mkdir -p /mnt/session/outputs
cp /workspace/iterate/calc.py /mnt/session/outputs/calc.py`,
	}
	return model.Response{
		Content: []domain.ContentBlock{{
			Type: "tool_use", ToolUseID: fmt.Sprintf("iterate_probe_%d", len(results)+1),
			ToolName: bashToolName, Input: map[string]any{"command": commands[len(results)]},
		}},
		StopReason: "tool_use",
	}, nil
}

func (m iterateProbeModel) CreateMessageStream(
	ctx context.Context,
	request model.Request,
	onDelta func(index int, text string),
) (model.Response, error) {
	response, err := m.CreateMessage(ctx, request)
	if err == nil && response.StopReason == "end_turn" && len(response.Content) == 1 && onDelta != nil {
		onDelta(0, response.Content[0].Text)
	}
	return response, err
}

func iterateVerificationCommand(modulePath string) string {
	return fmt.Sprintf(`python3 -B - <<'PY'
import importlib.util

spec = importlib.util.spec_from_file_location("calc", %q)
calc = importlib.util.module_from_spec(spec)
spec.loader.exec_module(calc)

assert calc.add(2, 3) == 5
assert calc.divide(10, 2) == 5
try:
    calc.divide(10, 0)
except ValueError:
    pass
else:
    raise AssertionError("divide(10, 0) must raise ValueError")
assert calc.mean([2, 4, 6]) == 4
print("all assertions passed")
PY`, modulePath)
}
