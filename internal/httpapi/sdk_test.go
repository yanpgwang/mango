package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
	mango "github.com/yanpgwang/mango/sdk/go"
)

// Mango's own client exercises the HTTP-to-SDK mapping. Exact wire shapes,
// server validation, and runtime invariants stay in their independent suites.
func sdkClientAndServer(t *testing.T) (*mango.Client, *httptest.Server) {
	t.Helper()
	client, server, _ := sdkClientServerAndSessions(t)
	return client, server
}

func sdkClientServerAndSessions(t *testing.T) (*mango.Client, *httptest.Server, *testSessionService) {
	t.Helper()
	handler, sessions := newTestHandlerWithSessions(t, Config{RequireAuth: true}, false)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: "sk-test"})
	if err != nil {
		t.Fatal(err)
	}
	return client, server, sessions
}

func mustAgent(t *testing.T, client *mango.Client, _ string, system string) mango.Agent {
	t.Helper()
	agent, err := client.Agents.New(context.Background(), mango.AgentCreateRequest{Name: "Test Agent", Model: mango.ModelID("claude-opus-4-8"), System: mango.Some(mango.Ptr(system))})
	if err != nil {
		t.Fatal(err)
	}
	return agent
}

func sdkBody[T any](t *testing.T, text string) T {
	t.Helper()
	var body T
	if err := json.Unmarshal([]byte(text), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestMangoSDKAgentSessionSnapshotsAndUpdates(t *testing.T) {
	client, server := sdkClientAndServer(t)
	ctx := context.Background()
	agent := mustAgent(t, client, "", "original")
	environmentID := mustEnv(t, server.URL)
	coordinator, err := client.Agents.New(ctx, mango.AgentCreateRequest{
		Name: "Coordinator", Model: mango.ModelID("claude-opus-4-8"),
		Multiagent: mango.Some(mango.Coordinator(mango.RosterAgentVersion(agent.ID, agent.Version), mango.Self(), mango.Advisor("claude-opus-4-8"))),
	})
	if err != nil {
		t.Fatal(err)
	}
	session, err := client.Sessions.New(ctx, mango.SessionCreateRequest{Agent: mango.AgentVersion(coordinator.ID, coordinator.Version), EnvironmentID: environmentID})
	if err != nil || session.Agent.Multiagent.SessionResolvedMultiagent == nil || len(session.Agent.Multiagent.SessionResolvedMultiagent.Agents) != 3 {
		t.Fatalf("roster: %+v, %v", session, err)
	}
	updated, err := client.Agents.Update(ctx, coordinator.ID, mango.AgentUpdateRequest{Name: mango.Some("New name")})
	if err != nil || updated.Version != coordinator.Version+1 {
		t.Fatalf("version: %+v, %v", updated, err)
	}
	frozen, err := client.Sessions.Get(ctx, session.ID)
	if err != nil || frozen.Agent.Version != coordinator.Version || frozen.Agent.Name != "Coordinator" {
		t.Fatalf("snapshot: %+v, %v", frozen, err)
	}
	changed, err := client.Sessions.Update(ctx, session.ID, sdkBody[mango.SessionUpdateRequest](t, `{"title":"review","metadata":{"team":"runtime"},"agent":{"system":"session only"}}`))
	if err != nil || changed.Title != "review" {
		t.Fatalf("session update: %+v, %v", changed, err)
	}
	current, err := client.Agents.Get(ctx, coordinator.ID)
	if err != nil || current.Version != updated.Version {
		t.Fatalf("session update changed Agent: %+v, %v", current, err)
	}
	if _, err := client.Agents.Archive(ctx, coordinator.ID); err != nil {
		t.Fatal(err)
	}
	_, err = client.Agents.Update(ctx, coordinator.ID, mango.AgentUpdateRequest{Name: mango.Some("forbidden")})
	assertAPIStatus(t, err, 400)
}

func TestMangoSDKInitialMemoryAndFileOutcome(t *testing.T) {
	client, server := sdkClientAndServer(t)
	agent := mustAgent(t, client, "", "sys")
	body := sdkBody[mango.SessionCreateRequest](t, `{"agent":"placeholder","environment_id":"placeholder","resources":[{"type":"memory_store","memory_store_id":"memstore_project","access":"read_only","instructions":"Use conventions"}],"initial_events":[{"type":"user.define_outcome","description":"produce report.md","rubric":{"type":"file","file_id":"file_rubric"}}]}`)
	body.Agent, body.EnvironmentID = mango.AgentID(agent.ID), mustEnv(t, server.URL)
	session, err := client.Sessions.New(context.Background(), body)
	if err != nil || len(session.Resources) != 1 {
		t.Fatalf("resources: %+v, %v", session, err)
	}
	memory := session.Resources[0]
	if memory.MemoryStoreID != "memstore_project" || memory.Access != "read_only" || memory.Instructions == nil || *memory.Instructions != "Use conventions" {
		t.Fatalf("Memory: %+v", memory)
	}
	events, err := client.Sessions.Events.List(context.Background(), session.ID, mango.ListSessionEventsParams{
		Types: mango.Some([]mango.CoreSessionEventType{mango.CoreSessionEventTypeUserDefineOutcome}),
	})
	if err != nil || len(events.Data) != 1 {
		t.Fatalf("outcome events: %+v, %v", events, err)
	}
	// This admission fixture echoes input fields without evaluating an Outcome.
	// Fully persisted outcome variants are exercised independently below.
	raw, err := json.Marshal(events.Data[0])
	if err != nil {
		t.Fatal(err)
	}
	rubric := rawJSONField(t, string(raw), "rubric")
	if rawJSONField(t, rubric, "file_id") != `"file_rubric"` {
		t.Fatalf("File outcome lost its reference: %s", raw)
	}
	sent, err := client.Sessions.Events.Send(context.Background(), session.ID, sdkBody[mango.SendSessionEventsRequest](t,
		`{"events":[{"type":"user.define_outcome","description":"produce report.md","rubric":{"type":"file","file_id":"file_rubric"}}]}`))
	if err != nil || len(sent.Data) != 1 {
		t.Fatalf("send File outcome: %+v, %v", sent, err)
	}
}

func TestMangoSDKLifecycleEventVariants(t *testing.T) {
	for _, item := range []struct {
		typ     string
		payload map[string]any
	}{
		{domain.EvUserDefineOutcome, map[string]any{"description": "produce report.md", "rubric": map[string]any{"type": "file", "file_id": "file_rubric"}, "max_iterations": 3, "outcome_id": "outcome_fixture"}},
		{domain.EvSessionError, map[string]any{"error": map[string]any{"type": "unknown_error", "message": "turn failed", "retry_status": map[string]any{"type": "terminal"}}}},
		{domain.EvSessionError, map[string]any{"error": map[string]any{"type": "model_rate_limited_error", "message": "slow down", "retry_status": map[string]any{"type": "retrying"}}}},
		{domain.EvSessionError, map[string]any{"error": map[string]any{"type": "billing_error", "message": "credits exhausted", "retry_status": map[string]any{"type": "terminal"}}}},
		{domain.EvSessionStatusRescheduling, map[string]any{}},
		{domain.EvSessionStatusRunning, map[string]any{}},
		{domain.EvAgentThinking, map[string]any{}},
	} {
		t.Run(item.typ, func(t *testing.T) {
			client, server, sessions := sdkClientServerAndSessions(t)
			agent := mustAgent(t, client, "", "sys")
			session, err := client.Sessions.New(context.Background(), mango.SessionCreateRequest{Agent: mango.AgentID(agent.ID), EnvironmentID: mustEnv(t, server.URL)})
			if err != nil {
				t.Fatal(err)
			}
			sessions.mu.Lock()
			sessions.appendEventLocked(session.ID, domain.EventDraft{Type: item.typ, Payload: item.payload})
			sessions.mu.Unlock()
			page, err := client.Sessions.Events.List(context.Background(), session.ID, mango.ListSessionEventsParams{Types: mango.Some([]mango.CoreSessionEventType{mango.CoreSessionEventType(item.typ)})})
			if err != nil || len(page.Data) != 1 {
				t.Fatalf("events: %+v, %v", page, err)
			}
			event := page.Data[0]
			if event.Raw != nil {
				t.Fatalf("known event fell back to raw JSON: %s", event.Raw)
			}
			encoded, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			var decoded map[string]any
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			for key, expected := range item.payload {
				want, _ := json.Marshal(expected)
				got, _ := json.Marshal(decoded[key])
				if string(got) != string(want) {
					t.Fatalf("event %s: %s, want %s", key, got, want)
				}
			}
		})
	}
}
