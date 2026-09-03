package mango

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnvironmentWorkerHandleItemUsesScopedCredentialForLifecycle(t *testing.T) {
	t.Parallel()
	const token = "sess_mango_scoped"
	posted := make(chan struct{})
	var postedOnce sync.Once
	var heartbeats atomic.Int32
	var stops atomic.Int32
	var executions atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer "+token {
			t.Errorf("%s %s Authorization = %q", request.Method, request.URL.Path, got)
		}
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/heartbeat"):
			heartbeats.Add(1)
			if got := request.URL.Query().Get("expected_last_heartbeat"); got != noEnvironmentHeartbeat {
				t.Errorf("expected_last_heartbeat = %q", got)
			}
			if got := request.URL.Query().Get("desired_ttl_seconds"); got != "20" {
				t.Errorf("desired_ttl_seconds = %q", got)
			}
			writeEnvironmentWorkerJSON(t, w, environmentHeartbeatFixture("2026-09-03T00:00:01Z", "active", true, 30))
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events/stream"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher := w.(http.Flusher)
			flusher.Flush()
			select {
			case <-posted:
				fmt.Fprint(w, "event: session.status_terminated\ndata: {\"id\":\"terminated_one\",\"type\":\"session.status_terminated\",\"processed_at\":null}\n\n")
				flusher.Flush()
			case <-request.Context().Done():
			}
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events"):
			writeEnvironmentWorkerJSON(t, w, map[string]any{
				"data":      []json.RawMessage{sessionToolUseJSON("tool_one", "echo", "allow", "thread_one")},
				"next_page": nil,
			})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events"):
			postedOnce.Do(func() { close(posted) })
			writeEnvironmentWorkerJSON(t, w, map[string]any{"data": []any{}})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/stop"):
			stops.Add(1)
			var body EnvironmentWorkStopRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode Stop: %v", err)
			}
			if force, ok := body.Force.Get(); !ok || !force {
				t.Errorf("Stop force = %v, %v", force, ok)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	worker := NewEnvironmentWorker(newEnvironmentWorkerClient(t, server.URL, "workspace-key"), EnvironmentWorkerOptions{
		Tools: []SessionTool{sessionToolFunc{name: "echo", run: func(_ context.Context, call SessionToolCall) ([]ResultContentInput, error) {
			executions.Add(1)
			if call.SessionID != "session_one" || call.ToolUseID != "tool_one" {
				t.Errorf("tool call = %#v", call)
			}
			return textSessionToolResult("done"), nil
		}}},
		MaxIdle: durationPointer(0), DesiredTTLSeconds: Some(int64(20)),
	})
	err := worker.HandleItem(context.Background(), EnvironmentWorkerHandleItemOptions{
		WorkID: "work_one", EnvironmentID: "env_one", SessionID: "session_one",
		WorkSecret: encodeEnvironmentWorkSecret(t, token),
	})
	if err != nil {
		t.Fatalf("HandleItem: %v", err)
	}
	if executions.Load() != 1 || heartbeats.Load() != 1 || stops.Load() != 1 {
		t.Fatalf("executions=%d heartbeats=%d stops=%d", executions.Load(), heartbeats.Load(), stops.Load())
	}
}

func TestEnvironmentWorkerRunKeepsSupervisorCredentialOutOfItemRequests(t *testing.T) {
	t.Parallel()
	const (
		supervisor = "workspace-supervisor"
		itemToken  = "sess_mango_item"
	)
	var polls atomic.Int32
	var itemCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/work/poll"):
			if got := request.Header.Get("Authorization"); got != "Bearer "+supervisor {
				t.Errorf("Poll Authorization = %q", got)
			}
			if polls.Add(1) == 1 {
				work := workPollerFixture("queued")
				work["secret"] = encodeEnvironmentWorkSecret(t, itemToken)
				writeEnvironmentWorkerJSON(t, w, work)
				return
			}
			writeEnvironmentWorkerJSON(t, w, map[string]any{})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/ack"):
			if got := request.Header.Get("Authorization"); got != "Bearer "+supervisor {
				t.Errorf("Ack Authorization = %q", got)
			}
			writeEnvironmentWorkerJSON(t, w, workPollerFixture("starting"))
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/heartbeat"):
			itemCalls.Add(1)
			if got := request.Header.Get("Authorization"); got != "Bearer "+itemToken {
				t.Errorf("Heartbeat Authorization = %q", got)
			}
			writeEnvironmentWorkerJSON(t, w, environmentHeartbeatFixture("2026-09-03T00:00:01Z", "stopping", true, 30))
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/stop"):
			itemCalls.Add(1)
			if got := request.Header.Get("Authorization"); got != "Bearer "+itemToken {
				t.Errorf("Stop Authorization = %q", got)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	worker := NewEnvironmentWorker(newEnvironmentWorkerClient(t, server.URL, supervisor), EnvironmentWorkerOptions{
		EnvironmentID: "env_test", WorkerID: "worker_test", Drain: true,
	})
	if err := worker.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if polls.Load() != 2 || itemCalls.Load() != 2 {
		t.Fatalf("polls=%d item calls=%d", polls.Load(), itemCalls.Load())
	}
}

func TestEnvironmentWorkerLeaseLossCancelsToolWithoutResultOrStop(t *testing.T) {
	t.Parallel()
	toolStarted := make(chan struct{})
	toolCancelled := make(chan struct{})
	var heartbeats atomic.Int32
	var posts atomic.Int32
	var stops atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/heartbeat"):
			if heartbeats.Add(1) == 1 {
				writeEnvironmentWorkerJSON(t, w, environmentHeartbeatFixture("2026-09-03T00:00:01Z", "active", true, 1))
				return
			}
			<-toolStarted
			if got := request.URL.Query().Get("expected_last_heartbeat"); got != "2026-09-03T00:00:01Z" {
				t.Errorf("second expected_last_heartbeat = %q", got)
			}
			w.WriteHeader(http.StatusPreconditionFailed)
			fmt.Fprint(w, `{"error":{"type":"lease_lost","message":"reclaimed"}}`)
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events/stream"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-request.Context().Done()
		case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/events"):
			writeEnvironmentWorkerJSON(t, w, map[string]any{
				"data":      []json.RawMessage{sessionToolUseJSON("tool_slow", "slow", "allow", "")},
				"next_page": nil,
			})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events"):
			posts.Add(1)
			writeEnvironmentWorkerJSON(t, w, map[string]any{"data": []any{}})
		case request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/stop"):
			stops.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	worker := NewEnvironmentWorker(newEnvironmentWorkerClient(t, server.URL, "workspace-key"), EnvironmentWorkerOptions{
		Tools: []SessionTool{sessionToolFunc{name: "slow", run: func(ctx context.Context, _ SessionToolCall) ([]ResultContentInput, error) {
			close(toolStarted)
			<-ctx.Done()
			close(toolCancelled)
			return nil, ctx.Err()
		}}},
		MaxIdle: durationPointer(0),
	})
	worker.heartbeatFloor = 5 * time.Millisecond
	worker.heartbeatCeiling = 10 * time.Millisecond
	err := worker.HandleItem(context.Background(), EnvironmentWorkerHandleItemOptions{
		WorkID: "work_lost", EnvironmentID: "env_lost", SessionID: "session_lost",
		WorkSecret: encodeEnvironmentWorkSecret(t, "sess_mango_lost"),
	})
	if !errors.Is(err, ErrEnvironmentWorkLeaseLost) {
		t.Fatalf("HandleItem error = %v, want ErrEnvironmentWorkLeaseLost", err)
	}
	select {
	case <-toolCancelled:
	case <-time.After(time.Second):
		t.Fatal("tool context was not cancelled")
	}
	if posts.Load() != 0 || stops.Load() != 0 {
		t.Fatalf("posts=%d stops=%d after lease loss", posts.Load(), stops.Load())
	}
}

func TestEnvironmentWorkerBoundsNeverSuccessfulHeartbeatByLeaseTTL(t *testing.T) {
	t.Parallel()
	var heartbeats atomic.Int32
	var stops atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/heartbeat"):
			heartbeats.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, `{"error":{"type":"server_error","message":"unavailable"}}`)
		case strings.HasSuffix(request.URL.Path, "/stop"):
			stops.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	worker := NewEnvironmentWorker(newEnvironmentWorkerClient(t, server.URL, "workspace-key"), EnvironmentWorkerOptions{})
	worker.initialLeaseTTL = 40 * time.Millisecond
	worker.heartbeatFloor = 5 * time.Millisecond
	worker.heartbeatCeiling = 10 * time.Millisecond
	worker.retryDelay = func(int) time.Duration { return time.Millisecond }
	started := time.Now()
	err := worker.HandleItem(context.Background(), EnvironmentWorkerHandleItemOptions{
		WorkID: "work_stale", EnvironmentID: "env_stale", SessionID: "session_stale",
		WorkSecret: encodeEnvironmentWorkSecret(t, "sess_mango_stale"),
	})
	if !errors.Is(err, ErrEnvironmentWorkLeaseLost) {
		t.Fatalf("HandleItem error = %v, want ErrEnvironmentWorkLeaseLost", err)
	}
	if elapsed := time.Since(started); elapsed >= 300*time.Millisecond {
		t.Fatalf("heartbeat staleness ceiling took %s", elapsed)
	}
	if heartbeats.Load() < 1 || stops.Load() != 0 {
		t.Fatalf("heartbeats=%d stops=%d", heartbeats.Load(), stops.Load())
	}
}

func TestEnvironmentWorkerCancellationStillForceStopsWithFreshContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	var stops atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case strings.HasSuffix(request.URL.Path, "/heartbeat"):
			writeEnvironmentWorkerJSON(t, w, environmentHeartbeatFixture("2026-09-03T00:00:01Z", "active", true, 30))
		case strings.HasSuffix(request.URL.Path, "/events/stream"):
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			cancel()
			<-request.Context().Done()
		case strings.HasSuffix(request.URL.Path, "/events"):
			writeEnvironmentWorkerJSON(t, w, map[string]any{"data": []any{}, "next_page": nil})
		case strings.HasSuffix(request.URL.Path, "/stop"):
			if request.Context().Err() != nil {
				t.Errorf("Stop inherited cancelled context: %v", request.Context().Err())
			}
			stops.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	worker := NewEnvironmentWorker(newEnvironmentWorkerClient(t, server.URL, "workspace-key"), EnvironmentWorkerOptions{
		MaxIdle: durationPointer(0),
	})
	err := worker.HandleItem(ctx, EnvironmentWorkerHandleItemOptions{
		WorkID: "work_cancel", EnvironmentID: "env_cancel", SessionID: "session_cancel",
		WorkSecret: encodeEnvironmentWorkSecret(t, "sess_mango_cancel"),
	})
	if err != nil {
		t.Fatalf("HandleItem after cancellation: %v", err)
	}
	if stops.Load() != 1 {
		t.Fatalf("stops = %d, want 1", stops.Load())
	}
}

func TestEnvironmentWorkerHandleItemReadsLauncherEnvironment(t *testing.T) {
	t.Setenv("MANGO_WORK_ID", "work_env")
	t.Setenv("MANGO_ENVIRONMENT_ID", "env_env")
	t.Setenv("MANGO_SESSION_ID", "session_env")
	t.Setenv("MANGO_WORK_SECRET", encodeEnvironmentWorkSecret(t, "sess_mango_env"))
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		paths = append(paths, request.URL.Path)
		if got := request.Header.Get("Authorization"); got != "Bearer sess_mango_env" {
			t.Errorf("Authorization = %q", got)
		}
		switch {
		case strings.HasSuffix(request.URL.Path, "/heartbeat"):
			writeEnvironmentWorkerJSON(t, w, environmentHeartbeatFixture("2026-09-03T00:00:01Z", "stopping", true, 30))
		case strings.HasSuffix(request.URL.Path, "/stop"):
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	worker := NewEnvironmentWorker(newEnvironmentWorkerClient(t, server.URL, ""), EnvironmentWorkerOptions{})
	if err := worker.HandleItem(context.Background(), EnvironmentWorkerHandleItemOptions{}); err != nil {
		t.Fatalf("HandleItem: %v", err)
	}
	want := []string{
		"/v1/environments/env_env/work/work_env/heartbeat",
		"/v1/environments/env_env/work/work_env/stop",
	}
	if strings.Join(paths, ",") != strings.Join(want, ",") {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
}

func TestEnvironmentWorkerRejectsInvalidConfigurationAndSecrets(t *testing.T) {
	t.Parallel()
	client := newEnvironmentWorkerClient(t, "http://mango.test", "workspace-key")
	if err := NewEnvironmentWorker(nil, EnvironmentWorkerOptions{EnvironmentID: "env"}).Run(context.Background()); err == nil {
		t.Fatal("nil client was accepted")
	}
	if err := NewEnvironmentWorker(client, EnvironmentWorkerOptions{}).Run(context.Background()); err == nil {
		t.Fatal("empty EnvironmentID was accepted")
	}
	if err := NewEnvironmentWorker(client, EnvironmentWorkerOptions{
		EnvironmentID: "env", DesiredTTLSeconds: Some(int64(301)),
	}).Run(context.Background()); err == nil {
		t.Fatal("invalid desired TTL was accepted")
	}
	worker := NewEnvironmentWorker(client, EnvironmentWorkerOptions{})
	if err := worker.HandleItem(context.Background(), EnvironmentWorkerHandleItemOptions{}); err == nil {
		t.Fatal("empty item identity was accepted")
	}
	if err := worker.HandleItem(context.Background(), EnvironmentWorkerHandleItemOptions{
		WorkID: "work", EnvironmentID: "env", SessionID: "session", WorkSecret: "not-secret",
	}); err == nil || strings.Contains(err.Error(), "sess_mango") {
		t.Fatalf("invalid secret error = %v", err)
	}
}

func TestSessionTokenFromWorkSecret(t *testing.T) {
	t.Parallel()
	valid := base64.URLEncoding.EncodeToString([]byte(`{"sessions_token":"sess_mango_test","other":"ignored"}`))
	if token, err := sessionTokenFromWorkSecret(valid); err != nil || token != "sess_mango_test" {
		t.Fatalf("token=%q error=%v", token, err)
	}
	for _, secret := range []string{"", "***", base64.RawURLEncoding.EncodeToString([]byte(`{}`)), base64.RawURLEncoding.EncodeToString([]byte(`[]`))} {
		if token, err := sessionTokenFromWorkSecret(secret); err == nil || token != "" {
			t.Fatalf("secret %q token=%q error=%v", secret, token, err)
		}
	}
}

func newEnvironmentWorkerClient(t *testing.T, baseURL, apiKey string) *Client {
	t.Helper()
	client, err := New(Config{BaseURL: baseURL, APIKey: apiKey, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func encodeEnvironmentWorkSecret(t *testing.T, token string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"sessions_token": token})
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(payload)
}

func environmentHeartbeatFixture(last, state string, extended bool, ttl int64) map[string]any {
	return map[string]any{
		"type": "work_heartbeat", "last_heartbeat": last, "state": state,
		"lease_extended": extended, "ttl_seconds": ttl,
	}
}

func writeEnvironmentWorkerJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
