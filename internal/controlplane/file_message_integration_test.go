package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/pg"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
)

func TestPostgresFileMessageContentSnapshotsAcrossAdmissionPaths(t *testing.T) {
	fixture := newPostgresFixture(t)
	ctx := context.Background()
	blobs := newResourceBlobStore()
	files := app.NewFileService(
		pg.NewFileRepository(fixture.store), blobs, fixture.ids, fixture.clock,
	)
	orchestrator := temporalpkg.NewOrchestrator(fixture.store, nil)
	sessions := NewSessionService(
		fixture.store, fixture.agentRepo, fixture.environmentRepo, orchestrator,
		fixture.ids, fixture.clock, nil,
	)
	sessions.EnableFileMessageContent(files)
	handler := httpapi.NewServer(httpapi.Deps{
		Agents: app.NewAgentService(fixture.agentRepo, fixture.ids, fixture.clock),
		Envs: app.NewEnvironmentService(
			fixture.environmentRepo, fixture.ids, fixture.clock,
		),
		Sessions: sessions, Events: NewEventService(fixture.store),
		Stream: app.NewHub(64), Files: files,
	}, httpapi.Config{}).Handler()
	agentID := createResource(t, handler, "/v1/agents",
		`{"name":"coder","model":"claude-test"}`)
	environmentID := createResource(t, handler, "/v1/environments",
		`{"name":"self-hosted","config":{"type":"self_hosted"}}`)
	before, err := fixture.store.ListSessions(ctx, app.ListPage{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	rejectedCreate := request(t, handler, http.MethodPost, "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`",`+
			`"initial_events":[{"type":"user.message","content":[`+
			`{"type":"document","source":{"type":"file",`+
			`"file_id":"file_missing"}}]}]}`)
	if rejectedCreate.Code != http.StatusNotFound {
		t.Fatalf("missing initial File -> %d: %s", rejectedCreate.Code, rejectedCreate.Body.String())
	}
	after, err := fixture.store.ListSessions(ctx, app.ListPage{Limit: 100})
	if err != nil || len(after.Sessions) != len(before.Sessions) {
		t.Fatalf("invalid initial File committed a Session: before=%d after=%d err=%v",
			len(before.Sessions), len(after.Sessions), err)
	}

	const initialContent = "# Initial attachment\n\nDurable evidence: 42"
	initialFile := uploadMessageFile(t, files, "evidence.md", "text/markdown", initialContent)
	create := request(t, handler, http.MethodPost, "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`",`+
			`"initial_events":[{"type":"user.message","content":[`+
			`{"type":"text","text":"Review the evidence"},`+
			`{"type":"document","title":"Evidence","context":"Use exact values",`+
			`"source":{"type":"file","file_id":"`+initialFile.ID+`"}}]}]}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create Session with File message -> %d: %s", create.Code, create.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	initialEvent := oneFileMessageEvent(t, fixture.store, created.ID)
	assertStoredFileMessage(t, initialEvent, initialFile.ID, initialContent)
	listed := request(t, handler, http.MethodGet, "/v1/sessions/"+created.ID+"/events", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), initialFile.ID) ||
		strings.Contains(listed.Body.String(), "Durable evidence") {
		t.Fatalf("public event projection = %d %s", listed.Code, listed.Body.String())
	}
	if _, err := files.Delete(ctx, initialFile.ID); err != nil {
		t.Fatalf("delete admitted source File: %v", err)
	}
	prepared, err := temporalpkg.NewActivities(
		nil, temporalpkg.NewStoreSource(fixture.store), nil, fixture.ids,
	).PrepareTurn(ctx, temporalpkg.PrepareTurnInput{
		SessionID: created.ID, TriggerEventID: initialEvent.ID,
	})
	if err != nil {
		t.Fatalf("PrepareTurn after source deletion: %v", err)
	}
	if len(prepared.Request.Messages) != 1 ||
		len(prepared.Request.Messages[0].Content) != 2 ||
		prepared.Request.Messages[0].Content[1].Type != "text" ||
		!strings.Contains(prepared.Request.Messages[0].Content[1].Text, initialContent) ||
		!strings.Contains(prepared.Request.Messages[0].Content[1].Text, `"title":"Evidence"`) {
		t.Fatalf("prepared File message = %#v", prepared.Request.Messages)
	}

	conversationID := createResource(t, handler, "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`"}`)
	missing := request(t, handler, http.MethodPost,
		"/v1/sessions/"+conversationID+"/events",
		`{"events":[{"type":"user.message","content":[{"type":"document",`+
			`"source":{"type":"file","file_id":"file_missing"}}]}]}`)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing File message -> %d: %s", missing.Code, missing.Body.String())
	}
	assertNoUserMessages(t, fixture.store, conversationID)

	pdf := uploadMessageFile(t, files, "scan.pdf", "application/pdf", "%PDF-1.4")
	rejected := request(t, handler, http.MethodPost,
		"/v1/sessions/"+conversationID+"/events",
		`{"events":[{"type":"user.message","content":[{"type":"document",`+
			`"source":{"type":"file","file_id":"`+pdf.ID+`"}}]}]}`)
	if rejected.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PDF File message -> %d: %s", rejected.Code, rejected.Body.String())
	}
	assertNoUserMessages(t, fixture.store, conversationID)

	const laterContent = "later text attachment"
	laterFile := uploadMessageFile(t, files, "later.txt", "text/plain", laterContent)
	sent := request(t, handler, http.MethodPost,
		"/v1/sessions/"+conversationID+"/events",
		`{"events":[{"type":"user.message","content":[{"type":"document",`+
			`"source":{"type":"file","file_id":"`+laterFile.ID+`"}}]}]}`)
	if sent.Code != http.StatusOK || strings.Contains(sent.Body.String(), laterContent) {
		t.Fatalf("later File message -> %d: %s", sent.Code, sent.Body.String())
	}
	assertStoredFileMessage(
		t, oneFileMessageEvent(t, fixture.store, conversationID), laterFile.ID, laterContent,
	)

	const deploymentContent = "deployment attachment"
	deploymentFile := uploadMessageFile(
		t, files, "deployment.txt", "text/plain", deploymentContent,
	)
	deployments := app.NewDeploymentService(app.DeploymentServiceConfig{
		Repository: pg.NewDeploymentRepository(fixture.store),
		Agents:     fixture.agentRepo, Environments: fixture.environmentRepo,
		Sessions: sessions, IDGenerator: fixture.ids, Clock: fixture.clock,
	})
	deployment, err := deployments.Create(ctx, app.DeploymentCreateInput{
		AgentID: agentID, EnvironmentID: environmentID, Name: "File message deployment",
		InitialEvents: []domain.EventDraft{{
			Type: domain.EvUserMessage,
			Payload: map[string]any{"content": []any{map[string]any{
				"type": "document",
				"source": map[string]any{
					"type": "file", "file_id": deploymentFile.ID,
				},
			}}},
		}},
	})
	if err != nil {
		t.Fatalf("create Deployment: %v", err)
	}
	run, err := deployments.Run(ctx, deployment.ID)
	if err != nil || run.SessionID == nil {
		t.Fatalf("Deployment Run = %+v, %v", run, err)
	}
	assertStoredFileMessage(
		t, oneFileMessageEvent(t, fixture.store, *run.SessionID),
		deploymentFile.ID, deploymentContent,
	)
	if _, err := files.Delete(ctx, deploymentFile.ID); err != nil {
		t.Fatal(err)
	}
	secondRun, err := deployments.Run(ctx, deployment.ID)
	if err != nil {
		t.Fatalf("record failed future Deployment Run: %v", err)
	}
	if secondRun.SessionID != nil || secondRun.ErrorType != "file_not_found_error" {
		t.Fatalf("future Deployment Run after source deletion = %+v", secondRun)
	}
}

func uploadMessageFile(
	t *testing.T,
	files *app.FileService,
	filename string,
	mediaType string,
	content string,
) domain.File {
	t.Helper()
	file, err := files.Upload(context.Background(), app.FileUploadInput{
		Filename: filename, MimeType: mediaType, Body: bytes.NewBufferString(content),
	})
	if err != nil {
		t.Fatalf("upload message File: %v", err)
	}
	return file
}

func oneFileMessageEvent(t *testing.T, store *pg.Store, sessionID string) domain.Event {
	t.Helper()
	events, err := store.QueryEvents(context.Background(), sessionID, app.EventQuery{
		Limit: 10, Types: []string{domain.EvUserMessage},
	})
	if err != nil || len(events) != 1 {
		t.Fatalf("user.message events = %+v, %v", events, err)
	}
	return events[0]
}

func assertNoUserMessages(t *testing.T, store *pg.Store, sessionID string) {
	t.Helper()
	events, err := store.QueryEvents(context.Background(), sessionID, app.EventQuery{
		Limit: 10, Types: []string{domain.EvUserMessage},
	})
	if err != nil || len(events) != 0 {
		t.Fatalf("unexpected user.message events = %+v, %v", events, err)
	}
}

func assertStoredFileMessage(
	t *testing.T,
	event domain.Event,
	fileID string,
	content string,
) {
	t.Helper()
	blocks, _ := event.Payload["content"].([]any)
	if len(blocks) == 0 {
		t.Fatalf("stored public content = %#v", event.Payload["content"])
	}
	last, _ := blocks[len(blocks)-1].(map[string]any)
	source, _ := last["source"].(map[string]any)
	if source["type"] != "file" || source["file_id"] != fileID {
		t.Fatalf("stored public File source = %#v", source)
	}
	snapshots := domain.FileMessageContents(event.Payload)
	snapshot, ok := snapshots[strconv.Itoa(len(blocks)-1)]
	if !ok || snapshot.FileID != fileID || snapshot.Content != content {
		t.Fatalf("stored private File snapshot = %#v", snapshots)
	}
}
