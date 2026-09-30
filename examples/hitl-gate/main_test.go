package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// This fixture exercises the tutorial's SDK requests and local recovery only.
// Runtime persistence, barrier admission, and recovery have independent tests.
type gateServer struct {
	mu               sync.Mutex
	t                *testing.T
	statePath        string
	history          []map[string]any
	creates, prompts int
	results          map[string]int
	ambiguous        string
	unaccepted       string
	createFailure    string
	cleanupFailure   bool
	deletes          []string
}

func newGateServer(t *testing.T) (*gateServer, *httptest.Server, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gate.json")
	f := &gateServer{t: t, statePath: path, results: map[string]int{}}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	t.Setenv("MANGO_EXAMPLE_BASE_URL", server.URL)
	t.Setenv("MANGO_API_KEY", "test-secret-must-not-be-saved")
	t.Setenv("MANGO_EXAMPLE_MODEL_ID", "test-model")
	return f, server, path
}

func (f *gateServer) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if r.Header.Get("Authorization") != "Bearer test-secret-must-not-be-saved" {
		f.t.Error("SDK request omitted workspace authentication")
	}
	write := func(v any) {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			f.t.Error(err)
		}
	}
	if r.Method == "POST" && (r.URL.Path == "/v1/environments" || r.URL.Path == "/v1/agents" || r.URL.Path == "/v1/sessions") {
		f.creates++
		if r.URL.Path == f.createFailure {
			w.WriteHeader(400)
			write(map[string]any{"error": map[string]string{"type": "invalid_request", "message": "creation rejected"}})
			return
		}
		ids := map[string]string{"/v1/environments": "env_1", "/v1/agents": "agent_1", "/v1/sessions": "session_1"}
		write(map[string]any{"id": ids[r.URL.Path]})
		return
	}
	if r.Method == "DELETE" || strings.HasSuffix(r.URL.Path, "/archive") {
		f.deletes = append(f.deletes, r.URL.Path)
		if f.cleanupFailure && r.URL.Path == "/v1/environments/env_1" {
			w.WriteHeader(503)
			write(map[string]any{"error": map[string]string{"type": "unavailable", "message": "delete unavailable"}})
			return
		}
		write(map[string]any{"id": "deleted", "deleted": true})
		return
	}
	if r.URL.Path != "/v1/sessions/session_1/events" {
		f.t.Errorf("unexpected route %s %s", r.Method, r.URL)
		w.WriteHeader(404)
		return
	}
	if r.Method == "GET" {
		if r.URL.Query().Get("order") != "asc" {
			f.t.Error("history must be ordered ascending")
		}
		start, _ := strconv.Atoi(r.URL.Query().Get("page"))
		end := min(start+100, len(f.history))
		var next any
		if end < len(f.history) {
			next = strconv.Itoa(end)
		}
		write(map[string]any{"data": f.history[start:end], "next_page": next})
		return
	}
	var body struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		f.t.Error(err)
		return
	}
	if len(body.Events) != 1 {
		f.t.Errorf("want one event, got %v", body.Events)
		return
	}
	event := body.Events[0]
	event["id"] = fmt.Sprintf("event_%d", len(f.history))
	event["processed_at"] = "2026-09-30T00:00:00Z"
	switch event["type"] {
	case "user.message":
		f.prompts++
		f.history = append(f.history, event,
			map[string]any{"id": "call_1", "type": "agent.custom_tool_use", "processed_at": "2026-09-30T00:00:00Z", "name": "decide", "input": map[string]any{"receipt_id": "r01", "action": "approve", "reason": "Within policy"}},
			map[string]any{"id": "call_2", "type": "agent.custom_tool_use", "processed_at": "2026-09-30T00:00:00Z", "name": "escalate", "input": map[string]any{"receipt_id": "r02", "question": "Approve the team activity?"}},
			map[string]any{"id": "idle_1", "type": "session.status_idle", "processed_at": "2026-09-30T00:00:00Z", "stop_reason": map[string]any{"type": "requires_action", "event_ids": []string{"call_1", "call_2"}}},
		)
		// Put result events beyond the old single-page limit of 1000.
		for i := 0; i < 1001; i++ {
			f.history = append(f.history, map[string]any{"id": fmt.Sprintf("update_%d", i), "type": "fixture.observation"})
		}
	case "user.custom_tool_result":
		id, _ := event["custom_tool_use_id"].(string)
		f.results[id]++
		data, err := os.ReadFile(f.statePath)
		if err != nil {
			f.t.Error(err)
			return
		}
		var state struct {
			Decisions map[string]json.RawMessage `json:"decisions"`
		}
		if err := json.Unmarshal(data, &state); err != nil {
			f.t.Error(err)
		}
		if len(state.Decisions[id]) == 0 {
			f.t.Errorf("result %s sent before durable local decision", id)
		}
		if strings.Contains(string(data), "test-secret") {
			f.t.Error("state contains credential")
		}
		if id == f.unaccepted {
			w.WriteHeader(503)
			write(map[string]any{"error": map[string]string{"type": "unavailable", "message": "not admitted"}})
			return
		}
		f.history = append(f.history, event)
		if len(f.results) == 2 {
			f.history = append(f.history,
				map[string]any{"id": "answer", "type": "agent.message", "processed_at": "2026-09-30T00:00:00Z", "content": []any{map[string]any{"type": "text", "text": "Expenses recorded."}}},
				map[string]any{"id": "idle_2", "type": "session.status_idle", "processed_at": "2026-09-30T00:00:00Z", "stop_reason": map[string]any{"type": "end_turn"}},
			)
		}
		if id == f.ambiguous {
			// The result is durable, but the successful response is truncated.
			_, _ = io.WriteString(w, `{"data":[`)
			return
		}
	default:
		f.t.Errorf("unexpected event %v", event)
	}
	write(map[string]any{"data": []any{event}})
}

func invoke(t *testing.T, path, command, input string, flags ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out bytes.Buffer
	args := append([]string{command, "-state", path}, flags...)
	err := runCommand(ctx, args, strings.NewReader(input), &out)
	return out.String(), err
}

func TestStartThenResumePartialGate(t *testing.T) {
	f, _, path := newGateServer(t)
	if _, err := invoke(t, path, "start", "", "-stop-after-first-result"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MANGO_EXAMPLE_MODEL_ID", "") // Resume needs no model configuration.
	out, err := invoke(t, path, "resume", "reject\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Expenses recorded.") {
		t.Fatalf("missing final response: %s", out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 3 || f.prompts != 1 || f.results["call_1"] != 1 || f.results["call_2"] != 1 {
		t.Fatalf("restart repeated work: creates=%d prompts=%d results=%v", f.creates, f.prompts, f.results)
	}
}

func TestAmbiguousAcceptedResultIsReconciledWithoutAskingAgain(t *testing.T) {
	f, _, path := newGateServer(t)
	f.ambiguous = "call_2"
	if _, err := invoke(t, path, "start", "reject\n"); err == nil {
		t.Fatal("expected ambiguous send error")
	}
	out, err := invoke(t, path, "resume", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Decision [") {
		t.Fatal("asked for a durable decision again")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.results["call_2"] != 1 {
		t.Fatalf("duplicated accepted result: %v", f.results)
	}
}

func TestStartRefusesExistingStateAndResumeRefusesDifferentServer(t *testing.T) {
	_, _, path := newGateServer(t)
	if _, err := invoke(t, path, "start", "", "-stop-after-first-result"); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, path, "start", ""); err == nil {
		t.Fatal("start overwrote state")
	}
	t.Setenv("MANGO_EXAMPLE_BASE_URL", "http://127.0.0.1:1")
	if _, err := invoke(t, path, "resume", ""); err == nil || !strings.Contains(err.Error(), "server") {
		t.Fatalf("want server mismatch: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed command changed state")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions %o", info.Mode().Perm())
	}
}

func TestFailedSetupKeepsKnownResourcesForCleanup(t *testing.T) {
	f, _, path := newGateServer(t)
	f.createFailure = "/v1/agents"
	if _, err := invoke(t, path, "start", ""); err == nil {
		t.Fatal("expected setup failure")
	}
	if _, err := invoke(t, path, "resume", ""); err == nil {
		t.Fatal("incomplete setup should need cleanup")
	}
	if _, err := invoke(t, path, "cleanup", ""); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.creates != 2 || len(f.deletes) != 1 || f.deletes[0] != "/v1/environments/env_1" {
		t.Fatalf("lost setup resources: %+v", f.deletes)
	}
}

func TestCleanupRetainsStateOnFailureAndCanBeRetried(t *testing.T) {
	f, _, path := newGateServer(t)
	if _, err := invoke(t, path, "start", "", "-stop-after-first-result"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.cleanupFailure = true
	f.mu.Unlock()
	if _, err := invoke(t, path, "cleanup", ""); err == nil {
		t.Fatal("expected cleanup failure")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lost cleanup state: %v", err)
	}
	if _, err := invoke(t, path, "resume", ""); err == nil {
		t.Fatal("resume accepted partially cleaned state")
	}
	f.mu.Lock()
	f.cleanupFailure = false
	f.mu.Unlock()
	if _, err := invoke(t, path, "cleanup", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("state remains after cleanup: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if fmt.Sprint(f.deletes) != "[/v1/sessions/session_1 /v1/agents/agent_1/archive /v1/environments/env_1 /v1/environments/env_1]" {
		t.Fatalf("cleanup not checkpointed: %v", f.deletes)
	}
}

func TestUnacceptedResultReusesSavedHumanDecision(t *testing.T) {
	f, _, path := newGateServer(t)
	f.unaccepted = "call_2"
	if _, err := invoke(t, path, "start", "reject\n"); err == nil {
		t.Fatal("expected send failure")
	}
	f.mu.Lock()
	f.unaccepted = ""
	f.mu.Unlock()
	out, err := invoke(t, path, "resume", "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "Decision [") || strings.Contains(out, "Recorded reject") {
		t.Fatalf("repeated human work: %s", out)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.results["call_2"] != 2 {
		t.Fatalf("result was not retried: %v", f.results)
	}
	for _, event := range f.history {
		if event["custom_tool_use_id"] == "call_2" {
			content := event["content"].([]any)[0].(map[string]any)["text"].(string)
			var result map[string]any
			if err := json.Unmarshal([]byte(content), &result); err != nil {
				t.Fatal(err)
			}
			if result["decision"] != "reject" || result["decided_by"] != "human" {
				t.Fatalf("lost recorded decision: %s", content)
			}
		}
	}
}

type cancelOnPrompt struct{ cancel context.CancelFunc }

func (w cancelOnPrompt) Write(data []byte) (int, error) {
	if bytes.Contains(data, []byte("Decision [")) {
		w.cancel()
	}
	return len(data), nil
}

func TestCancelWhileReadingPreservesUnresolvedGate(t *testing.T) {
	f, _, path := newGateServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	err := runCommand(ctx, []string{"start", "-state", path}, reader, cancelOnPrompt{cancel})
	if err != context.Canceled {
		t.Fatalf("input wait did not cancel: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := invoke(t, path, "resume", "approve\n"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.results["call_1"] != 1 || f.results["call_2"] != 1 || f.prompts != 1 {
		t.Fatalf("cancellation repeated work: %v", f.results)
	}
}
