package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
	mango "github.com/yanpgwang/mango/sdk/go"
)

func TestSessionBudgetHTTPContractLifecycle(t *testing.T) {
	handler := NewTestHandler(t)
	agentID := createID(t, handler, "POST", "/v1/agents",
		`{"name":"budget contract","model":"claude-opus-4-8"}`)
	environmentID := createID(t, handler, "POST", "/v1/environments",
		`{"name":"budget contract","config":{"type":"self_hosted"}}`)
	const initialBudget = `{"type":"limit","max_list_cost":{"amount":"2500","currency":"USD"}}`
	const raisedBudget = `{"type":"limit","max_list_cost":{"amount":"3000","currency":"USD"}}`
	createFields := `"agent":"` + agentID + `","environment_id":"` + environmentID + `"`

	request := func(t *testing.T, method, path, body string, wantStatus int) []byte {
		t.Helper()
		response := do(handler, method, path, body)
		if response.Code != wantStatus {
			t.Fatalf("%s %s -> %d, want %d: %s", method, path, response.Code, wantStatus, response.Body)
		}
		return response.Body.Bytes()
	}
	created := request(t, "POST", "/v1/sessions", `{`+createFields+`,"budget":`+initialBudget+`}`, http.StatusOK)
	assertBudgetContractJSON(t, rawJSONField(t, string(created), "budget"), initialBudget)
	assertBudgetContractJSON(t, rawJSONField(t, rawJSONField(t, string(created), "usage"), "list_cost"),
		`{"amount":"0","currency":"USD"}`)
	sessionID := decodeBody(t, created)["id"].(string)
	path := "/v1/sessions/" + sessionID

	// Check both mutation responses and subsequent reads. An omitted budget
	// preserves the ceiling; explicit null removes it permanently.
	for _, step := range []struct {
		name, method, body, wantBudget string
	}{
		{"read initial", "GET", "", initialBudget},
		{"omit budget", "POST", `{"title":"ceiling preserved"}`, initialBudget},
		{"raise budget", "POST", `{"budget":` + raisedBudget + `}`, raisedBudget},
		{"read raised", "GET", "", raisedBudget},
		{"clear budget", "POST", `{"budget":null}`, "null"},
		{"read cleared", "GET", "", "null"},
		{"clear again", "POST", `{"budget":null}`, "null"},
	} {
		if !t.Run(step.name, func(t *testing.T) {
			body := request(t, step.method, path, step.body, http.StatusOK)
			assertBudgetContractJSON(t, rawJSONField(t, string(body), "budget"), step.wantBudget)
		}) {
			return
		}
	}
	rejected := request(t, "POST", path, `{"budget":`+raisedBudget+`}`, http.StatusBadRequest)
	assertErrorEnvelope(t, rejected, "invalid_request_error")
	after := request(t, "GET", path, "", http.StatusOK)
	assertBudgetContractJSON(t, rawJSONField(t, string(after), "budget"), "null")

	for _, withoutBudget := range []struct {
		name, field string
	}{
		{"omitted at creation", ""},
		{"null at creation", `,"budget":null`},
	} {
		t.Run(withoutBudget.name, func(t *testing.T) {
			created := request(t, "POST", "/v1/sessions", `{`+createFields+withoutBudget.field+`}`, http.StatusOK)
			assertBudgetContractJSON(t, rawJSONField(t, string(created), "budget"), "null")
			path := "/v1/sessions/" + decodeBody(t, created)["id"].(string)
			cleared := request(t, "POST", path, `{"budget":null}`, http.StatusOK)
			assertBudgetContractJSON(t, rawJSONField(t, string(cleared), "budget"), "null")
			rejected := request(t, "POST", path, `{"budget":`+initialBudget+`}`, http.StatusBadRequest)
			assertErrorEnvelope(t, rejected, "invalid_request_error")
			after := request(t, "GET", path, "", http.StatusOK)
			assertBudgetContractJSON(t, rawJSONField(t, string(after), "budget"), "null")
		})
	}
}

func TestMangoSDKSessionUsageBudgetContract(t *testing.T) {
	handler, sessions := newTestHandlerWithSessions(t, Config{RequireAuth: true}, false)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, responseJSON := recordedSDKClient(t, server.URL)
	ctx := context.Background()
	agent := mustAgent(t, client, "", "budget contract")
	session, err := client.Sessions.New(ctx, mango.SessionCreateRequest{
		Agent: mango.AgentID(agent.ID), EnvironmentID: mustEnv(t, server.URL),
		Budget: mango.Some(&mango.SessionBudgetLimit{
			Type: "limit", MaxListCost: mango.MonetaryAmount{Amount: "2500", Currency: "USD"},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if session.Budget == nil || session.Budget.Type != "limit" ||
		session.Budget.MaxListCost.Amount != "2500" || session.Budget.MaxListCost.Currency != "USD" {
		t.Fatalf("created Session budget = %+v", session.Budget)
	}

	// Supply cumulative execution facts, then exercise the production usage
	// projection and real HTTP decoding. This fixture does not test billing or
	// persistence; its independently specified wire expectations are below.
	sessions.mu.Lock()
	stored := sessions.sessions[session.ID]
	stored.ModelListCostNanoUSD = 1_230_000_000 // Exactly USD 1.23; the wire uses cents.
	stored.Usage = domain.TokenUsage{InputTokens: 11, OutputTokens: 13}
	sessions.sessions[session.ID] = stored
	event := sessions.appendEventLocked(session.ID, domain.EventDraft{
		Type: "session.usage", Payload: stored.UsageEventPayload(sessions.clock.Now()),
	})
	sessions.mu.Unlock()

	page, err := client.Sessions.Events.List(ctx, session.ID, mango.ListSessionEventsParams{
		Types: mango.Some([]mango.CoreSessionEventType{"session.usage"}),
	})
	if err != nil || len(page.Data) != 1 {
		t.Fatalf("Session usage events = %+v, err=%v", page, err)
	}
	usageEvent := page.Data[0].SessionUsageEvent
	if page.Data[0].Raw != nil || usageEvent == nil {
		t.Fatalf("session.usage did not decode as SessionUsageEvent: %+v", page.Data[0])
	}
	if usageEvent.ID != event.ID || usageEvent.Type != "session.usage" ||
		usageEvent.Budget == nil || usageEvent.Budget.Type != "limit" ||
		usageEvent.Budget.MaxListCost.Amount != "2500" || usageEvent.Budget.MaxListCost.Currency != "USD" {
		t.Fatalf("decoded usage budget = %+v", usageEvent)
	}
	if usageEvent.Usage.ListCost.Amount != "123" || usageEvent.Usage.ListCost.Currency != "USD" ||
		usageEvent.Usage.InputTokens != 11 || usageEvent.Usage.OutputTokens != 13 {
		t.Fatalf("decoded cumulative usage = %+v", usageEvent.Usage)
	}

	// Read original response bytes before generated decoding so required fields,
	// nulls and numeric/string distinctions cannot be hidden by Go zero values.
	var rawPage struct {
		Data []json.RawMessage `json:"data"`
	}
	decodeTestJSON(t, []byte(responseJSON()), &rawPage)
	if len(rawPage.Data) != 1 {
		t.Fatalf("raw usage events = %s", responseJSON())
	}
	assertBudgetContractJSON(t, rawJSONField(t, string(rawPage.Data[0]), "budget"),
		`{"type":"limit","max_list_cost":{"amount":"2500","currency":"USD"}}`)
	assertBudgetContractJSON(t, rawJSONField(t, string(rawPage.Data[0]), "usage"),
		`{"active_seconds":0,"cache_creation":{"ephemeral_1h_input_tokens":0,"ephemeral_5m_input_tokens":0},"cache_read_input_tokens":0,"input_tokens":11,"list_cost":{"amount":"123","currency":"USD"},"output_tokens":13,"server_tool_use":{"web_fetch_requests":0,"web_search_requests":0}}`)
}

func assertBudgetContractJSON(t *testing.T, got, want string) {
	t.Helper()
	var actual, expected any
	if err := json.Unmarshal([]byte(got), &actual); err != nil {
		t.Fatalf("missing or invalid budget contract JSON %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(want), &expected); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("budget contract JSON = %s, want %s", got, want)
	}
}
