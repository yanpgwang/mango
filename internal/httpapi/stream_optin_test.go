package httpapi

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestStreamEvents_EventDeltasValidation verifies the event_deltas[] opt-in
// contract: an unknown value is rejected 400, more than 100 values is rejected
// 400, and a valid value opens a 200 stream.
func TestStreamEvents_EventDeltasValidation(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"self_hosted"}}`)
	sess := createID(t, h, "POST", "/v1/sessions", `{"agent":"`+ag+`","environment_id":"`+env+`"}`)

	// Unknown value -> 400.
	if rec := do(h, "GET", "/v1/sessions/"+sess+"/events/stream?event_deltas[]=bogus", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bogus event_deltas[] -> %d: %s", rec.Code, rec.Body)
	}

	// 101 values -> 400.
	var b strings.Builder
	b.WriteString("/v1/sessions/" + sess + "/events/stream?")
	for i := 0; i < 101; i++ {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString("event_deltas[]=agent.message")
	}
	if rec := do(h, "GET", b.String(), ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("101 event_deltas[] -> %d: %s", rec.Code, rec.Body)
	}

	// A valid value opens a 200 stream. Use a real server so streaming flushes
	// the 200 header before the handler blocks reading frames.
	ts := httptest.NewServer(h)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	q := url.Values{}
	q.Add("event_deltas[]", "agent.message")
	q.Add("event_deltas[]", "agent.thinking")
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/sessions/"+sess+"/events/stream?"+q.Encode(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer closeTestResource(t, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid event_deltas[] stream -> %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("stream Content-Type = %q", ct)
	}
}

// TestStreamEvents_PreviewRenderedAsSSE proves that an opted-in HTTP stream
// client observes preview frames rendered as SSE (event_start + event_delta)
// followed by the persisted agent.message, and that a non-opted client sees
// only the persisted agent.message.
func TestStreamEvents_PreviewRenderedAsSSE(t *testing.T) {
	h := NewTestHandlerWithPreviews(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"self_hosted"}}`)
	sess := createID(t, h, "POST", "/v1/sessions", `{"agent":"`+ag+`","environment_id":"`+env+`"}`)

	ts := httptest.NewServer(h)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Opted-in stream.
	q := url.Values{}
	q.Add("event_deltas[]", "agent.message")
	optedReq, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/sessions/"+sess+"/events/stream?"+q.Encode(), nil)
	optedResp, err := http.DefaultClient.Do(optedReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := optedResp.Body.Close(); err != nil {
			t.Errorf("close opted-in stream: %v", err)
		}
	}()

	// Plain (non-opted) stream.
	plainReq, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/sessions/"+sess+"/events/stream", nil)
	plainResp, err := http.DefaultClient.Do(plainReq)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := plainResp.Body.Close(); err != nil {
			t.Errorf("close plain stream: %v", err)
		}
	}()

	// Drive a turn.
	do(h, "POST", "/v1/sessions/"+sess+"/events",
		`{"events":[{"type":"user.message","content":[{"type":"text","text":"hello"}]}]}`)

	optedFrames := collectSSE(optedResp.Body, "agent.message")
	var sawStart, sawDelta, sawMessage bool
	for _, fr := range optedFrames {
		switch fr.event {
		case "event_start":
			sawStart = true
		case "event_delta":
			sawDelta = true
		case "agent.message":
			sawMessage = true
		}
	}
	if !sawStart || !sawDelta {
		t.Fatalf("opted-in SSE missing preview frames: start=%v delta=%v", sawStart, sawDelta)
	}
	if !sawMessage {
		t.Fatal("opted-in SSE never saw persisted agent.message")
	}

	plainFrames := collectSSE(plainResp.Body, "agent.message")
	for _, fr := range plainFrames {
		if fr.event == "event_start" || fr.event == "event_delta" {
			t.Fatalf("non-opted SSE received a preview frame: %q", fr.event)
		}
	}
}

type sseFrame struct {
	event string
	data  string
}

// collectSSE reads SSE frames until it observes one whose event: line equals
// stopAt, or a short timeout elapses.
func collectSSE(body interface{ Read([]byte) (int, error) }, stopAt string) []sseFrame {
	type result struct{ frames []sseFrame }
	done := make(chan result, 1)
	go func() {
		var frames []sseFrame
		sc := bufio.NewScanner(body)
		var ev string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				frames = append(frames, sseFrame{event: ev, data: strings.TrimPrefix(line, "data: ")})
				if ev == stopAt {
					done <- result{frames}
					return
				}
				ev = ""
			}
		}
		done <- result{frames}
	}()
	select {
	case r := <-done:
		return r.frames
	case <-time.After(2 * time.Second):
		return nil
	}
}
