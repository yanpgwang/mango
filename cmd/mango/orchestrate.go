package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/controlplane"
	"github.com/yanpgwang/mango/internal/credentialruntime"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpegress"
	"github.com/yanpgwang/mango/internal/live"
	"github.com/yanpgwang/mango/internal/pg"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
)

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
	fileRuntime, err := resolveFiles(ctx, store, ids, realClock{}, false)
	if err != nil {
		log.Printf("orchestrate: Files and custom Skills disabled: %v", err)
		fileRuntime = nil
	}
	var skillInstructions temporalpkg.SkillInstructionLoader
	if fileRuntime != nil {
		skillInstructions = app.NewSessionSkillMaterializer(store, fileRuntime.blobs)
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
		TemporalClient:    client,
		Store:             store,
		ModelClient:       modelClient,
		IDGenerator:       ids,
		RelayConfig:       temporalpkg.RelayConfig{},
		SkillInstructions: skillInstructions,
		MCPAuth:           mcpAuth,
		PreviewPublisher:  broker,
	})

	agentsRepo := pg.NewAgentRepository(store)
	environmentsRepo := pg.NewEnvironmentRepository(store)
	var skillResolver app.SkillReferenceResolver
	if fileRuntime != nil {
		skills := app.NewSkillService(
			pg.NewSkillRepository(store), fileRuntime.blobs, ids, realClock{},
		)
		skillResolver = skills
	}
	deploymentSessions := controlplane.NewSessionService(
		store, agentsRepo, environmentsRepo, runtime.Orchestrator(), ids,
		realClock{}, skillResolver,
	)
	if fileRuntime != nil {
		deploymentSessions.EnableFileOutcomeRubrics(fileRuntime.service)
		deploymentSessions.EnableFileMessageContent(fileRuntime.service)
	}
	deploymentSessions.EnableMemoryStoreResources(memory)
	if vaults != nil {
		deploymentSessions.EnableVaults()
	}
	deployments := app.NewDeploymentService(app.DeploymentServiceConfig{
		Repository: pg.NewDeploymentRepository(store),
		Agents:     agentsRepo, Environments: environmentsRepo, Sessions: deploymentSessions,
		Memory: memory, Vaults: vaults,
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
	log.Printf("orchestrate: deletion lifecycle reconciler running")
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
