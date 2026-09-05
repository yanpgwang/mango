package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/pg"
	"github.com/yanpgwang/mango/internal/sandbox"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
)

func TestPostgresSessionFileResourcesAreIndependentAndRecoverable(t *testing.T) {
	fixture := newPostgresFixture(t)
	ctx := context.Background()
	blobs := newResourceBlobStore()
	fileRepository := pg.NewFileRepository(fixture.store)
	files := app.NewFileService(fileRepository, blobs, fixture.ids, fixture.clock)
	resources := NewSessionResourceService(
		fixture.store, fileRepository, blobs, fixture.ids, fixture.clock, true,
	)
	sessions := NewSessionService(
		fixture.store,
		fixture.agentRepo,
		fixture.environmentRepo,
		temporalpkg.NewOrchestrator(fixture.store, nil),
		fixture.ids,
		fixture.clock,
		nil,
		resources,
	)
	handler := httpapi.NewServer(httpapi.Deps{
		Agents: app.NewAgentService(fixture.agentRepo, fixture.ids, fixture.clock),
		Envs: app.NewEnvironmentService(
			fixture.environmentRepo,
			fixture.ids,
			fixture.clock,
			app.EnvironmentCapabilities{},
		),
		Sessions:         sessions,
		Events:           NewEventService(fixture.store),
		Stream:           app.NewHub(64),
		Files:            files,
		SessionResources: resources,
	}, httpapi.Config{}).Handler()
	disabledResources := NewSessionResourceService(
		fixture.store, fileRepository, blobs, fixture.ids, fixture.clock, false,
	)
	disabledSessions := NewSessionService(
		fixture.store,
		fixture.agentRepo,
		fixture.environmentRepo,
		temporalpkg.NewOrchestrator(fixture.store, nil),
		fixture.ids,
		fixture.clock,
		nil,
		disabledResources,
	)
	disabledHandler := httpapi.NewServer(httpapi.Deps{
		Agents: app.NewAgentService(fixture.agentRepo, fixture.ids, fixture.clock),
		Envs: app.NewEnvironmentService(
			fixture.environmentRepo,
			fixture.ids,
			fixture.clock,
			app.EnvironmentCapabilities{},
		),
		Sessions:         disabledSessions,
		Events:           NewEventService(fixture.store),
		Stream:           app.NewHub(64),
		Files:            files,
		SessionResources: disabledResources,
	}, httpapi.Config{}).Handler()

	source, err := files.Upload(ctx, app.FileUploadInput{
		Filename: "source.txt", MimeType: "text/plain",
		Body: bytes.NewBufferString("source survives attachment"),
	})
	if err != nil {
		t.Fatalf("upload source: %v", err)
	}
	agentID := createResource(t, handler, "/v1/agents",
		`{"name":"coder","model":"claude-test"}`)
	environmentID := createResource(t, handler, "/v1/environments",
		`{"name":"cloud","config":{"type":"cloud"}}`)
	disabledResponse := request(t, disabledHandler, http.MethodPost, "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`",`+
			`"resources":[{"type":"file","file_id":"`+source.ID+`"}]}`)
	if disabledResponse.Code != http.StatusUnprocessableEntity {
		t.Fatalf(
			"create resource on provider-disabled deployment -> %d: %s",
			disabledResponse.Code,
			disabledResponse.Body.String(),
		)
	}
	response := request(t, handler, http.MethodPost, "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`",`+
			`"resources":[{"type":"file","file_id":"`+source.ID+`"}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("create session with File Resource -> %d: %s", response.Code, response.Body.String())
	}
	var sessionResponse struct {
		ID        string `json:"id"`
		Resources []struct {
			ID        string `json:"id"`
			FileID    string `json:"file_id"`
			MountPath string `json:"mount_path"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &sessionResponse); err != nil {
		t.Fatal(err)
	}
	if len(sessionResponse.Resources) != 1 {
		t.Fatalf("create resources = %+v", sessionResponse.Resources)
	}
	first := sessionResponse.Resources[0]
	if first.FileID == source.ID || first.MountPath != domain.SessionUploadsRoot+"/"+source.ID {
		t.Fatalf("created File Resource = %+v", first)
	}
	clone, err := fileRepository.Get(ctx, first.FileID)
	if err != nil {
		t.Fatalf("get session File clone: %v", err)
	}
	if !clone.Downloadable || clone.Scope == nil ||
		clone.Scope.ID != sessionResponse.ID || clone.Scope.Type != "session" {
		t.Fatalf("session File clone = %+v", clone)
	}

	if _, err := files.Delete(ctx, source.ID); err != nil {
		t.Fatalf("delete source upload: %v", err)
	}
	if _, err := files.Download(ctx, first.FileID); err != nil {
		t.Fatalf("attached copy stopped being downloadable: %v", err)
	}
	response = request(t, handler, http.MethodDelete, "/v1/files/"+first.FileID, "")
	if response.Code != http.StatusConflict {
		t.Fatalf("delete Session Resource copy through Files API -> %d: %s", response.Code, response.Body.String())
	}
	if _, err := files.Download(ctx, first.FileID); err != nil {
		t.Fatalf("rejected Files delete changed attached copy: %v", err)
	}

	materializer := app.NewSessionResourceMaterializer(
		fixture.store, fileRepository, blobs,
	)
	firstBox := newResourceSandbox()
	if err := materializer.Reconcile(ctx, sessionResponse.ID, firstBox); err != nil {
		t.Fatalf("materialize create-time resource: %v", err)
	}
	if got := string(firstBox.files[first.MountPath]); got != "source survives attachment" {
		t.Fatalf("mounted create-time resource = %q", got)
	}

	secondSource, err := files.Upload(ctx, app.FileUploadInput{
		Filename: "runtime.txt", MimeType: "text/plain",
		Body: bytes.NewBufferString("runtime attachment"),
	})
	if err != nil {
		t.Fatal(err)
	}
	response = request(t, handler, http.MethodPost,
		"/v1/sessions/"+sessionResponse.ID+"/resources",
		`{"type":"file","file_id":"`+secondSource.ID+`","mount_path":"/runtime/data.txt"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("runtime add -> %d: %s", response.Code, response.Body.String())
	}
	var second domainResourceJSON
	if err := json.Unmarshal(response.Body.Bytes(), &second); err != nil {
		t.Fatal(err)
	}
	if second.MountPath != domain.SessionUploadsRoot+"/runtime/data.txt" {
		t.Fatalf("runtime mount path = %q", second.MountPath)
	}
	response = request(t, handler, http.MethodPost,
		"/v1/sessions/"+sessionResponse.ID+"/resources",
		`{"type":"file","file_id":"`+secondSource.ID+`","mount_path":"/runtime"}`)
	if response.Code != http.StatusConflict {
		t.Fatalf("overlapping runtime mount -> %d: %s", response.Code, response.Body.String())
	}
	if err := materializer.Reconcile(ctx, sessionResponse.ID, firstBox); err != nil {
		t.Fatalf("materialize runtime resource: %v", err)
	}
	if got := string(firstBox.files[second.MountPath]); got != "runtime attachment" {
		t.Fatalf("mounted runtime resource = %q", got)
	}

	restartedBox := newResourceSandbox()
	if err := materializer.Reconcile(ctx, sessionResponse.ID, restartedBox); err != nil {
		t.Fatalf("restore resources after worker restart: %v", err)
	}
	if len(restartedBox.files) != 2 {
		t.Fatalf("restored mount count = %d, want 2", len(restartedBox.files))
	}

	response = request(t, disabledHandler, http.MethodDelete,
		"/v1/sessions/"+sessionResponse.ID+"/resources/"+second.ID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("delete runtime resource -> %d: %s", response.Code, response.Body.String())
	}
	if _, err := resources.Get(ctx, sessionResponse.ID, second.ID); err == nil {
		t.Fatal("deleting resource remains public")
	}
	response = request(t, handler, http.MethodPost,
		"/v1/sessions/"+sessionResponse.ID+"/resources",
		`{"type":"file","file_id":"`+secondSource.ID+`","mount_path":"/runtime/data.txt"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("immediate re-add at deleting mount -> %d: %s", response.Code, response.Body.String())
	}
	var replacement domainResourceJSON
	if err := json.Unmarshal(response.Body.Bytes(), &replacement); err != nil {
		t.Fatal(err)
	}
	if err := materializer.Reconcile(ctx, sessionResponse.ID, restartedBox); err != nil {
		t.Fatalf("reconcile delete and immediate re-add: %v", err)
	}
	if got := string(restartedBox.files[replacement.MountPath]); got != "runtime attachment" {
		t.Fatalf("replacement resource mount = %q", got)
	}
	if _, err := fileRepository.Get(ctx, second.FileID); err == nil {
		t.Fatal("deleted resource File remains public")
	}
	response = request(t, handler, http.MethodDelete,
		"/v1/sessions/"+sessionResponse.ID+"/resources/"+replacement.ID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("delete replacement resource -> %d: %s", response.Code, response.Body.String())
	}
	if err := materializer.Reconcile(ctx, sessionResponse.ID, restartedBox); err != nil {
		t.Fatalf("reconcile replacement deletion: %v", err)
	}
	if _, present := restartedBox.files[replacement.MountPath]; present {
		t.Fatal("deleted replacement resource remains mounted")
	}
	remaining, err := fixture.store.SessionResourcesForReconcile(ctx, sessionResponse.ID)
	if err != nil || len(remaining) != 1 || remaining[0].ID != first.ID {
		t.Fatalf("remaining durable resources = %+v, err=%v", remaining, err)
	}
	if err := sessions.Delete(ctx, sessionResponse.ID); err != nil {
		t.Fatalf("delete Session with remaining File Resource: %v", err)
	}
	if _, err := fixture.store.GetSession(ctx, sessionResponse.ID); err == nil {
		t.Fatal("deleted Session remains visible")
	}
	if _, err := fileRepository.Get(ctx, first.FileID); err == nil {
		t.Fatal("Session deletion left its scoped File visible")
	}
	if blobs.has("files/" + first.FileID) {
		t.Fatal("Session deletion left its scoped File object")
	}
}

func TestPostgresGitRepositoryResourceFreezesSnapshotAndMaterializesOffline(t *testing.T) {
	fixture := newPostgresFixture(t)
	ctx := context.Background()
	blobs := newResourceBlobStore()
	fileRepository := pg.NewFileRepository(fixture.store)
	files := app.NewFileService(fileRepository, blobs, fixture.ids, fixture.clock)
	resources := NewSessionResourceService(
		fixture.store, fileRepository, blobs, fixture.ids, fixture.clock, true,
	)
	snapshotter := &staticGitRepositorySnapshotter{
		commit:  "0123456789abcdef0123456789abcdef01234567",
		archive: []byte("immutable repository snapshot"),
	}
	resources.EnableGitRepositoryResources(snapshotter)
	sessions := NewSessionService(
		fixture.store, fixture.agentRepo, fixture.environmentRepo,
		temporalpkg.NewOrchestrator(fixture.store, nil), fixture.ids, fixture.clock,
		nil, resources,
	)
	handler := httpapi.NewServer(httpapi.Deps{
		Agents: app.NewAgentService(fixture.agentRepo, fixture.ids, fixture.clock),
		Envs: app.NewEnvironmentService(
			fixture.environmentRepo, fixture.ids, fixture.clock,
			app.EnvironmentCapabilities{},
		),
		Sessions: sessions, Events: NewEventService(fixture.store),
		Stream: app.NewHub(64), Files: files, SessionResources: resources,
	}, httpapi.Config{}).Handler()
	agentID := createResource(t, handler, "/v1/agents",
		`{"name":"repo-coder","model":"claude-test"}`)
	environmentID := createResource(t, handler, "/v1/environments",
		`{"name":"repo-cloud","config":{"type":"cloud"}}`)
	response := request(t, handler, http.MethodPost, "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`",`+
			`"resources":[{"type":"git_repository",`+
			`"url":"https://github.com/acme/widgets.git",`+
			`"checkout":{"type":"branch","name":"main"}}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("create Git repository Session -> %d: %s", response.Code, response.Body.String())
	}
	var created struct {
		ID        string `json:"id"`
		Resources []struct {
			ID             string         `json:"id"`
			Type           string         `json:"type"`
			URL            string         `json:"url"`
			Checkout       map[string]any `json:"checkout"`
			ResolvedCommit string         `json:"resolved_commit"`
			MountPath      string         `json:"mount_path"`
			FileID         string         `json:"file_id"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Resources) != 1 {
		t.Fatalf("resources = %+v", created.Resources)
	}
	repository := created.Resources[0]
	if repository.Type != domain.SessionResourceTypeGitRepository ||
		repository.URL != "https://github.com/acme/widgets.git" ||
		repository.ResolvedCommit != snapshotter.commit ||
		repository.MountPath != "/workspace/widgets" || repository.FileID != "" ||
		repository.Checkout["type"] != "branch" || repository.Checkout["name"] != "main" {
		t.Fatalf("Git repository response = %+v", repository)
	}
	if len(snapshotter.requests) != 1 ||
		snapshotter.requests[0].CheckoutType != domain.GitRepositoryCheckoutBranch ||
		snapshotter.requests[0].CheckoutValue != "main" {
		t.Fatalf("snapshot requests = %+v", snapshotter.requests)
	}
	durableResources, err := fixture.store.SessionResourcesForReconcile(ctx, created.ID)
	if err != nil || len(durableResources) != 1 {
		t.Fatalf("durable Git resources = %+v, err=%v", durableResources, err)
	}
	archiveID := durableResources[0].FileID
	archive, err := fileRepository.Get(ctx, archiveID)
	if err != nil || !archive.Internal || archive.Downloadable {
		t.Fatalf("internal repository archive = %+v, err=%v", archive, err)
	}
	response = request(t, handler, http.MethodGet, "/v1/files?scope_id="+created.ID, "")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"data":[]`) {
		t.Fatalf(
			"repository archive leaked through Files list -> %d: %s",
			response.Code, response.Body.String(),
		)
	}
	for _, endpoint := range []string{
		"/v1/files/" + archiveID,
		"/v1/files/" + archiveID + "/content",
	} {
		response = request(t, handler, http.MethodGet, endpoint, "")
		if response.Code != http.StatusNotFound {
			t.Fatalf(
				"internal repository archive GET %s -> %d: %s",
				endpoint, response.Code, response.Body.String(),
			)
		}
	}
	response = request(t, handler, http.MethodDelete, "/v1/files/"+archiveID, "")
	if response.Code != http.StatusNotFound {
		t.Fatalf(
			"internal repository archive delete -> %d: %s",
			response.Code, response.Body.String(),
		)
	}

	box := newResourceSandbox()
	materializer := app.NewSessionResourceMaterializer(fixture.store, fileRepository, blobs)
	if err := materializer.Reconcile(ctx, created.ID, box); err != nil {
		t.Fatal(err)
	}
	if got := string(box.repositories[repository.MountPath]); got != string(snapshotter.archive) {
		t.Fatalf("materialized repository snapshot = %q", got)
	}
	response = request(
		t, handler, http.MethodDelete,
		"/v1/sessions/"+created.ID+"/resources/"+repository.ID, "",
	)
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("runtime repository detach -> %d: %s", response.Code, response.Body.String())
	}
}

func TestPostgresDeploymentResolvesGitRepositoryForEachRun(t *testing.T) {
	fixture := newPostgresFixture(t)
	ctx := context.Background()
	blobs := newResourceBlobStore()
	fileRepository := pg.NewFileRepository(fixture.store)
	resources := NewSessionResourceService(
		fixture.store, fileRepository, blobs, fixture.ids, fixture.clock, true,
	)
	snapshotter := &staticGitRepositorySnapshotter{
		commit:  "0123456789abcdef0123456789abcdef01234567",
		archive: []byte("first repository snapshot"),
	}
	resources.EnableGitRepositoryResources(snapshotter)
	sessions := NewSessionService(
		fixture.store, fixture.agentRepo, fixture.environmentRepo,
		temporalpkg.NewOrchestrator(fixture.store, nil), fixture.ids, fixture.clock,
		nil, resources,
	)
	handler := httpapi.NewServer(httpapi.Deps{
		Agents: app.NewAgentService(fixture.agentRepo, fixture.ids, fixture.clock),
		Envs: app.NewEnvironmentService(
			fixture.environmentRepo, fixture.ids, fixture.clock,
			app.EnvironmentCapabilities{},
		),
	}, httpapi.Config{}).Handler()
	agentID := createResource(t, handler, "/v1/agents",
		`{"name":"deployment-repo-coder","model":"claude-test"}`)
	environmentID := createResource(t, handler, "/v1/environments",
		`{"name":"deployment-repo-cloud","config":{"type":"cloud"}}`)
	deployments := app.NewDeploymentService(app.DeploymentServiceConfig{
		Repository: pg.NewDeploymentRepository(fixture.store),
		Agents:     fixture.agentRepo, Environments: fixture.environmentRepo,
		Sessions: sessions, IDGenerator: fixture.ids, Clock: fixture.clock,
	})
	deployment, err := deployments.Create(ctx, app.DeploymentCreateInput{
		AgentID: agentID, EnvironmentID: environmentID, Name: "Repository deployment",
		InitialEvents: []domain.EventDraft{{Type: domain.EvUserMessage}},
		Resources: []domain.DeploymentResource{{
			Type:                    domain.SessionResourceTypeGitRepository,
			RepositoryURL:           "https://github.com/acme/widgets.git",
			RepositoryCheckoutType:  domain.GitRepositoryCheckoutBranch,
			RepositoryCheckoutValue: "main",
		}},
	})
	if err != nil {
		t.Fatalf("create Deployment: %v", err)
	}
	firstRun, err := deployments.Run(ctx, deployment.ID)
	if err != nil || firstRun.SessionID == nil {
		t.Fatalf("first Deployment Run = %+v, %v", firstRun, err)
	}
	firstResources, err := fixture.store.ListSessionResources(
		ctx, *firstRun.SessionID, app.SessionResourceListQuery{Limit: 500},
	)
	if err != nil || len(firstResources.Resources) != 1 ||
		firstResources.Resources[0].RepositoryResolvedCommit != snapshotter.commit {
		t.Fatalf("first Run resources = %+v, %v", firstResources.Resources, err)
	}

	snapshotter.commit = "89abcdef0123456789abcdef0123456789abcdef"
	snapshotter.archive = []byte("second repository snapshot")
	secondRun, err := deployments.Run(ctx, deployment.ID)
	if err != nil || secondRun.SessionID == nil {
		t.Fatalf("second Deployment Run = %+v, %v", secondRun, err)
	}
	secondResources, err := fixture.store.ListSessionResources(
		ctx, *secondRun.SessionID, app.SessionResourceListQuery{Limit: 500},
	)
	if err != nil || len(secondResources.Resources) != 1 ||
		secondResources.Resources[0].RepositoryResolvedCommit != snapshotter.commit {
		t.Fatalf("second Run resources = %+v, %v", secondResources.Resources, err)
	}
	if len(snapshotter.requests) != 2 ||
		snapshotter.requests[0].CheckoutType != domain.GitRepositoryCheckoutBranch ||
		snapshotter.requests[1].CheckoutValue != "main" {
		t.Fatalf("Deployment snapshot requests = %+v", snapshotter.requests)
	}

	materializer := app.NewSessionResourceMaterializer(fixture.store, fileRepository, blobs)
	firstBox, secondBox := newResourceSandbox(), newResourceSandbox()
	if err := materializer.Reconcile(ctx, *firstRun.SessionID, firstBox); err != nil {
		t.Fatal(err)
	}
	if err := materializer.Reconcile(ctx, *secondRun.SessionID, secondBox); err != nil {
		t.Fatal(err)
	}
	if got := string(firstBox.repositories["/workspace/widgets"]); got != "first repository snapshot" {
		t.Fatalf("first Run materialized snapshot = %q", got)
	}
	if got := string(secondBox.repositories["/workspace/widgets"]); got != "second repository snapshot" {
		t.Fatalf("second Run materialized snapshot = %q", got)
	}
}

func TestPostgresSessionMemoryStoreResourceSnapshotsAndOwnsNoStore(t *testing.T) {
	fixture := newPostgresFixture(t)
	ctx := context.Background()
	memory := app.NewMemoryService(
		pg.NewMemoryRepository(fixture.store), fixture.ids, fixture.clock,
	)
	sessions := NewSessionService(
		fixture.store,
		fixture.agentRepo,
		fixture.environmentRepo,
		temporalpkg.NewOrchestrator(fixture.store, nil),
		fixture.ids,
		fixture.clock,
		nil,
	)
	sessions.EnableMemoryStoreResources(memory)
	// A legacy cloud provider without Memory mounts must not gate the external
	// self-hosted worker, which synchronizes through the public Memory API.
	sessions.ConfigureCloudMemoryStores(false)
	handler := httpapi.NewServer(httpapi.Deps{
		Agents: app.NewAgentService(fixture.agentRepo, fixture.ids, fixture.clock),
		Envs: app.NewEnvironmentService(
			fixture.environmentRepo, fixture.ids, fixture.clock,
			app.EnvironmentCapabilities{},
		),
		Sessions: sessions,
		Events:   NewEventService(fixture.store),
		Stream:   app.NewHub(64),
		Memory:   memory,
	}, httpapi.Config{}).Handler()
	memoryStore, err := memory.CreateStore(ctx, app.MemoryStoreCreateInput{
		Name: "Project Knowledge", Description: "Shared project conventions.",
	})
	if err != nil {
		t.Fatal(err)
	}
	agentID := createResource(t, handler, "/v1/agents",
		`{"name":"coder","model":"claude-test"}`)
	environmentID := createResource(t, handler, "/v1/environments",
		`{"name":"self hosted","config":{"type":"self_hosted"}}`)
	response := request(t, handler, http.MethodPost, "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`",`+
			`"resources":[{"type":"memory_store","memory_store_id":"`+memoryStore.ID+`",`+
			`"access":"read_only","instructions":"Prefer established decisions."}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("create Session with Memory Store -> %d: %s", response.Code, response.Body.String())
	}
	var created struct {
		ID        string `json:"id"`
		Resources []struct {
			Type          string `json:"type"`
			MemoryStoreID string `json:"memory_store_id"`
			Access        string `json:"access"`
			Name          string `json:"name"`
			Description   string `json:"description"`
			Instructions  string `json:"instructions"`
			MountPath     string `json:"mount_path"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Resources) != 1 ||
		created.Resources[0].Type != domain.SessionResourceTypeMemoryStore ||
		created.Resources[0].MemoryStoreID != memoryStore.ID ||
		created.Resources[0].Access != domain.MemoryAccessReadOnly ||
		created.Resources[0].Name != memoryStore.Name ||
		created.Resources[0].Description != memoryStore.Description ||
		created.Resources[0].Instructions != "Prefer established decisions." ||
		created.Resources[0].MountPath != "/mnt/memory/project-knowledge" {
		t.Fatalf("Memory Store resource = %+v", created.Resources)
	}
	renamed := "Renamed Store"
	if _, err := memory.UpdateStore(ctx, memoryStore.ID, app.MemoryStoreUpdateInput{Name: &renamed}); err != nil {
		t.Fatal(err)
	}
	response = request(t, handler, http.MethodGet, "/v1/sessions/"+created.ID, "")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), renamed) ||
		!strings.Contains(response.Body.String(), "Project Knowledge") {
		t.Fatalf("Session resource snapshot drifted -> %d: %s", response.Code, response.Body.String())
	}
	if err := memory.DeleteStore(ctx, memoryStore.ID); err == nil {
		t.Fatal("deleted Memory Store while Session remained attached")
	}
	response = request(t, handler, http.MethodDelete, "/v1/sessions/"+created.ID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("delete Session -> %d: %s", response.Code, response.Body.String())
	}
	if err := memory.DeleteStore(ctx, memoryStore.ID); err != nil {
		t.Fatalf("delete Store after Session cleanup: %v", err)
	}
}

type domainResourceJSON struct {
	ID        string `json:"id"`
	FileID    string `json:"file_id"`
	MountPath string `json:"mount_path"`
}

type resourceBlobStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

type staticGitRepositorySnapshotter struct {
	commit   string
	archive  []byte
	requests []app.GitRepositorySnapshotRequest
}

func (s *staticGitRepositorySnapshotter) OpenSnapshot(
	_ context.Context,
	request app.GitRepositorySnapshotRequest,
) (app.GitRepositorySnapshot, error) {
	s.requests = append(s.requests, request)
	return app.GitRepositorySnapshot{
		ResolvedCommit: s.commit,
		Archive:        io.NopCloser(bytes.NewReader(s.archive)),
	}, nil
}

func newResourceBlobStore() *resourceBlobStore {
	return &resourceBlobStore{objects: make(map[string][]byte)}
}

func (s *resourceBlobStore) Put(
	_ context.Context,
	key string,
	_ string,
	body io.Reader,
	maxBytes int64,
) (app.BlobInfo, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return app.BlobInfo{}, err
	}
	if int64(len(data)) > maxBytes {
		return app.BlobInfo{}, app.ErrBlobTooLarge
	}
	s.mu.Lock()
	s.objects[key] = append([]byte(nil), data...)
	s.mu.Unlock()
	return app.ComputeBlobInfo(data), nil
}

func (s *resourceBlobStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, errors.New("missing object")
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}

func (s *resourceBlobStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	delete(s.objects, key)
	s.mu.Unlock()
	return nil
}

func (s *resourceBlobStore) has(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.objects[key]
	return ok
}

type resourceSandbox struct {
	files        map[string][]byte
	repositories map[string][]byte
}

func newResourceSandbox() *resourceSandbox {
	return &resourceSandbox{
		files: make(map[string][]byte), repositories: make(map[string][]byte),
	}
}

func (*resourceSandbox) Exec(context.Context, sandbox.Command) (*sandbox.Result, error) {
	panic("not used")
}

func (s *resourceSandbox) ReadFile(_ context.Context, path string) ([]byte, error) {
	data, ok := s.files[path]
	if !ok {
		return nil, errors.New("missing file")
	}
	return append([]byte(nil), data...), nil
}

func (*resourceSandbox) WriteFile(context.Context, string, []byte) error {
	panic("not used")
}

func (*resourceSandbox) Root() string { return "/workspace" }

func (*resourceSandbox) Destroy(context.Context) error { return nil }

func (s *resourceSandbox) HasFileResource(
	_ context.Context,
	mount sandbox.FileResourceMount,
) (bool, error) {
	data, ok := s.files[mount.RuntimePath]
	if !ok {
		return false, nil
	}
	info := app.ComputeBlobInfo(data)
	return info.SizeBytes == mount.SizeBytes &&
		info.ChecksumSHA256 == mount.ChecksumSHA256, nil
}

func (s *resourceSandbox) ImportFileResource(
	_ context.Context,
	mount sandbox.FileResourceMount,
	content io.Reader,
) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	info := app.ComputeBlobInfo(data)
	if info.SizeBytes != mount.SizeBytes || info.ChecksumSHA256 != mount.ChecksumSHA256 {
		return errors.New("mount integrity mismatch")
	}
	s.files[mount.RuntimePath] = data
	return nil
}

func (s *resourceSandbox) RemoveFileResource(_ context.Context, path string, _ string) error {
	delete(s.files, path)
	return nil
}

func (s *resourceSandbox) HasGitRepository(
	_ context.Context,
	mount sandbox.GitRepositoryMount,
) (bool, error) {
	data, ok := s.repositories[mount.RuntimePath]
	if !ok {
		return false, nil
	}
	info := app.ComputeBlobInfo(data)
	return info.SizeBytes == mount.SizeBytes && info.ChecksumSHA256 == mount.ChecksumSHA256, nil
}

func (s *resourceSandbox) ImportGitRepository(
	_ context.Context,
	mount sandbox.GitRepositoryMount,
	content io.Reader,
) error {
	data, err := io.ReadAll(content)
	if err != nil {
		return err
	}
	info := app.ComputeBlobInfo(data)
	if info.SizeBytes != mount.SizeBytes || info.ChecksumSHA256 != mount.ChecksumSHA256 {
		return errors.New("repository snapshot integrity mismatch")
	}
	s.repositories[mount.RuntimePath] = data
	return nil
}

func (s *resourceSandbox) RemoveGitRepository(
	_ context.Context,
	runtimePath string,
	_ string,
) error {
	delete(s.repositories, runtimePath)
	return nil
}
