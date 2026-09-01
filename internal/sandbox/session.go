package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

// ErrProvisioningUnavailable means a provisioning intent could not be opened
// because the owning session is absent, deletion-fenced, or already bound.
// Callers should re-read the binding before deciding whether the session is
// unavailable: another worker may have committed it after the first lookup.
var ErrProvisioningUnavailable = errors.New("sandbox: provisioning unavailable")

// Binding is the durable association between a Mango Session and the
// provider resource that owns its workspace. SpecHash is diagnostic drift
// metadata and, for non-empty package plans, evidence that setup completed for
// the requested plan. Credentials and provider configuration must never be
// stored here.
type Binding struct {
	SessionID string
	Ref       Ref
	SpecHash  string
}

// ProvisioningIntent is the durable record written before Provider.Create.
// Spec contains sandbox configuration only; provider credentials must never be
// placed in it. Deleting is a reconciliation projection, not persisted intent
// state.
type ProvisioningIntent struct {
	SessionID string
	Provider  string
	Spec      Spec
	SpecHash  string
	Deleting  bool
}

// BindingStore persists sandbox ownership independently of worker memory.
// PutSandboxProvisioningIntent is insert-if-absent and records the
// provider-create obligation before the external call. PutSandboxBinding is
// insert-if-absent, returns the authoritative binding (which may have been won
// by another worker), and atomically removes the intent. Delete methods must
// remove only a record that still matches the caller's value.
type BindingStore interface {
	GetSandboxProvisioningIntent(
		ctx context.Context,
		sessionID string,
	) (ProvisioningIntent, bool, error)
	PutSandboxProvisioningIntent(
		ctx context.Context,
		intent ProvisioningIntent,
	) (ProvisioningIntent, error)
	ListSandboxProvisioningIntents(
		ctx context.Context,
		provider string,
		limit int,
	) ([]ProvisioningIntent, error)
	DeleteSandboxProvisioningIntent(
		ctx context.Context,
		intent ProvisioningIntent,
	) error
	GetSandboxBinding(ctx context.Context, sessionID string) (Binding, bool, error)
	PutSandboxBinding(ctx context.Context, binding Binding) (Binding, error)
	DeleteSandboxBinding(ctx context.Context, binding Binding) error
}

// SessionManager gives each session one logical sandbox across turns and worker
// restarts. Its in-memory map is only a client cache; BindingStore is the
// ownership source of truth, and Provider.Attach reconstructs a client from the
// persisted external reference.
//
// Operations for one session are serialized locally. Provider-side identity
// lookup recovers lost create responses; if separate worker processes both
// create successfully before either commits, BindingStore elects one durable
// winner and the losing resource is destroyed.
type SessionManager struct {
	provider Provider
	bindings BindingStore

	mu    sync.Mutex
	boxes map[string]Sandbox
	// specHashes records the durable binding evidence associated with a cached
	// client. It prevents a later non-empty package request from bypassing setup
	// merely because the resource is already in process memory.
	specHashes map[string]string
	// networkHashes tracks the final limited policy applied to each cached
	// sandbox. A Session agent update can change the MCP-derived allowlist
	// without replacing the durable workspace.
	networkHashes map[string]string
	locks         map[string]*sessionMutex
}

type sessionMutex struct {
	mu    sync.Mutex
	users int
}

// NewSessionManager wraps a provider with durable session ownership.
// BindingStore is required: sandbox identity must never fall back to process
// memory, even in a single-worker deployment.
func NewSessionManager(provider Provider, bindings BindingStore) *SessionManager {
	if bindings == nil {
		panic("sandbox: binding store is required")
	}
	return &SessionManager{
		provider:      provider,
		bindings:      bindings,
		boxes:         make(map[string]Sandbox),
		specHashes:    make(map[string]string),
		networkHashes: make(map[string]string),
		locks:         make(map[string]*sessionMutex),
	}
}

func (m *SessionManager) acquireSessionLock(sessionID string) func() {
	m.mu.Lock()
	lock, ok := m.locks[sessionID]
	if !ok {
		lock = &sessionMutex{}
		m.locks[sessionID] = lock
	}
	lock.users++
	m.mu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		m.mu.Lock()
		lock.users--
		if lock.users == 0 && m.locks[sessionID] == lock {
			delete(m.locks, sessionID)
		}
		m.mu.Unlock()
	}
}

// Acquire returns the session's live sandbox client. On first use it creates an
// idempotently named provider resource and commits its Ref. After a process
// restart it loads that Ref and attaches to the existing resource.
func (m *SessionManager) Acquire(
	ctx context.Context,
	sessionID string,
	spec Spec,
) (Sandbox, error) {
	if sessionID == "" {
		return nil, errors.New("sandbox: session id is required")
	}
	unlock := m.acquireSessionLock(sessionID)
	defer unlock()
	if err := m.validateAcquireSpec(spec); err != nil {
		return nil, err
	}
	if box, found, err := m.acquireExistingLocked(ctx, sessionID, spec); err != nil {
		return nil, err
	} else if found {
		return box, nil
	}

	proposedIntent := ProvisioningIntent{
		SessionID: sessionID,
		Provider:  m.provider.Name(),
		Spec:      spec,
		SpecHash:  specHash(spec),
	}
	intent, err := m.bindings.PutSandboxProvisioningIntent(ctx, proposedIntent)
	if err != nil {
		if errors.Is(err, ErrProvisioningUnavailable) {
			binding, found, loadErr := m.bindings.GetSandboxBinding(ctx, sessionID)
			if loadErr != nil {
				return nil, fmt.Errorf(
					"sandbox: reload binding after provisioning race: %w",
					loadErr,
				)
			}
			if found {
				box, attachErr := m.attach(ctx, binding, spec)
				if attachErr != nil {
					return nil, attachErr
				}
				m.cache(sessionID, box, binding.SpecHash, spec)
				return box, nil
			}
			return nil, Permanent(fmt.Errorf(
				"%w for session %s",
				ErrProvisioningUnavailable,
				sessionID,
			))
		}
		return nil, fmt.Errorf("sandbox: persist provisioning intent: %w", err)
	}
	if intent.Provider != proposedIntent.Provider ||
		intent.SpecHash != proposedIntent.SpecHash {
		return nil, Permanent(fmt.Errorf(
			"sandbox: session %s provisioning intent is for provider/spec %s/%s, worker requested %s/%s",
			sessionID,
			intent.Provider,
			intent.SpecHash,
			proposedIntent.Provider,
			proposedIntent.SpecHash,
		))
	}
	// A retry uses the authoritative saved Spec so provider identity does not
	// drift across a worker/configuration restart.
	spec = intent.Spec

	ref, box, err := m.provider.Create(ctx, sessionID, spec)
	if err != nil {
		return nil, err
	}
	if err := validateSandbox(m.provider, ref, box); err != nil {
		_ = box.Destroy(context.WithoutCancel(ctx))
		return nil, err
	}
	// Package installation may need registry access even when the final limited
	// policy does not allow package managers. Expand the policy only while the
	// sandbox is still unbound and therefore unavailable to tool execution.
	if spec.Network == "limited" {
		if err := applyLimitedNetwork(ctx, box, spec.SetupNetworkAllowedHosts); err != nil {
			return nil, err
		}
	}
	// A binding is evidence that provisioning, including package setup,
	// completed. Run setup before publishing the binding so a crash or transient
	// package-manager failure leaves the durable intent available for retry.
	if err := initializeSandbox(ctx, box, spec); err != nil {
		return nil, err
	}
	if spec.Network == "limited" {
		if err := applyLimitedNetwork(ctx, box, spec.NetworkAllowedHosts); err != nil {
			return nil, err
		}
	}
	proposed := Binding{
		SessionID: sessionID,
		Ref:       ref,
		SpecHash:  bindingSpecHash(spec),
	}
	authoritative, err := m.bindings.PutSandboxBinding(ctx, proposed)
	if err != nil {
		// The write result is ambiguous: destroying here could remove a
		// resource whose binding actually committed. Provider-side idempotency
		// lets the next attempt recover it, while the durable provisioning
		// intent keeps a true no-commit case visible to reconciliation.
		return nil, fmt.Errorf("sandbox: persist binding: %w", err)
	}
	if authoritative.Ref != proposed.Ref {
		_ = box.Destroy(context.WithoutCancel(ctx))
		box, err = m.attach(ctx, authoritative, spec)
		if err != nil {
			return nil, err
		}
	}
	m.cache(sessionID, box, authoritative.SpecHash, spec)
	return box, nil
}

// AcquireExisting attaches to a Session sandbox only when a durable binding
// already exists. It never provisions a new resource. Idle-boundary duties use
// this to avoid creating an otherwise unused sandbox for text-only Sessions.
func (m *SessionManager) AcquireExisting(
	ctx context.Context,
	sessionID string,
	spec Spec,
) (Sandbox, bool, error) {
	if sessionID == "" {
		return nil, false, errors.New("sandbox: session id is required")
	}
	unlock := m.acquireSessionLock(sessionID)
	defer unlock()
	if err := m.validateAcquireSpec(spec); err != nil {
		return nil, false, err
	}
	return m.acquireExistingLocked(ctx, sessionID, spec)
}

func (m *SessionManager) validateAcquireSpec(spec Spec) error {
	if err := validateSandboxNetworkSpec(spec); err != nil {
		return Permanent(err)
	}
	if !spec.Packages.Empty() {
		capability, ok := m.provider.(PackageSetupProvider)
		if !ok || !capability.SupportsPackageSetup() {
			return Permanent(fmt.Errorf(
				"sandbox: provider %q does not support isolated package setup",
				m.provider.Name(),
			))
		}
	}
	if spec.Network == "limited" {
		capability, ok := m.provider.(LimitedNetworkProvider)
		if !ok || !capability.SupportsLimitedNetwork() {
			return Permanent(fmt.Errorf(
				"sandbox: provider %q does not support limited networking",
				m.provider.Name(),
			))
		}
	}
	return nil
}

func (m *SessionManager) acquireExistingLocked(
	ctx context.Context,
	sessionID string,
	spec Spec,
) (Sandbox, bool, error) {
	m.mu.Lock()
	if box := m.boxes[sessionID]; box != nil {
		boundSpecHash := m.specHashes[sessionID]
		boundNetworkHash := m.networkHashes[sessionID]
		m.mu.Unlock()
		if !bindingProvesPackageSetup(boundSpecHash, spec) {
			return nil, false, Permanent(fmt.Errorf(
				"sandbox: session %s cached binding does not prove setup for the requested package plan",
				sessionID,
			))
		}
		if spec.Network == "limited" && boundNetworkHash != limitedNetworkHash(spec) {
			if err := applyLimitedNetwork(ctx, box, spec.NetworkAllowedHosts); err != nil {
				return nil, false, err
			}
			m.mu.Lock()
			m.networkHashes[sessionID] = limitedNetworkHash(spec)
			m.mu.Unlock()
		}
		return box, true, nil
	}
	m.mu.Unlock()

	binding, found, err := m.bindings.GetSandboxBinding(ctx, sessionID)
	if err != nil {
		return nil, false, fmt.Errorf("sandbox: load binding: %w", err)
	}
	if !found {
		return nil, false, nil
	}
	box, err := m.attach(ctx, binding, spec)
	if err != nil {
		return nil, false, err
	}
	m.cache(sessionID, box, binding.SpecHash, spec)
	return box, true, nil
}

func (m *SessionManager) attach(
	ctx context.Context,
	binding Binding,
	spec Spec,
) (Sandbox, error) {
	if !bindingProvesPackageSetup(binding.SpecHash, spec) {
		return nil, Permanent(fmt.Errorf(
			"sandbox: session %s binding does not prove setup for the requested package plan",
			binding.SessionID,
		))
	}
	if binding.Ref.Provider != m.provider.Name() {
		return nil, Permanent(fmt.Errorf(
			"sandbox: session %s is bound to provider %q, worker has %q",
			binding.SessionID,
			binding.Ref.Provider,
			m.provider.Name(),
		))
	}
	box, err := m.provider.Attach(ctx, binding.SessionID, binding.Ref, spec)
	if err != nil {
		return nil, fmt.Errorf("sandbox: attach session %s: %w", binding.SessionID, err)
	}
	if err := validateSandbox(m.provider, binding.Ref, box); err != nil {
		return nil, err
	}
	if spec.Network == "limited" {
		if err := applyLimitedNetwork(ctx, box, spec.NetworkAllowedHosts); err != nil {
			return nil, err
		}
	}
	return box, nil
}

func (m *SessionManager) cache(
	sessionID string,
	box Sandbox,
	specHash string,
	spec Spec,
) {
	m.mu.Lock()
	m.boxes[sessionID] = box
	m.specHashes[sessionID] = specHash
	if spec.Network == "limited" {
		m.networkHashes[sessionID] = limitedNetworkHash(spec)
	}
	m.mu.Unlock()
}

// Release idempotently tears down the provider resource and removes its
// binding. If the provider reports that the external resource is already gone,
// deleting the stale binding completes recovery. Other provider failures leave
// the binding intact so a durable cleanup retry can resume safely.
func (m *SessionManager) Release(ctx context.Context, sessionID string) error {
	if sessionID == "" {
		return errors.New("sandbox: session id is required")
	}
	unlock := m.acquireSessionLock(sessionID)
	defer unlock()

	m.mu.Lock()
	box := m.boxes[sessionID]
	delete(m.boxes, sessionID)
	delete(m.specHashes, sessionID)
	delete(m.networkHashes, sessionID)
	m.mu.Unlock()

	binding, found, err := m.bindings.GetSandboxBinding(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("sandbox: load binding for release: %w", err)
	}
	if !found {
		intent, intentFound, intentErr := m.bindings.GetSandboxProvisioningIntent(
			ctx,
			sessionID,
		)
		if intentErr != nil {
			return fmt.Errorf("sandbox: load provisioning intent for release: %w", intentErr)
		}
		if !intentFound {
			if box != nil {
				return box.Destroy(ctx)
			}
			return nil
		}
		if intent.Provider != m.provider.Name() {
			return Permanent(fmt.Errorf(
				"sandbox: cannot reconcile session %s intent for provider %q with %q",
				sessionID,
				intent.Provider,
				m.provider.Name(),
			))
		}
		// Provider.Create is idempotent by session key. It recovers a resource
		// created before a lost binding write; if the process died after writing
		// only the intent, creating then destroying an empty resource safely
		// discharges the same obligation.
		ref, orphan, createErr := m.provider.Create(ctx, sessionID, intent.Spec)
		if createErr != nil {
			return fmt.Errorf("sandbox: recover unbound resource for release: %w", createErr)
		}
		if err := validateSandbox(m.provider, ref, orphan); err != nil {
			if orphan != nil {
				_ = orphan.Destroy(context.WithoutCancel(ctx))
			}
			return err
		}
		if destroyErr := orphan.Destroy(ctx); destroyErr != nil &&
			!errors.Is(destroyErr, ErrNotFound) {
			return destroyErr
		}
		if err := m.bindings.DeleteSandboxProvisioningIntent(ctx, intent); err != nil {
			return fmt.Errorf("sandbox: delete provisioning intent: %w", err)
		}
		return nil
	}
	if binding.Ref.Provider != m.provider.Name() {
		return Permanent(fmt.Errorf(
			"sandbox: cannot release session %s bound to provider %q with %q",
			sessionID,
			binding.Ref.Provider,
			m.provider.Name(),
		))
	}
	if destroyer, ok := m.provider.(BoundSessionDestroyer); ok {
		if err := destroyer.DestroyBoundSession(ctx, sessionID, binding.Ref); err != nil &&
			!errors.Is(err, ErrNotFound) {
			return err
		}
		if err := m.bindings.DeleteSandboxBinding(ctx, binding); err != nil {
			return fmt.Errorf("sandbox: delete binding: %w", err)
		}
		return m.clearMatchingProvisioningIntent(ctx, sessionID)
	}
	if box == nil {
		box, err = m.provider.Attach(ctx, sessionID, binding.Ref, Spec{})
		if errors.Is(err, ErrNotFound) {
			if err := m.bindings.DeleteSandboxBinding(ctx, binding); err != nil {
				return err
			}
			return m.clearMatchingProvisioningIntent(ctx, sessionID)
		}
		if err != nil {
			return fmt.Errorf("sandbox: attach for release: %w", err)
		}
		if err := validateSandbox(m.provider, binding.Ref, box); err != nil {
			return err
		}
	}
	if err := box.Destroy(ctx); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	if err := m.bindings.DeleteSandboxBinding(ctx, binding); err != nil {
		return fmt.Errorf("sandbox: delete binding: %w", err)
	}
	return m.clearMatchingProvisioningIntent(ctx, sessionID)
}

// clearMatchingProvisioningIntent removes a stale same-provider intent after
// the authoritative binding has been destroyed. Such a coexistence could be
// produced by an older worker racing a binding commit. A different-provider
// intent may represent a second external resource and must be reconciled by a
// worker configured for that provider instead of being discarded here.
func (m *SessionManager) clearMatchingProvisioningIntent(
	ctx context.Context,
	sessionID string,
) error {
	intent, found, err := m.bindings.GetSandboxProvisioningIntent(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("sandbox: load stale provisioning intent: %w", err)
	}
	if !found {
		return nil
	}
	if intent.Provider != m.provider.Name() {
		return Permanent(fmt.Errorf(
			"sandbox: session %s also has an intent for provider %q; worker has %q",
			sessionID,
			intent.Provider,
			m.provider.Name(),
		))
	}
	if err := m.bindings.DeleteSandboxProvisioningIntent(ctx, intent); err != nil {
		return fmt.Errorf("sandbox: delete stale provisioning intent: %w", err)
	}
	return nil
}

// ReconcileProvisioning recovers provider resources left between the durable
// intent and binding commits. Active sessions finish binding the idempotently
// named resource; deleting sessions recover-and-destroy it. Every intent is
// attempted even if an earlier one fails.
func (m *SessionManager) ReconcileProvisioning(
	ctx context.Context,
	limit int,
) (int, error) {
	if limit <= 0 {
		limit = 100
	}
	intents, err := m.bindings.ListSandboxProvisioningIntents(
		ctx,
		m.provider.Name(),
		limit,
	)
	if err != nil {
		return 0, fmt.Errorf("sandbox: list provisioning intents: %w", err)
	}
	var (
		completed int
		errs      []error
	)
	for _, intent := range intents {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		if intent.Deleting {
			err = m.Release(ctx, intent.SessionID)
		} else {
			_, err = m.Acquire(ctx, intent.SessionID, intent.Spec)
			if err == nil {
				err = m.clearBoundProvisioningIntent(ctx, intent)
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"session %s: %w",
				intent.SessionID,
				err,
			))
			continue
		}
		_, found, err := m.bindings.GetSandboxProvisioningIntent(
			ctx,
			intent.SessionID,
		)
		if err != nil {
			errs = append(errs, fmt.Errorf(
				"verify session %s provisioning intent: %w",
				intent.SessionID,
				err,
			))
			continue
		}
		if found {
			errs = append(errs, fmt.Errorf(
				"session %s provisioning intent remains unresolved",
				intent.SessionID,
			))
			continue
		}
		completed++
	}
	return completed, errors.Join(errs...)
}

func (m *SessionManager) clearBoundProvisioningIntent(
	ctx context.Context,
	intent ProvisioningIntent,
) error {
	binding, found, err := m.bindings.GetSandboxBinding(ctx, intent.SessionID)
	if err != nil {
		return fmt.Errorf("sandbox: load binding while reconciling intent: %w", err)
	}
	if !found {
		return errors.New("sandbox: acquire completed without a durable binding")
	}
	if binding.Ref.Provider != intent.Provider {
		return Permanent(fmt.Errorf(
			"sandbox: session %s is bound to provider %q but intent uses %q",
			intent.SessionID,
			binding.Ref.Provider,
			intent.Provider,
		))
	}
	if err := m.bindings.DeleteSandboxProvisioningIntent(ctx, intent); err != nil {
		return fmt.Errorf("sandbox: clear reconciled provisioning intent: %w", err)
	}
	return nil
}

func specHash(spec Spec) string {
	body, _ := json.Marshal(spec)
	sum := sha256.Sum256(body)
	return fmt.Sprintf("sha256:%x", sum[:])
}
