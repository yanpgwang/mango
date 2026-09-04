// mango-worker is the standalone worker-side runtime for self_hosted Mango
// Environments. The docker command is a trusted supervisor that Polls/Acks and
// launches one sandbox per Work item. The run command executes only inside a
// launcher-marked sandbox and uses the per-Work credential.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/moby/moby/client"
	"github.com/yanpgwang/mango/internal/selfhosted"
	mango "github.com/yanpgwang/mango/sdk/go"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, os.Args[1:]); err != nil {
		slog.Error("mango worker stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 {
		return errors.New("usage: mango-worker <docker|run> [flags]")
	}
	switch arguments[0] {
	case "docker":
		return runDocker(ctx, arguments[1:])
	case "run":
		return runItem(ctx, arguments[1:])
	case "help", "-h", "--help":
		_, err := fmt.Fprintln(os.Stdout, "usage: mango-worker <docker|run> [flags]")
		return err
	default:
		return fmt.Errorf("unknown mango-worker command %q", arguments[0])
	}
}

func runDocker(ctx context.Context, arguments []string) error {
	flags := flag.NewFlagSet("mango-worker docker", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	environmentID := flags.String("environment-id", os.Getenv("MANGO_ENVIRONMENT_ID"), "self-hosted Environment ID")
	baseURL := flags.String("base-url", envOr("MANGO_BASE_URL", "http://localhost:8080"), "Mango URL visible to the supervisor")
	sandboxBaseURL := flags.String("sandbox-base-url", os.Getenv("MANGO_DOCKER_BASE_URL"), "Mango URL visible inside Docker")
	workerID := flags.String("worker-id", defaultWorkerID(), "queue worker identifier")
	image := flags.String("image", envOr("MANGO_WORKER_IMAGE", selfhosted.DefaultWorkerImage), "worker sandbox image")
	network := flags.String("network", envOr("MANGO_WORKER_NETWORK", "bridge"), "Docker network mode or network name")
	user := flags.String("user", envOr("MANGO_WORKER_USER", "65532:65532"), "sandbox uid:gid")
	drain := flags.Bool("drain", false, "exit once the queue is empty")
	maxIdle := flags.Duration("max-idle", envDuration("MANGO_WORKER_MAX_IDLE", time.Minute), "idle time after end_turn")
	memoryBytes := flags.Int64("memory-bytes", envInt64("MANGO_WORKER_MEMORY_BYTES", 1<<30), "per-sandbox memory limit")
	nanoCPUs := flags.Int64("nano-cpus", envInt64("MANGO_WORKER_NANO_CPUS", 1_000_000_000), "per-sandbox CPU limit in nano CPUs")
	pidsLimit := flags.Int64("pids-limit", envInt64("MANGO_WORKER_PIDS_LIMIT", 256), "per-sandbox process limit")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("mango-worker docker does not accept positional arguments")
	}
	apiKey := os.Getenv("MANGO_API_KEY")
	if apiKey == "" {
		return errors.New("MANGO_API_KEY is required by the trusted Docker supervisor")
	}
	supervisor, err := mango.New(mango.Config{BaseURL: *baseURL, APIKey: apiKey})
	if err != nil {
		return err
	}
	engine, err := client.New(client.FromEnv)
	if err != nil {
		return fmt.Errorf("create Docker Engine client: %w", err)
	}
	defer func() { _ = engine.Close() }()
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_, err = engine.Ping(probeCtx, client.PingOptions{NegotiateAPIVersion: true})
	cancel()
	if err != nil {
		return fmt.Errorf("docker engine is unreachable: %w", err)
	}
	launcher, err := selfhosted.NewDockerLauncher(engine, selfhosted.DockerLauncherOptions{
		Client: supervisor, EnvironmentID: *environmentID, WorkerID: *workerID,
		Image: *image, SandboxBaseURL: *sandboxBaseURL, NetworkMode: *network,
		User: *user, Drain: *drain, MaxIdle: *maxIdle,
		MemoryBytes: *memoryBytes, NanoCPUs: *nanoCPUs, PidsLimit: *pidsLimit,
	})
	if err != nil {
		return err
	}
	slog.Info("starting Docker self-hosted worker", "environment_id", *environmentID, "worker_id", *workerID, "image", *image)
	return launcher.Run(ctx)
}

func runItem(ctx context.Context, arguments []string) (runErr error) {
	flags := flag.NewFlagSet("mango-worker run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	workdir := flags.String("workdir", envOr("MANGO_WORKDIR", "/workspace"), "sandbox workspace")
	maxIdle := flags.Duration("max-idle", envDuration("MANGO_WORKER_MAX_IDLE", time.Minute), "idle time after end_turn")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("mango-worker run does not accept positional arguments")
	}
	baseURL := envOr("MANGO_BASE_URL", "http://localhost:8080")
	itemClient, err := mango.New(mango.Config{BaseURL: baseURL})
	if err != nil {
		return err
	}
	toolset, err := selfhosted.SandboxTools(*workdir)
	if err != nil {
		return err
	}
	defer func() {
		if err := selfhosted.CloseSandboxTools(toolset); err != nil {
			runErr = errors.Join(runErr, err)
		}
	}()
	worker := mango.NewEnvironmentWorker(itemClient, mango.EnvironmentWorkerOptions{
		Tools: toolset, MaxIdle: maxIdle,
	})
	return worker.HandleItem(ctx, mango.EnvironmentWorkerHandleItemOptions{})
}

func defaultWorkerID() string {
	if value := strings.TrimSpace(os.Getenv("MANGO_WORKER_ID")); value != "" {
		return value
	}
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "docker-worker"
	}
	return hostname
}

func envOr(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envInt64(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
