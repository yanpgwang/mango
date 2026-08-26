package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	opensandbox "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

const (
	OpenSandboxProviderName = "opensandbox"
	defaultOpenSandboxImage = "python:3.12-slim"
)

type OpenSandboxConfig struct {
	BaseURL    string
	APIKey     string
	Image      string
	UseProxy   bool
	HTTPClient *http.Client
}

type openSandboxResource struct {
	id       string
	metadata map[string]string
}

type openSandboxRemote interface {
	ID() string
	Exec(context.Context, string, string, time.Duration) (string, string, int, error)
	ReadFile(context.Context, string) ([]byte, error)
	WriteFile(context.Context, string, []byte) error
	ApplyLimitedNetwork(context.Context, []string) error
	Destroy(context.Context) error
	remoteFileResourceDataPlane
}

type openSandboxService interface {
	List(context.Context, map[string]string) ([]openSandboxResource, error)
	Get(context.Context, string) (openSandboxResource, error)
	Create(context.Context, string, Spec) (openSandboxRemote, error)
	Connect(context.Context, string) (openSandboxRemote, error)
}

type openSandboxProvider struct {
	service openSandboxService
	root    string
}

func NewOpenSandboxProvider(cfg OpenSandboxConfig) (Provider, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		return nil, errors.New(
			"sandbox: OPEN_SANDBOX_DOMAIN is required for the opensandbox provider",
		)
	}
	image := strings.TrimSpace(cfg.Image)
	if image == "" {
		image = defaultOpenSandboxImage
	}
	retry := opensandbox.DefaultRetryConfig()
	connection := opensandbox.ConnectionConfig{
		Domain:         baseURL,
		APIKey:         strings.TrimSpace(cfg.APIKey),
		UseServerProxy: cfg.UseProxy,
		RequestTimeout: remoteDefaultPeriod,
		HTTPClient:     cfg.HTTPClient,
		Retry:          &retry,
		DisableMetrics: true,
	}
	return &openSandboxProvider{
		service: &openSandboxSDKService{
			config:  connection,
			manager: opensandbox.NewSandboxManager(connection),
			image:   image,
		},
		root: remoteDefaultRoot,
	}, nil
}

func newOpenSandboxProvider(service openSandboxService, root string) Provider {
	if root == "" {
		root = remoteDefaultRoot
	}
	return &openSandboxProvider{service: service, root: root}
}

func (p *openSandboxProvider) Name() string { return OpenSandboxProviderName }

func (*openSandboxProvider) SupportsPackageSetup() bool { return true }

func (*openSandboxProvider) SupportsLimitedNetwork() bool { return true }

func (*openSandboxProvider) SupportsFileResources() bool { return true }

func (*openSandboxProvider) SupportsSessionOutputs() bool { return true }

func (*openSandboxProvider) SupportsSkillBundles() bool { return true }

func (*openSandboxProvider) SupportsGitRepositories() bool { return true }

func (p *openSandboxProvider) Create(
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
	existing, err := p.service.List(ctx, remoteMetadata(sessionKey))
	if err != nil {
		return Ref{}, nil, fmt.Errorf("sandbox: opensandbox list: %w", err)
	}
	sort.Slice(existing, func(i, j int) bool { return existing[i].id < existing[j].id })
	if len(existing) > 0 {
		return p.attachResource(ctx, sessionKey, existing[0], spec)
	}
	remote, err := p.service.Create(ctx, sessionKey, spec)
	if err != nil {
		existing, findErr := p.service.List(ctx, remoteMetadata(sessionKey))
		if findErr == nil && len(existing) > 0 {
			sort.Slice(existing, func(i, j int) bool {
				return existing[i].id < existing[j].id
			})
			return p.attachResource(ctx, sessionKey, existing[0], spec)
		}
		return Ref{}, nil, fmt.Errorf("sandbox: opensandbox create: %w", err)
	}
	box := newOpenSandboxBox(remote, p.root, spec.Timeout)
	if err := box.ensureRoot(ctx); err != nil {
		_ = remote.Destroy(context.Background())
		return Ref{}, nil, err
	}
	return Ref{Provider: p.Name(), ID: remote.ID()}, box, nil
}

func (p *openSandboxProvider) Attach(
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
		if isOpenSandboxNotFound(err) {
			return nil, fmt.Errorf("%w: opensandbox sandbox %q", ErrNotFound, ref.ID)
		}
		return nil, fmt.Errorf("sandbox: opensandbox get: %w", err)
	}
	if err := validateRemoteOwnership(
		p.Name(),
		resource.id,
		sessionKey,
		resource.metadata,
	); err != nil {
		return nil, err
	}
	remote, err := p.service.Connect(ctx, ref.ID)
	if err != nil {
		if isOpenSandboxNotFound(err) {
			return nil, fmt.Errorf("%w: opensandbox sandbox %q", ErrNotFound, ref.ID)
		}
		return nil, fmt.Errorf("sandbox: opensandbox connect: %w", err)
	}
	box := newOpenSandboxBox(remote, p.root, spec.Timeout)
	if err := box.ensureRoot(ctx); err != nil {
		return nil, err
	}
	return box, nil
}

func (p *openSandboxProvider) attachResource(
	ctx context.Context,
	sessionKey string,
	resource openSandboxResource,
	spec Spec,
) (Ref, Sandbox, error) {
	ref := Ref{Provider: p.Name(), ID: resource.id}
	box, err := p.Attach(ctx, sessionKey, ref, spec)
	return ref, box, err
}

type openSandboxBox struct {
	remote       openSandboxRemote
	root         string
	timeout      time.Duration
	resources    *remoteFileResources
	skills       *remoteSkillBundles
	repositories *commandGitRepositories
	sync         remoteResourceSynchronization
}

func newOpenSandboxBox(
	remote openSandboxRemote,
	root string,
	timeout time.Duration,
) *openSandboxBox {
	box := &openSandboxBox{
		remote: remote, root: root, timeout: timeout,
		resources: newRemoteFileResources(OpenSandboxProviderName, remote),
	}
	box.skills = newRemoteSkillBundles(
		OpenSandboxProviderName,
		box.resources,
		func(ctx context.Context, command Command) (*Result, error) {
			return box.exec(
				ctx, command, remoteOperationCommandTimeout(ctx, box.timeout),
			)
		},
	)
	box.repositories = newRemoteGitRepositories(
		OpenSandboxProviderName, box.resources,
		func(ctx context.Context, command Command) (*Result, error) {
			return box.exec(ctx, command, remoteOperationCommandTimeout(ctx, box.timeout))
		},
	)
	return box
}

func (s *openSandboxBox) Root() string { return s.root }

func (s *openSandboxBox) ApplyLimitedNetwork(
	ctx context.Context,
	allowedHosts []string,
) error {
	return s.remote.ApplyLimitedNetwork(ctx, allowedHosts)
}

func (s *openSandboxBox) ensureRoot(ctx context.Context) error {
	if err := s.resources.ensureDirectory(ctx, s.root, 0o777); err != nil {
		return err
	}
	return s.resources.ensureSessionOutputLayout(ctx)
}

func (s *openSandboxBox) Exec(
	ctx context.Context,
	cmd Command,
) (*Result, error) {
	return s.exec(ctx, cmd, s.timeout)
}

func (s *openSandboxBox) exec(
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
		return nil, fmt.Errorf("sandbox: opensandbox exec: %w", err)
	}
	return remoteResult(runCtx, timeout, stdout, stderr, code)
}

func (s *openSandboxBox) ReadFile(
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

func (s *openSandboxBox) ReadFileBounded(
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

func (s *openSandboxBox) WriteFile(
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
	if err := s.resources.ensureDirectory(ctx, path.Dir(full), 0o777); err != nil {
		return fmt.Errorf("sandbox: opensandbox create parent: %w", err)
	}
	return s.remote.WriteFile(ctx, full, data)
}

func (s *openSandboxBox) HasFileResource(
	ctx context.Context,
	mount FileResourceMount,
) (bool, error) {
	return s.resources.HasFileResource(ctx, mount)
}

func (s *openSandboxBox) ImportFileResource(
	ctx context.Context,
	mount FileResourceMount,
	content io.Reader,
) error {
	return s.resources.ImportFileResource(ctx, mount, content)
}

func (s *openSandboxBox) RemoveFileResource(
	ctx context.Context,
	runtimePath string,
	identity string,
) error {
	return s.resources.RemoveFileResource(ctx, runtimePath, identity)
}

func (s *openSandboxBox) HasReadOnlySkill(
	ctx context.Context,
	mount ReadOnlySkillMount,
) (bool, error) {
	return s.skills.HasReadOnlySkill(ctx, mount)
}

func (s *openSandboxBox) ImportReadOnlySkill(
	ctx context.Context,
	mount ReadOnlySkillMount,
	content io.Reader,
) error {
	return s.skills.ImportReadOnlySkill(ctx, mount, content)
}

func (s *openSandboxBox) HasGitRepository(ctx context.Context, mount GitRepositoryMount) (bool, error) {
	return s.repositories.HasGitRepository(ctx, mount)
}

func (s *openSandboxBox) ImportGitRepository(ctx context.Context, mount GitRepositoryMount, content io.Reader) error {
	return s.repositories.ImportGitRepository(ctx, mount, content)
}

func (s *openSandboxBox) RemoveGitRepository(ctx context.Context, runtimePath, identity string) error {
	return s.repositories.RemoveGitRepository(ctx, runtimePath, identity)
}

func (s *openSandboxBox) OpenSessionOutputs(ctx context.Context) (io.ReadCloser, error) {
	return openRemoteSessionOutputs(
		ctx,
		OpenSandboxProviderName,
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

func (s *openSandboxBox) LockResourceOperation(ctx context.Context) (func(), error) {
	return s.sync.LockResourceOperation(ctx)
}

func (s *openSandboxBox) TryLockResourceSync(
	ctx context.Context,
) (context.Context, func(), bool, error) {
	return s.sync.TryLockResourceSync(ctx)
}

func (s *openSandboxBox) LockResourceSync(
	ctx context.Context,
) (context.Context, func(), error) {
	return s.sync.LockResourceSync(ctx)
}

func (s *openSandboxBox) Destroy(ctx context.Context) error {
	err := s.remote.Destroy(ctx)
	if isOpenSandboxNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sandbox: opensandbox destroy: %w", err)
	}
	return nil
}

var _ SkillBundleSandbox = (*openSandboxBox)(nil)

type openSandboxSDKService struct {
	config  opensandbox.ConnectionConfig
	manager *opensandbox.SandboxManager
	image   string
}

func (s *openSandboxSDKService) List(
	ctx context.Context,
	metadata map[string]string,
) ([]openSandboxResource, error) {
	const pageSize = 100
	var resources []openSandboxResource
	for page := 1; ; page++ {
		response, err := s.manager.ListSandboxInfos(ctx, opensandbox.ListOptions{
			Metadata: metadata,
			Page:     page,
			PageSize: pageSize,
		})
		if err != nil {
			return nil, err
		}
		for _, item := range response.Items {
			resources = append(resources, openSandboxResource{
				id:       item.ID,
				metadata: item.Metadata,
			})
		}
		if !response.Pagination.HasNextPage {
			return resources, nil
		}
		if len(response.Items) == 0 ||
			(response.Pagination.Page > 0 &&
				response.Pagination.Page != page) ||
			(response.Pagination.TotalPages > 0 &&
				page >= response.Pagination.TotalPages) {
			return nil, errors.New(
				"sandbox: opensandbox returned non-advancing pagination",
			)
		}
	}
}

func (s *openSandboxSDKService) Get(
	ctx context.Context,
	id string,
) (openSandboxResource, error) {
	info, err := s.manager.GetSandboxInfo(ctx, id)
	if err != nil {
		return openSandboxResource{}, err
	}
	return openSandboxResource{id: info.ID, metadata: info.Metadata}, nil
}

func (s *openSandboxSDKService) Create(
	ctx context.Context,
	sessionKey string,
	spec Spec,
) (openSandboxRemote, error) {
	image := s.image
	if spec.Image != "" {
		image = spec.Image
	}
	var limits opensandbox.ResourceLimits
	if spec.CPUs != "" {
		limits = opensandbox.ResourceLimits{}
		limits["cpu"] = spec.CPUs
	}
	if spec.Memory != "" {
		if limits == nil {
			limits = opensandbox.ResourceLimits{}
		}
		limits["memory"] = normalizeKubernetesMemory(spec.Memory)
	}
	policy := openSandboxNetworkPolicy(spec)
	created, err := opensandbox.CreateSandbox(
		ctx,
		s.config,
		opensandbox.SandboxCreateOptions{
			Image:          image,
			ResourceLimits: limits,
			Metadata:       remoteMetadata(sessionKey),
			ManualCleanup:  true,
			NetworkPolicy:  policy,
		},
	)
	if err != nil {
		return nil, err
	}
	return &openSandboxSDKRemote{sandbox: created}, nil
}

func openSandboxNetworkPolicy(spec Spec) *opensandbox.NetworkPolicy {
	switch spec.Network {
	case "limited":
		rules := make([]opensandbox.NetworkRule, 0, len(spec.NetworkAllowedHosts))
		for _, host := range spec.NetworkAllowedHosts {
			rules = append(rules, opensandbox.NetworkRule{Action: "allow", Target: host})
		}
		return &opensandbox.NetworkPolicy{DefaultAction: "deny", Egress: rules}
	case "", "none":
		return &opensandbox.NetworkPolicy{DefaultAction: "deny"}
	default:
		return nil
	}
}

func (s *openSandboxSDKService) Connect(
	ctx context.Context,
	id string,
) (openSandboxRemote, error) {
	connected, err := opensandbox.ConnectSandbox(ctx, s.config, id)
	if err != nil {
		return nil, err
	}
	return &openSandboxSDKRemote{sandbox: connected}, nil
}

type openSandboxSDKRemote struct {
	sandbox *opensandbox.Sandbox
}

func (s *openSandboxSDKRemote) ID() string { return s.sandbox.ID() }

func (s *openSandboxSDKRemote) Exec(
	ctx context.Context,
	command string,
	cwd string,
	timeout time.Duration,
) (string, string, int, error) {
	request := opensandbox.RunCommandRequest{
		Command: command,
		Cwd:     cwd,
	}
	if timeout > 0 {
		request.Timeout = timeout.Milliseconds()
	}
	execution, err := s.sandbox.RunCommandWithOpts(
		ctx,
		request,
		nil,
	)
	if err != nil {
		return "", "", -1, err
	}
	exitCode := 0
	if execution.ExitCode != nil {
		exitCode = *execution.ExitCode
	}
	stdout := joinOpenSandboxOutput(execution.Stdout)
	stderr := joinOpenSandboxOutput(execution.Stderr)
	return stdout, stderr, exitCode, nil
}

func (s *openSandboxSDKRemote) ReadFile(
	ctx context.Context,
	value string,
) (data []byte, err error) {
	reader, err := s.sandbox.DownloadFile(ctx, value, "")
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

func (s *openSandboxSDKRemote) WriteFile(
	ctx context.Context,
	value string,
	data []byte,
) error {
	return s.sandbox.UploadFile(
		ctx,
		bytes.NewReader(data),
		opensandbox.UploadFileOptions{
			FileName: path.Base(value),
			Metadata: opensandbox.FileMetadata{
				Path: value,
				// OpenSandbox's wire API uses chmod-style digits (600), not
				// Go's os.FileMode integer representation (0o600 == 384).
				Mode: 600,
			},
		},
	)
}

func (s *openSandboxSDKRemote) ResourceCreateDirectory(
	ctx context.Context,
	directory string,
	permissions remoteFilePermissions,
) error {
	return s.sandbox.CreateDirectory(
		ctx, directory, opensandbox.OctalMode(os.FileMode(permissions.Mode)),
	)
}

func (s *openSandboxSDKRemote) ResourceUpload(
	ctx context.Context,
	filePath string,
	content io.Reader,
	permissions remoteFilePermissions,
) error {
	return s.sandbox.UploadFile(ctx, content, opensandbox.UploadFileOptions{
		FileName: path.Base(filePath),
		Metadata: opensandbox.FileMetadata{
			Path: filePath, Mode: opensandbox.OctalMode(os.FileMode(permissions.Mode)),
		},
	})
}

func (s *openSandboxSDKRemote) ResourceOpen(
	ctx context.Context,
	filePath string,
) (io.ReadCloser, error) {
	return s.sandbox.DownloadFile(ctx, filePath, "")
}

func (s *openSandboxSDKRemote) ResourceStat(
	ctx context.Context,
	filePath string,
) (remoteFileInfo, error) {
	items, err := s.sandbox.GetFileInfo(ctx, filePath)
	if err != nil {
		return remoteFileInfo{}, err
	}
	item, ok := items[filePath]
	if !ok {
		return remoteFileInfo{}, &opensandbox.APIError{
			StatusCode: http.StatusNotFound,
			Response: opensandbox.ErrorResponse{
				Code: "not_found", Message: "file metadata is missing",
			},
		}
	}
	return openSandboxRemoteFileInfo(item), nil
}

func (s *openSandboxSDKRemote) ResourceRemoveFile(
	ctx context.Context,
	filePath string,
) error {
	return s.sandbox.DeleteFiles(ctx, []string{filePath})
}

func (*openSandboxSDKRemote) ResourceIsNotFound(err error) bool {
	return isOpenSandboxNotFound(err)
}

func openSandboxRemoteFileInfo(item opensandbox.FileInfo) remoteFileInfo {
	return remoteFileInfo{
		SizeBytes: item.Size,
		Regular:   item.Type == "file",
		Directory: item.Type == "directory",
	}
}

func (s *openSandboxSDKRemote) ApplyLimitedNetwork(
	ctx context.Context,
	allowedHosts []string,
) error {
	status, err := s.sandbox.GetEgressPolicy(ctx)
	if err != nil {
		return err
	}
	if status == nil || status.Policy == nil {
		return errors.New("opensandbox: egress policy is unavailable")
	}
	if status.Policy.DefaultAction != "deny" {
		return fmt.Errorf(
			"opensandbox: egress default action is %q, want deny",
			status.Policy.DefaultAction,
		)
	}
	desired := make(map[string]struct{}, len(allowedHosts))
	for _, host := range allowedHosts {
		desired[host] = struct{}{}
	}
	currentAllows := make(map[string]struct{}, len(status.Policy.Egress))
	remove := make([]string, 0)
	for _, rule := range status.Policy.Egress {
		if rule.Action != "allow" {
			continue
		}
		currentAllows[rule.Target] = struct{}{}
		if _, keep := desired[rule.Target]; !keep {
			remove = append(remove, rule.Target)
		}
	}
	sort.Strings(remove)
	if len(remove) > 0 {
		if _, err := s.sandbox.DeleteEgressRules(ctx, remove); err != nil {
			return err
		}
	}
	add := make([]opensandbox.NetworkRule, 0)
	for _, host := range allowedHosts {
		if _, present := currentAllows[host]; present {
			continue
		}
		add = append(add, opensandbox.NetworkRule{Action: "allow", Target: host})
	}
	if len(add) > 0 {
		if _, err := s.sandbox.PatchEgressRules(ctx, add); err != nil {
			return err
		}
	}
	return nil
}

func (s *openSandboxSDKRemote) Destroy(ctx context.Context) error {
	return s.sandbox.Kill(ctx)
}

func joinOpenSandboxOutput(messages []opensandbox.OutputMessage) string {
	var builder strings.Builder
	for _, message := range messages {
		builder.WriteString(message.Text)
	}
	return builder.String()
}

func normalizeKubernetesMemory(value string) string {
	lower := strings.ToLower(strings.TrimSpace(value))
	if strings.HasSuffix(lower, "m") &&
		!strings.HasSuffix(lower, "mi") {
		return strings.TrimSuffix(lower, "m") + "Mi"
	}
	return value
}

func isOpenSandboxNotFound(err error) bool {
	var apiErr *opensandbox.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}
