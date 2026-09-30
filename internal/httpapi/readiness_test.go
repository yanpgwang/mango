package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestReadinessWithoutCheckFailsClosed(t *testing.T) {
	handler := NewServer(Deps{}, Config{RequireAuth: true}).Handler()
	for _, tc := range []struct {
		path   string
		status int
	}{
		{"/healthz", http.StatusOK},
		{"/readyz", http.StatusServiceUnavailable},
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if response.Code != tc.status {
			t.Errorf("%s status = %d, want %d", tc.path, response.Code, tc.status)
		}
	}
}

func TestReadinessFailureAndRecovery(t *testing.T) {
	failure := errors.New("postgres://user:secret@private-host/database: connection refused")
	calls := 0
	handler := NewServer(Deps{Readiness: func(ctx context.Context) error {
		calls++
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 2*time.Second {
			t.Error("probe requires a two-second deadline")
		}
		return failure
	}}, Config{RequireAuth: true}).Handler()
	for _, path := range []string{"/healthz", "/readyz", "/readyz"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if path == "/healthz" {
			if response.Code != 200 || calls != 0 {
				t.Fatalf("liveness performed probe or failed: %d, %d", response.Code, calls)
			}
			continue
		}
		if response.Header().Get("Cache-Control") != "no-store" {
			t.Error("readiness must not be cached")
		}
		if failure == nil {
			if response.Code != 200 || response.Body.Len() != 0 {
				t.Fatalf("recovery = %d %s", response.Code, response.Body.String())
			}
			continue
		}
		if response.Code != 503 {
			t.Fatalf("failure = %d", response.Code)
		}
		var body struct {
			Type      string
			Error     struct{ Type, Message string }
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Type != "error" || body.Error.Type != "api_error" || body.RequestID == "" || body.RequestID != response.Header().Get("request-id") {
			t.Fatalf("bad error envelope: %s", response.Body.String())
		}
		if strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "private-host") {
			t.Fatal("database details leaked")
		}
		failure = nil
	}
	if calls != 2 {
		t.Fatalf("probe calls = %d, want 2", calls)
	}
}

func TestReadinessDeadline(t *testing.T) {
	handler := NewServer(Deps{Readiness: func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}}, Config{}).Handler()
	for _, timeout := range []time.Duration{0, 20 * time.Millisecond} {
		t.Run(timeout.String(), func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
			if timeout > 0 {
				ctx, cancel := context.WithTimeout(req.Context(), timeout)
				defer cancel()
				req = req.WithContext(ctx)
			}
			start := time.Now()
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, req)
			if response.Code != 503 {
				t.Fatalf("timeout status = %d", response.Code)
			}
			limit := 3 * time.Second
			if timeout > 0 {
				limit = time.Second
			}
			if time.Since(start) > limit {
				t.Fatal("probe did not honor cancellation")
			}
		})
	}
}
