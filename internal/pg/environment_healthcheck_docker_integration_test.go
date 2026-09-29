package pg

import (
	"context"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/selfhosted"
	"github.com/yanpgwang/mango/internal/workspace"
	mango "github.com/yanpgwang/mango/sdk/go"
)

// This is the complete operator journey: real API, PostgreSQL, supervisor key,
// claimed per-Work secret and the installed worker process in a Docker sandbox.
// No Session/model/event service is configured; using any would fail the check.
func TestEnvironmentHealthcheckDockerExecutionAndCleanup(t *testing.T) {
	if os.Getenv("MANGO_TEST_DOCKER") != "1" {
		t.Skip("set MANGO_TEST_DOCKER=1 to require Docker")
	}
	image := os.Getenv("MANGO_TEST_WORKER_IMAGE")
	if image == "" {
		t.Fatal("MANGO_TEST_WORKER_IMAGE is required")
	}
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	envs := NewEnvironmentRepository(store)
	now := time.Now().UTC()
	if err := envs.Put(ctx, domain.Environment{ID: "env_healthcheck", Name: "healthcheck", ConfigType: "self_hosted", Config: map[string]any{"type": "self_hosted"}, CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	_, operatorKey, err := store.CreateAPIKey(ctx, workspace.DefaultID, "operator")
	if err != nil {
		t.Fatal(err)
	}
	_, supervisorKey, err := store.CreateEnvironmentKey(ctx, workspace.DefaultID, "env_healthcheck", "supervisor")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(httpapi.NewServer(httpapi.Deps{EnvironmentWork: app.NewEnvironmentWorkService(NewEnvironmentWorkRepository(store), envs)}, httpapi.Config{RequireAuth: true, Authenticator: store}).Handler())
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	defer server.Close()
	operator, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: operatorKey})
	if err != nil {
		t.Fatal(err)
	}
	supervisor, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: supervisorKey})
	if err != nil {
		t.Fatal(err)
	}
	check, err := operator.Environments.Work.New(ctx, "env_healthcheck", mango.EnvironmentWorkCreateRequest{Data: mango.HealthcheckWorkData{Type: "healthcheck"}})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = engine.Close() }()
	if _, err := engine.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		t.Fatal(err)
	}
	launcher, err := selfhosted.NewDockerLauncher(engine, selfhosted.DockerLauncherOptions{Client: supervisor, EnvironmentID: "env_healthcheck", WorkerID: "healthcheck-test", Image: image, Drain: true, BlockMs: mango.Some[int64](1), SandboxBaseURL: fmt.Sprintf("http://host.docker.internal:%d", listener.Addr().(*net.TCPAddr).Port)})
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.Run(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := operator.Environments.Work.Get(ctx, "env_healthcheck", check.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "stopped" || result.Result == nil || result.Result.Status != "succeeded" || result.StartedAt == nil || result.StoppedAt == nil || result.Secret != nil {
		t.Fatalf("healthcheck did not execute: %+v", result)
	}
	var sessions int
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&sessions); err != nil || sessions != 0 {
		t.Fatalf("Sessions=%d err=%v", sessions, err)
	}
	containers, err := engine.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: client.Filters{}.Add("label", "io.mango.work-id="+check.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if len(containers.Items) != 0 {
		t.Fatalf("healthcheck leaked %d containers", len(containers.Items))
	}
}
