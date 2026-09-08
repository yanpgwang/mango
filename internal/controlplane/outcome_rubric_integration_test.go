package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/pg"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
)

func TestPostgresFileOutcomeRubricSnapshotsAcrossAdmissionPaths(t *testing.T) {
	fixture := newPostgresFixture(t)
	ctx := context.Background()
	blobs := newResourceBlobStore()
	fileRepository := pg.NewFileRepository(fixture.store)
	files := app.NewFileService(fileRepository, blobs, fixture.ids, fixture.clock)
	orchestrator := temporalpkg.NewOrchestrator(fixture.store, nil)
	sessions := NewSessionService(
		fixture.store,
		fixture.agentRepo,
		fixture.environmentRepo,
		orchestrator,
		fixture.ids,
		fixture.clock,
		nil,
	)
	sessions.EnableFileOutcomeRubrics(files)
	handler := httpapi.NewServer(httpapi.Deps{
		Agents:   app.NewAgentService(fixture.agentRepo, fixture.ids, fixture.clock),
		Envs:     app.NewEnvironmentService(fixture.environmentRepo, fixture.ids, fixture.clock),
		Sessions: sessions,
		Events:   NewEventService(fixture.store),
		Stream:   app.NewHub(64),
		Files:    files,
	}, httpapi.Config{}).Handler()
	agentID := createResource(t, handler, "/v1/agents",
		`{"name":"coder","model":"claude-test"}`)
	environmentID := createResource(t, handler, "/v1/environments",
		`{"name":"self-hosted","config":{"type":"self_hosted"}}`)
	emptyFile := uploadOutcomeRubric(t, files, "")
	before, err := fixture.store.ListSessions(ctx, app.ListPage{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	rejected := request(t, handler, http.MethodPost, "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`",`+
			`"initial_events":[{"type":"user.define_outcome",`+
			`"description":"must not commit","rubric":{"type":"file",`+
			`"file_id":"`+emptyFile.ID+`"}}]}`)
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("empty initial rubric -> %d: %s", rejected.Code, rejected.Body.String())
	}
	after, err := fixture.store.ListSessions(ctx, app.ListPage{Limit: 100})
	if err != nil || len(after.Sessions) != len(before.Sessions) {
		t.Fatalf("invalid initial rubric committed a Session: before=%d after=%d err=%v",
			len(before.Sessions), len(after.Sessions), err)
	}

	const initialRubric = "# Initial rubric\n- cites evidence\n- produces report.md"
	initialFile := uploadOutcomeRubric(t, files, initialRubric)
	create := request(t, handler, http.MethodPost, "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`",`+
			`"initial_events":[{"type":"user.define_outcome",`+
			`"description":"produce report.md","rubric":{"type":"file",`+
			`"file_id":"`+initialFile.ID+`"}}]}`)
	if create.Code != http.StatusOK {
		t.Fatalf("create Session with file rubric -> %d: %s", create.Code, create.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	initialEvent := oneOutcomeEvent(t, fixture.store, created.ID)
	assertStoredOutcomeRubric(t, initialEvent, initialFile.ID, initialRubric)
	listed := request(t, handler, http.MethodGet, "/v1/sessions/"+created.ID+"/events", "")
	if listed.Code != http.StatusOK || strings.Contains(listed.Body.String(), "Initial rubric") {
		t.Fatalf("public events leaked rubric bytes: %d %s", listed.Code, listed.Body.String())
	}
	if _, err := files.Delete(ctx, initialFile.ID); err != nil {
		t.Fatalf("delete admitted source rubric: %v", err)
	}
	prepared, err := temporalpkg.NewActivities(
		nil, temporalpkg.NewStoreSource(fixture.store), nil, fixture.ids,
	).PrepareTurn(ctx, temporalpkg.PrepareTurnInput{
		SessionID: created.ID, TriggerEventID: initialEvent.ID,
	})
	if err != nil {
		t.Fatalf("PrepareTurn after source deletion: %v", err)
	}
	if prepared.Outcome == nil || prepared.Outcome.Rubric["content"] != initialRubric {
		t.Fatalf("prepared snapshotted outcome = %#v", prepared.Outcome)
	}

	conversationID := createResource(t, handler, "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`"}`)
	missing := request(t, handler, http.MethodPost,
		"/v1/sessions/"+conversationID+"/events",
		`{"events":[{"type":"user.define_outcome","description":"must not commit",`+
			`"rubric":{"type":"file","file_id":"file_missing"}}]}`)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing later rubric -> %d: %s", missing.Code, missing.Body.String())
	}
	uncommitted, err := fixture.store.QueryEvents(ctx, conversationID, app.EventQuery{
		Limit: 10, Types: []string{domain.EvUserDefineOutcome},
	})
	if err != nil || len(uncommitted) != 0 {
		t.Fatalf("missing later rubric committed events = %+v, %v", uncommitted, err)
	}
	const laterRubric = "# Later rubric\n- includes a summary"
	laterFile := uploadOutcomeRubric(t, files, laterRubric)
	sent := request(t, handler, http.MethodPost,
		"/v1/sessions/"+conversationID+"/events",
		`{"events":[{"type":"user.define_outcome","description":"summarize",`+
			`"rubric":{"type":"file","file_id":"`+laterFile.ID+`"}}]}`)
	if sent.Code != http.StatusOK || strings.Contains(sent.Body.String(), "Later rubric") {
		t.Fatalf("send file rubric -> %d: %s", sent.Code, sent.Body.String())
	}
	assertStoredOutcomeRubric(
		t, oneOutcomeEvent(t, fixture.store, conversationID), laterFile.ID, laterRubric,
	)

	const deploymentRubric = "# Deployment rubric\n- produces deployment.md"
	deploymentFile := uploadOutcomeRubric(t, files, deploymentRubric)
	deployments := app.NewDeploymentService(app.DeploymentServiceConfig{
		Repository: pg.NewDeploymentRepository(fixture.store),
		Agents:     fixture.agentRepo, Environments: fixture.environmentRepo,
		Sessions: sessions, IDGenerator: fixture.ids, Clock: fixture.clock,
	})
	deployment, err := deployments.Create(ctx, app.DeploymentCreateInput{
		AgentID: agentID, EnvironmentID: environmentID, Name: "rubric deployment",
		InitialEvents: []domain.EventDraft{{
			Type: domain.EvUserDefineOutcome,
			Payload: map[string]any{
				"description": "produce deployment.md",
				"rubric": map[string]any{
					"type": "file", "file_id": deploymentFile.ID,
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("create Deployment: %v", err)
	}
	firstRun, err := deployments.Run(ctx, deployment.ID)
	if err != nil || firstRun.SessionID == nil {
		t.Fatalf("first Deployment Run = %+v, %v", firstRun, err)
	}
	assertStoredOutcomeRubric(
		t,
		oneOutcomeEvent(t, fixture.store, *firstRun.SessionID),
		deploymentFile.ID,
		deploymentRubric,
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

func uploadOutcomeRubric(
	t *testing.T,
	files *app.FileService,
	content string,
) domain.File {
	t.Helper()
	file, err := files.Upload(context.Background(), app.FileUploadInput{
		Filename: "rubric.md", MimeType: "text/markdown",
		Body: bytes.NewBufferString(content),
	})
	if err != nil {
		t.Fatalf("upload outcome rubric: %v", err)
	}
	return file
}

func oneOutcomeEvent(t *testing.T, store *pg.Store, sessionID string) domain.Event {
	t.Helper()
	events, err := store.QueryEvents(context.Background(), sessionID, app.EventQuery{
		Limit: 10, Types: []string{domain.EvUserDefineOutcome},
	})
	if err != nil || len(events) != 1 {
		t.Fatalf("outcome events = %+v, %v", events, err)
	}
	return events[0]
}

func assertStoredOutcomeRubric(
	t *testing.T,
	event domain.Event,
	fileID string,
	content string,
) {
	t.Helper()
	rubric, _ := event.Payload["rubric"].(map[string]any)
	if rubric["type"] != "file" || rubric["file_id"] != fileID {
		t.Fatalf("stored public rubric = %#v", rubric)
	}
	if got, ok := domain.OutcomeRubricContent(event.Payload); !ok || got != content {
		t.Fatalf("stored private rubric = %q, %v", got, ok)
	}
}
