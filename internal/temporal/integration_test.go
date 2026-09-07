package temporal_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"github.com/yanpgwang/mango/internal/agentruntime"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
)

// TestVerticalSlice_EndToEnd is the real integration path required by the
// milestone: a genuine Temporal service + real PostgreSQL. It admits one
// user.message and asserts the full spine runs — admission writes the outbox,
// the relay delivers a Signal-With-Start, the SessionWorkflow drives the
// Workflow-owned model loop through granular Activities, and the turn's
// authoritative agent.message plus terminal idle land in PostgreSQL in receipt
// order with the session projected back to idle.
//
// It skips unless BOTH MANGO_TEST_DATABASE_URL and
// MANGO_TEST_TEMPORAL_HOSTPORT are set, so `go test ./...` passes with no
// local stack. The local dev stack (deployments/local) satisfies both.
func TestVerticalSlice_EndToEnd(t *testing.T) {
	runVerticalSliceEndToEnd(t, model.NewFake(), "fake", 30*time.Second)
}

func TestVerticalSlice_MultiagentDelegationEndToEnd(t *testing.T) {
	dbURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	hostPort := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if dbURL == "" || hostPort == "" {
		t.Skip("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT to run the multiagent end-to-end slice")
	}
	ctx := context.Background()
	store, cleanup := integrationStore(t, dbURL)
	defer cleanup()
	c, err := client.Dial(client.Options{HostPort: hostPort})
	if err != nil {
		t.Skipf("temporal unreachable at %s: %v", hostPort, err)
	}
	defer c.Close()

	ids := domain.NewRandomIDGen()
	probe := &multiagentProbeModel{}
	runtime := temporalpkg.NewRuntime(temporalpkg.RuntimeConfig{
		TemporalClient: c, Store: store, ModelClient: probe, IDGenerator: ids,
		RelayConfig: temporalpkg.RelayConfig{PollInterval: 50 * time.Millisecond},
		TaskQueue:   "mango-multiagent-test-" + ids.NewID(""),
	})
	if err := runtime.Worker.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer runtime.Worker.Stop()
	relayCtx, stopRelay := context.WithCancel(ctx)
	defer stopRelay()
	go func() { _ = runtime.Relay.Run(relayCtx) }()

	session := domain.Session{
		ID:      "sess_multiagent_e2e_" + ids.NewID(""),
		AgentID: "agent_coordinator", AgentVersion: 1,
		EnvironmentID: "env_1", Status: domain.StatusIdle,
		Metadata: map[string]any{}, CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
		AgentSnapshot: domain.Agent{
			ID: "agent_coordinator", Version: 1, Name: "coordinator",
			Model: domain.NormalizeModel(domain.Model{ID: "coordinator-model"}),
			Multiagent: &domain.Multiagent{
				Type: "coordinator",
				Agents: []domain.AgentReference{{
					Type: "agent", ID: "agent_reviewer", Version: 1,
				}},
			},
		},
		MultiagentRoster: []domain.Agent{{
			ID: "agent_reviewer", Version: 1, Name: "reviewer",
			Model: domain.NormalizeModel(domain.Model{ID: "child-model"}),
		}},
	}
	orch := runtime.Orchestrator()
	if _, _, err := orch.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create coordinator Session: %v", err)
	}
	defer terminateIntegrationWorkflow(t, c, session.ID)
	if _, err := orch.Admit(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "delegate a review"},
		}},
	}}); err != nil {
		t.Fatalf("admit coordinator task: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	var primaryEvents []domain.Event
	var threads []domain.SessionThread
	for time.Now().Before(deadline) {
		primaryEvents, err = store.EventsAfter(ctx, session.ID, 0, 200)
		if err != nil {
			t.Fatalf("read primary events: %v", err)
		}
		threads, err = store.ListSessionThreads(
			ctx, session.ID, app.SessionThreadListQuery{Limit: 10},
		)
		if err != nil {
			t.Fatalf("list Threads: %v", err)
		}
		current, getErr := store.GetSession(ctx, session.ID)
		if getErr != nil {
			t.Fatalf("get Session: %v", getErr)
		}
		if len(threads) == 2 && current.Status == domain.StatusIdle &&
			hasType(primaryEvents, domain.EvAgentThreadMessageReceived) &&
			probe.coordinatorCount() >= 3 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(threads) != 2 {
		t.Fatalf("Threads = %+v, want primary and child", threads)
	}
	child := threads[1]
	defer func() {
		err := c.TerminateWorkflow(
			context.Background(), "session-thread:"+child.ID, "", "test cleanup",
		)
		var notFound *serviceerror.NotFound
		if err != nil && !errors.As(err, &notFound) {
			t.Errorf("terminate child Workflow: %v", err)
		}
	}()
	childEvents, err := store.ThreadEventsAfter(
		ctx, session.ID, child.ID, 0, 200,
	)
	if err != nil {
		t.Fatalf("read child events: %v", err)
	}
	for _, eventType := range []string{
		domain.EvAgentThreadMessageReceived,
		domain.EvSessionThreadStatusRunning,
		domain.EvAgentThreadMessageSent,
		domain.EvSessionThreadStatusIdle,
	} {
		if !hasType(childEvents, eventType) {
			t.Fatalf("child event %s missing from %s", eventType, typeList(childEvents))
		}
	}
	if hasType(childEvents, domain.EvAgentMessage) {
		t.Fatalf("child report leaked as agent.message: %s", typeList(childEvents))
	}
	for _, eventType := range []string{
		domain.EvSessionThreadCreated,
		domain.EvAgentThreadMessageSent,
		domain.EvSessionThreadStatusRunning,
		domain.EvAgentThreadMessageReceived,
		domain.EvSessionThreadStatusIdle,
	} {
		if !hasType(primaryEvents, eventType) {
			t.Fatalf("primary event %s missing from %s", eventType, typeList(primaryEvents))
		}
	}
	if probe.childCount() != 1 || probe.coordinatorCount() != 3 {
		t.Fatalf(
			"model calls coordinator=%d child=%d, want 3/1",
			probe.coordinatorCount(), probe.childCount(),
		)
	}
	archived, err := store.ArchiveSessionThread(ctx, session.ID, child.ID)
	if err != nil || archived.Status != domain.StatusTerminated ||
		archived.ArchivedAt == nil {
		t.Fatalf("archive child = %+v, err=%v", archived, err)
	}
	shutdownDeadline := time.Now().Add(10 * time.Second)
	for {
		described, describeErr := c.DescribeWorkflowExecution(
			ctx, "session-thread:"+child.ID, "",
		)
		if describeErr != nil {
			t.Fatalf("describe archived child Workflow: %v", describeErr)
		}
		if described.WorkflowExecutionInfo.Status ==
			enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED {
			break
		}
		if time.Now().After(shutdownDeadline) {
			t.Fatalf(
				"archived child Workflow status = %s, want terminated",
				described.WorkflowExecutionInfo.Status,
			)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

type multiagentProbeModel struct {
	mu               sync.Mutex
	coordinatorCalls int
	childCalls       int
}

func (m *multiagentProbeModel) CreateMessage(
	_ context.Context,
	req model.Request,
) (model.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch req.Model {
	case "child-model":
		m.childCalls++
		if !requestContainsText(req, "Review the delegated change and report.") {
			return model.Response{}, fmt.Errorf("child context omitted delegated task")
		}
		return textModelResponse("Child report: no blocking issues."), nil
	case "coordinator-model":
		m.coordinatorCalls++
		switch m.coordinatorCalls {
		case 1:
			if !strings.Contains(req.System, "<mango-coordinator>") {
				return model.Response{}, fmt.Errorf("coordinator runtime context was not attached")
			}
			if !requestHasTool(req, agentruntime.SendToAgentToolName) ||
				!requestHasTool(req, agentruntime.ListAgentsToolName) {
				return model.Response{}, fmt.Errorf("coordinator tools were not attached")
			}
			return model.Response{
				Content: []domain.ContentBlock{{
					Type: "tool_use", ToolUseID: "toolu_delegate",
					ToolName: agentruntime.SendToAgentToolName,
					Input: map[string]any{
						"agent_name": "reviewer",
						"message":    "Review the delegated change and report.",
					},
				}},
				StopReason: "tool_use",
			}, nil
		case 2:
			return textModelResponse("The review is delegated."), nil
		case 3:
			if !requestContainsText(req, `"from_agent_name":"reviewer"`) ||
				!requestContainsText(req, "<agent-thread-message>") {
				return model.Response{}, fmt.Errorf("coordinator context omitted child identity")
			}
			if !requestContainsText(req, "Child report: no blocking issues.") {
				return model.Response{}, fmt.Errorf("coordinator context omitted child report")
			}
			return textModelResponse("Synthesis: no blocking issues."), nil
		default:
			return model.Response{}, fmt.Errorf(
				"unexpected coordinator model call %d", m.coordinatorCalls,
			)
		}
	default:
		return model.Response{}, fmt.Errorf("unexpected model %q", req.Model)
	}
}

func (m *multiagentProbeModel) CreateMessageStream(
	ctx context.Context,
	req model.Request,
	onDelta func(index int, text string),
) (model.Response, error) {
	response, err := m.CreateMessage(ctx, req)
	if err != nil {
		return model.Response{}, err
	}
	for index, block := range response.Content {
		if block.Type == "text" && block.Text != "" && onDelta != nil {
			onDelta(index, block.Text)
		}
	}
	return response, nil
}

func (m *multiagentProbeModel) coordinatorCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.coordinatorCalls
}

func (m *multiagentProbeModel) childCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.childCalls
}

func requestHasTool(req model.Request, name string) bool {
	for _, tool := range req.Tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func requestContainsText(req model.Request, text string) bool {
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Type == "text" && strings.Contains(block.Text, text) {
				return true
			}
		}
	}
	return false
}

func textModelResponse(text string) model.Response {
	return model.Response{
		Content:    []domain.ContentBlock{{Type: "text", Text: text}},
		StopReason: "end_turn",
	}
}

// TestVerticalSlice_LiveModelEndToEnd exercises the same durable platform path
// with a real Anthropic-shaped Messages endpoint. It is deliberately gated so
// normal development and CI never make billable, credentialed network calls.
func TestVerticalSlice_LiveModelEndToEnd(t *testing.T) {
	modelClient, modelID := liveModelForTest(t, "platform smoke test")
	runVerticalSliceEndToEnd(t, modelClient, modelID, 2*time.Minute)
}

func liveModelForTest(t *testing.T, purpose string) (model.Client, string) {
	t.Helper()
	if os.Getenv("MANGO_TEST_LIVE_MODEL") != "1" {
		t.Skipf("set MANGO_TEST_LIVE_MODEL=1 to run the live-model %s", purpose)
	}
	if os.Getenv("MANGO_TEST_DATABASE_URL") == "" ||
		os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT") == "" {
		t.Skipf("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT to run the live-model %s", purpose)
	}
	modelID := strings.TrimSpace(os.Getenv("MANGO_MODEL_ID"))
	if modelID == "" {
		t.Fatalf("MANGO_MODEL_ID is required for the live-model %s", purpose)
	}
	modelClient, configured, err := model.AnthropicFromEnv()
	if err != nil {
		t.Fatalf("configure live model: %v", err)
	}
	if !configured {
		t.Fatalf("MANGO_MODEL_BASE_URL and MANGO_MODEL_API_KEY are required for the live-model %s", purpose)
	}
	return modelClient, modelID
}

func runVerticalSliceEndToEnd(
	t *testing.T,
	modelClient model.Client,
	modelID string,
	testTimeout time.Duration,
) {
	t.Helper()
	dbURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	hostPort := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if dbURL == "" || hostPort == "" {
		t.Skip("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT to run the real end-to-end slice")
	}
	ctx := context.Background()

	// Isolated PostgreSQL schema for this test.
	store, cleanup := integrationStore(t, dbURL)
	defer cleanup()

	// Real Temporal client against the running dev cluster.
	c, err := client.Dial(client.Options{HostPort: hostPort})
	if err != nil {
		t.Skipf("temporal unreachable at %s: %v", hostPort, err)
	}
	defer c.Close()

	ids := domain.NewRandomIDGen()

	runtime := temporalpkg.NewRuntime(temporalpkg.RuntimeConfig{
		TemporalClient: c,
		Store:          store,
		ModelClient:    modelClient,
		IDGenerator:    ids,
		RelayConfig:    temporalpkg.RelayConfig{PollInterval: 200 * time.Millisecond},
		TaskQueue:      "mango-test-" + ids.NewID(""),
	})

	// Start the worker.
	if err := runtime.Worker.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer runtime.Worker.Stop()

	// Start the relay.
	relayCtx, stopRelay := context.WithCancel(ctx)
	defer stopRelay()
	go func() { _ = runtime.Relay.Run(relayCtx) }()

	// Create a session and admit one user.message through the orchestrator (which
	// admits to PostgreSQL and fast-path signals).
	orch := runtime.Orchestrator()
	sess := domain.Session{
		ID:            "sess_e2e_" + ids.NewID(""),
		AgentID:       "agent_1",
		AgentVersion:  1,
		EnvironmentID: "env_1",
		Status:        domain.StatusIdle,
		Metadata:      map[string]any{},
		AgentSnapshot: domain.Agent{
			ID: "agent_1", Version: 1, Model: domain.Model{ID: modelID},
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if _, _, err := orch.CreateSession(ctx, sess, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := orch.Admit(ctx, sess.ID, []domain.EventDraft{{
		Type:    domain.EvUserMessage,
		Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": "hello world"}}},
	}}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	defer terminateIntegrationWorkflow(t, c, sess.ID)

	// Poll PostgreSQL until the agent.message and terminal idle land.
	deadline := time.Now().Add(testTimeout)
	var events []domain.Event
	for time.Now().Before(deadline) {
		events, err = store.EventsAfter(ctx, sess.ID, 0, 100)
		if err != nil {
			t.Fatalf("events after: %v", err)
		}
		if hasType(events, domain.EvAgentMessage) && hasType(events, domain.EvSessionStatusIdle) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	if !hasType(events, domain.EvAgentMessage) {
		t.Fatalf("agent.message never committed; got %d events: %s", len(events), typeList(events))
	}
	if !hasType(events, domain.EvSessionStatusIdle) {
		t.Fatalf("terminal idle never committed; got: %s", typeList(events))
	}

	// Receipt order follows the CMA model request span around the buffered
	// message: start is durable before provider work and end closes that request.
	assertOrder(t, events,
		domain.EvUserMessage,
		domain.EvSessionStatusRunning,
		domain.EvSpanModelRequestStart,
		domain.EvAgentMessage,
		domain.EvSpanModelRequestEnd,
		domain.EvSessionStatusIdle,
	)
	assertModelRequestSpans(t, events, false)

	// Session projected back to idle.
	final, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if final.Status != domain.StatusIdle {
		t.Fatalf("expected idle, got %s", final.Status)
	}

	// The outbox wakeup was consumed by the relay.
	if _, ok, err := store.PendingWakeup(ctx, sess.ID); err != nil || ok {
		t.Fatalf("expected no pending wakeup after processing: ok=%v err=%v", ok, err)
	}
}

// TestVerticalSlice_InterruptCancelsModelActivity proves the cross-process
// cancellation path against real Temporal and PostgreSQL. The public interrupt
// is first committed to PostgreSQL, its metadata-only wakeup reaches the
// Workflow, the Workflow rereads the durable ledger, and only then requests
// cancellation of the heartbeat-enabled model Activity.
func TestVerticalSlice_InterruptCancelsModelActivity(t *testing.T) {
	dbURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	hostPort := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if dbURL == "" || hostPort == "" {
		t.Skip("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT to run the interrupt end-to-end slice")
	}
	ctx := context.Background()

	store, cleanup := integrationStore(t, dbURL)
	defer cleanup()

	c, err := client.Dial(client.Options{HostPort: hostPort})
	if err != nil {
		t.Skipf("temporal unreachable at %s: %v", hostPort, err)
	}
	defer c.Close()

	ids := domain.NewRandomIDGen()
	blockingModel := newInterruptBlockingModel()
	runtime := temporalpkg.NewRuntime(temporalpkg.RuntimeConfig{
		TemporalClient: c,
		Store:          store,
		ModelClient:    blockingModel,
		IDGenerator:    ids,
		RelayConfig:    temporalpkg.RelayConfig{PollInterval: 200 * time.Millisecond},
		TaskQueue:      "mango-test-" + ids.NewID(""),
	})
	if err := runtime.Worker.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer runtime.Worker.Stop()
	relayCtx, stopRelay := context.WithCancel(ctx)
	defer stopRelay()
	go func() { _ = runtime.Relay.Run(relayCtx) }()

	orch := runtime.Orchestrator()
	sess := domain.Session{
		ID:            "sess_interrupt_e2e_" + ids.NewID(""),
		AgentID:       "agent_1",
		AgentVersion:  1,
		EnvironmentID: "env_1",
		Status:        domain.StatusIdle,
		Metadata:      map[string]any{},
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
	}
	if _, _, err := orch.CreateSession(ctx, sess, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := orch.Admit(ctx, sess.ID, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{
			map[string]any{"type": "text", "text": "block until interrupted"},
		}},
	}}); err != nil {
		t.Fatalf("admit message: %v", err)
	}
	defer terminateIntegrationWorkflow(t, c, sess.ID)

	select {
	case <-blockingModel.started:
	case <-time.After(15 * time.Second):
		t.Fatal("model Activity never started")
	}

	admitted, err := orch.Admit(ctx, sess.ID, []domain.EventDraft{{
		Type:    domain.EvUserInterrupt,
		Payload: map[string]any{},
	}})
	if err != nil {
		t.Fatalf("admit interrupt: %v", err)
	}
	if len(admitted) != 1 {
		t.Fatalf("interrupt events = %d, want 1", len(admitted))
	}
	interruptID := admitted[0].ID

	select {
	case <-blockingModel.canceled:
	case <-time.After(15 * time.Second):
		t.Fatal("durable interrupt did not cancel the model Activity context")
	}

	deadline := time.Now().Add(15 * time.Second)
	var events []domain.Event
	for time.Now().Before(deadline) {
		events, err = store.EventsAfter(ctx, sess.ID, 0, 100)
		if err != nil {
			t.Fatalf("events after: %v", err)
		}
		interrupt, getErr := store.GetEvent(ctx, sess.ID, interruptID)
		if getErr != nil {
			t.Fatalf("get interrupt: %v", getErr)
		}
		if interrupt.ProcessedAt != nil && hasType(events, domain.EvSessionStatusIdle) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}

	assertOrder(t, events,
		domain.EvUserMessage,
		domain.EvSessionStatusRunning,
		domain.EvSpanModelRequestStart,
		domain.EvUserInterrupt,
		domain.EvSpanModelRequestEnd,
		domain.EvSessionStatusIdle,
	)
	assertModelRequestSpans(t, events, true)
	idleCount := 0
	for _, event := range events {
		switch event.Type {
		case domain.EvSessionStatusIdle:
			idleCount++
			stopReason, _ := event.Payload["stop_reason"].(map[string]any)
			if stopReason["type"] != "end_turn" {
				t.Fatalf("interrupt stop reason = %#v, want end_turn", stopReason)
			}
		case domain.EvSessionError, domain.EvSessionStatusTerminated:
			t.Fatalf("interrupt published failure event %s", event.Type)
		case domain.EvAgentMessage:
			t.Fatal("canceled blocking model unexpectedly published agent.message")
		}
	}
	if idleCount != 1 {
		t.Fatalf("idle events = %d, want exactly 1; got %s", idleCount, typeList(events))
	}
	interrupt, err := store.GetEvent(ctx, sess.ID, interruptID)
	if err != nil {
		t.Fatalf("get final interrupt: %v", err)
	}
	if interrupt.ProcessedAt == nil {
		t.Fatal("interrupt was not marked processed with turn completion")
	}
	final, err := store.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if final.Status != domain.StatusIdle {
		t.Fatalf("final status = %s, want idle", final.Status)
	}
}

func hasType(events []domain.Event, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

type interruptBlockingModel struct {
	started  chan struct{}
	canceled chan struct{}

	startOnce  sync.Once
	cancelOnce sync.Once
}

func newInterruptBlockingModel() *interruptBlockingModel {
	return &interruptBlockingModel{
		started:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
}

func (m *interruptBlockingModel) CreateMessage(ctx context.Context, req model.Request) (model.Response, error) {
	return m.CreateMessageStream(ctx, req, nil)
}

func (m *interruptBlockingModel) CreateMessageStream(
	ctx context.Context,
	_ model.Request,
	_ func(index int, text string),
) (model.Response, error) {
	m.startOnce.Do(func() { close(m.started) })
	<-ctx.Done()
	m.cancelOnce.Do(func() { close(m.canceled) })
	return model.Response{}, ctx.Err()
}

func eventsHaveOrder(events []domain.Event, types ...string) bool {
	index := 0
	for _, event := range events {
		if index < len(types) && event.Type == types[index] {
			index++
		}
	}
	return index == len(types)
}

func eventsOfType(events []domain.Event, eventType string) []domain.Event {
	var matches []domain.Event
	for _, event := range events {
		if event.Type == eventType {
			matches = append(matches, event)
		}
	}
	return matches
}

func firstFailureEvent(events []domain.Event) (domain.Event, bool) {
	for _, event := range events {
		if event.Type == domain.EvSessionError || event.Type == domain.EvSessionStatusTerminated {
			return event, true
		}
	}
	return domain.Event{}, false
}

func eventText(event domain.Event) (text string, isError bool, ok bool) {
	isError, _ = event.Payload["is_error"].(bool)
	content, ok := event.Payload["content"].([]any)
	if !ok {
		return "", isError, false
	}
	var out strings.Builder
	for _, raw := range content {
		block, blockOK := raw.(map[string]any)
		part, textOK := block["text"].(string)
		if !blockOK || !textOK {
			return "", isError, false
		}
		out.WriteString(part)
	}
	return out.String(), isError, true
}

func assertModelRequestSpans(t *testing.T, events []domain.Event, expectedErrors ...bool) {
	t.Helper()
	starts := eventsOfType(events, domain.EvSpanModelRequestStart)
	ends := eventsOfType(events, domain.EvSpanModelRequestEnd)
	if len(starts) != len(expectedErrors) || len(ends) != len(expectedErrors) {
		t.Fatalf("model request spans = %d starts/%d ends, want %d each; got %s", len(starts), len(ends), len(expectedErrors), typeList(events))
	}
	for i := range expectedErrors {
		startID, _ := ends[i].Payload["model_request_start_id"].(string)
		if startID != starts[i].ID {
			t.Fatalf("model request end %d references %q, want %q", i, startID, starts[i].ID)
		}
		isError, ok := ends[i].Payload["is_error"].(bool)
		if !ok || isError != expectedErrors[i] {
			t.Fatalf("model request end %d is_error = %#v, want %v", i, ends[i].Payload["is_error"], expectedErrors[i])
		}
	}
}

func typeList(events []domain.Event) string {
	var out strings.Builder
	for _, event := range events {
		out.WriteString(event.Type)
		out.WriteByte(' ')
	}
	return out.String()
}

func terminateIntegrationWorkflow(t *testing.T, temporalClient client.Client, workflowID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := temporalClient.TerminateWorkflow(ctx, workflowID, "", "mango integration test cleanup"); err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return
		}
		t.Errorf("terminate integration workflow %s: %v", workflowID, err)
	}
}

func assertOrder(t *testing.T, events []domain.Event, types ...string) {
	t.Helper()
	if !eventsHaveOrder(events, types...) {
		t.Fatalf("events not in expected order %v; got %s", types, typeList(events))
	}
}
