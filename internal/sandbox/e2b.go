package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	cubesandbox "github.com/tencentcloud/CubeSandbox/sdk/go"
)

const (
	E2BProviderName  = "e2b"
	CubeProviderName = "cube"

	defaultE2BAPIURL   = "https://api.e2b.app"
	defaultE2BDomain   = "e2b.app"
	defaultE2BTemplate = "base"
)

// E2BConfig configures E2B's managed service. APIKey is worker-local and is
// never included in the durable Ref.
type E2BConfig struct {
	APIURL      string
	APIKey      string
	TemplateID  string
	Domain      string
	IdleTimeout time.Duration
	HTTPClient  *http.Client
}

// CubeConfig configures a self-hosted CubeSandbox deployment.
type CubeConfig struct {
	APIURL      string
	APIKey      string
	TemplateID  string
	Domain      string
	ProxyNodeIP string
	ProxyPort   int
	ProxyScheme string
	IdleTimeout time.Duration
	HTTPClient  *http.Client
}

type e2bResource struct {
	id       string
	metadata map[string]string
}

type e2bServiceSandbox interface {
	ID() string
	Exec(context.Context, string, string, time.Duration) (string, string, int, error)
	ReadFile(context.Context, string) ([]byte, error)
	WriteFile(context.Context, string, []byte) error
	Destroy(context.Context) error
	remoteFileResourceDataPlane
}

type e2bService interface {
	List(context.Context, map[string]string) ([]e2bResource, error)
	Get(context.Context, string) (e2bResource, error)
	Create(context.Context, string, Spec) (e2bServiceSandbox, error)
	Connect(context.Context, string) (e2bServiceSandbox, error)
}

type e2bLikeProvider struct {
	name    string
	service e2bService
	root    string
}

func NewE2BProvider(cfg E2BConfig) (Provider, error) {
	apiURL := strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/")
	if apiURL == "" {
		apiURL = defaultE2BAPIURL
	}
	apiKey := strings.TrimSpace(cfg.APIKey)
	if apiKey == "" {
		return nil, errors.New("sandbox: E2B_API_KEY is required for the e2b provider")
	}
	templateID := strings.TrimSpace(cfg.TemplateID)
	if templateID == "" {
		templateID = defaultE2BTemplate
	}
	domain := strings.TrimSpace(cfg.Domain)
	if domain == "" {
		domain = defaultE2BDomain
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 5 * time.Minute
	}
	transport, err := newE2BTransport(cfg.HTTPClient, apiURL, apiKey, idleTimeout)
	if err != nil {
		return nil, err
	}
	client := cubesandbox.NewClient(cubesandbox.Config{
		APIURL:         apiURL,
		TemplateID:     templateID,
		SandboxDomain:  domain,
		ProxyScheme:    "https",
		Timeout:        idleTimeout,
		RequestTimeout: remoteDefaultPeriod,
	}, cubesandbox.WithHTTPClient(transport))
	return newE2BLikeProvider(
		E2BProviderName,
		&cubeSDKService{
			name:   E2BProviderName,
			client: client,
			control: &e2bControlClient{
				apiURL: apiURL,
				client: remoteControlHTTPClient(transport),
			},
			idleTimeout: idleTimeout,
		},
		remoteDefaultRoot,
	), nil
}

func NewCubeProvider(cfg CubeConfig) (Provider, error) {
	apiURL := strings.TrimRight(strings.TrimSpace(cfg.APIURL), "/")
	if apiURL == "" {
		return nil, errors.New("sandbox: CUBE_API_URL is required for the cube provider")
	}
	templateID := strings.TrimSpace(cfg.TemplateID)
	if templateID == "" {
		return nil, errors.New("sandbox: CUBE_TEMPLATE_ID is required for the cube provider")
	}
	idleTimeout := cfg.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 5 * time.Minute
	}
	scheme := strings.TrimSpace(cfg.ProxyScheme)
	if scheme == "" {
		scheme = "http"
	}
	clientCfg := cubesandbox.Config{
		APIURL:         apiURL,
		APIKey:         strings.TrimSpace(cfg.APIKey),
		TemplateID:     templateID,
		ProxyNodeIP:    strings.TrimSpace(cfg.ProxyNodeIP),
		ProxyPortHTTP:  cfg.ProxyPort,
		ProxyScheme:    scheme,
		SandboxDomain:  strings.TrimSpace(cfg.Domain),
		Timeout:        idleTimeout,
		RequestTimeout: remoteDefaultPeriod,
	}
	options := []cubesandbox.ClientOption{}
	controlHTTP := remoteControlHTTPClient(cfg.HTTPClient)
	if cfg.HTTPClient != nil {
		options = append(options, cubesandbox.WithHTTPClient(cfg.HTTPClient))
	}
	client := cubesandbox.NewClient(clientCfg, options...)
	return newE2BLikeProvider(
		CubeProviderName,
		&cubeSDKService{
			name:   CubeProviderName,
			client: client,
			control: &e2bControlClient{
				apiURL:     apiURL,
				apiKey:     strings.TrimSpace(cfg.APIKey),
				bearerAuth: true,
				client:     controlHTTP,
			},
			idleTimeout: idleTimeout,
		},
		remoteDefaultRoot,
	), nil
}

func newE2BLikeProvider(
	name string,
	service e2bService,
	root string,
) Provider {
	if root == "" {
		root = remoteDefaultRoot
	}
	return &e2bLikeProvider{
		name:    name,
		service: service,
		root:    root,
	}
}

func (p *e2bLikeProvider) Name() string { return p.name }

func (*e2bLikeProvider) SupportsPackageSetup() bool { return true }

func (*e2bLikeProvider) SupportsFileResources() bool { return true }

func (*e2bLikeProvider) SupportsSessionOutputs() bool { return true }

func (*e2bLikeProvider) SupportsSkillBundles() bool { return true }

func (*e2bLikeProvider) SupportsGitRepositories() bool { return true }

func (p *e2bLikeProvider) Create(
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
	existing, err := p.findSession(ctx, sessionKey)
	if err != nil {
		return Ref{}, nil, err
	}
	if len(existing) > 0 {
		return p.attachResource(ctx, sessionKey, existing[0], spec)
	}
	remote, err := p.service.Create(ctx, sessionKey, spec)
	if err != nil {
		// A lost response or a concurrent creator may have provisioned the
		// resource. Metadata lookup is the provider-side recovery path.
		existing, findErr := p.findSession(ctx, sessionKey)
		if findErr == nil && len(existing) > 0 {
			return p.attachResource(ctx, sessionKey, existing[0], spec)
		}
		return Ref{}, nil, fmt.Errorf("sandbox: %s create: %w", p.name, err)
	}
	box := newE2BLikeSandbox(p.name, remote, p.root, spec.Timeout)
	if err := box.ensureRoot(ctx); err != nil {
		_ = remote.Destroy(context.Background())
		return Ref{}, nil, err
	}
	return Ref{Provider: p.name, ID: remote.ID()}, box, nil
}

func (p *e2bLikeProvider) Attach(
	ctx context.Context,
	sessionKey string,
	ref Ref,
	spec Spec,
) (Sandbox, error) {
	if err := validateRemoteReference(p.name, sessionKey, ref); err != nil {
		return nil, err
	}
	resource, err := p.service.Get(ctx, ref.ID)
	if err != nil {
		if isCubeNotFound(err) {
			return nil, fmt.Errorf(
				"%w: %s sandbox %q",
				ErrNotFound,
				p.name,
				ref.ID,
			)
		}
		return nil, fmt.Errorf("sandbox: %s get: %w", p.name, err)
	}
	if resource.id == "" {
		return nil, fmt.Errorf(
			"%w: %s sandbox %q",
			ErrNotFound,
			p.name,
			ref.ID,
		)
	}
	if err := validateRemoteOwnership(
		p.name,
		resource.id,
		sessionKey,
		resource.metadata,
	); err != nil {
		return nil, err
	}
	remote, err := p.service.Connect(ctx, resource.id)
	if err != nil {
		if isCubeNotFound(err) {
			return nil, fmt.Errorf("%w: %s sandbox %q", ErrNotFound, p.name, ref.ID)
		}
		return nil, fmt.Errorf("sandbox: %s connect: %w", p.name, err)
	}
	box := newE2BLikeSandbox(p.name, remote, p.root, spec.Timeout)
	if err := box.ensureRoot(ctx); err != nil {
		return nil, err
	}
	return box, nil
}

func (p *e2bLikeProvider) attachResource(
	ctx context.Context,
	sessionKey string,
	resource e2bResource,
	spec Spec,
) (Ref, Sandbox, error) {
	box, err := p.Attach(
		ctx,
		sessionKey,
		Ref{Provider: p.name, ID: resource.id},
		spec,
	)
	if err != nil {
		return Ref{}, nil, err
	}
	return Ref{Provider: p.name, ID: resource.id}, box, nil
}

func (p *e2bLikeProvider) findSession(
	ctx context.Context,
	sessionKey string,
) ([]e2bResource, error) {
	resources, err := p.service.List(ctx, remoteMetadata(sessionKey))
	if err != nil {
		return nil, fmt.Errorf("sandbox: %s list: %w", p.name, err)
	}
	matches := make([]e2bResource, 0, 1)
	for _, resource := range resources {
		if resource.metadata[remoteManagedKey] == remoteManagedValue &&
			resource.metadata[remoteSessionKey] ==
				remoteSessionIdentity(sessionKey) {
			matches = append(matches, resource)
		}
	}
	sort.Slice(matches, func(i, j int) bool { return matches[i].id < matches[j].id })
	return matches, nil
}

type e2bLikeSandbox struct {
	providerName string
	remote       e2bServiceSandbox
	root         string
	timeout      time.Duration
	resources    *remoteFileResources
	skills       *remoteSkillBundles
	repositories *commandGitRepositories
	sync         remoteResourceSynchronization
}

func newE2BLikeSandbox(
	providerName string,
	remote e2bServiceSandbox,
	root string,
	timeout time.Duration,
) *e2bLikeSandbox {
	box := &e2bLikeSandbox{
		providerName: providerName,
		remote:       remote,
		root:         root,
		timeout:      timeout,
		resources:    newRemoteFileResources(providerName, remote),
	}
	box.skills = newRemoteSkillBundles(
		providerName,
		box.resources,
		func(ctx context.Context, command Command) (*Result, error) {
			return box.exec(
				ctx, command, remoteOperationCommandTimeout(ctx, box.timeout),
			)
		},
	)
	box.repositories = newRemoteGitRepositories(
		providerName, box.resources,
		func(ctx context.Context, command Command) (*Result, error) {
			return box.exec(ctx, command, remoteOperationCommandTimeout(ctx, box.timeout))
		},
	)
	return box
}

func (s *e2bLikeSandbox) Root() string { return s.root }

func (s *e2bLikeSandbox) ensureRoot(ctx context.Context) error {
	_, stderr, code, err := s.remote.Exec(
		ctx,
		"mkdir -p "+shellQuote(s.root),
		"/",
		s.timeout,
	)
	if err != nil {
		return fmt.Errorf("sandbox: %s create workspace: %w", s.providerName, err)
	}
	if code != 0 {
		return fmt.Errorf(
			"sandbox: %s create workspace failed (exit %d): %s",
			s.providerName,
			code,
			strings.TrimSpace(stderr),
		)
	}
	return s.resources.ensureSessionOutputLayout(ctx)
}

func (s *e2bLikeSandbox) Exec(
	ctx context.Context,
	cmd Command,
) (*Result, error) {
	return s.exec(ctx, cmd, s.timeout)
}

func (s *e2bLikeSandbox) exec(
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
		return nil, fmt.Errorf("sandbox: %s exec: %w", s.providerName, err)
	}
	return remoteResult(runCtx, timeout, stdout, stderr, code)
}

func (s *e2bLikeSandbox) ReadFile(
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

func (s *e2bLikeSandbox) ReadFileBounded(
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

func (s *e2bLikeSandbox) WriteFile(
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
		return fmt.Errorf("sandbox: %s create parent: %w", s.providerName, err)
	}
	return s.remote.WriteFile(ctx, full, data)
}

func (s *e2bLikeSandbox) HasFileResource(
	ctx context.Context,
	mount FileResourceMount,
) (bool, error) {
	return s.resources.HasFileResource(ctx, mount)
}

func (s *e2bLikeSandbox) ImportFileResource(
	ctx context.Context,
	mount FileResourceMount,
	content io.Reader,
) error {
	return s.resources.ImportFileResource(ctx, mount, content)
}

func (s *e2bLikeSandbox) RemoveFileResource(
	ctx context.Context,
	runtimePath string,
	identity string,
) error {
	return s.resources.RemoveFileResource(ctx, runtimePath, identity)
}

func (s *e2bLikeSandbox) HasReadOnlySkill(
	ctx context.Context,
	mount ReadOnlySkillMount,
) (bool, error) {
	return s.skills.HasReadOnlySkill(ctx, mount)
}

func (s *e2bLikeSandbox) ImportReadOnlySkill(
	ctx context.Context,
	mount ReadOnlySkillMount,
	content io.Reader,
) error {
	return s.skills.ImportReadOnlySkill(ctx, mount, content)
}

func (s *e2bLikeSandbox) HasGitRepository(ctx context.Context, mount GitRepositoryMount) (bool, error) {
	return s.repositories.HasGitRepository(ctx, mount)
}

func (s *e2bLikeSandbox) ImportGitRepository(ctx context.Context, mount GitRepositoryMount, content io.Reader) error {
	return s.repositories.ImportGitRepository(ctx, mount, content)
}

func (s *e2bLikeSandbox) RemoveGitRepository(ctx context.Context, runtimePath, identity string) error {
	return s.repositories.RemoveGitRepository(ctx, runtimePath, identity)
}

func (s *e2bLikeSandbox) OpenSessionOutputs(ctx context.Context) (io.ReadCloser, error) {
	return openRemoteSessionOutputs(
		ctx,
		s.providerName,
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

func (s *e2bLikeSandbox) LockResourceOperation(ctx context.Context) (func(), error) {
	return s.sync.LockResourceOperation(ctx)
}

func (s *e2bLikeSandbox) TryLockResourceSync(
	ctx context.Context,
) (context.Context, func(), bool, error) {
	return s.sync.TryLockResourceSync(ctx)
}

func (s *e2bLikeSandbox) LockResourceSync(
	ctx context.Context,
) (context.Context, func(), error) {
	return s.sync.LockResourceSync(ctx)
}

func (s *e2bLikeSandbox) Destroy(ctx context.Context) error {
	err := s.remote.Destroy(ctx)
	if isCubeNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("sandbox: %s destroy: %w", s.providerName, err)
	}
	return nil
}

var _ SkillBundleSandbox = (*e2bLikeSandbox)(nil)

type cubeSDKService struct {
	name        string
	client      *cubesandbox.Client
	control     *e2bControlClient
	idleTimeout time.Duration
}

func (s *cubeSDKService) List(
	ctx context.Context,
	metadata map[string]string,
) ([]e2bResource, error) {
	items, err := s.control.List(ctx, metadata)
	if err != nil {
		return nil, err
	}
	resources := make([]e2bResource, 0, len(items))
	for _, item := range items {
		resources = append(resources, e2bResource{
			id:       item.SandboxID,
			metadata: item.Metadata,
		})
	}
	return resources, nil
}

func (s *cubeSDKService) Get(
	ctx context.Context,
	id string,
) (e2bResource, error) {
	item, err := s.control.Get(ctx, id)
	if err != nil {
		return e2bResource{}, err
	}
	return e2bResource{id: item.SandboxID, metadata: item.Metadata}, nil
}

func (s *cubeSDKService) Create(
	ctx context.Context,
	sessionKey string,
	spec Spec,
) (e2bServiceSandbox, error) {
	timeout := s.idleTimeout
	allowInternet := spec.Network != "" && spec.Network != "none"
	opts := cubesandbox.CreateOptions{
		Timeout:  &timeout,
		Metadata: remoteMetadata(sessionKey),
		Extra: map[string]any{
			"autoPause":       true,
			"autoPauseMemory": false,
			"autoResume":      map[string]bool{"enabled": false},
			"secure":          true,
		},
	}
	if s.name == E2BProviderName {
		opts.Extra["allow_internet_access"] = allowInternet
	} else {
		opts.AllowInternetAccess = &allowInternet
	}
	created, err := s.client.Create(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &cubeSDKSandbox{sandbox: created}, nil
}

func (s *cubeSDKService) Connect(
	ctx context.Context,
	id string,
) (e2bServiceSandbox, error) {
	connected, err := s.client.Connect(ctx, id)
	if err != nil {
		return nil, err
	}
	return &cubeSDKSandbox{sandbox: connected}, nil
}

type cubeSDKSandbox struct {
	sandbox *cubesandbox.Sandbox
}

func (s *cubeSDKSandbox) ID() string { return s.sandbox.SandboxID }

func (s *cubeSDKSandbox) Exec(
	ctx context.Context,
	command string,
	cwd string,
	timeout time.Duration,
) (string, string, int, error) {
	result, err := s.sandbox.Commands().Run(ctx, command, cubesandbox.CommandOptions{
		Cwd:     cwd,
		Timeout: timeout,
	})
	if err != nil {
		return "", "", -1, err
	}
	return result.Stdout, result.Stderr, result.ExitCode, nil
}

func (s *cubeSDKSandbox) ReadFile(
	ctx context.Context,
	value string,
) ([]byte, error) {
	content, err := s.sandbox.Files().Read(ctx, value)
	return []byte(content), err
}

func (s *cubeSDKSandbox) WriteFile(
	ctx context.Context,
	value string,
	data []byte,
) error {
	return s.sandbox.Files().Write(ctx, value, data)
}

// The current CubeSandbox Go SDK exposes whole-value Read and Write methods.
// Mango adapts them to its Reader boundary here, accepting one in-memory copy
// for E2B and Cube until that SDK exposes streaming file operations. It also
// lacks per-operation mode options, so these adapters retain provider-default
// permissions inside their Session-isolated sandboxes.
func (s *cubeSDKSandbox) ResourceCreateDirectory(
	ctx context.Context,
	directory string,
	_ remoteFilePermissions,
) error {
	_, err := s.sandbox.Files().MakeDir(ctx, directory)
	return err
}

func (s *cubeSDKSandbox) ResourceUpload(
	ctx context.Context,
	filePath string,
	content io.Reader,
	_ remoteFilePermissions,
) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	return s.sandbox.Files().Write(ctx, filePath, data)
}

func (s *cubeSDKSandbox) ResourceOpen(
	ctx context.Context,
	filePath string,
) (io.ReadCloser, error) {
	content, err := s.sandbox.Files().Read(ctx, filePath)
	if err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

func (s *cubeSDKSandbox) ResourceStat(
	ctx context.Context,
	filePath string,
) (remoteFileInfo, error) {
	item, err := s.sandbox.Files().Stat(ctx, filePath)
	if err != nil {
		return remoteFileInfo{}, err
	}
	return remoteFileInfo{
		SizeBytes: item.Size,
		Regular:   item.Type == "FILE_TYPE_FILE",
		Directory: item.IsDir(),
	}, nil
}

func (s *cubeSDKSandbox) ResourceRemoveFile(
	ctx context.Context,
	filePath string,
) error {
	if _, err := s.sandbox.Files().Stat(ctx, filePath); err != nil {
		return err
	}
	if err := s.sandbox.Files().Remove(ctx, filePath); err != nil {
		// The pinned SDK does not preserve a typed 404 from Remove. Re-read
		// after a failed delete so a concurrent or retried absence is still
		// idempotent at Mango's resource boundary.
		if _, statErr := s.sandbox.Files().Stat(ctx, filePath); s.ResourceIsNotFound(statErr) {
			return statErr
		}
		return err
	}
	return nil
}

func (*cubeSDKSandbox) ResourceIsNotFound(err error) bool {
	var notFound *cubesandbox.NotFoundError
	return errors.As(err, &notFound) || isCubeNotFound(err)
}

func (s *cubeSDKSandbox) Destroy(ctx context.Context) error {
	return s.sandbox.Kill(ctx)
}

func isCubeNotFound(err error) bool {
	return errors.Is(err, cubesandbox.ErrSandboxNotFound)
}

type e2bControlClient struct {
	apiURL     string
	apiKey     string
	bearerAuth bool
	client     *http.Client
}

func (c *e2bControlClient) List(
	ctx context.Context,
	metadata map[string]string,
) ([]cubesandbox.SandboxInfo, error) {
	var all []cubesandbox.SandboxInfo
	nextToken := ""
	seenTokens := map[string]struct{}{}
	for {
		query := url.Values{}
		query.Set("limit", "100")
		query.Set("state", "running,paused")
		if nextToken != "" {
			query.Set("nextToken", nextToken)
		}
		if len(metadata) > 0 {
			encodedMetadata := url.Values{}
			for key, value := range metadata {
				encodedMetadata.Set(key, value)
			}
			query.Set("metadata", encodedMetadata.Encode())
		}
		var page []cubesandbox.SandboxInfo
		response, err := c.doJSON(
			ctx,
			http.MethodGet,
			"/v2/sandboxes?"+query.Encode(),
			nil,
			&page,
		)
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
		nextToken = response.Header.Get("X-Next-Token")
		if nextToken == "" {
			return all, nil
		}
		if _, exists := seenTokens[nextToken]; exists {
			return nil, errors.New("sandbox: repeated sandbox-list page token")
		}
		seenTokens[nextToken] = struct{}{}
	}
}

func (c *e2bControlClient) Get(
	ctx context.Context,
	id string,
) (cubesandbox.SandboxInfo, error) {
	var info cubesandbox.SandboxInfo
	_, err := c.doJSON(
		ctx,
		http.MethodGet,
		"/sandboxes/"+url.PathEscape(id),
		nil,
		&info,
	)
	return info, err
}

func (c *e2bControlClient) doJSON(
	ctx context.Context,
	method string,
	requestPath string,
	body io.Reader,
	out any,
) (*http.Response, error) {
	if c.client == nil {
		return nil, errors.New("sandbox: HTTP client is required")
	}
	request, err := http.NewRequestWithContext(
		ctx,
		method,
		c.apiURL+requestPath,
		body,
	)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		if c.bearerAuth {
			request.Header.Set("Authorization", "Bearer "+c.apiKey)
		} else {
			request.Header.Set("X-API-Key", c.apiKey)
		}
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNotFound {
		return response, cubesandbox.ErrSandboxNotFound
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		return response, fmt.Errorf(
			"HTTP %d: %s",
			response.StatusCode,
			strings.TrimSpace(string(message)),
		)
	}
	if out == nil || response.StatusCode == http.StatusNoContent {
		return response, nil
	}
	if err := json.NewDecoder(response.Body).Decode(out); err != nil {
		return response, err
	}
	return response, nil
}

// newE2BTransport adapts CubeSandbox's E2B-compatible Go client to E2B Cloud:
// E2B uses X-API-Key instead of Bearer auth, requires a timeout on Connect, and
// returns 201 when Connect resumes a paused sandbox.
func newE2BTransport(
	baseClient *http.Client,
	apiURL string,
	apiKey string,
	idleTimeout time.Duration,
) (*http.Client, error) {
	parsed, err := url.Parse(apiURL)
	if err != nil || parsed.Host == "" {
		return nil, fmt.Errorf("sandbox: invalid E2B_API_URL %q", apiURL)
	}
	base := http.DefaultTransport
	if baseClient != nil && baseClient.Transport != nil {
		base = baseClient.Transport
	}
	client := &http.Client{}
	if baseClient != nil {
		*client = *baseClient
	}
	client.Transport = &e2bRoundTripper{
		base:           base,
		apiHost:        parsed.Host,
		apiKey:         apiKey,
		connectTimeout: int(idleTimeout.Round(time.Second) / time.Second),
	}
	return client, nil
}

type e2bRoundTripper struct {
	base           http.RoundTripper
	apiHost        string
	apiKey         string
	connectTimeout int
}

func (t *e2bRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	if clone.URL.Host == t.apiHost {
		clone.Header.Del("Authorization")
		clone.Header.Set("X-API-Key", t.apiKey)
	}
	isConnect := clone.Method == http.MethodPost &&
		strings.HasSuffix(clone.URL.Path, "/connect")
	if isConnect && clone.URL.Host == t.apiHost {
		payload := []byte(fmt.Sprintf(`{"timeout":%d}`, t.connectTimeout))
		clone.Body = io.NopCloser(bytes.NewReader(payload))
		clone.ContentLength = int64(len(payload))
		clone.Header.Set("Content-Type", "application/json")
	}
	response, err := t.base.RoundTrip(clone)
	if err != nil {
		return nil, err
	}
	if isConnect && response.StatusCode == http.StatusCreated {
		response.StatusCode = http.StatusOK
		response.Status = "200 OK"
	}
	return response, nil
}
