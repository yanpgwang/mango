package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Mirrors the workshop agent_complete.py protocol flow (custom-tool handoff),
// driven by the deterministic fake runtime. No real model, no Files API.
func TestWorkshop_CustomToolHandoff(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents",
		`{"name":"SRE Agent","model":"claude-opus-4-8","tools":[{"type":"custom","name":"get_metrics","description":"d","input_schema":{"type":"object"}}]}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"self_hosted"}}`)
	sess := createID(t, h, "POST", "/v1/sessions", `{"agent":"`+ag+`","environment_id":"`+env+`"}`)

	// Send a message that triggers the tool.
	rec := do(h, "POST", "/v1/sessions/"+sess+"/events",
		`{"events":[{"type":"user.message","content":[{"type":"text","text":"tool: get_metrics"}]}]}`)
	if rec.Code != 200 {
		t.Fatalf("send: %d %s", rec.Code, rec.Body)
	}
	// Poll history until agent.custom_tool_use appears; capture its id.
	var toolUseID string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && toolUseID == "" {
		hrec := do(h, "GET", "/v1/sessions/"+sess+"/events", "")
		var hist map[string]any
		if err := json.Unmarshal(hrec.Body.Bytes(), &hist); err != nil {
			t.Fatalf("unmarshal history: %v", err)
		}
		for _, raw := range hist["data"].([]any) {
			e := raw.(map[string]any)
			if e["type"] == "agent.custom_tool_use" {
				toolUseID = e["id"].(string)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if toolUseID == "" {
		t.Fatal("never saw agent.custom_tool_use")
	}

	// Return the custom tool result -> fake emits agent.message + status_idle.
	rec = do(h, "POST", "/v1/sessions/"+sess+"/events",
		`{"events":[{"type":"user.custom_tool_result","custom_tool_use_id":"`+toolUseID+`","content":[{"type":"text","text":"cpu 99%"}]}]}`)
	if rec.Code != 200 {
		t.Fatalf("tool result: %d %s", rec.Code, rec.Body)
	}

	// Session ends idle/end_turn.
	deadline = time.Now().Add(3 * time.Second)
	ended := false
	for time.Now().Before(deadline) && !ended {
		grec := do(h, "GET", "/v1/sessions/"+sess, "")
		var s map[string]any
		if err := json.Unmarshal(grec.Body.Bytes(), &s); err != nil {
			t.Fatalf("unmarshal session: %v", err)
		}
		if s["status"] == "idle" {
			ended = true
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ended {
		t.Fatal("session did not return to idle after tool result")
	}

	// Verify agent.message appeared after the tool result (fake emits it before status_idle).
	finalHrec := do(h, "GET", "/v1/sessions/"+sess+"/events", "")
	var finalHist map[string]any
	if err := json.Unmarshal(finalHrec.Body.Bytes(), &finalHist); err != nil {
		t.Fatalf("unmarshal final history: %v", err)
	}
	foundAgentMessage := false
	for _, raw := range finalHist["data"].([]any) {
		e := raw.(map[string]any)
		if e["type"] == "agent.message" {
			foundAgentMessage = true
			break
		}
	}
	if !foundAgentMessage {
		t.Fatal("no agent.message event found in history after custom_tool_result")
	}

	// delete session (workshop step 7)
	if rec := do(h, "DELETE", "/v1/sessions/"+sess, ""); rec.Code != 200 {
		t.Fatalf("delete: %d", rec.Code)
	}
}

// Proves the documented reconnect pattern: open stream first, list history,
// merge by id with no gaps and no duplicates.
func TestWorkshop_ReconnectNoGapNoDup(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"self_hosted"}}`)
	sess := createID(t, h, "POST", "/v1/sessions", `{"agent":"`+ag+`","environment_id":"`+env+`"}`)

	ts := httptest.NewServer(h)
	defer ts.Close()

	// Open the live stream FIRST.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/sessions/"+sess+"/events/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, resp.Body)

	streamIDs := make(chan string, 64)
	framingErrors := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		var eventType string
		for sc.Scan() {
			line := sc.Text()
			if strings.HasPrefix(line, "id:") {
				select {
				case framingErrors <- line:
				default:
				}
			}
			if strings.HasPrefix(line, "event: ") {
				eventType = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				var event map[string]any
				if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil {
					if event["type"] != eventType {
						select {
						case framingErrors <- "event/data type mismatch":
						default:
						}
					}
					if id, ok := event["id"].(string); ok {
						streamIDs <- id
					}
				}
				eventType = ""
			}
		}
	}()

	// Now generate events.
	do(h, "POST", "/v1/sessions/"+sess+"/events",
		`{"events":[{"type":"user.message","content":[{"type":"text","text":"hello"}]}]}`)

	// Collect stream ids for a moment.
	seen := map[string]bool{}
	timeout := time.After(2 * time.Second)
	collecting := true
	for collecting {
		select {
		case id := <-streamIDs:
			if seen[id] {
				t.Fatalf("duplicate event id on stream: %s", id)
			}
			seen[id] = true
		case <-timeout:
			collecting = false
		}
	}

	// List history and merge; union must have no duplicate ids and cover history.
	hrec := do(h, "GET", "/v1/sessions/"+sess+"/events", "")
	var hist map[string]any
	if err := json.Unmarshal(hrec.Body.Bytes(), &hist); err != nil {
		t.Fatalf("unmarshal history: %v", err)
	}
	histIDs := map[string]bool{}
	for _, raw := range hist["data"].([]any) {
		e := raw.(map[string]any)
		histIDs[e["id"].(string)] = true
	}
	if len(histIDs) == 0 {
		t.Fatal("history empty")
	}
	// Every event seen on the live stream must be resolvable in history
	// (persisted) — proves the stream delivered nothing history lacks.
	for id := range seen {
		if !histIDs[id] {
			t.Errorf("stream id %s not found in history", id)
		}
	}
	// Every persisted history event must have appeared on the stream that was
	// opened before the events were generated — proves no gap (no committed
	// event was missed by a stream open at subscribe time).
	for id := range histIDs {
		if !seen[id] {
			t.Errorf("history id %s missing from live stream (gap)", id)
		}
	}
	select {
	case line := <-framingErrors:
		t.Errorf("invalid SSE framing: %q", line)
	default:
	}
}

func TestStreamEvents_DeleteTerminalAndEOF(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"self_hosted"}}`)
	sess := createID(t, h, "POST", "/v1/sessions", `{"agent":"`+ag+`","environment_id":"`+env+`"}`)

	ts := httptest.NewServer(h)
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/v1/sessions/" + sess + "/events/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, resp.Body)

	if rec := do(h, "DELETE", "/v1/sessions/"+sess, ""); rec.Code != http.StatusOK {
		t.Fatalf("delete: %d: %s", rec.Code, rec.Body)
	}

	scanner := bufio.NewScanner(resp.Body)
	if !scanner.Scan() {
		t.Fatalf("stream ended before session.deleted: %v", scanner.Err())
	}
	line := scanner.Text()
	if line != "event: session.deleted" {
		t.Fatalf("terminal frame event type = %q", line)
	}
	if !scanner.Scan() {
		t.Fatalf("stream ended before terminal data: %v", scanner.Err())
	}
	line = scanner.Text()
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("terminal frame has no data field: %q", line)
	}
	var terminal map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &terminal); err != nil {
		t.Fatalf("decode terminal event: %v", err)
	}
	if terminal["type"] != "session.deleted" || terminal["id"] == "" || terminal["processed_at"] == nil {
		t.Fatalf("terminal event = %#v", terminal)
	}
	// Consume the blank delimiter, then the handler must close the response.
	if !scanner.Scan() || scanner.Text() != "" {
		t.Fatalf("missing SSE frame delimiter")
	}
	if scanner.Scan() {
		t.Fatalf("stream emitted data after terminal event: %q", scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("stream read: %v", err)
	}
}
