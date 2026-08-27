package mango

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClientRejectsCredentialsInEndpointURL(t *testing.T) {
	if _, err := New(Config{BaseURL: "https://secret@example.com"}); err == nil {
		t.Fatal("client accepted credentials embedded in endpoint URL")
	}
	if _, err := New(Config{BaseURL: "https://example.com?api_key=secret"}); err == nil {
		t.Fatal("client accepted query parameters in endpoint URL")
	}
	if _, err := New(Config{BaseURL: "ftp://example.com"}); err == nil {
		t.Fatal("client accepted unsupported endpoint scheme")
	}
}

func TestClientUsesMangoEventContract(t *testing.T) {
	var posted []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer test-key" {
			t.Errorf("authorization = %q", r.Header.Get("authorization"))
		}
		if r.Header.Get("anthropic-beta") != "" || r.Header.Get("anthropic-version") != "" ||
			r.Header.Get("x-api-key") != "" {
			t.Errorf("provider-specific headers leaked to Mango: %v", r.Header)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/sessions/sesn_1/events" {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Events []map[string]any `json:"events"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		posted = append(posted, body.Events...)
		writeJSON(t, w, map[string]any{"data": body.Events})
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL, APIKey: "test-key"})
	ctx := context.Background()
	if _, err := client.SendMessage(ctx, "sesn_1", "hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Interrupt(ctx, "sesn_1", "sthr_child"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Interrupt(ctx, "sesn_1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ResolveAction(ctx, "sesn_1", Action{ID: "sevt_crosspost", ThreadID: "sthr_child", Kind: ActionConfirmation}, ActionResponse{Result: "allow"}); err != nil {
		t.Fatal(err)
	}
	if len(posted) != 4 {
		t.Fatalf("posted = %#v", posted)
	}
	if posted[0]["type"] != "user.message" || posted[1]["session_thread_id"] != "sthr_child" ||
		posted[2]["type"] != "user.interrupt" || posted[2]["session_thread_id"] != nil ||
		posted[3]["tool_use_id"] != "sevt_crosspost" {
		t.Fatalf("posted = %#v", posted)
	}
}

func TestFiniteRequestsHaveTimeoutWithoutCuttingStreamPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	client, err := New(Config{BaseURL: server.URL, RequestTimeout: 25 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = client.ListSessions(context.Background())
	if err == nil || time.Since(started) > time.Second {
		t.Fatalf("err=%v elapsed=%s", err, time.Since(started))
	}
}

func TestClientCreatesAgentEnvironmentAndSession(t *testing.T) {
	requests := map[string]map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests[r.URL.Path] = body
		switch r.URL.Path {
		case "/v1/agents":
			writeJSON(t, w, map[string]any{"id": "agent_1", "name": body["name"], "model": map[string]any{"id": body["model"]}, "version": 1})
		case "/v1/environments":
			writeJSON(t, w, map[string]any{"id": "env_1", "name": body["name"], "config": body["config"]})
		case "/v1/sessions":
			writeJSON(t, w, map[string]any{"id": "sesn_1", "title": body["title"], "status": "running"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL})
	ctx := context.Background()
	if _, err := client.CreateAgent(ctx, CreateAgentInput{
		Name: "coder", Model: "deepseek-v4-flash", System: "be concise",
		Tools:      []map[string]any{{"type": "agent_toolset_20260401"}, {"type": "mcp_toolset", "mcp_server_name": "github"}},
		MCPServers: []map[string]any{{"type": "url", "name": "github", "url": "https://api.githubcopilot.com/mcp"}},
		Multiagent: &MultiagentInput{Type: "coordinator", Agents: []string{"agent_worker"}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateEnvironment(ctx, CreateEnvironmentInput{Name: "cloud"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CreateSession(ctx, CreateSessionInput{AgentID: "agent_1", EnvironmentID: "env_1", Title: "Fix", InitialPrompt: "inspect this repo"}); err != nil {
		t.Fatal(err)
	}
	if requests["/v1/agents"]["model"] != "deepseek-v4-flash" {
		t.Fatalf("agent body = %#v", requests["/v1/agents"])
	}
	tools, _ := requests["/v1/agents"]["tools"].([]any)
	servers, _ := requests["/v1/agents"]["mcp_servers"].([]any)
	topology, _ := requests["/v1/agents"]["multiagent"].(map[string]any)
	if len(tools) != 2 || len(servers) != 1 || topology["type"] != "coordinator" {
		t.Fatalf("agent capabilities = %#v", requests["/v1/agents"])
	}
	config, _ := requests["/v1/environments"]["config"].(map[string]any)
	if config["type"] != "cloud" {
		t.Fatalf("environment body = %#v", requests["/v1/environments"])
	}
	initial, _ := requests["/v1/sessions"]["initial_events"].([]any)
	if requests["/v1/sessions"]["agent"] != "agent_1" || len(initial) != 1 {
		t.Fatalf("session body = %#v", requests["/v1/sessions"])
	}
}

func TestClientManagesSessionLifecycle(t *testing.T) {
	var requests []struct {
		method string
		path   string
		body   map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil && r.ContentLength != 0 {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
		}
		requests = append(requests, struct {
			method string
			path   string
			body   map[string]any
		}{r.Method, r.URL.Path, body})
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/archive"):
			writeJSON(t, w, map[string]any{"id": "sesn_1", "title": "Renamed", "status": "idle", "archived_at": time.Now().UTC()})
		case r.Method == http.MethodPost:
			writeJSON(t, w, map[string]any{"id": "sesn_1", "title": body["title"], "status": "idle"})
		case r.Method == http.MethodDelete:
			writeJSON(t, w, map[string]any{"id": "sesn_1", "type": "session_deleted"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL})
	ctx := context.Background()
	renamed, err := client.UpdateSessionTitle(ctx, "sesn_1", "  Renamed  ")
	if err != nil || renamed.Title != "Renamed" {
		t.Fatalf("rename = %+v, %v", renamed, err)
	}
	archived, err := client.ArchiveSession(ctx, "sesn_1")
	if err != nil || archived.ArchivedAt == nil {
		t.Fatalf("archive = %+v, %v", archived, err)
	}
	if err := client.DeleteSession(ctx, "sesn_1"); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 3 || requests[0].method != http.MethodPost || requests[0].path != "/v1/sessions/sesn_1" ||
		requests[0].body["title"] != "Renamed" || requests[1].path != "/v1/sessions/sesn_1/archive" ||
		requests[2].method != http.MethodDelete || requests[2].path != "/v1/sessions/sesn_1" {
		t.Fatalf("requests = %#v", requests)
	}
}

func TestAttachOpensStreamBeforeListingHistory(t *testing.T) {
	streamOpened := make(chan struct{}, 1)
	streamRelease := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sessions/sesn_1":
			writeJSON(t, w, map[string]any{"id": "sesn_1", "title": "Demo", "status": "idle", "agent": map[string]any{"name": "coordinator"}})
		case "/v1/sessions/sesn_1/threads":
			writeJSON(t, w, map[string]any{"data": []any{map[string]any{"id": "sthr_primary", "session_id": "sesn_1", "parent_thread_id": nil, "status": "idle", "agent": map[string]any{"type": "agent", "id": "agent_1", "name": "coordinator", "version": 1, "model": map[string]any{"id": "model_1"}}}}, "next_page": nil})
		case "/v1/sessions/sesn_1/events/stream":
			if !strings.Contains(r.URL.RawQuery, "event_deltas") {
				t.Errorf("query = %q", r.URL.RawQuery)
			}
			w.Header().Set("content-type", "text/event-stream")
			flusher, _ := w.(http.Flusher)
			flusher.Flush()
			streamOpened <- struct{}{}
			<-streamRelease
		case "/v1/sessions/sesn_1/threads/sthr_primary/events":
			select {
			case <-streamOpened:
			default:
				t.Error("history listed before stream opened")
			}
			writeJSON(t, w, map[string]any{"data": []any{map[string]any{"id": "sevt_1", "type": "agent.message"}}, "next_page": nil})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	attachment, err := client.Attach(ctx, "sesn_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(attachment.Events["sthr_primary"]) != 1 {
		t.Fatalf("events = %#v", attachment.Events)
	}
	attachment.Cancel()
	close(streamRelease)
}

func TestRefreshDecodesAdvisorThreadUnion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sessions/sesn_1":
			writeJSON(t, w, map[string]any{
				"id": "sesn_1", "title": "Review", "status": "running",
				"agent": map[string]any{
					"name": "coordinator", "multiagent": map[string]any{
						"type": "coordinator", "agents": []any{map[string]any{"type": "advisor", "model": "claude-opus-5"}},
					},
				},
			})
		case "/v1/sessions/sesn_1/threads":
			writeJSON(t, w, map[string]any{"data": []any{
				map[string]any{
					"id": "sthr_primary", "session_id": "sesn_1", "parent_thread_id": nil, "status": "running",
					"agent": map[string]any{"type": "agent", "id": "agent_1", "name": "coordinator", "version": 1, "model": map[string]any{"id": "model_1"}},
				},
				map[string]any{
					"id": "sthr_advisor", "session_id": "sesn_1", "parent_thread_id": "sthr_primary", "status": "terminated",
					"agent": map[string]any{"type": "advisor", "model": "claude-opus-5"},
				},
			}, "next_page": nil})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL})
	summary, err := client.Refresh(context.Background(), "sesn_1")
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Threads) != 2 {
		t.Fatalf("threads = %#v", summary.Threads)
	}
	advisor := summary.Threads[1].Agent
	if advisor.Type != "advisor" || advisor.Name != "anthropic.advisor" || advisor.Model.ID != "claude-opus-5" {
		t.Fatalf("advisor = %#v", advisor)
	}
	roster := summary.Session.Agent.Multiagent.Agents
	if len(roster) != 1 || roster[0].Name != "anthropic.advisor" || roster[0].Model.ID != "claude-opus-5" {
		t.Fatalf("advisor roster = %#v", roster)
	}
}

func TestStreamDecodesPreviewAndPersistedFrames(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, "event: event_start\ndata: {\"type\":\"event_start\",\"event\":{\"id\":\"sevt_preview\",\"type\":\"agent.message\"}}\n\n")
		_, _ = io.WriteString(w, "event: agent.message\ndata: {\"id\":\"sevt_preview\",\"type\":\"agent.message\",\"content\":[]}\n\n")
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	updates := make(chan StreamUpdate, 4)
	ready := make(chan error, 1)
	go client.subscribeThread(ctx, "sesn_1", "sthr_1", false, updates, ready)
	if err := <-ready; err != nil {
		t.Fatal(err)
	}
	first, second := <-updates, <-updates
	if first.Frame.Type() != "event_start" || second.Frame.Type() != "agent.message" {
		t.Fatalf("frames = %#v %#v", first, second)
	}
}

func TestAggregateSubscriptionReconnectsWhenAnyChildStreamEnds(t *testing.T) {
	releaseFirst := make(chan struct{})
	primary := "sthr_first"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		flusher.Flush()
		if strings.Contains(r.URL.Path, "/events/stream") {
			<-releaseFirst
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	client, _ := New(Config{BaseURL: server.URL})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	updates, ready := client.subscribe(ctx, "sesn_1", []Thread{
		{ID: "sthr_first"},
		{ID: "sthr_second", ParentThreadID: &primary},
	})
	for range 2 {
		if err := <-ready; err != nil {
			t.Fatal(err)
		}
	}
	close(releaseFirst)
	select {
	case _, open := <-updates:
		if open {
			t.Fatal("aggregate stream remained open after a child stream ended")
		}
	case <-time.After(time.Second):
		t.Fatal("aggregate stream did not close for reconnect")
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("content-type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Fatal(err)
	}
}
