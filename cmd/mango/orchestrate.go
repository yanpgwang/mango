package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/controlplane"
	"github.com/yanpgwang/mango/internal/credentialruntime"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpegress"
	"github.com/yanpgwang/mango/internal/live"
	"github.com/yanpgwang/mango/internal/pg"
	"github.com/yanpgwang/mango/internal/sandbox"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
)

type unavailableSessionResourceReconciler struct {
	store  *pg.Store
	cause  error
	memory *app.SessionMemoryMaterializer
}

type retryingSessionResourceReconciler struct {
	store   *pg.Store
	resolve func(context.Context) (*resolvedFiles, error)

	mu             sync.Mutex
	materializer   *app.SessionRuntimeMaterializer
	memory         *app.SessionMemoryMaterializer
	sessionOutputs bool
}

func (r *retryingSessionResourceReconciler) resolveMaterializer(
	ctx context.Context,
) (*app.SessionRuntimeMaterializer, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.materializer != nil {
		return r.materializer, nil
	}
	runtime, err := r.resolve(ctx)
	if err != nil {
		return nil, err
	}
	if runtime == nil {
		return nil, sandbox.Permanent(errors.New(
			fileS3BucketEnv + " is not configured",
		))
	}
	r.materializer = app.NewSessionRuntimeMaterializer(
		app.NewSessionResourceMaterializer(
			r.store, runtime.repository, runtime.blobs,
		),
		app.NewSessionSkillMaterializer(r.store, runtime.blobs),
		r.memory,
	).WithSessionOutputPublisher(runtime.outputs)
	return r.materializer, nil
}

func (r *retryingSessionResourceReconciler) SupportsSessionOutputs() bool {
	return r.sessionOutputs
}

func (r *retryingSessionResourceReconciler) PublishSessionOutputs(
	ctx context.Context,
	sessionID string,
	box sandbox.Sandbox,
) error {
	if !r.sessionOutputs {
		return sandbox.Permanent(errors.New("session output publication is disabled"))
	}
	materializer, err := r.resolveMaterializer(ctx)
	if err != nil {
		return err
	}
	return materializer.PublishSessionOutputs(ctx, sessionID, box)
}

func (r *retryingSessionResourceReconciler) Reconcile(
	ctx context.Context,
	sessionID string,
	box sandbox.Sandbox,
) error {
	resources, err := r.store.SessionResourcesForReconcile(ctx, sessionID)
	if err != nil {
		return err
	}
	skills, err := r.store.SessionSkillsForRuntime(ctx, sessionID)
	if err != nil || len(resources) == 0 && len(skills) == 0 {
		return err
	}
	needsObjectStore := len(skills) > 0
	for _, resource := range resources {
		if resource.Type() != domain.SessionResourceTypeMemoryStore {
			needsObjectStore = true
			break
		}
	}
	if !needsObjectStore {
		if r.memory == nil {
			return nil
		}
		return r.memory.Reconcile(ctx, sessionID, box)
	}
	materializer, err := r.resolveMaterializer(ctx)
	if err != nil {
		return err
	}
	return materializer.Reconcile(ctx, sessionID, box)
}

func (r *retryingSessionResourceReconciler) ReconcileThread(
	ctx context.Context,
	sessionID string,
	threadID string,
	box sandbox.Sandbox,
) error {
	resources, err := r.store.SessionResourcesForReconcile(ctx, sessionID)
	if err != nil {
		return err
	}
	runtime, err := r.store.SessionThreadSkillRuntime(ctx, sessionID, threadID)
	if err != nil || len(resources) == 0 && len(runtime.Versions) == 0 {
		return err
	}
	needsObjectStore := len(runtime.Versions) > 0
	for _, resource := range resources {
		if resource.Type() != domain.SessionResourceTypeMemoryStore {
			needsObjectStore = true
			break
		}
	}
	if !needsObjectStore {
		if r.memory == nil {
			return nil
		}
		return r.memory.Reconcile(ctx, sessionID, box)
	}
	materializer, err := r.resolveMaterializer(ctx)
	if err != nil {
		return err
	}
	return materializer.ReconcileThread(ctx, sessionID, threadID, box)
}

func (r *retryingSessionResourceReconciler) Writeback(
	ctx context.Context,
	sessionID string,
	box sandbox.Sandbox,
) error {
	if r.memory == nil {
		return nil
	}
	return r.memory.Writeback(ctx, sessionID, box)
}

func (r *retryingSessionResourceReconciler) WritebackForRelease(
	ctx context.Context,
	sessionID string,
	box sandbox.Sandbox,
) error {
	if r.memory == nil {
		return nil
	}
	return r.memory.WritebackForRelease(ctx, sessionID, box)
}

func (r *retryingSessionResourceReconciler) MemoryStoreMountsForRelease(
	ctx context.Context,
	sessionID string,
) ([]sandbox.MemoryStoreMount, error) {
	if r.memory == nil {
		return nil, nil
	}
	return r.memory.MemoryStoreMountsForRelease(ctx, sessionID)
}

func (r *retryingSessionResourceReconciler) CleanupSession(
	ctx context.Context,
	sessionID string,
) error {
	resources, err := r.store.SessionResourcesForReconcile(ctx, sessionID)
	if err != nil {
		return err
	}
	hasOutputs, err := r.store.SessionOutputFilesExist(ctx, sessionID)
	if err != nil {
		return err
	}
	needsObjectStore := hasOutputs
	for _, resource := range resources {
		if resource.Type() != domain.SessionResourceTypeMemoryStore {
			needsObjectStore = true
			break
		}
	}
	if !needsObjectStore {
		if r.memory == nil {
			return nil
		}
		return r.memory.CleanupSession(ctx, sessionID)
	}
	materializer, err := r.resolveMaterializer(ctx)
	if err != nil {
		return err
	}
	return materializer.CleanupSession(ctx, sessionID)
}

func (r unavailableSessionResourceReconciler) Reconcile(
	ctx context.Context,
	sessionID string,
	box sandbox.Sandbox,
) error {
	resources, err := r.store.SessionResourcesForReconcile(ctx, sessionID)
	if err != nil {
		return err
	}
	skills, err := r.store.SessionSkillsForRuntime(ctx, sessionID)
	if err != nil || len(resources) == 0 && len(skills) == 0 {
		return err
	}
	if r.memory != nil {
		if err := r.memory.Reconcile(ctx, sessionID, box); err != nil {
			return err
		}
	}
	needsObjectStore := len(skills) > 0
	for _, resource := range resources {
		if resource.Type() != domain.SessionResourceTypeMemoryStore {
			needsObjectStore = true
			break
		}
	}
	if !needsObjectStore {
		return nil
	}
	return sandbox.Permanent(fmt.Errorf(
		"session File/Git Resources or custom Skills are unavailable on this worker: %w",
		r.cause,
	))
}

func (r unavailableSessionResourceReconciler) ReconcileThread(
	ctx context.Context,
	sessionID string,
	threadID string,
	box sandbox.Sandbox,
) error {
	resources, err := r.store.SessionResourcesForReconcile(ctx, sessionID)
	if err != nil {
		return err
	}
	runtime, err := r.store.SessionThreadSkillRuntime(ctx, sessionID, threadID)
	if err != nil || len(resources) == 0 && len(runtime.Versions) == 0 {
		return err
	}
	if r.memory != nil {
		if err := r.memory.Reconcile(ctx, sessionID, box); err != nil {
			return err
		}
	}
	needsObjectStore := len(runtime.Versions) > 0
	for _, resource := range resources {
		if resource.Type() != domain.SessionResourceTypeMemoryStore {
			needsObjectStore = true
			break
		}
	}
	if !needsObjectStore {
		return nil
	}
	return sandbox.Permanent(fmt.Errorf(
		"session File/Git Resources or custom Skills are unavailable on this worker: %w",
		r.cause,
	))
}

func (r unavailableSessionResourceReconciler) Writeback(
	ctx context.Context,
	sessionID string,
	box sandbox.Sandbox,
) error {
	if r.memory == nil {
		return nil
	}
	return r.memory.Writeback(ctx, sessionID, box)
}

func (r unavailableSessionResourceReconciler) WritebackForRelease(
	ctx context.Context,
	sessionID string,
	box sandbox.Sandbox,
) error {
	if r.memory == nil {
		return nil
	}
	return r.memory.WritebackForRelease(ctx, sessionID, box)
}

func (r unavailableSessionResourceReconciler) MemoryStoreMountsForRelease(
	ctx context.Context,
	sessionID string,
) ([]sandbox.MemoryStoreMount, error) {
	if r.memory == nil {
		return nil, nil
	}
	return r.memory.MemoryStoreMountsForRelease(ctx, sessionID)
}

func (r unavailableSessionResourceReconciler) CleanupSession(
	ctx context.Context,
	sessionID string,
) error {
	resources, err := r.store.SessionResourcesForReconcile(ctx, sessionID)
	if err != nil {
		return err
	}
	hasOutputs, err := r.store.SessionOutputFilesExist(ctx, sessionID)
	if err != nil {
		return err
	}
	if len(resources) == 0 && !hasOutputs {
		return nil
	}
	if r.memory != nil {
		if err := r.memory.CleanupSession(ctx, sessionID); err != nil {
			return err
		}
	}
	if hasOutputs {
		return fmt.Errorf(
			"session output Files are unavailable on this worker: %w",
			r.cause,
		)
	}
	for _, resource := range resources {
		if resource.Type() != domain.SessionResourceTypeMemoryStore {
			return fmt.Errorf(
				"session File/Git Resources are unavailable on this worker: %w",
				r.cause,
			)
		}
	}
	return nil
}

// Environment variables shared by the PostgreSQL HTTP control plane and the
// Temporal execution worker.
const (
	envDatabaseURL       = "MANGO_DATABASE_URL"
	envTemporalHostPort  = "MANGO_TEMPORAL_HOSTPORT"
	envTemporalNamespace = "MANGO_TEMPORAL_NAMESPACE"
	envNATSURL           = "MANGO_NATS_URL"
	envAPIKey            = "MANGO_API_KEY"
)

// runOrchestrate boots the Temporal execution role: it runs PostgreSQL
// migrations, starts the SessionWorkflow worker, and runs the outbox relay.
// HTTP is served by a separate `serve` process so API and worker capacity can be
// scaled independently.
func runOrchestrate() {
	databaseURL := os.Getenv(envDatabaseURL)
	if databaseURL == "" {
		log.Fatalf("orchestrate: %s is required", envDatabaseURL)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pg.Pool(ctx, databaseURL)
	if err != nil {
		log.Fatalf("orchestrate: postgres: %v", err)
	}
	defer pool.Close()
	if err := pg.Migrate(ctx, pool); err != nil {
		log.Fatalf("orchestrate: migrate: %v", err)
	}
	log.Printf("orchestrate: postgres connected and migrated")

	ids := domain.NewRandomIDGen()
	store := pg.NewSystemStore(pool, ids, realClock{})
	secretCipher, err := resolveSecretCipher()
	if err != nil {
		log.Fatalf("orchestrate: secret keyring: %v", err)
	}
	vaults := resolveVaultService(store, secretCipher, ids, realClock{})
	var mcpAuth credentialruntime.AuthSource
	if vaults == nil {
		mcpAuth = app.NewUnavailableVaultAuthSource(pg.NewVaultRepository(store))
		log.Printf("orchestrate: Vault-backed MCP authentication disabled; %s is not configured", vaultKeyringFileEnv)
	} else {
		mcpAuth = vaults
		log.Printf("orchestrate: Vault-backed MCP authentication enabled")
	}
	memory := app.NewMemoryService(pg.NewMemoryRepository(store), ids, realClock{})
	memoryMaterializer := app.NewSessionMemoryMaterializer(store, memory)
	broker, err := live.Connect(os.Getenv(envNATSURL))
	if err != nil {
		log.Fatalf("orchestrate: nats: %v", err)
	}
	defer broker.Close()
	store.SetEventNotifier(broker)
	log.Printf("orchestrate: NATS live channel connected")

	// Workflow executions call the selected model through granular model/tool
	// Activities. The offline fake model needs no configuration.
	modelClient, _, err := resolveModelClient()
	if err != nil {
		log.Fatalf("orchestrate: runtime: %v", err)
	}
	provider, err := resolveSandboxProvider()
	if err != nil {
		log.Fatalf("orchestrate: sandbox: %v", err)
	}
	providerRegistry, err := sandboxProviderRegistry()
	if err != nil {
		log.Fatalf("orchestrate: sandbox registry: %v", err)
	}
	providerCapabilities, err := providerRegistry.Capabilities(configuredSandboxProviderName())
	if err != nil {
		log.Fatalf("orchestrate: sandbox capabilities: %v", err)
	}
	fileRuntime, err := resolveFiles(ctx, store, ids, realClock{}, false)
	var resourceReconciler temporalpkg.SandboxResourceReconciler
	switch {
	case err != nil:
		log.Printf("orchestrate: object store unavailable; File/Skill turns will retry connection: %v", err)
		resourceReconciler = &retryingSessionResourceReconciler{
			store:          store,
			memory:         memoryMaterializer,
			sessionOutputs: providerCapabilities.SessionOutputs,
			resolve: func(resolveCtx context.Context) (*resolvedFiles, error) {
				return resolveFiles(resolveCtx, store, ids, realClock{}, false)
			},
		}
	case fileRuntime == nil:
		cause := errors.New(fileS3BucketEnv + " is not configured")
		resourceReconciler = unavailableSessionResourceReconciler{
			store: store, cause: cause, memory: memoryMaterializer,
		}
		log.Printf("orchestrate: Session File Resources and custom Skill runtime disabled: %v", cause)
	default:
		materializer := app.NewSessionRuntimeMaterializer(
			app.NewSessionResourceMaterializer(
				store, fileRuntime.repository, fileRuntime.blobs,
			),
			app.NewSessionSkillMaterializer(store, fileRuntime.blobs),
			memoryMaterializer,
		)
		materializer.WithSessionOutputPublisher(fileRuntime.outputs)
		resourceReconciler = materializer
		log.Printf("orchestrate: Session File Resource, output, and custom Skill materializers enabled")
	}

	client, err := temporalpkg.Dial(temporalpkg.ClientConfig{
		HostPort:  os.Getenv(envTemporalHostPort),
		Namespace: os.Getenv(envTemporalNamespace),
	})
	if err != nil {
		log.Fatalf("orchestrate: temporal: %v", err)
	}
	defer client.Close()
	log.Printf("orchestrate: temporal connected")

	runtime := temporalpkg.NewRuntime(temporalpkg.RuntimeConfig{
		TemporalClient:   client,
		Store:            store,
		ModelClient:      modelClient,
		SandboxProvider:  provider,
		IDGenerator:      ids,
		RelayConfig:      temporalpkg.RelayConfig{},
		Resources:        resourceReconciler,
		MCPAuth:          mcpAuth,
		PreviewPublisher: broker,
	})

	agentsRepo := pg.NewAgentRepository(store)
	environmentsRepo := pg.NewEnvironmentRepository(store)
	var skillResolver app.SkillReferenceResolver
	var sessionResources *controlplane.SessionResourceService
	if fileRuntime != nil {
		skills := app.NewSkillService(
			pg.NewSkillRepository(store), fileRuntime.blobs, ids, realClock{},
		)
		skillResolver = skills
		sessionResources = controlplane.NewSessionResourceService(
			store, fileRuntime.repository, fileRuntime.blobs, ids, realClock{},
			providerCapabilities.FileResources,
		)
	}
	deploymentSessions := controlplane.NewSessionService(
		store, agentsRepo, environmentsRepo, runtime.Orchestrator(), ids,
		realClock{}, skillResolver, sessionResources,
	)
	if fileRuntime != nil {
		deploymentSessions.EnableFileOutcomeRubrics(fileRuntime.service)
		deploymentSessions.EnableFileMessageContent(fileRuntime.service)
	}
	deploymentSessions.ConfigureCloudSkillBundles(providerCapabilities.SkillBundles)
	if vaults != nil {
		deploymentSessions.EnableVaults()
	}
	if providerCapabilities.MemoryStores {
		deploymentSessions.EnableMemoryStoreResources(memory)
	}
	var deploymentFiles app.DeploymentFileReader
	if fileRuntime != nil && providerCapabilities.FileResources {
		deploymentFiles = fileRuntime.service
	}
	var deploymentMemory app.DeploymentMemoryReader
	if providerCapabilities.MemoryStores {
		deploymentMemory = memory
	}
	deployments := app.NewDeploymentService(app.DeploymentServiceConfig{
		Repository: pg.NewDeploymentRepository(store),
		Agents:     agentsRepo, Environments: environmentsRepo, Sessions: deploymentSessions,
		Files: deploymentFiles, Memory: deploymentMemory, Vaults: vaults,
		IDGenerator: ids, Clock: realClock{},
	})
	deploymentReconciler := app.NewDeploymentReconciler(deployments)
	var webhookDispatcher *app.WebhookDispatcher
	if secretCipher != nil {
		webhookClient := httpegress.NewPublicClient(app.DefaultWebhookHTTPTimeout)
		webhookClient.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
		webhookDispatcher = app.NewWebhookDispatcher(
			pg.NewWebhookRepository(store), secretCipher, webhookClient, ids, realClock{},
		)
	} else {
		log.Printf("orchestrate: Webhook delivery disabled; %s is not configured", vaultKeyringFileEnv)
	}

	if err := runtime.Worker.Start(); err != nil {
		log.Fatalf("orchestrate: worker start: %v", err)
	}
	defer runtime.Worker.Stop()
	log.Printf("orchestrate: session worker started on task queue %s", temporalpkg.TaskQueue)

	relayErr := make(chan error, 1)
	go func() { relayErr <- runtime.Relay.Run(ctx) }()
	log.Printf("orchestrate: outbox relay running")
	lifecycleErr := make(chan error, 1)
	go func() { lifecycleErr <- runtime.Lifecycle.Run(ctx) }()
	log.Printf("orchestrate: sandbox and deletion lifecycle reconciler running")
	deploymentErr := make(chan error, 1)
	go func() { deploymentErr <- deploymentReconciler.Run(ctx) }()
	log.Printf("orchestrate: scheduled Deployment reconciler running")
	var webhookErr <-chan error
	if webhookDispatcher != nil {
		channel := make(chan error, 1)
		webhookErr = channel
		go func() { channel <- webhookDispatcher.Run(ctx) }()
		log.Printf("orchestrate: durable Webhook dispatcher running")
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-stop:
		log.Printf("orchestrate: shutting down")
	case err := <-relayErr:
		if err != nil && ctx.Err() == nil {
			log.Printf("orchestrate: relay stopped: %v", err)
		}
	case err := <-lifecycleErr:
		if err != nil && ctx.Err() == nil {
			log.Printf("orchestrate: lifecycle reconciler stopped: %v", err)
		}
	case err := <-deploymentErr:
		if err != nil && ctx.Err() == nil {
			log.Printf("orchestrate: scheduled Deployment reconciler stopped: %v", err)
		}
	case err := <-webhookErr:
		if err != nil && ctx.Err() == nil {
			log.Printf("orchestrate: Webhook dispatcher stopped: %v", err)
		}
	}
	cancel()
}
