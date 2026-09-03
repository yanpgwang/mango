package mango

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWorkPollerClaimsThenDrainsWithoutChangingWorkState(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.RequestURI())
		call := len(calls)
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments/env_test/work/poll" && call == 1:
			writeWorkPollerJSON(t, w, workPollerFixture("queued"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments/env_test/work/work_test/ack":
			writeWorkPollerJSON(t, w, workPollerFixture("starting"))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments/env_test/work/poll":
			writeWorkPollerJSON(t, w, map[string]any{})
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := newWorkPollerTestClient(t, server.URL)
	poller := NewWorkPoller(context.Background(), client, WorkPollerOptions{
		EnvironmentID:      "env_test",
		WorkerID:           "worker_test",
		Drain:              true,
		ReclaimOlderThanMs: Some(int64(2500)),
	})
	if !poller.Next() {
		t.Fatalf("Next() = false, error = %v", poller.Err())
	}
	if got := poller.Current(); got == nil || got.ID != "work_test" || got.State != EnvironmentWorkStateStarting {
		t.Fatalf("Current() = %#v", got)
	}
	if poller.Next() {
		t.Fatal("Next() = true after queue drained")
	}
	if err := poller.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"GET /v1/environments/env_test/work/poll?reclaim_older_than_ms=2500&worker_id=worker_test",
		"POST /v1/environments/env_test/work/work_test/ack",
		"GET /v1/environments/env_test/work/poll?reclaim_older_than_ms=2500&worker_id=worker_test",
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
	for index := range want {
		if calls[index] != want[index] {
			t.Fatalf("calls[%d] = %q, want %q", index, calls[index], want[index])
		}
	}
}

func TestWorkPollerAckFailureLeavesAmbiguousClaimForTTLReclaim(t *testing.T) {
	t.Parallel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments/env_test/work/poll":
			writeWorkPollerJSON(t, w, workPollerFixture("queued"))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments/env_test/work/work_test/ack":
			http.Error(w, `{"error":{"type":"server_error","message":"unavailable"}}`, http.StatusServiceUnavailable)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()

	poller := NewWorkPoller(context.Background(), newWorkPollerTestClient(t, server.URL), WorkPollerOptions{
		EnvironmentID: "env_test", Drain: true,
	})
	if poller.Next() {
		t.Fatal("Next() = true after Ack failure")
	}
	if poller.Err() == nil {
		t.Fatal("Err() = nil after Ack failure")
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want Poll and Ack only", requests)
	}
}

func TestWorkPollerRejectsCrossEnvironmentResponse(t *testing.T) {
	t.Parallel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		work := workPollerFixture("queued")
		work["environment_id"] = "env_other"
		writeWorkPollerJSON(t, w, work)
	}))
	defer server.Close()

	poller := NewWorkPoller(context.Background(), newWorkPollerTestClient(t, server.URL), WorkPollerOptions{
		EnvironmentID: "env_test", Drain: true,
	})
	if poller.Next() {
		t.Fatal("Next() = true for a cross-Environment Work response")
	}
	if poller.Err() == nil {
		t.Fatal("Err() = nil for a cross-Environment Work response")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want only Poll", requests)
	}
}

func TestWorkPollerRejectsNonEmptyFallbackObjects(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body any
	}{
		{name: "partial work", body: map[string]any{"type": "work", "id": "work_partial"}},
		{name: "error object", body: map[string]any{"error": map[string]any{"message": "wrong status"}}},
		{name: "null", body: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeWorkPollerJSON(t, w, test.body)
			}))
			defer server.Close()
			poller := NewWorkPoller(context.Background(), newWorkPollerTestClient(t, server.URL), WorkPollerOptions{
				EnvironmentID: "env_test", Drain: true,
			})
			if poller.Next() || poller.Err() == nil {
				t.Fatalf("Next() = true or Err() = nil: %v", poller.Err())
			}
		})
	}
}

func TestWorkPollerValidatesWorkIdentityAndState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "empty work id", mutate: func(work map[string]any) { work["id"] = "" }},
		{name: "wrong data type", mutate: func(work map[string]any) {
			work["data"] = map[string]any{"type": "healthcheck", "id": "sesn_test"}
		}},
		{name: "empty session id", mutate: func(work map[string]any) {
			work["data"] = map[string]any{"type": "session", "id": ""}
		}},
		{name: "wrong state", mutate: func(work map[string]any) { work["state"] = "active" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests++
				work := workPollerFixture("queued")
				test.mutate(work)
				writeWorkPollerJSON(t, w, work)
			}))
			defer server.Close()
			poller := NewWorkPoller(context.Background(), newWorkPollerTestClient(t, server.URL), WorkPollerOptions{
				EnvironmentID: "env_test", Drain: true,
			})
			if poller.Next() || poller.Err() == nil {
				t.Fatalf("Next() = true or Err() = nil: %v", poller.Err())
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want only Poll", requests)
			}
		})
	}
}

func TestWorkPollerValidatesAcknowledgedIdentityAndState(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "wrong work id", mutate: func(work map[string]any) { work["id"] = "work_other" }},
		{name: "wrong environment", mutate: func(work map[string]any) { work["environment_id"] = "env_other" }},
		{name: "wrong session", mutate: func(work map[string]any) {
			work["data"] = map[string]any{"type": "session", "id": "sesn_other"}
		}},
		{name: "wrong state", mutate: func(work map[string]any) { work["state"] = "active" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/work/poll"):
					writeWorkPollerJSON(t, w, workPollerFixture("queued"))
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ack"):
					work := workPollerFixture("starting")
					test.mutate(work)
					writeWorkPollerJSON(t, w, work)
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			}))
			defer server.Close()
			poller := NewWorkPoller(context.Background(), newWorkPollerTestClient(t, server.URL), WorkPollerOptions{
				EnvironmentID: "env_test", Drain: true,
			})
			if poller.Next() || poller.Err() == nil {
				t.Fatalf("Next() = true or Err() = nil: %v", poller.Err())
			}
			if requests != 2 {
				t.Fatalf("requests = %d, want Poll and Ack only", requests)
			}
		})
	}
}

func TestWorkPollerCancellationBetweenPollAndAckLeavesTentativeClaim(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	body, err := json.Marshal(workPollerFixture("queued"))
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	httpClient := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if !strings.HasSuffix(request.URL.Path, "/work/poll") {
			t.Fatalf("unexpected request after cancellation: %s", request.URL.Path)
		}
		cancel()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(string(body))),
			Request:    request,
		}, nil
	})}
	client, err := New(Config{BaseURL: "http://mango.test", APIKey: "test-key", HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	poller := NewWorkPoller(ctx, client, WorkPollerOptions{EnvironmentID: "env_test", Drain: true})
	if poller.Next() {
		t.Fatal("Next() = true after cancellation")
	}
	if poller.Err() != nil {
		t.Fatalf("Err() = %v after cancellation", poller.Err())
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want Poll only", requests)
	}
}

func TestWorkPollerLongRunningDefaultAndCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	query := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query <- r.URL.RawQuery
		cancel()
		writeWorkPollerJSON(t, w, map[string]any{})
	}))
	defer server.Close()
	poller := NewWorkPoller(ctx, newWorkPollerTestClient(t, server.URL), WorkPollerOptions{
		EnvironmentID: "env_test", WorkerID: "worker_test",
	})
	poller.sleep = func(context.Context, time.Duration) {}
	if poller.Next() {
		t.Fatal("Next() = true after cancellation")
	}
	if poller.Err() != nil {
		t.Fatalf("Err() = %v after cancellation", poller.Err())
	}
	if got := <-query; got != "block_ms=999&worker_id=worker_test" {
		t.Fatalf("query = %q", got)
	}
}

func TestWorkPollerRetriesTransientPollFailure(t *testing.T) {
	t.Parallel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		if requests == 1 {
			http.Error(w, `{"error":{"type":"server_error","message":"unavailable"}}`, http.StatusServiceUnavailable)
			return
		}
		writeWorkPollerJSON(t, w, map[string]any{})
	}))
	defer server.Close()

	poller := NewWorkPoller(context.Background(), newWorkPollerTestClient(t, server.URL), WorkPollerOptions{
		EnvironmentID: "env_test", Drain: true,
	})
	poller.sleep = func(context.Context, time.Duration) {}
	if poller.Next() {
		t.Fatal("Next() = true for a drained queue")
	}
	if poller.Err() != nil {
		t.Fatalf("Err() = %v after transient recovery", poller.Err())
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
}

func TestWorkPollerStopsOnPermanentPollFailure(t *testing.T) {
	t.Parallel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(w, `{"error":{"type":"authentication_error","message":"bad key"}}`, http.StatusUnauthorized)
	}))
	defer server.Close()

	poller := NewWorkPoller(context.Background(), newWorkPollerTestClient(t, server.URL), WorkPollerOptions{
		EnvironmentID: "env_test",
	})
	poller.sleep = func(context.Context, time.Duration) {}
	if poller.Next() {
		t.Fatal("Next() = true after permanent failure")
	}
	if poller.Err() == nil {
		t.Fatal("Err() = nil after permanent failure")
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestWorkPollerValidatesOptions(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		make func() *WorkPoller
	}{
		{name: "client", make: func() *WorkPoller {
			return NewWorkPoller(context.Background(), nil, WorkPollerOptions{EnvironmentID: "env"})
		}},
		{name: "environment", make: func() *WorkPoller {
			return NewWorkPoller(context.Background(), &Client{}, WorkPollerOptions{})
		}},
		{name: "block", make: func() *WorkPoller {
			return NewWorkPoller(context.Background(), &Client{}, WorkPollerOptions{EnvironmentID: "env", BlockMs: Some(int64(0))})
		}},
		{name: "reclaim", make: func() *WorkPoller {
			return NewWorkPoller(context.Background(), &Client{}, WorkPollerOptions{EnvironmentID: "env", ReclaimOlderThanMs: Some(int64(-1))})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			poller := test.make()
			if poller.Next() || poller.Err() == nil {
				t.Fatalf("Next() = true or Err() = nil: %v", poller.Err())
			}
			if err := poller.Close(); err == nil {
				t.Fatal("Close() error = nil, want validation error")
			}
		})
	}
}

func TestWorkPollerAllEarlyBreakClosesWithoutStoppingWork(t *testing.T) {
	t.Parallel()
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch {
		case r.Method == http.MethodGet:
			writeWorkPollerJSON(t, w, workPollerFixture("queued"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ack"):
			writeWorkPollerJSON(t, w, workPollerFixture("starting"))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	poller := NewWorkPoller(context.Background(), newWorkPollerTestClient(t, server.URL), WorkPollerOptions{
		EnvironmentID: "env_test",
	})
	for work, err := range poller.All() {
		if err != nil || work == nil {
			t.Fatalf("All() = (%v, %v)", work, err)
		}
		break
	}
	if !poller.closed {
		t.Fatal("early break did not close poller")
	}
	if err := poller.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want Poll and Ack only", requests)
	}
}

func newWorkPollerTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	client, err := New(Config{BaseURL: baseURL, APIKey: "test-key"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func workPollerFixture(state string) map[string]any {
	return map[string]any{
		"id": "work_test", "type": "work", "environment_id": "env_test",
		"data":  map[string]any{"type": "session", "id": "sesn_test"},
		"state": state, "metadata": map[string]string{}, "secret": nil,
		"created_at": "2026-09-03T00:00:00Z", "acknowledged_at": nil,
		"started_at": nil, "latest_heartbeat_at": nil,
		"stop_requested_at": nil, "stopped_at": nil,
	}
}

func writeWorkPollerJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (fn roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return fn(request)
}
