package mango

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnvironmentWorkerHealthcheckWithoutExecutorReportsFailure(t *testing.T) {
	var results atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer check-token" {
			t.Error("worker did not use its own token")
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			writeEnvironmentWorkerJSON(t, w, environmentHeartbeatFixture("2026-09-29T00:00:01Z", "active", true, 30))
		case strings.HasSuffix(r.URL.Path, "/result"):
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if body["status"] != "failed" || body["message"] == "" {
				t.Errorf("result=%+v", body)
			}
			results.Add(1)
			writeEnvironmentWorkerJSON(t, w, map[string]any{"id": "work_check", "state": "stopped"})
		default:
			t.Errorf("healthcheck called %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	secret := encodeEnvironmentWorkSecret(t, "check-token")
	worker := NewEnvironmentWorker(newEnvironmentWorkerClient(t, server.URL, "workspace-key"), EnvironmentWorkerOptions{})
	err := worker.handleWork(context.Background(), EnvironmentWork{ID: "work_check", EnvironmentID: "env_one", State: EnvironmentWorkStateStarting, Secret: &secret, Data: EnvironmentWorkData{HealthcheckWorkData: &HealthcheckWorkData{Type: "healthcheck"}}})
	if err != nil {
		t.Fatal(err)
	}
	if results.Load() != 1 {
		t.Fatalf("results=%d", results.Load())
	}
}

func TestEnvironmentWorkerHealthcheckExecutesOnceAcrossAmbiguousCompletion(t *testing.T) {
	var executions, results atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer check-token" {
			t.Error("worker did not isolate token")
		}
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			writeEnvironmentWorkerJSON(t, w, environmentHeartbeatFixture("2026-09-29T00:00:01Z", "active", true, 30))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/result") {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var body EnvironmentWorkResultRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Status != "succeeded" {
			t.Errorf("status=%q", body.Status)
		}
		if results.Add(1) == 1 {
			http.Error(w, "response lost after commit", 503)
			return
		}
		writeEnvironmentWorkerJSON(t, w, map[string]any{"id": "work_check", "state": "stopped"})
	}))
	defer server.Close()
	worker := NewEnvironmentWorker(newEnvironmentWorkerClient(t, server.URL, "workspace-key"), EnvironmentWorkerOptions{Workdir: "/sandbox", Healthcheck: func(ctx context.Context, dir string) error {
		executions.Add(1)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 10*time.Second || dir != "/sandbox" {
			t.Error("probe lacked execution bound or workdir")
		}
		return nil
	}})
	worker.sleep = func(context.Context, time.Duration) {}
	if err := worker.HandleItem(context.Background(), EnvironmentWorkerHandleItemOptions{WorkID: "work_check", EnvironmentID: "env_one", WorkType: "healthcheck", WorkSecret: encodeEnvironmentWorkSecret(t, "check-token")}); err != nil {
		t.Fatal(err)
	}
	if executions.Load() != 1 || results.Load() != 2 {
		t.Fatalf("executions=%d result requests=%d", executions.Load(), results.Load())
	}
}

func TestEnvironmentWorkerHealthcheckTimeoutIsReportedAsFailure(t *testing.T) {
	var result string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/heartbeat") {
			writeEnvironmentWorkerJSON(t, w, environmentHeartbeatFixture("2026-09-29T00:00:01Z", "active", true, 30))
			return
		}
		if !strings.HasSuffix(r.URL.Path, "/result") {
			t.Errorf("unexpected request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		var body EnvironmentWorkResultRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		result = string(body.Status)
		writeEnvironmentWorkerJSON(t, w, map[string]any{"id": "work_check", "state": "stopped"})
	}))
	defer server.Close()
	worker := NewEnvironmentWorker(newEnvironmentWorkerClient(t, server.URL, "workspace-key"), EnvironmentWorkerOptions{Healthcheck: func(ctx context.Context, _ string) error { <-ctx.Done(); return ctx.Err() }})
	if err := worker.HandleItem(context.Background(), EnvironmentWorkerHandleItemOptions{WorkID: "work_check", EnvironmentID: "env_one", WorkType: "healthcheck", WorkSecret: encodeEnvironmentWorkSecret(t, "check-token")}); err != nil {
		t.Fatal(err)
	}
	if result != "failed" {
		t.Fatalf("timeout result=%q", result)
	}
}
