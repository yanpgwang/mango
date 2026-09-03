package httpapi

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

func TestSessionCredentialCannotInjectOrdinaryUserEvents(t *testing.T) {
	for name, body := range map[string]string{
		"user message":   `{"events":[{"type":"user.message","content":[{"type":"text","text":"no"}]}]}`,
		"system message": `{"events":[{"type":"user.tool_result","tool_use_id":"toolu_one","content":[{"type":"text","text":"done"}]},{"type":"system.message","content":[{"type":"text","text":"override"}]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newTestHandler(t, Config{}, false)
			req := httptest.NewRequest(http.MethodPost, "/v1/sessions/sesn_scoped/events",
				strings.NewReader(body))
			req.Header.Set("content-type", "application/json")
			req = req.WithContext(workspace.WithSessionScope(req.Context(), workspace.DefaultID,
				workspace.SessionScope{SessionID: "sesn_scoped"}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
			}
			if got := decodeErrorEnvelope(t, rec.Body.Bytes())["type"]; got != "permission_error" {
				t.Fatalf("error type = %q, want permission_error", got)
			}
		})
	}
}

func TestSessionCredentialStreamClosesWhenLeaseIsRevoked(t *testing.T) {
	authenticator := &revocableSessionAuthenticator{valid: true}
	hub := app.NewHub(8)
	session := domain.Session{ID: "sesn_scoped", Status: domain.StatusRunning}
	server := httptest.NewServer(NewServer(Deps{
		Sessions: staticSessionService{session: session}, Stream: hub,
	}, Config{RequireAuth: true, Authenticator: authenticator}).Handler())
	t.Cleanup(server.Close)

	request, err := http.NewRequest(http.MethodGet,
		server.URL+"/v1/sessions/"+session.ID+"/events/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("authorization", "Bearer session-token")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", response.StatusCode)
	}

	authenticator.revoke()
	closed := make(chan error, 1)
	go func() {
		buffer := make([]byte, 1)
		_, readErr := response.Body.Read(buffer)
		closed <- readErr
	}()
	select {
	case readErr := <-closed:
		if readErr == nil {
			t.Fatal("revoked stream returned data instead of closing")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("revoked Session credential stream remained open")
	}
}

type revocableSessionAuthenticator struct {
	mu    sync.Mutex
	valid bool
}

func (*revocableSessionAuthenticator) AuthenticateAPIKey(context.Context, string) (string, error) {
	return "", workspace.ErrInvalidAPIKey
}

func (a *revocableSessionAuthenticator) AuthenticateSessionToken(
	_ context.Context,
	token string,
) (string, workspace.SessionScope, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if token != "session-token" || !a.valid {
		return "", workspace.SessionScope{}, workspace.ErrInvalidSessionToken
	}
	return workspace.DefaultID, workspace.SessionScope{
		EnvironmentID: "env_scoped", WorkID: "work_scoped", SessionID: "sesn_scoped",
		CredentialDigest: []byte("digest"),
	}, nil
}

func (a *revocableSessionAuthenticator) ValidateSessionScope(
	_ context.Context,
	scope workspace.SessionScope,
) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.valid || scope.WorkID != "work_scoped" {
		return domain.Precondition("work lease is no longer owned")
	}
	return nil
}

func (a *revocableSessionAuthenticator) revoke() {
	a.mu.Lock()
	a.valid = false
	a.mu.Unlock()
}

type staticSessionService struct{ session domain.Session }

func (s staticSessionService) Create(context.Context, app.CreateSessionInput) (domain.Session, error) {
	return s.session, nil
}

func (s staticSessionService) Get(_ context.Context, id string) (domain.Session, error) {
	if id != s.session.ID {
		return domain.Session{}, domain.NotFound("session not found")
	}
	return s.session, nil
}

func (staticSessionService) List(context.Context, app.ListPage) (app.SessionListPage, error) {
	return app.SessionListPage{}, nil
}

func (staticSessionService) SendEvent(
	context.Context, string, []domain.EventDraft,
) ([]domain.Event, error) {
	return nil, nil
}

func (s staticSessionService) Update(context.Context, string, domain.SessionUpdate) (domain.Session, error) {
	return s.session, nil
}

func (s staticSessionService) Archive(context.Context, string) (domain.Session, error) {
	return s.session, nil
}

func (staticSessionService) Delete(context.Context, string) error { return nil }

func TestSession_FullLifecycleWithSSE(t *testing.T) {
	h := NewTestHandler(t)
	// setup agent + env
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"cloud"}}`)
	// create idle session
	sess := createID(t, h, "POST", "/v1/sessions",
		`{"agent":"`+ag+`","environment_id":"`+env+`"}`)

	// open live SSE stream via real httptest server so streaming flushes
	ts := httptest.NewServer(h)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", ts.URL+"/v1/sessions/"+sess+"/events/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// send a user.message (drives fake runtime -> agent.message + status_idle)
	go func() {
		time.Sleep(50 * time.Millisecond)
		do(h, "POST", "/v1/sessions/"+sess+"/events",
			`{"events":[{"type":"user.message","content":[{"type":"text","text":"hello"}]}]}`)
	}()

	// read stream lines until we see a session.status_idle event or timeout
	sawIdle := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if strings.Contains(sc.Text(), "session.status_idle") {
				sawIdle <- true
				return
			}
		}
	}()
	select {
	case <-sawIdle:
	case <-time.After(3 * time.Second):
		t.Fatal("did not observe status_idle on stream")
	}

	// history + stream reconciliation: history is non-empty and ends idle
	rec := do(h, "GET", "/v1/sessions/"+sess+"/events", "")
	var hist map[string]any
	json.Unmarshal(rec.Body.Bytes(), &hist)
	data := hist["data"].([]any)
	if len(data) < 2 {
		t.Fatalf("expected multiple history events, got %d", len(data))
	}
}

func createID(t *testing.T, h http.Handler, method, path, body string) string {
	t.Helper()
	rec := do(h, method, path, body)
	if rec.Code != 200 {
		t.Fatalf("%s %s -> %d: %s", method, path, rec.Code, rec.Body)
	}
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return m["id"].(string)
}

func TestCreateSession_MissingEnvironment(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	rec := do(h, "POST", "/v1/sessions", `{"agent":"`+ag+`"}`)
	if rec.Code == 200 {
		t.Fatalf("expected non-200 without environment_id, got 200: %s", rec.Body)
	}
}

func TestCreateSession_RejectsNonStringMetadata(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"cloud"}}`)
	rec := do(h, "POST", "/v1/sessions",
		`{"agent":"`+ag+`","environment_id":"`+env+`","metadata":{"bad":1}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestUpdateSession_OmittedTitlePreservesValue(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"cloud"}}`)
	id := createID(t, h, "POST", "/v1/sessions",
		`{"agent":"`+ag+`","environment_id":"`+env+`","title":"keep me"}`)

	rec := do(h, "POST", "/v1/sessions/"+id, `{}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("empty update -> %d: %s", rec.Code, rec.Body)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if got["title"] != "keep me" {
		t.Fatalf("omitted title changed value: %#v", got["title"])
	}
}

func TestCreateSession_AgentObjectForm(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"cloud"}}`)
	rec := do(h, "POST", "/v1/sessions",
		`{"agent":{"type":"agent","id":"`+ag+`"},"environment_id":"`+env+`"}`)
	if rec.Code != 200 {
		t.Fatalf("create with agent-object form -> %d: %s", rec.Code, rec.Body)
	}
	var sess map[string]any
	json.Unmarshal(rec.Body.Bytes(), &sess)
	id := sess["id"].(string)

	rec = do(h, "GET", "/v1/sessions/"+id, "")
	if rec.Code != 200 {
		t.Fatalf("GET session -> %d: %s", rec.Code, rec.Body)
	}
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "idle" {
		t.Fatalf("expected status idle, got %v", got["status"])
	}
}

func TestCreateSession_RejectsInvalidAgentReferences(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"cloud"}}`)
	for _, agent := range []string{
		`{"type":"bogus","id":"` + ag + `"}`,
		`{"type":"agent","id":"` + ag + `","version":0}`,
		`{"type":"agent","id":"` + ag + `","system":"not-an-override"}`,
		`{"type":"agent_with_overrides","id":"` + ag + `","model":null}`,
		`{"type":"agent_with_overrides","id":"` + ag + `","unknown":true}`,
	} {
		rec := do(h, "POST", "/v1/sessions",
			`{"agent":`+agent+`,"environment_id":"`+env+`"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("agent %s: got %d, want 400: %s", agent, rec.Code, rec.Body)
		}
	}
}

func TestStreamEvents_NotFound(t *testing.T) {
	h := NewTestHandler(t)
	rec := do(h, "GET", "/v1/sessions/nope/events/stream", "")
	if rec.Code != 404 {
		t.Fatalf("expected 404 for non-existent session stream, got %d: %s", rec.Code, rec.Body)
	}
}

func TestListEvents_NotFound(t *testing.T) {
	h := NewTestHandler(t)
	rec := do(h, "GET", "/v1/sessions/nope/events", "")
	if rec.Code != 404 {
		t.Fatalf("expected 404 for non-existent session events, got %d: %s", rec.Code, rec.Body)
	}
}

func TestListEvents_HasPaginationEnvelope(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"cloud"}}`)
	id := createID(t, h, "POST", "/v1/sessions",
		`{"agent":"`+ag+`","environment_id":"`+env+`"}`)

	rec := do(h, "GET", "/v1/sessions/"+id+"/events", "")
	if rec.Code != 200 {
		t.Fatalf("listEvents -> %d: %s", rec.Code, rec.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := body["data"]; !ok {
		t.Fatalf("missing 'data' key in listEvents response: %v", body)
	}
	if _, ok := body["next_page"]; !ok {
		t.Fatalf("missing 'next_page' key in listEvents response: %v", body)
	}
	// The List Events reference envelope carries data + next_page only; unlike
	// GET /v1/sessions, it does not return prev_page.
	if _, ok := body["prev_page"]; ok {
		t.Fatalf("listEvents must not expose prev_page: %v", body)
	}
}

func TestSendEvents_ValidatesVariantShape(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"cloud"}}`)
	id := createID(t, h, "POST", "/v1/sessions",
		`{"agent":"`+ag+`","environment_id":"`+env+`"}`)

	for _, body := range []string{
		`{"events":[]}`,
		`{"events":[{"type":"user.message"}]}`,
		`{"events":[{"type":"user.interrupt","session_thread_id":""}]}`,
		`{"events":[{"type":"user.interrupt","session_thread_id":"thread_invalid"}]}`,
		`{"events":[{"type":"user.custom_tool_result","content":[]}]}`,
		`{"events":[{"type":"user.tool_confirmation","tool_use_id":"sevt_x","result":"maybe"}]}`,
		`{"events":[{"type":"user.tool_confirmation","tool_use_id":"sevt_x","result":"deny","deny_message":42}]}`,
		`{"events":[{"type":"user.define_outcome","description":"x"}]}`,
		`{"events":[{"type":"user.message","content":[{"type":"text","text":"x"}],"bogus":true}]}`,
		`{"events":[{"type":"user.message","content":[{"type":"text","text":"x"}],"session_thread_id":"st_1"}]}`,
		`{"events":[{"type":"system.message","content":[{"type":"text","text":"late context"}]}]}`,
		`{"events":[{"type":"system.message","content":[{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}}]}]}`,
		`{"events":[{"type":"user.message","content":[{"type":"text","text":"x","bogus":true}]}]}`,
		`{"events":[{"type":"user.message","content":[{"type":"image","source":{"type":"url"}}]}]}`,
		`{"events":[{"type":"user.message","content":[{"type":"document","source":{"type":"text","data":"x","media_type":"text/markdown"}}]}]}`,
		`{"events":[{"type":"user.tool_result","tool_use_id":"sevt_x","content":[{"type":"search_result","source":"https://example.com","title":"x","content":[]}]}]}`,
	} {
		rec := do(h, "POST", "/v1/sessions/"+id+"/events", body)
		if rec.Code != 400 {
			t.Errorf("body %s: got %d, want 400 (%s)", body, rec.Code, rec.Body)
		}
	}

	paired := `{"events":[` +
		`{"type":"user.message","content":[{"type":"text","text":"hello"}]},` +
		`{"type":"system.message","content":[{"type":"text","text":"timezone UTC"}]}` +
		`]}`
	if rec := do(h, "POST", "/v1/sessions/"+id+"/events", paired); rec.Code != 200 {
		t.Fatalf("paired system.message -> %d: %s", rec.Code, rec.Body)
	}

	fileRubric := `{"events":[{"type":"user.define_outcome","description":"x",` +
		`"rubric":{"type":"file","file_id":"file_x"}}]}`
	if rec := do(h, "POST", "/v1/sessions/"+id+"/events", fileRubric); rec.Code != 200 {
		t.Fatalf("file rubric -> %d, want 200: %s", rec.Code, rec.Body)
	}

	fileDocument := `{"events":[{"type":"user.message","content":[{` +
		`"type":"document","source":{"type":"file","file_id":"file_x"}}]}]}`
	if rec := do(h, "POST", "/v1/sessions/"+id+"/events", fileDocument); rec.Code != 200 {
		t.Fatalf("file document -> %d, want 200: %s", rec.Code, rec.Body)
	}
	fileImage := `{"events":[{"type":"user.message","content":[{` +
		`"type":"image","source":{"type":"file","file_id":"file_x"}}]}]}`
	if rec := do(h, "POST", "/v1/sessions/"+id+"/events", fileImage); rec.Code != 422 {
		t.Fatalf("file image -> %d, want 422: %s", rec.Code, rec.Body)
	}
	fileToolResult := `{"events":[{"type":"user.tool_result","tool_use_id":"sevt_x",` +
		`"content":[{"type":"document","source":{"type":"file",` +
		`"file_id":"file_x"}}]}]}`
	if rec := do(h, "POST", "/v1/sessions/"+id+"/events", fileToolResult); rec.Code != 422 {
		t.Fatalf("file tool result -> %d, want 422: %s", rec.Code, rec.Body)
	}
}

func TestValidateClientEventAcceptsDocumentAndSearchResultShapes(t *testing.T) {
	events := []map[string]any{
		{
			"type": "user.message",
			"content": []any{map[string]any{
				"type": "document",
				"source": map[string]any{
					"type": "text", "data": "evidence", "media_type": "text/plain",
				},
				"context": "supporting material",
				"title":   "Evidence",
			}},
		},
		{
			"type":        "user.tool_result",
			"tool_use_id": "sevt_tool",
			"content": []any{map[string]any{
				"type": "search_result", "source": "https://example.com",
				"title":     "Example",
				"citations": map[string]any{"enabled": true},
				"content":   []any{map[string]any{"type": "text", "text": "result"}},
			}},
		},
	}
	for _, event := range events {
		if err := validateClientEvent(event); err != nil {
			t.Errorf("validate %s: %v", event["type"], err)
		}
	}
}

func TestValidateClientEventAcceptsThreadRoutingHintsOnActionResults(t *testing.T) {
	events := []map[string]any{
		{
			"type": "user.tool_confirmation", "tool_use_id": "sevt_tool",
			"result": "allow", "session_thread_id": "sthr_child",
		},
		{
			"type": "user.custom_tool_result", "custom_tool_use_id": "sevt_custom",
			"content": []any{}, "session_thread_id": "sthr_child",
		},
		{
			"type": "user.tool_result", "tool_use_id": "sevt_tool",
			"content": []any{}, "session_thread_id": "sthr_child",
		},
	}
	for _, event := range events {
		if err := validateClientEvent(event); err != nil {
			t.Errorf("validate %s: %v", event["type"], err)
		}
		event["session_thread_id"] = 42
		if err := validateClientEvent(event); err == nil {
			t.Errorf("validate %s accepted a non-string session_thread_id", event["type"])
		}
	}
}

func TestValidateClientEventRejectsMalformedNestedObjects(t *testing.T) {
	message := func(block map[string]any) map[string]any {
		return map[string]any{"type": "user.message", "content": []any{block}}
	}
	toolResult := func(block map[string]any) map[string]any {
		return map[string]any{
			"type": "user.tool_result", "tool_use_id": "sevt_tool",
			"content": []any{block},
		}
	}
	invalid := []map[string]any{
		message(map[string]any{"type": "text", "text": "x", "bogus": true}),
		message(map[string]any{
			"type": "image", "source": map[string]any{"type": "url"},
		}),
		message(map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "url", "url": "https://example.com/image.png", "data": "extra",
			},
		}),
		message(map[string]any{
			"type": "document",
			"source": map[string]any{
				"type": "text", "data": "x", "media_type": "text/markdown",
			},
		}),
		message(map[string]any{
			"type": "document", "source": map[string]any{
				"type": "url", "url": "https://example.com/document.pdf",
			}, "title": nil,
		}),
		toolResult(map[string]any{
			"type": "search_result", "source": "https://example.com", "title": "Example",
			"content": []any{},
		}),
		toolResult(map[string]any{
			"type": "search_result", "source": "https://example.com", "title": "Example",
			"citations": map[string]any{"enabled": true, "bogus": true},
			"content":   []any{},
		}),
		toolResult(map[string]any{
			"type": "search_result", "source": "https://example.com", "title": "Example",
			"citations": map[string]any{"enabled": true},
			"content": []any{map[string]any{
				"type": "text", "text": "result", "bogus": true,
			}},
		}),
		{
			"type": "user.define_outcome", "description": "x",
			"rubric": map[string]any{"type": "text", "content": "good", "bogus": true},
		},
		{
			"type": "user.define_outcome", "description": "x",
			"rubric": map[string]any{
				"type": "text", "content": strings.Repeat("x", domain.MaxOutcomeRubricCharacters+1),
			},
		},
	}
	for i, event := range invalid {
		if err := validateClientEvent(event); err == nil {
			t.Errorf("case %d accepted malformed event: %#v", i, event)
		}
	}
}

func TestCreateSessionRejectsMoreThanFiftyInitialEvents(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"cloud"}}`)
	events := make([]map[string]any, 51)
	for index := range events {
		events[index] = map[string]any{
			"type":    "user.message",
			"content": []any{map[string]any{"type": "text", "text": "x"}},
		}
	}
	body, err := json.Marshal(map[string]any{
		"agent": ag, "environment_id": env, "initial_events": events,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec := do(h, "POST", "/v1/sessions", string(body))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestListEvents_RejectsInvalidQueryValues(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"cloud"}}`)
	id := createID(t, h, "POST", "/v1/sessions",
		`{"agent":"`+ag+`","environment_id":"`+env+`"}`)

	for _, query := range []string{"?limit=zero", "?limit=0", "?limit=1001", "?order=sideways", "?created_at%5Bgt%5D=nope"} {
		rec := do(h, "GET", "/v1/sessions/"+id+"/events"+query, "")
		if rec.Code != 400 {
			t.Errorf("query %s: got %d, want 400 (%s)", query, rec.Code, rec.Body)
		}
	}
}

// TestListEvents_LimitBoundary proves the shared max page limit on List Events:
// limit=1000 is accepted (200) and limit=1001 is rejected (400).
func TestListEvents_LimitBoundary(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"cloud"}}`)
	id := createID(t, h, "POST", "/v1/sessions",
		`{"agent":"`+ag+`","environment_id":"`+env+`"}`)

	if rec := do(h, "GET", "/v1/sessions/"+id+"/events?limit=1000", ""); rec.Code != 200 {
		t.Errorf("limit=1000 -> %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := do(h, "GET", "/v1/sessions/"+id+"/events?limit=1001", ""); rec.Code != 400 {
		t.Errorf("limit=1001 -> %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestListEvents_CursorIsBoundToSessionAndFilters(t *testing.T) {
	h := NewTestHandler(t)
	ag := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	env := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"cloud"}}`)
	firstSession := createID(t, h, "POST", "/v1/sessions",
		`{"agent":"`+ag+`","environment_id":"`+env+`"}`)
	secondSession := createID(t, h, "POST", "/v1/sessions",
		`{"agent":"`+ag+`","environment_id":"`+env+`"}`)
	for range 2 {
		rec := do(h, "POST", "/v1/sessions/"+firstSession+"/events",
			`{"events":[{"type":"user.message","content":[{"type":"text","text":"hello"}]}]}`)
		if rec.Code != 200 {
			t.Fatalf("send event: %d: %s", rec.Code, rec.Body)
		}
	}

	rec := do(h, "GET", "/v1/sessions/"+firstSession+
		"/events?limit=1&types%5B%5D=user.message", "")
	if rec.Code != 200 {
		t.Fatalf("first page: %d: %s", rec.Code, rec.Body)
	}
	var page struct {
		NextPage string `json:"next_page"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || page.NextPage == "" {
		t.Fatalf("decode next page: %v: %s", err, rec.Body)
	}

	for _, path := range []string{
		"/v1/sessions/" + firstSession + "/events?limit=1&types%5B%5D=agent.message&page=" + page.NextPage,
		"/v1/sessions/" + secondSession + "/events?limit=1&types%5B%5D=user.message&page=" + page.NextPage,
		"/v1/sessions/" + firstSession + "/events?limit=1&order=desc&types%5B%5D=user.message&page=" + page.NextPage,
	} {
		rec = do(h, "GET", path, "")
		if rec.Code != 400 {
			t.Errorf("GET %s -> %d, want 400: %s", path, rec.Code, rec.Body)
		}
	}
}

func TestListEvents_OrdersAndPagesByProcessedAt(t *testing.T) {
	h, sessions := newTestHandlerWithSessions(t, Config{}, false)
	agentID := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	environmentID := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"cloud"}}`)
	sessionID := createID(t, h, "POST", "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`"}`)

	early := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	late := early.Add(time.Minute)
	created := early.Add(-time.Hour)
	sessions.mu.Lock()
	sessions.events[sessionID] = []domain.Event{
		{ID: "late-a", SessionID: sessionID, Sequence: 1, Type: domain.EvAgentMessage, CreatedAt: created, ProcessedAt: &late},
		{ID: "pending", SessionID: sessionID, Sequence: 2, Type: domain.EvUserMessage, CreatedAt: created.Add(time.Second)},
		{ID: "early", SessionID: sessionID, Sequence: 3, Type: domain.EvAgentMessage, CreatedAt: created.Add(2 * time.Second), ProcessedAt: &early},
		{ID: "late-b", SessionID: sessionID, Sequence: 4, Type: domain.EvAgentMessage, CreatedAt: created.Add(3 * time.Second), ProcessedAt: &late},
	}
	sessions.sequences[sessionID] = 4
	sessions.mu.Unlock()

	type eventPage struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		NextPage string `json:"next_page"`
	}
	list := func(path string) eventPage {
		t.Helper()
		rec := do(h, "GET", path, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s -> %d: %s", path, rec.Code, rec.Body)
		}
		var page eventPage
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return page
	}
	ids := func(page eventPage) []string {
		result := make([]string, len(page.Data))
		for index := range page.Data {
			result[index] = page.Data[index].ID
		}
		return result
	}

	first := list("/v1/sessions/" + sessionID + "/events?limit=2")
	if got, want := ids(first), []string{"early", "late-a"}; !sameStrings(got, want) {
		t.Fatalf("first page ids = %v, want %v", got, want)
	}
	if first.NextPage == "" {
		t.Fatal("first page omitted next_page")
	}
	second := list("/v1/sessions/" + sessionID + "/events?limit=2&page=" + first.NextPage)
	if got, want := ids(second), []string{"late-b", "pending"}; !sameStrings(got, want) {
		t.Fatalf("second page ids = %v, want %v", got, want)
	}
	if second.NextPage != "" {
		t.Fatalf("last page next_page = %q, want empty", second.NextPage)
	}

	descending := list("/v1/sessions/" + sessionID + "/events?limit=10&order=desc")
	if got, want := ids(descending), []string{"pending", "late-b", "late-a", "early"}; !sameStrings(got, want) {
		t.Fatalf("descending ids = %v, want %v", got, want)
	}
	filtered := list("/v1/sessions/" + sessionID + "/events?limit=10&created_at%5Bgte%5D=" + late.Format(time.RFC3339Nano))
	if got, want := ids(filtered), []string{"late-a", "late-b"}; !sameStrings(got, want) {
		t.Fatalf("filtered ids = %v, want %v", got, want)
	}
}
