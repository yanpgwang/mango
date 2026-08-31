package sandbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type failBindingCommitStore struct {
	*memoryBindingStore
	failures int
}

func (s *failBindingCommitStore) PutSandboxBinding(
	ctx context.Context,
	binding Binding,
) (Binding, error) {
	if s.failures > 0 {
		s.failures--
		return Binding{}, errors.New("simulated binding commit failure")
	}
	return s.memoryBindingStore.PutSandboxBinding(ctx, binding)
}

func TestSessionManager_ReconcilesCreateBeforeBindingCrash(t *testing.T) {
	ctx := context.Background()
	provider := newStateProvider()
	store := &failBindingCommitStore{
		memoryBindingStore: newMemoryBindingStore(),
		failures:           1,
	}
	spec := Spec{Timeout: time.Second}

	first := NewSessionManager(provider, store)
	if _, err := first.Acquire(ctx, "sesn_orphan_recover", spec); err == nil {
		t.Fatal("Acquire succeeded despite simulated binding commit failure")
	}
	if _, found, err := store.GetSandboxBinding(ctx, "sesn_orphan_recover"); err != nil || found {
		t.Fatalf("binding after failed commit: found=%v err=%v", found, err)
	}
	if _, found, err := store.GetSandboxProvisioningIntent(
		ctx,
		"sesn_orphan_recover",
	); err != nil || !found {
		t.Fatalf("provisioning intent after failed commit: found=%v err=%v", found, err)
	}

	// The provider resource exists even though its binding did not commit. A
	// marker proves reconciliation attaches the same workspace rather than
	// silently replacing it.
	_, orphan, err := provider.Create(ctx, "sesn_orphan_recover", spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := orphan.WriteFile(ctx, "survives-crash", []byte("present")); err != nil {
		t.Fatalf("write crash marker: %v", err)
	}
	restarted := NewSessionManager(provider, store)
	completed, err := restarted.ReconcileProvisioning(ctx, 10)
	if err != nil {
		t.Fatalf("reconcile provisioning: %v", err)
	}
	if completed != 1 {
		t.Fatalf("reconciled = %d, want 1", completed)
	}
	box, err := restarted.Acquire(ctx, "sesn_orphan_recover", spec)
	if err != nil {
		t.Fatalf("acquire reconciled sandbox: %v", err)
	}
	got, err := box.ReadFile(ctx, "survives-crash")
	if err != nil || string(got) != "present" {
		t.Fatalf("reconciled marker = %q, err=%v", got, err)
	}
	if _, found, err := store.GetSandboxProvisioningIntent(
		ctx,
		"sesn_orphan_recover",
	); err != nil || found {
		t.Fatalf("intent survived binding commit: found=%v err=%v", found, err)
	}
}

func TestSessionManager_ReleaseRecoversUnboundResource(t *testing.T) {
	ctx := context.Background()
	provider := newStateProvider()
	store := newMemoryBindingStore()
	spec := Spec{Timeout: time.Second}
	intent := ProvisioningIntent{
		SessionID: "sesn_orphan_delete",
		Provider:  provider.Name(),
		Spec:      spec,
		SpecHash:  specHash(spec),
		Deleting:  true,
	}
	if _, err := store.PutSandboxProvisioningIntent(ctx, intent); err != nil {
		t.Fatalf("put intent: %v", err)
	}
	ref, _, err := provider.Create(ctx, intent.SessionID, spec)
	if err != nil {
		t.Fatalf("create unbound resource: %v", err)
	}

	manager := NewSessionManager(provider, store)
	if err := manager.Release(ctx, intent.SessionID); err != nil {
		t.Fatalf("release unbound resource: %v", err)
	}
	if _, err := provider.Attach(ctx, intent.SessionID, ref, spec); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphan resource still exists: %v", err)
	}
	if _, found, err := store.GetSandboxProvisioningIntent(ctx, intent.SessionID); err != nil || found {
		t.Fatalf("intent survived orphan release: found=%v err=%v", found, err)
	}
}

func TestSessionManager_ReleaseClearsStaleIntentAlongsideBinding(t *testing.T) {
	ctx := context.Background()
	provider := newStateProvider()
	store := newMemoryBindingStore()
	spec := Spec{Timeout: time.Second}
	sessionID := "sesn_stale_intent_delete"
	ref, _, err := provider.Create(ctx, sessionID, spec)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	binding := Binding{
		SessionID: sessionID,
		Ref:       ref,
		SpecHash:  specHash(spec),
	}
	if _, err := store.PutSandboxBinding(ctx, binding); err != nil {
		t.Fatalf("put binding: %v", err)
	}
	// Model the state an older racing worker could leave by recreating an
	// intent after the binding transaction had cleared the original one.
	store.mu.Lock()
	store.intents[sessionID] = ProvisioningIntent{
		SessionID: sessionID,
		Provider:  provider.Name(),
		Spec:      spec,
		SpecHash:  specHash(spec),
		Deleting:  true,
	}
	store.mu.Unlock()

	manager := NewSessionManager(provider, store)
	if err := manager.Release(ctx, sessionID); err != nil {
		t.Fatalf("release stale intent and binding: %v", err)
	}
	if _, found, err := store.GetSandboxBinding(ctx, sessionID); err != nil || found {
		t.Fatalf("binding after release: found=%v err=%v", found, err)
	}
	if _, found, err := store.GetSandboxProvisioningIntent(
		ctx,
		sessionID,
	); err != nil || found {
		t.Fatalf("intent after release: found=%v err=%v", found, err)
	}
}

func TestSessionManager_ReconcileClearsStaleIntentAlongsideBinding(t *testing.T) {
	ctx := context.Background()
	provider := newStateProvider()
	store := newMemoryBindingStore()
	spec := Spec{Timeout: time.Second}
	sessionID := "sesn_stale_intent_active"
	ref, _, err := provider.Create(ctx, sessionID, spec)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	binding := Binding{SessionID: sessionID, Ref: ref, SpecHash: specHash(spec)}
	if _, err := store.PutSandboxBinding(ctx, binding); err != nil {
		t.Fatalf("put binding: %v", err)
	}
	store.mu.Lock()
	store.intents[sessionID] = ProvisioningIntent{
		SessionID: sessionID,
		Provider:  provider.Name(),
		Spec:      spec,
		SpecHash:  specHash(spec),
	}
	store.mu.Unlock()

	manager := NewSessionManager(provider, store)
	completed, err := manager.ReconcileProvisioning(ctx, 10)
	if err != nil || completed != 1 {
		t.Fatalf("reconcile stale intent: completed=%d err=%v", completed, err)
	}
	if _, found, err := store.GetSandboxProvisioningIntent(
		ctx,
		sessionID,
	); err != nil || found {
		t.Fatalf("intent after reconciliation: found=%v err=%v", found, err)
	}
	if err := manager.Release(ctx, sessionID); err != nil {
		t.Fatalf("release reconciled sandbox: %v", err)
	}
}

// countingProvider wraps a Provider and counts Create calls so tests can assert
// a session creates its logical sandbox exactly once.
type countingProvider struct {
	inner        Provider
	provisions   atomic.Int64
	attachments  atomic.Int64
	provisionErr error
}

func (p *countingProvider) Name() string { return p.inner.Name() }

func (p *countingProvider) Create(
	ctx context.Context,
	sessionKey string,
	spec Spec,
) (Ref, Sandbox, error) {
	p.provisions.Add(1)
	if p.provisionErr != nil {
		return Ref{}, nil, p.provisionErr
	}
	return p.inner.Create(ctx, sessionKey, spec)
}

func (p *countingProvider) Attach(
	ctx context.Context,
	sessionKey string,
	ref Ref,
	spec Spec,
) (Sandbox, error) {
	p.attachments.Add(1)
	return p.inner.Attach(ctx, sessionKey, ref, spec)
}

// countingSandbox wraps a Sandbox and counts Destroy calls so tests can assert
// a session's sandbox is torn down exactly once.
type countingSandbox struct {
	inner    Sandbox
	destroys atomic.Int64
}

func (s *countingSandbox) Exec(ctx context.Context, cmd Command) (*Result, error) {
	return s.inner.Exec(ctx, cmd)
}
func (s *countingSandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return s.inner.ReadFile(ctx, path)
}
func (s *countingSandbox) WriteFile(ctx context.Context, path string, data []byte) error {
	return s.inner.WriteFile(ctx, path, data)
}
func (s *countingSandbox) Root() string { return s.inner.Root() }
func (s *countingSandbox) Destroy(ctx context.Context) error {
	s.destroys.Add(1)
	return s.inner.Destroy(ctx)
}

// destroyCountingProvider hands out countingSandbox instances and remembers the
// last one it created so a test can inspect its destroy count.
type destroyCountingProvider struct {
	inner Provider
	mu    sync.Mutex
	last  *countingSandbox
}

func (p *destroyCountingProvider) Name() string { return p.inner.Name() }

func (p *destroyCountingProvider) Create(
	ctx context.Context,
	sessionKey string,
	spec Spec,
) (Ref, Sandbox, error) {
	ref, box, err := p.inner.Create(ctx, sessionKey, spec)
	if err != nil {
		return Ref{}, nil, err
	}
	cs := &countingSandbox{inner: box}
	p.mu.Lock()
	p.last = cs
	p.mu.Unlock()
	return ref, cs, nil
}

func (p *destroyCountingProvider) Attach(
	ctx context.Context,
	sessionKey string,
	ref Ref,
	spec Spec,
) (Sandbox, error) {
	box, err := p.inner.Attach(ctx, sessionKey, ref, spec)
	if err != nil {
		return nil, err
	}
	return &countingSandbox{inner: box}, nil
}

func TestSessionManager_ReusesSandboxPerSession(t *testing.T) {
	cp := &countingProvider{inner: newStateProvider()}
	m := NewSessionManager(cp, newMemoryBindingStore())
	ctx := context.Background()

	first, err := m.Acquire(ctx, "sesn_a", Spec{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Acquire(ctx, "sesn_a", Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("Acquire returned a different sandbox for the same session")
	}
	if got := cp.provisions.Load(); got != 1 {
		t.Fatalf("provisions = %d, want 1 (session reuses one logical sandbox)", got)
	}
	if err := m.Release(ctx, "sesn_a"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionManager_AcquireExistingNeverProvisions(t *testing.T) {
	provider := &countingProvider{inner: newStateProvider()}
	bindings := newMemoryBindingStore()
	manager := NewSessionManager(provider, bindings)

	box, found, err := manager.AcquireExisting(
		context.Background(), "sesn_text_only", Spec{},
	)
	if err != nil || found || box != nil {
		t.Fatalf("AcquireExisting = box:%v found:%t err:%v", box, found, err)
	}
	if got := provider.provisions.Load(); got != 0 {
		t.Fatalf("AcquireExisting provisioned %d sandboxes, want 0", got)
	}
}

func TestSessionManager_AcquireExistingReattachesDurableBinding(t *testing.T) {
	ctx := context.Background()
	provider := &countingProvider{inner: newStateProvider()}
	bindings := newMemoryBindingStore()
	first := NewSessionManager(provider, bindings)
	box, err := first.Acquire(ctx, "sesn_existing", Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.WriteFile(ctx, "state.txt", []byte("kept")); err != nil {
		t.Fatal(err)
	}

	restarted := NewSessionManager(provider, bindings)
	attached, found, err := restarted.AcquireExisting(ctx, "sesn_existing", Spec{})
	if err != nil || !found {
		t.Fatalf("AcquireExisting found=%t err=%v", found, err)
	}
	data, err := attached.ReadFile(ctx, "state.txt")
	if err != nil || string(data) != "kept" {
		t.Fatalf("reattached data=%q err=%v", data, err)
	}
	if got := provider.provisions.Load(); got != 1 {
		t.Fatalf("Create calls=%d, want 1", got)
	}
	if got := provider.attachments.Load(); got != 1 {
		t.Fatalf("Attach calls=%d, want 1", got)
	}
	if err := restarted.Release(ctx, "sesn_existing"); err != nil {
		t.Fatal(err)
	}
}

func TestNewSessionManager_RequiresBindingStore(t *testing.T) {
	defer func() {
		if recovered := recover(); recovered == nil {
			t.Fatal("NewSessionManager accepted a nil BindingStore")
		}
	}()
	NewSessionManager(newStateProvider(), nil)
}

func TestSessionManager_IsolatesSessions(t *testing.T) {
	m := NewSessionManager(newStateProvider(), newMemoryBindingStore())
	ctx := context.Background()

	a, err := m.Acquire(ctx, "sesn_a", Spec{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := m.Acquire(ctx, "sesn_b", Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if a == b || a.Root() == b.Root() {
		t.Fatal("different sessions must get distinct sandboxes")
	}

	if err := a.WriteFile(ctx, "shared.txt", []byte("A")); err != nil {
		t.Fatal(err)
	}
	if _, err := b.ReadFile(ctx, "shared.txt"); err == nil {
		t.Fatal("session B must not see a file written in session A")
	}
	_ = m.Release(ctx, "sesn_a")
	_ = m.Release(ctx, "sesn_b")
}

func TestSessionManager_ReleaseDestroysExactlyOnce(t *testing.T) {
	dp := &destroyCountingProvider{inner: newStateProvider()}
	m := NewSessionManager(dp, newMemoryBindingStore())
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "sesn_a", Spec{}); err != nil {
		t.Fatal(err)
	}
	box := dp.last

	if err := m.Release(ctx, "sesn_a"); err != nil {
		t.Fatal(err)
	}
	// A second Release for the same (now forgotten) session must be a no-op.
	if err := m.Release(ctx, "sesn_a"); err != nil {
		t.Fatal(err)
	}
	// Releasing a session that never provisioned is also a no-op.
	if err := m.Release(ctx, "sesn_never"); err != nil {
		t.Fatal(err)
	}
	if got := box.destroys.Load(); got != 1 {
		t.Fatalf("destroys = %d, want exactly 1", got)
	}
}

func TestSessionManager_ConcurrentAcquireProvisionsOnce(t *testing.T) {
	cp := &countingProvider{inner: newStateProvider()}
	m := NewSessionManager(cp, newMemoryBindingStore())
	ctx := context.Background()

	const goroutines = 32
	var wg sync.WaitGroup
	boxes := make([]Sandbox, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			box, err := m.Acquire(ctx, "sesn_race", Spec{})
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			boxes[i] = box
		}(i)
	}
	wg.Wait()

	if got := cp.provisions.Load(); got != 1 {
		t.Fatalf("concurrent Acquire provisioned %d times, want 1", got)
	}
	for i := 1; i < goroutines; i++ {
		if boxes[i] != boxes[0] {
			t.Fatal("concurrent Acquire returned different sandbox instances")
		}
	}
	_ = m.Release(ctx, "sesn_race")
}

func TestSessionManager_ProvisionFailureIsNotCached(t *testing.T) {
	cp := &countingProvider{inner: newStateProvider(), provisionErr: errors.New("boom")}
	m := NewSessionManager(cp, newMemoryBindingStore())
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "sesn_a", Spec{}); err == nil {
		t.Fatal("expected provision error")
	}
	// The failure must not be cached: a later Acquire should retry provisioning.
	cp.provisionErr = nil
	if _, err := m.Acquire(ctx, "sesn_a", Spec{}); err != nil {
		t.Fatalf("retry after failed provision: %v", err)
	}
	if got := cp.provisions.Load(); got != 2 {
		t.Fatalf("provisions = %d, want 2 (failure retried, not cached)", got)
	}
	_ = m.Release(ctx, "sesn_a")
}

func TestSessionManager_ReattachesPersistedBindingAfterRestart(t *testing.T) {
	ctx := context.Background()
	bindings := newMemoryBindingStore()
	provider := &countingProvider{inner: newStateProvider()}

	firstManager := NewSessionManager(provider, bindings)
	first, err := firstManager.Acquire(ctx, "sesn_restart", Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.WriteFile(ctx, "state.txt", []byte("survived")); err != nil {
		t.Fatal(err)
	}

	// Simulate a worker restart by abandoning the process-local client cache
	// while retaining the provider resource and durable binding.
	secondManager := NewSessionManager(provider, bindings)
	second, err := secondManager.Acquire(ctx, "sesn_restart", Spec{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := second.ReadFile(ctx, "state.txt")
	if err != nil || string(data) != "survived" {
		t.Fatalf("reattached workspace data = %q, err=%v", data, err)
	}
	if got := provider.provisions.Load(); got != 1 {
		t.Fatalf("Create calls = %d, want 1", got)
	}
	if got := provider.attachments.Load(); got != 1 {
		t.Fatalf("Attach calls = %d, want 1", got)
	}
	if err := secondManager.Release(ctx, "sesn_restart"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionManager_MissingPersistedResourceFailsWithoutReplacement(t *testing.T) {
	ctx := context.Background()
	bindings := newMemoryBindingStore()
	provider := &countingProvider{inner: newStateProvider()}
	firstManager := NewSessionManager(provider, bindings)
	box, err := firstManager.Acquire(ctx, "sesn_missing", Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Destroy(ctx); err != nil {
		t.Fatal(err)
	}

	secondManager := NewSessionManager(provider, bindings)
	if _, err := secondManager.Acquire(ctx, "sesn_missing", Spec{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Acquire after resource loss = %v, want ErrNotFound", err)
	}
	if got := provider.provisions.Load(); got != 1 {
		t.Fatalf("resource loss triggered %d Create calls, want no replacement", got)
	}
}

// blockingProvider makes Create block until proceed is closed, so a test can
// drive Release into the exact window where a provision is still in flight. It
// signals entry on entered and records the countingSandbox it eventually hands
// out so the test can assert the real instance is destroyed.
type blockingProvider struct {
	inner   Provider
	entered chan struct{} // closed once Provision has started
	proceed chan struct{} // Provision blocks until this is closed
	mu      sync.Mutex
	last    *countingSandbox
}

func (p *blockingProvider) Name() string { return p.inner.Name() }

func (p *blockingProvider) Create(
	ctx context.Context,
	sessionKey string,
	spec Spec,
) (Ref, Sandbox, error) {
	close(p.entered)
	<-p.proceed
	ref, box, err := p.inner.Create(ctx, sessionKey, spec)
	if err != nil {
		return Ref{}, nil, err
	}
	cs := &countingSandbox{inner: box}
	p.mu.Lock()
	p.last = cs
	p.mu.Unlock()
	return ref, cs, nil
}

func (p *blockingProvider) Attach(
	ctx context.Context,
	sessionKey string,
	ref Ref,
	spec Spec,
) (Sandbox, error) {
	return p.inner.Attach(ctx, sessionKey, ref, spec)
}

// TestSessionManager_ReleaseWaitsForInflightProvision guards the narrow window
// where Release races a still-provisioning Acquire. With a deliberately blocking
// provider, Release begins while Provision is blocked: it must not return early,
// and once the provision finishes it must destroy the real instance exactly
// once rather than a half-provisioned entry.
func TestSessionManager_ReleaseWaitsForInflightProvision(t *testing.T) {
	p := &blockingProvider{
		inner:   newStateProvider(),
		entered: make(chan struct{}),
		proceed: make(chan struct{}),
	}
	m := NewSessionManager(p, newMemoryBindingStore())
	ctx := context.Background()

	acquired := make(chan struct{})
	go func() {
		if _, err := m.Acquire(ctx, "sesn_a", Spec{}); err != nil {
			t.Errorf("acquire: %v", err)
		}
		close(acquired)
	}()

	// Wait until Provision is actually in flight (Acquire is blocked inside the
	// provider), so Release runs against a still-provisioning entry.
	select {
	case <-p.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Provision was never entered")
	}

	// Release now races the in-flight provision. It must NOT return until the
	// provision finishes, because it has to destroy the real instance.
	released := make(chan error, 1)
	go func() { released <- m.Release(ctx, "sesn_a") }()

	select {
	case <-released:
		t.Fatal("Release returned before the in-flight provision completed")
	case <-time.After(100 * time.Millisecond):
		// Expected: Release is blocked waiting on the provision to finish.
	}

	// Unblock the provision. Acquire completes and Release can now proceed to
	// destroy the freshly provisioned sandbox.
	close(p.proceed)

	select {
	case err := <-released:
		if err != nil {
			t.Fatalf("release: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Release did not complete after the provision finished")
	}
	<-acquired

	p.mu.Lock()
	box := p.last
	p.mu.Unlock()
	if box == nil {
		t.Fatal("provider never handed out a sandbox")
	}
	if got := box.destroys.Load(); got != 1 {
		t.Fatalf("destroys = %d, want exactly 1", got)
	}
}

type authoritativeBindingStore struct {
	winner Binding
}

func (*authoritativeBindingStore) GetSandboxProvisioningIntent(
	context.Context,
	string,
) (ProvisioningIntent, bool, error) {
	return ProvisioningIntent{}, false, nil
}

func (*authoritativeBindingStore) PutSandboxProvisioningIntent(
	_ context.Context,
	intent ProvisioningIntent,
) (ProvisioningIntent, error) {
	return intent, nil
}

func (*authoritativeBindingStore) ListSandboxProvisioningIntents(
	context.Context,
	string,
	int,
) ([]ProvisioningIntent, error) {
	return []ProvisioningIntent{}, nil
}

func (*authoritativeBindingStore) DeleteSandboxProvisioningIntent(
	context.Context,
	ProvisioningIntent,
) error {
	return nil
}

func (*authoritativeBindingStore) GetSandboxBinding(
	context.Context,
	string,
) (Binding, bool, error) {
	return Binding{}, false, nil
}

func (s *authoritativeBindingStore) PutSandboxBinding(
	context.Context,
	Binding,
) (Binding, error) {
	return s.winner, nil
}

func (*authoritativeBindingStore) DeleteSandboxBinding(context.Context, Binding) error {
	return nil
}

type createTrackingProvider struct {
	inner       Provider
	createdRef  Ref
	creates     int
	attachments atomic.Int64
}

func (p *createTrackingProvider) Name() string { return p.inner.Name() }

func (p *createTrackingProvider) Create(
	ctx context.Context,
	sessionKey string,
	spec Spec,
) (Ref, Sandbox, error) {
	// This resilience double deliberately returns a distinct resource for each
	// Create so the manager's losing-bind cleanup path remains testable even
	// though conforming production providers are idempotent by session key.
	p.creates++
	ref, box, err := p.inner.Create(ctx, fmt.Sprintf("%s-resource-%d", sessionKey, p.creates), spec)
	if box != nil {
		p.createdRef = ref
	}
	return ref, box, err
}

func (p *createTrackingProvider) Attach(
	ctx context.Context,
	sessionKey string,
	ref Ref,
	spec Spec,
) (Sandbox, error) {
	p.attachments.Add(1)
	// This deliberately non-idempotent double keys its resources by identity.
	return p.inner.Attach(ctx, ref.ID, ref, spec)
}

func TestSessionManager_DestroysLosingCreateAndAttachesBindingWinner(t *testing.T) {
	ctx := context.Background()
	inner := newStateProvider()
	provider := &createTrackingProvider{inner: inner}
	winnerRef, winnerBox, err := provider.Create(ctx, "sesn_election", Spec{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = winnerBox.Destroy(context.Background()) })
	if err := winnerBox.WriteFile(ctx, "winner.txt", []byte("winner")); err != nil {
		t.Fatal(err)
	}

	provider.createdRef = Ref{}
	bindings := &authoritativeBindingStore{winner: Binding{
		SessionID: "sesn_election",
		Ref:       winnerRef,
		SpecHash:  specHash(Spec{}),
	}}
	manager := NewSessionManager(provider, bindings)
	box, err := manager.Acquire(ctx, "sesn_election", Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if provider.createdRef.ID == "" {
		t.Fatal("provider did not create a losing resource")
	}
	if _, err := inner.Attach(ctx, provider.createdRef.ID, provider.createdRef, Spec{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("losing resource still exists: %+v err=%v", provider.createdRef, err)
	}
	data, err := box.ReadFile(ctx, "winner.txt")
	if err != nil || string(data) != "winner" {
		t.Fatalf("attached winner data = %q, err=%v", data, err)
	}
	if got := provider.attachments.Load(); got != 1 {
		t.Fatalf("Attach calls = %d, want 1", got)
	}
}

type destroyFailingProvider struct {
	inner Provider
	err   error
}

func (p *destroyFailingProvider) Name() string { return p.inner.Name() }

func (p *destroyFailingProvider) Create(
	ctx context.Context,
	sessionKey string,
	spec Spec,
) (Ref, Sandbox, error) {
	ref, box, err := p.inner.Create(ctx, sessionKey, spec)
	if err != nil {
		return Ref{}, nil, err
	}
	return ref, &destroyFailingSandbox{Sandbox: box, err: p.err}, nil
}

func (p *destroyFailingProvider) Attach(
	ctx context.Context,
	sessionKey string,
	ref Ref,
	spec Spec,
) (Sandbox, error) {
	box, err := p.inner.Attach(ctx, sessionKey, ref, spec)
	if err != nil {
		return nil, err
	}
	return &destroyFailingSandbox{Sandbox: box, err: p.err}, nil
}

type destroyFailingSandbox struct {
	Sandbox
	err error
}

func (s *destroyFailingSandbox) Destroy(context.Context) error { return s.err }

func TestSessionManager_DestroyFailureRetainsBindingForRetry(t *testing.T) {
	ctx := context.Background()
	bindings := newMemoryBindingStore()
	inner := newStateProvider()
	provider := &destroyFailingProvider{
		inner: inner,
		err:   errors.New("provider unavailable"),
	}
	manager := NewSessionManager(provider, bindings)
	if _, err := manager.Acquire(ctx, "sesn_destroy_retry", Spec{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Release(ctx, "sesn_destroy_retry"); err == nil {
		t.Fatal("Release succeeded despite provider destroy failure")
	}
	binding, found, err := bindings.GetSandboxBinding(ctx, "sesn_destroy_retry")
	if err != nil || !found {
		t.Fatalf("binding after destroy failure = %+v, found=%v, err=%v", binding, found, err)
	}

	// Bypass the failing wrapper for test cleanup.
	box, err := inner.Attach(ctx, binding.SessionID, binding.Ref, Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := box.Destroy(ctx); err != nil {
		t.Fatal(err)
	}
}
