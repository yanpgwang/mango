package kubernetes_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkDiagnosticsKeepOnlyPublicLifecycleMetadata(t *testing.T) {
	raw := []byte(`{"data":[
		{"id":"sevt_3","type":"session.status_idle","processed_at":null,"stop_reason":{"type":"requires_action","message":"secret-stop"}},
		{"id":"sevt_2","type":"user.tool_confirmation","processed_at":"now","tool_use_id":"sevt_1","result":"allow","credential":"secret-work-token"},
		{"id":"sevt_1","type":"agent.tool_use","processed_at":"now","input":{"command":"secret-args"},"content":"secret-result"}
	],"next_page":null,"secret":"secret-environment-key"}`)
	summary, err := diagnosticEventPage(raw)
	require.NoError(t, err)
	require.Equal(t, "id=sevt_1 type=agent.tool_use processed=true\nid=sevt_2 type=user.tool_confirmation processed=true tool_use_id=sevt_1 result=allow\nid=sevt_3 type=session.status_idle processed=false stop_reason=requires_action\nhas_more=false", summary)
	require.NotContains(t, summary, "secret")

	summary, err = diagnosticEventPage([]byte(`{"data":[{"id":"secret-id\n","type":"secret-type","stop_reason":{"type":"secret-reason"}}]}`))
	require.NoError(t, err)
	require.Equal(t, "id=unknown type=unknown processed=false\nhas_more=false", summary)
	_, err = diagnosticEventPage(bytes.Repeat([]byte("x"), workDiagnosticBytes+1))
	require.Error(t, err)

	raw = []byte(`{"data":[
		{"id":"work_1","environment_id":"env_1","data":{"type":"session","id":"sesn_1"},"state":"active","latest_heartbeat_at":"2026-10-18T01:54:00Z","secret":"secret-token","metadata":{"credential":"secret-key"},"result":{"message":"secret-error"}},
		{"id":"work_2","environment_id":"env_1","data":{"type":"session","id":"sesn_2"},"state":"stopped"},
		{"id":"work_3","environment_id":"env_2","data":{"type":"session","id":"sesn_1"},"state":"stopped"},
		{"id":"work_4","environment_id":"env_1","data":{"type":"healthcheck","id":"sesn_1"},"state":"stopped"}
	],"next_page":null}`)
	summary, err = diagnosticWorkPage(raw, "env_1", "sesn_1")
	require.NoError(t, err)
	require.Equal(t, "id=work_1 state=active latest_heartbeat_at=2026-10-18T01:54:00Z\nhas_more=false", summary)
	require.NotContains(t, summary, "secret")
}

func TestWorkLifecycleLogsOmitAllDynamicFieldsAndBoundOutput(t *testing.T) {
	line := "2026/10/18 01:54:00 WARN event stream read failed; reconnecting component=session-tool-runner error=secret-work-token args=secret-command\n"
	summary := diagnosticLifecycleLogs([]byte("2026/10/18 01:54:00 INFO secret-custom-message token=secret-key\n" + strings.Repeat(line, 1000)))
	require.Equal(t, strings.TrimSpace(strings.Repeat("2026/10/18 01:54:00 WARN event stream read failed; reconnecting\n", 20)), summary)
	require.Less(t, len(summary), 2048)
	require.NotContains(t, summary, "secret")
	require.Equal(t, "no allowlisted lifecycle messages", diagnosticLifecycleLogs([]byte("secret-arbitrary-stdout\nnot/a/date INFO dispatched tool error=secret\n")))
	summary, err := diagnosticEventPage([]byte(`{"data":[` + strings.Repeat(`{"id":"sevt_1","type":"user.message"},`, 40) + `{"id":"sevt_1","type":"user.message"}]}`))
	require.NoError(t, err)
	require.Equal(t, 30, strings.Count(summary, "id=sevt_1"))
	require.Contains(t, summary, "has_more=true")
}

func TestWorkDiagnosticsInspectOwnershipBeforeReadingLogs(t *testing.T) {
	var logIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.55")
		assert.Equal(t, "GET", r.Method, "diagnostics must never mutate Docker")
		switch {
		case path == "/containers/json":
			assert.Equal(t, "1", r.URL.Query().Get("all"))
			_, _ = w.Write([]byte(`[{"Id":"owned"},{"Id":"other-session"},{"Id":"other-env"},{"Id":"other-image"},{"Id":"unmanaged"},{"Id":"no-work"}]`))
		case strings.HasSuffix(path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
			labels := map[string]string{"io.mango.self-hosted-worker": "true", "io.mango.environment-id": "env_1", "io.mango.session-id": "sesn_1", "io.mango.work-id": "work_1"}
			image := "worker:owned"
			switch id {
			case "other-session":
				labels["io.mango.session-id"] = "sesn_2"
			case "other-env":
				labels["io.mango.environment-id"] = "env_2"
			case "other-image":
				image = "worker:other"
			case "unmanaged":
				delete(labels, "io.mango.self-hosted-worker")
			case "no-work":
				delete(labels, "io.mango.work-id")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Config": map[string]any{"Image": image, "Labels": labels, "Env": []string{"secret-credential"}}, "State": map[string]any{"Running": true, "OOMKilled": false, "Error": "secret-engine-error"}})
		case strings.HasSuffix(path, "/logs"):
			logIDs = append(logIDs, strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/logs"))
			assert.Equal(t, "80", r.URL.Query().Get("tail"))
			assert.Empty(t, r.URL.Query().Get("follow"))
			payload := []byte("2026/10/18 01:54:00 INFO dispatched tool component=environment-worker error=secret-key\n")
			header := make([]byte, 8)
			header[0] = 2
			binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
			_, _ = w.Write(append(header, payload...))
		default:
			t.Errorf("unexpected diagnostic request %s", path)
		}
	}))
	defer server.Close()
	engine, err := client.New(client.WithHost(server.URL), client.WithAPIVersion("1.55"))
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	var output []string
	collectOwnedWorkDiagnostics(t.Context(), engine, "env_1", "sesn_1", "worker:owned", func(text string) { output = append(output, text) })
	require.Equal(t, []string{"owned"}, logIDs)
	require.Contains(t, strings.Join(output, "\n"), "item work_id=work_1 running=true paused=false oom_killed=false exit_code=0")
	require.Contains(t, strings.Join(output, "\n"), "INFO dispatched tool")
	require.NotContains(t, strings.Join(output, "\n"), "secret")
	require.Empty(t, diagnosticDockerLogPayload([]byte{2, 0, 0, 0, 255, 255, 255, 255}), "forged frame size must not allocate")
}

func TestWorkDiagnosticsRespectCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	engine, err := client.New(client.WithHost(server.URL), client.WithAPIVersion("1.55"))
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	started := time.Now()
	var output []string
	collectOwnedWorkDiagnostics(ctx, engine, "env_1", "sesn_1", "worker:owned", func(text string) { output = append(output, text) })
	require.Less(t, time.Since(started), time.Second)
	require.Equal(t, []string{"item diagnostics unavailable: container list failed"}, output)
}
