package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	mango "github.com/yanpgwang/mango/sdk/go"
	"github.com/yanpgwang/mango/sdk/go/tools/agenttoolset"
)

// This example owns its Docker launch, including the business environment
// variables. The SDK owns Poll/Ack, tool dispatch, heartbeats, and Work Stop.
func launchWorker(ctx context.Context, client *mango.Client, environmentID string) error {
	poller := mango.NewWorkPoller(ctx, client, mango.WorkPollerOptions{EnvironmentID: environmentID})
	defer func() { _ = poller.Close() }()
	if !poller.Next() {
		return poller.Err()
	}
	work := poller.Current()
	input, err := json.Marshal(work)
	if err != nil {
		return err
	}
	name := "mango-mongodb-" + work.ID
	command := exec.CommandContext(ctx, "docker", "run", "--rm", "--interactive",
		"--name", name, "--network", envOr("MONGO_DOCKER_NETWORK", "mango-mongodb-example"),
		"--add-host", "host.docker.internal:host-gateway",
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--read-only", "--tmpfs", "/tmp:rw,nosuid,nodev,size=128m",
		"--tmpfs", "/workspace:rw,nosuid,nodev,size=128m,uid=65532,gid=65532",
		"--env", "MONGO_URI",
		"--env", "MONGO_DATABASE="+envOr("MONGO_DATABASE", "mango_example"),
		"--env", "MONGO_COLLECTION="+envOr("MONGO_COLLECTION", "inventory"),
		"--env", "MANGO_BASE_URL="+envOr("MANGO_DOCKER_BASE_URL", "http://host.docker.internal:8080"),
		envOr("MONGO_WORKER_IMAGE", "mango-mongodb-example:local"), "worker",
	)
	// The URI is inherited by Docker through --env NAME, not placed in argv.
	// Only the scoped Work payload goes through stdin; the Workspace key stays
	// in this application. The worker closes stdin before running tools.
	command.Stdin = bytes.NewReader(input)
	command.Stdout, command.Stderr = os.Stderr, os.Stderr
	command.Cancel = func() error {
		stopCtx, cancel := context.WithTimeout(context.Background(), 130*time.Second)
		defer cancel()
		return exec.CommandContext(stopCtx, "docker", "stop", "--time", "120", name).Run()
	}
	command.WaitDelay = 10 * time.Second
	err = command.Run()
	// A cancelled docker client can race with container creation. Remove only
	// this example's uniquely named container after the foreground client exits.
	cleanup, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = exec.CommandContext(cleanup, "docker", "rm", "--force", name).Run()
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("MongoDB worker: %w", err)
	}
	return nil
}

func runWorker(ctx context.Context) error {
	var work mango.EnvironmentWork
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 64<<10)).Decode(&work); err != nil {
		return fmt.Errorf("read Work from launcher: %w", err)
	}
	if err := os.Stdin.Close(); err != nil {
		return err
	}
	if work.Secret == nil || *work.Secret == "" {
		return errors.New("missing scoped Work secret")
	}
	client, err := mango.New(mango.Config{BaseURL: os.Getenv("MANGO_BASE_URL")})
	if err != nil {
		return err
	}
	maxIdle := 2 * time.Second
	worker := mango.NewEnvironmentWorker(client, mango.EnvironmentWorkerOptions{
		Workdir: "/workspace", MaxIdle: &maxIdle,
		ToolsFunc: func(toolContext mango.EnvironmentWorkerToolContext) ([]mango.SessionTool, error) {
			return agenttoolset.New(agenttoolset.Context{
				Workdir: toolContext.Workdir, AllowedRoots: toolContext.AllowedRoots,
				ReadOnlyRoots: toolContext.ReadOnlyRoots,
			})
		},
	})
	return worker.HandleItem(ctx, mango.EnvironmentWorkerHandleItemOptions{
		WorkID: work.ID, EnvironmentID: work.EnvironmentID,
		SessionID: work.Data.ID, WorkSecret: *work.Secret,
	})
}
