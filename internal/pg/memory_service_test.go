package pg

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/sandbox"
	"github.com/yanpgwang/mango/internal/sandbox/sandboxtest"
)

func TestMemoryService_PostgresOfficialSDKLifecycle(t *testing.T) {
	store := testStore(t)
	ids := domain.NewSeqIDGen()
	service := app.NewMemoryService(NewMemoryRepository(store), ids, fixedClock{})
	server := httptest.NewServer(httpapi.NewServer(httpapi.Deps{Memory: service}, httpapi.Config{
		RequireAuth: true,
	}).Handler())
	t.Cleanup(server.Close)
	client := anthropic.NewClient(option.WithBaseURL(server.URL), option.WithAuthToken("sk-memory-test"))
	ctx := context.Background()

	createdStore, err := client.Beta.MemoryStores.New(ctx, anthropic.BetaMemoryStoreNewParams{
		Name: "Project Knowledge", Description: anthropic.String("Decisions and conventions."),
		Metadata: map[string]string{"project": "mango"},
	})
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	if createdStore.ID == "" || createdStore.Type != "memory_store" || createdStore.ArchivedAt.Unix() != -62135596800 {
		t.Fatalf("created store = %s", createdStore.RawJSON())
	}

	gotStore, err := client.Beta.MemoryStores.Get(ctx, createdStore.ID, anthropic.BetaMemoryStoreGetParams{})
	if err != nil || gotStore.Metadata["project"] != "mango" {
		t.Fatalf("get store = %+v, %v", gotStore, err)
	}
	updatedStore, err := client.Beta.MemoryStores.Update(ctx, createdStore.ID, anthropic.BetaMemoryStoreUpdateParams{
		Name: anthropic.String("Project Memory"), Metadata: map[string]string{"owner": "platform"},
	})
	if err != nil || updatedStore.Name != "Project Memory" || updatedStore.Metadata["owner"] != "platform" {
		t.Fatalf("update store = %+v, %v", updatedStore, err)
	}
	stores, err := client.Beta.MemoryStores.List(ctx, anthropic.BetaMemoryStoreListParams{})
	if err != nil || len(stores.Data) != 1 || stores.Data[0].ID != createdStore.ID {
		t.Fatalf("list stores = %+v, %v", stores, err)
	}

	created, err := client.Beta.MemoryStores.Memories.New(ctx, createdStore.ID,
		anthropic.BetaMemoryStoreMemoryNewParams{
			Path: "/architecture/decisions.md", Content: anthropic.String("PostgreSQL is canonical."),
			View: anthropic.BetaManagedAgentsMemoryViewFull,
		})
	if err != nil {
		t.Fatalf("create memory: %v", err)
	}
	if created.Content != "PostgreSQL is canonical." || created.ContentSha256 == "" || created.MemoryVersionID == "" {
		t.Fatalf("created memory = %s", created.RawJSON())
	}
	firstVersionID := created.MemoryVersionID

	got, err := client.Beta.MemoryStores.Memories.Get(ctx, created.ID,
		anthropic.BetaMemoryStoreMemoryGetParams{MemoryStoreID: createdStore.ID})
	if err != nil || got.Content != created.Content {
		t.Fatalf("get memory = %+v, %v", got, err)
	}

	updated, err := client.Beta.MemoryStores.Memories.Update(ctx, created.ID,
		anthropic.BetaMemoryStoreMemoryUpdateParams{
			MemoryStoreID: createdStore.ID,
			Content:       anthropic.String("PostgreSQL is the canonical Memory source."),
			Path:          anthropic.String("/architecture/storage.md"),
			View:          anthropic.BetaManagedAgentsMemoryViewFull,
			Precondition: anthropic.BetaManagedAgentsPreconditionParam{
				Type:          anthropic.BetaManagedAgentsPreconditionTypeContentSha256,
				ContentSha256: anthropic.String(created.ContentSha256),
			},
		})
	if err != nil || updated.MemoryVersionID == firstVersionID || updated.Path != "/architecture/storage.md" {
		t.Fatalf("update memory = %+v, %v", updated, err)
	}

	// A stale precondition is successful when the stored state already equals
	// the requested state, and must not append a second no-op version.
	idempotent, err := client.Beta.MemoryStores.Memories.Update(ctx, created.ID,
		anthropic.BetaMemoryStoreMemoryUpdateParams{
			MemoryStoreID: createdStore.ID,
			Content:       anthropic.String(updated.Content),
			Path:          anthropic.String(updated.Path),
			Precondition: anthropic.BetaManagedAgentsPreconditionParam{
				Type:          anthropic.BetaManagedAgentsPreconditionTypeContentSha256,
				ContentSha256: anthropic.String(created.ContentSha256),
			},
		})
	if err != nil || idempotent.MemoryVersionID != updated.MemoryVersionID {
		t.Fatalf("idempotent stale update = %+v, %v", idempotent, err)
	}

	_, err = client.Beta.MemoryStores.Memories.Update(ctx, created.ID,
		anthropic.BetaMemoryStoreMemoryUpdateParams{
			MemoryStoreID: createdStore.ID, Content: anthropic.String("conflicting write"),
			Precondition: anthropic.BetaManagedAgentsPreconditionParam{
				Type:          anthropic.BetaManagedAgentsPreconditionTypeContentSha256,
				ContentSha256: anthropic.String(created.ContentSha256),
			},
		})
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 409 {
		t.Fatalf("stale update error = %v", err)
	}

	if _, err := client.Beta.MemoryStores.Memories.New(ctx, createdStore.ID,
		anthropic.BetaMemoryStoreMemoryNewParams{
			Path: "/architecture/nested/format.md", Content: anthropic.String("Markdown"),
		}); err != nil {
		t.Fatalf("create nested memory: %v", err)
	}
	listed, err := client.Beta.MemoryStores.Memories.List(ctx, createdStore.ID,
		anthropic.BetaMemoryStoreMemoryListParams{
			Depth: anthropic.Int(1), PathPrefix: anthropic.String("/architecture/"),
		})
	if err != nil || len(listed.Data) != 2 {
		t.Fatalf("list memories = %+v, %v", listed, err)
	}
	if listed.Data[0].Type != "memory_prefix" && listed.Data[1].Type != "memory_prefix" {
		t.Fatalf("list did not contain a rolled-up prefix: %+v", listed.Data)
	}

	firstVersion, err := client.Beta.MemoryStores.MemoryVersions.Get(ctx, firstVersionID,
		anthropic.BetaMemoryStoreMemoryVersionGetParams{
			MemoryStoreID: createdStore.ID, View: anthropic.BetaManagedAgentsMemoryViewFull,
		})
	if err != nil || firstVersion.Operation != "created" || firstVersion.Content != created.Content {
		t.Fatalf("get version = %+v, %v", firstVersion, err)
	}
	versions, err := client.Beta.MemoryStores.MemoryVersions.List(ctx, createdStore.ID,
		anthropic.BetaMemoryStoreMemoryVersionListParams{
			MemoryID: anthropic.String(created.ID), View: anthropic.BetaManagedAgentsMemoryViewFull,
		})
	if err != nil || len(versions.Data) != 2 {
		t.Fatalf("list versions = %+v, %v", versions, err)
	}
	redacted, err := client.Beta.MemoryStores.MemoryVersions.Redact(ctx, firstVersionID,
		anthropic.BetaMemoryStoreMemoryVersionRedactParams{MemoryStoreID: createdStore.ID})
	if err != nil || redacted.RedactedAt.IsZero() || redacted.JSON.Content.Raw() != "null" || redacted.JSON.Path.Raw() != "null" {
		t.Fatalf("redact version = %s, %v", redacted.RawJSON(), err)
	}

	deleted, err := client.Beta.MemoryStores.Memories.Delete(ctx, created.ID,
		anthropic.BetaMemoryStoreMemoryDeleteParams{
			MemoryStoreID: createdStore.ID, ExpectedContentSha256: anthropic.String(updated.ContentSha256),
		})
	if err != nil || deleted.ID != created.ID || deleted.Type != "memory_deleted" {
		t.Fatalf("delete memory = %+v, %v", deleted, err)
	}
	versions, err = client.Beta.MemoryStores.MemoryVersions.List(ctx, createdStore.ID,
		anthropic.BetaMemoryStoreMemoryVersionListParams{MemoryID: anthropic.String(created.ID)})
	if err != nil || len(versions.Data) != 3 || versions.Data[0].Operation != "deleted" {
		t.Fatalf("versions after delete = %+v, %v", versions, err)
	}

	archived, err := client.Beta.MemoryStores.Archive(ctx, createdStore.ID, anthropic.BetaMemoryStoreArchiveParams{})
	if err != nil || archived.ArchivedAt.IsZero() {
		t.Fatalf("archive store = %+v, %v", archived, err)
	}
	if _, err := client.Beta.MemoryStores.Memories.New(ctx, createdStore.ID,
		anthropic.BetaMemoryStoreMemoryNewParams{Path: "/blocked.md", Content: anthropic.String("x")}); err == nil {
		t.Fatal("created memory in archived store")
	}
	storeDeleted, err := client.Beta.MemoryStores.Delete(ctx, createdStore.ID, anthropic.BetaMemoryStoreDeleteParams{})
	if err != nil || storeDeleted.ID != createdStore.ID || storeDeleted.Type != "memory_store_deleted" {
		t.Fatalf("delete store = %+v, %v", storeDeleted, err)
	}
}

func TestMemoryRuntime_OpenSandboxPostgresRoundTrip(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	ids := domain.NewSeqIDGen()
	service := app.NewMemoryService(NewMemoryRepository(store), ids, fixedClock{})
	memoryStore, err := service.CreateStore(ctx, app.MemoryStoreCreateInput{Name: "Agent Memory"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CreateMemory(ctx, memoryStore.ID, app.MemoryCreateInput{
		Path: "/notes/a.md", Content: "initial",
		Actor: domain.MemoryActor{Type: "api_actor", ID: "api"},
	}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	session := newSession("sess_opensandbox_memory")
	session.CreatedAt, session.UpdatedAt = now, now
	resource := domain.SessionResource{
		ID: "sesrsc_opensandbox_memory", SessionID: session.ID,
		ResourceType:  domain.SessionResourceTypeMemoryStore,
		MemoryStoreID: memoryStore.ID, MemoryAccess: domain.MemoryAccessReadWrite,
		MemoryStoreName: memoryStore.Name, MemoryStoreDescription: memoryStore.Description,
		MountPath: "/mnt/memory/agent-memory",
		CreatedAt: now, UpdatedAt: now, State: domain.SessionResourceActive,
	}
	if _, err := store.createSession(ctx, session, nil, false,
		[]app.PreparedSessionResource{{Resource: resource}}, nil); err != nil {
		t.Fatal(err)
	}
	provider := sandboxtest.OpenSandboxProvider(t)
	_, box, err := provider.Create(ctx, t.Name(), sandbox.Spec{MemoryStores: []sandbox.MemoryStoreMount{{
		Identity: resource.ID, StoreID: memoryStore.ID,
		RuntimePath: resource.MountPath, Access: resource.MemoryAccess,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Destroy(context.Background()) })
	materializer := app.NewSessionMemoryMaterializer(store, service)
	if err := materializer.Reconcile(ctx, session.ID, box); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	read, err := box.Exec(ctx, sandbox.Command{
		Path: "sh", Args: []string{"-c", "cat /mnt/memory/agent-memory/notes/a.md"},
	})
	if err != nil || read.ExitCode != 0 || strings.TrimSpace(string(read.Stdout)) != "initial" {
		t.Fatalf("read mounted Memory: result=%+v err=%v", read, err)
	}
	locker, ok := box.(sandbox.ResourceSynchronizationSandbox)
	if !ok {
		t.Fatal("OpenSandbox sandbox does not coordinate tool operations with Memory sync")
	}
	unlockOperation, err := locker.LockResourceOperation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wave, err := box.Exec(ctx, sandbox.Command{
		Path: "sh", Args: []string{"-c", `printf wave > /mnt/memory/agent-memory/notes/a.md`},
	})
	if err != nil || wave.ExitCode != 0 {
		unlockOperation()
		t.Fatalf("change active tool wave: result=%+v err=%v", wave, err)
	}
	// A concurrent tool joins the current filesystem wave. Its pre-tool
	// Reconcile must not refresh the mount underneath the active operation.
	if err := materializer.Reconcile(ctx, session.ID, box); err != nil {
		unlockOperation()
		t.Fatalf("concurrent reconcile: %v", err)
	}
	waveRead, err := box.Exec(ctx, sandbox.Command{
		Path: "sh", Args: []string{"-c", "cat /mnt/memory/agent-memory/notes/a.md"},
	})
	if err != nil || waveRead.ExitCode != 0 || strings.TrimSpace(string(waveRead.Stdout)) != "wave" {
		unlockOperation()
		t.Fatalf("reconcile clobbered active tool data: result=%+v err=%v", waveRead, err)
	}
	writebackDone := make(chan error, 1)
	go func() {
		writebackDone <- materializer.Writeback(ctx, session.ID, box)
	}()
	select {
	case err := <-writebackDone:
		unlockOperation()
		t.Fatalf("writeback completed before the active tool wave ended: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	unlockOperation()
	select {
	case err := <-writebackDone:
		if err != nil {
			t.Fatalf("coordinated writeback: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writeback did not resume after the active tool wave ended")
	}
	waveHeads, err := service.RuntimeHeads(ctx, memoryStore.ID)
	if err != nil || len(waveHeads) != 1 || waveHeads[0].Content != "wave" {
		t.Fatalf("coordinated heads = %+v, %v", waveHeads, err)
	}
	changed, err := box.Exec(ctx, sandbox.Command{
		Path: "sh", Args: []string{"-c", `printf updated > /mnt/memory/agent-memory/notes/a.md; printf new > /mnt/memory/agent-memory/new.md`},
	})
	if err != nil || changed.ExitCode != 0 {
		t.Fatalf("change mounted Memory: result=%+v err=%v", changed, err)
	}
	if err := materializer.Writeback(ctx, session.ID, box); err != nil {
		t.Fatalf("writeback: %v", err)
	}
	heads, err := service.RuntimeHeads(ctx, memoryStore.ID)
	if err != nil || len(heads) != 2 || heads[0].Content != "new" || heads[1].Content != "updated" {
		t.Fatalf("persisted heads = %+v, %v", heads, err)
	}
	versions, err := service.ListMemoryVersions(ctx, memoryStore.ID, app.MemoryVersionListQuery{
		SessionID: session.ID, Limit: 100,
	})
	if err != nil || len(versions.Versions) != 3 {
		t.Fatalf("session-authored versions = %+v, %v", versions, err)
	}
	// Simulate a worker crash after a final tool changed the mount but before the
	// ordinary post-tool hook ran. The deletion path flushes the dirty mount
	// before destroying the sandbox, so the final Memory is not lost.
	last, err := box.Exec(ctx, sandbox.Command{
		Path: "sh", Args: []string{"-c", `printf final > /mnt/memory/agent-memory/notes/a.md`},
	})
	if err != nil || last.ExitCode != 0 {
		t.Fatalf("final Memory change: result=%+v err=%v", last, err)
	}
	if err := store.PrepareSessionDeletion(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	mounts, err := materializer.MemoryStoreMountsForRelease(ctx, session.ID)
	if err != nil || len(mounts) != 1 || mounts[0].RuntimePath != resource.MountPath {
		t.Fatalf("release mounts = %+v, %v", mounts, err)
	}
	if err := materializer.WritebackForRelease(ctx, session.ID, box); err != nil {
		t.Fatalf("release writeback: %v", err)
	}
	finalHeads, err := service.RuntimeHeads(ctx, memoryStore.ID)
	if err != nil || len(finalHeads) != 2 || finalHeads[1].Content != "final" {
		t.Fatalf("final persisted heads = %+v, %v", finalHeads, err)
	}
	if err := box.Destroy(ctx); err != nil {
		t.Fatal(err)
	}
	if err := materializer.CleanupSession(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeSessionDeletion(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryStoreSessionResource_PostgresSnapshotLifecycle(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	ids := domain.NewSeqIDGen()
	memory := app.NewMemoryService(NewMemoryRepository(store), ids, fixedClock{})
	memoryStore, err := memory.CreateStore(ctx, app.MemoryStoreCreateInput{
		Name: "Project Knowledge", Description: "Shared conventions.",
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	session := newSession("sess_memory")
	session.CreatedAt, session.UpdatedAt = now, now
	resource := domain.SessionResource{
		ID: "sesrsc_memory", SessionID: session.ID,
		ResourceType:           domain.SessionResourceTypeMemoryStore,
		MemoryStoreID:          memoryStore.ID,
		MemoryAccess:           domain.MemoryAccessReadWrite,
		MemoryInstructions:     "Keep architectural decisions current.",
		MemoryStoreName:        memoryStore.Name,
		MemoryStoreDescription: memoryStore.Description,
		MountPath:              "/mnt/memory/project-knowledge",
		CreatedAt:              now, UpdatedAt: now, State: domain.SessionResourceActive,
	}
	admission, err := store.createSession(
		ctx,
		session,
		nil,
		false,
		[]app.PreparedSessionResource{{Resource: resource}},
		nil,
	)
	if err != nil {
		t.Fatalf("create Session with Memory Store: %v", err)
	}
	if len(admission.Session.Resources) != 1 ||
		admission.Session.Resources[0].MemoryStoreID != memoryStore.ID {
		t.Fatalf("Session resources = %+v", admission.Session.Resources)
	}
	got, err := store.GetSessionResource(ctx, session.ID, resource.ID)
	if err != nil || got.Type() != domain.SessionResourceTypeMemoryStore ||
		got.MemoryStoreName != memoryStore.Name || got.MemoryInstructions != resource.MemoryInstructions {
		t.Fatalf("stored Memory Resource = %+v, %v", got, err)
	}
	filtered, err := store.ListSessions(ctx, app.ListPage{
		MemoryStoreID: &memoryStore.ID, Limit: 100,
	})
	if err != nil || len(filtered.Sessions) != 1 || filtered.Sessions[0].ID != session.ID {
		t.Fatalf("Memory Store Session filter = %+v, %v", filtered, err)
	}
	if _, err := memory.ArchiveStore(ctx, memoryStore.ID); err != nil {
		t.Fatal(err)
	}
	if err := memory.DeleteStore(ctx, memoryStore.ID); err == nil {
		t.Fatal("deleted a Memory Store still attached to a Session")
	}
	if err := store.PrepareSessionDeletion(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeSessionMemoryResources(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeSessionDeletion(ctx, session.ID); err != nil {
		t.Fatal(err)
	}
	if err := memory.DeleteStore(ctx, memoryStore.ID); err != nil {
		t.Fatalf("delete detached Memory Store: %v", err)
	}
}

func TestMemoryRuntimeSync_IsAtomicAndVersioned(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	ids := domain.NewSeqIDGen()
	service := app.NewMemoryService(NewMemoryRepository(store), ids, fixedClock{})
	memoryStore, err := service.CreateStore(ctx, app.MemoryStoreCreateInput{Name: "Runtime"})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.CreateMemory(ctx, memoryStore.ID, app.MemoryCreateInput{
		Path: "/a.md", Content: "A", Actor: domain.MemoryActor{Type: "api_actor", ID: "api"},
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.CreateMemory(ctx, memoryStore.ID, app.MemoryCreateInput{
		Path: "/b.md", Content: "B", Actor: domain.MemoryActor{Type: "api_actor", ID: "api"},
	})
	if err != nil {
		t.Fatal(err)
	}
	baseline := []app.MemoryStoreSyncBaseline{
		{MemoryID: first.ID, Path: first.Path, ContentSHA256: first.ContentSHA256},
		{MemoryID: second.ID, Path: second.Path, ContentSHA256: second.ContentSHA256},
	}
	heads, err := service.SyncRuntimeSnapshot(
		ctx,
		memoryStore.ID,
		baseline,
		[]app.MemoryStoreSyncContent{{Path: "/a.md", Content: "A2"}, {Path: "/c.md", Content: "C"}},
		domain.MemoryActor{Type: "session_actor", ID: "sess_runtime"},
	)
	if err != nil {
		t.Fatalf("sync runtime snapshot: %v", err)
	}
	if len(heads) != 2 || heads[0].Path != "/a.md" || heads[0].Content != "A2" ||
		heads[1].Path != "/c.md" {
		t.Fatalf("synced heads = %+v", heads)
	}
	versions, err := service.ListMemoryVersions(ctx, memoryStore.ID, app.MemoryVersionListQuery{
		SessionID: "sess_runtime", Limit: 100,
	})
	if err != nil || len(versions.Versions) != 3 {
		t.Fatalf("Session versions = %+v, %v", versions, err)
	}

	// Establish a fresh baseline, then race one remote update against two local
	// edits. The conflict is detected before any local mutation is committed.
	fresh, err := service.RuntimeHeads(ctx, memoryStore.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale := make([]app.MemoryStoreSyncBaseline, 0, len(fresh))
	for _, head := range fresh {
		stale = append(stale, app.MemoryStoreSyncBaseline{
			MemoryID: head.ID, Path: head.Path, ContentSHA256: head.ContentSHA256,
		})
	}
	var cHead domain.Memory
	for _, head := range fresh {
		if head.Path == "/c.md" {
			cHead = head
		}
	}
	remote := "remote"
	if _, err := service.UpdateMemory(ctx, memoryStore.ID, cHead.ID, app.MemoryUpdateInput{
		Content:            &remote,
		ExpectedContentSHA: &cHead.ContentSHA256,
		Actor:              domain.MemoryActor{Type: "api_actor", ID: "api"},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = service.SyncRuntimeSnapshot(
		ctx,
		memoryStore.ID,
		stale,
		[]app.MemoryStoreSyncContent{{Path: "/a.md", Content: "local-a"}, {Path: "/c.md", Content: "local-c"}},
		domain.MemoryActor{Type: "session_actor", ID: "sess_runtime"},
	)
	var domainErr *domain.DomainError
	if !errors.As(err, &domainErr) || domainErr.Code != "memory_precondition_failed_error" {
		t.Fatalf("conflicting sync error = %v", err)
	}
	aAfter, err := service.GetMemory(ctx, memoryStore.ID, heads[0].ID)
	if err != nil || aAfter.Content != "A2" {
		t.Fatalf("atomic sync changed /a.md before conflict: %+v, %v", aAfter, err)
	}
}
