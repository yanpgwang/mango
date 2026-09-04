package temporal

import (
	"context"
	"errors"
	"log"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/credentialruntime"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
	"github.com/yanpgwang/mango/internal/sandbox"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// Orchestrator bridges PostgreSQL admission to Temporal. It makes a best-effort
// post-commit fast-path signal to reduce latency and relies on the durable
// outbox relay for eventual, correct delivery.
type Orchestrator struct {
	store    *pg.Store
	signaler *Signaler
}

func NewOrchestrator(store *pg.Store, signaler *Signaler) *Orchestrator {
	return &Orchestrator{store: store, signaler: signaler}
}

func (o *Orchestrator) fastPathWake(
	ctx context.Context,
	sessionID string,
	adm pg.Admission,
) {
	if o.signaler == nil {
		return
	}
	if adm.PrimaryEnqueued {
		if err := o.signaler.Wake(ctx, sessionID, adm.MaxSeq); err != nil {
			log.Printf(
				"orchestrator: fast-path signal failed session_id=%s (relay will deliver): %v",
				sessionID, err,
			)
		}
	}
	for _, threadID := range adm.WakeThreadIDs {
		if err := o.signaler.WakeThread(ctx, sessionID, threadID, adm.MaxSeq); err != nil {
			log.Printf(
				"orchestrator: fast-path thread signal failed session_id=%s thread_id=%s (relay will deliver): %v",
				sessionID, threadID, err,
			)
		}
	}
}

// Admit admits a client event batch into PostgreSQL and returns the committed
// public events. The admission transaction has already written the coalescible
// outbox wakeup, so the returned events are durable and will be processed even
// if the fast-path signal below fails. The fast-path signal is best-effort: a
// failure is logged and left to the relay, because correctness depends on the
// outbox, not on this call.
func (o *Orchestrator) Admit(ctx context.Context, sessionID string, drafts []domain.EventDraft) ([]domain.Event, error) {
	adm, err := o.store.AdmitEvents(ctx, sessionID, drafts)
	if err != nil {
		return nil, err
	}
	o.fastPathWake(ctx, sessionID, adm)
	// Echo only the caller-submitted events; orchestration-generated events are
	// observed through list and stream endpoints.
	if len(adm.SubmittedEvents) > 0 {
		return adm.SubmittedEvents, nil
	}
	return adm.Events, nil
}

func (o *Orchestrator) TerminateSession(ctx context.Context, sessionID string) error {
	if o.signaler == nil {
		return nil
	}
	if o.store != nil {
		threadIDs, err := o.store.ListChildSessionThreadIDs(ctx, sessionID)
		if err != nil {
			return err
		}
		return terminateSessionWorkflows(
			ctx, o.signaler, sessionID, threadIDs,
		)
	}
	return o.signaler.TerminateSession(ctx, sessionID)
}

func terminateSessionWorkflows(
	ctx context.Context,
	signaler *Signaler,
	sessionID string,
	threadIDs []string,
) error {
	var terminationErrs []error
	for _, threadID := range threadIDs {
		if err := signaler.TerminateThread(ctx, sessionID, threadID); err != nil {
			terminationErrs = append(terminationErrs, err)
		}
	}
	if err := errors.Join(terminationErrs...); err != nil {
		return err
	}
	return signaler.TerminateSession(ctx, sessionID)
}

// CreateSession creates a session and admits its initial events, then fast-path
// signals as above.
func (o *Orchestrator) CreateSession(ctx context.Context, session domain.Session, initial []domain.EventDraft) (domain.Session, []domain.Event, error) {
	adm, err := o.store.CreateSession(ctx, session, initial)
	if err != nil {
		return domain.Session{}, nil, err
	}
	o.fastPathWake(ctx, session.ID, adm)
	return adm.Session, adm.Events, nil
}

// CreateAPISession is the HTTP control-plane variant. In addition to the
// admission/outbox transaction it requires the exact Agent version and
// Environment to remain active through the session insert.
func (o *Orchestrator) CreateAPISession(
	ctx context.Context,
	session domain.Session,
	initial []domain.EventDraft,
	resourceSets ...[]app.PreparedSessionResource,
) (domain.Session, []domain.Event, error) {
	adm, err := o.store.CreateAPISession(ctx, session, initial, resourceSets...)
	if err != nil {
		return domain.Session{}, nil, err
	}
	o.fastPathWake(ctx, session.ID, adm)
	return adm.Session, adm.Events, nil
}

// CreateDeploymentSession commits the Session and its immutable Deployment Run
// success record together. A retry can therefore never expose a successful
// Session without the audit row that names it.
func (o *Orchestrator) CreateDeploymentSession(
	ctx context.Context,
	session domain.Session,
	initial []domain.EventDraft,
	run domain.DeploymentRun,
	resourceSets ...[]app.PreparedSessionResource,
) (domain.Session, []domain.Event, error) {
	adm, err := o.store.CreateDeploymentSession(ctx, session, initial, run, resourceSets...)
	if err != nil {
		return domain.Session{}, nil, err
	}
	o.fastPathWake(ctx, session.ID, adm)
	return adm.Session, adm.Events, nil
}

// Runtime bundles the worker and relay so cmd can run the execution plane with a
// single call.
type Runtime struct {
	Client    client.Client
	Worker    worker.Worker
	Relay     *Relay
	Lifecycle *LifecycleReconciler
	Store     *pg.Store
	Signal    *Signaler
	Sandbox   *sandbox.SessionManager
}

// RuntimeConfig declares the execution-plane dependencies and optional
// capabilities assembled by NewRuntime. TaskQueue defaults to TaskQueue when
// empty; the remaining optional interfaces may be nil when their capability is
// disabled.
type RuntimeConfig struct {
	TemporalClient   client.Client
	Store            *pg.Store
	ModelClient      model.Client
	SandboxProvider  sandbox.Provider
	IDGenerator      domain.IDGenerator
	RelayConfig      RelayConfig
	TaskQueue        string
	Resources        SandboxResourceReconciler
	MCPAuth          credentialruntime.AuthSource
	PreviewPublisher PreviewPublisher
}

// NewRuntime wires the full Temporal execution plane. The store is both the
// event source and durable tool-execution journal; the sandbox provider is
// wrapped in a session-scoped manager so filesystem state persists across
// turns. The returned Worker, Relay, and Lifecycle must be started by the
// caller.
func NewRuntime(config RuntimeConfig) *Runtime {
	taskQueue := config.TaskQueue
	if taskQueue == "" {
		taskQueue = TaskQueue
	}
	sandboxes := sandbox.NewSessionManager(config.SandboxProvider, config.Store)
	src := storeSource{store: config.Store} // satisfies both EventSource and JournalStore
	acts := NewActivities(
		config.ModelClient,
		src,
		src,
		sandboxes,
		config.IDGenerator,
		config.PreviewPublisher,
	)
	acts.WithMCPAuthSource(config.MCPAuth)
	skillCapability, hasSkillCapability := config.SandboxProvider.(sandbox.SkillBundleProvider)
	skillResources, hasSkillResources := config.Resources.(SkillRuntimeReconciler)
	acts.WithSkillRuntimeSupported(
		hasSkillCapability && skillCapability.SupportsSkillBundles() &&
			hasSkillResources && skillResources.SupportsSkillRuntime(),
	)
	if loader, ok := config.Resources.(SkillInstructionLoader); ok {
		acts.WithSkillInstructionLoader(loader)
	}
	outputCapability, hasOutputCapability := config.SandboxProvider.(sandbox.SessionOutputProvider)
	outputPublisher, hasOutputPublisher := config.Resources.(SessionOutputPublisher)
	if hasOutputCapability && outputCapability.SupportsSessionOutputs() &&
		hasOutputPublisher && outputPublisher.SupportsSessionOutputs() {
		acts.WithSessionOutputPublisher(outputPublisher)
	}
	if config.Resources != nil {
		acts.WithSandboxResourceReconciler(config.Resources)
	}
	w := NewWorkerOnTaskQueue(config.TemporalClient, acts, taskQueue)
	signaler := NewSignalerOnTaskQueue(config.TemporalClient, taskQueue)
	relay := NewRelay(config.Store, signaler, config.RelayConfig)
	terminator := NewOrchestrator(config.Store, signaler)
	lifecycle := NewLifecycleReconciler(
		config.Store,
		terminator,
		sandboxes,
		LifecycleReconcilerConfig{},
		resourceDeletionReconciler(config.Resources),
	)
	return &Runtime{
		Client: config.TemporalClient, Worker: w, Relay: relay, Lifecycle: lifecycle,
		Store: config.Store, Signal: signaler, Sandbox: sandboxes,
	}
}

func resourceDeletionReconciler(
	resources SandboxResourceReconciler,
) SessionResourceDeletionReconciler {
	if resources == nil {
		return nil
	}
	reconciler, _ := resources.(SessionResourceDeletionReconciler)
	return reconciler
}

// Orchestrator returns an admission orchestrator sharing this runtime's store
// and signaler (post-commit fast path).
func (r *Runtime) Orchestrator() *Orchestrator {
	return NewOrchestrator(r.Store, r.Signal)
}
