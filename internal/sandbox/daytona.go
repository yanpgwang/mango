package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/daytona/clients/sdk-go/pkg/daytona"
	daytonaerrors "github.com/daytona/clients/sdk-go/pkg/errors"
	daytonaoptions "github.com/daytona/clients/sdk-go/pkg/options"
	daytonatypes "github.com/daytona/clients/sdk-go/pkg/types"
)

const (
	DaytonaProviderName = "daytona"
	defaultDaytonaImage = "python:3.12-slim"
	defaultDaytonaRoot  = "/home/daytona"
)

type DaytonaConfig struct {
	APIURL           string
	APIKey           string
	Target           string
	Snapshot         string
	Image            string
	AutoPauseMinutes int
	HTTPClient       *http.Client
}

type daytonaResource struct {
	id     string
	labels map[string]string
	remote daytonaRemote
}

type daytonaRemote interface {
	ID() string
	Exec(context.Context, string, string, time.Duration) (string, string, int, error)
	ReadFile(context.Context, string) ([]byte, error)
	WriteFile(context.Context, string, []byte) error
	Start(context.Context) error
	Destroy(context.Context) error
	remoteFileResourceDataPlane
}

type daytonaService interface {
	Get(context.Context, string) (daytonaResource, error)
	Create(context.Context, string, string, Spec) (daytonaResource, error)
}

type daytonaProvider struct {
	service daytonaService
	root    string
}

func NewDaytonaProvider(cfg DaytonaConfig) (Provider, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, errors.New(
			"sandbox: DAYTONA_API_KEY is required for the daytona provider",
		)
	}
	autoPause := cfg.AutoPauseMinutes
	if autoPause <= 0 {
		autoPause = 15
	}
	image := strings.TrimSpace(cfg.Image)
	if image == "" {
		image = defaultDaytonaImage
	}
	client, err := daytona.NewClientWithConfig(&daytonatypes.DaytonaConfig{
		APIKey:      apiKey,
		APIUrl:      strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/"),
		Target:      strings.TrimSpace(cfg.Target),
		OtelEnabled: false,
		HTTPClient:  cfg.HTTPClient,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox: initialize Daytona client: %w", err)
	}
	return &daytonaProvider{
		service: &daytonaSDKService{
			client:           client,
			snapshot:         strings.TrimSpace(cfg.Snapshot),
			image:            image,
			autoPauseMinutes: autoPause,
		},
		root: defaultDaytonaRoot,
	}, nil
}

func newDaytonaProvider(service daytonaService, root string) Provider {
	if root == "" {
		root = defaultDaytonaRoot
	}
	return &daytonaProvider{service: service, root: root}
}

func (p *daytonaProvider) Name() string { return DaytonaProviderName }

func (*daytonaProvider) SupportsPackageSetup() bool { return true }

func (*daytonaProvider) SupportsFileResources() bool { return true }

func (*daytonaProvider) SupportsSessionOutputs() bool { return true }

func (*daytonaProvider) SupportsSkillBundles() bool { return true }

func (*daytonaProvider) SupportsGitRepositories() bool { return true }

func (p *daytonaProvider) Create(
	ctx context.Context,
	sessionKey string,
	spec Spec,
) (Ref, Sandbox, error) {
	if sessionKey == "" {
		return Ref{}, nil, errors.New("sandbox: session key is required")
	}
	if err := ctx.Err(); err != nil {
		return Ref{}, nil, err
	}
	name := deterministicRemoteName(p.Name(), sessionKey)
	resource, err := p.service.Get(ctx, name)
	if err == nil {
		return p.attachResource(ctx, sessionKey, resource, spec)
	}
	if !isDaytonaNotFound(err) {
		return Ref{}, nil, fmt.Errorf("sandbox: daytona get by name: %w", err)
	}
	resource, err = p.service.Create(ctx, name, sessionKey, spec)
	if err != nil {
		// Daytona names are unique. A conflict or lost response means the
		// deterministic name is the recovery key.
		resource, getErr := p.service.Get(ctx, name)
		if getErr == nil {
			return p.attachResource(ctx, sessionKey, resource, spec)
		}
		return Ref{}, nil, fmt.Errorf("sandbox: daytona create: %w", err)
	}
	return p.attachResource(ctx, sessionKey, resource, spec)
}

func (p *daytonaProvider) Attach(
	ctx context.Context,
	sessionKey string,
	ref Ref,
	spec Spec,
) (Sandbox, error) {
	if err := validateRemoteReference(p.Name(), sessionKey, ref); err != nil {
		return nil, err
	}
	resource, err := p.service.Get(ctx, ref.ID)
	if err != nil {
		if isDaytonaNotFound(err) {
			return nil, fmt.Errorf("%w: daytona sandbox %q", ErrNotFound, ref.ID)
		}
		return nil, fmt.Errorf("sandbox: daytona get: %w", err)
	}
	if err := validateRemoteOwnership(
		p.Name(),
		resource.id,
		sessionKey,
		resource.labels,
	); err != nil {
		return nil, err
	}
	if err := resource.remote.Start(ctx); err != nil {
		if isDaytonaNotFound(err) {
			return nil, fmt.Errorf("%w: daytona sandbox %q", ErrNotFound, ref.ID)
		}
		return nil, fmt.Errorf("sandbox: daytona start: %w", err)
	}
	box := newDaytonaBox(resource.remote, p.root, spec.Timeout)
	if err := box.ensureRoot(ctx); err != nil {
		return nil, err
	}
	return box, nil
}

func (p *daytonaProvider) attachResource(
	ctx context.Context,
	sessionKey string,
	resource daytonaResource,
	spec Spec,
) (Ref, Sandbox, error) {
	ref := Ref{Provider: p.Name(), ID: resource.id}
	box, err := p.Attach(ctx, sessionKey, ref, spec)
	return ref, box, err
}

type daytonaBox struct {
	remote       daytonaRemote
	root         string
	timeout      time.Duration
	resources    *remoteFileResources
	skills       *remoteSkillBundles
	repositories *commandGitRepositories
	sync         remoteResourceSynchronization
}

func newDaytonaBox(
	remote daytonaRemote,
	root string,
	timeout time.Duration,
) *daytonaBox {
	box := &daytonaBox{
		remote: remote, root: root, timeout: timeout,
		resources: newRemoteFileResources(DaytonaProviderName, remote),
	}
	box.skills = newRemoteSkillBundles(
		DaytonaProviderName,
		box.resources,
		func(ctx context.Context, command Command) (*Result, error) {
			return box.exec(
				ctx, command, remoteOperationCommandTimeout(ctx, box.timeout),
			)
		},
	)
	box.repositories = newRemoteGitRepositories(
		DaytonaProviderName, box.resources,
		func(ctx context.Context, command Command) (*Result, error) {
			return box.exec(ctx, command, remoteOperationCommandTimeout(ctx, box.timeout))
		},
	)
	return box
}

func (s *daytonaBox) Root() string { return s.root }

func (s *daytonaBox) ensureRoot(ctx context.Context) error {
	if err := s.resources.ensureDirectory(ctx, s.root, 0o755); err != nil {
		return err
	}
	return s.resources.ensureSessionOutputLayout(ctx)
}

func (s *daytonaBox) Exec(
	ctx context.Context,
	cmd Command,
) (*Result, error) {
	return s.exec(ctx, cmd, s.timeout)
}

func (s *daytonaBox) exec(
	ctx context.Context,
	cmd Command,
	timeout time.Duration,
) (*Result, error) {
	if cmd.Path == "" {
		return nil, errors.New("sandbox: command path is required")
	}
	runCtx, cancel := commandTimeout(ctx, timeout)
	defer cancel()
	stdout, stderr, code, err := s.remote.Exec(
		runCtx,
		remoteCommandLine(cmd),
		s.root,
		timeout,
	)
	if err != nil {
		if runCtx.Err() != nil {
			return remoteResult(runCtx, timeout, stdout, stderr, code)
		}
		return nil, fmt.Errorf("sandbox: daytona exec: %w", err)
	}
	return remoteResult(runCtx, timeout, stdout, stderr, code)
}

func (s *daytonaBox) ReadFile(
	ctx context.Context,
	value string,
) ([]byte, error) {
	full, err := remoteToolPath(
		s.root, value, SessionUploadsRoot, SessionOutputsRoot, SessionSkillsRoot,
		SessionRepositoryRoot,
	)
	if err != nil {
		return nil, err
	}
	return s.remote.ReadFile(ctx, full)
}

func (s *daytonaBox) ReadFileBounded(
	ctx context.Context,
	value string,
	maxBytes int64,
) ([]byte, bool, error) {
	full, err := remoteToolPath(
		s.root, value, SessionUploadsRoot, SessionOutputsRoot, SessionSkillsRoot,
		SessionRepositoryRoot,
	)
	if err != nil {
		return nil, false, err
	}
	return readFileBoundedByCommand(ctx, s, full, maxBytes)
}

func (s *daytonaBox) WriteFile(
	ctx context.Context,
	value string,
	data []byte,
) error {
	full, err := remoteWritableToolPath(
		s.root, value, SessionUploadsRoot, SessionOutputsRoot, SessionRepositoryRoot,
	)
	if err != nil {
		return err
	}
	if err := s.resources.ensureDirectory(ctx, path.Dir(full), 0o755); err != nil {
		return fmt.Errorf("sandbox: daytona create parent: %w", err)
	}
	return s.remote.WriteFile(ctx, full, data)
}

func (s *daytonaBox) HasFileResource(
	ctx context.Context,
	mount FileResourceMount,
) (bool, error) {
	return s.resources.HasFileResource(ctx, mount)
}

func (s *daytonaBox) ImportFileResource(
	ctx context.Context,
	mount FileResourceMount,
	content io.Reader,
) error {
	return s.resources.ImportFileResource(ctx, mount, content)
}

func (s *daytonaBox) RemoveFileResource(
	ctx context.Context,
	runtimePath string,
	identity string,
) error {
	return s.resources.RemoveFileResource(ctx, runtimePath, identity)
}

func (s *daytonaBox) HasReadOnlySkill(
	ctx context.Context,
	mount ReadOnlySkillMount,
) (bool, error) {
	return s.skills.HasReadOnlySkill(ctx, mount)
}

func (s *daytonaBox) ImportReadOnlySkill(
	ctx context.Context,
	mount ReadOnlySkillMount,
	content io.Reader,
) error {
	return s.skills.ImportReadOnlySkill(ctx, mount, content)
}

func (s *daytonaBox) HasGitRepository(ctx context.Context, mount GitRepositoryMount) (bool, error) {
	return s.repositories.HasGitRepository(ctx, mount)
}

func (s *daytonaBox) ImportGitRepository(ctx context.Context, mount GitRepositoryMount, content io.Reader) error {
	return s.repositories.ImportGitRepository(ctx, mount, content)
}

func (s *daytonaBox) RemoveGitRepository(ctx context.Context, runtimePath, identity string) error {
	return s.repositories.RemoveGitRepository(ctx, runtimePath, identity)
}

func (s *daytonaBox) OpenSessionOutputs(ctx context.Context) (io.ReadCloser, error) {
	return openRemoteSessionOutputs(
		ctx,
		DaytonaProviderName,
		s.resources,
		func(executeCtx context.Context, command Command) (*Result, error) {
			return s.exec(
				executeCtx,
				command,
				remoteOperationCommandTimeout(executeCtx, s.timeout),
			)
		},
	)
}

func (s *daytonaBox) LockResourceOperation(ctx context.Context) (func(), error) {
	return s.sync.LockResourceOperation(ctx)
}

func (s *daytonaBox) TryLockResourceSync(
	ctx context.Context,
) (context.Context, func(), bool, error) {
	return s.sync.TryLockResourceSync(ctx)
}

func (s *daytonaBox) LockResourceSync(
	ctx context.Context,
) (context.Context, func(), error) {
	return s.sync.LockResourceSync(ctx)
}

func (s *daytonaBox) Destroy(ctx context.Context) error {
	err := s.remote.Destroy(ctx)
	if isDaytonaNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sandbox: daytona destroy: %w", err)
	}
	return nil
}

var _ SkillBundleSandbox = (*daytonaBox)(nil)

type daytonaSDKService struct {
	client           *daytona.Client
	snapshot         string
	image            string
	autoPauseMinutes int
}

func (s *daytonaSDKService) Get(
	ctx context.Context,
	idOrName string,
) (daytonaResource, error) {
	box, err := s.client.Get(ctx, idOrName)
	if err != nil {
		return daytonaResource{}, err
	}
	return daytonaResource{
		id:     box.ID,
		labels: box.Labels,
		remote: &daytonaSDKRemote{sandbox: box},
	}, nil
}

func (s *daytonaSDKService) Create(
	ctx context.Context,
	name string,
	sessionKey string,
	spec Spec,
) (daytonaResource, error) {
	autoPause := s.autoPauseMinutes
	neverDelete := -1
	noTTL := 0
	base := daytonatypes.SandboxBaseParams{
		Name:               name,
		Labels:             remoteMetadata(sessionKey),
		AutoPauseInterval:  &autoPause,
		AutoDeleteInterval: &neverDelete,
		TtlMinutes:         &noTTL,
		NetworkBlockAll:    spec.Network == "" || spec.Network == "none",
	}
	var params any
	if s.snapshot != "" && spec.Image == "" {
		params = daytonatypes.SnapshotParams{
			SandboxBaseParams: base,
			Snapshot:          s.snapshot,
		}
	} else {
		image := s.image
		if spec.Image != "" {
			image = spec.Image
		}
		params = daytonatypes.ImageParams{
			SandboxBaseParams: base,
			Image:             image,
			Resources:         daytonaResources(spec),
		}
	}
	box, err := s.client.Create(ctx, params)
	if err != nil {
		return daytonaResource{}, err
	}
	return daytonaResource{
		id:     box.ID,
		labels: box.Labels,
		remote: &daytonaSDKRemote{sandbox: box},
	}, nil
}

type daytonaSDKRemote struct {
	sandbox *daytona.Sandbox
}

func (s *daytonaSDKRemote) ID() string { return s.sandbox.ID }

func (s *daytonaSDKRemote) Exec(
	ctx context.Context,
	command string,
	cwd string,
	timeout time.Duration,
) (string, string, int, error) {
	options := []func(*daytonaoptions.ExecuteCommand){
		daytonaoptions.WithCwd(cwd),
	}
	if timeout > 0 {
		options = append(options, daytonaoptions.WithExecuteTimeout(timeout))
	}
	result, err := s.sandbox.Process.ExecuteCommand(ctx, command, options...)
	if err != nil {
		return "", "", -1, err
	}
	return result.Result, "", result.ExitCode, nil
}

func (s *daytonaSDKRemote) ReadFile(
	ctx context.Context,
	value string,
) (data []byte, err error) {
	reader, err := s.sandbox.FileSystem.DownloadFileStream(ctx, value)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := reader.Close(); err == nil {
			err = closeErr
		}
	}()
	return io.ReadAll(reader)
}

func (s *daytonaSDKRemote) WriteFile(
	ctx context.Context,
	value string,
	data []byte,
) error {
	return s.sandbox.FileSystem.UploadFileStream(
		ctx,
		bytes.NewReader(data),
		value,
	)
}

func (s *daytonaSDKRemote) ResourceCreateDirectory(
	ctx context.Context,
	directory string,
	permissions remoteFilePermissions,
) error {
	return s.sandbox.FileSystem.CreateFolder(
		ctx, directory, daytonaoptions.WithMode(remotePermissionDigits(permissions.Mode)),
	)
}

func (s *daytonaSDKRemote) ResourceUpload(
	ctx context.Context,
	filePath string,
	content io.Reader,
	_ remoteFilePermissions,
) error {
	return s.sandbox.FileSystem.UploadFileStream(ctx, content, filePath)
}

func (s *daytonaSDKRemote) ResourceOpen(
	ctx context.Context,
	filePath string,
) (io.ReadCloser, error) {
	return s.sandbox.FileSystem.DownloadFileStream(ctx, filePath)
}

func (s *daytonaSDKRemote) ResourceStat(
	ctx context.Context,
	filePath string,
) (remoteFileInfo, error) {
	item, err := s.sandbox.FileSystem.GetFileInfo(ctx, filePath)
	if err != nil {
		return remoteFileInfo{}, err
	}
	return remoteFileInfo{
		SizeBytes: item.Size,
		Regular:   !item.IsDirectory,
		Directory: item.IsDirectory,
	}, nil
}

func (s *daytonaSDKRemote) ResourceRemoveFile(
	ctx context.Context,
	filePath string,
) error {
	return s.sandbox.FileSystem.DeleteFile(ctx, filePath, false)
}

func (*daytonaSDKRemote) ResourceIsNotFound(err error) bool {
	return isDaytonaNotFound(err)
}

func (s *daytonaSDKRemote) Start(ctx context.Context) error {
	return s.sandbox.Start(ctx)
}

func (s *daytonaSDKRemote) Destroy(ctx context.Context) error {
	return s.sandbox.DeleteAndWait(ctx, time.Minute)
}

func daytonaResources(spec Spec) *daytonatypes.Resources {
	resources := &daytonatypes.Resources{}
	hasResource := false
	if spec.CPUs != "" {
		if cpu := parseWholeCPU(spec.CPUs); cpu > 0 {
			resources.CPU = cpu
			hasResource = true
		}
	}
	if memory := parseMemoryMB(spec.Memory); memory > 0 {
		resources.Memory = memory
		hasResource = true
	}
	if !hasResource {
		return nil
	}
	return resources
}

func parseWholeCPU(value string) int {
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
	if err != nil || parsed <= 0 || parsed != math.Trunc(parsed) {
		return 0
	}
	return int(parsed)
}

func parseMemoryMB(value string) int {
	normalized := strings.ToLower(strings.TrimSpace(value))
	for _, suffix := range []string{"mib", "mb", "mi", "m"} {
		if strings.HasSuffix(normalized, suffix) {
			normalized = strings.TrimSpace(strings.TrimSuffix(normalized, suffix))
			break
		}
	}
	parsed, err := strconv.Atoi(normalized)
	if err != nil || parsed <= 0 {
		return 0
	}
	return parsed
}

func isDaytonaNotFound(err error) bool {
	return errors.Is(err, daytonaerrors.ErrNotFound)
}
