package temporal_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/controlplane"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/live"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
	"github.com/yanpgwang/mango/internal/sandbox/sandboxtest"
	"github.com/yanpgwang/mango/internal/selfhosted"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
	mango "github.com/yanpgwang/mango/sdk/go"
	temporalclient "go.temporal.io/sdk/client"
)

// Real Mango HTTP, authentication, PostgreSQL, Temporal, NATS, Environment
// Work, and Docker worker containers. Only model inference is a deterministic
// fixture: sandbox execution never leaves the operator-owned Docker Engine.
func TestVerticalSlice_SelfHostedDockerSurvivesRuntimeRestartAndReactivation(t *testing.T) {
	fixture := newDockerSystemFixture(
		t, dockerSystemProbe{}, "docker-system-probe", "always_ask",
	)
	fixture.send(map[string]any{
		"type": "user.message", "content": []any{map[string]any{
			"type": "text", "text": "write the durable marker",
		}},
	})
	firstLaunch := fixture.launch(30 * time.Second)
	firstActionID := waitForDockerSystemApproval(t, fixture.store, fixture.sessionID, 1, 20*time.Second)
	waitForDockerSystemWorkState(
		t, fixture.store, fixture.environmentID, 1, domain.EnvironmentWorkActive, 20*time.Second,
	)
	fixture.restartRuntime()

	fixture.send(map[string]any{
		"type": "user.tool_confirmation", "tool_use_id": firstActionID, "result": "allow",
	})
	waitForDockerSystemCompletion(t, fixture.store, fixture.sessionID, 1, 20*time.Second)
	require.NoError(t, waitDockerSystemLauncher(firstLaunch, 30*time.Second))
	waitForDockerSystemWorkState(
		t, fixture.store, fixture.environmentID, 1, domain.EnvironmentWorkStopped, 20*time.Second,
	)

	fixture.send(map[string]any{
		"type": "user.message", "content": []any{map[string]any{
			"type": "text", "text": "read the durable marker",
		}},
	})
	secondLaunch := fixture.launch(30 * time.Second)
	secondActionID := waitForDockerSystemApproval(t, fixture.store, fixture.sessionID, 2, 20*time.Second)
	fixture.send(map[string]any{
		"type": "user.tool_confirmation", "tool_use_id": secondActionID, "result": "allow",
	})
	events := waitForDockerSystemCompletion(t, fixture.store, fixture.sessionID, 2, 20*time.Second)
	require.NoError(t, waitDockerSystemLauncher(secondLaunch, 30*time.Second))
	waitForDockerSystemWorkState(
		t, fixture.store, fixture.environmentID, 2, domain.EnvironmentWorkStopped, 20*time.Second,
	)

	encoded, err := json.Marshal(events)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "durable-docker-workspace")
	require.NotContains(t, string(encoded), domain.EvSessionError)
	pending, err := fixture.store.UnresolvedPendingActions(context.Background(), fixture.sessionID)
	require.NoError(t, err)
	require.Empty(t, pending)
}

// A deliberately small, opt-in live smoke. It proves that the configured real
// Messages endpoint can select a tool and complete one turn through Mango's
// self-hosted Work boundary; default tests and CI never enable it.
func TestVerticalSlice_LiveModelSelfHostedDockerEndToEnd(t *testing.T) {
	modelClient, modelID := liveModelForTest(t, "self-hosted Docker smoke test")
	fixture := newDockerSystemFixture(t, modelClient, modelID, "always_allow")
	const marker = "mango-self-hosted-live-ok"
	fixture.send(map[string]any{
		"type": "user.message", "content": []any{map[string]any{
			"type": "text",
			"text": "Use the bash tool exactly once. Pass the text between <command> tags as the command without changes; do not include the tags. <command>printf '" + marker + "' > live-marker.txt && cat live-marker.txt</command> After receiving the tool result, reply with a short confirmation and do not call another tool.",
		}},
	})
	done := fixture.launch(3 * time.Minute)
	events := waitForDockerSystemCompletion(t, fixture.store, fixture.sessionID, 1, 2*time.Minute)
	require.NoError(t, waitDockerSystemLauncher(done, 3*time.Minute))
	waitForDockerSystemWorkState(
		t, fixture.store, fixture.environmentID, 1, domain.EnvironmentWorkStopped, 20*time.Second,
	)
	toolUses := eventsOfType(events, domain.EvAgentToolUse)
	require.Len(t, toolUses, 1)
	toolResults := eventsOfType(events, domain.EvUserToolResult)
	require.Len(t, toolResults, 1, "self-hosted workers resolve calls with user.tool_result")
	require.Equal(t, toolUses[0].ID, toolResults[0].Payload["tool_use_id"])
	text, isError, ok := eventText(toolResults[0])
	require.True(t, ok)
	require.False(t, isError)
	require.Contains(t, text, marker)
}

type dockerSystemFixture struct {
	t                *testing.T
	store            *pg.Store
	temporalClient   temporalclient.Client
	runtimeConfig    temporalpkg.RuntimeConfig
	stopRuntime      func()
	hostBaseURL      string
	containerBaseURL string
	workspaceKey     string
	environmentID    string
	sessionID        string
	workerImage      string
	engine           *client.Client
	supervisor       *mango.Client
	launcherParent   context.Context
	cancelLaunchers  context.CancelFunc
	launcherWG       sync.WaitGroup
}

// DockerLauncher cancellation may spend 15 seconds stopping a container and
// another 15 seconds removing it. Keep the fixture alive until both operations
// have had time to finish so later LIFO cleanups cannot close the Engine early.
const dockerSystemLauncherCleanupTimeout = 40 * time.Second

func newDockerSystemFixture(
	t *testing.T,
	modelClient model.Client,
	modelID string,
	bashPermission string,
) *dockerSystemFixture {
	t.Helper()
	if os.Getenv("MANGO_TEST_DOCKER") != "1" {
		t.Skip("set MANGO_TEST_DOCKER=1 to require the Docker system E2E")
	}
	databaseURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	temporalAddress := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	natsURL := os.Getenv("MANGO_TEST_NATS_URL")
	workerImage := os.Getenv("MANGO_TEST_WORKER_IMAGE")
	if databaseURL == "" || temporalAddress == "" || natsURL == "" || workerImage == "" {
		t.Skip("set PostgreSQL, Temporal, NATS, and MANGO_TEST_WORKER_IMAGE integration variables")
	}
	require.NotNil(t, modelClient)
	require.NotEmpty(t, modelID)
	require.Contains(t, []string{"always_allow", "always_ask"}, bashPermission)

	fixture := &dockerSystemFixture{
		t: t, workerImage: workerImage,
		workspaceKey: "sk-mango-system-e2e-workspace-key",
	}
	ctx := context.Background()
	var storeCleanup func()
	fixture.store, storeCleanup = integrationStore(t, databaseURL)
	t.Cleanup(storeCleanup)
	require.NoError(t, fixture.store.BootstrapAPIKey(ctx, fixture.workspaceKey))

	broker, err := live.Connect(natsURL)
	require.NoError(t, err)
	t.Cleanup(broker.Close)
	fixture.store.SetEventNotifier(broker)
	fixture.temporalClient, err = temporalclient.Dial(temporalclient.Options{HostPort: temporalAddress})
	require.NoError(t, err)
	t.Cleanup(fixture.temporalClient.Close)

	ids := domain.NewRandomIDGen()
	fixture.runtimeConfig = temporalpkg.RuntimeConfig{
		TemporalClient: fixture.temporalClient, Store: fixture.store, ModelClient: modelClient,
		SandboxProvider: sandboxtest.NoProvision(t), IDGenerator: ids,
		TaskQueue:        "self-hosted-docker-system-" + ids.NewID(""),
		RelayConfig:      temporalpkg.RelayConfig{PollInterval: 20 * time.Millisecond},
		PreviewPublisher: broker,
	}
	runtime := temporalpkg.NewRuntime(fixture.runtimeConfig)
	fixture.stopRuntime = startGateRuntime(t, ctx, runtime)
	t.Cleanup(func() { fixture.stopRuntime() })

	agents := pg.NewAgentRepository(fixture.store)
	environments := pg.NewEnvironmentRepository(fixture.store)
	sessions := controlplane.NewSessionService(
		fixture.store, agents, environments, runtime.Orchestrator(), ids, realClock{}, nil,
	)
	handler := httpapi.NewServer(httpapi.Deps{
		Agents:   app.NewAgentService(agents, ids, realClock{}),
		Envs:     app.NewEnvironmentService(environments, ids, realClock{}),
		Sessions: sessions,
		Events:   controlplane.NewEventService(fixture.store),
		Stream: live.NewStream(
			fixture.store, broker, ids, realClock{}, 20*time.Millisecond,
		),
		EnvironmentWork: app.NewEnvironmentWorkService(
			pg.NewEnvironmentWorkRepository(fixture.store), environments,
		),
	}, httpapi.Config{RequireAuth: true, Authenticator: fixture.store}).Handler()
	server := httptest.NewUnstartedServer(handler)
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	require.NoError(t, err)
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	port := listener.Addr().(*net.TCPAddr).Port
	fixture.hostBaseURL = "http://127.0.0.1:" + strconv.Itoa(port)
	fixture.containerBaseURL = "http://host.docker.internal:" + strconv.Itoa(port)

	agentID := createAuthenticatedResource(
		t, fixture.hostBaseURL, fixture.workspaceKey, "/v1/agents", map[string]any{
			"name": "docker-system-e2e", "model": modelID,
			"tools": []any{map[string]any{
				"type": domain.BuiltinToolsetType,
				"default_config": map[string]any{
					"enabled": false, "permission_policy": map[string]any{"type": "always_allow"},
				},
				"configs": []any{map[string]any{
					"name": "bash", "enabled": true,
					"permission_policy": map[string]any{"type": bashPermission},
				}},
			}},
		},
	)
	fixture.environmentID = createAuthenticatedResource(
		t, fixture.hostBaseURL, fixture.workspaceKey, "/v1/environments",
		map[string]any{"name": "operator-docker", "config": map[string]any{"type": "self_hosted"}},
	)
	fixture.sessionID = createAuthenticatedResource(
		t, fixture.hostBaseURL, fixture.workspaceKey, "/v1/sessions",
		map[string]any{"agent": agentID, "environment_id": fixture.environmentID},
	)
	t.Cleanup(func() {
		terminateIntegrationWorkflow(t, fixture.temporalClient, fixture.sessionID)
	})

	fixture.engine, err = client.New(client.FromEnv)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, fixture.engine.Close()) })
	probeCtx, cancelProbe := context.WithTimeout(ctx, 10*time.Second)
	_, err = fixture.engine.Ping(probeCtx, client.PingOptions{NegotiateAPIVersion: true})
	cancelProbe()
	require.NoError(t, err)
	t.Cleanup(func() { removeDockerSystemVolume(t, fixture.engine, fixture.sessionID) })
	fixture.launcherParent, fixture.cancelLaunchers = context.WithCancel(ctx)
	t.Cleanup(fixture.stopLaunchers)
	fixture.supervisor, err = mango.New(mango.Config{
		BaseURL: fixture.hostBaseURL, APIKey: fixture.workspaceKey,
	})
	require.NoError(t, err)
	return fixture
}

func (f *dockerSystemFixture) send(event map[string]any) {
	f.t.Helper()
	sendAuthenticatedEvent(f.t, f.hostBaseURL, f.workspaceKey, f.sessionID, event)
}

func (f *dockerSystemFixture) launch(timeout time.Duration) <-chan error {
	f.t.Helper()
	launcher, err := selfhosted.NewDockerLauncher(f.engine, selfhosted.DockerLauncherOptions{
		Client: f.supervisor, EnvironmentID: f.environmentID, Image: f.workerImage, Drain: true,
		SandboxBaseURL: f.containerBaseURL, MaxIdle: 750 * time.Millisecond,
	})
	require.NoError(f.t, err)
	done := make(chan error, 1)
	f.launcherWG.Add(1)
	go func() {
		defer f.launcherWG.Done()
		launchCtx, cancel := context.WithTimeout(f.launcherParent, timeout)
		defer cancel()
		done <- launcher.Run(launchCtx)
	}()
	return done
}

func (f *dockerSystemFixture) restartRuntime() {
	f.t.Helper()
	f.stopRuntime()
	runtime := temporalpkg.NewRuntime(f.runtimeConfig)
	f.stopRuntime = startGateRuntime(f.t, context.Background(), runtime)
}

func (f *dockerSystemFixture) stopLaunchers() {
	f.cancelLaunchers()
	done := make(chan struct{})
	go func() {
		f.launcherWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(dockerSystemLauncherCleanupTimeout):
		f.t.Error("self-hosted Docker launchers did not stop during cleanup")
	}
}

type dockerSystemProbe struct{}

func (dockerSystemProbe) CreateMessage(_ context.Context, request model.Request) (model.Response, error) {
	instruction := latestDockerSystemInstruction(request.Messages)
	toolID := "provider_write_marker"
	command := "printf durable-docker-workspace > proof.txt && cat proof.txt"
	if strings.Contains(instruction, "read the durable marker") {
		toolID = "provider_read_marker"
		command = "cat proof.txt"
	}
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type != "tool_result" || block.ToolResultFor != toolID {
				continue
			}
			if block.IsError || !strings.Contains(block.Text, "durable-docker-workspace") {
				return model.Response{}, fmt.Errorf("Docker tool result for %s = %#v", toolID, block)
			}
			return model.Response{
				Content:    []domain.ContentBlock{{Type: "text", Text: "completed " + toolID}},
				StopReason: "end_turn",
			}, nil
		}
	}
	if instruction == "" {
		return model.Response{}, fmt.Errorf("model request contains no user instruction")
	}
	return model.Response{
		Content: []domain.ContentBlock{{
			Type: "tool_use", ToolUseID: toolID, ToolName: "bash",
			Input: map[string]any{"command": command},
		}},
		StopReason: "tool_use",
	}, nil
}

func (p dockerSystemProbe) CreateMessageStream(
	ctx context.Context,
	request model.Request,
	onDelta func(int, string),
) (model.Response, error) {
	response, err := p.CreateMessage(ctx, request)
	if err == nil && response.StopReason == "end_turn" && onDelta != nil {
		onDelta(0, response.Content[0].Text)
	}
	return response, err
}

func latestDockerSystemInstruction(messages []domain.Message) string {
	var instruction string
	for _, message := range messages {
		for _, block := range message.Content {
			if message.Role == domain.RoleUser && block.Type == "text" {
				instruction = block.Text
			}
		}
	}
	return instruction
}

func createAuthenticatedResource(
	t *testing.T,
	baseURL, apiKey, path string,
	body map[string]any,
) string {
	t.Helper()
	response := authenticatedJSONRequest(t, baseURL, apiKey, http.MethodPost, path, body)
	defer func() { require.NoError(t, response.Body.Close()) }()
	var object map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&object))
	require.Equal(t, http.StatusOK, response.StatusCode, "%v", object)
	id, _ := object["id"].(string)
	require.NotEmpty(t, id, "%v", object)
	return id
}

func sendAuthenticatedEvent(
	t *testing.T,
	baseURL, apiKey, sessionID string,
	event map[string]any,
) {
	t.Helper()
	response := authenticatedJSONRequest(t, baseURL, apiKey, http.MethodPost,
		"/v1/sessions/"+sessionID+"/events", map[string]any{"events": []any{event}})
	defer func() { require.NoError(t, response.Body.Close()) }()
	var object map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&object))
	require.Equal(t, http.StatusOK, response.StatusCode, "%v", object)
}

func authenticatedJSONRequest(
	t *testing.T,
	baseURL, apiKey, method, path string,
	body any,
) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	request, err := http.NewRequestWithContext(
		context.Background(), method, baseURL+path, bytes.NewReader(encoded),
	)
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer "+apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	require.NoError(t, err)
	return response
}

func waitForDockerSystemApproval(
	t *testing.T,
	store *pg.Store,
	sessionID string,
	want int,
	timeout time.Duration,
) string {
	t.Helper()
	var actionID string
	var sessionFailure *domain.Event
	require.Eventually(t, func() bool {
		events, err := store.EventsAfter(context.Background(), sessionID, 0, 200)
		if err != nil {
			return false
		}
		count := 0
		for _, event := range events {
			if event.Type == domain.EvSessionError {
				failure := event
				sessionFailure = &failure
				return true
			}
			if event.Type == domain.EvAgentToolUse && event.Payload["evaluated_permission"] == "ask" {
				count++
				actionID = event.ID
			}
		}
		return count >= want && latestIdleReason(events) == "requires_action"
	}, timeout, 50*time.Millisecond)
	require.Nil(t, sessionFailure, "self-hosted Docker Session failed: %#v", sessionFailure)
	require.NotEmpty(t, actionID)
	return actionID
}

func waitForDockerSystemCompletion(
	t *testing.T,
	store *pg.Store,
	sessionID string,
	wantMessages int,
	timeout time.Duration,
) []domain.Event {
	t.Helper()
	var result []domain.Event
	var sessionFailure *domain.Event
	require.Eventually(t, func() bool {
		events, err := store.EventsAfter(context.Background(), sessionID, 0, 300)
		if err != nil {
			return false
		}
		messages := 0
		for _, event := range events {
			if event.Type == domain.EvSessionError {
				failure := event
				sessionFailure = &failure
				return true
			}
			if event.Type == domain.EvAgentMessage {
				messages++
			}
		}
		result = events
		return messages >= wantMessages && latestIdleReason(events) == "end_turn"
	}, timeout, 50*time.Millisecond)
	require.Nil(t, sessionFailure, "self-hosted Docker Session failed: %#v", sessionFailure)
	return result
}

func waitForDockerSystemWorkState(
	t *testing.T,
	store *pg.Store,
	environmentID string,
	wantItems int,
	wantState domain.EnvironmentWorkState,
	timeout time.Duration,
) {
	t.Helper()
	repository := pg.NewEnvironmentWorkRepository(store)
	require.Eventually(t, func() bool {
		page, err := repository.ListWork(
			context.Background(), environmentID, app.EnvironmentWorkListQuery{Limit: 10},
		)
		if err != nil || len(page.Work) != wantItems {
			return false
		}
		for _, item := range page.Work {
			if item.State != wantState {
				return false
			}
		}
		return true
	}, timeout, 50*time.Millisecond)
}

func waitDockerSystemLauncher(done <-chan error, timeout time.Duration) error {
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		return fmt.Errorf("Docker launcher did not stop after draining")
	}
}

func removeDockerSystemVolume(t *testing.T, engine *client.Client, sessionID string) {
	t.Helper()
	sum := sha256.Sum256([]byte(sessionID))
	volume := "mango-workspace-" + hex.EncodeToString(sum[:12])
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := engine.VolumeRemove(cleanupCtx, volume, client.VolumeRemoveOptions{Force: true})
	if err != nil && !errdefs.IsNotFound(err) {
		t.Errorf("remove Docker system-test volume %s: %v", volume, err)
	}
}
