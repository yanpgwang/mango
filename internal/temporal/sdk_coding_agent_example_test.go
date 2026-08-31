package temporal_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/blob"
	"github.com/yanpgwang/mango/internal/controlplane"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/live"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
	"go.temporal.io/sdk/client"
)

// Run the exact documented Python program through real HTTP, authentication,
// PostgreSQL, Temporal, NATS, object storage and Docker. Only inference is a
// deterministic fixture; the SDK has no access to runtime internals.
func TestCodingAgentSDKExample(t *testing.T) {
	if os.Getenv("MANGO_TEST_EXAMPLES") != "1" {
		t.Skip("run make test-coding-agent-example after installing the Python SDK")
	}
	for _, reconnect := range []bool{false, true} {
		name := "normal"
		if reconnect {
			name = "disconnect_and_resume"
		}
		t.Run(name, func(t *testing.T) {
			runCodingAgentSDKExample(t, iterateProbeModel{outputPath: "repaired_calc.py"}, "fake", reconnect)
		})
	}
}

func TestLiveModelCodingAgentSDKExample(t *testing.T) {
	modelClient, modelID := liveModelForTest(t, "Python SDK coding-agent example")
	runCodingAgentSDKExample(t, modelClient, modelID, false)
}

func runCodingAgentSDKExample(t *testing.T, modelClient model.Client, modelID string, reconnect bool) {
	t.Helper()
	requireIterateServices(t)
	for _, key := range []string{"MANGO_TEST_NATS_URL", "MANGO_TEST_S3_ENDPOINT", "MANGO_TEST_S3_BUCKET"} {
		require.NotEmpty(t, os.Getenv(key), "set %s via the example Make target", key)
	}
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	python := filepath.Join(root, "sdk/python/.venv/bin/python")
	_, err = os.Stat(python)
	require.NoError(t, err, "install the checkout SDK with uv sync --project sdk/python --frozen")
	ctx := context.Background()
	store, cleanup := integrationStore(t, os.Getenv("MANGO_TEST_DATABASE_URL"))
	defer cleanup()
	const key = "coding-agent-sdk-example-test-key"
	require.NoError(t, store.BootstrapAPIKey(ctx, key))
	tc, err := client.Dial(client.Options{HostPort: os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")})
	require.NoError(t, err)
	defer tc.Close()
	provider := iterateDockerProvider(t, dockerRequired)
	broker, err := live.Connect(os.Getenv("MANGO_TEST_NATS_URL"))
	require.NoError(t, err)
	defer broker.Close()
	store.SetEventNotifier(broker)
	blobs, err := blob.NewS3Store(ctx, blob.S3Config{
		Endpoint: os.Getenv("MANGO_TEST_S3_ENDPOINT"), Bucket: os.Getenv("MANGO_TEST_S3_BUCKET"),
		AccessKey: os.Getenv("MANGO_TEST_S3_ACCESS_KEY"), SecretKey: os.Getenv("MANGO_TEST_S3_SECRET_KEY"),
		UsePathStyle: true, CreateBucket: true,
	})
	require.NoError(t, err)
	ids := domain.NewRandomIDGen()
	clock := realClock{}
	fileRepo := pg.NewFileRepository(store)
	files := app.NewFileService(fileRepo, blobs, ids, clock)
	resources := controlplane.NewSessionResourceService(store, fileRepo, blobs, ids, clock, true)
	materializer := app.NewSessionRuntimeMaterializer(
		app.NewSessionResourceMaterializer(store, fileRepo, blobs), nil,
	).WithSessionOutputPublisher(app.NewSessionOutputPublisher(fileRepo, blobs, ids, clock))
	runtime := temporalpkg.NewRuntime(temporalpkg.RuntimeConfig{
		TemporalClient: tc, Store: store, ModelClient: modelClient,
		SandboxProvider: provider, IDGenerator: ids, Resources: materializer,
		TaskQueue:   "sdk-iterate-" + ids.NewID(""),
		RelayConfig: temporalpkg.RelayConfig{PollInterval: 20 * time.Millisecond},
	})
	stop := startGateRuntime(t, ctx, runtime)
	defer stop()
	agentRepo := pg.NewAgentRepository(store)
	environmentRepo := pg.NewEnvironmentRepository(store)
	agents := app.NewAgentService(agentRepo, ids, clock)
	environments := app.NewEnvironmentService(environmentRepo, ids, clock)
	sessions := controlplane.NewSessionService(store, agentRepo, environmentRepo, runtime.Orchestrator(), ids, clock, nil, resources)
	// All rows belong to this isolated test schema. Cleanup runs even when the
	// Python client failed before receiving an HTTP mutation response.
	defer func() {
		remaining, listErr := sessions.List(ctx, app.ListPage{Limit: 100})
		require.NoError(t, listErr)
		for _, session := range remaining.Sessions {
			require.NoError(t, sessions.Delete(ctx, session.ID))
		}
		remainingFiles, listErr := files.List(ctx, app.FileListQuery{Limit: 100})
		require.NoError(t, listErr)
		for _, file := range remainingFiles.Files {
			_, deleteErr := files.Delete(ctx, file.ID)
			require.NoError(t, deleteErr)
		}
	}()
	handler := httpapi.NewServer(httpapi.Deps{
		Agents: agents, Envs: environments, Sessions: sessions, Files: files,
		SessionResources: resources, Events: controlplane.NewEventService(store),
		Stream: live.NewStream(store, broker, ids, clock, 50*time.Millisecond),
	}, httpapi.Config{RequireAuth: true, Authenticator: store}).Handler()
	var streams, sends, uploads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/events") {
			sends.Add(1)
		}
		if r.Method == http.MethodPost && r.URL.Path == "/v1/files" {
			uploads.Add(1)
		}
		if strings.HasSuffix(r.URL.Path, "/events/stream") && streams.Add(1) == 1 && reconnect {
			// Fault injection at the connection boundary, never fake history:
			// accept a stream and immediately end it before it can deliver events.
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			return
		}
		handler.ServeHTTP(w, r)
	}))
	defer server.Close()
	invoke := func(arguments ...string) string {
		t.Helper()
		commandCtx, cancel := context.WithTimeout(ctx, 6*time.Minute)
		defer cancel()
		args := append([]string{filepath.Join(root, "examples/coding-agent/main.py"), "--model", modelID, "--output-dir", t.TempDir()}, arguments...)
		command := exec.CommandContext(commandCtx, python, args...)
		command.Dir = root
		for _, entry := range os.Environ() {
			name, _, _ := strings.Cut(entry, "=")
			if strings.HasPrefix(name, "MANGO_") || strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "OPENAI_") {
				continue
			}
			command.Env = append(command.Env, entry)
		}
		command.Env = append(command.Env, "MANGO_BASE_URL="+server.URL, "MANGO_API_KEY="+key, "PYTHONUNBUFFERED=1")
		output, runErr := command.CombinedOutput()
		require.NotContains(t, string(output), key, "example must never print credentials")
		require.NoError(t, runErr, "%s", output)
		require.Contains(t, string(output), "Independent verification passed. Coding-agent example completed.")
		t.Log(string(output))
		return string(output)
	}
	if reconnect {
		invoke("--keep-resources")
		page, err := sessions.List(ctx, app.ListPage{Limit: 100})
		require.NoError(t, err)
		require.Len(t, page.Sessions, 1)
		invoke("--session-id", page.Sessions[0].ID)
		require.GreaterOrEqual(t, streams.Load(), int64(3))
		stillPresent, err := sessions.List(ctx, app.ListPage{Limit: 100})
		require.NoError(t, err)
		require.Len(t, stillPresent.Sessions, 1, "resuming does not delete resources it did not create")
	} else {
		invoke()
		page, err := sessions.List(ctx, app.ListPage{Limit: 100})
		require.NoError(t, err)
		require.Empty(t, page.Sessions, "successful example must delete its Session")
		remainingFiles, err := files.List(ctx, app.FileListQuery{Limit: 100})
		require.NoError(t, err)
		require.Empty(t, remainingFiles.Files, "successful example must clean uploads and scoped output")
	}
	require.Equal(t, int64(1), sends.Load(), "recovery must never resubmit the task")
	require.Equal(t, int64(2), uploads.Load(), "the SDK uploads both real fixtures")
}
