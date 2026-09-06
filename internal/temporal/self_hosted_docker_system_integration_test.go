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
	"testing"
	"time"

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

	ctx := context.Background()
	const workspaceKey = "sk-mango-system-e2e-workspace-key"
	store, cleanupStore := integrationStore(t, databaseURL)
	defer cleanupStore()
	require.NoError(t, store.BootstrapAPIKey(ctx, workspaceKey))

	broker, err := live.Connect(natsURL)
	require.NoError(t, err)
	defer broker.Close()
	store.SetEventNotifier(broker)

	tc, err := temporalclient.Dial(temporalclient.Options{HostPort: temporalAddress})
	require.NoError(t, err)
	defer tc.Close()
	ids := domain.NewRandomIDGen()
	probe := dockerSystemProbe{}
	runtimeConfig := temporalpkg.RuntimeConfig{
		TemporalClient:   tc,
		Store:            store,
		ModelClient:      probe,
		SandboxProvider:  sandboxtest.NoProvision(t),
		IDGenerator:      ids,
		TaskQueue:        "self-hosted-docker-system-" + ids.NewID(""),
		RelayConfig:      temporalpkg.RelayConfig{PollInterval: 20 * time.Millisecond},
		PreviewPublisher: broker,
	}
	runtimeOne := temporalpkg.NewRuntime(runtimeConfig)
	stopRuntimeOne := startGateRuntime(t, ctx, runtimeOne)
	runtimeOneStopped := false
	defer func() {
		if !runtimeOneStopped {
			stopRuntimeOne()
		}
	}()

	agents := pg.NewAgentRepository(store)
	environments := pg.NewEnvironmentRepository(store)
	sessions := controlplane.NewSessionService(
		store, agents, environments, runtimeOne.Orchestrator(), ids, realClock{}, nil,
	)
	work := app.NewEnvironmentWorkService(pg.NewEnvironmentWorkRepository(store), environments)
	handler := httpapi.NewServer(httpapi.Deps{
		Agents:   app.NewAgentService(agents, ids, realClock{}),
		Envs:     app.NewEnvironmentService(environments, ids, realClock{}),
		Sessions: sessions,
		Events:   controlplane.NewEventService(store),
		Stream: live.NewStream(
			store, broker, ids, realClock{}, 20*time.Millisecond,
		),
		EnvironmentWork: work,
	}, httpapi.Config{RequireAuth: true, Authenticator: store}).Handler()

	server := httptest.NewUnstartedServer(handler)
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	require.NoError(t, err)
	server.Listener = listener
	server.Start()
	defer server.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	hostBaseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	containerBaseURL := "http://host.docker.internal:" + strconv.Itoa(port)

	agentID := createAuthenticatedResource(t, hostBaseURL, workspaceKey, "/v1/agents", map[string]any{
		"name":  "docker-system-e2e",
		"model": "docker-system-probe",
		"tools": []any{map[string]any{
			"type": domain.BuiltinToolsetType,
			"default_config": map[string]any{
				"enabled":           false,
				"permission_policy": map[string]any{"type": "always_allow"},
			},
			"configs": []any{map[string]any{
				"name":              "bash",
				"enabled":           true,
				"permission_policy": map[string]any{"type": "always_ask"},
			}},
		}},
	})
	environmentID := createAuthenticatedResource(t, hostBaseURL, workspaceKey, "/v1/environments", map[string]any{
		"name": "operator-docker", "config": map[string]any{"type": "self_hosted"},
	})
	sessionID := createAuthenticatedResource(t, hostBaseURL, workspaceKey, "/v1/sessions", map[string]any{
		"agent": agentID, "environment_id": environmentID,
	})
	defer terminateIntegrationWorkflow(t, tc, sessionID)

	engine, err := client.New(client.FromEnv)
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	probeCtx, cancelProbe := context.WithTimeout(ctx, 10*time.Second)
	_, err = engine.Ping(probeCtx, client.PingOptions{NegotiateAPIVersion: true})
	cancelProbe()
	require.NoError(t, err)
	defer removeDockerSystemVolume(t, engine, sessionID)
	launcherParent, cancelLaunchers := context.WithCancel(ctx)
	defer cancelLaunchers()

	supervisor, err := mango.New(mango.Config{BaseURL: hostBaseURL, APIKey: workspaceKey})
	require.NoError(t, err)
	launch := func() <-chan error {
		t.Helper()
		launcher, err := selfhosted.NewDockerLauncher(engine, selfhosted.DockerLauncherOptions{
			Client: supervisor, EnvironmentID: environmentID, Image: workerImage, Drain: true,
			SandboxBaseURL: containerBaseURL, MaxIdle: 750 * time.Millisecond,
		})
		require.NoError(t, err)
		done := make(chan error, 1)
		go func() {
			launchCtx, cancel := context.WithTimeout(launcherParent, 30*time.Second)
			defer cancel()
			done <- launcher.Run(launchCtx)
		}()
		return done
	}

	sendAuthenticatedEvent(t, hostBaseURL, workspaceKey, sessionID, map[string]any{
		"type": "user.message", "content": []any{map[string]any{
			"type": "text", "text": "write the durable marker",
		}},
	})
	firstLaunch := launch()
	firstActionID := waitForDockerSystemApproval(t, store, sessionID, 1)
	waitForDockerSystemWorkState(t, store, environmentID, 1, domain.EnvironmentWorkActive)

	stopRuntimeOne()
	runtimeOneStopped = true
	runtimeTwo := temporalpkg.NewRuntime(runtimeConfig)
	stopRuntimeTwo := startGateRuntime(t, ctx, runtimeTwo)
	defer stopRuntimeTwo()

	sendAuthenticatedEvent(t, hostBaseURL, workspaceKey, sessionID, map[string]any{
		"type": "user.tool_confirmation", "tool_use_id": firstActionID, "result": "allow",
	})
	waitForDockerSystemCompletion(t, store, sessionID, 1)
	require.NoError(t, waitDockerSystemLauncher(t, firstLaunch))
	waitForDockerSystemWorkState(t, store, environmentID, 1, domain.EnvironmentWorkStopped)

	sendAuthenticatedEvent(t, hostBaseURL, workspaceKey, sessionID, map[string]any{
		"type": "user.message", "content": []any{map[string]any{
			"type": "text", "text": "read the durable marker",
		}},
	})
	secondLaunch := launch()
	secondActionID := waitForDockerSystemApproval(t, store, sessionID, 2)
	sendAuthenticatedEvent(t, hostBaseURL, workspaceKey, sessionID, map[string]any{
		"type": "user.tool_confirmation", "tool_use_id": secondActionID, "result": "allow",
	})
	events := waitForDockerSystemCompletion(t, store, sessionID, 2)
	require.NoError(t, waitDockerSystemLauncher(t, secondLaunch))
	waitForDockerSystemWorkState(t, store, environmentID, 2, domain.EnvironmentWorkStopped)

	encoded, err := json.Marshal(events)
	require.NoError(t, err)
	require.Contains(t, string(encoded), "durable-docker-workspace")
	require.NotContains(t, string(encoded), domain.EvSessionError)
	pending, err := store.UnresolvedPendingActions(ctx, sessionID)
	require.NoError(t, err)
	require.Empty(t, pending)
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
	}, 20*time.Second, 50*time.Millisecond)
	require.Nil(t, sessionFailure, "self-hosted Docker Session failed: %#v", sessionFailure)
	require.NotEmpty(t, actionID)
	return actionID
}

func waitForDockerSystemCompletion(
	t *testing.T,
	store *pg.Store,
	sessionID string,
	wantMessages int,
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
	}, 20*time.Second, 50*time.Millisecond)
	require.Nil(t, sessionFailure, "self-hosted Docker Session failed: %#v", sessionFailure)
	return result
}

func waitForDockerSystemWorkState(
	t *testing.T,
	store *pg.Store,
	environmentID string,
	wantItems int,
	wantState domain.EnvironmentWorkState,
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
	}, 20*time.Second, 50*time.Millisecond)
}

func waitDockerSystemLauncher(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		return fmt.Errorf("Docker launcher did not stop after draining")
	}
}

func removeDockerSystemVolume(t *testing.T, engine *client.Client, sessionID string) {
	t.Helper()
	sum := sha256.Sum256([]byte(sessionID))
	volume := "mango-workspace-" + hex.EncodeToString(sum[:12])
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = engine.VolumeRemove(cleanupCtx, volume, client.VolumeRemoveOptions{Force: true})
}
