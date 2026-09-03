package mango

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type sessionToolFunc struct {
	name string
	run  func(context.Context, SessionToolCall) ([]ResultContentInput, error)
}

func (t sessionToolFunc) Name() string { return t.name }
func (t sessionToolFunc) Execute(ctx context.Context, call SessionToolCall) ([]ResultContentInput, error) {
	return t.run(ctx, call)
}

func TestSessionToolRunnerReconcilesAfterConnectingAndDeduplicatesStreamOverlap(t *testing.T) {
	t.Parallel()
	streamSent := make(chan struct{})
	posted := make(chan map[string]any, 1)
	toolUse := sessionToolUseJSON("tool_recover", "echo", "allow", "sthr_one")
	var listCalls atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events/stream"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			flusher.Flush()
			fmt.Fprintf(w, "event: agent.tool_use\ndata: %s\n\n", toolUse)
			flusher.Flush()
			close(streamSent)
			<-request.Context().Done()
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events"):
			<-streamSent
			listCalls.Add(1)
			if request.URL.Query().Get("page") == "" {
				writeSessionToolRunnerJSON(t, w, map[string]any{"data": []json.RawMessage{toolUse}, "next_page": "page_two"})
				return
			}
			if request.URL.Query().Get("page") != "page_two" {
				t.Errorf("page = %q", request.URL.Query().Get("page"))
			}
			writeSessionToolRunnerJSON(t, w, map[string]any{"data": []any{}, "next_page": nil})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events"):
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode result: %v", err)
			}
			posted <- body
			writeSessionToolRunnerJSON(t, w, map[string]any{"data": []any{}})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	var executions atomic.Int32
	runner := NewSessionToolRunner(context.Background(), newSessionToolRunnerClient(t, server.URL), "session_one", SessionToolRunnerOptions{
		Tools: []SessionTool{sessionToolFunc{name: "echo", run: func(_ context.Context, call SessionToolCall) ([]ResultContentInput, error) {
			executions.Add(1)
			if call.SessionID != "session_one" || call.ToolUseID != "tool_recover" || call.Name != "echo" || call.Custom {
				t.Errorf("call = %#v", call)
			}
			if threadID, ok := call.SessionThreadID.Get(); !ok || threadID != "sthr_one" {
				t.Errorf("thread ID = %q, %v", threadID, ok)
			}
			if string(call.Input) != `{"value":"hello"}` {
				t.Errorf("input = %s", call.Input)
			}
			return textSessionToolResult("done"), nil
		}}},
		MaxIdle: durationPointer(0),
	})
	defer runner.Close()

	if !runner.Next() {
		t.Fatalf("Next() = false: %v", runner.Err())
	}
	call := runner.Current()
	if !call.Owned || !call.Posted || call.IsError || call.Result == nil || call.CustomResult != nil {
		t.Fatalf("dispatched call = %#v", call)
	}
	if call.Result.Type != "user.tool_result" || call.Result.ToolUseID != "tool_recover" {
		t.Fatalf("result = %#v", call.Result)
	}
	select {
	case body := <-posted:
		events := body["events"].([]any)
		result := events[0].(map[string]any)
		if result["tool_use_id"] != "tool_recover" || result["session_thread_id"] != "sthr_one" {
			t.Fatalf("posted result = %#v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("tool result was not posted")
	}
	if got := executions.Load(); got != 1 {
		t.Fatalf("tool executions = %d, want 1", got)
	}
	if listCalls.Load() != 2 {
		t.Fatalf("history pages = %d, want 2", listCalls.Load())
	}
}

func TestSessionToolRunnerHonorsPermissionsAndDispatchesCustomTools(t *testing.T) {
	t.Parallel()
	history := []json.RawMessage{
		sessionToolUseJSON("tool_ask", "echo", "ask", "sthr_ask"),
		json.RawMessage(`{"id":"confirmation_allow","type":"user.tool_confirmation","processed_at":null,"tool_use_id":"tool_ask","result":"allow","session_thread_id":"sthr_ask"}`),
		sessionToolUseJSON("tool_deny", "echo", "deny", ""),
		sessionToolUseJSON("tool_unknown", "echo", "future_policy", ""),
		json.RawMessage(`{"id":"custom_one","type":"agent.custom_tool_use","processed_at":null,"name":"custom","input":{"count":2},"session_thread_id":"sthr_custom"}`),
		json.RawMessage(`{"id":"custom_external","type":"agent.custom_tool_use","processed_at":null,"name":"external_owner","input":{}}`),
		json.RawMessage(`{"id":"mcp_one","type":"agent.mcp_tool_use","processed_at":null,"name":"remote","mcp_server_name":"server","input":{},"evaluated_permission":"allow"}`),
	}
	var postMu sync.Mutex
	var posted []map[string]any
	server := sessionToolRunnerHistoryServer(t, history, func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Events []map[string]any `json:"events"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode result: %v", err)
		}
		postMu.Lock()
		posted = append(posted, body.Events...)
		postMu.Unlock()
		writeSessionToolRunnerJSON(t, w, map[string]any{"data": []any{}})
	})
	defer server.Close()

	var executedMu sync.Mutex
	var executed []string
	tool := func(name string) SessionTool {
		return sessionToolFunc{name: name, run: func(_ context.Context, call SessionToolCall) ([]ResultContentInput, error) {
			executedMu.Lock()
			executed = append(executed, call.ToolUseID)
			executedMu.Unlock()
			return textSessionToolResult(call.Name + " result"), nil
		}}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := NewSessionToolRunner(ctx, newSessionToolRunnerClient(t, server.URL), "session_permissions", SessionToolRunnerOptions{
		Tools: []SessionTool{tool("echo"), tool("custom")}, MaxIdle: durationPointer(0),
	})
	defer runner.Close()

	calls := make(map[string]DispatchedToolCall)
	for len(calls) < 4 && runner.Next() {
		calls[runner.Current().ToolUseID] = runner.Current()
	}
	if len(calls) != 4 {
		t.Fatalf("calls = %#v, runner error = %v", calls, runner.Err())
	}
	if got := calls["tool_ask"]; !got.Posted || got.Confirmation != "allow" || got.Custom {
		t.Fatalf("approved call = %#v", got)
	}
	if got := calls["tool_deny"]; got.Posted || got.Confirmation != "deny" || !got.Owned || got.Result != nil {
		t.Fatalf("denied call = %#v", got)
	}
	if got := calls["custom_one"]; !got.Posted || !got.Custom || got.CustomResult == nil {
		t.Fatalf("custom call = %#v", got)
	}
	if got := calls["custom_external"]; got.Posted || got.Owned || !got.Custom || got.CustomResult != nil {
		t.Fatalf("unowned custom call = %#v", got)
	}
	if _, ok := calls["tool_unknown"]; ok {
		t.Fatal("unknown permission was dispatched without explicit approval")
	}
	if _, ok := calls["mcp_one"]; ok {
		t.Fatal("server-side MCP call was dispatched locally")
	}

	executedMu.Lock()
	sort.Strings(executed)
	if strings.Join(executed, ",") != "custom_one,tool_ask" {
		t.Fatalf("executed = %v", executed)
	}
	executedMu.Unlock()
	postMu.Lock()
	if len(posted) != 2 {
		t.Fatalf("posted = %#v", posted)
	}
	postMu.Unlock()
}

func TestSessionToolRunnerRecognizesAmbiguousCommittedResult(t *testing.T) {
	t.Parallel()
	toolUse := sessionToolUseJSON("tool_ambiguous", "echo", "allow", "")
	var committed atomic.Bool
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events/stream"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-request.Context().Done()
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events"):
			if len(request.URL.Query()["types[]"]) > 0 && committed.Load() {
				result := json.RawMessage(`{"id":"result_one","type":"user.tool_result","processed_at":null,"tool_use_id":"tool_ambiguous","content":[{"type":"text","text":"done"}],"is_error":false}`)
				writeSessionToolRunnerJSON(t, w, map[string]any{"data": []json.RawMessage{result}, "next_page": nil})
				return
			}
			writeSessionToolRunnerJSON(t, w, map[string]any{"data": []json.RawMessage{toolUse}, "next_page": nil})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events"):
			posts.Add(1)
			committed.Store(true)
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"type":"server_error","message":"response lost"}}`)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	var executions atomic.Int32
	runner := NewSessionToolRunner(context.Background(), newSessionToolRunnerClient(t, server.URL), "session_ambiguous", SessionToolRunnerOptions{
		Tools: []SessionTool{sessionToolFunc{name: "echo", run: func(context.Context, SessionToolCall) ([]ResultContentInput, error) {
			executions.Add(1)
			return textSessionToolResult("done"), nil
		}}},
		MaxIdle: durationPointer(0),
	})
	defer runner.Close()
	if !runner.Next() {
		t.Fatalf("Next() = false: %v", runner.Err())
	}
	if !runner.Current().Posted {
		t.Fatalf("call = %#v", runner.Current())
	}
	if executions.Load() != 1 || posts.Load() != 1 {
		t.Fatalf("executions = %d, posts = %d", executions.Load(), posts.Load())
	}
}

func TestSessionToolRunnerBoundsHangingResultReconciliation(t *testing.T) {
	t.Parallel()
	toolUse := sessionToolUseJSON("tool_hanging_reconcile", "echo", "allow", "")
	var postFailed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events/stream"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-request.Context().Done()
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events"):
			if postFailed.Load() {
				<-request.Context().Done()
				return
			}
			writeSessionToolRunnerJSON(t, w, map[string]any{"data": []json.RawMessage{toolUse}, "next_page": nil})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events"):
			postFailed.Store(true)
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"type":"server_error","message":"try again"}}`)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := New(Config{BaseURL: server.URL, APIKey: "session-token", RequestTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	runner := NewSessionToolRunner(context.Background(), client, "session_hanging_reconcile", SessionToolRunnerOptions{
		Tools: []SessionTool{sessionToolFunc{name: "echo", run: func(context.Context, SessionToolCall) ([]ResultContentInput, error) {
			return textSessionToolResult("done"), nil
		}}},
		MaxIdle: durationPointer(0), SendRetryWindow: 40 * time.Millisecond,
	})
	defer runner.Close()
	if !runner.Next() {
		t.Fatalf("missing failed dispatch: %v", runner.Err())
	}
	if call := runner.Current(); call.ToolUseID != "tool_hanging_reconcile" || call.Posted {
		t.Fatalf("dispatched call = %#v", call)
	}
	if runner.Next() {
		t.Fatalf("unexpected second dispatch: %#v", runner.Current())
	}
	if err := runner.Err(); err == nil || !strings.Contains(err.Error(), "retry window exhausted") {
		t.Fatalf("Err() = %v, want retry window exhaustion", err)
	}
	if elapsed := time.Since(started); elapsed >= 500*time.Millisecond {
		t.Fatalf("retry window took %s; reconciliation inherited the client request timeout", elapsed)
	}
}

func TestSessionToolRunnerReconcilesHistoryAfterReconnect(t *testing.T) {
	t.Parallel()
	toolUse := sessionToolUseJSON("tool_reconnected", "echo", "allow", "")
	var streamCalls atomic.Int32
	var listCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events/stream"):
			call := streamCalls.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			if call > 1 {
				<-request.Context().Done()
			}
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events"):
			if listCalls.Add(1) == 1 {
				writeSessionToolRunnerJSON(t, w, map[string]any{"data": []any{}, "next_page": nil})
				return
			}
			writeSessionToolRunnerJSON(t, w, map[string]any{"data": []json.RawMessage{toolUse}, "next_page": nil})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events"):
			writeSessionToolRunnerJSON(t, w, map[string]any{"data": []any{}})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	var executions atomic.Int32
	runner := NewSessionToolRunner(context.Background(), newSessionToolRunnerClient(t, server.URL), "session_reconnect", SessionToolRunnerOptions{
		Tools: []SessionTool{sessionToolFunc{name: "echo", run: func(context.Context, SessionToolCall) ([]ResultContentInput, error) {
			executions.Add(1)
			return textSessionToolResult("done"), nil
		}}},
		MaxIdle: durationPointer(0),
	})
	runner.retryDelay = func(int) time.Duration { return 0 }
	defer runner.Close()
	if !runner.Next() {
		t.Fatalf("Next() = false: %v", runner.Err())
	}
	if runner.Current().ToolUseID != "tool_reconnected" || !runner.Current().Posted {
		t.Fatalf("call = %#v", runner.Current())
	}
	if streamCalls.Load() < 2 || listCalls.Load() < 2 || executions.Load() != 1 {
		t.Fatalf("stream calls = %d, list calls = %d, executions = %d", streamCalls.Load(), listCalls.Load(), executions.Load())
	}
}

func TestSessionToolRunnerStopsExecutionWhenLeaseIsLost(t *testing.T) {
	t.Parallel()
	toolStarted := make(chan struct{})
	var streamCalls atomic.Int32
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events/stream"):
			if streamCalls.Add(1) > 1 {
				w.WriteHeader(http.StatusPreconditionFailed)
				fmt.Fprint(w, `{"error":{"type":"lease_lost","message":"claim expired"}}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			flusher.Flush()
			fmt.Fprintf(w, "event: agent.tool_use\ndata: %s\n\n", sessionToolUseJSON("tool_slow", "slow", "allow", ""))
			flusher.Flush()
			<-toolStarted
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events"):
			writeSessionToolRunnerJSON(t, w, map[string]any{"data": []any{}, "next_page": nil})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events"):
			posts.Add(1)
			writeSessionToolRunnerJSON(t, w, map[string]any{"data": []any{}})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := NewSessionToolRunner(ctx, newSessionToolRunnerClient(t, server.URL), "session_lease", SessionToolRunnerOptions{
		Tools: []SessionTool{sessionToolFunc{name: "slow", run: func(ctx context.Context, _ SessionToolCall) ([]ResultContentInput, error) {
			close(toolStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		}}},
		MaxIdle: durationPointer(0),
	})
	runner.retryDelay = func(int) time.Duration { return 0 }
	defer runner.Close()
	for runner.Next() {
		if runner.Current().Posted {
			t.Fatalf("posted after lease loss: %#v", runner.Current())
		}
	}
	if !errors.Is(runner.Err(), ErrSessionLeaseLost) {
		t.Fatalf("Err() = %v, want ErrSessionLeaseLost", runner.Err())
	}
	if posts.Load() != 0 {
		t.Fatalf("posts = %d after lease loss", posts.Load())
	}
}

func TestSessionToolRunnerStopsAfterEndTurnIdle(t *testing.T) {
	t.Parallel()
	idle := json.RawMessage(`{"id":"idle_one","type":"session.status_idle","processed_at":null,"stop_reason":{"type":"end_turn"}}`)
	server := sessionToolRunnerHistoryServer(t, []json.RawMessage{idle}, nil)
	defer server.Close()

	maxIdle := 10 * time.Millisecond
	runner := NewSessionToolRunner(context.Background(), newSessionToolRunnerClient(t, server.URL), "session_idle", SessionToolRunnerOptions{MaxIdle: &maxIdle})
	defer runner.Close()
	if runner.Next() {
		t.Fatalf("unexpected call: %#v", runner.Current())
	}
	if !errors.Is(runner.Err(), ErrIdleTimeout) {
		t.Fatalf("Err() = %v, want ErrIdleTimeout", runner.Err())
	}
}

func TestSessionToolRunnerDefersIdleWhileApprovalIsPending(t *testing.T) {
	t.Parallel()
	historyServed := make(chan struct{})
	toolUse := sessionToolUseJSON("tool_waiting", "echo", "ask", "")
	idle := json.RawMessage(`{"id":"idle_waiting","type":"session.status_idle","processed_at":null,"stop_reason":{"type":"end_turn"}}`)
	confirmation := json.RawMessage(`{"id":"confirmation_late","type":"user.tool_confirmation","processed_at":null,"tool_use_id":"tool_waiting","result":"allow"}`)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events/stream"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			flusher.Flush()
			<-historyServed
			timer := time.NewTimer(30 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-timer.C:
				fmt.Fprintf(w, "event: user.tool_confirmation\ndata: %s\n\n", confirmation)
				flusher.Flush()
			case <-request.Context().Done():
				return
			}
			<-request.Context().Done()
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events"):
			writeSessionToolRunnerJSON(t, w, map[string]any{"data": []json.RawMessage{toolUse, idle}, "next_page": nil})
			close(historyServed)
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events"):
			writeSessionToolRunnerJSON(t, w, map[string]any{"data": []any{}})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	maxIdle := 10 * time.Millisecond
	runner := NewSessionToolRunner(ctx, newSessionToolRunnerClient(t, server.URL), "session_waiting", SessionToolRunnerOptions{
		Tools: []SessionTool{sessionToolFunc{name: "echo", run: func(context.Context, SessionToolCall) ([]ResultContentInput, error) {
			return textSessionToolResult("approved"), nil
		}}},
		MaxIdle: &maxIdle,
	})
	defer runner.Close()
	if !runner.Next() {
		t.Fatalf("approval-gated tool was lost to idle timeout: %v", runner.Err())
	}
	if call := runner.Current(); call.ToolUseID != "tool_waiting" || call.Confirmation != "allow" || !call.Posted {
		t.Fatalf("call = %#v", call)
	}
}

func TestSessionToolRunnerValidatesConfiguration(t *testing.T) {
	t.Parallel()
	client := newSessionToolRunnerClient(t, "http://mango.test")
	tests := []struct {
		name      string
		client    *Client
		sessionID string
		opts      SessionToolRunnerOptions
	}{
		{name: "nil client", sessionID: "session", opts: SessionToolRunnerOptions{}},
		{name: "empty session", client: client, opts: SessionToolRunnerOptions{}},
		{name: "negative tool timeout", client: client, sessionID: "session", opts: SessionToolRunnerOptions{ToolTimeout: -1}},
		{name: "negative send timeout", client: client, sessionID: "session", opts: SessionToolRunnerOptions{SendTimeout: -1}},
		{name: "negative send retry", client: client, sessionID: "session", opts: SessionToolRunnerOptions{SendRetryWindow: -1}},
		{name: "empty tool name", client: client, sessionID: "session", opts: SessionToolRunnerOptions{Tools: []SessionTool{sessionToolFunc{}}}},
		{name: "duplicate tool", client: client, sessionID: "session", opts: SessionToolRunnerOptions{Tools: []SessionTool{
			sessionToolFunc{name: "same"}, sessionToolFunc{name: "same"},
		}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runner := NewSessionToolRunner(context.Background(), test.client, test.sessionID, test.opts)
			if runner.Next() || runner.Err() == nil {
				t.Fatalf("Next() = true or Err() = nil: %v", runner.Err())
			}
			if err := runner.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func sessionToolRunnerHistoryServer(
	t *testing.T,
	history []json.RawMessage,
	post func(http.ResponseWriter, *http.Request),
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events/stream"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-request.Context().Done()
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events"):
			writeSessionToolRunnerJSON(t, w, map[string]any{"data": history, "next_page": nil})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events") && post != nil:
			post(w, request)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
}

func sessionToolUseJSON(id, name, permission, threadID string) json.RawMessage {
	event := map[string]any{
		"id": id, "type": "agent.tool_use", "processed_at": nil,
		"name": name, "input": map[string]any{"value": "hello"},
	}
	if permission != "" {
		event["evaluated_permission"] = permission
	}
	if threadID != "" {
		event["session_thread_id"] = threadID
	}
	data, _ := json.Marshal(event)
	return data
}

func newSessionToolRunnerClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := New(Config{BaseURL: baseURL, APIKey: "session-token", RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeSessionToolRunnerJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func durationPointer(value time.Duration) *time.Duration { return &value }
