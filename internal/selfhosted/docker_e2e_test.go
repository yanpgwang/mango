package selfhosted

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	mango "github.com/yanpgwang/mango/sdk/go"
)

func TestDockerLauncherRealItemLifecycle(t *testing.T) {
	if os.Getenv("MANGO_TEST_DOCKER") != "1" {
		t.Skip("set MANGO_TEST_DOCKER=1 to require Docker worker E2E")
	}
	image := os.Getenv("MANGO_TEST_WORKER_IMAGE")
	if image == "" {
		t.Fatal("MANGO_TEST_WORKER_IMAGE is required when Docker tests are enabled")
	}

	work := acknowledgedWork()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	work.ID += "_" + suffix
	work.Data.ID += "_" + suffix
	second := work
	second.ID += "_resume"
	works := []mango.EnvironmentWork{work, second}
	skillArchive := dockerSkillArchive(t)
	var polls, acknowledgements, firstHeartbeats, renewals, streams, results, stops atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments/env_test/work/poll":
			if got := r.Header.Get("Authorization"); got != "Bearer workspace-key" {
				t.Errorf("poll Authorization = %q", got)
			}
			index := int(polls.Add(1)) - 1
			if index >= len(works) {
				writeJSON(t, w, map[string]any{})
				return
			}
			writeJSON(t, w, dockerWorkFixture(works[index], "queued", true))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/environments/env_test/work/") && strings.HasSuffix(r.URL.Path, "/ack"):
			if got := r.Header.Get("Authorization"); got != "Bearer workspace-key" {
				t.Errorf("ack Authorization = %q", got)
			}
			for _, candidate := range works {
				if r.URL.Path == "/v1/environments/env_test/work/"+candidate.ID+"/ack" {
					acknowledgements.Add(1)
					writeJSON(t, w, dockerWorkFixture(candidate, "starting", false))
					return
				}
			}
			http.Error(w, "unknown Work", http.StatusNotFound)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/heartbeat"):
			assertItemAuthorization(t, r)
			expected := r.URL.Query().Get("expected_last_heartbeat")
			if expected == "NO_HEARTBEAT" {
				firstHeartbeats.Add(1)
			} else {
				renewals.Add(1)
				if expected != "2026-09-04T00:00:01Z" {
					t.Errorf("renewal heartbeat precondition = %q", expected)
				}
			}
			writeJSON(t, w, map[string]any{
				"type": "work_heartbeat", "last_heartbeat": "2026-09-04T00:00:01Z",
				"lease_extended": true, "state": "active", "ttl_seconds": 1,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions/"+work.Data.ID:
			assertItemAuthorization(t, r)
			writeJSON(t, w, map[string]any{
				"id": work.Data.ID,
				"agent": map[string]any{
					"id": "agent_docker", "version": 1,
					"skills": []any{map[string]any{
						"type": "custom", "skill_id": "skill_docker", "version": "1",
					}},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/skills/skill_docker/versions/1":
			assertItemAuthorization(t, r)
			writeJSON(t, w, map[string]any{
				"id": "1", "skill_id": "skill_docker", "version": "1",
				"name": "docker-skill", "directory": "docker-skill",
				"size_bytes":      len(skillArchive),
				"checksum_sha256": fmt.Sprintf("%x", sha256.Sum256(skillArchive)),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/skills/skill_docker/versions/1/content":
			assertItemAuthorization(t, r)
			w.Header().Set("Content-Type", "application/zip")
			_, _ = w.Write(skillArchive)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events/stream"):
			assertItemAuthorization(t, r)
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Error("response writer cannot flush SSE")
				return
			}
			activation := streams.Add(1)
			calls := []dockerBashCall{
				{ID: "tool_e2e_resume", Input: map[string]any{"command": "cat proof.txt"}},
				{ID: "tool_e2e_skill_resume", Input: map[string]any{"command": "grep -o self-hosted-skill skills/docker-skill/SKILL.md"}},
			}
			if activation == 1 {
				calls = []dockerBashCall{
					{ID: "tool_e2e_credential_boundary", Input: map[string]any{"command": "if cat /proc/$PPID/environ >/tmp/mango-parent-environ 2>/dev/null; then printf parent-readable; else printf parent-protected; fi"}},
					{ID: "tool_e2e_state_1", Input: map[string]any{"command": "mkdir -p state-dir; cd state-dir; export PROOF=docker-e2e; printf ready"}},
					{ID: "tool_e2e_state_2", Input: map[string]any{"command": "printf '%s|%s' \"$PROOF\" \"$PWD\""}},
					{ID: "tool_e2e_restart", Input: map[string]any{"restart": true, "command": "printf '[%s]|%s' \"$PROOF\" \"$PWD\""}},
					{ID: "tool_e2e_file", Input: map[string]any{"command": "printf docker-e2e > proof.txt && cat proof.txt"}},
					{ID: "tool_e2e_skill", Input: map[string]any{"command": "grep -o self-hosted-skill skills/docker-skill/SKILL.md"}},
				}
			}
			for _, call := range calls {
				if err := writeDockerBashEvent(w, call); err != nil {
					t.Errorf("write tool event: %v", err)
					return
				}
			}
			if _, err := fmt.Fprintf(w, "event: session.status_idle\ndata: {\"id\":\"idle_e2e_%d\",\"type\":\"session.status_idle\",\"processed_at\":null,\"stop_reason\":{\"type\":\"end_turn\"}}\n\n", activation); err != nil {
				t.Errorf("write idle event: %v", err)
				return
			}
			flusher.Flush()
			<-r.Context().Done()
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			assertItemAuthorization(t, r)
			writeJSON(t, w, map[string]any{"data": []any{}, "next_page": nil})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events"):
			assertItemAuthorization(t, r)
			body := make(map[string]any)
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode result: %v", err)
			}
			encoded, _ := json.Marshal(body)
			result := string(encoded)
			expected := map[string]string{
				"tool_e2e_credential_boundary": "parent-protected",
				"tool_e2e_state_1":             "ready",
				"tool_e2e_state_2":             "docker-e2e|/workspace/state-dir",
				"tool_e2e_restart":             "[]|/workspace",
				"tool_e2e_file":                "docker-e2e",
				"tool_e2e_resume":              "docker-e2e",
				"tool_e2e_skill":               "self-hosted-skill",
				"tool_e2e_skill_resume":        "self-hosted-skill",
			}
			matched := false
			for toolUseID, text := range expected {
				if strings.Contains(result, toolUseID) {
					matched = true
					if !strings.Contains(result, text) {
						t.Errorf("tool result for %s omitted %q: %s", toolUseID, text, encoded)
					}
				}
			}
			if !matched {
				t.Errorf("unexpected tool result = %s", encoded)
			}
			results.Add(1)
			writeJSON(t, w, map[string]any{"data": []any{}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			assertItemAuthorization(t, r)
			stops.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	defer server.Close()

	engine, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close() }()
	probeCtx, cancelProbe := context.WithTimeout(context.Background(), 10*time.Second)
	_, err = engine.Ping(probeCtx, client.PingOptions{NegotiateAPIVersion: true})
	cancelProbe()
	if err != nil {
		t.Fatalf("Docker Engine is required: %v", err)
	}

	volume := dockerSessionVolume(work.Data.ID)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = engine.VolumeRemove(cleanupCtx, volume, client.VolumeRemoveOptions{Force: true})
	}()
	port := listener.Addr().(*net.TCPAddr).Port
	supervisor, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: "workspace-key"})
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: supervisor, EnvironmentID: "env_test", Image: image, Drain: true,
		SandboxBaseURL: "http://host.docker.internal:" + strconv.Itoa(port),
		MaxIdle:        750 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := launcher.Run(ctx); err != nil {
		t.Fatal(err)
	}
	assertContainerRemoved(t, engine, dockerWorkName(work.ID))
	assertContainerRemoved(t, engine, dockerWorkName(second.ID))
	if polls.Load() != 3 || acknowledgements.Load() != 2 || firstHeartbeats.Load() != 2 || renewals.Load() < 2 || streams.Load() != 2 || results.Load() != 8 || stops.Load() != 2 {
		t.Fatalf("polls=%d acknowledgements=%d first_heartbeats=%d renewals=%d streams=%d results=%d stops=%d", polls.Load(), acknowledgements.Load(), firstHeartbeats.Load(), renewals.Load(), streams.Load(), results.Load(), stops.Load())
	}
}

type dockerBashCall struct {
	ID    string
	Input map[string]any
}

func writeDockerBashEvent(writer http.ResponseWriter, call dockerBashCall) error {
	input, err := json.Marshal(call.Input)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "event: agent.tool_use\ndata: {\"id\":%q,\"type\":\"agent.tool_use\",\"processed_at\":null,\"input\":%s,\"name\":\"bash\",\"evaluated_permission\":\"allow\"}\n\n", call.ID, input)
	return err
}

func TestDockerLauncherCancellationPostsToolErrorBeforeStop(t *testing.T) {
	if os.Getenv("MANGO_TEST_DOCKER") != "1" {
		t.Skip("set MANGO_TEST_DOCKER=1 to require Docker worker E2E")
	}
	image := os.Getenv("MANGO_TEST_WORKER_IMAGE")
	if image == "" {
		t.Fatal("MANGO_TEST_WORKER_IMAGE is required when Docker tests are enabled")
	}

	started := make(chan struct{})
	posted := make(chan string, 1)
	var startedOnce sync.Once
	var stops atomic.Int32
	var sandboxPort atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/started":
			startedOnce.Do(func() { close(started) })
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/heartbeat"):
			assertItemAuthorization(t, r)
			writeJSON(t, w, map[string]any{
				"type": "work_heartbeat", "last_heartbeat": "2026-09-04T00:00:01Z",
				"lease_extended": true, "state": "active", "ttl_seconds": 30,
			})
		case r.Method == http.MethodGet &&
			strings.HasPrefix(r.URL.Path, "/v1/sessions/") &&
			!strings.Contains(strings.TrimPrefix(r.URL.Path, "/v1/sessions/"), "/"):
			assertItemAuthorization(t, r)
			writeJSON(t, w, map[string]any{
				"id":    strings.TrimPrefix(r.URL.Path, "/v1/sessions/"),
				"agent": map[string]any{"skills": []any{}},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events/stream"):
			assertItemAuthorization(t, r)
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			command := fmt.Sprintf("printf 'GET /started HTTP/1.0\\r\\nHost: host.docker.internal\\r\\n\\r\\n' > /dev/tcp/host.docker.internal/%d; sleep 30", sandboxPort.Load())
			if _, err := fmt.Fprintf(w, "event: agent.tool_use\ndata: {\"id\":\"tool_cancel\",\"type\":\"agent.tool_use\",\"processed_at\":null,\"input\":{\"command\":%q},\"name\":\"bash\",\"evaluated_permission\":\"allow\"}\n\n", command); err != nil {
				t.Errorf("write cancellation tool event: %v", err)
				return
			}
			flusher.Flush()
			<-r.Context().Done()
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/events"):
			assertItemAuthorization(t, r)
			writeJSON(t, w, map[string]any{"data": []any{}, "next_page": nil})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events"):
			assertItemAuthorization(t, r)
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode cancellation result: %v", err)
			}
			encoded, _ := json.Marshal(body)
			select {
			case posted <- string(encoded):
			default:
				t.Errorf("duplicate cancellation result: %s", encoded)
			}
			writeJSON(t, w, map[string]any{"data": []any{}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/stop"):
			assertItemAuthorization(t, r)
			stops.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	defer server.Close()
	sandboxPort.Store(int32(listener.Addr().(*net.TCPAddr).Port))

	engine, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close() }()
	work := acknowledgedWork()
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	work.ID += "_cancel_" + suffix
	work.Data.ID += "_cancel_" + suffix
	volume := dockerSessionVolume(work.Data.ID)
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = engine.VolumeRemove(cleanupCtx, volume, client.VolumeRemoveOptions{Force: true})
	}()
	itemClient, err := mango.New(mango.Config{BaseURL: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: itemClient, EnvironmentID: "env_test", Image: image,
		SandboxBaseURL: "http://host.docker.internal:" + strconv.Itoa(int(sandboxPort.Load())),
		MaxIdle:        0,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- launcher.runItem(ctx, work) }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("Bash tool did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cancelled Work container did not exit")
	}
	select {
	case result := <-posted:
		if !strings.Contains(result, `"tool_use_id":"tool_cancel"`) ||
			!strings.Contains(result, `"is_error":true`) || !strings.Contains(result, context.Canceled.Error()) {
			t.Fatalf("cancellation result = %s", result)
		}
	default:
		t.Fatal("cancelled tool result was not posted")
	}
	if stops.Load() != 1 {
		t.Fatalf("stops = %d, want 1", stops.Load())
	}
	assertContainerRemoved(t, engine, dockerWorkName(work.ID))
}

func assertItemAuthorization(t *testing.T, request *http.Request) {
	t.Helper()
	if got := request.Header.Get("Authorization"); got != "Bearer sess_test" {
		t.Errorf("%s %s Authorization = %q", request.Method, request.URL.Path, got)
	}
}

func dockerWorkFixture(work mango.EnvironmentWork, state string, includeSecret bool) map[string]any {
	secret := any(nil)
	if includeSecret {
		secret = *work.Secret
	}
	return map[string]any{
		"id": work.ID, "type": "work", "environment_id": work.EnvironmentID,
		"data":  map[string]any{"type": "session", "id": work.Data.ID},
		"state": state, "metadata": map[string]string{}, "secret": secret,
		"created_at": "2026-09-04T00:00:00Z", "acknowledged_at": nil,
		"started_at": nil, "latest_heartbeat_at": nil,
		"stop_requested_at": nil, "stopped_at": nil,
	}
}

func dockerSkillArchive(t *testing.T) []byte {
	t.Helper()
	var body bytes.Buffer
	writer := zip.NewWriter(&body)
	header := &zip.FileHeader{Name: "docker-skill/SKILL.md", Method: zip.Deflate}
	header.SetMode(0o644)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprint(entry, "---\nname: docker-skill\ndescription: Docker E2E\n---\nself-hosted-skill\n")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body.Bytes()
}

func assertContainerRemoved(t *testing.T, engine *client.Client, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := engine.ContainerInspect(ctx, name, client.ContainerInspectOptions{}); !errdefs.IsNotFound(err) {
		t.Fatalf("container %q was not removed: %v", name, err)
	}
}
