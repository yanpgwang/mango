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

// Real HTTP admission, PostgreSQL receipts, and Temporal recovery; the model
// and external tool result are deterministic local fixtures, not a CMA call.
func TestVerticalSlice_ExternalApprovalHTTPRecovery(t *testing.T) {
	databaseURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	temporalAddress := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if databaseURL == "" || temporalAddress == "" {
		t.Skip("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT")
	}
	for _, verdict := range []string{"allow", "deny"} {
		t.Run(verdict, func(t *testing.T) {
			ctx := context.Background()
			store, cleanup := integrationStore(t, databaseURL)
			defer cleanup()
			tc, err := client.Dial(client.Options{HostPort: temporalAddress})
			require.NoError(t, err)
			defer tc.Close()
			ids := domain.NewRandomIDGen()
			probe := &externalApprovalProbe{denied: verdict == "deny"}
			cfg := temporalpkg.RuntimeConfig{TemporalClient: tc, Store: store, ModelClient: probe,
				SandboxProvider: sandboxtest.NoProvision(t), IDGenerator: ids,
				TaskQueue:   "external-approval-" + ids.NewID(""),
				RelayConfig: temporalpkg.RelayConfig{PollInterval: 20 * time.Millisecond}}
			runtime := temporalpkg.NewRuntime(cfg)
			stopFirst := startGateRuntime(t, ctx, runtime)
			defer stopFirst()
			now := time.Now().UTC()
			environment := domain.Environment{ID: "env_external", Name: "external", ConfigType: "self_hosted",
				Config: map[string]any{"type": "self_hosted"}, Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now}
			require.NoError(t, pg.NewEnvironmentRepository(store).Put(ctx, environment))
			session := domain.Session{ID: "sesn_" + ids.NewID(""), AgentID: "agent_external", AgentVersion: 1,
				EnvironmentID: environment.ID, EnvironmentType: "self_hosted", EnvironmentConfig: environment.Config,
				Status: domain.StatusIdle, Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now,
				AgentSnapshot: domain.Agent{ID: "agent_external", Version: 1, Name: "external",
					Model: domain.Model{ID: "approval-probe"}, Tools: []any{map[string]any{
						"type": domain.BuiltinToolsetType, "default_config": map[string]any{"enabled": false},
						"configs": []any{map[string]any{"name": "read", "enabled": true,
							"permission_policy": map[string]any{"type": "always_ask"}}},
					}}}}
			_, _, err = runtime.Orchestrator().CreateSession(ctx, session, nil)
			require.NoError(t, err)
			defer terminateIntegrationWorkflow(t, tc, session.ID)
			service := controlplane.NewSessionService(store, nil, nil, runtime.Orchestrator(), ids, realClock{}, nil)
			server := httptest.NewServer(httpapi.NewServer(httpapi.Deps{
				Sessions: service, Events: controlplane.NewEventService(store),
			}, httpapi.Config{}).Handler())
			defer server.Close()
			eventsURL := server.URL + "/v1/sessions/" + session.ID + "/events"
			send := func(event map[string]any, status int) map[string]any {
				t.Helper()
				body, err := json.Marshal(map[string]any{"events": []any{event}})
				require.NoError(t, err)
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, eventsURL, bytes.NewReader(body))
				require.NoError(t, err)
				request.Header.Set("Content-Type", "application/json")
				response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
				require.NoError(t, err)
				defer func() { require.NoError(t, response.Body.Close()) }()
				var output map[string]any
				require.NoError(t, json.NewDecoder(response.Body).Decode(&output))
				require.Equal(t, status, response.StatusCode, "%v", output)
				return output
			}
			send(map[string]any{"type": "user.message", "content": []any{map[string]any{"type": "text", "text": "read report"}}}, 200)
			var actionID string
			require.Eventually(t, func() bool {
				events, err := store.EventsAfter(ctx, session.ID, 0, 100)
				if err != nil || latestIdleReason(events) != "requires_action" {
					return false
				}
				for _, event := range events {
					if event.Type == domain.EvAgentToolUse && event.Payload["evaluated_permission"] == "ask" {
						actionID = event.ID
					}
				}
				return actionID != ""
			}, 15*time.Second, 20*time.Millisecond)
			result := map[string]any{"type": "user.tool_result", "tool_use_id": actionID,
				"content": []any{map[string]any{"type": "text", "text": "external contents"}}}
			send(result, 400)
			confirmation := map[string]any{"type": "user.tool_confirmation", "tool_use_id": actionID, "result": verdict}
			if verdict == "allow" {
				response := send(confirmation, 200)
				receipt := response["data"].([]any)[0].(map[string]any)
				require.NotNil(t, receipt["processed_at"])
				current, err := store.GetSession(ctx, session.ID)
				require.NoError(t, err)
				require.Equal(t, domain.StatusIdle, current.Status)
				require.Equal(t, int64(1), probe.calls.Load())
				stopFirst()
				restarted := temporalpkg.NewRuntime(cfg)
				stopSecond := startGateRuntime(t, ctx, restarted)
				defer stopSecond()
				// Recovery needs the persisted approval, not any SDK/process memory.
				historyRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, eventsURL, nil)
				require.NoError(t, err)
				historyResponse, err := (&http.Client{Timeout: 5 * time.Second}).Do(historyRequest)
				require.NoError(t, err)
				var history struct {
					Data []map[string]any `json:"data"`
				}
				err = json.NewDecoder(historyResponse.Body).Decode(&history)
				require.NoError(t, historyResponse.Body.Close())
				require.NoError(t, err)
				found := false
				for _, event := range history.Data {
					found = found || event["id"] == receipt["id"] && event["result"] == "allow"
				}
				require.True(t, found)
				send(confirmation, 409)
				send(result, 200)
			} else {
				send(confirmation, 200)
				send(result, 400)
			}
			waitForGateCompletion(t, store, session.ID, 15*time.Second)
			require.Equal(t, int64(2), probe.calls.Load())
			pending, err := store.UnresolvedPendingActions(ctx, session.ID)
			require.NoError(t, err)
			require.Empty(t, pending)
			if verdict == "allow" {
				send(result, 409)
			}
		})
	}
}

type externalApprovalProbe struct {
	calls  atomic.Int64
	denied bool
}

func (p *externalApprovalProbe) CreateMessage(_ context.Context, request model.Request) (model.Response, error) {
	if p.calls.Add(1) == 1 {
		return model.Response{StopReason: "tool_use", Content: []domain.ContentBlock{{Type: "tool_use",
			ToolUseID: "provider_read", ToolName: "read", Input: map[string]any{"path": "never-read-on-server"}}}}, nil
	}
	want := "external contents"
	if p.denied {
		want = "Tool call denied by user."
	}
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_result" && block.ToolResultFor == "provider_read" && block.Text == want && block.IsError == p.denied {
				return model.Response{StopReason: "end_turn", Content: []domain.ContentBlock{{Type: "text", Text: "done"}}}, nil
			}
		}
	}
	return model.Response{}, fmt.Errorf("model resumed without the expected correlated external result or denial")
}

func (p *externalApprovalProbe) CreateMessageStream(ctx context.Context, request model.Request, _ func(int, string)) (model.Response, error) {
	return p.CreateMessage(ctx, request)
}
