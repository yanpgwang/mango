package selfhosted

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	mango "github.com/yanpgwang/mango/sdk/go"
)

func TestDockerLauncherPollsWithSupervisorAndTransportsOnlyWorkSecret(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer workspace-key" {
			t.Errorf("supervisor Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/environments/env_test/work/poll":
			mu.Lock()
			polls++
			call := polls
			mu.Unlock()
			if call == 1 {
				writeJSON(t, w, workFixture("queued", workSecret))
			} else {
				writeJSON(t, w, map[string]any{})
			}
		case r.Method == http.MethodPost && r.URL.Path == "/v1/environments/env_test/work/work_test/ack":
			writeJSON(t, w, workFixture("starting", ""))
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()

	supervisor, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: "workspace-key"})
	if err != nil {
		t.Fatal(err)
	}
	engine := newFakeDockerEngine()
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: supervisor, EnvironmentID: "env_test", WorkerID: "worker_test",
		SandboxBaseURL: "http://host.docker.internal:8080", Drain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.created) != 1 {
		t.Fatalf("created containers = %d, want 1", len(engine.created))
	}
	created := engine.created[0]
	environment := strings.Join(created.Config.Env, "\n")
	for _, expected := range []string{
		"MANGO_WORK_ID=work_test",
		"MANGO_SESSION_ID=sesn_test",
		"MANGO_BASE_URL=http://host.docker.internal:8080",
		"MANGO_WORKER_MAX_IDLE=0s",
	} {
		if !strings.Contains(environment, expected) {
			t.Errorf("container environment missing %q: %s", expected, environment)
		}
	}
	if strings.Contains(environment, "workspace-key") || strings.Contains(environment, workSecret) ||
		strings.Contains(environment, "MANGO_API_KEY") || strings.Contains(environment, "MANGO_WORK_SECRET") {
		t.Fatalf("credential crossed into container environment: %s", environment)
	}
	if !created.Config.AttachStdin || !created.Config.OpenStdin || !created.Config.StdinOnce {
		t.Fatalf("container stdin transport = %+v", created.Config)
	}
	select {
	case encoded := <-engine.attachedInput:
		secret, err := ReadWorkSecret(bytes.NewReader(encoded))
		if err != nil || secret != workSecret {
			t.Fatalf("attached Work secret = %q, %v", secret, err)
		}
	case <-time.After(time.Second):
		t.Fatal("Work secret was not written to attached stdin")
	}
	if !engine.attachOptions.Stream || !engine.attachOptions.Stdin || engine.attachOptions.Stdout || engine.attachOptions.Stderr {
		t.Fatalf("ContainerAttach options = %+v", engine.attachOptions)
	}
	if !created.HostConfig.ReadonlyRootfs || len(created.HostConfig.CapDrop) != 1 || created.HostConfig.CapDrop[0] != "ALL" {
		t.Fatalf("container hardening = %+v", created.HostConfig)
	}
	if got := created.HostConfig.Mounts[0].Source; got != dockerSessionVolume("sesn_test") {
		t.Fatalf("workspace volume = %q", got)
	}
}

func TestDockerLauncherReplacesMatchingPreviousAttempt(t *testing.T) {
	client, _ := mango.New(mango.Config{BaseURL: "http://mango.invalid", APIKey: "workspace"})
	work := acknowledgedWork()
	engine := newFakeDockerEngine()
	engine.inspectErr = nil
	engine.inspectResult = inspectResultForWork("previous", work, true)
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: client, EnvironmentID: "env_test", SandboxBaseURL: "http://mango.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.runItem(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if engine.stopCalls != 1 || engine.removeCalls != 2 {
		t.Fatalf("stop calls=%d remove calls=%d, want 1 and 2", engine.stopCalls, engine.removeCalls)
	}
}

func TestDockerLauncherForceRemovesPreviousAttemptWhenStopFails(t *testing.T) {
	client, _ := mango.New(mango.Config{BaseURL: "http://mango.invalid", APIKey: "workspace"})
	work := acknowledgedWork()
	engine := newFakeDockerEngine()
	engine.inspectErr = nil
	engine.inspectResult = inspectResultForWork("previous", work, true)
	engine.stopErr = errors.New("graceful stop failed")
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: client, EnvironmentID: "env_test", SandboxBaseURL: "http://mango.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.runItem(context.Background(), work); err != nil {
		t.Fatal(err)
	}
	if engine.stopCalls != 1 || engine.removeCalls != 2 || len(engine.created) != 1 {
		t.Fatalf("stops=%d removes=%d created=%d, want 1, 2, 1", engine.stopCalls, engine.removeCalls, len(engine.created))
	}
}

func TestDockerLauncherRejectsPreviousAttemptFromAnotherEnvironment(t *testing.T) {
	client, _ := mango.New(mango.Config{BaseURL: "http://mango.invalid", APIKey: "workspace"})
	work := acknowledgedWork()
	other := work
	other.EnvironmentID = "env_other"
	engine := newFakeDockerEngine()
	engine.inspectErr = nil
	engine.inspectResult = inspectResultForWork("previous", other, true)
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: client, EnvironmentID: "env_test", SandboxBaseURL: "http://mango.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.runItem(context.Background(), work); err == nil || !strings.Contains(err.Error(), "unrelated container") {
		t.Fatalf("runItem error = %v", err)
	}
	if len(engine.created) != 0 || engine.stopCalls != 0 || engine.removeCalls != 0 {
		t.Fatalf("mutated unrelated container: created=%d stops=%d removes=%d", len(engine.created), engine.stopCalls, engine.removeCalls)
	}
}

func TestDockerLauncherReportsContainerCleanupFailure(t *testing.T) {
	client, _ := mango.New(mango.Config{BaseURL: "http://mango.invalid", APIKey: "workspace"})
	engine := newFakeDockerEngine()
	engine.removeErr = errors.New("remove failed")
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: client, EnvironmentID: "env_test", SandboxBaseURL: "http://mango.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = launcher.runItem(context.Background(), acknowledgedWork())
	if err == nil || !strings.Contains(err.Error(), "remove Work container") {
		t.Fatalf("runItem error = %v", err)
	}
}

func TestDockerLauncherCleansUpContainerWhenSecretInputAttachFails(t *testing.T) {
	client, _ := mango.New(mango.Config{BaseURL: "http://mango.invalid", APIKey: "workspace"})
	engine := newFakeDockerEngine()
	engine.attachErr = errors.New("attach failed")
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: client, EnvironmentID: "env_test", SandboxBaseURL: "http://mango.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = launcher.runItem(context.Background(), acknowledgedWork())
	if err == nil || !strings.Contains(err.Error(), "attach Work secret input") {
		t.Fatalf("runItem error = %v", err)
	}
	if engine.removeCalls != 1 {
		t.Fatalf("ContainerRemove calls = %d, want 1", engine.removeCalls)
	}
	select {
	case <-engine.started:
		t.Fatal("container started after input attach failed")
	default:
	}
}

func TestDockerLauncherReconcilesAmbiguousCommittedStart(t *testing.T) {
	supervisor, _ := mango.New(mango.Config{BaseURL: "http://mango.invalid", APIKey: "workspace"})
	engine := newFakeDockerEngine()
	engine.startErr = context.DeadlineExceeded
	engine.inspectErr = nil
	engine.inspectResult = client.ContainerInspectResult{Container: container.InspectResponse{
		ID: "container_test", State: &container.State{Status: container.StateRunning, Running: true},
	}}
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: supervisor, EnvironmentID: "env_test", SandboxBaseURL: "http://mango.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.startContainer(context.Background(), "container_test"); err != nil {
		t.Fatalf("reconcile committed Start: %v", err)
	}
}

func TestDockerLauncherRejectsAmbiguousUncommittedStart(t *testing.T) {
	supervisor, _ := mango.New(mango.Config{BaseURL: "http://mango.invalid", APIKey: "workspace"})
	engine := newFakeDockerEngine()
	engine.startErr = context.DeadlineExceeded
	engine.inspectErr = nil
	engine.inspectResult = client.ContainerInspectResult{Container: container.InspectResponse{
		ID: "container_test", State: &container.State{Status: container.StateCreated},
	}}
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: supervisor, EnvironmentID: "env_test", SandboxBaseURL: "http://mango.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := launcher.startContainer(context.Background(), "container_test"); err == nil || !strings.Contains(err.Error(), "start Work container") {
		t.Fatalf("startContainer error = %v", err)
	}
}

func TestDockerLauncherReconcilesAmbiguousCommittedCreate(t *testing.T) {
	supervisor, _ := mango.New(mango.Config{BaseURL: "http://mango.invalid", APIKey: "workspace"})
	work := acknowledgedWork()
	engine := newFakeDockerEngine()
	engine.createErr = context.DeadlineExceeded
	engine.inspectErr = nil
	engine.inspectResult = inspectResultForWork("container_reconciled", work, false)
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: supervisor, EnvironmentID: "env_test", SandboxBaseURL: "http://mango.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := launcher.createContainer(context.Background(), dockerWorkName(work.ID), work, client.ContainerCreateOptions{})
	if err != nil || id != "container_reconciled" {
		t.Fatalf("createContainer id=%q error=%v", id, err)
	}
}

func TestDockerLauncherRunReportsShutdownCleanupFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/work/poll"):
			writeJSON(t, w, workFixture("queued", workSecret))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ack"):
			writeJSON(t, w, workFixture("starting", ""))
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer server.Close()
	supervisor, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	engine := newFakeDockerEngine()
	engine.waitImmediately = false
	engine.stopErr = errors.New("stop failed")
	engine.removeErr = errors.New("remove failed")
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: supervisor, EnvironmentID: "env_test", SandboxBaseURL: "http://mango.invalid", Drain: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- launcher.Run(ctx) }()
	select {
	case <-engine.started:
	case <-time.After(time.Second):
		t.Fatal("container did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "stop Work container") || !strings.Contains(err.Error(), "remove Work container") {
			t.Fatalf("Run error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not report cleanup failure")
	}
}

func TestDockerLauncherStopsContainerWhenSupervisorIsCancelled(t *testing.T) {
	client, err := mango.New(mango.Config{BaseURL: "http://mango.invalid", APIKey: "workspace"})
	if err != nil {
		t.Fatal(err)
	}
	engine := newFakeDockerEngine()
	engine.waitImmediately = false
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: client, EnvironmentID: "env_test", SandboxBaseURL: "http://mango.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- launcher.runItem(ctx, acknowledgedWork()) }()
	select {
	case <-engine.started:
	case <-time.After(time.Second):
		t.Fatal("container did not start")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runItem did not stop after cancellation")
	}
	if engine.stopCalls != 1 {
		t.Fatalf("ContainerStop calls = %d, want 1", engine.stopCalls)
	}
}

func TestDockerLauncherReportsNonZeroContainerExitWithoutLeakingSecret(t *testing.T) {
	client, _ := mango.New(mango.Config{BaseURL: "http://mango.invalid", APIKey: "workspace"})
	engine := newFakeDockerEngine()
	engine.waitStatus = 17
	launcher, err := NewDockerLauncher(engine, DockerLauncherOptions{
		Client: client, EnvironmentID: "env_test", SandboxBaseURL: "http://mango.invalid",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = launcher.runItem(context.Background(), acknowledgedWork())
	if err == nil || !strings.Contains(err.Error(), "status 17") {
		t.Fatalf("runItem error = %v", err)
	}
	if strings.Contains(err.Error(), workSecret) {
		t.Fatal("runItem error leaked Work secret")
	}
}

const workSecret = "eyJzZXNzaW9uc190b2tlbiI6InNlc3NfdGVzdCJ9"

func acknowledgedWork() mango.EnvironmentWork {
	secret := workSecret
	return mango.EnvironmentWork{
		ID: "work_test", EnvironmentID: "env_test", State: mango.EnvironmentWorkStateStarting,
		Data: mango.EnvironmentWorkData{Type: "session", ID: "sesn_test"}, Secret: &secret,
	}
}

func workFixture(state, secret string) map[string]any {
	secretValue := any(nil)
	if secret != "" {
		secretValue = secret
	}
	return map[string]any{
		"id": "work_test", "type": "work", "environment_id": "env_test",
		"data":  map[string]any{"type": "session", "id": "sesn_test"},
		"state": state, "metadata": map[string]string{}, "secret": secretValue,
		"created_at": "2026-09-04T00:00:00Z", "acknowledged_at": nil,
		"started_at": nil, "latest_heartbeat_at": nil,
		"stop_requested_at": nil, "stopped_at": nil,
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Error(err)
	}
}

type fakeDockerEngine struct {
	mu              sync.Mutex
	created         []client.ContainerCreateOptions
	attachedInput   chan []byte
	attachOptions   client.ContainerAttachOptions
	waitImmediately bool
	waitStatus      int64
	waitResult      chan container.WaitResponse
	waitError       chan error
	started         chan struct{}
	stopCalls       int
	removeCalls     int
	stopErr         error
	startErr        error
	createErr       error
	attachErr       error
	removeErr       error
	inspectResult   client.ContainerInspectResult
	inspectErr      error
}

func newFakeDockerEngine() *fakeDockerEngine {
	return &fakeDockerEngine{
		waitImmediately: true,
		started:         make(chan struct{}, 1),
		attachedInput:   make(chan []byte, 8),
		inspectErr:      errdefs.ErrNotFound,
	}
}

func (f *fakeDockerEngine) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	return client.ImageInspectResult{}, nil
}

func (f *fakeDockerEngine) ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	return f.inspectResult, f.inspectErr
}

func (f *fakeDockerEngine) ContainerCreate(_ context.Context, options client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	f.mu.Lock()
	f.created = append(f.created, options)
	f.mu.Unlock()
	return client.ContainerCreateResult{ID: "container_test"}, f.createErr
}

func (f *fakeDockerEngine) ContainerAttach(_ context.Context, _ string, options client.ContainerAttachOptions) (client.ContainerAttachResult, error) {
	f.mu.Lock()
	f.attachOptions = options
	err := f.attachErr
	f.mu.Unlock()
	if err != nil {
		return client.ContainerAttachResult{}, err
	}
	launcher, daemon := net.Pipe()
	go func() {
		defer func() { _ = daemon.Close() }()
		encoded, _ := io.ReadAll(daemon)
		f.attachedInput <- encoded
	}()
	return client.ContainerAttachResult{HijackedResponse: client.NewHijackedResponse(launcher, "")}, nil
}

func (f *fakeDockerEngine) ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error) {
	select {
	case f.started <- struct{}{}:
	default:
	}
	return client.ContainerStartResult{}, f.startErr
}

func (f *fakeDockerEngine) ContainerWait(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult {
	result := make(chan container.WaitResponse, 1)
	errorsCh := make(chan error, 1)
	f.mu.Lock()
	f.waitResult, f.waitError = result, errorsCh
	immediate, status := f.waitImmediately, f.waitStatus
	f.mu.Unlock()
	if immediate {
		result <- container.WaitResponse{StatusCode: status}
	}
	return client.ContainerWaitResult{Result: result, Error: errorsCh}
}

func (f *fakeDockerEngine) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	f.mu.Lock()
	f.stopCalls++
	result := f.waitResult
	err := f.stopErr
	f.mu.Unlock()
	if err != nil {
		return client.ContainerStopResult{}, err
	}
	if result != nil {
		select {
		case result <- container.WaitResponse{}:
		default:
		}
	}
	return client.ContainerStopResult{}, nil
}

func (f *fakeDockerEngine) ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeCalls++
	return client.ContainerRemoveResult{}, f.removeErr
}

func inspectResultForWork(id string, work mango.EnvironmentWork, running bool) client.ContainerInspectResult {
	return client.ContainerInspectResult{Container: container.InspectResponse{
		ID: id,
		Config: &container.Config{Labels: map[string]string{
			dockerManagedLabel: "true", dockerWorkIDLabel: work.ID,
			dockerSessionIDLabel: work.Data.ID, dockerEnvironmentLabel: work.EnvironmentID,
		}},
		State: &container.State{Running: running},
	}}
}

var _ dockerEngine = (*fakeDockerEngine)(nil)
