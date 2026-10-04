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
	"go.temporal.io/sdk/client"

	"github.com/yanpgwang/mango/internal/controlplane"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
)

// Only the provider and sandbox result are fixtures. Real PostgreSQL, Temporal,
// HTTP admission, accounting, and a worker restart prove the durable lifecycle.
func TestVerticalSlice_AutomaticPermissionOutcomes(t *testing.T) {
	databaseURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	temporalAddress := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if databaseURL == "" || temporalAddress == "" {
		t.Skip("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT")
	}
	for _, decision := range []string{"allow", "ask", "deny"} {
		t.Run(decision, func(t *testing.T) {
			ctx := context.Background()
			store, cleanup := integrationStore(t, databaseURL)
			defer cleanup()
			tc, err := client.Dial(client.Options{HostPort: temporalAddress})
			require.NoError(t, err)
			defer tc.Close()
			ids := domain.NewRandomIDGen()
			probe := &automaticPermissionProbe{decision: decision}
			cfg := temporalpkg.RuntimeConfig{TemporalClient: tc, Store: store, ModelClient: probe, IDGenerator: ids, TaskQueue: "automatic-permission-" + ids.NewID(""), RelayConfig: temporalpkg.RelayConfig{PollInterval: 20 * time.Millisecond}}
			runtime := temporalpkg.NewRuntime(cfg)
			stopFirst := startIntegrationRuntime(t, ctx, runtime)
			defer stopFirst()
			now := time.Now().UTC()
			environment := domain.Environment{ID: "env_auto", Name: "automatic", ConfigType: "self_hosted", Config: map[string]any{"type": "self_hosted"}, Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now}
			require.NoError(t, pg.NewEnvironmentRepository(store).Put(ctx, environment))
			session := domain.Session{ID: "sesn_" + ids.NewID(""), AgentID: "agent_auto", AgentVersion: 1, EnvironmentID: environment.ID, EnvironmentType: "self_hosted", EnvironmentConfig: environment.Config, Status: domain.StatusIdle, ListCostKnown: true, Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now,
				AgentSnapshot: domain.Agent{ID: "agent_auto", Version: 1, Name: "automatic", Model: domain.Model{ID: "claude-opus-4-8"}, Tools: []any{map[string]any{"type": domain.BuiltinToolsetType, "default_config": map[string]any{"enabled": false}, "configs": []any{map[string]any{"name": "read", "enabled": true, "permission_policy": map[string]any{"type": "auto"}}}}}}}
			_, _, err = runtime.Orchestrator().CreateSession(ctx, session, nil)
			require.NoError(t, err)
			defer terminateIntegrationWorkflow(t, tc, session.ID)
			service := controlplane.NewSessionService(store, nil, nil, runtime.Orchestrator(), ids, realClock{}, nil)
			server := httptest.NewServer(httpapi.NewServer(httpapi.Deps{Sessions: service, Events: controlplane.NewEventService(store)}, httpapi.Config{}).Handler())
			defer server.Close()
			send := func(event map[string]any, wantStatus int) {
				t.Helper()
				body, err := json.Marshal(map[string]any{"events": []any{event}})
				require.NoError(t, err)
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/sessions/"+session.ID+"/events", bytes.NewReader(body))
				require.NoError(t, err)
				request.Header.Set("Content-Type", "application/json")
				response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
				require.NoError(t, err)
				defer func() { require.NoError(t, response.Body.Close()) }()
				var output map[string]any
				require.NoError(t, json.NewDecoder(response.Body).Decode(&output))
				require.Equal(t, wantStatus, response.StatusCode, "%v", output)
			}
			send(map[string]any{"type": "user.message", "content": []any{map[string]any{"type": "text", "text": "Read README.md."}}}, 200)
			var actionID string
			require.Eventually(t, func() bool {
				events, err := store.EventsAfter(ctx, session.ID, 0, 100)
				if err != nil {
					return false
				}
				for _, event := range events {
					if event.Type == domain.EvAgentToolUse {
						actionID = event.ID
						if event.Payload["evaluated_permission"] != decision {
							return false
						}
						evaluation, ok := event.Payload["evaluation"].(map[string]any)
						return ok && evaluation["type"] == "auto" && latestIdleReason(events) != ""
					}
				}
				return false
			}, 15*time.Second, 20*time.Millisecond)
			if decision == "ask" {
				send(map[string]any{"type": "user.tool_confirmation", "tool_use_id": actionID, "result": "allow"}, 200)
				stopFirst()
				restarted := temporalpkg.NewRuntime(cfg)
				stopSecond := startIntegrationRuntime(t, ctx, restarted)
				defer stopSecond()
			} else {
				send(map[string]any{"type": "user.tool_confirmation", "tool_use_id": actionID, "result": "allow"}, 400)
			}
			if decision != "deny" {
				send(map[string]any{"type": "user.tool_result", "tool_use_id": actionID, "content": []any{map[string]any{"type": "text", "text": "README contents"}}}, 200)
			}
			waitForIntegrationCompletion(t, store, session.ID, 15*time.Second)
			require.Equal(t, int64(1), probe.evaluations.Load())
			require.Equal(t, int64(2), probe.calls.Load())
			current, err := store.GetSession(ctx, session.ID)
			require.NoError(t, err)
			require.Equal(t, int64(215), current.Usage.InputTokens, "permission and working calls must each be billed once")
			pending, err := store.UnresolvedPendingActions(ctx, session.ID)
			require.NoError(t, err)
			require.Empty(t, pending)
		})
	}
}

type automaticPermissionProbe struct {
	decision    string
	calls       atomic.Int64
	evaluations atomic.Int64
}

func (p *automaticPermissionProbe) CreateMessage(_ context.Context, request model.Request) (model.Response, error) {
	if request.MaxTokens == 256 && len(request.Tools) == 0 {
		p.evaluations.Add(1)
		var envelope struct {
			Intent domain.PermissionIntent `json:"intent"`
		}
		if len(request.Messages) != 1 || len(request.Messages[0].Content) != 1 {
			return model.Response{}, fmt.Errorf("permission request missing envelope")
		}
		if err := json.Unmarshal([]byte(request.Messages[0].Content[0].Text), &envelope); err != nil || !envelope.Intent.Complete || len(envelope.Intent.Entries) != 1 || envelope.Intent.Entries[0].Text != "Read README.md." {
			return model.Response{}, fmt.Errorf("permission request lost original client intent")
		}
		reason := ""
		if p.decision == "ask" {
			reason = "indeterminate"
		}
		if p.decision == "deny" {
			reason = "high_risk"
		}
		body, _ := json.Marshal(domain.ToolPermissionDecision{Type: p.decision, ReasonCode: reason})
		return model.Response{StopReason: "end_turn", Usage: domain.TokenUsage{InputTokens: 13}, Content: []domain.ContentBlock{{Type: "text", Text: string(body)}}}, nil
	}
	if p.calls.Add(1) == 1 {
		return model.Response{StopReason: "tool_use", Usage: domain.TokenUsage{InputTokens: 101}, Content: []domain.ContentBlock{{Type: "tool_use", ToolUseID: "provider_read", ToolName: "read", Input: map[string]any{"path": "README.md"}}}}, nil
	}
	want := "README contents"
	if p.decision == "deny" {
		want = "Permission to use read has been denied."
	}
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_result" && block.ToolResultFor == "provider_read" && block.Text == want && block.IsError == (p.decision == "deny") {
				return model.Response{StopReason: "end_turn", Usage: domain.TokenUsage{InputTokens: 101}, Content: []domain.ContentBlock{{Type: "text", Text: "done"}}}, nil
			}
		}
	}
	return model.Response{}, fmt.Errorf("model resumed without correlated result")
}
func (p *automaticPermissionProbe) CreateMessageStream(ctx context.Context, request model.Request, _ func(int, string)) (model.Response, error) {
	return p.CreateMessage(ctx, request)
}
