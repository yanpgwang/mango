// Package dockertest contains opt-in Docker infrastructure for repository tests.
// It is not a sandbox backend or an application runtime dependency.
package dockertest

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// Require gates before connecting, so ordinary unit tests never auto-detect a
// daemon. Once enabled, a missing/broken daemon is a failure, never a skip.
func Require(t testing.TB) {
	t.Helper()
	if os.Getenv("MANGO_TEST_DOCKER") != "1" {
		t.Skip("set MANGO_TEST_DOCKER=1 to run Docker service tests")
	}
}

// Connect returns a required, reachable Engine client owned by the test.
func Connect(t testing.TB) *client.Client {
	t.Helper()
	Require(t)
	engine, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("required Docker client: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := engine.Ping(ctx, client.PingOptions{NegotiateAPIVersion: true}); err != nil {
		t.Fatalf("required Docker Engine: %v", err)
	}
	return engine
}

// Fixture runs simulated remote-service commands in Docker, using a private
// Linux filesystem or a caller-owned test directory. Neither credentials nor a
// socket are passed into the container. File-service doubles share that tree.
type Fixture struct {
	engine *client.Client
	id     string
	Root   string
	user   string
}

func NewFixture(t testing.TB, root string) *Fixture {
	t.Helper()
	engine := Connect(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const image = "python:3.12-alpine"
	if _, err := engine.ImageInspect(ctx, image); err != nil {
		if !errdefs.IsNotFound(err) {
			t.Fatal(err)
		}
		pull, err := engine.ImagePull(ctx, image, client.ImagePullOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = pull.Close() }()
		if err := pull.Wait(ctx); err != nil {
			t.Fatal(err)
		}
	}
	privateFilesystem := root == ""
	var mounts []mount.Mount
	if root != "" {
		mounts = []mount.Mount{{Type: mount.TypeBind, Source: root, Target: root}}
	} else {
		root = "/fixtures"
	}
	created, err := engine.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: image, Cmd: []string{"sleep", "2147483647"},
			Labels: map[string]string{"io.mango.test": "true"},
		},
		HostConfig: &container.HostConfig{
			NetworkMode: "none",
			Mounts:      mounts,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := engine.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			t.Errorf("remove Docker test fixture: %v", err)
		}
	})
	if _, err := engine.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		t.Fatal(err)
	}
	fixture := &Fixture{engine: engine, id: created.ID, Root: root}
	if !privateFilesystem {
		// Linux bind mounts retain numeric ownership. Commands must create test
		// files as the test runner, not root-owned files the runner cannot edit.
		fixture.user = fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
	}
	_, stderr, code, err := fixture.Exec(ctx, "/", []string{"mkdir", "-p", root}, nil)
	if err != nil || code != 0 {
		t.Fatalf("prepare Docker fixture root: %v, %s", err, stderr)
	}
	if privateFilesystem {
		_, stderr, code, err = fixture.Exec(ctx, "/", []string{"chown", "1000:1000", root}, nil)
		if err != nil || code != 0 {
			t.Fatalf("own Docker fixture root: %v, %s", err, stderr)
		}
		fixture.user = "1000:1000"
	}
	return fixture
}

func (f *Fixture) Exec(ctx context.Context, directory string, args []string, stdin []byte) ([]byte, []byte, int, error) {
	// Even tests passing Background must not hang forever if a command stalls.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	created, err := f.engine.ExecCreate(ctx, f.id, client.ExecCreateOptions{
		User: f.user,
		Cmd:  args, WorkingDir: directory, AttachStdin: true, AttachStdout: true, AttachStderr: true,
		Env: []string{"COPYFILE_DISABLE=1"},
	})
	if err != nil {
		return nil, nil, -1, err
	}
	attached, err := f.engine.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return nil, nil, -1, err
	}
	defer attached.Close()
	writeDone := make(chan struct{})
	go func() {
		defer close(writeDone)
		_, _ = attached.Conn.Write(stdin)
		_ = attached.CloseWrite()
	}()
	var stdout, stderr bytes.Buffer
	readDone := make(chan error, 1)
	go func() {
		_, err := stdcopy.StdCopy(&stdout, &stderr, attached.Reader)
		readDone <- err
	}()
	select {
	case err = <-readDone:
	case <-ctx.Done():
		attached.Close()
		<-readDone
		err = ctx.Err()
		killCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = f.engine.ContainerKill(killCtx, f.id, client.ContainerKillOptions{Signal: "SIGKILL"})
	}
	attached.Close()
	<-writeDone
	if err != nil {
		return stdout.Bytes(), stderr.Bytes(), -1, err
	}
	for {
		state, err := f.engine.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			return stdout.Bytes(), stderr.Bytes(), -1, err
		}
		if !state.Running {
			return stdout.Bytes(), stderr.Bytes(), state.ExitCode, nil
		}
		select {
		case <-ctx.Done():
			return nil, nil, -1, fmt.Errorf("wait for Docker fixture exec: %w", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
