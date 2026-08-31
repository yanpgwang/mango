package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/containerd/errdefs"
	"github.com/distribution/reference"
	dockerconfig "github.com/docker/cli/cli/config"
	"github.com/docker/cli/cli/config/credentials"
	"github.com/docker/go-units"
	"github.com/moby/moby/api/pkg/authconfig"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	registrytypes "github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/client"
	"github.com/yanpgwang/mango/internal/domain"
)

const (
	dockerManagedLabel    = "io.mango.managed"
	dockerSessionKeyLabel = "io.mango.session_key"
	// DefaultDockerImage includes Python and POSIX tools for the default OSS
	// workflow. Operators may choose their own image through DockerConfig.
	DefaultDockerImage = "python:3.12-alpine"
)

// Docker provider notes on isolation:
//
// Each sandbox is a container with its own kernel view via Linux namespaces and
// cgroups, its filesystem is separate from the host, and networking defaults to
// --network none. This is a genuine security boundary for ordinary untrusted
// code, not merely a dev-grade guardrail.
//
// It has NOT been audited for hostile multi-tenant use: a container shares the
// host kernel, so a kernel-level exploit could still cross the boundary. When
// that threat matters, gVisor can be layered under the same interface by adding
// --runtime=runsc to the create arguments; no interface change is required.

// DockerConfig configures the Docker-backed Provider.
type DockerConfig struct {
	// DefaultImage is used when Spec.Image is empty. Empty defaults to
	// DefaultDockerImage.
	DefaultImage string
	// ResourceBaseDir stores provider-owned File Resource staging directories.
	// Empty uses a stable, non-hidden directory beneath the host user's home.
	ResourceBaseDir string
	// RegistryAuthConfigDir is the directory containing Docker's config.json.
	// Empty honors DOCKER_CONFIG and then ~/.docker. The provider resolves the
	// standard auths, credsStore, and credHelpers entries only when a missing
	// image must be pulled.
	RegistryAuthConfigDir string
}

// dockerRoot is the working directory inside every container.
const dockerRoot = "/workspace"

// keepAlive holds the container open so exec can attach repeatedly. sleep with a
// huge argument keeps PID 1 alive until the container is removed.
const keepAlive = "sleep 2147483647"

// dockerEngine is the narrow Engine API surface used by the provider. Keeping
// this boundary smaller than client.APIClient makes lifecycle failure paths
// unit-testable without a daemon.
type dockerEngine interface {
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerKill(context.Context, string, client.ContainerKillOptions) (client.ContainerKillResult, error)
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
	ImagePull(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error)
	ExecCreate(context.Context, string, client.ExecCreateOptions) (client.ExecCreateResult, error)
	ExecAttach(context.Context, string, client.ExecAttachOptions) (client.ExecAttachResult, error)
	ExecInspect(context.Context, string, client.ExecInspectOptions) (client.ExecInspectResult, error)
	CopyFromContainer(context.Context, string, client.CopyFromContainerOptions) (client.CopyFromContainerResult, error)
	CopyToContainer(context.Context, string, client.CopyToContainerOptions) (client.CopyToContainerResult, error)
}

// dockerProvider provisions container-backed sandboxes through the Docker
// Engine API. It never depends on the docker CLI binary or its subprocess
// behavior.
type dockerProvider struct {
	engine          dockerEngine
	defaultImage    string
	resourceBaseDir string
	registryAuthDir string

	resourceAuditMu sync.Mutex
	resourceAuditAt time.Time
}

const (
	dockerResourceReapGrace    = 24 * time.Hour
	dockerResourceAuditEvery   = time.Hour
	dockerExecInspectPollEvery = 10 * time.Millisecond
)

// NewDockerProvider returns a Provider backed by Docker's supported Go client.
// client.FromEnv honors DOCKER_HOST, DOCKER_API_VERSION and TLS variables; the
// client negotiates a compatible Engine API version on its first request.
func NewDockerProvider(cfg DockerConfig) (Provider, error) {
	engine, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("sandbox: create Docker Engine client: %w", err)
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := engine.Ping(probeCtx, client.PingOptions{
		NegotiateAPIVersion: true,
	}); err != nil {
		_ = engine.Close()
		return nil, fmt.Errorf("sandbox: Docker Engine is unreachable: %w", err)
	}
	provider, err := newDockerProviderWithEngine(cfg, engine)
	if err != nil {
		_ = engine.Close()
		return nil, err
	}
	return provider, nil
}

func newDockerProviderWithEngine(cfg DockerConfig, engine dockerEngine) (Provider, error) {
	if engine == nil {
		return nil, errors.New("sandbox: Docker Engine client is required")
	}
	image := cfg.DefaultImage
	if image == "" {
		image = DefaultDockerImage
	}
	resourceBaseDir := cfg.ResourceBaseDir
	if resourceBaseDir == "" {
		userDir, userErr := os.UserHomeDir()
		if userErr != nil {
			userDir = filepath.Join(
				os.TempDir(), fmt.Sprintf("mango-resources-%d", os.Getuid()),
			)
			resourceBaseDir = userDir
		} else {
			// Keep this directory non-hidden. Docker Desktop's macOS file sharing can
			// retain a negative lookup for newly created descendants of hidden
			// directories, causing otherwise valid bind mounts to fail until the VM is
			// restarted. A visible, stable home-directory path avoids that daemon-side
			// cache edge while remaining configurable for production deployments.
			resourceBaseDir = filepath.Join(userDir, "mango-resources")
		}
	}
	resourceBaseDir, err := filepath.Abs(resourceBaseDir)
	if err != nil {
		return nil, fmt.Errorf("sandbox: resolve docker resource directory: %w", err)
	}
	return &dockerProvider{
		engine: engine, defaultImage: image, resourceBaseDir: resourceBaseDir,
		registryAuthDir: cfg.RegistryAuthConfigDir,
	}, nil
}

func (p *dockerProvider) Name() string { return DockerProviderName }

func (*dockerProvider) SupportsPackageSetup() bool { return true }

func (*dockerProvider) SupportsFileResources() bool { return true }

func (*dockerProvider) SupportsSessionOutputs() bool { return true }

func (*dockerProvider) SupportsSkillBundles() bool { return true }

func (*dockerProvider) SupportsMemoryStores() bool { return true }

func (*dockerProvider) SupportsGitRepositories() bool { return true }

func (p *dockerProvider) Create(
	ctx context.Context,
	sessionKey string,
	spec Spec,
) (Ref, Sandbox, error) {
	if sessionKey == "" {
		return Ref{}, nil, errors.New("sandbox: session key is required")
	}
	p.auditResourceRoots()
	name := dockerContainerName(sessionKey)
	if box, err := p.attachTarget(ctx, name, sessionKey, spec); err == nil {
		return Ref{Provider: p.Name(), ID: box.cid}, box, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Ref{}, nil, err
	}

	image := spec.Image
	if image == "" {
		image = p.defaultImage
	}
	network := spec.Network
	if network == "" {
		network = "none"
	}
	resourceRoot, resourceFiles, resourceOutputs, resourceSkills, resourceMemory, err :=
		p.ensureResourceRoot(sessionKey)
	if err != nil {
		return Ref{}, nil, err
	}
	memoryMounts, err := prepareDockerMemoryMounts(resourceMemory, spec.MemoryStores)
	if err != nil {
		_ = os.RemoveAll(resourceRoot)
		return Ref{}, nil, err
	}

	mounts := []mount.Mount{
		{
			Type: mount.TypeBind, Source: resourceFiles,
			Target: SessionUploadsRoot, ReadOnly: true,
		},
		{
			Type: mount.TypeBind, Source: resourceOutputs,
			Target: SessionOutputsRoot,
		},
		{
			Type: mount.TypeBind, Source: resourceSkills,
			Target: domain.SessionSkillsRoot, ReadOnly: true,
		},
	}
	for _, memoryMount := range spec.MemoryStores {
		mounts = append(mounts, mount.Mount{
			Type:     mount.TypeBind,
			Source:   memoryMounts[memoryMount.Identity],
			Target:   memoryMount.RuntimePath,
			ReadOnly: memoryMount.Access == domain.MemoryAccessReadOnly,
		})
	}
	resources := container.Resources{}
	if spec.Memory != "" {
		resources.Memory, err = units.RAMInBytes(spec.Memory)
		if err != nil || resources.Memory <= 0 {
			_ = os.RemoveAll(resourceRoot)
			return Ref{}, nil, Permanent(fmt.Errorf(
				"sandbox: invalid Docker memory limit %q", spec.Memory,
			))
		}
	}
	if spec.CPUs != "" {
		cpus, parseErr := strconv.ParseFloat(spec.CPUs, 64)
		if parseErr != nil || math.IsNaN(cpus) || math.IsInf(cpus, 0) ||
			cpus <= 0 || cpus >= float64(math.MaxInt64)/1e9 {
			_ = os.RemoveAll(resourceRoot)
			return Ref{}, nil, Permanent(fmt.Errorf(
				"sandbox: invalid Docker CPU limit %q", spec.CPUs,
			))
		}
		resources.NanoCPUs = int64(cpus * 1e9)
		if resources.NanoCPUs <= 0 {
			_ = os.RemoveAll(resourceRoot)
			return Ref{}, nil, Permanent(fmt.Errorf(
				"sandbox: invalid Docker CPU limit %q", spec.CPUs,
			))
		}
	}
	if spec.PidsLimit > 0 {
		pidsLimit := int64(spec.PidsLimit)
		resources.PidsLimit = &pidsLimit
	}
	if err := p.ensureImage(ctx, image); err != nil {
		_ = os.RemoveAll(resourceRoot)
		return Ref{}, nil, err
	}
	created, err := p.engine.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:      image,
			WorkingDir: dockerRoot,
			Cmd:        []string{"sh", "-c", keepAlive},
			Labels: map[string]string{
				dockerManagedLabel:    "true",
				dockerSessionKeyLabel: sessionKey,
			},
		},
		HostConfig: &container.HostConfig{
			NetworkMode: container.NetworkMode(network),
			Mounts:      mounts,
			Resources:   resources,
		},
	})
	if err != nil {
		// Two workers may race after both observe no persisted binding. Docker's
		// unique container name is the provider-side idempotency key: the loser
		// attaches to the winner rather than creating a second resource.
		if errdefs.IsConflict(err) || errdefs.IsAlreadyExists(err) {
			box, attachErr := p.attachTarget(ctx, name, sessionKey, spec)
			if attachErr == nil {
				_ = os.RemoveAll(resourceRoot)
				return Ref{Provider: p.Name(), ID: box.cid}, box, nil
			}
			_ = os.RemoveAll(resourceRoot)
			return Ref{}, nil, fmt.Errorf(
				"sandbox: Docker Engine create conflict: %w",
				errors.Join(err, attachErr),
			)
		}
		if dockerCreateDefinitelyRejected(err) {
			_ = os.RemoveAll(resourceRoot)
		}
		// A transport failure can lose the create acknowledgement after the
		// daemon accepted it. Preserve the unique mount generation for the next
		// attach/audit instead of deleting a live container's bind source.
		return Ref{}, nil, fmt.Errorf("sandbox: Docker Engine create: %w", err)
	}
	cid := strings.TrimSpace(created.ID)
	if cid == "" {
		return Ref{}, nil, errors.New("sandbox: Docker Engine create returned empty container ID")
	}

	// From here on, any failure must remove the created container so we never
	// leak it.
	if _, startErr := p.engine.ContainerStart(ctx, cid, client.ContainerStartOptions{}); startErr != nil {
		if p.forceRemove(cid) {
			_ = os.RemoveAll(resourceRoot)
		}
		// forceRemove is best-effort. Keep the mount generation if daemon cleanup
		// could not be acknowledged; a provider audit can remove both safely.
		return Ref{}, nil, fmt.Errorf("sandbox: Docker Engine start: %w", startErr)
	}

	ref := Ref{Provider: p.Name(), ID: cid}
	box := &dockerSandbox{
		provider:           p,
		cid:                cid,
		timeout:            spec.Timeout,
		fileRoots:          dockerToolFileRoots(spec),
		resourceRoot:       resourceRoot,
		resourceMountReady: true,
		outputMountReady:   true,
		skillMountReady:    true,
		memoryMounts:       memoryMounts,
	}
	box.initGitRepositories()
	return ref, box, nil
}

// ensureImage preserves `docker create` parity without invoking the CLI. The
// Engine create endpoint never pulls a missing image, so the adapter performs
// an explicit pull and consumes its progress stream before creating the
// container. Concurrent pulls are safe and deduplicated by the daemon.
func (p *dockerProvider) ensureImage(ctx context.Context, image string) error {
	if _, err := p.engine.ImageInspect(ctx, image); err == nil {
		return nil
	} else if !errdefs.IsNotFound(err) {
		return fmt.Errorf("sandbox: Docker Engine inspect image %q: %w", image, err)
	}
	registryAuth, err := dockerRegistryAuth(p.registryAuthDir, image)
	if err != nil {
		return fmt.Errorf("sandbox: resolve Docker registry auth for %q: %w", image, err)
	}
	pulled, err := p.engine.ImagePull(ctx, image, client.ImagePullOptions{
		RegistryAuth: registryAuth,
	})
	if err != nil {
		return fmt.Errorf("sandbox: Docker Engine pull image %q: %w", image, err)
	}
	defer func() { _ = pulled.Close() }()
	if err := pulled.Wait(ctx); err != nil {
		return fmt.Errorf("sandbox: Docker Engine pull image %q: %w", image, err)
	}
	return nil
}

// dockerRegistryAuth preserves the credential behavior users get from
// `docker pull` without executing the docker CLI. Docker's official config
// package resolves inline auth plus native credential stores/helpers; the
// Engine API receives the same base64url-encoded AuthConfig payload.
func dockerRegistryAuth(configDir, image string) (string, error) {
	configFile, err := dockerconfig.Load(configDir)
	if err != nil {
		return "", err
	}
	if !configFile.ContainsAuth() {
		configFile.CredentialsStore = credentials.DetectDefaultStore(
			configFile.CredentialsStore,
		)
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return "", err
	}
	resolved, err := configFile.GetAuthConfig(reference.Domain(named))
	if err != nil {
		return "", err
	}
	return authconfig.Encode(registrytypes.AuthConfig{
		Username:      resolved.Username,
		Password:      resolved.Password,
		Auth:          resolved.Auth,
		ServerAddress: resolved.ServerAddress,
		IdentityToken: resolved.IdentityToken,
		RegistryToken: resolved.RegistryToken,
	})
}

// dockerCreateDefinitelyRejected identifies Engine responses for which the
// daemon definitively did not create a container. Transport, cancellation,
// internal, and unknown errors deliberately retain the bind-mount generation:
// they may represent a lost acknowledgement after a successful create.
func dockerCreateDefinitelyRejected(err error) bool {
	return errdefs.IsInvalidArgument(err) || errdefs.IsNotFound(err) ||
		errdefs.IsPermissionDenied(err) || errdefs.IsUnauthorized(err) ||
		errdefs.IsFailedPrecondition(err) || errdefs.IsNotImplemented(err)
}

func (p *dockerProvider) Attach(
	ctx context.Context,
	sessionKey string,
	ref Ref,
	spec Spec,
) (Sandbox, error) {
	if sessionKey == "" {
		return nil, Permanent(errors.New("sandbox: session key is required"))
	}
	p.auditResourceRoots()
	if err := ref.validate(); err != nil {
		return nil, Permanent(err)
	}
	if ref.Provider != p.Name() {
		return nil, Permanent(fmt.Errorf(
			"sandbox: docker provider cannot attach reference for %q",
			ref.Provider,
		))
	}
	return p.attachTarget(ctx, ref.ID, sessionKey, spec)
}

func dockerContainerName(sessionKey string) string {
	sum := sha256.Sum256([]byte(sessionKey))
	return fmt.Sprintf("mango-%x", sum[:16])
}

func (p *dockerProvider) attachTarget(
	ctx context.Context,
	target string,
	expectedSessionKey string,
	spec Spec,
) (*dockerSandbox, error) {
	result, err := p.engine.ContainerInspect(ctx, target, client.ContainerInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil, fmt.Errorf("%w: docker container %q", ErrNotFound, target)
		}
		return nil, fmt.Errorf("sandbox: Docker Engine inspect: %w", err)
	}
	inspected := result.Container
	if inspected.ID == "" || inspected.Config == nil || inspected.State == nil {
		return nil, fmt.Errorf("sandbox: invalid docker inspect result for %q", target)
	}
	if inspected.Config.Labels[dockerManagedLabel] != "true" {
		return nil, Permanent(fmt.Errorf(
			"sandbox: refusing to attach unmanaged docker container %q",
			target,
		))
	}
	if expectedSessionKey != "" &&
		inspected.Config.Labels[dockerSessionKeyLabel] != expectedSessionKey {
		return nil, Permanent(fmt.Errorf(
			"sandbox: docker container %q belongs to another session",
			target,
		))
	}
	if !inspected.State.Running {
		if _, startErr := p.engine.ContainerStart(
			ctx, inspected.ID, client.ContainerStartOptions{},
		); startErr != nil {
			return nil, fmt.Errorf("sandbox: Docker Engine start attached container: %w", startErr)
		}
	}
	resourceRoot, resourceMountReady, err := p.inspectResourceMount(
		ctx, inspected.ID, expectedSessionKey,
	)
	if err != nil {
		return nil, err
	}
	skillMountReady, err := p.inspectSkillMount(ctx, inspected.ID, resourceRoot)
	if err != nil {
		return nil, err
	}
	outputMountReady, err := p.inspectOutputMount(ctx, inspected.ID, resourceRoot)
	if err != nil {
		return nil, err
	}
	memoryMounts, err := p.inspectMemoryMounts(
		ctx, inspected.ID, resourceRoot, spec.MemoryStores,
	)
	if err != nil {
		return nil, err
	}
	box := &dockerSandbox{
		provider:           p,
		cid:                inspected.ID,
		timeout:            spec.Timeout,
		fileRoots:          dockerToolFileRoots(spec),
		resourceRoot:       resourceRoot,
		resourceMountReady: resourceMountReady,
		outputMountReady:   outputMountReady,
		skillMountReady:    skillMountReady,
		memoryMounts:       memoryMounts,
	}
	box.initGitRepositories()
	return box, nil
}

// forceRemove best-effort removes a container with a fresh bounded context so
// cleanup still runs when the originating request has already been cancelled.
func (p *dockerProvider) forceRemove(cid string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := p.engine.ContainerRemove(ctx, cid, client.ContainerRemoveOptions{Force: true})
	return err == nil || errdefs.IsNotFound(err)
}

// kill best-effort SIGKILLs the container (PID 1), tearing down any exec'd
// processes so a timed-out command does not linger. The container remains
// present (stopped) so Destroy can still remove it.
func (p *dockerProvider) kill(cid string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _ = p.engine.ContainerKill(ctx, cid, client.ContainerKillOptions{Signal: "KILL"})
}

type dockerSandbox struct {
	provider *dockerProvider
	cid      string
	timeout  time.Duration
	// fileRoots is the complete file-tool authority inside the container.
	// Bash still follows the container's own filesystem permissions, while
	// Read/Write/Edit are limited to these explicit roots.
	fileRoots []dockerToolFileRoot

	resourceRoot       string
	resourceMountReady bool
	outputMountReady   bool
	skillMountReady    bool
	memoryMounts       map[string]string
	repositories       *commandGitRepositories

	// mu guards dead. Once the container is torn down (timed-out and killed, or
	// destroyed), the sandbox is permanently dead: further calls must fail fast
	// rather than shell out to docker against a stopped/removed container, which
	// would otherwise surface as a confusing non-zero exit with err==nil.
	mu   sync.Mutex
	dead bool
}

// errDead is returned by operations on a sandbox whose container has been torn
// down. The caller must provision a fresh sandbox.
var errDead = errors.New("sandbox: container terminated (timed out or destroyed); provision a new sandbox")

func (s *dockerSandbox) markDead() {
	s.mu.Lock()
	s.dead = true
	s.mu.Unlock()
}

func (s *dockerSandbox) isDead() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dead
}

func (s *dockerSandbox) Root() string { return dockerRoot }

func (p *dockerProvider) execContainer(
	ctx context.Context,
	cid string,
	cmd Command,
) (*Result, error) {
	command := append([]string{cmd.Path}, cmd.Args...)
	created, err := p.engine.ExecCreate(ctx, cid, client.ExecCreateOptions{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		WorkingDir:   dockerRoot,
		Cmd:          command,
	})
	if err != nil {
		return &Result{ExitCode: -1}, fmt.Errorf("sandbox: Docker Engine exec create: %w", err)
	}
	attached, err := p.engine.ExecAttach(ctx, created.ID, client.ExecAttachOptions{})
	if err != nil {
		return &Result{ExitCode: -1}, fmt.Errorf("sandbox: Docker Engine exec attach: %w", err)
	}
	defer attached.Close()

	// Stdin and output must flow concurrently: either side may exceed a socket
	// buffer before the process consumes the other. Closing the write half is
	// also how commands waiting for EOF are allowed to finish.
	go func() {
		if len(cmd.Stdin) > 0 {
			_, _ = io.Copy(attached.Conn, bytes.NewReader(cmd.Stdin))
		}
		_ = attached.CloseWrite()
	}()

	stdout := &cappedBuffer{cap: maxOutput}
	stderr := &cappedBuffer{cap: maxOutput}
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := stdcopy.StdCopy(stdout, stderr, attached.Reader)
		copyDone <- copyErr
	}()

	var copyErr error
	select {
	case copyErr = <-copyDone:
	case <-ctx.Done():
		attached.Close()
		<-copyDone
		return &Result{
			Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1,
		}, ctx.Err()
	}
	if copyErr != nil {
		return &Result{
			Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1,
		}, fmt.Errorf("sandbox: read Docker exec stream: %w", copyErr)
	}
	for {
		inspected, err := p.engine.ExecInspect(ctx, created.ID, client.ExecInspectOptions{})
		if err != nil {
			return &Result{
				Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1,
			}, fmt.Errorf("sandbox: Docker Engine exec inspect: %w", err)
		}
		if !inspected.Running {
			return &Result{
				Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: inspected.ExitCode,
			}, nil
		}
		timer := time.NewTimer(dockerExecInspectPollEvery)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return &Result{
				Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1,
			}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *dockerSandbox) Exec(ctx context.Context, cmd Command) (*Result, error) {
	if s.isDead() {
		return nil, errDead
	}

	// Bound the command by the sandbox timeout while still honoring an
	// already-cancelled parent context.
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}

	res, err := s.provider.execContainer(ctx, s.cid, cmd)

	// Timeout: the deadline elapsing means docker exec was killed. The process
	// inside the container keeps running, so kill the container best-effort to
	// avoid a lingering runaway command. Killing PID 1 stops the whole
	// container, so the sandbox is now permanently dead.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		if res == nil {
			res = &Result{}
		}
		res.TimedOut = true
		res.ExitCode = -1
		s.markDead()
		s.provider.kill(s.cid)
		return res, nil
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return res, ctx.Err()
	}
	if err != nil {
		return res, err
	}
	return res, nil
}

type dockerToolFileRoot struct {
	path     string
	writable bool
}

// dockerToolFileRoots builds the explicit authority used by standard file
// tools. More-specific roots come first so the immutable Skill tree remains
// read-only even though it is nested beneath the writable workspace.
func dockerToolFileRoots(spec Spec) []dockerToolFileRoot {
	roots := []dockerToolFileRoot{
		{path: domain.SessionSkillsRoot},
		{path: SessionUploadsRoot},
		{path: SessionOutputsRoot, writable: true},
	}
	for _, mount := range spec.MemoryStores {
		roots = append(roots, dockerToolFileRoot{
			path:     path.Clean(mount.RuntimePath),
			writable: mount.Access == domain.MemoryAccessReadWrite,
		})
	}
	roots = append(roots, dockerToolFileRoot{path: dockerRoot, writable: true})
	sort.SliceStable(roots, func(i, j int) bool {
		return len(roots[i].path) > len(roots[j].path)
	})
	return roots
}

// toolFilePath resolves a caller-supplied path using POSIX container semantics
// and verifies it against the sandbox's explicit file-tool authority. It never
// touches the host filesystem, so traversal checks are identical on every
// worker OS.
func (s *dockerSandbox) toolFilePath(value string, write bool) (string, error) {
	clean := path.Clean(value)
	if !path.IsAbs(clean) {
		clean = path.Clean(path.Join(dockerRoot, clean))
	}
	roots := s.fileRoots
	if len(roots) == 0 {
		roots = dockerToolFileRoots(Spec{})
	}
	for _, root := range roots {
		if clean != root.path && !strings.HasPrefix(clean+"/", root.path+"/") {
			continue
		}
		if write && !root.writable {
			return "", fmt.Errorf("sandbox: path %q is read-only", value)
		}
		return clean, nil
	}
	return "", fmt.Errorf("sandbox: path %q escapes root and approved resource mounts", value)
}

// ReadFile streams the Engine archive response and rejects non-regular files.
// Binary content round-trips without exposing a container-provided symlink on
// the worker filesystem.
func (s *dockerSandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	if s.isDead() {
		return nil, errDead
	}
	containerPath, err := s.toolFilePath(path, false)
	if err != nil {
		return nil, err
	}

	copied, err := s.provider.engine.CopyFromContainer(
		ctx,
		s.cid,
		client.CopyFromContainerOptions{SourcePath: containerPath},
	)
	if err != nil {
		return nil, fmt.Errorf("sandbox: Docker Engine copy from container: %w", err)
	}
	defer func() { _ = copied.Content.Close() }()
	reader := tar.NewReader(copied.Content)
	header, err := reader.Next()
	if err != nil {
		return nil, fmt.Errorf("sandbox: read Docker file archive: %w", err)
	}
	if !header.FileInfo().Mode().IsRegular() {
		return nil, Permanent(fmt.Errorf(
			"sandbox: path %q is not a regular file", path,
		))
	}
	content, err := io.ReadAll(reader)
	if err != nil {
		return nil, fmt.Errorf("sandbox: read Docker file content: %w", err)
	}
	return content, nil
}

func (s *dockerSandbox) ReadFileBounded(
	ctx context.Context,
	value string,
	maxBytes int64,
) ([]byte, bool, error) {
	containerPath, err := s.toolFilePath(value, false)
	if err != nil {
		return nil, false, err
	}
	return readFileBoundedByCommand(ctx, s, containerPath, maxBytes)
}

// OpenSessionOutputs snapshots the provider-owned writable output mount as a
// stream. Docker serializes the directory through its archive API, so worker
// memory never scales with deliverable size. The application layer validates
// every tar entry before publishing it.
func (s *dockerSandbox) OpenSessionOutputs(
	ctx context.Context,
) (io.ReadCloser, error) {
	if s.isDead() {
		return nil, errDead
	}
	if !s.outputMountReady {
		return nil, Permanent(errors.New(
			"sandbox: Docker Session predates the output mount; recreate the Session sandbox",
		))
	}
	copied, err := s.provider.engine.CopyFromContainer(
		ctx,
		s.cid,
		client.CopyFromContainerOptions{SourcePath: SessionOutputsRoot + "/."},
	)
	if err != nil {
		return nil, fmt.Errorf("sandbox: export Docker Session outputs: %w", err)
	}
	return copied.Content, nil
}

// WriteFile creates the parent directory through Engine exec, then streams one
// regular-file tar entry into the container through the archive API.
func (s *dockerSandbox) WriteFile(ctx context.Context, filePath string, data []byte) error {
	if s.isDead() {
		return errDead
	}
	containerPath, err := s.toolFilePath(filePath, true)
	if err != nil {
		return err
	}

	parent := path.Dir(containerPath)
	result, err := s.Exec(ctx, Command{
		Path: "mkdir", Args: []string{"-p", parent},
	})
	if err != nil {
		return fmt.Errorf("sandbox: create Docker file parent: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf(
			"sandbox: create Docker file parent failed (exit %d): %s",
			result.ExitCode, strings.TrimSpace(string(result.Stderr)),
		)
	}

	var archive bytes.Buffer
	writer := tar.NewWriter(&archive)
	if err := writer.WriteHeader(&tar.Header{
		// Keep archive writes readable for Memory synchronization; path
		// authority still prevents writes outside approved roots. Directory
		// ownership still follows the image user on native Linux bind mounts.
		Name: path.Base(containerPath), Mode: 0o644, Size: int64(len(data)),
		Typeflag: tar.TypeReg, ModTime: time.Now(),
	}); err != nil {
		return fmt.Errorf("sandbox: create Docker file archive: %w", err)
	}
	if _, err := writer.Write(data); err != nil {
		return fmt.Errorf("sandbox: write Docker file archive: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("sandbox: close Docker file archive: %w", err)
	}
	if _, err := s.provider.engine.CopyToContainer(
		ctx,
		s.cid,
		client.CopyToContainerOptions{
			DestinationPath: parent,
			Content:         bytes.NewReader(archive.Bytes()),
		},
	); err != nil {
		return fmt.Errorf("sandbox: Docker Engine copy to container: %w", err)
	}
	return nil
}

func (s *dockerSandbox) Destroy(ctx context.Context) error {
	// A destroyed sandbox is dead regardless of the rm outcome: the intent is
	// teardown, so block any later Exec from hitting a removed container.
	s.markDead()
	// Teardown uses a fresh, bounded context (not the caller's ctx) so the
	// container is still removed even when the caller's context is already
	// cancelled — the same rule forceRemove/kill follow. Using the caller ctx
	// here would let a cancelled Run leak the container once cancellation is
	// wired into the runtime.
	rmCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := s.provider.engine.ContainerRemove(
		rmCtx, s.cid, client.ContainerRemoveOptions{Force: true},
	); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("sandbox: Docker Engine remove: %w", err)
	}
	if s.resourceRoot == "" {
		return nil
	}
	if err := os.RemoveAll(s.resourceRoot); err != nil {
		return fmt.Errorf("sandbox: remove docker resource directory: %w", err)
	}
	return nil
}
