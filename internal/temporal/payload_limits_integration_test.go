package temporal_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/client"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/controlplane"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
)

type capacityProbeModel struct {
	calls atomic.Int64
	text  string
}

func (m *capacityProbeModel) CreateMessage(context.Context, model.Request) (model.Response, error) {
	m.calls.Add(1)
	return model.Response{Content: []domain.ContentBlock{{Type: "text", Text: m.text}},
		StopReason: "end_turn", Usage: domain.TokenUsage{InputTokens: 123, OutputTokens: 42}}, nil
}

func (m *capacityProbeModel) CreateMessageStream(ctx context.Context, request model.Request, _ func(int, string)) (model.Response, error) {
	return m.CreateMessage(ctx, request)
}

// Only inference is simulated. Admission/authentication, projection, Temporal
// transport, Activity retries and usage accounting use the actual services.
func TestVerticalSlice_PayloadCapacity(t *testing.T) {
	database, temporalAddress := os.Getenv("MANGO_TEST_DATABASE_URL"), os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if database == "" || temporalAddress == "" {
		t.Skip("set PostgreSQL and Temporal service-test addresses")
	}
	for _, test := range []struct {
		name       string
		messages   []string
		response   string
		terminated bool
		modelCalls int64
	}{
		{name: "normal", messages: []string{"hello"}, response: "small reply", modelCalls: 1},
		{name: "escaped_input", messages: []string{strings.Repeat("<", 400_000)}, response: "unused", terminated: true},
		{name: "oversized_response", messages: []string{"hello"}, response: strings.Repeat("<", 400_000), terminated: true, modelCalls: 1},
		{name: "cumulative_context", messages: []string{
			strings.Repeat("<", 80_000), strings.Repeat("<", 80_000), strings.Repeat("<", 80_000),
			strings.Repeat("<", 80_000), strings.Repeat("<", 80_000), strings.Repeat("<", 80_000),
		}, response: "small reply", terminated: true, modelCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			store, cleanup := integrationStore(t, database)
			defer cleanup()
			const key = "sk-mango-payload-capacity-test"
			require.NoError(t, store.BootstrapAPIKey(ctx, key))
			connection, err := client.Dial(client.Options{HostPort: temporalAddress})
			require.NoError(t, err)
			defer connection.Close()
			ids := domain.NewRandomIDGen()
			probe := &capacityProbeModel{text: test.response}
			runtime := temporalpkg.NewRuntime(temporalpkg.RuntimeConfig{
				TemporalClient: connection, Store: store, ModelClient: probe, IDGenerator: ids,
				TaskQueue:   "payload-capacity-" + ids.NewID(""),
				RelayConfig: temporalpkg.RelayConfig{PollInterval: 20 * time.Millisecond},
			})
			agents, environments := pg.NewAgentRepository(store), pg.NewEnvironmentRepository(store)
			sessions := controlplane.NewSessionService(store, agents, environments, runtime.Orchestrator(), ids, realClock{}, nil)
			server := httptest.NewServer(httpapi.NewServer(httpapi.Deps{
				Agents:   app.NewAgentService(agents, ids, realClock{}),
				Envs:     app.NewEnvironmentService(environments, ids, realClock{}),
				Sessions: sessions, Events: controlplane.NewEventService(store),
			}, httpapi.Config{RequireAuth: true, Authenticator: store}).Handler())
			defer server.Close()
			agentID := createAuthenticatedResource(t, server.URL, key, "/v1/agents", map[string]any{
				"name": "payload-probe", "model": "payload-probe",
			})
			environmentID := createAuthenticatedResource(t, server.URL, key, "/v1/environments", map[string]any{"name": "payload-probe"})
			sessionID := createAuthenticatedResource(t, server.URL, key, "/v1/sessions", map[string]any{
				"agent": agentID, "environment_id": environmentID,
			})
			defer terminateIntegrationWorkflow(t, connection, sessionID)
			// Admit every input before starting the worker, so a terminal turn
			// cannot race the remaining independent HTTP admissions in the batch.
			for _, text := range test.messages {
				body, err := json.Marshal(map[string]any{"events": []any{
					map[string]any{"type": "user.message", "content": []any{
						map[string]any{"type": "text", "text": text},
					}},
				}})
				require.NoError(t, err)
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/sessions/"+sessionID+"/events", bytes.NewReader(body))
				require.NoError(t, err)
				request.Header.Set("Authorization", "Bearer "+key)
				request.Header.Set("Content-Type", "application/json")
				response, err := server.Client().Do(request)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Equal(t, http.StatusOK, response.StatusCode)
			}
			stop := startIntegrationRuntime(t, ctx, runtime)
			defer stop()
			wantStatus := domain.StatusIdle
			if test.terminated {
				wantStatus = domain.StatusTerminated
			}
			var events []domain.Event
			require.Eventually(t, func() bool {
				var err error
				events, err = store.EventsAfter(ctx, sessionID, 0, 100)
				return err == nil && (hasType(events, domain.EvSessionStatusTerminated) ||
					(!test.terminated && hasType(events, domain.EvAgentMessage) && latestIdleReason(events) == "end_turn"))
			}, 20*time.Second, 50*time.Millisecond)
			final, err := store.GetSession(ctx, sessionID)
			require.NoError(t, err)
			require.Equal(t, wantStatus, final.Status)
			require.Equal(t, test.modelCalls, probe.calls.Load())
			require.Equal(t, 123*test.modelCalls, final.Usage.InputTokens)
			require.Equal(t, 42*test.modelCalls, final.Usage.OutputTokens)
			require.Len(t, eventsOfType(events, domain.EvUserMessage), len(test.messages), "accepted input must remain durable")
			if test.terminated {
				errors := eventsOfType(events, domain.EvSessionError)
				require.Len(t, errors, 1)
				failure := errors[0].Payload["error"].(map[string]any)
				require.Contains(t, failure["message"], "capacity")
				require.Equal(t, "terminal", failure["retry_status"].(map[string]any)["type"])
				// Successful workflow closure proves there is no Activity left
				// repeatedly retrying the same oversized content.
				require.NoError(t, connection.GetWorkflow(ctx, sessionID, "").Get(ctx, nil))
				require.Equal(t, test.modelCalls, probe.calls.Load())
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/sessions/"+sessionID+"/events?limit=100", nil)
			require.NoError(t, err)
			request.Header.Set("Authorization", "Bearer "+key)
			response, err := server.Client().Do(request)
			require.NoError(t, err)
			defer func() { require.NoError(t, response.Body.Close()) }()
			require.Equal(t, http.StatusOK, response.StatusCode)
			var public struct {
				Data []struct {
					Type string `json:"type"`
				} `json:"data"`
			}
			require.NoError(t, json.NewDecoder(response.Body).Decode(&public))
			publicErrors := 0
			for _, event := range public.Data {
				if event.Type == domain.EvSessionError {
					publicErrors++
				}
			}
			if test.terminated {
				require.Equal(t, 1, publicErrors)
			} else {
				require.Zero(t, publicErrors)
			}
		})
	}
}
