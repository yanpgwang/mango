package kubernetes_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWorkflowRestoreRejectsReconstruction(t *testing.T) {
	first, second := sha256.Sum256([]byte("original event")), sha256.Sum256([]byte("resume event"))
	before := workflowSnapshot{runID: "original-run", history: [][32]byte{first}}
	require.NoError(t, verifyWorkflowPrefix(before, workflowSnapshot{runID: "original-run", history: [][32]byte{first, second}}))
	for _, after := range []workflowSnapshot{
		{runID: "reconstructed-run", history: [][32]byte{first, second}},
		{runID: "original-run"},
		{runID: "original-run", history: [][32]byte{second}},
	} {
		require.Error(t, verifyWorkflowPrefix(before, after))
	}
}

func TestOwnedWorkCleanupVerifiesInspectedIdentity(t *testing.T) {
	// The list deliberately returns unrelated containers and lies about one
	// summary. Cleanup must trust the inspected identity, never list filters.
	var removed []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1.55")
		switch {
		case r.Method == "GET" && path == "/containers/json":
			assert.Equal(t, "1", r.URL.Query().Get("all"))
			_, _ = w.Write([]byte(`[{"Id":"owned"},{"Id":"other-session"},{"Id":"other-env"},{"Id":"other-image"},{"Id":"unmanaged"},{"Id":"no-work"}]`))
		case r.Method == "GET" && strings.HasSuffix(path, "/json"):
			id := strings.TrimSuffix(strings.TrimPrefix(path, "/containers/"), "/json")
			labels := map[string]string{"io.mango.self-hosted-worker": "true", "io.mango.environment-id": "env-owned", "io.mango.session-id": "session-owned", "io.mango.work-id": "work-owned"}
			image := "worker:fixture-owned"
			switch id {
			case "other-session":
				labels["io.mango.session-id"] = "session-unrelated"
			case "other-env":
				labels["io.mango.environment-id"] = "env-unrelated"
			case "other-image":
				image = "worker:unrelated"
			case "unmanaged":
				delete(labels, "io.mango.self-hosted-worker")
			case "no-work":
				delete(labels, "io.mango.work-id")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Id": id, "Config": map[string]any{"Image": image, "Labels": labels}})
		case r.Method == "DELETE":
			assert.Equal(t, "1", r.URL.Query().Get("force"))
			assert.Empty(t, r.URL.Query().Get("v"), "container cleanup must retain workspace volumes")
			mu.Lock()
			removed = append(removed, strings.TrimPrefix(path, "/containers/"))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected Docker request %s %s", r.Method, path)
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	engine, err := client.New(client.WithHost(server.URL), client.WithAPIVersion("1.55"))
	require.NoError(t, err)
	defer func() { require.NoError(t, engine.Close()) }()
	count, err := removeOwnedWork(context.Background(), engine, "env-owned", "session-owned", "worker:fixture-owned")
	require.NoError(t, err)
	require.Equal(t, 1, count)
	mu.Lock()
	require.Equal(t, []string{"owned"}, removed)
	mu.Unlock()
}

func (c *clusterFixture) verifyOwnedWorkCleanup() {
	t := c.t
	t.Helper()
	t.Log("Checking paused orphan cleanup and preservation of an unrelated item")
	engine, err := client.New(client.FromEnv)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, engine.Close()) })
	_, err = engine.Ping(c.ctx, client.PingOptions{NegotiateAPIVersion: true})
	require.NoError(t, err)
	volume := c.name + "-cleanup-volume"
	t.Cleanup(func() { c.removeWorkspace(volume) })
	var ids []string
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, id := range ids {
			_, err := engine.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
			if err != nil && !errdefs.IsNotFound(err) {
				t.Errorf("cleanup test-owned orphan witness: %v", err)
			}
		}
	})
	for _, environment := range []string{c.name, c.name + "-unrelated"} {
		id := strings.TrimSpace(string(c.command(nil, nil, "docker", "create", "--label", "io.mango.self-hosted-worker=true",
			"--label", "io.mango.environment-id="+environment, "--label", "io.mango.session-id="+c.name,
			"--label", "io.mango.work-id="+environment, "--mount", "type=volume,src="+volume+",dst=/workspace",
			"--entrypoint", "sh", c.workerImage, "-c", "sleep 300")))
		ids = append(ids, id)
		c.command(nil, nil, "docker", "start", id)
		c.command(nil, nil, "docker", "pause", id)
	}
	ctx, cancel := context.WithTimeout(c.ctx, 30*time.Second)
	defer cancel()
	count, err := removeOwnedWork(ctx, engine, c.name, c.name, c.workerImage)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	_, err = engine.ContainerInspect(ctx, ids[0], client.ContainerInspectOptions{})
	require.True(t, errdefs.IsNotFound(err), "owned paused orphan was not removed")
	sentinel, err := engine.ContainerInspect(ctx, ids[1], client.ContainerInspectOptions{})
	require.NoError(t, err)
	require.True(t, sentinel.Container.State.Paused, "unrelated item was changed")
}
