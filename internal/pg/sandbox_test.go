package pg

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/sandbox"
	"github.com/yanpgwang/mango/internal/sandbox/sandboxtest"
)

func TestSandboxProvisioningIntentSerializesWithDeletionFence(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_sandbox_intent_fence_race")
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Hold the same row lock used by PrepareSessionDeletion and make its fence
	// update visible only when this transaction commits.
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin deletion transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err := tx.Exec(
		ctx,
		"SELECT id FROM sessions WHERE id = $1 FOR UPDATE",
		session.ID,
	); err != nil {
		t.Fatalf("lock session: %v", err)
	}
	if _, err := tx.Exec(
		ctx,
		"UPDATE sessions SET deleting_at = now() WHERE id = $1",
		session.ID,
	); err != nil {
		t.Fatalf("mark deleting: %v", err)
	}

	intent := sandbox.ProvisioningIntent{
		SessionID: session.ID,
		Provider:  "docker",
		Spec:      sandbox.Spec{Image: "example.test/sandbox:fixed"},
		SpecHash:  "sha256:fence-race",
	}
	putDone := make(chan error, 1)
	go func() {
		_, putErr := store.PutSandboxProvisioningIntent(ctx, intent)
		putDone <- putErr
	}()
	select {
	case err := <-putDone:
		t.Fatalf("intent creation did not wait for deletion fence: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit deletion fence: %v", err)
	}
	select {
	case err := <-putDone:
		if !errors.Is(err, sandbox.ErrProvisioningUnavailable) {
			t.Fatalf("intent after concurrent fence = %v, want unavailable", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("intent creation stayed blocked after deletion fence commit")
	}
}

func TestSandboxReleaseRepairsLegacyBindingAndIntentCoexistence(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_sandbox_legacy_double_state")
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	provider := sandboxtest.DockerProvider(t)
	spec := sandbox.Spec{}
	ref, _, err := provider.Create(ctx, session.ID, spec)
	if err != nil {
		t.Fatalf("create sandbox: %v", err)
	}
	binding := sandbox.Binding{
		SessionID: session.ID,
		Ref:       ref,
		SpecHash:  "sha256:legacy-double-state",
	}
	if _, err := store.PutSandboxBinding(ctx, binding); err != nil {
		t.Fatalf("put binding: %v", err)
	}
	rawSpec, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal spec: %v", err)
	}
	// Bypass the fixed write API to model a row left by an older worker.
	if _, err := store.pool.Exec(
		ctx,
		`INSERT INTO sandbox_provisioning_intents
            (session_id, provider, spec, spec_hash, created_at, updated_at)
         VALUES ($1, $2, $3, $4, now(), now())`,
		session.ID,
		provider.Name(),
		rawSpec,
		binding.SpecHash,
	); err != nil {
		t.Fatalf("insert legacy intent: %v", err)
	}
	if err := store.PrepareSessionDeletion(ctx, session.ID); err != nil {
		t.Fatalf("prepare deletion: %v", err)
	}
	manager := sandbox.NewSessionManager(provider, store)
	if err := manager.Release(ctx, session.ID); err != nil {
		t.Fatalf("release double state: %v", err)
	}
	if err := store.FinalizeSessionDeletion(ctx, session.ID); err != nil {
		t.Fatalf("finalize repaired deletion: %v", err)
	}
}

func TestSandboxBindingPersistsAndFencesSessionDeletion(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_sandbox_binding")
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}

	first := sandbox.Binding{
		SessionID: session.ID,
		Ref:       sandbox.Ref{Provider: "docker", ID: "container-1"},
		SpecHash:  "sha256:first",
	}
	authoritative, err := store.PutSandboxBinding(ctx, first)
	if err != nil {
		t.Fatalf("put binding: %v", err)
	}
	if authoritative != first {
		t.Fatalf("binding = %+v, want %+v", authoritative, first)
	}

	// A second worker cannot overwrite the elected provider resource.
	loser := sandbox.Binding{
		SessionID: session.ID,
		Ref:       sandbox.Ref{Provider: "docker", ID: "container-2"},
		SpecHash:  "sha256:second",
	}
	authoritative, err = store.PutSandboxBinding(ctx, loser)
	if err != nil {
		t.Fatalf("put competing binding: %v", err)
	}
	if authoritative != first {
		t.Fatalf("competing put returned %+v, want original %+v", authoritative, first)
	}

	// A fresh Store instance (standing in for a restarted worker) reads the same
	// opaque provider identity from PostgreSQL.
	restarted := NewSystemStore(store.pool, &seqIDGen{}, fixedClock{})
	got, found, err := restarted.GetSandboxBinding(ctx, session.ID)
	if err != nil || !found || got != first {
		t.Fatalf("restarted GetSandboxBinding = %+v, found=%v, err=%v", got, found, err)
	}

	if err := store.PrepareSessionDeletion(ctx, session.ID); err != nil {
		t.Fatalf("prepare deletion: %v", err)
	}
	if err := store.FinalizeSessionDeletion(ctx, session.ID); err == nil {
		t.Fatal("session deletion discarded a live sandbox binding")
	} else {
		var domainErr *domain.DomainError
		if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindConflict {
			t.Fatalf("finalize with live binding = %v, want conflict", err)
		}
	}
	if err := store.DeleteSandboxBinding(ctx, first); err != nil {
		t.Fatalf("delete binding: %v", err)
	}
	if err := store.FinalizeSessionDeletion(ctx, session.ID); err != nil {
		t.Fatalf("finalize after sandbox teardown: %v", err)
	}
}

func TestSandboxProvisioningIntentReconcilesCrashBoundaries(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_sandbox_intent")
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	intent := sandbox.ProvisioningIntent{
		SessionID: session.ID,
		Provider:  "docker",
		Spec: sandbox.Spec{
			Image:   "example.test/sandbox:fixed",
			Timeout: 15,
		},
		SpecHash: "sha256:intent",
	}
	authoritative, err := store.PutSandboxProvisioningIntent(ctx, intent)
	if err != nil {
		t.Fatalf("put provisioning intent: %v", err)
	}
	if !reflect.DeepEqual(authoritative, intent) {
		t.Fatalf("intent = %+v, want %+v", authoritative, intent)
	}
	listed, err := store.ListSandboxProvisioningIntents(ctx, "docker", 10)
	if err != nil {
		t.Fatalf("list provisioning intents: %v", err)
	}
	if len(listed) != 1 || !reflect.DeepEqual(listed[0], intent) {
		t.Fatalf("listed intents = %+v, want [%+v]", listed, intent)
	}

	// Committing the elected binding and clearing its crash-recovery intent is
	// one transaction.
	binding := sandbox.Binding{
		SessionID: session.ID,
		Ref:       sandbox.Ref{Provider: "docker", ID: "container-intent"},
		SpecHash:  intent.SpecHash,
	}
	if _, err := store.PutSandboxBinding(ctx, binding); err != nil {
		t.Fatalf("put binding: %v", err)
	}
	if _, found, err := store.GetSandboxProvisioningIntent(ctx, session.ID); err != nil || found {
		t.Fatalf("intent after binding commit: found=%v err=%v", found, err)
	}

	// A worker that observed the missing binding before this commit must not be
	// able to recreate the intent afterward.
	if _, err := store.PutSandboxProvisioningIntent(ctx, intent); !errors.Is(
		err,
		sandbox.ErrProvisioningUnavailable,
	) {
		t.Fatalf("recreate intent after binding = %v, want provisioning unavailable", err)
	}
}

func TestSandboxProvisioningIntentFencesDeletionUntilReconciled(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_sandbox_intent_delete")
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	intent := sandbox.ProvisioningIntent{
		SessionID: session.ID,
		Provider:  "docker",
		Spec:      sandbox.Spec{Image: "example.test/sandbox:fixed"},
		SpecHash:  "sha256:delete-intent",
	}
	if _, err := store.PutSandboxProvisioningIntent(ctx, intent); err != nil {
		t.Fatalf("put provisioning intent: %v", err)
	}
	if err := store.PrepareSessionDeletion(ctx, session.ID); err != nil {
		t.Fatalf("prepare deletion: %v", err)
	}
	deleting, err := store.ListDeletingSessionIDs(ctx, 10)
	if err != nil || len(deleting) != 1 || deleting[0] != session.ID {
		t.Fatalf("deleting sessions = %v, err=%v", deleting, err)
	}
	listed, err := store.ListSandboxProvisioningIntents(ctx, "docker", 10)
	if err != nil || len(listed) != 1 || !listed[0].Deleting {
		t.Fatalf("deleting intent projection = %+v, err=%v", listed, err)
	}
	if err := store.FinalizeSessionDeletion(ctx, session.ID); err == nil {
		t.Fatal("finalized deletion while provisioning intent was unresolved")
	}
	// A deletion fence also prevents a late worker from opening a new
	// provisioning obligation.
	late := intent
	late.SpecHash = "sha256:late"
	if _, err := store.PutSandboxProvisioningIntent(ctx, late); err == nil {
		t.Fatal("created provisioning intent after deletion fence")
	}
	if err := store.DeleteSandboxProvisioningIntent(ctx, intent); err != nil {
		t.Fatalf("delete provisioning intent: %v", err)
	}
	if err := store.FinalizeSessionDeletion(ctx, session.ID); err != nil {
		t.Fatalf("finalize after intent reconciliation: %v", err)
	}
}
