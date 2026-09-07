package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/pg"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
)

const testDatabaseURLEnv = "MANGO_TEST_DATABASE_URL"

var testSchemaSequence atomic.Int64

type postgresFixture struct {
	store           *pg.Store
	agentRepo       *pg.AgentRepository
	environmentRepo *pg.EnvironmentRepository
	ids             domain.IDGenerator
	clock           domain.Clock
}

func newPostgresFixture(t *testing.T) postgresFixture {
	t.Helper()
	databaseURL := os.Getenv(testDatabaseURLEnv)
	if databaseURL == "" {
		t.Skipf("%s not set; skipping PostgreSQL HTTP integration test", testDatabaseURLEnv)
	}
	ctx := context.Background()
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse database url: %v", err)
	}
	fixtureSequence := testSchemaSequence.Add(1)
	fixtureSuffix := domain.NewRandomIDGen().NewID("")
	schema := "controlplane_" + safeName(t.Name()) + "_" +
		intString(fixtureSequence) + "_" + fixtureSuffix
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	if _, err := pool.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		pool.Close()
		t.Fatalf("create schema: %v", err)
	}
	if err := pg.Migrate(ctx, pool); err != nil {
		pool.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		pool.Close()
	})

	ids := &testIDs{suffix: fixtureSuffix}
	clock := &testClock{}
	store := pg.NewDefaultWorkspaceStore(pool, ids, clock)
	agentRepo := pg.NewAgentRepository(store)
	environmentRepo := pg.NewEnvironmentRepository(store)
	return postgresFixture{
		store: store, agentRepo: agentRepo, environmentRepo: environmentRepo,
		ids: ids, clock: clock,
	}
}

func postgresHandler(t *testing.T) http.Handler {
	t.Helper()
	handler, _ := postgresHandlerWithFixture(t)
	return handler
}

// postgresHandlerWithFixture also returns the fixture so a test can drive the
// durable store or a Temporal Activity directly against the same data the HTTP
// surface just wrote.
func postgresHandlerWithFixture(t *testing.T) (http.Handler, postgresFixture) {
	t.Helper()
	fixture := newPostgresFixture(t)
	ids := fixture.ids
	clock := fixture.clock
	store := fixture.store
	agentRepo := fixture.agentRepo
	environmentRepo := fixture.environmentRepo
	orchestrator := temporalpkg.NewOrchestrator(store, nil)
	sessions := NewSessionService(
		store, agentRepo, environmentRepo, orchestrator, ids, clock, nil,
	)
	server := httpapi.NewServer(httpapi.Deps{
		Agents: app.NewAgentService(agentRepo, ids, clock),
		Envs: app.NewEnvironmentService(
			environmentRepo, ids, clock,
		),
		Sessions: sessions,
		Threads:  NewSessionThreadService(store),
		Events:   NewEventService(store),
		Stream:   app.NewHub(64),
	}, httpapi.Config{})
	return server.Handler(), fixture
}

func TestPostgresHTTPResourceSessionAndEventPath(t *testing.T) {
	handler := postgresHandler(t)
	agentID := createResource(t, handler, "/v1/agents",
		`{"name":"coder","model":"claude-test"}`)
	environmentID := createResource(t, handler, "/v1/environments",
		`{"name":"self-hosted","config":{"type":"self_hosted"}}`)
	sessionID := createResource(t, handler, "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`"}`)

	response := request(t, handler, http.MethodPost, "/v1/sessions/"+sessionID+"/events",
		`{"events":[{"type":"user.message","content":[{"type":"text","text":"hello"}]}]}`)
	if response.Code != http.StatusOK {
		t.Fatalf("send event -> %d: %s", response.Code, response.Body.String())
	}
	response = request(t, handler, http.MethodGet, "/v1/sessions/"+sessionID+"/events", "")
	if response.Code != http.StatusOK {
		t.Fatalf("list events -> %d: %s", response.Code, response.Body.String())
	}
	var envelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(envelope.Data) != 2 ||
		envelope.Data[0]["type"] != domain.EvSessionStatusRunning ||
		envelope.Data[0]["processed_at"] == nil ||
		envelope.Data[1]["type"] != domain.EvUserMessage ||
		envelope.Data[1]["processed_at"] != nil {
		t.Fatalf("event order = %#v", envelope.Data)
	}

	interrupt := request(
		t,
		handler,
		http.MethodPost,
		"/v1/sessions/"+sessionID+"/events",
		`{"events":[{"type":"user.interrupt"}]}`,
	)
	if interrupt.Code != http.StatusOK {
		t.Fatalf("interrupt status = %d, want 200: %s", interrupt.Code, interrupt.Body.String())
	}
	threadsResponse := request(
		t,
		handler,
		http.MethodGet,
		"/v1/sessions/"+sessionID+"/threads",
		"",
	)
	if threadsResponse.Code != http.StatusOK {
		t.Fatalf("list Threads -> %d: %s", threadsResponse.Code, threadsResponse.Body.String())
	}
	var threadEnvelope struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(threadsResponse.Body.Bytes(), &threadEnvelope); err != nil ||
		len(threadEnvelope.Data) != 1 {
		t.Fatalf("decode Threads = %+v, err=%v", threadEnvelope, err)
	}
	primaryThreadID := threadEnvelope.Data[0].ID
	targeted := request(
		t,
		handler,
		http.MethodPost,
		"/v1/sessions/"+sessionID+"/events",
		`{"events":[{"type":"user.interrupt","session_thread_id":"`+primaryThreadID+`"}]}`,
	)
	if targeted.Code != http.StatusOK {
		t.Fatalf(
			"targeted interrupt status = %d, want 200: %s",
			targeted.Code,
			targeted.Body.String(),
		)
	}
	var targetedEnvelope struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(targeted.Body.Bytes(), &targetedEnvelope); err != nil ||
		len(targetedEnvelope.Data) != 1 ||
		targetedEnvelope.Data[0]["session_thread_id"] != primaryThreadID {
		t.Fatalf("targeted interrupt response = %+v, err=%v", targetedEnvelope, err)
	}
}

func TestPostgresSessionPersistsResolvedMultiagentRoster(t *testing.T) {
	handler := postgresHandler(t)
	peerID := createResource(t, handler, "/v1/agents",
		`{"name":"reviewer","model":"claude-test","system":"review-v1","description":"reviews code"}`)
	coordinatorID := createResource(t, handler, "/v1/agents",
		`{"name":"coordinator","model":"claude-test","system":"coordinate",`+
			`"multiagent":{"type":"coordinator","agents":[{"type":"agent","id":"`+
			peerID+`","version":1},{"type":"self"}]}}`)
	environmentID := createResource(t, handler, "/v1/environments",
		`{"name":"self-hosted","config":{"type":"self_hosted"}}`)

	response := request(t, handler, http.MethodPost, "/v1/sessions",
		`{"agent":{"type":"agent_with_overrides","id":"`+coordinatorID+`",`+
			`"system":"session-coordinate"},"environment_id":"`+environmentID+`"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("create Session -> %d: %s", response.Code, response.Body.String())
	}
	var created map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	agent := created["agent"].(map[string]any)
	topology := agent["multiagent"].(map[string]any)
	roster := topology["agents"].([]any)
	if len(roster) != 2 {
		t.Fatalf("resolved roster = %#v", topology)
	}
	peer := roster[0].(map[string]any)
	self := roster[1].(map[string]any)
	if peer["id"] != peerID || peer["name"] != "reviewer" ||
		peer["system"] != "review-v1" || peer["version"] != float64(1) {
		t.Fatalf("resolved peer = %#v", peer)
	}
	if self["id"] != coordinatorID || self["system"] != "session-coordinate" ||
		self["version"] != float64(1) {
		t.Fatalf("resolved self = %#v", self)
	}
	if _, nested := self["multiagent"]; nested {
		t.Fatalf("self snapshot retained nested topology: %#v", self)
	}

	if archived := request(
		t, handler, http.MethodPost, "/v1/agents/"+peerID+"/archive", "",
	); archived.Code != http.StatusOK {
		t.Fatalf("archive peer -> %d: %s", archived.Code, archived.Body.String())
	}
	sessionID := created["id"].(string)
	reloaded := request(t, handler, http.MethodGet, "/v1/sessions/"+sessionID, "")
	if reloaded.Code != http.StatusOK {
		t.Fatalf("reload Session -> %d: %s", reloaded.Code, reloaded.Body.String())
	}
	var after map[string]any
	if err := json.Unmarshal(reloaded.Body.Bytes(), &after); err != nil {
		t.Fatal(err)
	}
	frozen := after["agent"].(map[string]any)["multiagent"].(map[string]any)["agents"].([]any)[0].(map[string]any)
	if frozen["version"] != float64(1) || frozen["system"] != "review-v1" {
		t.Fatalf("Session roster drifted after peer archive: %#v", frozen)
	}
}

func TestPostgresHTTPSessionPrimaryThreadLifecycle(t *testing.T) {
	handler := postgresHandler(t)
	agentID := createResource(t, handler, "/v1/agents",
		`{"name":"coordinator","model":"claude-test"}`)
	environmentID := createResource(t, handler, "/v1/environments",
		`{"name":"self-hosted","config":{"type":"self_hosted"}}`)
	sessionID := createResource(t, handler, "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`"}`)

	response := request(t, handler, http.MethodGet,
		"/v1/sessions/"+sessionID+"/threads?limit=1", "")
	if response.Code != http.StatusOK {
		t.Fatalf("list threads -> %d: %s", response.Code, response.Body.String())
	}
	var page struct {
		Data []struct {
			ID             string  `json:"id"`
			SessionID      string  `json:"session_id"`
			ParentThreadID *string `json:"parent_thread_id"`
			Status         string  `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode threads: %v", err)
	}
	if len(page.Data) != 1 || page.Data[0].ID == "" ||
		page.Data[0].SessionID != sessionID || page.Data[0].ParentThreadID != nil ||
		page.Data[0].Status != string(domain.StatusIdle) {
		t.Fatalf("primary thread page = %+v", page.Data)
	}
	threadID := page.Data[0].ID

	response = request(t, handler, http.MethodGet,
		"/v1/sessions/"+sessionID+"/threads/"+threadID+"/events", "")
	if response.Code != http.StatusOK {
		t.Fatalf("list thread events -> %d: %s", response.Code, response.Body.String())
	}
	response = request(t, handler, http.MethodPost,
		"/v1/sessions/"+sessionID+"/threads/"+threadID+"/archive", "")
	if response.Code != http.StatusOK {
		t.Fatalf("archive thread -> %d: %s", response.Code, response.Body.String())
	}
	var archived map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &archived); err != nil {
		t.Fatalf("decode archived thread: %v", err)
	}
	if archived["status"] != string(domain.StatusTerminated) || archived["archived_at"] == nil {
		t.Fatalf("archived thread = %#v", archived)
	}
}

func TestOfficialGoSDKArchivesPostgresChildThread(t *testing.T) {
	handler, fixture := postgresHandlerWithFixture(t)
	ctx := context.Background()
	session := domain.Session{
		ID: "sesn_child_archive_sdk", AgentID: "agent_coordinator",
		AgentVersion: 1, EnvironmentID: "env_cloud", Status: domain.StatusIdle,
		AgentSnapshot: domain.Agent{
			ID: "agent_coordinator", Version: 1, Name: "coordinator",
			Model: domain.NormalizeModel(domain.Model{ID: "claude-test"}),
			Multiagent: &domain.Multiagent{
				Type: "coordinator",
				Agents: []domain.AgentReference{{
					Type: "agent", ID: "agent_reviewer", Version: 2,
				}},
			},
		},
		MultiagentRoster: []domain.Agent{{
			ID: "agent_reviewer", Version: 2, Name: "reviewer",
			Model: domain.NormalizeModel(domain.Model{ID: "claude-test"}),
		}},
		Metadata: map[string]any{}, CreatedAt: fixture.clock.Now(),
		UpdatedAt: fixture.clock.Now(),
	}
	if _, err := fixture.store.CreateSession(ctx, session, nil); err != nil {
		t.Fatal(err)
	}
	threads, err := fixture.store.ListSessionThreads(
		ctx, session.ID, app.SessionThreadListQuery{Limit: 1},
	)
	if err != nil || len(threads) != 1 {
		t.Fatalf("primary Threads = %+v, err=%v", threads, err)
	}
	child, _, err := fixture.store.CreateChildSessionThread(
		ctx, session.ID, threads[0].ID, "reviewer",
	)
	if err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client := anthropic.NewClient(
		option.WithBaseURL(server.URL+"/"), option.WithAuthToken("test-key"),
	)
	archived, err := client.Beta.Sessions.Threads.Archive(
		ctx,
		child.ID,
		anthropic.BetaSessionThreadArchiveParams{SessionID: session.ID},
	)
	if err != nil || archived.ID != child.ID || archived.ArchivedAt.IsZero() ||
		archived.Status != anthropic.BetaManagedAgentsSessionThreadStatusTerminated {
		t.Fatalf("Archive child Session Thread = %+v, err=%v", archived, err)
	}
	wakeups, err := fixture.store.ListWakeupsForDelivery(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	var intent pg.OrchestrationIntent
	for _, wakeup := range wakeups {
		if wakeup.SessionID == session.ID && wakeup.ThreadID == child.ID {
			intent = wakeup.Intent
		}
	}
	if intent != pg.OrchestrationTerminate {
		t.Fatalf("child termination intent = %q", intent)
	}
}

func TestDeleteFencesAdmissionBeforeWorkflowTermination(t *testing.T) {
	fixture := newPostgresFixture(t)
	ctx := context.Background()
	session := domain.Session{
		ID: "sesn_delete_running", Status: domain.StatusIdle,
		Metadata: map[string]any{}, CreatedAt: fixture.clock.Now(),
		UpdatedAt: fixture.clock.Now(),
	}
	if _, err := fixture.store.CreateSession(ctx, session, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "running"}},
		},
	}}); err != nil {
		t.Fatalf("create running session: %v", err)
	}
	orchestrator := &recordingOrchestrator{}
	service := NewSessionService(
		fixture.store, fixture.agentRepo, fixture.environmentRepo,
		orchestrator, fixture.ids, fixture.clock, nil,
	)
	if err := service.Delete(ctx, session.ID); err == nil {
		t.Fatal("delete running session succeeded")
	}
	if orchestrator.terminationCalls != 0 {
		t.Fatalf("workflow terminated before running conflict: calls=%d", orchestrator.terminationCalls)
	}
}

func TestDeleteTerminationFailureKeepsFenceAndRetryCompletes(t *testing.T) {
	fixture := newPostgresFixture(t)
	ctx := context.Background()
	session := domain.Session{
		ID: "sesn_delete_retry", Status: domain.StatusIdle,
		Metadata: map[string]any{}, CreatedAt: fixture.clock.Now(),
		UpdatedAt: fixture.clock.Now(),
	}
	if _, err := fixture.store.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	orchestrator := &recordingOrchestrator{terminateErr: errors.New("temporal unavailable")}
	service := NewSessionService(
		fixture.store, fixture.agentRepo, fixture.environmentRepo,
		orchestrator, fixture.ids, fixture.clock, nil,
	)
	if err := service.Delete(ctx, session.ID); err == nil {
		t.Fatal("delete succeeded despite termination failure")
	}
	if _, err := fixture.store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{
		Type:    domain.EvUserDefineOutcome,
		Payload: map[string]any{"description": "must remain fenced"},
	}}); err == nil {
		t.Fatal("admission reopened after ambiguous termination result")
	}
	orchestrator.terminateErr = nil
	if err := service.Delete(ctx, session.ID); err != nil {
		t.Fatalf("retry delete: %v", err)
	}
	if _, err := fixture.store.GetSession(ctx, session.ID); err == nil {
		t.Fatal("session still exists after successful delete retry")
	}
}

type recordingOrchestrator struct {
	terminationCalls int
	terminateErr     error
}

func (o *recordingOrchestrator) CreateAPISession(
	context.Context,
	domain.Session,
	[]domain.EventDraft,
	...[]app.PreparedSessionResource,
) (domain.Session, []domain.Event, error) {
	panic("not used")
}

func (o *recordingOrchestrator) Admit(
	context.Context,
	string,
	[]domain.EventDraft,
) ([]domain.Event, error) {
	panic("not used")
}

func (o *recordingOrchestrator) TerminateSession(context.Context, string) error {
	o.terminationCalls++
	return o.terminateErr
}

func createResource(t *testing.T, handler http.Handler, path, body string) string {
	t.Helper()
	response := request(t, handler, http.MethodPost, path, body)
	if response.Code != http.StatusOK {
		t.Fatalf("POST %s -> %d: %s", path, response.Code, response.Body.String())
	}
	var object map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
		t.Fatalf("decode POST %s: %v", path, err)
	}
	id, _ := object["id"].(string)
	if id == "" {
		t.Fatalf("POST %s returned no id: %v", path, object)
	}
	return id
}

func request(
	t *testing.T,
	handler http.Handler,
	method string,
	path string,
	body string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

type testIDs struct {
	mu     sync.Mutex
	n      int
	suffix string
}

func (g *testIDs) NewID(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return prefix + intString(int64(g.n)) + "_" + g.suffix
}

type testClock struct {
	n atomic.Int64
}

func (c *testClock) Now() time.Time {
	return time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC).
		Add(time.Duration(c.n.Add(1)) * time.Millisecond)
}

func safeName(value string) string {
	var builder strings.Builder
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9':
			builder.WriteRune(character)
		default:
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

func intString(value int64) string {
	if value == 0 {
		return "0"
	}
	var buffer [20]byte
	index := len(buffer)
	for value > 0 {
		index--
		buffer[index] = byte('0' + value%10)
		value /= 10
	}
	return string(buffer[index:])
}
