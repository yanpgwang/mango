// Package selfhosted implements operator-side launchers for Mango's
// self_hosted Environment Work protocol. Provider code lives here, outside the
// control plane: the provider only supplies compute while the Mango SDK owns
// lease, event-recovery, tool-result, and Stop semantics.
package selfhosted

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	mango "github.com/yanpgwang/mango/sdk/go"
)

const (
	DefaultWorkerImage       = "mango-self-hosted-worker:local"
	defaultSandboxWorkdir    = "/workspace"
	defaultRunnerPath        = "/usr/local/bin/mango-worker"
	defaultWorkerMemoryBytes = int64(1 << 30)
	defaultWorkerNanoCPUs    = int64(1_000_000_000)
	defaultWorkerPidsLimit   = int64(256)
	defaultWorkerStopTimeout = 15 * time.Second
)

const (
	dockerManagedLabel     = "io.mango.self-hosted-worker"
	dockerWorkIDLabel      = "io.mango.work-id"
	dockerSessionIDLabel   = "io.mango.session-id"
	dockerEnvironmentLabel = "io.mango.environment-id"
)

// DockerLauncherOptions configure a trusted host-side queue consumer. Client
// carries the Workspace credential and is used only by WorkPoller. The launcher
// passes the per-item Work secret, never that client credential, into Docker.
type DockerLauncherOptions struct {
	Client         *mango.Client
	EnvironmentID  string
	WorkerID       string
	Image          string
	SandboxBaseURL string
	RunnerPath     string
	NetworkMode    string
	User           string
	Drain          bool
	BlockMs        mango.Optional[int64]
	ReclaimAfterMs mango.Optional[int64]
	MaxIdle        time.Duration
	Logger         *slog.Logger

	MemoryBytes int64
	NanoCPUs    int64
	PidsLimit   int64
}

type dockerEngine interface {
	ContainerCreate(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error)
	ContainerAttach(context.Context, string, client.ContainerAttachOptions) (client.ContainerAttachResult, error)
	ContainerInspect(context.Context, string, client.ContainerInspectOptions) (client.ContainerInspectResult, error)
	ContainerStart(context.Context, string, client.ContainerStartOptions) (client.ContainerStartResult, error)
	ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error)
	ContainerRemove(context.Context, string, client.ContainerRemoveOptions) (client.ContainerRemoveResult, error)
	ContainerWait(context.Context, string, client.ContainerWaitOptions) client.ContainerWaitResult
	ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
}

// DockerLauncher polls one self-hosted Environment and runs each acknowledged
// item in a hardened, per-Work Docker container. A named volume is reused by
// all Work items for the same Session.
type DockerLauncher struct {
	engine dockerEngine
	opts   DockerLauncherOptions
	log    *slog.Logger
}

// NewDockerLauncher creates a launcher over an already-connected Docker Engine
// client. Callers normally pass the result of client.New(client.FromEnv) after
// a negotiated Ping; accepting the narrow interface keeps lifecycle tests
// independent of a daemon.
func NewDockerLauncher(engine dockerEngine, opts DockerLauncherOptions) (*DockerLauncher, error) {
	if engine == nil {
		return nil, errors.New("selfhosted: Docker Engine client is required")
	}
	if opts.Client == nil {
		return nil, errors.New("selfhosted: Mango supervisor client is required")
	}
	if strings.TrimSpace(opts.EnvironmentID) == "" {
		return nil, errors.New("selfhosted: environment ID is required")
	}
	if strings.TrimSpace(opts.SandboxBaseURL) == "" {
		return nil, errors.New("selfhosted: sandbox-visible Mango base URL is required")
	}
	if opts.MaxIdle < 0 || opts.MemoryBytes < 0 || opts.NanoCPUs < 0 || opts.PidsLimit < 0 {
		return nil, errors.New("selfhosted: durations and resource limits must be non-negative")
	}
	if opts.Image == "" {
		opts.Image = DefaultWorkerImage
	}
	if opts.RunnerPath == "" {
		opts.RunnerPath = defaultRunnerPath
	}
	if opts.NetworkMode == "" {
		opts.NetworkMode = "bridge"
	}
	if opts.User == "" {
		opts.User = "65532:65532"
	}
	if opts.MemoryBytes == 0 {
		opts.MemoryBytes = defaultWorkerMemoryBytes
	}
	if opts.NanoCPUs == 0 {
		opts.NanoCPUs = defaultWorkerNanoCPUs
	}
	if opts.PidsLimit == 0 {
		opts.PidsLimit = defaultWorkerPidsLimit
	}
	if _, ok := opts.BlockMs.Get(); !ok {
		opts.BlockMs = mango.Some[int64](999)
	}
	if _, ok := opts.ReclaimAfterMs.Get(); !ok {
		opts.ReclaimAfterMs = mango.Some[int64](30_000)
	}
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &DockerLauncher{engine: engine, opts: opts, log: log.With("component", "docker-self-hosted-launcher")}, nil
}

// Run owns Poll and Ack on the host and waits for each item container to exit.
// The item-side EnvironmentWorker inside the container owns heartbeat, Session
// execution, and final Stop. Individual item failures are logged and left for
// the normal Environment Work reclaim path.
func (l *DockerLauncher) Run(ctx context.Context) error {
	if _, err := l.engine.ImageInspect(ctx, l.opts.Image); err != nil {
		return fmt.Errorf("selfhosted: worker image %q is unavailable: %w", l.opts.Image, err)
	}
	poller := mango.NewWorkPoller(ctx, l.opts.Client, mango.WorkPollerOptions{
		EnvironmentID:      l.opts.EnvironmentID,
		WorkerID:           l.opts.WorkerID,
		Drain:              l.opts.Drain,
		BlockMs:            l.opts.BlockMs,
		ReclaimOlderThanMs: l.opts.ReclaimAfterMs,
	})
	defer func() { _ = poller.Close() }()
	for poller.Next() {
		work := poller.Current()
		if work == nil {
			continue
		}
		if err := l.runItem(ctx, *work); err != nil {
			if ctx.Err() != nil {
				if err == ctx.Err() {
					continue
				}
				return err
			}
			l.log.Error("Work container failed", "work_id", work.ID, "session_id", work.Data.ID, "error", err)
		}
	}
	if err := poller.Err(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}

func (l *DockerLauncher) runItem(ctx context.Context, work mango.EnvironmentWork) (runErr error) {
	if work.ID == "" || work.EnvironmentID == "" || work.Data.Type != "session" || work.Data.ID == "" || work.Secret == nil || *work.Secret == "" {
		return errors.New("selfhosted: acknowledged Work item has an invalid identity or secret")
	}
	if work.EnvironmentID != l.opts.EnvironmentID {
		return fmt.Errorf("selfhosted: Work environment %q does not match launcher environment %q", work.EnvironmentID, l.opts.EnvironmentID)
	}
	name := dockerWorkName(work.ID)
	if err := l.clearPreviousAttempt(ctx, name, work); err != nil {
		return err
	}
	createOptions := client.ContainerCreateOptions{
		Name: name,
		Config: &container.Config{
			Image:       l.opts.Image,
			Entrypoint:  []string{l.opts.RunnerPath, "run"},
			WorkingDir:  defaultSandboxWorkdir,
			User:        l.opts.User,
			Env:         l.itemEnvironment(work),
			AttachStdin: true,
			OpenStdin:   true,
			StdinOnce:   true,
			Labels: map[string]string{
				dockerManagedLabel: "true", dockerWorkIDLabel: work.ID,
				dockerSessionIDLabel: work.Data.ID, dockerEnvironmentLabel: work.EnvironmentID,
			},
		},
		HostConfig: &container.HostConfig{
			NetworkMode:    container.NetworkMode(l.opts.NetworkMode),
			ReadonlyRootfs: true,
			CapDrop:        []string{"ALL"},
			SecurityOpt:    []string{"no-new-privileges=true"},
			ExtraHosts:     []string{"host.docker.internal:host-gateway"},
			Tmpfs:          map[string]string{"/tmp": "rw,nosuid,nodev,size=64m"},
			Mounts: []mount.Mount{{
				Type: mount.TypeVolume, Source: dockerSessionVolume(work.Data.ID),
				Target: defaultSandboxWorkdir,
			}},
			Resources: container.Resources{
				Memory: l.opts.MemoryBytes, NanoCPUs: l.opts.NanoCPUs,
				PidsLimit: &l.opts.PidsLimit,
			},
		},
	}
	containerID, err := l.createContainer(ctx, name, work, createOptions)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultWorkerStopTimeout)
		defer cancel()
		_, cleanupErr := l.engine.ContainerRemove(cleanupCtx, containerID, client.ContainerRemoveOptions{Force: true})
		if cleanupErr != nil && !errdefs.IsNotFound(cleanupErr) {
			cleanupErr = fmt.Errorf("selfhosted: remove Work container %s: %w", shortContainerID(containerID), cleanupErr)
			l.log.Error("failed to remove Work container", "container_id", shortContainerID(containerID), "error", cleanupErr)
			runErr = errors.Join(runErr, cleanupErr)
		}
	}()
	secretInput, err := l.attachContainerInput(ctx, containerID)
	if err != nil {
		return err
	}
	defer secretInput.Close()
	if err := l.startContainer(ctx, containerID); err != nil {
		return err
	}
	if err := sendContainerWorkSecret(secretInput, *work.Secret); err != nil {
		return err
	}
	l.log.Info("started Work container", "work_id", work.ID, "session_id", work.Data.ID, "container_id", shortContainerID(containerID))

	wait := l.engine.ContainerWait(context.Background(), containerID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case <-ctx.Done():
		stopCtx, cancel := context.WithTimeout(context.Background(), defaultWorkerStopTimeout)
		timeoutSeconds := int(defaultWorkerStopTimeout / time.Second)
		_, stopErr := l.engine.ContainerStop(stopCtx, containerID, client.ContainerStopOptions{Timeout: &timeoutSeconds})
		cancel()
		if stopErr != nil && !errdefs.IsNotFound(stopErr) && !errdefs.IsNotModified(stopErr) {
			stopErr = fmt.Errorf("selfhosted: stop Work container %s after cancellation: %w", shortContainerID(containerID), stopErr)
			l.log.Warn("failed to stop Work container after cancellation", "container_id", shortContainerID(containerID), "error", stopErr)
			return stopErr
		}
		return nil
	case err := <-wait.Error:
		if err != nil {
			return fmt.Errorf("selfhosted: wait for Work container: %w", err)
		}
		return errors.New("selfhosted: Docker wait ended without an exit status")
	case status := <-wait.Result:
		if status.Error != nil {
			return fmt.Errorf("selfhosted: Work container wait: %s", status.Error.Message)
		}
		if status.StatusCode != 0 {
			return fmt.Errorf("selfhosted: Work container exited with status %d", status.StatusCode)
		}
		return nil
	}
}

func (l *DockerLauncher) attachContainerInput(ctx context.Context, containerID string) (*client.ContainerAttachResult, error) {
	attachCtx, cancel := context.WithTimeout(ctx, defaultWorkerStopTimeout)
	attached, err := l.engine.ContainerAttach(attachCtx, containerID, client.ContainerAttachOptions{
		Stream: true,
		Stdin:  true,
	})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("selfhosted: attach Work secret input: %w", err)
	}
	if attached.Conn == nil {
		return nil, errors.New("selfhosted: Docker Engine returned an empty Work secret input connection")
	}
	return &attached, nil
}

func sendContainerWorkSecret(attached *client.ContainerAttachResult, secret string) error {
	if err := attached.Conn.SetWriteDeadline(time.Now().Add(defaultWorkerStopTimeout)); err != nil {
		return fmt.Errorf("selfhosted: bound Work secret input: %w", err)
	}
	if err := WriteWorkSecret(attached.Conn, secret); err != nil {
		return err
	}
	if err := attached.CloseWrite(); err != nil {
		return fmt.Errorf("selfhosted: close Work secret input: %w", err)
	}
	return nil
}

func (l *DockerLauncher) createContainer(
	ctx context.Context,
	name string,
	work mango.EnvironmentWork,
	options client.ContainerCreateOptions,
) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Like Start, Create becomes ambiguous when Docker commits the operation
	// while the HTTP response races with shutdown or a transport timeout. The
	// deterministic name and complete identity labels make reconciliation safe.
	createCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultWorkerStopTimeout)
	created, createErr := l.engine.ContainerCreate(createCtx, options)
	cancel()
	containerID := strings.TrimSpace(created.ID)
	if createErr == nil {
		if containerID == "" {
			return "", errors.New("selfhosted: Docker Engine returned an empty container ID")
		}
		return containerID, nil
	}

	inspectCtx, cancel := context.WithTimeout(context.Background(), defaultWorkerStopTimeout)
	inspect, inspectErr := l.engine.ContainerInspect(inspectCtx, name, client.ContainerInspectOptions{})
	cancel()
	if inspectErr != nil {
		return "", errors.Join(
			fmt.Errorf("selfhosted: create Work container: %w", createErr),
			fmt.Errorf("selfhosted: reconcile ambiguous container create: %w", inspectErr),
		)
	}
	if err := validateWorkContainer(inspect, name, work); err != nil {
		return "", errors.Join(fmt.Errorf("selfhosted: create Work container: %w", createErr), err)
	}
	l.log.Warn("Docker reported a Create error after the labeled container appeared; continuing with reconciled lifecycle",
		"container_id", shortContainerID(inspect.Container.ID), "error", createErr)
	return strings.TrimSpace(inspect.Container.ID), nil
}

func (l *DockerLauncher) startContainer(ctx context.Context, containerID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Once Create commits, finish or reconcile Start independently from host
	// shutdown. Otherwise Docker may accept Start while the cancelled HTTP call
	// returns an error, and the launcher could skip the sandbox's graceful
	// cancellation-result/Stop sequence.
	startCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), defaultWorkerStopTimeout)
	_, startErr := l.engine.ContainerStart(startCtx, containerID, client.ContainerStartOptions{})
	cancel()
	if startErr == nil {
		return nil
	}
	inspectCtx, cancel := context.WithTimeout(context.Background(), defaultWorkerStopTimeout)
	inspect, inspectErr := l.engine.ContainerInspect(inspectCtx, containerID, client.ContainerInspectOptions{})
	cancel()
	if inspectErr != nil {
		return errors.Join(
			fmt.Errorf("selfhosted: start Work container: %w", startErr),
			fmt.Errorf("selfhosted: reconcile ambiguous container start: %w", inspectErr),
		)
	}
	if inspect.Container.State == nil || inspect.Container.State.Status == "" || inspect.Container.State.Status == container.StateCreated {
		return fmt.Errorf("selfhosted: start Work container: %w", startErr)
	}
	l.log.Warn("Docker reported a Start error after the container left created state; continuing with reconciled lifecycle",
		"container_id", shortContainerID(containerID), "state", inspect.Container.State.Status, "error", startErr)
	return nil
}

func (l *DockerLauncher) itemEnvironment(work mango.EnvironmentWork) []string {
	values := []string{
		"MANGO_SANDBOXED=1",
		"MANGO_BASE_URL=" + l.opts.SandboxBaseURL,
		"MANGO_WORK_ID=" + work.ID,
		"MANGO_ENVIRONMENT_ID=" + work.EnvironmentID,
		"MANGO_SESSION_ID=" + work.Data.ID,
		"MANGO_WORKDIR=" + defaultSandboxWorkdir,
	}
	values = append(values, "MANGO_WORKER_MAX_IDLE="+l.opts.MaxIdle.String())
	return values
}

func (l *DockerLauncher) clearPreviousAttempt(ctx context.Context, name string, work mango.EnvironmentWork) error {
	inspect, err := l.engine.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("selfhosted: inspect previous Work container: %w", err)
	}
	if err := validateWorkContainer(inspect, name, work); err != nil {
		return err
	}
	var stopErr error
	if inspect.Container.State != nil && inspect.Container.State.Running {
		stopCtx, cancel := context.WithTimeout(ctx, defaultWorkerStopTimeout)
		timeoutSeconds := int(defaultWorkerStopTimeout / time.Second)
		_, stopErr = l.engine.ContainerStop(stopCtx, inspect.Container.ID, client.ContainerStopOptions{Timeout: &timeoutSeconds})
		cancel()
		if errdefs.IsNotFound(stopErr) {
			stopErr = nil
		}
	}
	removeCtx, cancel := context.WithTimeout(ctx, defaultWorkerStopTimeout)
	_, err = l.engine.ContainerRemove(removeCtx, inspect.Container.ID, client.ContainerRemoveOptions{Force: true})
	cancel()
	if err != nil && !errdefs.IsNotFound(err) {
		removeErr := fmt.Errorf("selfhosted: remove previous Work container: %w", err)
		if stopErr != nil {
			stopErr = fmt.Errorf("selfhosted: stop previous Work container: %w", stopErr)
		}
		return errors.Join(stopErr, removeErr)
	}
	if stopErr != nil {
		l.log.Warn("force-removed previous Work container after graceful stop failed", "container_id", shortContainerID(inspect.Container.ID), "error", stopErr)
	}
	return nil
}

func validateWorkContainer(inspect client.ContainerInspectResult, name string, work mango.EnvironmentWork) error {
	if inspect.Container.Config == nil || strings.TrimSpace(inspect.Container.ID) == "" {
		return fmt.Errorf("selfhosted: refusing to replace malformed container %q", name)
	}
	labels := inspect.Container.Config.Labels
	if labels[dockerManagedLabel] != "true" || labels[dockerWorkIDLabel] != work.ID ||
		labels[dockerSessionIDLabel] != work.Data.ID || labels[dockerEnvironmentLabel] != work.EnvironmentID {
		return fmt.Errorf("selfhosted: refusing to replace unrelated container %q", name)
	}
	return nil
}

func dockerWorkName(workID string) string {
	return "mango-work-" + shortHash(workID)
}

func dockerSessionVolume(sessionID string) string {
	return "mango-workspace-" + shortHash(sessionID)
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return fmt.Sprintf("%x", sum[:12])
}

func shortContainerID(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}
