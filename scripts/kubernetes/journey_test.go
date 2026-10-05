package kubernetes_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
)

func TestKubernetesAlpha(t *testing.T) {
	if os.Getenv("MANGO_TEST_KUBERNETES") != "1" {
		t.Skip("set MANGO_TEST_KUBERNETES=1 to require isolated Kubernetes acceptance")
	}
	cluster := newCluster(t)
	source := cluster.startState("source")
	cluster.install(source, "alpha", false)
	evidence := source.verifyRestartJourney()
	cluster.verifyOwnedWorkCleanup()
	source.verifyQuiescedRestore(evidence)
}

type journeyEvidence struct {
	fileID, skillID, skillVersion, storeID, environment string
}

func (s *stateFixture) request(method, path, contentType string, body []byte, status int, authenticated bool) []byte {
	s.cluster.t.Helper()
	req, err := http.NewRequestWithContext(s.cluster.ctx, method, s.baseURL+path, bytes.NewReader(body))
	require.NoError(s.cluster.t, err)
	if authenticated {
		req.Header.Set("Authorization", "Bearer fixture-workspace-only")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	client := &http.Client{Timeout: 15 * time.Second}
	var response *http.Response
	if method == "GET" {
		// Single-replica replacement permits transient transport failure while
		// EndpointSlice/proxy state catches up. Retry only reads; POST admission
		// must retain its unknown-outcome semantics and is never retried here.
		s.cluster.await(15*time.Second, func() bool {
			response, err = client.Do(req)
			return err == nil
		})
	} else {
		response, err = client.Do(req)
	}
	require.NoError(s.cluster.t, err)
	defer func() { require.NoError(s.cluster.t, response.Body.Close()) }()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	require.NoError(s.cluster.t, err)
	require.Equal(s.cluster.t, status, response.StatusCode, "%s %s: %s", method, path, data)
	return data
}

func (s *stateFixture) json(method, path string, input any, status int) map[string]any {
	s.cluster.t.Helper()
	var body []byte
	if input != nil {
		var err error
		body, err = json.Marshal(input)
		require.NoError(s.cluster.t, err)
	}
	data := s.request(method, path, "application/json", body, status, true)
	var result map[string]any
	require.NoError(s.cluster.t, json.Unmarshal(data, &result))
	return result
}

func (s *stateFixture) upload(path, field, name string, content []byte) map[string]any {
	s.cluster.t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile(field, name)
	require.NoError(s.cluster.t, err)
	_, err = part.Write(content)
	require.NoError(s.cluster.t, err)
	require.NoError(s.cluster.t, writer.Close())
	raw := s.request("POST", path, writer.FormDataContentType(), body.Bytes(), 200, true)
	var result map[string]any
	require.NoError(s.cluster.t, json.Unmarshal(raw, &result))
	return result
}

func skillArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	file, err := writer.Create("alpha-skill/SKILL.md")
	require.NoError(t, err)
	_, err = io.WriteString(file, "---\nname: alpha-skill\ndescription: Exercise the alpha fixture Skill.\n---\nalpha-skill-bundle\n")
	require.NoError(t, err)
	require.NoError(t, writer.Close())
	return buffer.Bytes()
}

func (s *stateFixture) send(session string, event map[string]any, status int) {
	s.cluster.t.Helper()
	s.json("POST", "/v1/sessions/"+session+"/events", map[string]any{"events": []any{event}}, status)
}

func (s *stateFixture) events(session string) []map[string]any {
	s.cluster.t.Helper()
	response := s.json("GET", "/v1/sessions/"+session+"/events?limit=1000", nil, 200)
	next, present := response["next_page"]
	require.True(s.cluster.t, present, "event pages require an explicit next_page")
	require.Nil(s.cluster.t, next, "fixture event history exceeded one page")
	var result []map[string]any
	for _, raw := range response["data"].([]any) {
		event := raw.(map[string]any)
		require.NotEqual(s.cluster.t, "session.error", event["type"], "unexpected Session failure: %v", event)
		result = append(result, event)
	}
	return result
}

func (s *stateFixture) waitEvents(session string, predicate func([]map[string]any) bool) []map[string]any {
	s.cluster.t.Helper()
	var events []map[string]any
	s.cluster.await(90*time.Second, func() bool {
		events = s.events(session)
		return predicate(events)
	})
	return events
}

func latestIdle(events []map[string]any) string {
	var reason string
	for _, event := range events {
		if event["type"] == "session.status_idle" {
			stop, _ := event["stop_reason"].(map[string]any)
			reason, _ = stop["type"].(string)
		}
	}
	return reason
}

func eventCount(events []map[string]any, kind string) int {
	count := 0
	for _, event := range events {
		if event["type"] == kind {
			count++
		}
	}
	return count
}

func (s *stateFixture) waitAction(session, kind string, count int) string {
	s.cluster.t.Helper()
	var action string
	s.waitEvents(session, func(events []map[string]any) bool {
		for _, event := range events {
			if event["type"] == kind {
				action = event["id"].(string)
			}
		}
		return action != "" && eventCount(events, kind) == count && latestIdle(events) == "requires_action"
	})
	return action
}

func removeOwnedWork(ctx context.Context, engine *client.Client, environment, session, image string) (int, error) {
	if environment == "" || session == "" || image == "" {
		return 0, errors.New("exact fixture identity is required for item cleanup")
	}
	filters := make(client.Filters).Add("label", "io.mango.self-hosted-worker=true", "io.mango.environment-id="+environment, "io.mango.session-id="+session)
	listed, err := engine.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, item := range listed.Items {
		inspected, err := engine.ContainerInspect(ctx, item.ID, client.ContainerInspectOptions{})
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			return removed, err
		}
		config := inspected.Container.Config
		if config == nil || config.Image != image || config.Labels["io.mango.self-hosted-worker"] != "true" ||
			config.Labels["io.mango.environment-id"] != environment || config.Labels["io.mango.session-id"] != session || config.Labels["io.mango.work-id"] == "" {
			continue
		}
		// Supervisor has exited. A surviving exact-owned item must be killed
		// before workspace/image cleanup, including paused/hung failure cases.
		_, err = engine.ContainerRemove(ctx, item.ID, client.ContainerRemoveOptions{Force: true})
		if err != nil && !errdefs.IsNotFound(err) {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

func (s *stateFixture) startSupervisor(environment, session string) func() {
	c, t := s.cluster, s.cluster.t
	t.Helper()
	var key string
	raw := c.kube(nil, "-n", s.namespace, "exec", "deployment/"+s.namespace+"-api", "--", "mango", "api-key", "create", "-workspace", "wrkspc_default", "-environment", environment, "-label", "alpha-fixture-supervisor")
	for _, line := range strings.Split(string(raw), "\n") {
		if value, ok := strings.CutPrefix(line, "api_key\t"); ok {
			require.True(t, key == "", "CLI returned more than one key")
			key = value
		}
	}
	require.NotEmpty(t, key, "CLI did not return its documented api_key field")
	logPath := filepath.Join(c.temp, s.namespace+"-supervisor.log")
	logfile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	require.NoError(t, err)
	cmd := exec.Command(c.workerBinary, "docker", "--environment-id", environment, "--base-url", s.baseURL,
		"--sandbox-base-url", s.internalURL, "--network", "kind", "--image", c.workerImage, "--max-idle", "1s")
	// Preserve only host/process/Docker transport configuration. Credentialed
	// provider environments must not become accidental supervisor inputs.
	allowed := map[string]bool{"PATH": true, "HOME": true, "TMPDIR": true, "TMP": true, "TEMP": true,
		"DOCKER_HOST": true, "DOCKER_CONTEXT": true, "DOCKER_TLS_VERIFY": true, "DOCKER_CERT_PATH": true, "XDG_RUNTIME_DIR": true}
	for _, value := range os.Environ() {
		name, _, _ := strings.Cut(value, "=")
		if allowed[name] {
			cmd.Env = append(cmd.Env, value)
		}
	}
	cmd.Env = append(cmd.Env, "MANGO_ENVIRONMENT_KEY="+key)
	cmd.Stdout, cmd.Stderr = logfile, logfile
	require.NoError(t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			var shutdownErr error
			if err := cmd.Process.Signal(syscall.SIGTERM); err != nil && err != os.ErrProcessDone {
				shutdownErr = err
			}
			select {
			case err := <-done:
				shutdownErr = errors.Join(shutdownErr, err)
			case <-time.After(3 * time.Minute):
				// Production permits 120s container grace + 15s Docker request
				// and 15s removal budgets; include host scheduling margin.
				_ = cmd.Process.Kill()
				<-done
				shutdownErr = errors.New("external supervisor shutdown exceeded its bound")
			}
			shutdownErr = errors.Join(shutdownErr, logfile.Close())
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			engine, err := client.New(client.FromEnv)
			if err == nil {
				defer func() { _ = engine.Close() }()
				_, err = engine.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true})
				if err == nil {
					var removed int
					removed, err = removeOwnedWork(ctx, engine, environment, session, c.workerImage)
					if removed != 0 {
						shutdownErr = errors.Join(shutdownErr, errors.New("supervisor left item containers; forced cleanup completed, backup cannot proceed"))
					}
				}
			}
			// Fatal only after fallback cleanup, so no backup can begin following
			// an uncertain shutdown and test cleanup can remove volumes/images.
			require.NoError(t, errors.Join(shutdownErr, err), "external supervisor shutdown and owned item cleanup")
		})
	}
	t.Cleanup(func() {
		stop()
		if t.Failed() {
			data, err := os.ReadFile(logPath)
			if err == nil {
				text := strings.ReplaceAll(string(data), key, "[redacted]")
				if len(text) > 4000 {
					text = text[len(text)-4000:]
				}
				t.Logf("external supervisor diagnostic:\n%s", text)
			}
		}
	})
	return stop
}

func (s *stateFixture) replaceControlPlane() {
	c, t := s.cluster, s.cluster.t
	t.Helper()
	t.Log("Replacing API and orchestration with a pending confirmation")
	c.kube(nil, "-n", s.namespace, "rollout", "restart", "deployment/"+s.namespace+"-api", "deployment/"+s.namespace+"-orchestrator")
	for _, role := range []string{"api", "orchestrator"} {
		c.kube(nil, "-n", s.namespace, "rollout", "status", "deployment/"+s.namespace+"-"+role, "--timeout", "2m")
	}
}

func (s *stateFixture) waitStoppedWork(environment string, count int) {
	s.cluster.t.Helper()
	s.cluster.await(45*time.Second, func() bool {
		page := s.json("GET", "/v1/environments/"+environment+"/work?limit=100", nil, 200)
		items := page["data"].([]any)
		if len(items) != count {
			return false
		}
		for _, raw := range items {
			if raw.(map[string]any)["state"] != "stopped" {
				return false
			}
		}
		return true
	})
}

func (s *stateFixture) verifyRestartJourney() journeyEvidence {
	c, t := s.cluster, s.cluster.t
	t.Helper()
	s.request("GET", "/healthz", "", nil, 200, false)
	s.request("GET", "/readyz", "", nil, 200, false)
	s.request("GET", "/v1/agents", "", nil, 401, false)
	file := s.upload("/v1/files", "file", "alpha.txt", []byte("alpha-file-bytes"))
	fileID := file["id"].(string)
	require.Equal(t, []byte("alpha-file-bytes"), s.request("GET", "/v1/files/"+fileID+"/content", "", nil, 200, true))
	skill := s.upload("/v1/skills", "files[]", "alpha-skill.zip", skillArchive(t))
	store := s.json("POST", "/v1/memory_stores", map[string]any{"name": "alpha-memory"}, 200)
	storeID := store["id"].(string)
	s.json("POST", "/v1/memory_stores/"+storeID+"/memories", map[string]any{"path": "/seed.txt", "content": "alpha-seed"}, 200)
	env := s.json("POST", "/v1/environments", map[string]any{"name": "alpha-external-docker"}, 200)
	environment := env["id"].(string)
	agent := s.json("POST", "/v1/agents", map[string]any{"name": "alpha-sandbox", "model": "alpha-fixture",
		"tools": []any{map[string]any{"type": "agent_toolset_20260401",
			"default_config": map[string]any{"enabled": false, "permission_policy": map[string]any{"type": "always_allow"}},
			"configs": []any{
				map[string]any{"name": "bash", "enabled": true, "permission_policy": map[string]any{"type": "always_ask"}},
				map[string]any{"name": "read", "enabled": true, "permission_policy": map[string]any{"type": "always_allow"}},
			}}},
		"skills": []any{map[string]any{"type": "custom", "skill_id": skill["id"], "version": skill["latest_version"]}}}, 200)
	session := s.json("POST", "/v1/sessions", map[string]any{"agent": agent["id"], "environment_id": environment,
		"resources": []any{map[string]any{"type": "memory_store", "memory_store_id": storeID, "access": "read_write"}}}, 200)
	sessionID := session["id"].(string)
	hash := sha256.Sum256([]byte(sessionID))
	t.Cleanup(func() { c.removeWorkspace(fmt.Sprintf("mango-workspace-%x", hash[:12])) })
	stop := s.startSupervisor(environment, sessionID)
	s.send(sessionID, map[string]any{"type": "user.message", "content": []any{map[string]any{"type": "text", "text": "alpha:write"}}}, 200)
	action := s.waitAction(sessionID, "agent.tool_use", 1)
	before := s.events(sessionID)
	s.replaceControlPlane()
	after := s.events(sessionID)
	require.GreaterOrEqual(t, len(after), len(before))
	require.Equal(t, before, after[:len(before)], "replacement changed public event history")
	require.Equal(t, action, s.waitAction(sessionID, "agent.tool_use", 1))
	confirmation := map[string]any{"type": "user.tool_confirmation", "tool_use_id": action, "result": "allow"}
	s.send(sessionID, confirmation, 200)
	s.send(sessionID, confirmation, 409)
	completed := s.waitEvents(sessionID, func(events []map[string]any) bool {
		return eventCount(events, "agent.message") == 1 && latestIdle(events) == "end_turn"
	})
	require.Equal(t, 1, eventCount(completed, "user.tool_result"))
	c.await(30*time.Second, func() bool {
		page := s.json("GET", "/v1/memory_stores/"+storeID+"/memories?view=full", nil, 200)
		for _, item := range page["data"].([]any) {
			memory := item.(map[string]any)
			if memory["path"] == "/proof.txt" && memory["content"] == "alpha-memory" {
				return true
			}
		}
		return false
	})
	s.waitStoppedWork(environment, 1)
	s.send(sessionID, map[string]any{"type": "user.message", "content": []any{map[string]any{"type": "text", "text": "alpha:read"}}}, 200)
	readAction := s.waitAction(sessionID, "agent.tool_use", 2)
	s.send(sessionID, map[string]any{"type": "user.tool_confirmation", "tool_use_id": readAction, "result": "allow"}, 200)
	completed = s.waitEvents(sessionID, func(events []map[string]any) bool {
		return eventCount(events, "agent.message") == 2 && latestIdle(events) == "end_turn"
	})
	require.Equal(t, 2, eventCount(completed, "user.tool_result"))
	s.waitStoppedWork(environment, 2)
	for _, event := range completed {
		if event["type"] == "user.tool_result" {
			raw, err := json.Marshal(event)
			require.NoError(t, err)
			require.Equal(t, 1, strings.Count(string(raw), "alpha-proof"), "write side effect repeated")
		}
	}
	stop()
	require.Equal(t, []byte("alpha-file-bytes"), s.request("GET", "/v1/files/"+fileID+"/content", "", nil, 200, true))
	t.Log("Authenticated install, external Bash/Skill/Memory, pending confirmation replacement and reactivation passed")
	return journeyEvidence{fileID: fileID, skillID: skill["id"].(string), skillVersion: skill["latest_version"].(string),
		storeID: storeID, environment: environment}
}
