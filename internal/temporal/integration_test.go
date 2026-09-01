package temporal_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
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
	"github.com/yanpgwang/mango/internal/pg"
	"github.com/yanpgwang/mango/internal/sandbox"
	"github.com/yanpgwang/mango/internal/sandbox/sandboxtest"
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
		TemporalClient: c, Store: store, ModelClient: probe,
		SandboxProvider: sandboxtest.NoProvision(t), IDGenerator: ids,
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
		TemporalClient:  c,
		Store:           store,
		ModelClient:     modelClient,
		SandboxProvider: sandboxtest.NoProvision(t),
		IDGenerator:     ids,
		RelayConfig:     temporalpkg.RelayConfig{PollInterval: 200 * time.Millisecond},
		TaskQueue:       "mango-test-" + ids.NewID(""),
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

// TestLifecycleReconciler_RecoversPreparedDeletionEndToEnd models an API
// process exiting after PostgreSQL commits the deletion fence but before it
// starts Temporal cleanup. A worker-side scan must discover the row, release
// the persisted sandbox through the deterministic cleanup Workflow, and
// physically finalize the Session without another DELETE request.
func TestLifecycleReconciler_RecoversPreparedDeletionEndToEnd(t *testing.T) {
	dbURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	hostPort := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if dbURL == "" || hostPort == "" {
		t.Skip("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT to run lifecycle recovery")
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
	provider := sandboxtest.OpenSandboxProvider(t)
	runtime := temporalpkg.NewRuntime(temporalpkg.RuntimeConfig{
		TemporalClient:  c,
		Store:           store,
		ModelClient:     model.NewFake(),
		SandboxProvider: provider,
		IDGenerator:     ids,
		RelayConfig:     temporalpkg.RelayConfig{},
		TaskQueue:       "mango-lifecycle-test-" + ids.NewID(""),
	})
	if err := runtime.Worker.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer runtime.Worker.Stop()

	session := domain.Session{
		ID:            "sess_lifecycle_" + ids.NewID(""),
		AgentID:       "agent_1",
		AgentVersion:  1,
		EnvironmentID: "env_1",
		Status:        domain.StatusIdle,
		Metadata:      map[string]any{},
		CreatedAt:     time.Now().UTC(),
		UpdatedAt:     time.Now().UTC(),
		AgentSnapshot: domain.Agent{
			ID: "agent_1", Version: 1, Name: "coordinator",
			Model: domain.NormalizeModel(domain.Model{ID: "coordinator-model"}),
			Multiagent: &domain.Multiagent{
				Type: "coordinator",
				Agents: []domain.AgentReference{{
					Type: "agent", ID: "agent_child", Version: 1,
				}},
			},
		},
		MultiagentRoster: []domain.Agent{{
			ID: "agent_child", Version: 1, Name: "child",
			Model: domain.NormalizeModel(domain.Model{ID: "child-model"}),
		}},
	}
	if _, err := store.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	box, err := runtime.Sandbox.Acquire(ctx, session.ID, sandbox.Spec{})
	if err != nil {
		t.Fatalf("acquire sandbox: %v", err)
	}
	binding, found, err := store.GetSandboxBinding(ctx, session.ID)
	if err != nil || !found {
		t.Fatalf("load sandbox binding: found=%v err=%v", found, err)
	}
	if err := box.WriteFile(ctx, "before-crash", []byte("durable")); err != nil {
		t.Fatalf("write sandbox marker: %v", err)
	}
	threads, err := store.ListSessionThreads(
		ctx, session.ID, app.SessionThreadListQuery{Limit: 1},
	)
	if err != nil || len(threads) != 1 {
		t.Fatalf("primary Threads = %+v, err=%v", threads, err)
	}
	child, _, err := store.CreateChildSessionThread(
		ctx, session.ID, threads[0].ID, "child",
	)
	if err != nil {
		t.Fatalf("create child Thread: %v", err)
	}
	if err := runtime.Signal.WakeThread(ctx, session.ID, child.ID, 0); err != nil {
		t.Fatalf("start child Workflow: %v", err)
	}
	childWorkflowID := "session-thread:" + child.ID
	described, err := c.DescribeWorkflowExecution(ctx, childWorkflowID, "")
	if err != nil || described.WorkflowExecutionInfo.Status !=
		enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		t.Fatalf("child Workflow before deletion = %+v, err=%v", described, err)
	}
	if err := store.PrepareSessionDeletion(ctx, session.ID); err != nil {
		t.Fatalf("prepare deletion: %v", err)
	}

	reconcileCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := runtime.Lifecycle.RunOnce(reconcileCtx)
	if err != nil {
		t.Fatalf("reconcile deletion: %v", err)
	}
	if result.Deletions != 1 {
		t.Fatalf("reconciled deletions = %d, want 1", result.Deletions)
	}
	if _, err := store.GetSession(ctx, session.ID); err == nil {
		t.Fatal("session survived reconciled deletion")
	}
	if _, found, err := store.GetSandboxBinding(ctx, session.ID); err != nil || found {
		t.Fatalf("sandbox binding survived: found=%v err=%v", found, err)
	}
	if _, err := provider.Attach(ctx, session.ID, binding.Ref, sandbox.Spec{}); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("OpenSandbox sandbox survived cleanup or attachment failed: %v", err)
	}
	described, err = c.DescribeWorkflowExecution(ctx, childWorkflowID, "")
	if err != nil || described.WorkflowExecutionInfo.Status !=
		enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED {
		t.Fatalf("child Workflow after deletion = %+v, err=%v", described, err)
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
		TemporalClient:  c,
		Store:           store,
		ModelClient:     blockingModel,
		SandboxProvider: sandboxtest.NoProvision(t),
		IDGenerator:     ids,
		RelayConfig:     temporalpkg.RelayConfig{PollInterval: 200 * time.Millisecond},
		TaskQueue:       "mango-test-" + ids.NewID(""),
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

// TestVerticalSlice_LiveModelToolStepEndToEnd verifies the external model
// contract beyond plain text streaming: a real model selects the offered bash
// tool, Mango executes it in OpenSandbox as a durable sandbox Activity, feeds the
// result back into the provider transcript, and commits the final assistant
// response. It is opt-in because it reaches a billable model endpoint.
func TestVerticalSlice_LiveModelToolStepEndToEnd(t *testing.T) {
	modelClient, modelID := liveModelForTest(t, "tool conformance test")
	provider := sandboxtest.OpenSandboxProvider(t)
	const marker = "mango-live-tool-ok"
	runToolStepEndToEnd(t, toolStepCase{
		provider:      provider,
		modelClient:   modelClient,
		modelID:       modelID,
		sessionPrefix: "sess_live_tool_e2e_",
		prompt: fmt.Sprintf(
			"Use the bash tool exactly once. Pass the text between <command> tags as the command without changes; do not include the tags. <command>printf '%s' > live-tool.txt && cat live-tool.txt</command> After you receive the tool result, reply with a short confirmation and do not call another tool.",
			marker,
		),
		tools:              bashOnlyToolset(t),
		expectedTool:       bashToolName,
		expectedToolOutput: marker,
		timeout:            2 * time.Minute,
	})
}

// TestVerticalSlice_OpenSandboxToolStepEndToEnd runs the PostgreSQL + Temporal
// tool path through the local OpenSandbox Docker runtime. It commits the journal, tool events, final message and
// terminal idle status. Its command checks
// /.dockerenv and /workspace before writing and reading the marker. The committed
// non-error tool_result therefore proves the Activity actually executed inside
// the provisioned container, not merely that provisioning succeeded.
func TestVerticalSlice_OpenSandboxToolStepEndToEnd(t *testing.T) {
	if os.Getenv("MANGO_TEST_DATABASE_URL") == "" ||
		os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT") == "" {
		t.Skip("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT to run the OpenSandbox tool end-to-end slice")
	}
	provider := sandboxtest.OpenSandboxProvider(t)
	const marker = "mango-temporal-opensandbox-ok"
	runToolStepEndToEnd(t, toolStepCase{
		provider: provider,
		modelClient: toolProbeModel{
			command:   "test -f /.dockerenv && test \"$(pwd)\" = /workspace && printf '" + marker + "' > probe.txt && cat probe.txt",
			finalText: "OpenSandbox probe completed",
		},
		modelID:            "fake",
		sessionPrefix:      "sess_opensandbox_tool_e2e_",
		prompt:             "run a tool",
		tools:              []any{map[string]any{"type": domain.BuiltinToolsetType}},
		expectedTool:       bashToolName,
		expectedToolOutput: marker,
		timeout:            30 * time.Second,
	})
}

// TestVerticalSlice_OpenSandboxSkillRuntimeEndToEnd proves the complete custom Skill
// execution path: PostgreSQL pins one immutable Version, PrepareTurn exposes its
// discovery metadata and private Skill dispatcher, the pre-tool reconciler
// extracts the canonical archive, and the runtime injects the complete SKILL.md
// without asking the model to call read or bash.
func TestVerticalSlice_OpenSandboxSkillRuntimeEndToEnd(t *testing.T) {
	if os.Getenv("MANGO_TEST_DATABASE_URL") == "" ||
		os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT") == "" {
		t.Skip("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT to run the OpenSandbox Skill end-to-end slice")
	}
	provider := sandboxtest.OpenSandboxProvider(t)
	runToolStepEndToEnd(t, toolStepCase{
		provider: provider,
		modelClient: skillProbeModel{
			skillName:      "runtime-probe",
			marker:         "runtime-e2e-marker",
			finalText:      "Skill probe completed",
			requiredSystem: "/workspace/skills/runtime-probe/SKILL.md",
		},
		modelID:            "fake",
		sessionPrefix:      "sess_opensandbox_skill_e2e_",
		prompt:             "use the runtime probe Skill",
		tools:              []any{map[string]any{"type": domain.BuiltinToolsetType}},
		expectedTool:       agentruntime.RuntimeSkillToolName,
		expectedToolOutput: "Launching skill: runtime-probe",
		timeout:            30 * time.Second,
		setup: func(
			t *testing.T,
			ctx context.Context,
			store *pg.Store,
			ids domain.IDGenerator,
		) ([]domain.SkillReference, temporalpkg.SandboxResourceReconciler) {
			t.Helper()
			blobs := newIntegrationBlobStore()
			skills := app.NewSkillService(
				pg.NewSkillRepository(store), blobs, ids,
				domain.FixedClock{T: time.Now().UTC()},
			)
			created, err := skills.Create(ctx, app.SkillCreateInput{
				Files: []app.SkillUploadFile{{
					Filename: "Runtime_Probe/SKILL.md",
					Body:     []byte("---\nname: runtime-probe\ndescription: Verify mounted runtime Skills\n---\nruntime-e2e-marker\n"),
				}},
			})
			if err != nil {
				t.Fatalf("create integration Skill: %v", err)
			}
			return []domain.SkillReference{{
					Type: "custom", SkillID: created.ID, Version: created.LatestVersion,
				}}, app.NewSessionRuntimeMaterializer(
					nil, app.NewSessionSkillMaterializer(store, blobs),
				)
		},
	})
}

type toolStepCase struct {
	provider           sandbox.Provider
	modelClient        model.Client
	modelID            string
	sessionPrefix      string
	prompt             string
	tools              []any
	expectedTool       string
	expectedToolOutput string
	timeout            time.Duration
	setup              func(
		*testing.T,
		context.Context,
		*pg.Store,
		domain.IDGenerator,
	) ([]domain.SkillReference, temporalpkg.SandboxResourceReconciler)
}

const bashToolName = "bash"

func bashOnlyToolset(t *testing.T) []any {
	t.Helper()
	raw := []any{map[string]any{
		"type": domain.BuiltinToolsetType,
		"default_config": map[string]any{
			"enabled": false,
			"permission_policy": map[string]any{
				"type": "always_allow",
			},
		},
		"configs": []any{map[string]any{
			"name": bashToolName, "enabled": true,
			"permission_policy": map[string]any{
				"type": "always_allow",
			},
		}},
	}}
	parsed, err := domain.ParseTools(raw)
	if err != nil {
		t.Fatalf("parse live tool configuration: %v", err)
	}
	bashEnabled, bashPolicy := parsed.BuiltinEnabled(bashToolName)
	if !bashEnabled || bashPolicy.Type != "always_allow" {
		t.Fatalf("live tool configuration enables bash = %v with policy %q, want true with always_allow", bashEnabled, bashPolicy.Type)
	}
	for _, name := range domain.BuiltinToolNames {
		enabled, policy := parsed.BuiltinEnabled(name)
		wantEnabled := name == bashToolName
		if enabled != wantEnabled {
			t.Fatalf("live tool configuration enables %q = %v, want %v", name, enabled, wantEnabled)
		}
		if enabled && policy.Type != "always_allow" {
			t.Fatalf("live tool configuration policy for %q = %q, want always_allow", name, policy.Type)
		}
	}
	return raw
}

func runToolStepEndToEnd(t *testing.T, tc toolStepCase) {
	t.Helper()
	if tc.provider == nil || tc.modelClient == nil {
		t.Fatal("tool step test provider and model client are required")
	}
	if tc.modelID == "" || tc.sessionPrefix == "" || tc.prompt == "" || tc.expectedTool == "" {
		t.Fatal("tool step test model ID, session prefix, prompt, and expected tool are required")
	}
	if tc.timeout <= 0 {
		t.Fatal("tool step test timeout must be positive")
	}
	if tc.expectedToolOutput == "" {
		t.Fatal("tool step test expected output must be non-empty")
	}
	dbURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	hostPort := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if dbURL == "" || hostPort == "" {
		t.Skip("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT to run the tool end-to-end slice")
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
	var skills []domain.SkillReference
	var resources temporalpkg.SandboxResourceReconciler
	if tc.setup != nil {
		skills, resources = tc.setup(t, ctx, store, ids)
	}
	taskQueue := "mango-test-" + ids.NewID("")
	runtime := temporalpkg.NewRuntime(temporalpkg.RuntimeConfig{
		TemporalClient:  c,
		Store:           store,
		ModelClient:     tc.modelClient,
		SandboxProvider: tc.provider,
		IDGenerator:     ids,
		RelayConfig:     temporalpkg.RelayConfig{PollInterval: 200 * time.Millisecond},
		TaskQueue:       taskQueue,
		Resources:       resources,
	})

	if err := runtime.Worker.Start(); err != nil {
		t.Fatalf("worker start: %v", err)
	}
	defer runtime.Worker.Stop()
	relayCtx, stopRelay := context.WithCancel(ctx)
	defer stopRelay()
	go func() { _ = runtime.Relay.Run(relayCtx) }()

	orch := runtime.Orchestrator()
	sessID := tc.sessionPrefix + ids.NewID("")
	// SessionManager keeps a sandbox alive across turns by design. Explicitly
	// release it after this integration test so the OpenSandbox variant cannot leak a
	// container (the second call is a harmless no-op after normal-path release).
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = runtime.Sandbox.Release(releaseCtx, sessID)
	}()
	sess := domain.Session{
		ID:            sessID,
		AgentID:       "agent_1",
		AgentVersion:  1,
		EnvironmentID: "env_1",
		Status:        domain.StatusIdle,
		Metadata:      map[string]any{},
		AgentSnapshot: domain.Agent{
			ID: "agent_1", Version: 1, Model: domain.Model{ID: tc.modelID},
			Tools: tc.tools, Skills: skills,
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	if _, _, err := orch.CreateSession(ctx, sess, nil); err != nil {
		t.Fatalf("create session: %v", err)
	}
	if _, err := orch.Admit(ctx, sessID, []domain.EventDraft{{
		Type:    domain.EvUserMessage,
		Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": tc.prompt}}},
	}}); err != nil {
		t.Fatalf("admit: %v", err)
	}
	// The production workflow is intentionally long-lived at idle. Integration
	// tests use disposable PostgreSQL schemas, so terminate their execution before
	// cleanup drops the schema; otherwise a later local worker can pick up a stale
	// retry against data that no longer exists.
	defer terminateIntegrationWorkflow(t, c, sessID)

	expectedOrder := []string{
		domain.EvUserMessage,
		domain.EvSessionStatusRunning,
		domain.EvSpanModelRequestStart,
		domain.EvSpanModelRequestEnd,
		domain.EvAgentToolUse,
		domain.EvAgentToolResult,
		domain.EvSpanModelRequestStart,
		domain.EvAgentMessage,
		domain.EvSpanModelRequestEnd,
		domain.EvSessionStatusIdle,
	}
	deadline := time.Now().Add(tc.timeout)
	var events []domain.Event
	completed := false
	for time.Now().Before(deadline) {
		events, err = store.EventsAfter(ctx, sessID, 0, 100)
		if err != nil {
			t.Fatalf("events: %v", err)
		}
		if failure, ok := firstFailureEvent(events); ok {
			t.Fatalf("tool workflow failed with %s: %#v; events=%s", failure.Type, failure.Payload, typeList(events))
		}
		if eventsHaveOrder(events, expectedOrder...) {
			completed = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !completed {
		t.Fatalf("timed out after %s waiting for %v; got %s", tc.timeout, expectedOrder, typeList(events))
	}

	assertOrder(t, events, expectedOrder...)
	assertModelRequestSpans(t, events, false, false)
	toolUses := eventsOfType(events, domain.EvAgentToolUse)
	if len(toolUses) != 1 {
		t.Fatalf("agent.tool_use count = %d, want exactly 1; got %s", len(toolUses), typeList(events))
	}
	toolUse := toolUses[0]
	toolName, ok := toolUse.Payload["name"].(string)
	if !ok || toolName == "" {
		t.Fatalf("agent.tool_use has invalid name payload: %#v", toolUse.Payload)
	}
	if toolName != tc.expectedTool {
		t.Fatalf("agent.tool_use name = %q, want %q", toolName, tc.expectedTool)
	}
	if permission, _ := toolUse.Payload["evaluated_permission"].(string); permission != "allow" {
		t.Fatalf("agent.tool_use evaluated_permission = %q, want allow", permission)
	}
	toolResults := eventsOfType(events, domain.EvAgentToolResult)
	if len(toolResults) != 1 {
		t.Fatalf("agent.tool_result count = %d, want exactly 1; got %s", len(toolResults), typeList(events))
	}
	toolResult := toolResults[0]
	toolUseID, ok := toolResult.Payload["tool_use_id"].(string)
	if !ok || toolUseID != toolUse.ID {
		t.Fatalf("agent.tool_result tool_use_id = %q, want %q", toolUseID, toolUse.ID)
	}
	text, isError, ok := eventText(toolResult)
	if !ok {
		t.Fatalf("agent.tool_result has invalid content payload: %#v", toolResult.Payload)
	}
	if isError {
		t.Fatalf("tool_result is_error=true; content=%q", text)
	}
	if strings.TrimSpace(text) != tc.expectedToolOutput {
		t.Fatalf("tool output = %q, want %q", text, tc.expectedToolOutput)
	}
	finalMessage, ok := firstEventOfTypeAfter(events, toolResult.ID, domain.EvAgentMessage)
	if !ok {
		t.Fatalf("agent.message missing after tool result; got %s", typeList(events))
	}
	finalText, _, ok := eventText(finalMessage)
	if !ok || strings.TrimSpace(finalText) == "" {
		t.Fatalf("final agent.message has empty or invalid content: %#v", finalMessage.Payload)
	}

	final, err := store.GetSession(ctx, sessID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if final.Status != domain.StatusIdle {
		t.Fatalf("expected idle, got %s", final.Status)
	}
	binding, found, err := store.GetSandboxBinding(ctx, sessID)
	if err != nil {
		t.Fatalf("get sandbox binding: %v", err)
	}
	if !found || binding.Ref.Provider != tc.provider.Name() || binding.Ref.ID == "" {
		t.Fatalf("sandbox binding = %+v, found=%v", binding, found)
	}
	if err := store.PrepareSessionDeletion(ctx, sessID); err != nil {
		t.Fatalf("prepare session deletion: %v", err)
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := orch.TerminateSession(releaseCtx, sessID); err != nil {
		t.Fatalf("terminate session and release sandbox: %v", err)
	}
	if _, found, err := store.GetSandboxBinding(ctx, sessID); err != nil || found {
		t.Fatalf("sandbox binding survived cleanup: found=%v err=%v", found, err)
	}
	if err := store.FinalizeSessionDeletion(releaseCtx, sessID); err != nil {
		t.Fatalf("finalize session deletion: %v", err)
	}
}

// toolProbeModel is a deterministic, retry-safe model client for integration
// tests. Before a tool result exists it requests one real bash step; afterwards
// it ends the turn. Behavior depends only on projected history, not a mutable
// call counter, so an Activity retry receives the same response.
type toolProbeModel struct {
	command        string
	finalText      string
	requiredSystem string
}

// skillProbeModel proves the CCB context lifecycle rather than merely proving
// that the files exist. It selects the runtime's private Skill dispatcher on
// the first round and refuses to finish unless the next request contains the
// complete injected SKILL.md body as a sibling user content block.
type skillProbeModel struct {
	skillName      string
	marker         string
	finalText      string
	requiredSystem string
}

func (m skillProbeModel) CreateMessage(
	_ context.Context,
	req model.Request,
) (model.Response, error) {
	if m.requiredSystem != "" && !strings.Contains(req.System, m.requiredSystem) {
		return model.Response{}, fmt.Errorf(
			"model request did not discover required Skill path %q",
			m.requiredSystem,
		)
	}
	dispatcherOffered := false
	for _, tool := range req.Tools {
		if tool.Name == agentruntime.RuntimeSkillToolName {
			dispatcherOffered = true
			break
		}
	}
	if !dispatcherOffered {
		return model.Response{}, errors.New("runtime Skill dispatcher was not offered")
	}
	seenResult := false
	seenBody := false
	for _, message := range req.Messages {
		for _, block := range message.Content {
			switch block.Type {
			case "tool_result":
				seenResult = true
			case "text":
				if strings.HasPrefix(
					block.Text,
					"Base directory for this skill: /workspace/skills/"+m.skillName,
				) && strings.Contains(block.Text, m.marker) {
					seenBody = true
				}
			}
		}
	}
	if seenResult {
		if !seenBody {
			return model.Response{}, fmt.Errorf(
				"Skill tool result returned without the complete SKILL.md injection: %s",
				summarizeSkillProbeMessages(req.Messages),
			)
		}
		return model.Response{
			Content:    []domain.ContentBlock{{Type: "text", Text: m.finalText}},
			StopReason: "end_turn",
		}, nil
	}
	return model.Response{
		Content: []domain.ContentBlock{{
			Type: "tool_use", ToolUseID: "probe_skill_1",
			ToolName: agentruntime.RuntimeSkillToolName,
			Input:    map[string]any{"skill": m.skillName},
		}},
		StopReason: "tool_use",
	}, nil
}

func summarizeSkillProbeMessages(messages []domain.Message) string {
	const prefixLimit = 120
	parts := make([]string, 0, len(messages))
	for messageIndex, message := range messages {
		for blockIndex, block := range message.Content {
			text := block.Text
			if len(text) > prefixLimit {
				text = text[:prefixLimit] + "..."
			}
			parts = append(parts, fmt.Sprintf(
				"message[%d](role=%q).content[%d](type=%q,text_len=%d,text_prefix=%q)",
				messageIndex,
				message.Role,
				blockIndex,
				block.Type,
				len(block.Text),
				text,
			))
		}
	}
	return strings.Join(parts, "; ")
}

func (m toolProbeModel) CreateMessage(_ context.Context, req model.Request) (model.Response, error) {
	if m.requiredSystem != "" && !strings.Contains(req.System, m.requiredSystem) {
		return model.Response{}, fmt.Errorf(
			"model request did not discover required Skill path %q",
			m.requiredSystem,
		)
	}
	for _, message := range req.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_result" {
				return model.Response{
					Content:    []domain.ContentBlock{{Type: "text", Text: m.finalText}},
					StopReason: "end_turn",
				}, nil
			}
		}
	}
	return model.Response{
		Content: []domain.ContentBlock{{
			Type: "tool_use", ToolUseID: "probe_tool_1", ToolName: bashToolName,
			Input: map[string]any{"command": m.command},
		}},
		StopReason: "tool_use",
	}, nil
}

func (m skillProbeModel) CreateMessageStream(
	ctx context.Context,
	req model.Request,
	onDelta func(index int, text string),
) (model.Response, error) {
	response, err := m.CreateMessage(ctx, req)
	if err == nil && response.StopReason == "end_turn" &&
		len(response.Content) == 1 && onDelta != nil {
		onDelta(0, response.Content[0].Text)
	}
	return response, err
}

type integrationBlobStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newIntegrationBlobStore() *integrationBlobStore {
	return &integrationBlobStore{objects: make(map[string][]byte)}
}

func (s *integrationBlobStore) Put(
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

func (s *integrationBlobStore) Open(
	_ context.Context,
	key string,
) (io.ReadCloser, error) {
	s.mu.Lock()
	data, ok := s.objects[key]
	s.mu.Unlock()
	if !ok {
		return nil, errors.New("integration blob not found")
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}

func (s *integrationBlobStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	delete(s.objects, key)
	s.mu.Unlock()
	return nil
}

func (m toolProbeModel) CreateMessageStream(ctx context.Context, req model.Request, onDelta func(index int, text string)) (model.Response, error) {
	resp, err := m.CreateMessage(ctx, req)
	if err == nil && resp.StopReason == "end_turn" && len(resp.Content) == 1 && onDelta != nil {
		onDelta(0, resp.Content[0].Text)
	}
	return resp, err
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

func (m *interruptBlockingModel) CreateMessage(
	ctx context.Context,
	req model.Request,
) (model.Response, error) {
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

func hasType(events []domain.Event, t string) bool {
	for _, e := range events {
		if e.Type == t {
			return true
		}
	}
	return false
}

func eventsHaveOrder(events []domain.Event, types ...string) bool {
	idx := 0
	for _, event := range events {
		if idx < len(types) && event.Type == types[idx] {
			idx++
		}
	}
	return idx == len(types)
}

func firstFailureEvent(events []domain.Event) (domain.Event, bool) {
	for _, event := range events {
		if event.Type == domain.EvSessionError || event.Type == domain.EvSessionStatusTerminated {
			return event, true
		}
	}
	return domain.Event{}, false
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

func assertModelRequestSpans(t *testing.T, events []domain.Event, errors ...bool) {
	t.Helper()
	starts := eventsOfType(events, domain.EvSpanModelRequestStart)
	ends := eventsOfType(events, domain.EvSpanModelRequestEnd)
	if len(starts) != len(errors) || len(ends) != len(errors) {
		t.Fatalf(
			"model request spans = %d starts/%d ends, want %d each; got %s",
			len(starts), len(ends), len(errors), typeList(events),
		)
	}
	for i := range errors {
		startID, _ := ends[i].Payload["model_request_start_id"].(string)
		if startID != starts[i].ID {
			t.Fatalf("model request end %d references %q, want %q", i, startID, starts[i].ID)
		}
		isError, ok := ends[i].Payload["is_error"].(bool)
		if !ok || isError != errors[i] {
			t.Fatalf("model request end %d is_error = %#v, want %v", i, ends[i].Payload["is_error"], errors[i])
		}
	}
}

func typeList(events []domain.Event) string {
	s := ""
	for _, e := range events {
		s += e.Type + " "
	}
	return s
}

func firstEventOfTypeAfter(events []domain.Event, afterID, eventType string) (domain.Event, bool) {
	after := false
	for _, event := range events {
		if after && event.Type == eventType {
			return event, true
		}
		if event.ID == afterID {
			after = true
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

func terminateIntegrationWorkflow(t *testing.T, c client.Client, workflowID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.TerminateWorkflow(ctx, workflowID, "", "mango integration test cleanup"); err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			return
		}
		t.Errorf("terminate integration workflow %s: %v", workflowID, err)
	}
}

// assertOrder checks that the given event types appear in the slice in the given
// relative order (not necessarily contiguous).
func assertOrder(t *testing.T, events []domain.Event, types ...string) {
	t.Helper()
	if !eventsHaveOrder(events, types...) {
		t.Fatalf("events not in expected order %v; got %s", types, typeList(events))
	}
}
