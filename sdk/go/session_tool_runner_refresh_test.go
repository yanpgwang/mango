package mango

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The SSE endpoint deliberately never sends a frame or EOF. Finite history and
// result requests remain healthy, as when a stale live connection loses traffic.
type silentSessionStream struct {
	streams            atomic.Int32
	lists              atomic.Int32
	posts              atomic.Int32
	approved           atomic.Bool
	activeReads        atomic.Int32
	openBodies         atomic.Int32
	overlappingReaders atomic.Bool
	history            func() []json.RawMessage
	historyStatus      func() int
}

func (s *silentSessionStream) client(t *testing.T) *Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events/stream"):
			s.streams.Add(1)
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			if calls := s.lists.Add(1); s.streams.Load() < calls {
				t.Error("history read preceded its fresh live subscription")
			}
			if s.historyStatus != nil {
				if status := s.historyStatus(); status != http.StatusOK {
					http.Error(w, "temporary history failure", status)
					return
				}
			}
			writeSessionToolRunnerJSON(t, w, map[string]any{"data": s.history(), "next_page": nil})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events"):
			s.posts.Add(1)
			writeSessionToolRunnerJSON(t, w, map[string]any{"data": []any{}})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	transport := &sessionStreamReadTracker{base: http.DefaultTransport, state: s}
	client, err := New(Config{BaseURL: server.URL, HTTPClient: &http.Client{Transport: transport}, RequestTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

type sessionStreamReadTracker struct {
	base  http.RoundTripper
	state *silentSessionStream
}

func (t *sessionStreamReadTracker) RoundTrip(r *http.Request) (*http.Response, error) {
	stream := strings.HasSuffix(r.URL.Path, "/events/stream")
	if stream && t.state.activeReads.Load() != 0 {
		t.state.overlappingReaders.Store(true)
	}
	response, err := t.base.RoundTrip(r)
	if err == nil && stream {
		t.state.openBodies.Add(1)
		response.Body = &sessionStreamTrackedBody{ReadCloser: response.Body, state: t.state}
	}
	return response, err
}

type sessionStreamTrackedBody struct {
	io.ReadCloser
	state  *silentSessionStream
	closed atomic.Bool
}

func (b *sessionStreamTrackedBody) Read(p []byte) (int, error) {
	b.state.activeReads.Add(1)
	defer b.state.activeReads.Add(-1)
	return b.ReadCloser.Read(p)
}

func (b *sessionStreamTrackedBody) Close() error {
	if b.closed.CompareAndSwap(false, true) {
		b.state.openBodies.Add(-1)
	}
	return b.ReadCloser.Close()
}

func awaitSessionRefresh(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-deadline.C:
			t.Fatal("silent stream did not recover through durable history")
		case <-tick.C:
		}
	}
}

func TestSessionToolRunnerRefreshesSilentApprovalWithoutRepeatingTool(t *testing.T) {
	t.Parallel()
	fixture := &silentSessionStream{}
	tool := sessionToolUseJSON("tool_silent", "echo", "ask", "")
	confirmation := json.RawMessage(`{"id":"approval_silent","type":"user.tool_confirmation","processed_at":null,"tool_use_id":"tool_silent","result":"allow"}`)
	fixture.history = func() []json.RawMessage {
		events := []json.RawMessage{tool, json.RawMessage(`{"id":"waiting_silent","type":"session.status_idle","processed_at":null,"stop_reason":{"type":"requires_action","event_ids":["tool_silent"]}}`)}
		if fixture.approved.Load() {
			events = append(events, confirmation)
		}
		if fixture.posts.Load() > 0 {
			events = append(events, json.RawMessage(`{"id":"result_silent","type":"user.tool_result","processed_at":null,"tool_use_id":"tool_silent","content":[]}`))
		}
		return events
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := make(chan context.Context, 1)
	release := make(chan struct{})
	var executions atomic.Int32
	maxIdle := 5 * time.Millisecond
	runner := NewSessionToolRunner(ctx, fixture.client(t), "session_silent", SessionToolRunnerOptions{
		Tools: []SessionTool{sessionToolFunc{name: "echo", run: func(ctx context.Context, _ SessionToolCall) ([]ResultContentInput, error) {
			executions.Add(1)
			started <- ctx
			select {
			case <-release:
				return textSessionToolResult("done"), nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}}}, MaxIdle: &maxIdle,
	})
	runner.streamRefreshInterval = 10 * time.Millisecond
	t.Cleanup(func() { _ = runner.Close() })
	next := make(chan bool, 1)
	go func() { next <- runner.Next() }()
	// Quiet approval waits remain valid across multiple refreshes and MaxIdle.
	awaitSessionRefresh(t, func() bool { return fixture.lists.Load() >= 3 })
	if executions.Load() != 0 || runner.Err() != nil {
		t.Fatalf("quiet approval executed or ended: executions=%d err=%v", executions.Load(), runner.Err())
	}
	fixture.approved.Store(true)
	var toolCtx context.Context
	select {
	case toolCtx = <-started:
	case <-ctx.Done():
		t.Fatal("durable approval was not discovered")
	}
	lists := fixture.lists.Load()
	awaitSessionRefresh(t, func() bool { return fixture.lists.Load() >= lists+3 })
	if toolCtx.Err() != nil || executions.Load() != 1 {
		t.Fatalf("refresh canceled or repeated running tool: ctx=%v executions=%d", toolCtx.Err(), executions.Load())
	}
	close(release)
	select {
	case ok := <-next:
		if !ok || !runner.Current().Posted {
			t.Fatalf("result not delivered: next=%v err=%v", ok, runner.Err())
		}
	case <-ctx.Done():
		t.Fatal("tool result not delivered")
	}
	lists = fixture.lists.Load()
	awaitSessionRefresh(t, func() bool { return fixture.lists.Load() >= lists+3 })
	if executions.Load() != 1 || fixture.posts.Load() != 1 {
		t.Fatalf("resolved tool repeated: executions=%d posts=%d", executions.Load(), fixture.posts.Load())
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	if fixture.activeReads.Load() != 0 || fixture.openBodies.Load() != 0 || fixture.overlappingReaders.Load() {
		t.Fatalf("stream readers were not joined: reads=%d bodies=%d overlap=%v", fixture.activeReads.Load(), fixture.openBodies.Load(), fixture.overlappingReaders.Load())
	}
}

func TestSessionToolRunnerRefreshPreservesIdleDeadline(t *testing.T) {
	t.Parallel()
	fixture := &silentSessionStream{history: func() []json.RawMessage {
		return []json.RawMessage{json.RawMessage(`{"id":"idle_unchanged","type":"session.status_idle","processed_at":null,"stop_reason":{"type":"end_turn"}}`)}
	}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	maxIdle := 80 * time.Millisecond
	runner := NewSessionToolRunner(ctx, fixture.client(t), "session_idle_refresh", SessionToolRunnerOptions{MaxIdle: &maxIdle})
	runner.streamRefreshInterval = 10 * time.Millisecond
	defer runner.Close()
	if runner.Next() || !errors.Is(runner.Err(), ErrIdleTimeout) {
		t.Fatalf("replayed idle boundary reset deadline: %v", runner.Err())
	}
	if fixture.streams.Load() < 2 {
		t.Fatal("idle deadline was not tested across connection refreshes")
	}
}

func TestSessionToolRunnerRefreshCancellationJoinsSilentReader(t *testing.T) {
	t.Parallel()
	fixture := &silentSessionStream{history: func() []json.RawMessage { return []json.RawMessage{} }}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := NewSessionToolRunner(ctx, fixture.client(t), "session_refresh_cancel", SessionToolRunnerOptions{MaxIdle: durationPointer(0)})
	runner.streamRefreshInterval = 10 * time.Millisecond
	t.Cleanup(func() { _ = runner.Close() })
	done := make(chan struct{})
	go func() { runner.Next(); close(done) }()
	awaitSessionRefresh(t, func() bool { return fixture.lists.Load() >= 3 })
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not join silent reader")
	}
	if fixture.activeReads.Load() != 0 || fixture.openBodies.Load() != 0 || fixture.overlappingReaders.Load() {
		t.Fatalf("reader leaked or overlapped: reads=%d bodies=%d overlap=%v", fixture.activeReads.Load(), fixture.openBodies.Load(), fixture.overlappingReaders.Load())
	}
}

func TestSessionToolRunnerRefreshWaitsForCompleteHistoryAfterFailure(t *testing.T) {
	t.Parallel()
	var phase atomic.Int32
	fixture := &silentSessionStream{
		history: func() []json.RawMessage {
			if phase.Load() < 2 {
				return []json.RawMessage{json.RawMessage(`{"id":"idle_before_outage","type":"session.status_idle","processed_at":null,"stop_reason":{"type":"end_turn"}}`)}
			}
			return []json.RawMessage{
				sessionToolUseJSON("tool_after_outage", "echo", "allow", ""),
				json.RawMessage(`{"id":"running_after_outage","type":"session.status_running","processed_at":null}`),
			}
		},
		historyStatus: func() int {
			if phase.Load() == 1 {
				return http.StatusServiceUnavailable
			}
			return http.StatusOK
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	maxIdle := 40 * time.Millisecond
	runner := NewSessionToolRunner(ctx, fixture.client(t), "session_history_outage", SessionToolRunnerOptions{
		Tools: []SessionTool{sessionToolFunc{name: "echo", run: func(context.Context, SessionToolCall) ([]ResultContentInput, error) {
			return textSessionToolResult("recovered"), nil
		}}}, MaxIdle: &maxIdle,
	})
	runner.streamRefreshInterval = 10 * time.Millisecond
	runner.retryDelay = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { _ = runner.Close() })
	next := make(chan bool, 1)
	go func() { next <- runner.Next() }()
	awaitSessionRefresh(t, func() bool { return !runner.idle.snapshot().IsZero() })
	phase.Store(1)
	awaitSessionRefresh(t, func() bool { return fixture.lists.Load() >= 2 })
	select {
	case ok := <-next:
		t.Fatalf("stale idle expired while history was unavailable: next=%v err=%v", ok, runner.Err())
	case <-time.After(3 * maxIdle):
	}
	phase.Store(2)
	select {
	case ok := <-next:
		if !ok || runner.Current().ToolUseID != "tool_after_outage" || !runner.Current().Posted {
			t.Fatalf("new durable activity was lost: next=%v err=%v", ok, runner.Err())
		}
	case <-ctx.Done():
		t.Fatal("new activity did not recover after history became available")
	}
}
