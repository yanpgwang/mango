package pg

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	mango "github.com/yanpgwang/mango/sdk/go"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
)

func TestMemoryService_PostgresMangoSDKLifecycle(t *testing.T) {
	store := testStore(t)
	service := app.NewMemoryService(NewMemoryRepository(store), domain.NewSeqIDGen(), fixedClock{})
	server := httptest.NewServer(httpapi.NewServer(httpapi.Deps{Memory: service}, httpapi.Config{RequireAuth: true}).Handler())
	t.Cleanup(server.Close)
	client, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: "memory-test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	createdStore, err := client.MemoryStores.New(ctx, mango.MemoryStoreCreateRequest{
		Name: "Project Knowledge", Description: mango.Some("Decisions and conventions."),
		Metadata: mango.Some(mango.MemoryMetadata{"project": "mango"}),
	})
	if err != nil || createdStore.ID == "" || createdStore.Type != "memory_store" || createdStore.ArchivedAt != nil {
		t.Fatalf("create: %+v, %v", createdStore, err)
	}
	gotStore, err := client.MemoryStores.Get(ctx, createdStore.ID)
	if err != nil || gotStore.Metadata["project"] != "mango" {
		t.Fatalf("get: %+v, %v", gotStore, err)
	}
	owner := "platform"
	updatedStore, err := client.MemoryStores.Update(ctx, createdStore.ID, mango.MemoryStoreUpdateRequest{Name: mango.Some("Project Memory"), Metadata: mango.Some(mango.MemoryMetadataPatch{"owner": &owner})})
	if err != nil || updatedStore.Name != "Project Memory" || updatedStore.Metadata["owner"] != owner {
		t.Fatalf("update store: %+v, %v", updatedStore, err)
	}
	stores, err := client.MemoryStores.List(ctx, mango.ListMemoryStoresParams{})
	if err != nil || len(stores.Data) != 1 || stores.Data[0].ID != createdStore.ID {
		t.Fatalf("list stores: %+v, %v", stores, err)
	}
	created, err := client.MemoryStores.Memories.New(ctx, createdStore.ID, mango.CreateMemoryParams{View: mango.Some("full")}, mango.MemoryCreateRequest{Path: "/architecture/decisions.md", Content: "PostgreSQL is canonical."})
	if err != nil || created.Content == nil || *created.Content != "PostgreSQL is canonical." || created.ContentSHA256 == "" || created.MemoryVersionID == "" {
		t.Fatalf("create memory: %+v, %v", created, err)
	}
	firstVersionID := created.MemoryVersionID
	got, err := client.MemoryStores.Memories.Get(ctx, createdStore.ID, created.ID, mango.GetMemoryParams{View: mango.Some("full")})
	if err != nil || got.Content == nil || *got.Content != *created.Content {
		t.Fatalf("get memory: %+v, %v", got, err)
	}
	update := mango.MemoryUpdateRequest{
		Content: mango.Some("PostgreSQL is the canonical Memory source."), Path: mango.Some(mango.MemoryPath("/architecture/storage.md")),
		Precondition: mango.Some(mango.MemoryPrecondition{Type: "content_sha256", ContentSHA256: created.ContentSHA256}),
	}
	updated, err := client.MemoryStores.Memories.Update(ctx, createdStore.ID, created.ID, mango.UpdateMemoryParams{View: mango.Some("full")}, update)
	if err != nil || updated.MemoryVersionID == firstVersionID || updated.Path != "/architecture/storage.md" {
		t.Fatalf("update memory: %+v, %v", updated, err)
	}
	// A stale precondition may confirm already-committed state without another version.
	idempotent, err := client.MemoryStores.Memories.Update(ctx, createdStore.ID, created.ID, mango.UpdateMemoryParams{}, update)
	if err != nil || idempotent.MemoryVersionID != updated.MemoryVersionID {
		t.Fatalf("idempotent update: %+v, %v", idempotent, err)
	}
	update.Content = mango.Some("conflicting write")
	_, err = client.MemoryStores.Memories.Update(ctx, createdStore.ID, created.ID, mango.UpdateMemoryParams{}, update)
	var apiErr *mango.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 409 {
		t.Fatalf("stale update: %v", err)
	}
	if _, err := client.MemoryStores.Memories.New(ctx, createdStore.ID, mango.CreateMemoryParams{}, mango.MemoryCreateRequest{Path: "/architecture/nested/format.md", Content: "Markdown"}); err != nil {
		t.Fatal(err)
	}
	listed, err := client.MemoryStores.Memories.List(ctx, createdStore.ID, mango.ListMemoriesParams{Depth: mango.Some[int64](1), PathPrefix: mango.Some("/architecture/")})
	if err != nil || len(listed.Data) != 2 {
		t.Fatalf("list memories: %+v, %v", listed, err)
	}
	if listed.Data[0].MemoryPrefix == nil && listed.Data[1].MemoryPrefix == nil {
		t.Fatalf("no rolled-up prefix: %+v", listed.Data)
	}
	firstVersion, err := client.MemoryStores.Versions.Get(ctx, createdStore.ID, firstVersionID, mango.GetMemoryVersionParams{View: mango.Some("full")})
	if err != nil || firstVersion.Operation != "created" || firstVersion.Content == nil || *firstVersion.Content != *created.Content {
		t.Fatalf("first version: %+v, %v", firstVersion, err)
	}
	versions, err := client.MemoryStores.Versions.List(ctx, createdStore.ID, mango.ListMemoryVersionsParams{MemoryID: mango.Some(created.ID), View: mango.Some("full")})
	if err != nil || len(versions.Data) != 2 {
		t.Fatalf("versions: %+v, %v", versions, err)
	}
	redacted, err := client.MemoryStores.Versions.Redact(ctx, createdStore.ID, firstVersionID)
	if err != nil || redacted.RedactedAt == nil || redacted.Content != nil || redacted.Path != nil {
		t.Fatalf("redacted: %+v, %v", redacted, err)
	}
	deleted, err := client.MemoryStores.Memories.Delete(ctx, createdStore.ID, created.ID, mango.DeleteMemoryParams{ExpectedContentSHA256: mango.Some(string(updated.ContentSHA256))})
	if err != nil || deleted.ID != created.ID || deleted.Type != "memory_deleted" {
		t.Fatalf("delete memory: %+v, %v", deleted, err)
	}
	versions, err = client.MemoryStores.Versions.List(ctx, createdStore.ID, mango.ListMemoryVersionsParams{MemoryID: mango.Some(created.ID)})
	if err != nil || len(versions.Data) != 3 || versions.Data[0].Operation != "deleted" {
		t.Fatalf("deleted versions: %+v, %v", versions, err)
	}
	archived, err := client.MemoryStores.Archive(ctx, createdStore.ID)
	if err != nil || archived.ArchivedAt == nil {
		t.Fatalf("archive store: %+v, %v", archived, err)
	}
	if _, err := client.MemoryStores.Memories.New(ctx, createdStore.ID, mango.CreateMemoryParams{}, mango.MemoryCreateRequest{Path: "/blocked.md", Content: "x"}); err == nil {
		t.Fatal("created memory in archived store")
	}
	storeDeleted, err := client.MemoryStores.Delete(ctx, createdStore.ID)
	if err != nil || storeDeleted.ID != createdStore.ID || storeDeleted.Type != "memory_store_deleted" {
		t.Fatalf("delete store: %+v, %v", storeDeleted, err)
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
