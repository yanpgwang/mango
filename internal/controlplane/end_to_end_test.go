package controlplane

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/live"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
	"github.com/yanpgwang/mango/internal/sandbox/sandboxtest"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
	enumspb "go.temporal.io/api/enums/v1"
)

func TestHTTPPostgresTemporalNATSEndToEnd(t *testing.T) {
	temporalAddress := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	natsURL := os.Getenv("MANGO_TEST_NATS_URL")
	if os.Getenv(testDatabaseURLEnv) == "" || temporalAddress == "" || natsURL == "" {
		t.Skip("PostgreSQL/Temporal/NATS integration environment is not configured")
	}
	fixture := newPostgresFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	broker, err := live.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer broker.Close()
	fixture.store.SetEventNotifier(broker)
	temporalClient, err := temporalpkg.Dial(temporalpkg.ClientConfig{
		HostPort: temporalAddress,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer temporalClient.Close()
	modelClient := model.NewFake()
	runtime := temporalpkg.NewRuntime(temporalpkg.RuntimeConfig{
		TemporalClient:   temporalClient,
		Store:            fixture.store,
		ModelClient:      modelClient,
		SandboxProvider:  sandboxtest.NoProvision(t),
		IDGenerator:      fixture.ids,
		RelayConfig:      temporalpkg.RelayConfig{PollInterval: 20 * time.Millisecond},
		TaskQueue:        "mango-test-" + domain.NewRandomIDGen().NewID(""),
		PreviewPublisher: broker,
	})
	if err := runtime.Worker.Start(); err != nil {
		t.Fatal(err)
	}
	defer runtime.Worker.Stop()
	relayErrors := make(chan error, 1)
	go func() { relayErrors <- runtime.Relay.Run(ctx) }()

	sessions := NewSessionService(
		fixture.store,
		fixture.agentRepo,
		fixture.environmentRepo,
		runtime.Orchestrator(),
		fixture.ids,
		fixture.clock,
		nil,
	)
	handler := httpapi.NewServer(httpapi.Deps{
		Agents: app.NewAgentService(
			fixture.agentRepo,
			fixture.ids,
			fixture.clock,
		),
		Envs: app.NewEnvironmentService(
			fixture.environmentRepo,
			fixture.ids,
			fixture.clock,
		),
		Sessions: sessions,
		Events:   NewEventService(fixture.store),
		Stream: live.NewStream(
			fixture.store,
			broker,
			fixture.ids,
			fixture.clock,
			50*time.Millisecond,
		),
	}, httpapi.Config{}).Handler()

	agentID := createResource(t, handler, "/v1/agents",
		`{"name":"coder","model":"claude-test"}`)
	environmentID := createResource(t, handler, "/v1/environments",
		`{"name":"cloud","config":{"type":"cloud"}}`)
	sessionID := createResource(t, handler, "/v1/sessions",
		`{"agent":"`+agentID+`","environment_id":"`+environmentID+`"}`)
	defer func() {
		_ = temporalClient.TerminateWorkflow(
			context.Background(),
			sessionID,
			"",
			"integration test cleanup",
		)
	}()

	server := httptest.NewServer(handler)
	defer server.Close()
	streamURL := server.URL + "/v1/sessions/" + sessionID + "/events/stream?" +
		url.Values{"event_deltas[]": {domain.EvAgentMessage}}.Encode()
	streamRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, streamURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	streamResponse, err := server.Client().Do(streamRequest)
	if err != nil {
		t.Fatalf("open event stream: %v", err)
	}
	defer streamResponse.Body.Close()
	if streamResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(streamResponse.Body)
		t.Fatalf("stream status = %d: %s", streamResponse.StatusCode, body)
	}

	lines := make(chan string, 64)
	go func() {
		scanner := bufio.NewScanner(streamResponse.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	response := request(
		t,
		handler,
		http.MethodPost,
		"/v1/sessions/"+sessionID+"/events",
		`{"events":[{"type":"user.message","content":[{"type":"text","text":"hello"}]}]}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("send user.message -> %d: %s", response.Code, response.Body.String())
	}

	wanted := map[string]bool{
		"event: " + domain.PreviewEventStart:   false,
		"event: " + domain.PreviewEventDelta:   false,
		"event: " + domain.EvAgentMessage:      false,
		"event: " + domain.EvSessionStatusIdle: false,
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for !allSeen(wanted) {
		select {
		case line, open := <-lines:
			if !open {
				t.Fatalf("stream closed early; seen=%v", wanted)
			}
			if _, tracked := wanted[line]; tracked {
				wanted[line] = true
			}
		case err := <-relayErrors:
			t.Fatalf("relay stopped: %v", err)
		case <-deadline.C:
			t.Fatalf("timed out waiting for complete stream; seen=%v", wanted)
		}
	}

	session, err := fixture.store.GetSession(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if session.Status != domain.StatusIdle {
		t.Fatalf("session status = %s, want idle", session.Status)
	}
	events, err := fixture.store.QueryEvents(ctx, sessionID, app.EventQuery{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !containsEventTypes(events,
		domain.EvUserMessage,
		domain.EvSessionStatusRunning,
		domain.EvAgentMessage,
		domain.EvSessionStatusIdle,
	) {
		t.Fatalf("event types = %v", eventTypes(events))
	}

	customAgentID := createResource(t, handler, "/v1/agents",
		`{"name":"custom-agent","model":"claude-test","tools":[{`+
			`"type":"custom","name":"ask_client","description":"ask",`+
			`"input_schema":{"type":"object"}}]}`)
	customSessionID := createResource(t, handler, "/v1/sessions",
		`{"agent":"`+customAgentID+`","environment_id":"`+environmentID+`"}`)
	defer func() {
		_ = temporalClient.TerminateWorkflow(
			context.Background(),
			customSessionID,
			"",
			"integration test cleanup",
		)
	}()
	response = request(
		t,
		handler,
		http.MethodPost,
		"/v1/sessions/"+customSessionID+"/events",
		`{"events":[{"type":"user.message","content":[{"type":"text","text":"inspect"}]}]}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("send custom session message -> %d: %s", response.Code, response.Body.String())
	}
	customEvents := waitForEvents(t, fixture.store, customSessionID, func(events []domain.Event) bool {
		return containsEventTypes(
			events,
			domain.EvAgentCustomToolUse,
			domain.EvSessionStatusIdle,
		)
	})
	var customActionID string
	for _, event := range customEvents {
		if event.Type == domain.EvAgentCustomToolUse {
			customActionID = event.ID
			break
		}
	}
	if customActionID == "" {
		t.Fatalf("custom action missing: %s", eventTypes(customEvents))
	}
	response = request(
		t,
		handler,
		http.MethodPost,
		"/v1/sessions/"+customSessionID+"/events",
		`{"events":[{"type":"user.custom_tool_result","custom_tool_use_id":"`+
			customActionID+
			`","content":[{"type":"text","text":"client result"}]}]}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("send custom result -> %d: %s", response.Code, response.Body.String())
	}
	waitForEvents(t, fixture.store, customSessionID, func(events []domain.Event) bool {
		return containsEventTypes(events, domain.EvUserCustomToolResult, domain.EvAgentMessage) &&
			countEventType(events, domain.EvSessionStatusIdle) >= 2
	})
	pending, err := fixture.store.UnresolvedPendingActions(ctx, customSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending barrier not cleared: %+v", pending)
	}
	transcript, err := fixture.store.LoadProviderTranscript(ctx, customSessionID)
	if err != nil {
		t.Fatal(err)
	}
	var providerToolUseID string
	for _, mapping := range transcript.ToolUseMappings {
		if mapping.PublicEventID == customActionID {
			providerToolUseID = mapping.ProviderToolUseID
			break
		}
	}
	if providerToolUseID == "" {
		t.Fatalf("provider mapping missing for public custom action %s", customActionID)
	}
	lastRequest := modelClient.LastRequest()
	if !requestHasToolResult(lastRequest, providerToolUseID, "client result") {
		t.Fatalf("resumed model request lost custom result: %#v", lastRequest.Messages)
	}

	confirmationAgentID := createResource(t, handler, "/v1/agents",
		`{"name":"confirmation-agent","model":"claude-test","tools":[{`+
			`"type":"agent_toolset_20260401",`+
			`"default_config":{"enabled":false,"permission_policy":{"type":"always_allow"}},`+
			`"configs":[{"name":"bash","enabled":true,`+
			`"permission_policy":{"type":"always_ask"}}]}]}`)
	confirmationSessionID := createResource(t, handler, "/v1/sessions",
		`{"agent":"`+confirmationAgentID+`","environment_id":"`+environmentID+`"}`)
	defer func() {
		_ = temporalClient.TerminateWorkflow(
			context.Background(),
			confirmationSessionID,
			"",
			"integration test cleanup",
		)
	}()
	response = request(
		t,
		handler,
		http.MethodPost,
		"/v1/sessions/"+confirmationSessionID+"/events",
		`{"events":[{"type":"user.message","content":[{"type":"text","text":"run"}]}]}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("send confirmation session message -> %d: %s", response.Code, response.Body.String())
	}
	confirmationEvents := waitForEvents(
		t,
		fixture.store,
		confirmationSessionID,
		func(events []domain.Event) bool {
			return containsEventTypes(
				events,
				domain.EvAgentToolUse,
				domain.EvSessionStatusIdle,
			)
		},
	)
	var confirmationActionID string
	for _, event := range confirmationEvents {
		if event.Type == domain.EvAgentToolUse &&
			event.Payload["evaluated_permission"] == "ask" {
			confirmationActionID = event.ID
			break
		}
	}
	if confirmationActionID == "" {
		t.Fatalf("confirmation action missing: %s", eventTypes(confirmationEvents))
	}
	response = request(
		t,
		handler,
		http.MethodPost,
		"/v1/sessions/"+confirmationSessionID+"/events",
		`{"events":[{"type":"user.tool_confirmation","tool_use_id":"`+
			confirmationActionID+
			`","result":"deny","deny_message":"not safe"}]}`,
	)
	if response.Code != http.StatusOK {
		t.Fatalf("send tool confirmation -> %d: %s", response.Code, response.Body.String())
	}
	waitForEvents(
		t,
		fixture.store,
		confirmationSessionID,
		func(events []domain.Event) bool {
			return containsEventTypes(
				events,
				domain.EvUserToolConfirmation,
				domain.EvAgentToolResult,
				domain.EvAgentMessage,
			) && countEventType(events, domain.EvSessionStatusIdle) >= 2
		},
	)
	pending, err = fixture.store.UnresolvedPendingActions(ctx, confirmationSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("confirmation barrier not cleared: %+v", pending)
	}
	transcript, err = fixture.store.LoadProviderTranscript(ctx, confirmationSessionID)
	if err != nil {
		t.Fatal(err)
	}
	providerToolUseID = ""
	for _, mapping := range transcript.ToolUseMappings {
		if mapping.PublicEventID == confirmationActionID {
			providerToolUseID = mapping.ProviderToolUseID
			break
		}
	}
	if providerToolUseID == "" {
		t.Fatalf("provider mapping missing for public confirmation action %s", confirmationActionID)
	}
	lastRequest = modelClient.LastRequest()
	if !requestHasToolResult(
		lastRequest,
		providerToolUseID,
		"Tool call denied by user. not safe",
	) {
		t.Fatalf("resumed model request lost confirmation result: %#v", lastRequest.Messages)
	}

	response = request(t, handler, http.MethodDelete, "/v1/sessions/"+sessionID, "")
	if response.Code != http.StatusOK {
		t.Fatalf("delete session -> %d: %s", response.Code, response.Body.String())
	}
	if _, err := fixture.store.GetSession(ctx, sessionID); err == nil {
		t.Fatal("session projection still exists after delete")
	}
	description, err := temporalClient.DescribeWorkflowExecution(ctx, sessionID, "")
	if err != nil {
		t.Fatalf("describe deleted workflow: %v", err)
	}
	if got := description.WorkflowExecutionInfo.Status; got != enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED {
		t.Fatalf("workflow status = %s, want TERMINATED", got)
	}
}

func allSeen(values map[string]bool) bool {
	for _, seen := range values {
		if !seen {
			return false
		}
	}
	return true
}

func containsEventTypes(events []domain.Event, types ...string) bool {
	found := make(map[string]bool, len(events))
	for _, event := range events {
		found[event.Type] = true
	}
	for _, eventType := range types {
		if !found[eventType] {
			return false
		}
	}
	return true
}

func eventTypes(events []domain.Event) string {
	types := make([]string, len(events))
	for i, event := range events {
		types[i] = event.Type
	}
	return strings.Join(types, ",")
}

func waitForEvents(
	t *testing.T,
	store *pg.Store,
	sessionID string,
	ready func([]domain.Event) bool,
) []domain.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		events, err := store.QueryEvents(
			context.Background(),
			sessionID,
			app.EventQuery{Limit: 100},
		)
		if err != nil {
			t.Fatal(err)
		}
		if ready(events) {
			return events
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for events: %s", eventTypes(events))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func countEventType(events []domain.Event, eventType string) int {
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

func requestHasToolResult(request model.Request, toolUseID, text string) bool {
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_result" &&
				block.ToolResultFor == toolUseID &&
				block.Text == text {
				return true
			}
		}
	}
	return false
}
