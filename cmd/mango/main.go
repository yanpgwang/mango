package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/blob"
	"github.com/yanpgwang/mango/internal/controlplane"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/gitrepo"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/live"
	"github.com/yanpgwang/mango/internal/mcpclient"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/oauthclient"
	"github.com/yanpgwang/mango/internal/pg"
	"github.com/yanpgwang/mango/internal/sandbox"
	"github.com/yanpgwang/mango/internal/secretcrypto"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
)

// defaultAddr binds to loopback by default so a fresh `serve` never exposes the
// unauthenticated API on all interfaces. Operators who want a public bind must
// pass -addr explicitly (e.g. -addr :8080).
const defaultAddr = "127.0.0.1:8080"

const (
	sandboxProviderEnv    = "MANGO_SANDBOX"
	sandboxImageEnv       = "MANGO_SANDBOX_IMAGE"
	sandboxResourceDirEnv = "MANGO_SANDBOX_RESOURCE_DIR"

	e2bAPIKeyEnv      = "E2B_API_KEY"
	e2bAPIURLEnv      = "E2B_API_URL"
	e2bTemplateEnv    = "E2B_TEMPLATE_ID"
	e2bDomainEnv      = "E2B_DOMAIN"
	e2bIdleTimeoutEnv = "E2B_IDLE_TIMEOUT"

	cubeAPIKeyEnv      = "CUBE_API_KEY"
	cubeAPIURLEnv      = "CUBE_API_URL"
	cubeTemplateEnv    = "CUBE_TEMPLATE_ID"
	cubeDomainEnv      = "CUBE_SANDBOX_DOMAIN"
	cubeProxyNodeIPEnv = "CUBE_PROXY_NODE_IP"
	cubeProxyPortEnv   = "CUBE_PROXY_PORT_HTTP"
	cubeProxySchemeEnv = "CUBE_PROXY_SCHEME"
	cubeIdleTimeoutEnv = "CUBE_IDLE_TIMEOUT"

	openSandboxDomainEnv   = "OPEN_SANDBOX_DOMAIN"
	openSandboxAPIKeyEnv   = "OPEN_SANDBOX_API_KEY"
	openSandboxImageEnv    = "OPEN_SANDBOX_IMAGE"
	openSandboxUseProxyEnv = "OPEN_SANDBOX_USE_SERVER_PROXY"

	daytonaAPIKeyEnv    = "DAYTONA_API_KEY"
	daytonaAPIURLEnv    = "DAYTONA_API_URL"
	daytonaTargetEnv    = "DAYTONA_TARGET"
	daytonaSnapshotEnv  = "DAYTONA_SNAPSHOT"
	daytonaImageEnv     = "DAYTONA_IMAGE"
	daytonaAutoPauseEnv = "DAYTONA_AUTO_PAUSE_MINUTES"

	fileS3EndpointEnv     = "MANGO_FILE_S3_ENDPOINT"
	fileS3RegionEnv       = "MANGO_FILE_S3_REGION"
	fileS3BucketEnv       = "MANGO_FILE_S3_BUCKET"
	fileS3AccessKeyEnv    = "MANGO_FILE_S3_ACCESS_KEY"
	fileS3SecretKeyEnv    = "MANGO_FILE_S3_SECRET_KEY"
	fileS3PathStyleEnv    = "MANGO_FILE_S3_PATH_STYLE"
	fileS3CreateBucketEnv = "MANGO_FILE_S3_CREATE_BUCKET"
	fileUploadTempDirEnv  = "MANGO_FILE_UPLOAD_TEMP_DIR"

	vaultKeyringFileEnv = "MANGO_VAULT_KEYRING_FILE"
)

// resolveModelClient returns the worker model client and reports whether it is
// backed by a real, network-connected model.
func resolveModelClient() (client model.Client, realModel bool, err error) {
	if client, ok, err := model.AnthropicFromEnv(); err != nil {
		return nil, false, err
	} else if ok {
		log.Printf("runtime: agent core using real Messages API")
		return client, true, nil
	}
	log.Printf("runtime: agent core using offline fake model")
	return model.NewFake(), false, nil
}

// sandboxProviderRegistry declares the adapters compiled into this worker.
// Factories are lazy: API admission reads capabilities without contacting a
// daemon, and optional remote adapters require credentials only when selected.
func sandboxProviderRegistry() (*sandbox.ProviderRegistry, error) {
	return sandbox.NewProviderRegistry(
		sandbox.ProviderRegistration{
			Name: sandbox.DockerProviderName,
			Capabilities: sandbox.ProviderCapabilities{
				PackageSetup:    true,
				FileResources:   true,
				SessionOutputs:  true,
				SkillBundles:    true,
				MemoryStores:    true,
				GitRepositories: true,
			},
			Factory: func() (sandbox.Provider, error) {
				return sandbox.NewDockerProvider(sandbox.DockerConfig{
					DefaultImage:    os.Getenv(sandboxImageEnv),
					ResourceBaseDir: os.Getenv(sandboxResourceDirEnv),
				})
			},
		},
		sandbox.ProviderRegistration{
			Name: sandbox.E2BProviderName,
			Capabilities: sandbox.ProviderCapabilities{
				PackageSetup:    true,
				FileResources:   true,
				SessionOutputs:  true,
				SkillBundles:    true,
				GitRepositories: true,
			},
			Factory: func() (sandbox.Provider, error) {
				idleTimeout, err := envDuration(e2bIdleTimeoutEnv)
				if err != nil {
					return nil, err
				}
				return sandbox.NewE2BProvider(sandbox.E2BConfig{
					APIURL:      os.Getenv(e2bAPIURLEnv),
					APIKey:      os.Getenv(e2bAPIKeyEnv),
					TemplateID:  os.Getenv(e2bTemplateEnv),
					Domain:      os.Getenv(e2bDomainEnv),
					IdleTimeout: idleTimeout,
				})
			},
		},
		sandbox.ProviderRegistration{
			Name: sandbox.CubeProviderName,
			Capabilities: sandbox.ProviderCapabilities{
				PackageSetup:    true,
				FileResources:   true,
				SessionOutputs:  true,
				SkillBundles:    true,
				GitRepositories: true,
			},
			Factory: func() (sandbox.Provider, error) {
				proxyPort, err := envPositiveInt(cubeProxyPortEnv)
				if err != nil {
					return nil, err
				}
				idleTimeout, err := envDuration(cubeIdleTimeoutEnv)
				if err != nil {
					return nil, err
				}
				return sandbox.NewCubeProvider(sandbox.CubeConfig{
					APIURL:      os.Getenv(cubeAPIURLEnv),
					APIKey:      os.Getenv(cubeAPIKeyEnv),
					TemplateID:  os.Getenv(cubeTemplateEnv),
					Domain:      os.Getenv(cubeDomainEnv),
					ProxyNodeIP: os.Getenv(cubeProxyNodeIPEnv),
					ProxyPort:   proxyPort,
					ProxyScheme: os.Getenv(cubeProxySchemeEnv),
					IdleTimeout: idleTimeout,
				})
			},
		},
		sandbox.ProviderRegistration{
			Name: sandbox.OpenSandboxProviderName,
			Capabilities: sandbox.ProviderCapabilities{
				PackageSetup:    true,
				LimitedNetwork:  true,
				FileResources:   true,
				SessionOutputs:  true,
				SkillBundles:    true,
				GitRepositories: true,
			},
			Factory: func() (sandbox.Provider, error) {
				useProxy, err := envBool(openSandboxUseProxyEnv)
				if err != nil {
					return nil, err
				}
				return sandbox.NewOpenSandboxProvider(sandbox.OpenSandboxConfig{
					BaseURL: os.Getenv(openSandboxDomainEnv),
					APIKey:  os.Getenv(openSandboxAPIKeyEnv),
					Image: firstNonEmpty(
						os.Getenv(openSandboxImageEnv),
						os.Getenv(sandboxImageEnv),
					),
					UseProxy: useProxy,
				})
			},
		},
		sandbox.ProviderRegistration{
			Name: sandbox.DaytonaProviderName,
			Capabilities: sandbox.ProviderCapabilities{
				PackageSetup:    true,
				FileResources:   true,
				SessionOutputs:  true,
				SkillBundles:    true,
				GitRepositories: true,
			},
			Factory: func() (sandbox.Provider, error) {
				autoPauseMinutes, err := envPositiveInt(daytonaAutoPauseEnv)
				if err != nil {
					return nil, err
				}
				return sandbox.NewDaytonaProvider(sandbox.DaytonaConfig{
					APIURL:   os.Getenv(daytonaAPIURLEnv),
					APIKey:   os.Getenv(daytonaAPIKeyEnv),
					Target:   os.Getenv(daytonaTargetEnv),
					Snapshot: os.Getenv(daytonaSnapshotEnv),
					Image: firstNonEmpty(
						os.Getenv(daytonaImageEnv),
						os.Getenv(sandboxImageEnv),
					),
					AutoPauseMinutes: autoPauseMinutes,
				})
			},
		},
	)
}

func envDuration(name string) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf(
			"configuration: %s must be a positive Go duration, got %q",
			name,
			value,
		)
	}
	return parsed, nil
}

func envPositiveInt(name string) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf(
			"configuration: %s must be a positive integer, got %q",
			name,
			value,
		)
	}
	return parsed, nil
}

func envBool(name string) (bool, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return false, nil
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, fmt.Errorf(
			"configuration: %s must be a boolean, got %q",
			name,
			value,
		)
	}
	return parsed, nil
}

type resolvedFiles struct {
	service    *app.FileService
	repository *pg.FileRepository
	blobs      app.FileBlobStore
	outputs    *app.SessionOutputPublisher
}

func resolveFiles(
	ctx context.Context,
	store *pg.Store,
	ids domain.IDGenerator,
	clock domain.Clock,
	reconcile bool,
) (*resolvedFiles, error) {
	bucket := strings.TrimSpace(os.Getenv(fileS3BucketEnv))
	if bucket == "" {
		return nil, nil
	}
	pathStyle, err := envBool(fileS3PathStyleEnv)
	if err != nil {
		return nil, err
	}
	createBucket, err := envBool(fileS3CreateBucketEnv)
	if err != nil {
		return nil, err
	}
	blobs, err := blob.NewS3Store(ctx, blob.S3Config{
		Endpoint:      strings.TrimSpace(os.Getenv(fileS3EndpointEnv)),
		Region:        strings.TrimSpace(os.Getenv(fileS3RegionEnv)),
		Bucket:        bucket,
		AccessKey:     strings.TrimSpace(os.Getenv(fileS3AccessKeyEnv)),
		SecretKey:     strings.TrimSpace(os.Getenv(fileS3SecretKeyEnv)),
		UsePathStyle:  pathStyle,
		UploadTempDir: strings.TrimSpace(os.Getenv(fileUploadTempDirEnv)),
		CreateBucket:  createBucket,
	})
	if err != nil {
		return nil, err
	}
	repository := pg.NewFileRepository(store)
	files := app.NewFileService(repository, blobs, ids, clock)
	if reconcile {
		if err := files.Reconcile(ctx); err != nil {
			return nil, fmt.Errorf("files: reconcile incomplete operations: %w", err)
		}
	}
	return &resolvedFiles{
		service: files, repository: repository, blobs: blobs,
		outputs: app.NewSessionOutputPublisher(repository, blobs, ids, clock),
	}, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// resolveSandboxProvider selects the process-wide sandbox backend from the
// internal registry. Docker is the default; host-process execution is not a
// selectable backend.
// Provider choice is deployment configuration, not part of the Mango
// public API. Unknown names fail closed instead of silently falling back to
// host execution.
func resolveSandboxProvider() (sandbox.Provider, error) {
	name := configuredSandboxProviderName()
	registry, err := sandboxProviderRegistry()
	if err != nil {
		return nil, err
	}
	provider, err := registry.Open(name)
	if err != nil {
		return nil, err
	}
	log.Printf("sandbox: %s provider", name)
	return provider, nil
}

func configuredSandboxProviderName() string {
	name := strings.TrimSpace(os.Getenv(sandboxProviderEnv))
	if name == "" {
		return sandbox.DockerProviderName
	}
	return name
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// newHTTPServer builds the serving http.Server with conservative connection
// bounds. ReadHeaderTimeout guards against slow-header (Slowloris) clients,
// IdleTimeout closes idle keep-alive connections, and MaxHeaderBytes caps
// header size. There is deliberately NO global WriteTimeout: it would abort the
// long-lived SSE event stream (GET /v1/sessions/{id}/events/stream with
// text/event-stream), so per-response deadlines belong at the handler layer.
func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MiB
	}
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: mango <serve|orchestrate|workspace|api-key> [flags]")
	}
	switch os.Args[1] {
	case "serve":
		runServe()
	case "orchestrate":
		runOrchestrate()
	case "workspace":
		runWorkspaceCommand()
	case "api-key":
		runAPIKeyCommand()
	default:
		log.Fatal("usage: mango <serve|orchestrate|workspace|api-key> [flags]")
	}
}

func runServe() {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", defaultAddr, "listen address (default binds to loopback; use e.g. :8080 to expose on all interfaces)")
	_ = fs.Parse(os.Args[2:])

	runPostgresAPI(*addr, httpapi.Config{})
}

func runWorkspaceCommand() {
	if len(os.Args) < 3 {
		log.Fatal("usage: mango workspace <create|list> [flags]")
	}
	switch os.Args[2] {
	case "create":
		fs := flag.NewFlagSet("workspace create", flag.ExitOnError)
		name := fs.String("name", "", "workspace display name")
		_ = fs.Parse(os.Args[3:])
		if err := withOperatorStore(func(ctx context.Context, store *pg.Store) error {
			item, err := store.CreateWorkspace(ctx, *name)
			if err != nil {
				return err
			}
			fmt.Printf("%s\t%s\n", item.ID, item.Name)
			return nil
		}); err != nil {
			log.Fatalf("workspace create: %v", err)
		}
	case "list":
		fs := flag.NewFlagSet("workspace list", flag.ExitOnError)
		_ = fs.Parse(os.Args[3:])
		if err := withOperatorStore(func(ctx context.Context, store *pg.Store) error {
			items, err := store.ListWorkspaces(ctx)
			if err != nil {
				return err
			}
			for _, item := range items {
				fmt.Printf("%s\t%s\t%s\n", item.ID, item.Name, item.CreatedAt.Format(time.RFC3339))
			}
			return nil
		}); err != nil {
			log.Fatalf("workspace list: %v", err)
		}
	default:
		log.Fatal("usage: mango workspace <create|list> [flags]")
	}
}

func runAPIKeyCommand() {
	if len(os.Args) < 3 {
		log.Fatal("usage: mango api-key <create|list|revoke> [flags]")
	}
	switch os.Args[2] {
	case "create":
		fs := flag.NewFlagSet("api-key create", flag.ExitOnError)
		workspaceID := fs.String("workspace", "", "workspace ID")
		label := fs.String("label", "", "operator-visible key label")
		_ = fs.Parse(os.Args[3:])
		if strings.TrimSpace(*workspaceID) == "" {
			log.Fatal("api-key create: -workspace is required")
		}
		if err := withOperatorStore(func(ctx context.Context, store *pg.Store) error {
			item, secret, err := store.CreateAPIKey(ctx, *workspaceID, *label)
			if err != nil {
				return err
			}
			// The plaintext secret is intentionally emitted only at creation.
			fmt.Printf("id\t%s\nworkspace\t%s\napi_key\t%s\n", item.ID, item.WorkspaceID, secret)
			return nil
		}); err != nil {
			log.Fatalf("api-key create: %v", err)
		}
	case "list":
		fs := flag.NewFlagSet("api-key list", flag.ExitOnError)
		workspaceID := fs.String("workspace", "", "workspace ID")
		_ = fs.Parse(os.Args[3:])
		if strings.TrimSpace(*workspaceID) == "" {
			log.Fatal("api-key list: -workspace is required")
		}
		if err := withOperatorStore(func(ctx context.Context, store *pg.Store) error {
			items, err := store.ListAPIKeys(ctx, *workspaceID)
			if err != nil {
				return err
			}
			for _, item := range items {
				status := "active"
				if item.RevokedAt != nil {
					status = "revoked"
				}
				fmt.Printf("%s\t%s\t%s\t%s\n", item.ID, item.Label, status, item.CreatedAt.Format(time.RFC3339))
			}
			return nil
		}); err != nil {
			log.Fatalf("api-key list: %v", err)
		}
	case "revoke":
		fs := flag.NewFlagSet("api-key revoke", flag.ExitOnError)
		id := fs.String("id", "", "API key ID")
		_ = fs.Parse(os.Args[3:])
		if strings.TrimSpace(*id) == "" {
			log.Fatal("api-key revoke: -id is required")
		}
		if err := withOperatorStore(func(ctx context.Context, store *pg.Store) error {
			return store.RevokeAPIKey(ctx, *id)
		}); err != nil {
			log.Fatalf("api-key revoke: %v", err)
		}
		fmt.Printf("revoked\t%s\n", *id)
	default:
		log.Fatal("usage: mango api-key <create|list|revoke> [flags]")
	}
}

func withOperatorStore(run func(context.Context, *pg.Store) error) error {
	databaseURL := strings.TrimSpace(os.Getenv(envDatabaseURL))
	if databaseURL == "" {
		return fmt.Errorf("%s is required", envDatabaseURL)
	}
	ctx := context.Background()
	pool, err := pg.Pool(ctx, databaseURL)
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer pool.Close()
	if err := pg.Migrate(ctx, pool); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return run(ctx, pg.NewSystemStore(pool, domain.NewRandomIDGen(), realClock{}))
}

func runPostgresAPI(addr string, cfg httpapi.Config) {
	databaseURL := os.Getenv(envDatabaseURL)
	if databaseURL == "" {
		log.Fatalf("serve: %s is required", envDatabaseURL)
	}
	ctx := context.Background()
	pool, err := pg.Pool(ctx, databaseURL)
	if err != nil {
		log.Fatalf("serve: postgres: %v", err)
	}
	defer pool.Close()
	if err := pg.Migrate(ctx, pool); err != nil {
		log.Fatalf("serve: migrate: %v", err)
	}

	ids := domain.NewRandomIDGen()
	clock := realClock{}
	pgStore := pg.NewStore(pool, ids, clock)
	if bootstrapKey := strings.TrimSpace(os.Getenv(envAPIKey)); bootstrapKey != "" {
		if err := pgStore.BootstrapAPIKey(ctx, bootstrapKey); err != nil {
			log.Fatalf("serve: bootstrap API key: %v", err)
		}
	}
	keyCount, err := pgStore.CountActiveAPIKeys(ctx)
	if err != nil {
		log.Fatalf("serve: count API keys: %v", err)
	}
	if keyCount == 0 {
		log.Fatalf("serve: no active API key; set %s or run mango api-key create", envAPIKey)
	}
	cfg.Authenticator = pgStore
	systemStore := pg.NewSystemStore(pool, ids, clock)
	memory := app.NewMemoryService(pg.NewMemoryRepository(pgStore), ids, clock)
	secretCipher, err := resolveSecretCipher()
	if err != nil {
		log.Fatalf("serve: secret keyring: %v", err)
	}
	vaults := resolveVaultService(pgStore, secretCipher, ids, clock)
	var webhooks *app.WebhookService
	if secretCipher != nil {
		webhooks = app.NewWebhookService(
			pg.NewWebhookRepository(pgStore), secretCipher, ids, clock,
		)
	}
	if vaults == nil {
		log.Printf("serve: Vault and Webhook APIs disabled; %s is not configured", vaultKeyringFileEnv)
	} else {
		log.Printf("serve: encrypted Vault and Webhook control planes enabled")
	}
	broker, err := live.Connect(os.Getenv(envNATSURL))
	if err != nil {
		log.Fatalf("serve: nats: %v", err)
	}
	defer broker.Close()
	pgStore.SetEventNotifier(broker)
	agentsRepo := pg.NewAgentRepository(pgStore)
	environmentsRepo := pg.NewEnvironmentRepository(pgStore)
	providerRegistry, err := sandboxProviderRegistry()
	if err != nil {
		log.Fatalf("serve: sandbox registry: %v", err)
	}
	providerCapabilities, err := providerRegistry.Capabilities(configuredSandboxProviderName())
	if err != nil {
		log.Fatalf("serve: sandbox: %v", err)
	}
	environments := app.NewEnvironmentService(
		environmentsRepo,
		ids,
		clock,
		app.EnvironmentCapabilities{
			PackageSetup:   providerCapabilities.PackageSetup,
			LimitedNetwork: providerCapabilities.LimitedNetwork,
		},
	)
	fileRuntime, err := resolveFiles(ctx, pgStore, ids, clock, false)
	if err != nil {
		log.Printf("serve: Files API disabled: %v", err)
		fileRuntime = nil
	} else if fileRuntime == nil {
		log.Printf("serve: Files API disabled; %s is not configured", fileS3BucketEnv)
	} else {
		fileReconciler := app.NewFileService(
			pg.NewFileRepository(systemStore), fileRuntime.blobs, ids, clock,
		)
		if err := fileReconciler.Reconcile(ctx); err != nil {
			log.Printf("serve: Files API disabled: reconcile incomplete operations: %v", err)
			fileRuntime = nil
		} else {
			log.Printf("serve: Files API object store connected and reconciled")
		}
	}
	var files *app.FileService
	var skills *app.SkillService
	var sessionResources *controlplane.SessionResourceService
	var sessionResourceLifecycle *controlplane.SessionResourceService
	if fileRuntime != nil {
		files = fileRuntime.service
		skills = app.NewSkillService(
			pg.NewSkillRepository(pgStore), fileRuntime.blobs, ids, clock,
		)
		skillReconciler := app.NewSkillService(
			pg.NewSkillRepository(systemStore), fileRuntime.blobs, ids, clock,
		)
		if err := skillReconciler.Reconcile(ctx); err != nil {
			log.Printf("serve: Skills API disabled: reconcile incomplete operations: %v", err)
			skills = nil
		} else {
			log.Printf("serve: Skills API object store connected and reconciled")
		}
		sessionResourceLifecycle = controlplane.NewSessionResourceService(
			pgStore, fileRuntime.repository, fileRuntime.blobs, ids, clock,
			providerCapabilities.FileResources,
		)
		if providerCapabilities.GitRepositories {
			sessionResourceLifecycle.EnableGitRepositoryResources(
				gitrepo.NewSnapshotter(os.Getenv(fileUploadTempDirEnv)),
			)
		}
		sessionResources = sessionResourceLifecycle
		if !providerCapabilities.FileResources {
			log.Printf(
				"serve: Session File Resource admission disabled; sandbox provider %q has no materialization capability; existing resources remain readable and detachable",
				configuredSandboxProviderName(),
			)
		}
	}
	var skillResolver app.SkillReferenceResolver
	if skills != nil {
		skillResolver = skills
	}
	if skills != nil && !providerCapabilities.SkillBundles {
		log.Printf(
			"serve: custom Skill storage remains available; Session Skill admission disabled because sandbox provider %q cannot materialize Skills; external worker Skill activation is not supported",
			configuredSandboxProviderName(),
		)
	}
	agents := app.NewAgentService(agentsRepo, ids, clock, skillResolver)

	temporalClient, err := temporalpkg.Dial(temporalpkg.ClientConfig{
		HostPort:  os.Getenv(envTemporalHostPort),
		Namespace: os.Getenv(envTemporalNamespace),
	})
	if err != nil {
		log.Fatalf("serve: temporal: %v", err)
	}
	defer temporalClient.Close()
	// Event admission remains correct through a Temporal outage because it
	// commits the PostgreSQL outbox first and treats the direct signal as a
	// best-effort latency path. Lifecycle operations such as physical deletion
	// use the client to stop the Workflow before removing its projection.
	orchestrator := temporalpkg.NewOrchestrator(
		pgStore,
		temporalpkg.NewSignaler(temporalClient),
	)
	sessions := controlplane.NewSessionService(
		pgStore, agentsRepo, environmentsRepo, orchestrator, ids, clock,
		skillResolver,
		sessionResourceLifecycle,
	)
	if files != nil {
		sessions.EnableFileOutcomeRubrics(files)
		sessions.EnableFileMessageContent(files)
	}
	sessions.ConfigureCloudSkillBundles(providerCapabilities.SkillBundles)
	if vaults != nil {
		sessions.EnableVaults()
	}
	if providerCapabilities.MemoryStores {
		sessions.EnableMemoryStoreResources(memory)
	} else {
		log.Printf(
			"serve: Session Memory Store admission disabled; sandbox provider %q has no durable Memory Store mount capability",
			configuredSandboxProviderName(),
		)
	}
	var deploymentFiles app.DeploymentFileReader
	if files != nil && providerCapabilities.FileResources {
		deploymentFiles = files
	}
	var deploymentMemory app.DeploymentMemoryReader
	if providerCapabilities.MemoryStores {
		deploymentMemory = memory
	}
	deployments := app.NewDeploymentService(app.DeploymentServiceConfig{
		Repository: pg.NewDeploymentRepository(pgStore),
		Agents:     agentsRepo, Environments: environmentsRepo, Sessions: sessions,
		Files: deploymentFiles, Memory: deploymentMemory, Vaults: vaults,
		IDGenerator: ids, Clock: clock,
	})
	environmentWork := app.NewEnvironmentWorkService(
		pg.NewEnvironmentWorkRepository(pgStore), environmentsRepo,
	)
	events := controlplane.NewEventService(pgStore)
	threads := controlplane.NewSessionThreadService(pgStore)
	stream := live.NewStream(pgStore, broker, ids, clock, 0)
	handler := httpapi.NewServer(httpapi.Deps{
		Agents: agents, Envs: environments, Sessions: sessions,
		Threads: threads, Events: events, Stream: stream, Files: files, Skills: skills, Memory: memory,
		Vaults: vaults, Webhooks: webhooks, Deployments: deployments, EnvironmentWork: environmentWork,
		SessionResources: sessionResources,
	}, cfg).Handler()
	log.Printf("serve: PostgreSQL control plane, Temporal client, and NATS live channel connected")
	serveHTTP(addr, handler)
}

func resolveSecretCipher() (secretcrypto.Cipher, error) {
	keyringPath := strings.TrimSpace(os.Getenv(vaultKeyringFileEnv))
	if keyringPath == "" {
		return nil, nil
	}
	return secretcrypto.LoadAESGCMKeyringFile(keyringPath)
}

func resolveVaultService(
	store *pg.Store,
	cipher secretcrypto.Cipher,
	ids domain.IDGenerator,
	clock domain.Clock,
) *app.VaultService {
	if cipher == nil {
		return nil
	}
	return app.NewVaultService(app.VaultServiceConfig{
		Repository: pg.NewVaultRepository(store),
		Cipher:     cipher, IDGenerator: ids, Clock: clock,
		OAuthRefresher: oauthclient.New(nil),
		MCPValidator:   mcpclient.NewRemote(nil),
	})
}

func serveHTTP(addr string, handler http.Handler) {
	srv := newHTTPServer(addr, handler)
	go func() {
		log.Printf("listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
