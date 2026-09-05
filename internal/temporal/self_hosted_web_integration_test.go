package temporal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/controlplane"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
	"github.com/yanpgwang/mango/internal/sandbox/sandboxtest"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
	"go.temporal.io/sdk/client"
)

// Real Mango HTTP, Messages adapter, PostgreSQL, and Temporal; only the
// Messages endpoint and external file-tool result are fixtures. No Web request
// or sandbox execution is delegated to a hosted agent service.
func TestVerticalSlice_SelfHostedWebTranscriptSurvivesWorkerRestart(t *testing.T) {
	databaseURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	temporalAddress := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if databaseURL == "" || temporalAddress == "" {
		t.Skip("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT")
	}
	ctx := context.Background()
	store, cleanup := integrationStore(t, databaseURL)
	defer cleanup()
	tc, err := client.Dial(client.Options{HostPort: temporalAddress})
	require.NoError(t, err)
	defer tc.Close()

	webContent := []json.RawMessage{
		json.RawMessage(`{"type":"server_tool_use","id":"srv_search","name":"web_search","input":{"query":"report"}}`),
		json.RawMessage(`{"type":"web_search_tool_result","tool_use_id":"srv_search","content":[{"type":"web_search_result","url":"https://example.com/report","title":"Report","encrypted_content":"opaque-evidence"}]}`),
		json.RawMessage(`{"type":"server_tool_use","id":"srv_fetch","name":"web_fetch","input":{"url":"https://example.com/private"}}`),
		json.RawMessage(`{"type":"web_fetch_tool_result","tool_use_id":"srv_fetch","content":{"type":"web_fetch_tool_result_error","error_code":"url_not_allowed"}}`),
	}
	var modelCalls atomic.Int64
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool `json:"stream"`
			Tools  []struct {
				Name        string         `json:"name"`
				Type        string         `json:"type"`
				InputSchema map[string]any `json:"input_schema"`
			} `json:"tools"`
			Messages []struct {
				Role    string            `json:"role"`
				Content []json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		require.Equal(t, "/v1/messages", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		require.False(t, request.Stream, "native Web blocks must use the lossless adapter path")
		webTools := 0
		for _, tool := range request.Tools {
			if tool.Name == "web_search" || tool.Name == "web_fetch" {
				webTools++
				require.Equal(t, tool.Name+"_20260318", tool.Type)
				require.Nil(t, tool.InputSchema)
			}
		}
		require.Equal(t, 2, webTools)
		w.Header().Set("Content-Type", "application/json")
		if modelCalls.Add(1) == 1 {
			content := append(append([]json.RawMessage(nil), webContent...),
				json.RawMessage(`{"type":"tool_use","id":"provider_read","name":"read","input":{"path":"report.txt"}}`))
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"content": content, "stop_reason": "tool_use",
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 10,
					"server_tool_use": map[string]any{"web_search_requests": 1, "web_fetch_requests": 1}},
			}))
			return
		}
		var replay []json.RawMessage
		foundResult := false
		for _, message := range request.Messages {
			for _, raw := range message.Content {
				var block map[string]any
				require.NoError(t, json.Unmarshal(raw, &block))
				switch block["type"] {
				case "server_tool_use", "web_search_tool_result", "web_fetch_tool_result":
					require.Equal(t, "assistant", message.Role)
					replay = append(replay, raw)
				case "tool_result":
					if block["tool_use_id"] == "provider_read" {
						foundResult = true
						require.Contains(t, string(raw), "external report")
					}
				}
			}
		}
		require.Len(t, replay, len(webContent))
		for i, want := range webContent {
			require.JSONEq(t, string(want), string(replay[i]))
		}
		require.True(t, foundResult, "model resumed without the correlated external result")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"content":     []any{map[string]any{"type": "text", "text": "Research complete."}},
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 20, "output_tokens": 5},
		}))
	}))
	defer endpoint.Close()
	modelClient, err := model.NewAnthropic(model.AnthropicConfig{
		BaseURL: endpoint.URL, APIKey: "test-model-key", Model: "web-probe", HTTPClient: endpoint.Client(),
	})
	require.NoError(t, err)
	ids := domain.NewRandomIDGen()
	cfg := temporalpkg.RuntimeConfig{TemporalClient: tc, Store: store, ModelClient: modelClient,
		SandboxProvider: sandboxtest.NoProvision(t), IDGenerator: ids,
		TaskQueue:   "self-hosted-web-" + ids.NewID(""),
		RelayConfig: temporalpkg.RelayConfig{PollInterval: 20 * time.Millisecond}}
	runtime := temporalpkg.NewRuntime(cfg)
	stopFirst := startGateRuntime(t, ctx, runtime)
	defer stopFirst()
	now := time.Now().UTC()
	environment := domain.Environment{ID: "env_web", Name: "external", ConfigType: "self_hosted",
		Config: map[string]any{"type": "self_hosted"}, Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, pg.NewEnvironmentRepository(store).Put(ctx, environment))
	session := domain.Session{ID: "sesn_" + ids.NewID(""), AgentID: "agent_web", AgentVersion: 1,
		EnvironmentID: environment.ID, EnvironmentType: "self_hosted", EnvironmentConfig: environment.Config,
		Status: domain.StatusIdle, Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now,
		AgentSnapshot: domain.Agent{ID: "agent_web", Version: 1, Name: "researcher",
			Model: domain.Model{ID: "web-probe"}, Tools: []any{map[string]any{"type": domain.BuiltinToolsetType}}}}
	_, _, err = runtime.Orchestrator().CreateSession(ctx, session, nil)
	require.NoError(t, err)
	defer terminateIntegrationWorkflow(t, tc, session.ID)
	service := controlplane.NewSessionService(store, nil, nil, runtime.Orchestrator(), ids, realClock{}, nil)
	server := httptest.NewServer(httpapi.NewServer(httpapi.Deps{
		Sessions: service, Events: controlplane.NewEventService(store),
	}, httpapi.Config{}).Handler())
	defer server.Close()
	send := func(event map[string]any) {
		t.Helper()
		body, err := json.Marshal(map[string]any{"events": []any{event}})
		require.NoError(t, err)
		response, err := http.Post(server.URL+"/v1/sessions/"+session.ID+"/events", "application/json", bytes.NewReader(body))
		require.NoError(t, err)
		defer func() { require.NoError(t, response.Body.Close()) }()
		var result map[string]any
		require.NoError(t, json.NewDecoder(response.Body).Decode(&result))
		require.Equal(t, http.StatusOK, response.StatusCode, "%v", result)
	}
	send(map[string]any{"type": "user.message", "content": []any{map[string]any{"type": "text", "text": "research and read the report"}}})
	var readActionID string
	require.Eventually(t, func() bool {
		events, err := store.EventsAfter(ctx, session.ID, 0, 100)
		if err != nil || latestIdleReason(events) != "requires_action" {
			return false
		}
		for _, event := range events {
			if event.Type == domain.EvAgentToolUse {
				require.Equal(t, "read", event.Payload["name"], "Web calls must not reach the worker event stream")
				readActionID = event.ID
			}
		}
		return readActionID != ""
	}, 15*time.Second, 20*time.Millisecond)
	pending, err := store.UnresolvedPendingActions(ctx, session.ID)
	require.NoError(t, err)
	require.Len(t, pending, 1, "only the file read needs an external result")
	require.Equal(t, int64(1), modelCalls.Load())

	stopFirst()
	stopSecond := startGateRuntime(t, ctx, temporalpkg.NewRuntime(cfg))
	defer stopSecond()
	send(map[string]any{"type": "user.tool_result", "tool_use_id": readActionID,
		"content": []any{map[string]any{"type": "text", "text": "external report"}}})
	waitForGateCompletion(t, store, session.ID, 15*time.Second)
	require.Equal(t, int64(2), modelCalls.Load())
	pending, err = store.UnresolvedPendingActions(ctx, session.ID)
	require.NoError(t, err)
	require.Empty(t, pending)
	transcript, err := store.LoadProviderTranscript(ctx, session.ID)
	require.NoError(t, err)
	encoded, err := json.Marshal(transcript.Messages)
	require.NoError(t, err)
	for _, evidence := range []string{"opaque-evidence", "url_not_allowed", "external report"} {
		require.Contains(t, string(encoded), evidence, fmt.Sprintf("lost durable %s", evidence))
	}
}
