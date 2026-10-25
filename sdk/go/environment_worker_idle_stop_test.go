package mango

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnvironmentWorkerIdleStopUsesDecisionBoundaryWithoutFallback(t *testing.T) {
	for _, outcome := range []string{"accepted", "stale", "ambiguous", "missing_boundary", "terminated"} {
		t.Run(outcome, func(t *testing.T) {
			t.Parallel()
			var stops, heartbeats atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/heartbeat"):
					heartbeats.Add(1)
					writeEnvironmentWorkerJSON(t, w, environmentHeartbeatFixture("2026-10-18T00:00:01Z", "active", true, 30))
				case r.URL.Path == "/v1/sessions/session_idle_stop":
					writeEnvironmentWorkerJSON(t, w, emptySessionFixture("session_idle_stop"))
				case strings.HasSuffix(r.URL.Path, "/events/stream"):
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					if outcome == "terminated" {
						if _, err := fmt.Fprint(w, "event: session.status_terminated\ndata: {\"id\":\"terminated_idle_stop\",\"type\":\"session.status_terminated\",\"processed_at\":null}\n\n"); err != nil {
							t.Error(err)
						}
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
				case strings.HasSuffix(r.URL.Path, "/events"):
					if outcome == "terminated" {
						writeEnvironmentWorkerJSON(t, w, map[string]any{"data": []any{}, "next_page": nil})
						return
					}
					id, eventType := "idle_decision", "session.status_idle"
					if outcome == "missing_boundary" {
						id = ""
					}

					writeEnvironmentWorkerJSON(t, w, map[string]any{"data": []any{map[string]any{
						"id": id, "type": eventType, "processed_at": nil, "stop_reason": map[string]any{"type": "end_turn"},
					}}, "next_page": nil})
				case strings.HasSuffix(r.URL.Path, "/stop"):
					stops.Add(1)
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					if body["force"] != true {
						t.Errorf("Stop body: %#v", body)
					}
					if outcome == "terminated" {
						if _, exists := body["expected_idle_event_id"]; exists {
							t.Errorf("non-idle exit sent boundary: %#v", body)
						}
					} else if body["expected_idle_event_id"] != "idle_decision" {
						t.Errorf("Stop lost exact idle decision: %#v", body)
					}
					if outcome == "stale" {
						http.Error(w, `{"type":"error","error":{"type":"precondition_failed_error","message":"stale idle"}}`, http.StatusPreconditionFailed)
						return
					}
					if outcome == "ambiguous" {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						if err := connection.Close(); err != nil {
							t.Error(err)
						}
						return
					}
					w.WriteHeader(http.StatusNoContent)
				default:
					http.Error(w, "unexpected request", http.StatusNotFound)
				}
			}))
			defer server.Close()
			worker := NewEnvironmentWorker(newEnvironmentWorkerClient(t, server.URL, "workspace-key"), EnvironmentWorkerOptions{MaxIdle: durationPointer(5 * time.Millisecond)})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := worker.HandleItem(ctx, EnvironmentWorkerHandleItemOptions{WorkID: "work_idle_stop", EnvironmentID: "env_idle_stop", SessionID: "session_idle_stop", WorkSecret: encodeEnvironmentWorkSecret(t, "scoped-token")})
			if ctx.Err() != nil {
				t.Fatalf("test ended through context instead of runner decision: %v", ctx.Err())
			}
			switch outcome {
			case "stale":
				if !errors.Is(err, ErrEnvironmentWorkLeaseLost) {
					t.Fatalf("stale Stop: %v", err)
				}
			case "ambiguous":
				if err == nil {
					t.Fatal("ambiguous Stop must be reported")
				}
			case "missing_boundary":
				if !errors.Is(err, ErrIdleTimeout) {
					t.Fatalf("missing boundary: %v", err)
				}
			default:
				if err != nil {
					t.Fatal(err)
				}
			}
			wantStops := int32(1)
			if outcome == "missing_boundary" {
				wantStops = 0
			}
			if stops.Load() != wantStops || heartbeats.Load() != 1 {
				t.Fatalf("stops=%d heartbeats=%d; unsafe fallback or renewal", stops.Load(), heartbeats.Load())
			}
		})
	}
}
