package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
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
	"github.com/yanpgwang/mango/internal/domain"
)

const (
	OpenSandboxProviderName        = "opensandbox"
	defaultOpenSandboxImage        = "python:3.12-slim"
	defaultOpenSandboxReadyTimeout = 2 * time.Minute
	openSandboxAgentUID            = int32(1000)
	openSandboxAgentGID            = int32(1000)
	openSandboxAgentCapture        = "/tmp/mango-command-output"
)

type OpenSandboxConfig struct {
	BaseURL    string
	APIKey     string
	Image      string
	UseProxy   bool
	HTTPClient *http.Client
	// ReadyTimeout bounds image pull, container startup, and execd health checks.
	// Zero uses a cold-start-safe default.
	ReadyTimeout time.Duration
}

type openSandboxResource struct {
	id       string
	metadata map[string]string
}

type openSandboxRemote interface {
	ID() string
	Exec(
		context.Context,
		string,
		string,
		time.Duration,
		*int32,
		*int32,
	) (string, string, int, error)
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
	Delete(context.Context, string) error
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
	readyTimeout := cfg.ReadyTimeout
	if readyTimeout == 0 {
		readyTimeout = defaultOpenSandboxReadyTimeout
	}
	if readyTimeout < 0 {
		return nil, errors.New("sandbox: OpenSandbox ready timeout must be positive")
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
			config:       connection,
			manager:      opensandbox.NewSandboxManager(connection),
			image:        image,
			readyTimeout: readyTimeout,
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

func (*openSandboxProvider) SupportsMemoryStores() bool { return true }

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
	if err := validateSandboxNetworkSpec(spec); err != nil {
		return Ref{}, nil, Permanent(err)
	}
	if err := validateMemoryStoreMounts(spec.MemoryStores); err != nil {
		return Ref{}, nil, Permanent(err)
	}
	existing, err := p.service.List(ctx, remoteMetadata(sessionKey))
	if err != nil {
		return Ref{}, nil, fmt.Errorf("sandbox: opensandbox list: %w", err)
	}
	if len(existing) > 0 {
		return p.adoptResource(ctx, sessionKey, "", existing, spec)
	}
	remote, err := p.service.Create(ctx, sessionKey, spec)
	if err != nil {
		existing, findErr := p.service.List(ctx, remoteMetadata(sessionKey))
		if findErr == nil && len(existing) > 0 {
			return p.adoptResource(ctx, sessionKey, "", existing, spec)
		}
		return Ref{}, nil, fmt.Errorf("sandbox: opensandbox create: %w", err)
	}
	// The server has no idempotency key for create. Re-list after creation and
	// deterministically adopt one resource so concurrent workers converge. Add
	// the resource returned by create in case list visibility lags behind create;
	// a later Create or Attach repeats this reconciliation and removes any
	// duplicate left by a process crash in this window.
	existing, err = p.service.List(ctx, remoteMetadata(sessionKey))
	if err != nil {
		return Ref{}, nil, fmt.Errorf(
			"sandbox: opensandbox reconcile created sandbox %q: %w", remote.ID(), err,
		)
	}
	if !containsOpenSandboxResource(existing, remote.ID()) {
		existing = append(existing, openSandboxResource{
			id: remote.ID(), metadata: remoteMetadata(sessionKey),
		})
	}
	return p.adoptResource(ctx, sessionKey, "", existing, spec)
}

func (p *openSandboxProvider) Attach(
	ctx context.Context,
	sessionKey string,
	ref Ref,
	spec Spec,
) (Sandbox, error) {
	if err := validateSandboxNetworkSpec(spec); err != nil {
		return nil, Permanent(err)
	}
	if err := validateMemoryStoreMounts(spec.MemoryStores); err != nil {
		return nil, Permanent(err)
	}
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
	existing, err := p.service.List(ctx, remoteMetadata(sessionKey))
	if err != nil {
		return nil, fmt.Errorf("sandbox: opensandbox list for attach: %w", err)
	}
	if !containsOpenSandboxResource(existing, ref.ID) {
		existing = append(existing, resource)
	}
	_, box, err := p.adoptResource(ctx, sessionKey, ref.ID, existing, spec)
	return box, err
}

func (p *openSandboxProvider) adoptResource(
	ctx context.Context,
	sessionKey string,
	preferredID string,
	resources []openSandboxResource,
	spec Spec,
) (Ref, Sandbox, error) {
	resource, err := p.reconcileResources(ctx, sessionKey, preferredID, resources)
	if err != nil {
		return Ref{}, nil, err
	}
	ref := Ref{Provider: p.Name(), ID: resource.id}
	remote, err := p.service.Connect(ctx, resource.id)
	if err != nil {
		if isOpenSandboxNotFound(err) {
			return Ref{}, nil, fmt.Errorf(
				"%w: opensandbox sandbox %q", ErrNotFound, resource.id,
			)
		}
		return Ref{}, nil, fmt.Errorf("sandbox: opensandbox connect: %w", err)
	}
	box := newOpenSandboxBox(remote, p.root, spec.Timeout, spec.MemoryStores)
	if err := box.ensureRoot(ctx); err != nil {
		return Ref{}, nil, err
	}
	return ref, box, nil
}

func (p *openSandboxProvider) reconcileResources(
	ctx context.Context,
	sessionKey string,
	preferredID string,
	resources []openSandboxResource,
) (openSandboxResource, error) {
	if len(resources) == 0 {
		return openSandboxResource{}, errors.New(
			"sandbox: opensandbox reconciliation found no sandbox",
		)
	}
	resources = append([]openSandboxResource(nil), resources...)
	sort.Slice(resources, func(i, j int) bool { return resources[i].id < resources[j].id })
	unique := resources[:0]
	for _, resource := range resources {
		if err := validateRemoteOwnership(
			p.Name(), resource.id, sessionKey, resource.metadata,
		); err != nil {
			return openSandboxResource{}, err
		}
		if len(unique) == 0 || unique[len(unique)-1].id != resource.id {
			unique = append(unique, resource)
		}
	}
	selected := unique[0]
	if preferredID != "" {
		found := false
		for _, resource := range unique {
			if resource.id == preferredID {
				selected, found = resource, true
				break
			}
		}
		if !found {
			return openSandboxResource{}, fmt.Errorf(
				"%w: opensandbox sandbox %q", ErrNotFound, preferredID,
			)
		}
	}
	for _, resource := range unique {
		if resource.id == selected.id {
			continue
		}
		if err := p.service.Delete(ctx, resource.id); err != nil &&
			!isOpenSandboxNotFound(err) {
			return openSandboxResource{}, fmt.Errorf(
				"sandbox: opensandbox remove duplicate %q: %w", resource.id, err,
			)
		}
	}
	return selected, nil
}

func containsOpenSandboxResource(resources []openSandboxResource, id string) bool {
	for _, resource := range resources {
		if resource.id == id {
			return true
		}
	}
	return false
}

func (p *openSandboxProvider) DestroyBoundSession(
	ctx context.Context,
	sessionKey string,
	ref Ref,
) error {
	if err := validateRemoteReference(p.Name(), sessionKey, ref); err != nil {
		return err
	}
	selected, getErr := p.service.Get(ctx, ref.ID)
	if getErr != nil && !isOpenSandboxNotFound(getErr) {
		return fmt.Errorf("sandbox: opensandbox get for destroy: %w", getErr)
	}
	if getErr == nil {
		if err := validateRemoteOwnership(
			p.Name(), selected.id, sessionKey, selected.metadata,
		); err != nil {
			return err
		}
	}
	resources, err := p.service.List(ctx, remoteMetadata(sessionKey))
	if err != nil {
		return fmt.Errorf("sandbox: opensandbox list for destroy: %w", err)
	}
	if getErr == nil && !containsOpenSandboxResource(resources, ref.ID) {
		resources = append(resources, selected)
	}
	resources = append([]openSandboxResource(nil), resources...)
	sort.Slice(resources, func(i, j int) bool {
		leftSelected := resources[i].id == ref.ID
		rightSelected := resources[j].id == ref.ID
		if leftSelected != rightSelected {
			return !leftSelected
		}
		return resources[i].id < resources[j].id
	})
	unique := resources[:0]
	previousID := ""
	for _, resource := range resources {
		if resource.id == previousID {
			continue
		}
		previousID = resource.id
		if err := validateRemoteOwnership(
			p.Name(), resource.id, sessionKey, resource.metadata,
		); err != nil {
			return err
		}
		unique = append(unique, resource)
	}
	for _, resource := range unique {
		if err := p.service.Delete(ctx, resource.id); err != nil &&
			!isOpenSandboxNotFound(err) {
			return fmt.Errorf(
				"sandbox: opensandbox destroy %q: %w", resource.id, err,
			)
		}
	}
	return nil
}

type openSandboxBox struct {
	remote       openSandboxRemote
	root         string
	timeout      time.Duration
	resources    *remoteFileResources
	skills       *remoteSkillBundles
	repositories *commandGitRepositories
	memoryStores map[string]MemoryStoreMount
	sync         remoteResourceSynchronization
}

func newOpenSandboxBox(
	remote openSandboxRemote,
	root string,
	timeout time.Duration,
	memoryStores []MemoryStoreMount,
) *openSandboxBox {
	mounts := make(map[string]MemoryStoreMount, len(memoryStores))
	for _, mount := range memoryStores {
		mounts[mount.Identity] = mount
	}
	box := &openSandboxBox{
		remote: remote, root: root, timeout: timeout,
		resources:    newRemoteFileResources(OpenSandboxProviderName, remote),
		memoryStores: mounts,
	}
	box.skills = newRemoteSkillBundles(
		OpenSandboxProviderName,
		box.resources,
		func(ctx context.Context, command Command) (*Result, error) {
			return box.execMaintenance(
				ctx, command, remoteOperationCommandTimeout(ctx, box.timeout),
			)
		},
	)
	box.repositories = newRemoteGitRepositories(
		OpenSandboxProviderName, box.resources,
		func(ctx context.Context, command Command) (*Result, error) {
			return box.execMaintenance(
				ctx, command, remoteOperationCommandTimeout(ctx, box.timeout),
			)
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
	for _, directory := range []struct {
		path string
		mode int
	}{
		{s.root, 0o777},
		{path.Join(s.root, ".mango-home"), 0o700},
		{openSandboxAgentCapture, 0o700},
	} {
		if err := s.resources.ensureDirectory(ctx, directory.path, directory.mode); err != nil {
			return err
		}
	}
	if err := s.resources.ensureSessionOutputLayout(ctx); err != nil {
		return err
	}
	if err := s.ensureMemoryLayout(ctx); err != nil {
		return err
	}
	result, err := s.execMaintenance(ctx, Command{
		Path: "chown",
		Args: []string{
			"-R",
			fmt.Sprintf("%d:%d", openSandboxAgentUID, openSandboxAgentGID),
			path.Join(s.root, ".mango-home"),
			openSandboxAgentCapture,
		},
	}, remoteOperationCommandTimeout(ctx, s.timeout))
	if err != nil {
		return fmt.Errorf("sandbox: opensandbox prepare agent identity: %w", err)
	}
	if result == nil || result.ExitCode != 0 {
		return Permanent(fmt.Errorf(
			"sandbox: opensandbox cannot prepare agent identity: %s",
			remoteCommandFailure(result),
		))
	}
	return nil
}

func (s *openSandboxBox) Exec(
	ctx context.Context,
	cmd Command,
) (*Result, error) {
	uid, gid := openSandboxAgentUID, openSandboxAgentGID
	return s.execWithIdentity(
		ctx, cmd, s.timeout, &uid, &gid,
	)
}

func (s *openSandboxBox) ExecPackageSetup(
	ctx context.Context,
	cmd Command,
) (*Result, error) {
	return s.execMaintenance(ctx, cmd, s.timeout)
}

func (s *openSandboxBox) execMaintenance(
	ctx context.Context,
	cmd Command,
	timeout time.Duration,
) (*Result, error) {
	return s.execWithIdentity(ctx, cmd, timeout, nil, nil)
}

func (s *openSandboxBox) execWithIdentity(
	ctx context.Context,
	cmd Command,
	timeout time.Duration,
	uid *int32,
	gid *int32,
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
		uid,
		gid,
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
	full, err := s.toolFilePath(value, false)
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
	if maxBytes <= 0 || maxBytes >= maxOutput {
		return nil, false, fmt.Errorf(
			"sandbox: bounded read limit must be between 1 and %d bytes",
			maxOutput-1,
		)
	}
	full, err := s.toolFilePath(value, false)
	if err != nil {
		return nil, false, err
	}
	reader, err := s.remote.ResourceOpen(ctx, full)
	if err != nil {
		return nil, false, fmt.Errorf("sandbox: opensandbox bounded read: %w", err)
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, false, fmt.Errorf("sandbox: opensandbox bounded read: %w", readErr)
	}
	if closeErr != nil {
		return nil, false, fmt.Errorf("sandbox: opensandbox bounded read close: %w", closeErr)
	}
	if int64(len(data)) > maxBytes {
		return data[:maxBytes], true, nil
	}
	return data, false, nil
}

func (s *openSandboxBox) WriteFile(
	ctx context.Context,
	value string,
	data []byte,
) error {
	full, err := s.toolFilePath(value, true)
	if err != nil {
		return err
	}
	if err := s.resources.ensureDirectory(ctx, path.Dir(full), 0o777); err != nil {
		return fmt.Errorf("sandbox: opensandbox create parent: %w", err)
	}
	return s.remote.WriteFile(ctx, full, data)
}

func (s *openSandboxBox) toolFilePath(value string, writable bool) (string, error) {
	if path.IsAbs(value) && pathWithinRemoteRoot(domain.SessionMemoryRoot, value) {
		clean := path.Clean(value)
		for _, mount := range s.memoryStores {
			if clean == mount.RuntimePath || !pathWithinRemoteRoot(mount.RuntimePath, clean) {
				continue
			}
			if writable && mount.Access != domain.MemoryAccessReadWrite {
				return "", fmt.Errorf("sandbox: path %q is read-only", value)
			}
			return clean, nil
		}
		return "", fmt.Errorf("sandbox: path %q is outside the authorized Memory mounts", value)
	}
	if writable {
		return remoteWritableToolPath(
			s.root, value, SessionUploadsRoot, SessionOutputsRoot, SessionRepositoryRoot,
		)
	}
	return remoteToolPath(
		s.root, value, SessionUploadsRoot, SessionOutputsRoot, SessionSkillsRoot,
		SessionRepositoryRoot,
	)
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
	if err := s.repositories.ImportGitRepository(ctx, mount, content); err != nil {
		return err
	}
	result, err := s.execMaintenance(ctx, Command{
		Path: "chown",
		Args: []string{
			"-R",
			fmt.Sprintf("%d:%d", openSandboxAgentUID, openSandboxAgentGID),
			mount.RuntimePath,
		},
	}, remoteOperationCommandTimeout(ctx, s.timeout))
	if err != nil {
		return fmt.Errorf("sandbox: opensandbox prepare Git repository ownership: %w", err)
	}
	if result == nil || result.ExitCode != 0 {
		return Permanent(fmt.Errorf(
			"sandbox: opensandbox cannot prepare Git repository ownership: %s",
			remoteCommandFailure(result),
		))
	}
	return nil
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
			return s.execMaintenance(
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
	config       opensandbox.ConnectionConfig
	manager      *opensandbox.SandboxManager
	image        string
	readyTimeout time.Duration
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

func (s *openSandboxSDKService) Delete(ctx context.Context, id string) error {
	return s.manager.KillSandbox(ctx, id)
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
	volumes, err := openSandboxMemoryVolumes(sessionKey, spec.MemoryStores)
	if err != nil {
		return nil, err
	}
	created, err := opensandbox.CreateSandbox(
		ctx,
		s.config,
		opensandbox.SandboxCreateOptions{
			Image:          image,
			ResourceLimits: limits,
			Metadata:       remoteMetadata(sessionKey),
			ManualCleanup:  true,
			NetworkPolicy:  policy,
			Volumes:        volumes,
			ReadyTimeout:   s.readyTimeout,
		},
	)
	if err != nil {
		return nil, err
	}
	return &openSandboxSDKRemote{sandbox: created}, nil
}

func openSandboxMemoryVolumes(
	sessionKey string,
	mounts []MemoryStoreMount,
) ([]opensandbox.Volume, error) {
	if err := validateMemoryStoreMounts(mounts); err != nil {
		return nil, Permanent(err)
	}
	volumes := make([]opensandbox.Volume, 0, len(mounts)*2)
	for _, mount := range mounts {
		identity := openSandboxMemoryIdentity(mount.Identity)
		claimSum := sha256.Sum256([]byte(
			"opensandbox-memory\x00" + sessionKey + "\x00" + mount.Identity,
		))
		claim := "mango-memory-" + hex.EncodeToString(claimSum[:16])
		create, cleanup := true, true
		pvc := func() *opensandbox.PVC {
			return &opensandbox.PVC{
				ClaimName:                  claim,
				CreateIfNotExists:          &create,
				DeleteOnSandboxTermination: &cleanup,
				AccessModes:                []string{"ReadWriteOnce"},
			}
		}
		volumes = append(volumes,
			opensandbox.Volume{
				Name: "memory-" + identity + "-public", PVC: pvc(),
				MountPath: mount.RuntimePath,
				ReadOnly:  mount.Access == domain.MemoryAccessReadOnly,
			},
			opensandbox.Volume{
				Name: "memory-" + identity + "-control", PVC: pvc(),
				MountPath: openSandboxMemoryControlPath(mount),
			},
		)
	}
	return volumes, nil
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
	case "bridge":
		return nil
	default:
		// Provider entry points reject unknown modes. Retain deny-by-default here
		// as a defense if a future internal caller bypasses that validation.
		return &opensandbox.NetworkPolicy{DefaultAction: "deny"}
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
	uid *int32,
	gid *int32,
) (string, string, int, error) {
	captureRoot := "/var/lib/mango/command-output"
	if uid != nil {
		captureRoot = openSandboxAgentCapture
	}
	stdoutPath, stderrPath, err := openSandboxCapturePaths(captureRoot)
	if err != nil {
		return "", "", -1, err
	}
	// execd exposes line-oriented output events, which cannot distinguish
	// `printf x` from `printf 'x\n'` and therefore cannot satisfy Mango's
	// byte-preserving Exec contract. Capture inside the sandbox and download the
	// two files after completion. The command still runs through execd, so
	// timeout and exit-code semantics remain provider-owned.
	capturedCommand := "umask 077; mkdir -p " + shellQuote(captureRoot) + " && (" +
		command + ") >" + shellQuote(stdoutPath) + " 2>" + shellQuote(stderrPath)
	request := opensandbox.RunCommandRequest{
		Command: capturedCommand,
		Cwd:     cwd,
		UID:     uid,
		GID:     gid,
	}
	if uid != nil {
		request.Envs = map[string]string{
			"HOME": path.Join(remoteDefaultRoot, ".mango-home"),
			"USER": "mango",
		}
	}
	if timeout > 0 {
		request.Timeout = timeout.Milliseconds()
	}
	execution, runErr := s.sandbox.RunCommandWithOpts(
		ctx,
		request,
		nil,
	)

	readCtx := ctx
	cancelRead := func() {}
	if ctx.Err() != nil {
		readCtx, cancelRead = context.WithTimeout(context.Background(), 5*time.Second)
	}
	defer cancelRead()
	stdout, stdoutErr := s.readCommandCapture(readCtx, stdoutPath)
	stderr, stderrErr := s.readCommandCapture(readCtx, stderrPath)
	cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelCleanup()
	_ = s.sandbox.DeleteFiles(cleanupCtx, []string{stdoutPath, stderrPath})

	// A command that never reached the capture wrapper has no exact files. Keep
	// the SDK stream as diagnostic fallback, then return the original run error.
	if execution == nil {
		execution = &opensandbox.Execution{}
	}
	if stdoutErr != nil {
		stdout = joinOpenSandboxOutput(execution.Stdout)
	}
	if stderrErr != nil {
		stderr = joinOpenSandboxOutput(execution.Stderr)
	}
	if runErr != nil {
		return stdout, stderr, -1, runErr
	}
	if stdoutErr != nil {
		return stdout, stderr, -1, fmt.Errorf(
			"opensandbox: read command stdout: %w", stdoutErr,
		)
	}
	if stderrErr != nil {
		return stdout, stderr, -1, fmt.Errorf(
			"opensandbox: read command stderr: %w", stderrErr,
		)
	}
	exitCode := 0
	if execution.ExitCode != nil {
		exitCode = *execution.ExitCode
	}
	return stdout, stderr, exitCode, nil
}

func openSandboxCapturePaths(root string) (string, string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", "", fmt.Errorf("opensandbox: create command capture id: %w", err)
	}
	base := path.Join(root, hex.EncodeToString(nonce[:]))
	return base + ".stdout", base + ".stderr", nil
}

func (s *openSandboxSDKRemote) readCommandCapture(
	ctx context.Context,
	filePath string,
) (string, error) {
	reader, err := s.sandbox.DownloadFile(ctx, filePath, "")
	if err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, maxOutput+1))
	closeErr := reader.Close()
	if readErr != nil {
		return "", readErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if len(data) > maxOutput {
		data = data[:maxOutput]
	}
	return string(data), nil
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
				// Agent-visible files must remain readable by the unprivileged
				// command identity. Internal resource uploads pass stricter modes
				// through ResourceUpload instead.
				Mode: 666,
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
