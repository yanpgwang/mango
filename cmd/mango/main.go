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
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/live"
	"github.com/yanpgwang/mango/internal/mcpclient"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/oauthclient"
	"github.com/yanpgwang/mango/internal/pg"
	"github.com/yanpgwang/mango/internal/secretcrypto"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
)

// defaultAddr binds to loopback by default so a fresh `serve` never exposes the
// unauthenticated API on all interfaces. Operators who want a public bind must
// pass -addr explicitly (e.g. -addr :8080).
const defaultAddr = "127.0.0.1:8080"

const (
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
	service *app.FileService
	blobs   app.FileBlobStore
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
		service: files, blobs: blobs,
	}, nil
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
	environments := app.NewEnvironmentService(environmentsRepo, ids, clock)
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
	}
	var skillResolver app.SkillReferenceResolver
	if skills != nil {
		skillResolver = skills
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
	)
	if files != nil {
		sessions.EnableFileOutcomeRubrics(files)
		sessions.EnableFileMessageContent(files)
	}
	sessions.EnableMemoryStoreResources(memory)
	if vaults != nil {
		sessions.EnableVaults()
	}
	deployments := app.NewDeploymentService(app.DeploymentServiceConfig{
		Repository: pg.NewDeploymentRepository(pgStore),
		Agents:     agentsRepo, Environments: environmentsRepo, Sessions: sessions,
		Memory: memory, Vaults: vaults,
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
