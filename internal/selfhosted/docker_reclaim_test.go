package selfhosted

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	mango "github.com/yanpgwang/mango/sdk/go"
)

func TestDockerLauncherReplacementKeepsStartingLeaseValid(t *testing.T) {
	work := acknowledgedWork()
	// Advance a logical clock instead of spending 31 seconds in a unit test.
	// The HTTP boundary independently enforces the default starting lease;
	// the Docker double models an old process requiring 31 seconds to stop.
	var elapsed atomic.Int64
	var heartbeats atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/heartbeat"):
			heartbeats.Add(1)
			if time.Duration(elapsed.Load()) > 30*time.Second {
				w.WriteHeader(http.StatusPreconditionFailed)
				_, _ = fmt.Fprint(w, `{"error":{"type":"precondition_failed","message":"work lease has expired"}}`)
				return
			}
			writeJSON(t, w, map[string]any{
				"type": "work_heartbeat", "last_heartbeat": "2026-09-08T00:00:01Z",
				"lease_extended": false, "state": "stopping", "ttl_seconds": 30,
			})
		case strings.HasSuffix(r.URL.Path, "/stop"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	sdkClient, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: "workspace-key"})
	if err != nil {
		t.Fatal(err)
	}
	engine := &slowReclaimedContainerEngine{
		fakeDockerEngine: newFakeDockerEngine(), elapsed: &elapsed,
		sdkClient: sdkClient, work: work, workdir: t.TempDir(),
	}
	engine.inspectErr = nil
	engine.inspectResult = inspectResultForWork("previous", work, true)
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: sdkClient, EnvironmentID: work.EnvironmentID, SandboxBaseURL: server.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.runItem(context.Background(), work); err != nil {
		t.Fatalf("replacement failed after %s: launcher=%v, worker=%v", time.Duration(elapsed.Load()), err, engine.itemErr)
	}
	if !engine.previousRemoved || heartbeats.Load() != 1 {
		t.Fatalf("previous removed=%v, replacement heartbeats=%d", engine.previousRemoved, heartbeats.Load())
	}
}

type slowReclaimedContainerEngine struct {
	*fakeDockerEngine
	elapsed         *atomic.Int64
	sdkClient       *mango.Client
	work            mango.EnvironmentWork
	workdir         string
	previousRemoved bool
	itemErr         error
}

func (e *slowReclaimedContainerEngine) ContainerStop(_ context.Context, _ string, opts client.ContainerStopOptions) (client.ContainerStopResult, error) {
	delay := 31 * time.Second
	if opts.Timeout != nil && *opts.Timeout >= 0 {
		delay = min(delay, time.Duration(*opts.Timeout)*time.Second)
	}
	e.elapsed.Add(int64(delay))
	return client.ContainerStopResult{}, nil
}

func (e *slowReclaimedContainerEngine) ContainerRemove(ctx context.Context, id string, opts client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if id == "previous" {
		if !opts.Force {
			return client.ContainerRemoveResult{}, fmt.Errorf("previous container is still running")
		}
		e.previousRemoved = true
	}
	return e.fakeDockerEngine.ContainerRemove(ctx, id, opts)
}

func (e *slowReclaimedContainerEngine) ContainerWait(ctx context.Context, _ string, _ client.ContainerWaitOptions) client.ContainerWaitResult {
	worker := mango.NewEnvironmentWorker(e.sdkClient, mango.EnvironmentWorkerOptions{Workdir: e.workdir, MemorySyncInterval: -1})
	e.itemErr = worker.HandleItem(ctx, mango.EnvironmentWorkerHandleItemOptions{
		WorkID: e.work.ID, EnvironmentID: e.work.EnvironmentID, SessionID: e.work.Data.ID, WorkSecret: *e.work.Secret,
	})
	result := make(chan container.WaitResponse, 1)
	status := int64(0)
	if e.itemErr != nil {
		status = 1
	}
	result <- container.WaitResponse{StatusCode: status}
	return client.ContainerWaitResult{Result: result, Error: make(chan error)}
}
