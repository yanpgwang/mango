package sandbox

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	containerderrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/authconfig"
	"github.com/moby/moby/api/types/container"
	dockerclient "github.com/moby/moby/client"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/testutil/dockertest"
)

func dockerAvailable(t *testing.T) {
	t.Helper()
	_ = dockertest.Connect(t)
}

type stubDockerEngine struct {
	dockerEngine
	listFn         func(context.Context, dockerclient.ContainerListOptions) (dockerclient.ContainerListResult, error)
	inspectFn      func(context.Context, string, dockerclient.ContainerInspectOptions) (dockerclient.ContainerInspectResult, error)
	createFn       func(context.Context, dockerclient.ContainerCreateOptions) (dockerclient.ContainerCreateResult, error)
	startFn        func(context.Context, string, dockerclient.ContainerStartOptions) (dockerclient.ContainerStartResult, error)
	removeFn       func(context.Context, string, dockerclient.ContainerRemoveOptions) (dockerclient.ContainerRemoveResult, error)
	copyFromFn     func(context.Context, string, dockerclient.CopyFromContainerOptions) (dockerclient.CopyFromContainerResult, error)
	inspectImageFn func(context.Context, string, ...dockerclient.ImageInspectOption) (dockerclient.ImageInspectResult, error)
	pullImageFn    func(context.Context, string, dockerclient.ImagePullOptions) (dockerclient.ImagePullResponse, error)
	execCreateFn   func(context.Context, string, dockerclient.ExecCreateOptions) (dockerclient.ExecCreateResult, error)
	execAttachFn   func(context.Context, string, dockerclient.ExecAttachOptions) (dockerclient.ExecAttachResult, error)
	execInspectFn  func(context.Context, string, dockerclient.ExecInspectOptions) (dockerclient.ExecInspectResult, error)
}

func (s *stubDockerEngine) ImageInspect(
	ctx context.Context,
	image string,
	options ...dockerclient.ImageInspectOption,
) (dockerclient.ImageInspectResult, error) {
	if s.inspectImageFn == nil {
		return dockerclient.ImageInspectResult{}, nil
	}
	return s.inspectImageFn(ctx, image, options...)
}

func (s *stubDockerEngine) ImagePull(
	ctx context.Context,
	image string,
	options dockerclient.ImagePullOptions,
) (dockerclient.ImagePullResponse, error) {
	return s.pullImageFn(ctx, image, options)
}

type stubImagePullResponse struct {
	dockerclient.ImagePullResponse
	waited bool
	err    error
}

func (*stubImagePullResponse) Close() error { return nil }

func (s *stubImagePullResponse) Wait(context.Context) error {
	s.waited = true
	return s.err
}

func (s *stubDockerEngine) ContainerCreate(
	ctx context.Context,
	options dockerclient.ContainerCreateOptions,
) (dockerclient.ContainerCreateResult, error) {
	return s.createFn(ctx, options)
}

func (s *stubDockerEngine) ContainerStart(
	ctx context.Context,
	cid string,
	options dockerclient.ContainerStartOptions,
) (dockerclient.ContainerStartResult, error) {
	if s.startFn == nil {
		return dockerclient.ContainerStartResult{}, nil
	}
	return s.startFn(ctx, cid, options)
}

func (s *stubDockerEngine) ContainerRemove(
	ctx context.Context,
	cid string,
	options dockerclient.ContainerRemoveOptions,
) (dockerclient.ContainerRemoveResult, error) {
	if s.removeFn == nil {
		return dockerclient.ContainerRemoveResult{}, nil
	}
	return s.removeFn(ctx, cid, options)
}

func (s *stubDockerEngine) ExecCreate(
	ctx context.Context,
	cid string,
	options dockerclient.ExecCreateOptions,
) (dockerclient.ExecCreateResult, error) {
	return s.execCreateFn(ctx, cid, options)
}

func (s *stubDockerEngine) ExecAttach(
	ctx context.Context,
	execID string,
	options dockerclient.ExecAttachOptions,
) (dockerclient.ExecAttachResult, error) {
	return s.execAttachFn(ctx, execID, options)
}

func (s *stubDockerEngine) ExecInspect(
	ctx context.Context,
	execID string,
	options dockerclient.ExecInspectOptions,
) (dockerclient.ExecInspectResult, error) {
	return s.execInspectFn(ctx, execID, options)
}

func (s *stubDockerEngine) CopyFromContainer(
	ctx context.Context,
	cid string,
	options dockerclient.CopyFromContainerOptions,
) (dockerclient.CopyFromContainerResult, error) {
	return s.copyFromFn(ctx, cid, options)
}

func (s *stubDockerEngine) ContainerList(
	ctx context.Context,
	options dockerclient.ContainerListOptions,
) (dockerclient.ContainerListResult, error) {
	if s.listFn == nil {
		return dockerclient.ContainerListResult{}, nil
	}
	return s.listFn(ctx, options)
}

func (s *stubDockerEngine) ContainerInspect(
	ctx context.Context,
	cid string,
	options dockerclient.ContainerInspectOptions,
) (dockerclient.ContainerInspectResult, error) {
	if s.inspectFn == nil {
		return dockerclient.ContainerInspectResult{}, nil
	}
	return s.inspectFn(ctx, cid, options)
}

func newDockerSB(t *testing.T, spec Spec) Sandbox {
	t.Helper()
	dockerAvailable(t)
	p, err := NewDockerProvider(DockerConfig{ResourceBaseDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if spec.Timeout == 0 {
		spec.Timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, sb, err := p.Create(ctx, fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := sb.Destroy(ctx); err != nil {
			t.Errorf("Docker cleanup: %v", err)
		}
	})
	return sb
}

func TestDocker_CreateBuildsTypedEngineRequest(t *testing.T) {
	var captured dockerclient.ContainerCreateOptions
	engine := &stubDockerEngine{
		inspectFn: func(
			context.Context,
			string,
			dockerclient.ContainerInspectOptions,
		) (dockerclient.ContainerInspectResult, error) {
			return dockerclient.ContainerInspectResult{},
				containerderrdefs.ErrNotFound.WithMessage("missing")
		},
		createFn: func(
			_ context.Context,
			options dockerclient.ContainerCreateOptions,
		) (dockerclient.ContainerCreateResult, error) {
			captured = options
			return dockerclient.ContainerCreateResult{ID: "container-id"}, nil
		},
	}
	provider, err := newDockerProviderWithEngine(DockerConfig{
		DefaultImage: "alpine:test", ResourceBaseDir: t.TempDir(),
	}, engine)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = provider.Create(context.Background(), t.Name(), Spec{
		Memory: "512m", CPUs: "1.5", Network: "bridge", PidsLimit: 64,
		MemoryStores: []MemoryStoreMount{{
			Identity: "sesrsc_memory", StoreID: "memstore_test",
			RuntimePath: "/mnt/memory/project", Access: domain.MemoryAccessReadOnly,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if captured.Config == nil || captured.Config.Image != "alpine:test" ||
		captured.Config.WorkingDir != dockerRoot ||
		captured.Config.Labels[dockerManagedLabel] != "true" ||
		captured.Config.Labels[dockerSessionKeyLabel] != t.Name() {
		t.Fatalf("container config = %+v", captured.Config)
	}
	if captured.HostConfig == nil || captured.HostConfig.NetworkMode != "bridge" ||
		captured.HostConfig.Memory != 512*1024*1024 ||
		captured.HostConfig.NanoCPUs != 1_500_000_000 ||
		captured.HostConfig.PidsLimit == nil ||
		*captured.HostConfig.PidsLimit != 64 {
		t.Fatalf("host config = %+v", captured.HostConfig)
	}
	mounts := make(map[string]bool, len(captured.HostConfig.Mounts))
	for _, mounted := range captured.HostConfig.Mounts {
		mounts[mounted.Target] = mounted.ReadOnly
	}
	if !mounts[SessionUploadsRoot] || !mounts[domain.SessionSkillsRoot] ||
		!mounts["/mnt/memory/project"] {
		t.Fatalf("mounts = %+v", captured.HostConfig.Mounts)
	}
	if readOnly, present := mounts[SessionOutputsRoot]; !present || readOnly {
		t.Fatalf("Session output mount = present:%t readOnly:%t", present, readOnly)
	}
}

func TestDocker_EnsureImagePullsAndWaitsWhenMissing(t *testing.T) {
	response := &stubImagePullResponse{}
	pulledImage := ""
	engine := &stubDockerEngine{
		inspectImageFn: func(
			context.Context,
			string,
			...dockerclient.ImageInspectOption,
		) (dockerclient.ImageInspectResult, error) {
			return dockerclient.ImageInspectResult{},
				containerderrdefs.ErrNotFound.WithMessage("missing")
		},
		pullImageFn: func(
			_ context.Context,
			image string,
			_ dockerclient.ImagePullOptions,
		) (dockerclient.ImagePullResponse, error) {
			pulledImage = image
			return response, nil
		},
	}
	provider := &dockerProvider{engine: engine}
	if err := provider.ensureImage(context.Background(), "alpine:latest"); err != nil {
		t.Fatal(err)
	}
	if pulledImage != "alpine:latest" || !response.waited {
		t.Fatalf("pull image = %q, waited = %t", pulledImage, response.waited)
	}
}

func TestDocker_EnsureImageUsesStandardRegistryAuth(t *testing.T) {
	configDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(configDir, "config.json"),
		[]byte(`{"auths":{"registry.example.com":{"auth":"dXNlcjpwYXNz"}}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	response := &stubImagePullResponse{}
	var encodedAuth string
	engine := &stubDockerEngine{
		inspectImageFn: func(
			context.Context,
			string,
			...dockerclient.ImageInspectOption,
		) (dockerclient.ImageInspectResult, error) {
			return dockerclient.ImageInspectResult{},
				containerderrdefs.ErrNotFound.WithMessage("missing")
		},
		pullImageFn: func(
			_ context.Context,
			_ string,
			options dockerclient.ImagePullOptions,
		) (dockerclient.ImagePullResponse, error) {
			encodedAuth = options.RegistryAuth
			return response, nil
		},
	}
	provider := &dockerProvider{engine: engine, registryAuthDir: configDir}
	if err := provider.ensureImage(
		context.Background(), "registry.example.com/team/private:latest",
	); err != nil {
		t.Fatal(err)
	}
	resolved, err := authconfig.Decode(encodedAuth)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Username != "user" || resolved.Password != "pass" ||
		resolved.ServerAddress != "registry.example.com" {
		t.Fatalf("registry auth = %+v", resolved)
	}
}

func TestDocker_ExecWaitsForFinalExitCode(t *testing.T) {
	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	go func() {
		defer func() { _ = serverConn.Close() }()
		header := make([]byte, 8)
		header[0] = 1 // stdout
		binary.BigEndian.PutUint32(header[4:], uint32(len("done")))
		_, _ = serverConn.Write(append(header, []byte("done")...))
	}()
	inspectCalls := 0
	engine := &stubDockerEngine{
		execCreateFn: func(
			context.Context,
			string,
			dockerclient.ExecCreateOptions,
		) (dockerclient.ExecCreateResult, error) {
			return dockerclient.ExecCreateResult{ID: "exec-id"}, nil
		},
		execAttachFn: func(
			context.Context,
			string,
			dockerclient.ExecAttachOptions,
		) (dockerclient.ExecAttachResult, error) {
			return dockerclient.ExecAttachResult{HijackedResponse: dockerclient.NewHijackedResponse(
				clientConn, "application/vnd.docker.multiplexed-stream",
			)}, nil
		},
		execInspectFn: func(
			context.Context,
			string,
			dockerclient.ExecInspectOptions,
		) (dockerclient.ExecInspectResult, error) {
			inspectCalls++
			if inspectCalls == 1 {
				return dockerclient.ExecInspectResult{Running: true}, nil
			}
			return dockerclient.ExecInspectResult{ExitCode: 7}, nil
		},
	}
	result, err := (&dockerProvider{engine: engine}).execContainer(
		context.Background(), "container-id", Command{Path: "false"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 7 || string(result.Stdout) != "done" || inspectCalls != 2 {
		t.Fatalf("result=%+v inspect calls=%d", result, inspectCalls)
	}
}

func TestDocker_CreateCleansRejectedGeneration(t *testing.T) {
	base := t.TempDir()
	engine := &stubDockerEngine{
		inspectFn: func(
			context.Context,
			string,
			dockerclient.ContainerInspectOptions,
		) (dockerclient.ContainerInspectResult, error) {
			return dockerclient.ContainerInspectResult{},
				containerderrdefs.ErrNotFound.WithMessage("missing")
		},
		createFn: func(
			context.Context,
			dockerclient.ContainerCreateOptions,
		) (dockerclient.ContainerCreateResult, error) {
			return dockerclient.ContainerCreateResult{},
				containerderrdefs.ErrInvalidArgument.WithMessage("bad create request")
		},
	}
	providerInterface, err := newDockerProviderWithEngine(DockerConfig{
		ResourceBaseDir: base,
	}, engine)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = providerInterface.Create(context.Background(), t.Name(), Spec{})
	if err == nil {
		t.Fatal("create unexpectedly succeeded")
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("rejected create leaked resource roots: %v", entries)
	}
}

func TestDocker_StartFailureRemovesContainerAndGeneration(t *testing.T) {
	base := t.TempDir()
	removed := false
	engine := &stubDockerEngine{
		inspectFn: func(
			context.Context,
			string,
			dockerclient.ContainerInspectOptions,
		) (dockerclient.ContainerInspectResult, error) {
			return dockerclient.ContainerInspectResult{},
				containerderrdefs.ErrNotFound.WithMessage("missing")
		},
		createFn: func(
			context.Context,
			dockerclient.ContainerCreateOptions,
		) (dockerclient.ContainerCreateResult, error) {
			return dockerclient.ContainerCreateResult{ID: "container-id"}, nil
		},
		startFn: func(
			context.Context,
			string,
			dockerclient.ContainerStartOptions,
		) (dockerclient.ContainerStartResult, error) {
			return dockerclient.ContainerStartResult{}, errors.New("start failed")
		},
		removeFn: func(
			_ context.Context,
			cid string,
			options dockerclient.ContainerRemoveOptions,
		) (dockerclient.ContainerRemoveResult, error) {
			removed = cid == "container-id" && options.Force
			return dockerclient.ContainerRemoveResult{}, nil
		},
	}
	providerInterface, err := newDockerProviderWithEngine(DockerConfig{
		ResourceBaseDir: base,
	}, engine)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = providerInterface.Create(context.Background(), t.Name(), Spec{})
	if err == nil || !removed {
		t.Fatalf("start failure err=%v removed=%t", err, removed)
	}
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("start failure leaked resource roots: %v", entries)
	}
}

func TestDocker_ReadFileRejectsContainerSymlinkArchive(t *testing.T) {
	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		Name: "link", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd",
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	engine := &stubDockerEngine{
		copyFromFn: func(
			context.Context,
			string,
			dockerclient.CopyFromContainerOptions,
		) (dockerclient.CopyFromContainerResult, error) {
			return dockerclient.CopyFromContainerResult{
				Content: io.NopCloser(bytes.NewReader(archive.Bytes())),
			}, nil
		},
	}
	box := &dockerSandbox{
		provider: &dockerProvider{engine: engine}, cid: "container-id",
		fileRoots: dockerToolFileRoots(Spec{}),
	}
	_, err := box.ReadFile(context.Background(), "/workspace/link")
	if err == nil || !IsPermanent(err) || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("ReadFile symlink error = %v", err)
	}
}

func TestDocker_OpenSessionOutputsStreamsProviderArchive(t *testing.T) {
	want := []byte("tar stream")
	var sourcePath string
	engine := &stubDockerEngine{
		copyFromFn: func(
			_ context.Context,
			cid string,
			options dockerclient.CopyFromContainerOptions,
		) (dockerclient.CopyFromContainerResult, error) {
			if cid != "container-id" {
				t.Fatalf("container id = %q", cid)
			}
			sourcePath = options.SourcePath
			return dockerclient.CopyFromContainerResult{
				Content: io.NopCloser(bytes.NewReader(want)),
			}, nil
		},
	}
	box := &dockerSandbox{
		provider: &dockerProvider{engine: engine}, cid: "container-id",
		outputMountReady: true,
	}
	stream, err := box.OpenSessionOutputs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Close() }()
	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	if sourcePath != SessionOutputsRoot+"/." || !bytes.Equal(got, want) {
		t.Fatalf("source=%q stream=%q", sourcePath, got)
	}
}

func TestDocker_OpenSessionOutputsRejectsLegacySandboxWithoutMount(t *testing.T) {
	box := &dockerSandbox{outputMountReady: false}
	_, err := box.OpenSessionOutputs(context.Background())
	if err == nil || !IsPermanent(err) || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("legacy Session output error = %v", err)
	}
}

func TestDocker_OpenSessionOutputsDoesNotTreatMissingMountAsEmpty(t *testing.T) {
	engine := &stubDockerEngine{
		copyFromFn: func(
			context.Context,
			string,
			dockerclient.CopyFromContainerOptions,
		) (dockerclient.CopyFromContainerResult, error) {
			return dockerclient.CopyFromContainerResult{},
				containerderrdefs.ErrNotFound.WithMessage("missing")
		},
	}
	box := &dockerSandbox{
		provider: &dockerProvider{engine: engine}, cid: "container-id",
		outputMountReady: true,
	}
	_, err := box.OpenSessionOutputs(context.Background())
	if err == nil || IsPermanent(err) || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing Session output mount error = %v", err)
	}
}

func TestDocker_ExecAndExitCode(t *testing.T) {
	sb := newDockerSB(t, Spec{})
	res, err := sb.Exec(context.Background(), Command{Path: "sh", Args: []string{"-c", "echo hi"}})
	if err != nil || strings.TrimSpace(string(res.Stdout)) != "hi" || res.ExitCode != 0 {
		t.Fatalf("exec echo: res=%+v err=%v", res, err)
	}
	res, err = sb.Exec(context.Background(), Command{Path: "sh", Args: []string{"-c", "exit 3"}})
	if err != nil || res.ExitCode != 3 {
		t.Fatalf("exit code: res=%+v err=%v", res, err)
	}
}

func TestDocker_Timeout(t *testing.T) {
	sb := newDockerSB(t, Spec{Timeout: 500 * time.Millisecond})
	res, err := sb.Exec(context.Background(), Command{Path: "sh", Args: []string{"-c", "sleep 10"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("expected TimedOut, got %+v", res)
	}

	// The kill on timeout stops the container: it must no longer be running.
	ds, ok := sb.(*dockerSandbox)
	if !ok {
		t.Fatalf("expected *dockerSandbox, got %T", sb)
	}
	inspected, err := ds.provider.engine.ContainerInspect(
		context.Background(), ds.cid, dockerclient.ContainerInspectOptions{},
	)
	if err != nil || inspected.Container.State == nil || inspected.Container.State.Running {
		t.Fatalf("container still running after timeout: inspect=%+v err=%v", inspected.Container.State, err)
	}
}

func TestDocker_FileRoundTripAndConfinement(t *testing.T) {
	sb := newDockerSB(t, Spec{})
	if err := sb.WriteFile(context.Background(), "sub/a.txt", []byte("data")); err != nil {
		t.Fatal(err)
	}
	b, err := sb.ReadFile(context.Background(), "sub/a.txt")
	if err != nil || string(b) != "data" {
		t.Fatalf("read = %q err=%v", b, err)
	}
	mode, err := sb.Exec(context.Background(), Command{
		Path: "stat", Args: []string{"-c", "%a", "sub/a.txt"},
	})
	if err != nil || mode.ExitCode != 0 || strings.TrimSpace(string(mode.Stdout)) != "644" {
		t.Fatalf("written file mode: result=%+v err=%v", mode, err)
	}
	// Path-escape confinement: assert on the specific "escapes root" signal,
	// not merely err != nil. A bare non-nil check cannot distinguish a
	// confinement rejection from an ordinary not-found error, so it would keep
	// passing even if containedPath were removed. Read and Write are checked
	// independently because they guard the boundary on separate code paths.
	if _, err := sb.ReadFile(context.Background(), "../escape"); err == nil ||
		!strings.Contains(err.Error(), "escapes root") {
		t.Fatalf("ReadFile path escape must be rejected by confinement, got err=%v", err)
	}
	if err := sb.WriteFile(context.Background(), "../escape", []byte("x")); err == nil ||
		!strings.Contains(err.Error(), "escapes root") {
		t.Fatalf("WriteFile path escape must be rejected by confinement, got err=%v", err)
	}
	// "sub/../../escape" cleans to a path above root and must also be rejected.
	if _, err := sb.ReadFile(context.Background(), "sub/../../escape"); err == nil ||
		!strings.Contains(err.Error(), "escapes root") {
		t.Fatalf("ReadFile nested ../ escape must be rejected by confinement, got err=%v", err)
	}
	// file written via WriteFile is visible to Exec
	res, _ := sb.Exec(context.Background(), Command{Path: "sh", Args: []string{"-c", "cat sub/a.txt"}})
	if strings.TrimSpace(string(res.Stdout)) != "data" {
		t.Fatalf("exec cat = %q", res.Stdout)
	}
}

func TestDocker_SessionOutputMountIsWritableAndExported(t *testing.T) {
	ctx := context.Background()
	box := newDockerSB(t, Spec{})
	want := []byte("docker output\n")
	if err := box.WriteFile(
		ctx, SessionOutputsRoot+"/nested/report.txt", want,
	); err != nil {
		t.Fatalf("write Session output: %v", err)
	}
	exporter, ok := box.(SessionOutputSandbox)
	if !ok {
		t.Fatalf("Docker sandbox does not expose SessionOutputSandbox: %T", box)
	}
	stream, err := exporter.OpenSessionOutputs(ctx)
	if err != nil {
		t.Fatalf("OpenSessionOutputs: %v", err)
	}
	defer func() { _ = stream.Close() }()
	reader := tar.NewReader(stream)
	for {
		header, nextErr := reader.Next()
		if errors.Is(nextErr, io.EOF) {
			break
		}
		if nextErr != nil {
			t.Fatalf("read output archive: %v", nextErr)
		}
		if strings.TrimPrefix(header.Name, "./") != "nested/report.txt" {
			continue
		}
		got, readErr := io.ReadAll(reader)
		if readErr != nil {
			t.Fatalf("read exported output: %v", readErr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("exported output = %q, want %q", got, want)
		}
		return
	}
	t.Fatal("nested/report.txt missing from Docker output archive")
}

func TestDocker_ToolFilePathUsesExplicitResourceRoots(t *testing.T) {
	box := &dockerSandbox{fileRoots: dockerToolFileRoots(Spec{
		MemoryStores: []MemoryStoreMount{
			{
				RuntimePath: "/mnt/memory/project",
				Access:      domain.MemoryAccessReadWrite,
			},
			{
				RuntimePath: "/mnt/memory/reference",
				Access:      domain.MemoryAccessReadOnly,
			},
		},
	})}
	tests := []struct {
		name    string
		path    string
		write   bool
		want    string
		wantErr string
	}{
		{name: "relative workspace", path: "src/main.go", want: "/workspace/src/main.go"},
		{name: "absolute workspace", path: "/workspace/src/main.go", want: "/workspace/src/main.go"},
		{name: "uploads read", path: "/mnt/session/uploads/input.csv", want: "/mnt/session/uploads/input.csv"},
		{name: "uploads write", path: "/mnt/session/uploads/input.csv", write: true, wantErr: "read-only"},
		{name: "skills read", path: "/workspace/skills/pdf/SKILL.md", want: "/workspace/skills/pdf/SKILL.md"},
		{name: "skills write", path: "/workspace/skills/pdf/SKILL.md", write: true, wantErr: "read-only"},
		{name: "memory read write", path: "/mnt/memory/project/note.md", write: true, want: "/mnt/memory/project/note.md"},
		{name: "memory read only read", path: "/mnt/memory/reference/note.md", want: "/mnt/memory/reference/note.md"},
		{name: "memory read only write", path: "/mnt/memory/reference/note.md", write: true, wantErr: "read-only"},
		{name: "relative escape", path: "../secret", wantErr: "escapes root"},
		{name: "absolute escape", path: "/etc/passwd", wantErr: "escapes root"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := box.toolFilePath(test.path, test.write)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("toolFilePath(%q, %t) error = %v, want %q", test.path, test.write, err, test.wantErr)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("toolFilePath(%q, %t) = %q, %v; want %q", test.path, test.write, got, err, test.want)
			}
		})
	}
}

func TestDocker_MemoryStoreMountRoundTripAndReadOnlyBoundary(t *testing.T) {
	readWrite := MemoryStoreMount{
		Identity: "sesrsc_memory_rw", StoreID: "memstore_rw",
		RuntimePath: "/mnt/memory/project", Access: domain.MemoryAccessReadWrite,
	}
	readOnly := MemoryStoreMount{
		Identity: "sesrsc_memory_ro", StoreID: "memstore_ro",
		RuntimePath: "/mnt/memory/reference", Access: domain.MemoryAccessReadOnly,
	}
	box := newDockerSB(t, Spec{MemoryStores: []MemoryStoreMount{readWrite, readOnly}})
	memoryBox, ok := box.(MemoryStoreSandbox)
	if !ok {
		t.Fatalf("Docker sandbox does not expose MemoryStoreSandbox: %T", box)
	}
	file := func(id, path, content string) MemoryStoreFile {
		sum := sha256.Sum256([]byte(content))
		return MemoryStoreFile{
			MemoryID: id, Path: path, Content: []byte(content),
			ContentSHA256: hex.EncodeToString(sum[:]),
		}
	}
	if err := memoryBox.ReplaceMemoryStore(
		context.Background(), readWrite,
		[]MemoryStoreFile{file("mem_a", "/notes/a.md", "initial")},
	); err != nil {
		t.Fatal(err)
	}
	if err := memoryBox.ReplaceMemoryStore(
		context.Background(), readOnly,
		[]MemoryStoreFile{file("mem_ref", "/policy.md", "fixed")},
	); err != nil {
		t.Fatal(err)
	}
	if content, err := box.ReadFile(
		context.Background(), "/mnt/memory/project/notes/a.md",
	); err != nil || string(content) != "initial" {
		t.Fatalf("ReadFile on read-write Memory = %q, %v", content, err)
	}
	if err := box.WriteFile(
		context.Background(), "/mnt/memory/project/notes/a.md", []byte("file-tool"),
	); err != nil {
		t.Fatalf("WriteFile on read-write Memory: %v", err)
	}
	if content, err := box.ReadFile(
		context.Background(), "/mnt/memory/reference/policy.md",
	); err != nil || string(content) != "fixed" {
		t.Fatalf("ReadFile on read-only Memory = %q, %v", content, err)
	}
	if err := box.WriteFile(
		context.Background(), "/mnt/memory/reference/policy.md", []byte("changed"),
	); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("WriteFile on read-only Memory error = %v", err)
	}
	result, err := box.Exec(context.Background(), Command{
		Path: "sh", Args: []string{"-c", `printf updated > /mnt/memory/project/notes/a.md && printf new > /mnt/memory/project/new.md`},
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("write Memory Store: result=%+v err=%v", result, err)
	}
	snapshot, err := memoryBox.ReadMemoryStore(context.Background(), readWrite)
	if err != nil || !snapshot.Initialized || len(snapshot.Baseline) != 1 ||
		len(snapshot.Current) != 2 {
		t.Fatalf("Memory snapshot = %+v, %v", snapshot, err)
	}
	contents := map[string]string{}
	for _, current := range snapshot.Current {
		contents[current.Path] = string(current.Content)
	}
	if contents["/notes/a.md"] != "updated" || contents["/new.md"] != "new" {
		t.Fatalf("Memory contents = %#v", contents)
	}
	result, err = box.Exec(context.Background(), Command{
		Path: "sh", Args: []string{"-c", `printf changed > /mnt/memory/reference/policy.md`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode == 0 {
		t.Fatalf("read-only Memory Store accepted a write: %+v", result)
	}
	readOnlySnapshot, err := memoryBox.ReadMemoryStore(context.Background(), readOnly)
	if err != nil || len(readOnlySnapshot.Current) != 1 ||
		string(readOnlySnapshot.Current[0].Content) != "fixed" {
		t.Fatalf("read-only Memory snapshot = %+v, %v", readOnlySnapshot, err)
	}
}

func TestDocker_NetworkNoneByDefault(t *testing.T) {
	sb := newDockerSB(t, Spec{})
	// With no network, DNS lookup and connection attempts must fail.
	res, _ := sb.Exec(context.Background(), Command{Path: "sh", Args: []string{"-c",
		"wget -T2 -q -O- http://example.com >/dev/null 2>&1 && echo REACHED || echo BLOCKED"}})
	if strings.TrimSpace(string(res.Stdout)) != "BLOCKED" {
		t.Fatalf("network should be blocked by default, got %q", res.Stdout)
	}
}

func TestDocker_ExecAfterTimeoutReturnsError(t *testing.T) {
	sb := newDockerSB(t, Spec{Timeout: 500 * time.Millisecond})

	// First exec times out and kills the container.
	res, err := sb.Exec(context.Background(), Command{Path: "sh", Args: []string{"-c", "sleep 10"}})
	if err != nil {
		t.Fatalf("first exec: unexpected err=%v", err)
	}
	if !res.TimedOut {
		t.Fatalf("first exec: expected TimedOut, got %+v", res)
	}

	// The container is now dead. A subsequent exec must return an explicit
	// error rather than a raw provider failure against a stopped container.
	res2, err := sb.Exec(context.Background(), Command{Path: "sh", Args: []string{"-c", "echo hi"}})
	if err == nil {
		t.Fatalf("exec after timeout: expected error, got res=%+v err=nil", res2)
	}
}

func TestDocker_CreateIsIdempotentAndAttachPreservesWorkspace(t *testing.T) {
	dockerAvailable(t)
	ctx := context.Background()
	firstProvider, err := NewDockerProvider(DockerConfig{DefaultImage: "alpine:latest"})
	if err != nil {
		t.Fatal(err)
	}
	ref, first, err := firstProvider.Create(ctx, t.Name(), Spec{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Destroy(context.Background()) })
	if err := first.WriteFile(ctx, "state.txt", []byte("durable")); err != nil {
		t.Fatal(err)
	}
	firstResources, ok := first.(FileResourceSandbox)
	if !ok {
		t.Fatalf("first Docker sandbox does not expose FileResourceSandbox: %T", first)
	}
	resourceContent := []byte("restored mount")
	resourceMount := testFileResourceMount(
		SessionUploadsRoot+"/restart.txt", resourceContent,
	)
	resourceMount.Identity = "sesrsc_restart"
	if err := firstResources.ImportFileResource(
		ctx, resourceMount, bytes.NewReader(resourceContent),
	); err != nil {
		t.Fatalf("import before worker restart: %v", err)
	}

	secondProvider, err := NewDockerProvider(DockerConfig{DefaultImage: "alpine:latest"})
	if err != nil {
		t.Fatal(err)
	}
	sameRef, same, err := secondProvider.Create(
		ctx,
		t.Name(),
		Spec{Timeout: 30 * time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	if sameRef != ref {
		t.Fatalf("idempotent Create ref = %+v, want %+v", sameRef, ref)
	}
	attached, err := secondProvider.Attach(
		ctx,
		t.Name(),
		ref,
		Spec{Timeout: 30 * time.Second},
	)
	if err != nil {
		t.Fatal(err)
	}
	for name, box := range map[string]Sandbox{"same": same, "attached": attached} {
		data, err := box.ReadFile(ctx, "state.txt")
		if err != nil || string(data) != "durable" {
			t.Fatalf("%s workspace data = %q, err=%v", name, data, err)
		}
	}
	resources, ok := attached.(FileResourceSandbox)
	if !ok {
		t.Fatalf("attached Docker sandbox does not expose FileResourceSandbox: %T", attached)
	}
	present, err := resources.HasFileResource(ctx, resourceMount)
	if err != nil || !present {
		t.Fatalf("attached sandbox did not recognize staged resource: present=%t err=%v", present, err)
	}
	resourceByFileTool, err := attached.ReadFile(ctx, resourceMount.RuntimePath)
	if err != nil || !bytes.Equal(resourceByFileTool, resourceContent) {
		t.Fatalf("attached ReadFile resource = %q, err=%v", resourceByFileTool, err)
	}
	result, err := attached.Exec(ctx, Command{
		Path: "sh", Args: []string{"-c", "cat /mnt/session/uploads/restart.txt"},
	})
	if err != nil || result.ExitCode != 0 || !bytes.Equal(result.Stdout, resourceContent) {
		t.Fatalf("attached resource mount: result=%+v err=%v", result, err)
	}
	if _, err := secondProvider.Attach(
		ctx,
		"another-session",
		ref,
		Spec{Timeout: 30 * time.Second},
	); err == nil || !IsPermanent(err) {
		t.Fatalf("cross-session Attach = %v, want permanent ownership error", err)
	}
}

func TestDocker_AttachLegacyContainerAllowsResourceDetach(t *testing.T) {
	dockerAvailable(t)
	ctx := context.Background()
	providerInterface, err := NewDockerProvider(DockerConfig{
		DefaultImage:    "alpine:latest",
		ResourceBaseDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := providerInterface.(*dockerProvider)
	sessionKey := t.Name()
	name := dockerContainerName(sessionKey)
	if err := provider.ensureImage(ctx, "alpine:latest"); err != nil {
		t.Fatal(err)
	}
	created, err := provider.engine.ContainerCreate(ctx, dockerclient.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image: "alpine:latest", WorkingDir: dockerRoot,
			Cmd: []string{"sh", "-c", keepAlive},
			Labels: map[string]string{
				dockerManagedLabel: "true", dockerSessionKeyLabel: sessionKey,
			},
		},
		HostConfig: &container.HostConfig{NetworkMode: "none"},
	})
	if err != nil {
		t.Fatalf("create legacy container: %v", err)
	}
	cid := strings.TrimSpace(created.ID)
	if cid == "" {
		t.Fatal("create legacy container returned no id")
	}
	t.Cleanup(func() { provider.forceRemove(cid) })
	if _, err := provider.engine.ContainerStart(
		ctx, cid, dockerclient.ContainerStartOptions{},
	); err != nil {
		t.Fatalf("start legacy container: %v", err)
	}

	box, err := provider.Attach(
		ctx,
		sessionKey,
		Ref{Provider: DockerProviderName, ID: cid},
		Spec{Timeout: 30 * time.Second},
	)
	if err != nil {
		t.Fatalf("attach legacy container: %v", err)
	}
	result, err := box.Exec(ctx, Command{Path: "sh", Args: []string{"-c", "printf legacy"}})
	if err != nil || result.ExitCode != 0 || string(result.Stdout) != "legacy" {
		t.Fatalf("legacy container exec: result=%+v err=%v", result, err)
	}
	resources := box.(FileResourceSandbox)
	content := []byte("resource")
	mount := testFileResourceMount(SessionUploadsRoot+"/legacy.txt", content)
	present, err := resources.HasFileResource(ctx, mount)
	if err != nil || present {
		t.Fatalf("legacy HasFileResource = %t, err=%v", present, err)
	}
	if err := resources.ImportFileResource(ctx, mount, bytes.NewReader(content)); err == nil || !IsPermanent(err) {
		t.Fatalf("legacy import error = %v, want permanent unsupported mount", err)
	}
	if err := resources.RemoveFileResource(ctx, mount.RuntimePath, mount.Identity); err != nil {
		t.Fatalf("legacy detach must be a no-op: %v", err)
	}
	archive, expanded := testSkillArchive(t, "legacy", map[string]skillTestFile{
		"SKILL.md": {
			body: []byte("---\nname: legacy\ndescription: Legacy test\n---\n"),
			mode: 0o644,
		},
	})
	skillMount := testReadOnlySkillMount(
		"skill_legacy@100", "legacy", "legacy", archive, expanded,
	)
	skills := box.(SkillBundleSandbox)
	present, err = skills.HasReadOnlySkill(ctx, skillMount)
	if err != nil || present {
		t.Fatalf("legacy HasReadOnlySkill = %t, err=%v", present, err)
	}
	if err := skills.ImportReadOnlySkill(
		ctx, skillMount, bytes.NewReader(archive),
	); err == nil || !IsPermanent(err) || !strings.Contains(err.Error(), "predates") {
		t.Fatalf("legacy Skill import error = %v, want permanent recreation error", err)
	}
}

func TestDocker_FileResourceMountIsAtomicAndReadOnly(t *testing.T) {
	sb := newDockerSB(t, Spec{})
	resources, ok := sb.(FileResourceSandbox)
	if !ok {
		t.Fatalf("Docker sandbox does not expose FileResourceSandbox: %T", sb)
	}
	content := []byte("durable resource\n")
	mount := testFileResourceMount("/mnt/session/uploads/nested/data.txt", content)

	present, err := resources.HasFileResource(context.Background(), mount)
	if err != nil || present {
		t.Fatalf("initial HasFileResource = %t, err=%v", present, err)
	}
	if err := resources.ImportFileResource(
		context.Background(), mount, bytes.NewReader(content),
	); err != nil {
		t.Fatal(err)
	}
	present, err = resources.HasFileResource(context.Background(), mount)
	if err != nil || !present {
		t.Fatalf("HasFileResource after import = %t, err=%v", present, err)
	}
	readByTool, err := sb.ReadFile(
		context.Background(), "/mnt/session/uploads/nested/data.txt",
	)
	if err != nil || !bytes.Equal(readByTool, content) {
		t.Fatalf("ReadFile mounted resource = %q, %v", readByTool, err)
	}
	if err := sb.WriteFile(
		context.Background(), "/mnt/session/uploads/nested/data.txt", []byte("changed"),
	); err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Fatalf("WriteFile mounted resource error = %v", err)
	}

	result, err := sb.Exec(context.Background(), Command{
		Path: "sh", Args: []string{"-c", "cat /mnt/session/uploads/nested/data.txt"},
	})
	if err != nil || result.ExitCode != 0 || !bytes.Equal(result.Stdout, content) {
		t.Fatalf("read mounted resource: result=%+v err=%v", result, err)
	}
	result, err = sb.Exec(context.Background(), Command{
		Path: "sh", Args: []string{"-c", "printf changed > /mnt/session/uploads/nested/data.txt"},
	})
	if err != nil || result.ExitCode == 0 {
		t.Fatalf("write to read-only mount: result=%+v err=%v", result, err)
	}

	replacement := testFileResourceMount(mount.RuntimePath, []byte("replacement"))
	if err := resources.ImportFileResource(
		context.Background(), replacement, &readerThatFails{data: []byte("partial")},
	); err == nil {
		t.Fatal("partial replacement unexpectedly succeeded")
	}
	result, err = sb.Exec(context.Background(), Command{
		Path: "sh", Args: []string{"-c", "cat /mnt/session/uploads/nested/data.txt"},
	})
	if err != nil || result.ExitCode != 0 || !bytes.Equal(result.Stdout, content) {
		t.Fatalf("failed replacement changed visible file: result=%+v err=%v", result, err)
	}

	invalid := testFileResourceMount("/mnt/session/uploads/../escape", content)
	if err := resources.ImportFileResource(
		context.Background(), invalid, bytes.NewReader(content),
	); err == nil {
		t.Fatal("path traversal unexpectedly succeeded")
	}
	control := testFileResourceMount("/mnt/session/uploads/line\nbreak", content)
	if err := resources.ImportFileResource(
		context.Background(), control, bytes.NewReader(content),
	); err == nil {
		t.Fatal("control character in path unexpectedly succeeded")
	}
	if err := resources.RemoveFileResource(context.Background(), mount.RuntimePath, mount.Identity); err != nil {
		t.Fatal(err)
	}
	if err := resources.RemoveFileResource(context.Background(), mount.RuntimePath, mount.Identity); err != nil {
		t.Fatalf("idempotent remove: %v", err)
	}
	present, err = resources.HasFileResource(context.Background(), mount)
	if err != nil || present {
		t.Fatalf("HasFileResource after remove = %t, err=%v", present, err)
	}
	parentContent := []byte("parent replacement")
	parent := testFileResourceMount("/mnt/session/uploads/nested", parentContent)
	parent.Identity = "sesrsc_parent"
	if err := resources.ImportFileResource(
		context.Background(), parent, bytes.NewReader(parentContent),
	); err != nil {
		t.Fatalf("import parent after nested deletion: %v", err)
	}
	if err := resources.RemoveFileResource(
		context.Background(), parent.RuntimePath, mount.Identity,
	); err != nil {
		t.Fatalf("stale identity removal: %v", err)
	}
	result, err = sb.Exec(context.Background(), Command{
		Path: "sh", Args: []string{"-c", "cat /mnt/session/uploads/nested"},
	})
	if err != nil || result.ExitCode != 0 || !bytes.Equal(result.Stdout, parentContent) {
		t.Fatalf("stale removal changed replacement: result=%+v err=%v", result, err)
	}
	if err := resources.RemoveFileResource(
		context.Background(), parent.RuntimePath, parent.Identity,
	); err != nil {
		t.Fatalf("remove parent replacement: %v", err)
	}
}

func TestDocker_SkillBundleSurvivesAttachRepairsCorruptionAndIsReadOnly(t *testing.T) {
	dockerAvailable(t)
	ctx := context.Background()
	resourceBase := t.TempDir()
	firstProviderInterface, err := NewDockerProvider(DockerConfig{
		DefaultImage: "alpine:latest", ResourceBaseDir: resourceBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstProvider := firstProviderInterface.(*dockerProvider)
	ref, first, err := firstProvider.Create(ctx, t.Name(), Spec{Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Destroy(context.Background()) })
	archive, expanded := testSkillArchive(t, "Report_Tool", map[string]skillTestFile{
		"SKILL.md": {
			body: []byte("---\nname: report-tool\ndescription: Analyze reports\n---\nUse the helper.\n"),
			mode: 0o644,
		},
		"scripts/run.sh": {body: []byte("#!/bin/sh\nprintf skill-ok"), mode: 0o755},
	})
	mount := testReadOnlySkillMount(
		"skill_reports@100", "report-tool", "Report_Tool", archive, expanded,
	)
	skills := first.(SkillBundleSandbox)
	if err := skills.ImportReadOnlySkill(ctx, mount, bytes.NewReader(archive)); err != nil {
		t.Fatalf("initial Skill import: %v", err)
	}
	present, err := skills.HasReadOnlySkill(ctx, mount)
	if err != nil || !present {
		t.Fatalf("initial Skill presence = %t, err=%v", present, err)
	}
	readBody, err := first.ReadFile(ctx, "skills/report-tool/SKILL.md")
	if err != nil || !bytes.Contains(readBody, []byte("Analyze reports")) {
		t.Fatalf("read mounted Skill: body=%q err=%v", readBody, err)
	}
	result, err := first.Exec(ctx, Command{
		Path: "sh", Args: []string{"-c", "test -x /workspace/skills/report-tool/scripts/run.sh && /workspace/skills/report-tool/scripts/run.sh"},
	})
	if err != nil || result.ExitCode != 0 || string(result.Stdout) != "skill-ok" {
		t.Fatalf("execute mounted Skill helper: result=%+v err=%v", result, err)
	}
	result, err = first.Exec(ctx, Command{
		Path: "sh", Args: []string{"-c", "printf changed > /workspace/skills/report-tool/SKILL.md"},
	})
	if err != nil || result.ExitCode == 0 {
		t.Fatalf("write to read-only Skill mount: result=%+v err=%v", result, err)
	}
	childArchive, childExpanded := testSkillArchive(t, "Report_Tool", map[string]skillTestFile{
		"SKILL.md": {
			body: []byte("---\nname: report-tool\ndescription: Child reports\n---\nChild scope.\n"),
			mode: 0o644,
		},
	})
	childMount := testReadOnlySkillMount(
		"skill_reports@200", "report-tool", "Report_Tool",
		childArchive, childExpanded,
	)
	childMount.RuntimePath = domain.SessionSkillsRoot +
		"/.agents/0123456789abcdef01234567/report-tool"
	if err := skills.ImportReadOnlySkill(
		ctx, childMount, bytes.NewReader(childArchive),
	); err != nil {
		t.Fatalf("import Agent-scoped Skill: %v", err)
	}
	result, err = first.Exec(ctx, Command{
		Path: "sh", Args: []string{"-c",
			"grep -q 'Analyze reports' /workspace/skills/report-tool/SKILL.md && " +
				"grep -q 'Child reports' /workspace/skills/.agents/0123456789abcdef01234567/report-tool/SKILL.md"},
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("isolated same-name Skill bundles: result=%+v err=%v", result, err)
	}

	firstBox := first.(*dockerSandbox)
	stagedSkillMD := filepath.Join(
		firstBox.resourceRoot, dockerResourceSkillsDir, "report-tool", "SKILL.md",
	)
	if err := os.Remove(stagedSkillMD); err != nil {
		t.Fatalf("damage staged Skill: %v", err)
	}
	abandoned := filepath.Join(
		firstBox.resourceRoot, dockerResourceSkillsDir, dockerSkillTempPrefix+"abandoned",
	)
	if err := os.Mkdir(abandoned, 0o700); err != nil {
		t.Fatalf("create abandoned staging directory: %v", err)
	}
	present, err = skills.HasReadOnlySkill(ctx, mount)
	if err != nil || present {
		t.Fatalf("damaged Skill presence = %t, err=%v", present, err)
	}
	if err := skills.ImportReadOnlySkill(ctx, mount, bytes.NewReader(archive)); err != nil {
		t.Fatalf("repair Skill: %v", err)
	}
	if _, err := os.Stat(abandoned); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("abandoned staging directory survived repair: %v", err)
	}

	secondProviderInterface, err := NewDockerProvider(DockerConfig{
		DefaultImage: "alpine:latest", ResourceBaseDir: resourceBase,
	})
	if err != nil {
		t.Fatal(err)
	}
	attached, err := secondProviderInterface.Attach(
		ctx, t.Name(), ref, Spec{Timeout: 30 * time.Second},
	)
	if err != nil {
		t.Fatalf("attach after provider restart: %v", err)
	}
	restartedSkills := attached.(SkillBundleSandbox)
	present, err = restartedSkills.HasReadOnlySkill(ctx, mount)
	if err != nil || !present {
		t.Fatalf("restarted Skill presence = %t, err=%v", present, err)
	}
	result, err = attached.Exec(ctx, Command{
		Path: "sh", Args: []string{"-c", "grep -q 'Analyze reports' /workspace/skills/report-tool/SKILL.md"},
	})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf("read Skill after provider restart: result=%+v err=%v", result, err)
	}
	resourceRoot := firstBox.resourceRoot
	if err := attached.Destroy(ctx); err != nil {
		t.Fatalf("destroy attached sandbox: %v", err)
	}
	if _, err := os.Stat(resourceRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Skill resource root survived sandbox destruction: %v", err)
	}
}

func TestReadOnlySkillMountAcceptsOnlyPrimaryOrAgentScopedPaths(t *testing.T) {
	archive, expanded := testSkillArchive(t, "reports", map[string]skillTestFile{
		"SKILL.md": {
			body: []byte("---\nname: reports\ndescription: Test reports\n---\n"),
			mode: 0o644,
		},
	})
	mount := testReadOnlySkillMount(
		"skill_reports@100", "reports", "reports", archive, expanded,
	)
	for _, runtimePath := range []string{
		domain.SessionSkillsRoot + "/reports",
		domain.SessionSkillsRoot + "/.agents/0123456789abcdef01234567/reports",
	} {
		mount.RuntimePath = runtimePath
		if err := validateReadOnlySkillMount(mount); err != nil {
			t.Fatalf("valid runtime path %q: %v", runtimePath, err)
		}
	}
	for _, runtimePath := range []string{
		domain.SessionSkillsRoot + "/other",
		domain.SessionSkillsRoot + "/../reports",
		domain.SessionSkillsRoot + "/.agents/not-a-scope/reports",
		domain.SessionSkillsRoot + "/.agents/0123456789abcdef01234567/nested/reports",
	} {
		mount.RuntimePath = runtimePath
		if err := validateReadOnlySkillMount(mount); err == nil {
			t.Fatalf("invalid runtime path %q was accepted", runtimePath)
		}
	}
}

func TestDocker_SkillBundleRejectsStoredArchiveTraversal(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{dockerResourceSkillsDir, dockerResourceStateDir} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	var raw bytes.Buffer
	writer := zip.NewWriter(&raw)
	header := &zip.FileHeader{Name: "Safe/../../escape", Method: zip.Deflate}
	header.SetMode(0o644)
	part, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("escape")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	archive := raw.Bytes()
	mount := testReadOnlySkillMount(
		"skill_safe@100", "safe", "Safe", archive, int64(len("escape")),
	)
	box := &dockerSandbox{resourceRoot: root, skillMountReady: true}
	if err := box.ImportReadOnlySkill(
		context.Background(), mount, bytes.NewReader(archive),
	); err == nil || !strings.Contains(err.Error(), "unsafe path") {
		t.Fatalf("traversal import error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "escape")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("archive traversal wrote outside Skill tree: %v", err)
	}
}

func TestDocker_SkillBundleAcceptsLegacyUnknownExpandedSize(t *testing.T) {
	root := t.TempDir()
	for _, directory := range []string{dockerResourceSkillsDir, dockerResourceStateDir} {
		if err := os.Mkdir(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	archive, expanded := testSkillArchive(t, "legacy", map[string]skillTestFile{
		"SKILL.md": {
			body: []byte("---\nname: legacy\ndescription: Legacy Skill\n---\nUse it.\n"),
			mode: 0o644,
		},
	})
	mount := testReadOnlySkillMount(
		"skill_legacy@100", "legacy", "legacy", archive, expanded,
	)
	mount.UncompressedSizeBytes = domain.UnknownSkillUncompressedSize
	box := &dockerSandbox{resourceRoot: root, skillMountReady: true}
	if err := box.ImportReadOnlySkill(
		context.Background(), mount, bytes.NewReader(archive),
	); err != nil {
		t.Fatalf("import legacy Skill: %v", err)
	}
	present, err := box.HasReadOnlySkill(context.Background(), mount)
	if err != nil || !present {
		t.Fatalf("legacy Skill presence = %t, err=%v", present, err)
	}
}

func TestDocker_FileResourceImportStreamsLargeContent(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, dockerResourceFilesDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, dockerResourceStateDir), 0o700); err != nil {
		t.Fatal(err)
	}
	box := &dockerSandbox{resourceRoot: root, resourceMountReady: true}
	const size = int64(32 << 20)
	hash := sha256.New()
	if _, err := io.CopyN(hash, zeroReader{}, size); err != nil {
		t.Fatal(err)
	}
	mount := FileResourceMount{
		Identity:       "sesrsc_large",
		RuntimePath:    SessionUploadsRoot + "/large.bin",
		SizeBytes:      size,
		ChecksumSHA256: hex.EncodeToString(hash.Sum(nil)),
	}
	reader := &trackingZeroReader{remaining: size}
	if err := box.ImportFileResource(context.Background(), mount, reader); err != nil {
		t.Fatal(err)
	}
	if reader.maxRequest > 1<<20 {
		t.Fatalf("stream read buffer = %d bytes, want at most 1 MiB", reader.maxRequest)
	}
	info, err := os.Stat(filepath.Join(root, dockerResourceFilesDir, "large.bin"))
	if err != nil || info.Size() != size {
		t.Fatalf("large resource info=%+v err=%v", info, err)
	}
}

func TestDocker_FileResourceDirectoryModesIgnoreUmask(t *testing.T) {
	oldUmask := syscall.Umask(0o077)
	defer syscall.Umask(oldUmask)

	providerInterface, err := newDockerProviderWithEngine(DockerConfig{
		ResourceBaseDir: t.TempDir(),
	}, &stubDockerEngine{})
	if err != nil {
		t.Fatal(err)
	}
	provider := providerInterface.(*dockerProvider)
	root, _, _, _, _, err := provider.ensureResourceRoot(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root) //nolint:errcheck // test cleanup

	box := &dockerSandbox{resourceRoot: root, resourceMountReady: true}
	content := []byte("mode check")
	mount := testFileResourceMount(SessionUploadsRoot+"/nested/mode.txt", content)
	if err := box.ImportFileResource(context.Background(), mount, bytes.NewReader(content)); err != nil {
		t.Fatal(err)
	}
	for target, want := range map[string]os.FileMode{
		filepath.Join(root, dockerResourceFilesDir):           0o755,
		filepath.Join(root, dockerResourceOutputsDir):         0o777,
		filepath.Join(root, dockerResourceFilesDir, "nested"): 0o755,
		filepath.Join(root, dockerResourceStateDir):           0o700,
	} {
		info, err := os.Stat(target)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("mode for %s = %#o, want %#o", target, got, want)
		}
	}
}

func TestDocker_ReapsOnlyStaleUnreferencedResourceRoots(t *testing.T) {
	base := t.TempDir()
	providerInterface, err := newDockerProviderWithEngine(DockerConfig{
		ResourceBaseDir: base,
	}, &stubDockerEngine{})
	if err != nil {
		t.Fatal(err)
	}
	provider := providerInterface.(*dockerProvider)
	stale := filepath.Join(base, dockerResourceRootPrefix("stale")+"abcdef")
	fresh := filepath.Join(base, dockerResourceRootPrefix("fresh")+"abcdef")
	unowned := filepath.Join(base, "other-stale-directory")
	for _, directory := range []string{stale, fresh, unowned} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(unowned, old, old); err != nil {
		t.Fatal(err)
	}
	if err := provider.reapStaleResourceRoots(
		context.Background(), now.Add(-time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale provider root remains: %v", err)
	}
	for _, directory := range []string{fresh, unowned} {
		if _, err := os.Stat(directory); err != nil {
			t.Fatalf("protected directory %s was removed: %v", directory, err)
		}
	}
}

func TestDocker_ReaperDoesNothingWhenContainerAuditFails(t *testing.T) {
	base := t.TempDir()
	providerInterface, err := newDockerProviderWithEngine(DockerConfig{
		ResourceBaseDir: base,
	}, &stubDockerEngine{
		listFn: func(
			context.Context,
			dockerclient.ContainerListOptions,
		) (dockerclient.ContainerListResult, error) {
			return dockerclient.ContainerListResult{}, errors.New("audit unavailable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := providerInterface.(*dockerProvider)
	stale := filepath.Join(base, dockerResourceRootPrefix("stale")+"abcdef")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := provider.reapStaleResourceRoots(
		context.Background(), time.Now().Add(-time.Hour),
	); err == nil {
		t.Fatal("reaper audit unexpectedly succeeded")
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("failed audit removed staging directory: %v", err)
	}
}

func TestDocker_ResourceMountInspectFailureRemainsRetryable(t *testing.T) {
	providerInterface, err := newDockerProviderWithEngine(DockerConfig{
		ResourceBaseDir: t.TempDir(),
	}, &stubDockerEngine{
		inspectFn: func(
			context.Context,
			string,
			dockerclient.ContainerInspectOptions,
		) (dockerclient.ContainerInspectResult, error) {
			return dockerclient.ContainerInspectResult{}, errors.New("inspect unavailable")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	provider := providerInterface.(*dockerProvider)
	_, _, err = provider.inspectResourceMount(
		context.Background(), "container", "session",
	)
	if err == nil || IsPermanent(err) {
		t.Fatalf("inspect error = %v, want retryable failure", err)
	}
}

func TestDocker_DefaultResourceDirectoryFallsBackWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	providerInterface, err := newDockerProviderWithEngine(
		DockerConfig{}, &stubDockerEngine{},
	)
	if err != nil {
		t.Fatal(err)
	}
	provider := providerInterface.(*dockerProvider)
	want := filepath.Join(
		os.TempDir(), fmt.Sprintf("mango-resources-%d", os.Getuid()),
	)
	if provider.resourceBaseDir != want {
		t.Fatalf("resourceBaseDir = %q, want %q", provider.resourceBaseDir, want)
	}
}

func testFileResourceMount(runtimePath string, content []byte) FileResourceMount {
	sum := sha256.Sum256(content)
	return FileResourceMount{
		Identity: "sesrsc_test", RuntimePath: runtimePath, SizeBytes: int64(len(content)),
		ChecksumSHA256: hex.EncodeToString(sum[:]),
	}
}

type skillTestFile struct {
	body []byte
	mode os.FileMode
}

func testSkillArchive(
	t *testing.T,
	root string,
	files map[string]skillTestFile,
) ([]byte, int64) {
	t.Helper()
	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	var expanded int64
	for _, relative := range []string{"SKILL.md", "scripts/run.sh"} {
		file, ok := files[relative]
		if !ok {
			continue
		}
		header := &zip.FileHeader{Name: root + "/" + relative, Method: zip.Deflate}
		header.SetMode(file.mode)
		part, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(file.body); err != nil {
			t.Fatal(err)
		}
		expanded += int64(len(file.body))
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return archive.Bytes(), expanded
}

func testReadOnlySkillMount(
	identity string,
	name string,
	archiveRoot string,
	archive []byte,
	expanded int64,
) ReadOnlySkillMount {
	sum := sha256.Sum256(archive)
	return ReadOnlySkillMount{
		Identity: identity, Name: name, ArchiveRoot: archiveRoot,
		SizeBytes: int64(len(archive)), UncompressedSizeBytes: expanded,
		ChecksumSHA256: hex.EncodeToString(sum[:]),
	}
}

type readerThatFails struct {
	data []byte
}

func (r *readerThatFails) Read(buffer []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, errors.New("injected read failure")
	}
	n := copy(buffer, r.data)
	r.data = r.data[n:]
	return n, nil
}

type zeroReader struct{}

func (zeroReader) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

type trackingZeroReader struct {
	remaining  int64
	maxRequest int
}

func (r *trackingZeroReader) Read(buffer []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	if len(buffer) > r.maxRequest {
		r.maxRequest = len(buffer)
	}
	if int64(len(buffer)) > r.remaining {
		buffer = buffer[:r.remaining]
	}
	clear(buffer)
	r.remaining -= int64(len(buffer))
	return len(buffer), nil
}
