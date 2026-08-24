package temporal_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
	"github.com/yanpgwang/mango/internal/sandbox"
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
	"go.temporal.io/sdk/client"
)

const gateActionCount = 7

const liveGateActionCount = 2

// TestVerticalSlice_HITLGateSurvivesWorkerRestart exercises Mango's durable
// client-action boundary over real PostgreSQL and Temporal. One model response
// emits seven parallel custom-tool calls. Mango exposes the complete barrier,
// accepts partial results without resuming early, atomically rejects a
// duplicate, and resumes the whole result round after the execution worker is
// replaced.
func TestVerticalSlice_HITLGateSurvivesWorkerRestart(t *testing.T) {
	databaseURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	temporalAddress := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if databaseURL == "" || temporalAddress == "" {
		t.Skip("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT to run the HITL gate scenario")
	}

	ctx := context.Background()
	store, cleanup := integrationStore(t, databaseURL)
	defer cleanup()
	temporalClient, err := client.Dial(client.Options{HostPort: temporalAddress})
	if err != nil {
		t.Skipf("temporal unreachable at %s: %v", temporalAddress, err)
	}
	defer temporalClient.Close()

	ids := domain.NewRandomIDGen()
	probe := &gateProbeModel{}
	taskQueue := "mango-hitl-gate-" + ids.NewID("")
	runtimeConfig := temporalpkg.RuntimeConfig{
		TemporalClient:  temporalClient,
		Store:           store,
		ModelClient:     probe,
		SandboxProvider: sandbox.NewLocalProvider(),
		IDGenerator:     ids,
		RelayConfig:     temporalpkg.RelayConfig{PollInterval: 50 * time.Millisecond},
		TaskQueue:       taskQueue,
	}

	runtimeOne := temporalpkg.NewRuntime(runtimeConfig)
	stopRuntimeOne := startGateRuntime(t, ctx, runtimeOne)
	runtimeOneStopped := false
	defer func() {
		if !runtimeOneStopped {
			stopRuntimeOne()
		}
	}()

	system := "Classify every submitted expense exactly once. Use decide for clear cases and escalate when a human judgment is required."
	session := domain.Session{
		ID:                "sesn_hitl_gate_" + ids.NewID(""),
		AgentID:           "agent_hitl_gate",
		AgentVersion:      1,
		EnvironmentID:     "env_hitl_gate",
		EnvironmentType:   "cloud",
		EnvironmentConfig: map[string]any{"type": "cloud"},
		Status:            domain.StatusIdle,
		Metadata:          map[string]any{},
		AgentSnapshot: domain.Agent{
			ID: "agent_hitl_gate", Version: 1, Name: "expense-gate",
			Model: domain.Model{ID: "gate-probe"}, System: &system,
			Tools: gateCustomTools(),
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	orchestrator := runtimeOne.Orchestrator()
	if _, _, err := orchestrator.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create HITL gate Session: %v", err)
	}
	defer terminateIntegrationWorkflow(t, temporalClient, session.ID)

	if _, err := orchestrator.Admit(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{map[string]any{
			"type": "text", "text": "Process expenses r01 through r07 and record one decision for each.",
		}}},
	}}); err != nil {
		t.Fatalf("admit HITL gate task: %v", err)
	}

	_, actions, actionIDs := waitForGateBarrier(
		t, store, session.ID, gateActionCount, 30*time.Second,
	)
	if got := probe.callCount(); got != 1 {
		t.Fatalf("model calls before resolution = %d, want 1", got)
	}
	assertGateActions(t, actions)
	assertStringSetEqual(t, actionIDs, eventIDs(actions), "requires_action event ids")

	const partialCount = 3
	partialDrafts := gateResolutionDrafts(actions[:partialCount])
	partialEvents, err := orchestrator.Admit(ctx, session.ID, partialDrafts)
	if err != nil {
		t.Fatalf("admit partial HITL results: %v", err)
	}
	if len(partialEvents) != partialCount {
		t.Fatalf("partial result events = %d, want %d", len(partialEvents), partialCount)
	}
	assertGatePendingState(t, store, session.ID, gateActionCount, partialCount)

	beforeDuplicate, err := store.EventsAfter(ctx, session.ID, 0, 200)
	if err != nil {
		t.Fatalf("events before duplicate: %v", err)
	}
	_, err = orchestrator.Admit(ctx, session.ID, gateResolutionDrafts(actions[:1]))
	var domainErr *domain.DomainError
	if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindConflict {
		t.Fatalf("duplicate result error = %#v, want conflict", err)
	}
	afterDuplicate, err := store.EventsAfter(ctx, session.ID, 0, 200)
	if err != nil {
		t.Fatalf("events after duplicate: %v", err)
	}
	if len(afterDuplicate) != len(beforeDuplicate) {
		t.Fatalf(
			"duplicate admission committed events: before=%d after=%d",
			len(beforeDuplicate), len(afterDuplicate),
		)
	}
	if got := probe.callCount(); got != 1 {
		t.Fatalf("partial barrier resumed the model: calls=%d", got)
	}
	current, err := store.GetSession(ctx, session.ID)
	if err != nil || current.Status != domain.StatusIdle {
		t.Fatalf("partially resolved Session = %+v, err=%v", current, err)
	}

	stopRuntimeOne()
	runtimeOneStopped = true
	runtimeTwo := temporalpkg.NewRuntime(runtimeConfig)
	stopRuntimeTwo := startGateRuntime(t, ctx, runtimeTwo)
	defer stopRuntimeTwo()

	rest := gateResolutionDrafts(actions[partialCount:])
	finalResolutionEvents, err := runtimeTwo.Orchestrator().Admit(ctx, session.ID, rest)
	if err != nil {
		t.Fatalf("admit remaining HITL results after worker restart: %v", err)
	}
	if len(finalResolutionEvents) != gateActionCount-partialCount {
		t.Fatalf(
			"remaining result events = %d, want %d",
			len(finalResolutionEvents), gateActionCount-partialCount,
		)
	}

	events := waitForGateCompletion(t, store, session.ID, 30*time.Second)
	if got := probe.callCount(); got != 2 {
		t.Fatalf("model calls after complete barrier = %d, want 2", got)
	}
	if got := len(eventsOfType(events, domain.EvAgentCustomToolUse)); got != gateActionCount {
		t.Fatalf("custom tool uses = %d, want %d; events=%s", got, gateActionCount, typeList(events))
	}
	if got := len(eventsOfType(events, domain.EvUserCustomToolResult)); got != gateActionCount {
		t.Fatalf("custom tool results = %d, want %d; events=%s", got, gateActionCount, typeList(events))
	}
	if got := len(eventsOfType(events, domain.EvSpanModelRequestStart)); got != 2 {
		t.Fatalf("model request starts = %d, want 2; events=%s", got, typeList(events))
	}
	if failures := eventsOfType(events, domain.EvSessionError); len(failures) != 0 {
		t.Fatalf("HITL gate emitted Session errors: %+v", failures)
	}
	pending, err := store.UnresolvedPendingActions(ctx, session.ID)
	if err != nil {
		t.Fatalf("list final pending actions: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("resolved gate retained pending actions: %+v", pending)
	}
	for _, resolution := range append(partialEvents, finalResolutionEvents...) {
		stored, getErr := store.GetEvent(ctx, session.ID, resolution.ID)
		if getErr != nil || stored.ProcessedAt == nil {
			t.Fatalf("resolution %s not durably processed: %+v err=%v", resolution.ID, stored, getErr)
		}
	}
	final, err := store.GetSession(ctx, session.ID)
	if err != nil || final.Status != domain.StatusIdle {
		t.Fatalf("completed HITL gate Session = %+v, err=%v", final, err)
	}
}

// TestVerticalSlice_LiveModelHITLGateEndToEnd executes the documented
// expense-approval journey with the explicitly configured Messages endpoint.
// Mango persists real model-generated custom-tool calls, the test client acts
// as the external approval application, and the same model receives the
// correlated results before completing the Session.
func TestVerticalSlice_LiveModelHITLGateEndToEnd(t *testing.T) {
	modelClient, modelID := liveModelForTest(t, "HITL gate scenario")
	databaseURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	temporalAddress := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")

	ctx := context.Background()
	store, cleanup := integrationStore(t, databaseURL)
	defer cleanup()
	temporalClient, err := client.Dial(client.Options{HostPort: temporalAddress})
	if err != nil {
		t.Skipf("temporal unreachable at %s: %v", temporalAddress, err)
	}
	defer temporalClient.Close()

	ids := domain.NewRandomIDGen()
	runtime := temporalpkg.NewRuntime(temporalpkg.RuntimeConfig{
		TemporalClient:  temporalClient,
		Store:           store,
		ModelClient:     modelClient,
		SandboxProvider: sandbox.NewLocalProvider(),
		IDGenerator:     ids,
		RelayConfig:     temporalpkg.RelayConfig{PollInterval: 100 * time.Millisecond},
		TaskQueue:       "mango-hitl-gate-live-" + ids.NewID(""),
	})
	stopRuntime := startGateRuntime(t, ctx, runtime)
	defer stopRuntime()

	system := "You process expense receipts through application-owned tools. Follow the supplied policy and call exactly one tool per receipt. After tool results arrive, summarize the recorded outcomes without calling another tool."
	session := domain.Session{
		ID:                "sesn_hitl_gate_live_" + ids.NewID(""),
		AgentID:           "agent_hitl_gate_live",
		AgentVersion:      1,
		EnvironmentID:     "env_hitl_gate_live",
		EnvironmentType:   "cloud",
		EnvironmentConfig: map[string]any{"type": "cloud"},
		Status:            domain.StatusIdle,
		Metadata:          map[string]any{},
		AgentSnapshot: domain.Agent{
			ID: "agent_hitl_gate_live", Version: 1, Name: "expense-gate-live",
			Model: domain.Model{ID: modelID}, System: &system,
			Tools: gateCustomTools(),
		},
		CreatedAt: time.Now().UTC(),
		UpdatedAt: time.Now().UTC(),
	}
	orchestrator := runtime.Orchestrator()
	if _, _, err := orchestrator.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create live HITL gate Session: %v", err)
	}
	defer terminateIntegrationWorkflow(t, temporalClient, session.ID)

	prompt := strings.Join([]string{
		"Apply this expense policy: office supplies at or below USD 100 with a receipt are approved; expenses above USD 500 without an itemized receipt require human review.",
		"Process exactly two receipts in one response.",
		"Receipt r01 is USD 12 for office pencils and has an itemized receipt. Call decide with action approve.",
		"Receipt r02 is USD 900 for an unspecified team activity and has no itemized receipt. Call escalate with a useful reviewer question.",
		"Call exactly one tool for each receipt now, with no prose before the two tool calls.",
	}, " ")
	if _, err := orchestrator.Admit(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{map[string]any{
			"type": "text", "text": prompt,
		}}},
	}}); err != nil {
		t.Fatalf("admit live HITL gate task: %v", err)
	}

	_, actions, actionIDs := waitForGateBarrier(
		t, store, session.ID, liveGateActionCount, 2*time.Minute,
	)
	assertLiveGateActions(t, actions)
	assertStringSetEqual(t, actionIDs, eventIDs(actions), "live requires_action event ids")

	firstResults := liveGateResolutionDrafts(t, actions[:1])
	if _, err := orchestrator.Admit(ctx, session.ID, firstResults); err != nil {
		t.Fatalf("admit first live HITL result: %v", err)
	}
	assertGatePendingState(t, store, session.ID, liveGateActionCount, 1)
	if got := len(eventsOfType(mustGateEvents(t, store, session.ID), domain.EvSpanModelRequestStart)); got != 1 {
		t.Fatalf("live gate resumed before its complete barrier: model requests=%d", got)
	}

	remainingResults := liveGateResolutionDrafts(t, actions[1:])
	if _, err := orchestrator.Admit(ctx, session.ID, remainingResults); err != nil {
		t.Fatalf("admit final live HITL result: %v", err)
	}
	events := waitForGateCompletion(t, store, session.ID, 2*time.Minute)
	if got := len(eventsOfType(events, domain.EvAgentCustomToolUse)); got != liveGateActionCount {
		t.Fatalf("live custom tool uses = %d, want %d; events=%s", got, liveGateActionCount, typeList(events))
	}
	if got := len(eventsOfType(events, domain.EvUserCustomToolResult)); got != liveGateActionCount {
		t.Fatalf("live custom tool results = %d, want %d; events=%s", got, liveGateActionCount, typeList(events))
	}
	if got := len(eventsOfType(events, domain.EvSpanModelRequestStart)); got != 2 {
		t.Fatalf("live model requests = %d, want 2; events=%s", got, typeList(events))
	}
	if messages := gateAgentMessages(events); strings.TrimSpace(messages) == "" {
		t.Fatalf("live gate completed without a final model message; events=%s", typeList(events))
	}
	if pending, err := store.UnresolvedPendingActions(ctx, session.ID); err != nil || len(pending) != 0 {
		t.Fatalf("live gate pending actions = %+v, err=%v", pending, err)
	}
}

func startGateRuntime(
	t *testing.T,
	parent context.Context,
	runtime *temporalpkg.Runtime,
) func() {
	t.Helper()
	if err := runtime.Worker.Start(); err != nil {
		t.Fatalf("start HITL gate worker: %v", err)
	}
	relayCtx, cancelRelay := context.WithCancel(parent)
	relayDone := make(chan error, 1)
	go func() { relayDone <- runtime.Relay.Run(relayCtx) }()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancelRelay()
			runtime.Worker.Stop()
			select {
			case err := <-relayDone:
				if err != nil && !errors.Is(err, context.Canceled) {
					t.Errorf("stop HITL gate relay: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Error("HITL gate relay did not stop")
			}
		})
	}
}

func waitForGateBarrier(
	t *testing.T,
	store *pg.Store,
	sessionID string,
	wantActions int,
	timeout time.Duration,
) ([]domain.Event, []domain.Event, []string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events, err := store.EventsAfter(context.Background(), sessionID, 0, 200)
		if err != nil {
			t.Fatalf("list HITL gate events: %v", err)
		}
		if failure, ok := firstFailureEvent(events); ok {
			t.Fatalf("HITL gate failed with %s: %#v", failure.Type, failure.Payload)
		}
		actions := eventsOfType(events, domain.EvAgentCustomToolUse)
		if len(actions) == wantActions {
			if ids, ok := latestRequiresActionIDs(events); ok && len(ids) == wantActions {
				return events, actions, ids
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	events, _ := store.EventsAfter(context.Background(), sessionID, 0, 200)
	t.Fatalf("timed out waiting for HITL gate barrier; events=%s", typeList(events))
	return nil, nil, nil
}

func waitForGateCompletion(
	t *testing.T,
	store *pg.Store,
	sessionID string,
	timeout time.Duration,
) []domain.Event {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events, err := store.EventsAfter(context.Background(), sessionID, 0, 300)
		if err != nil {
			t.Fatalf("list completed HITL gate events: %v", err)
		}
		if failure, ok := firstFailureEvent(events); ok {
			t.Fatalf("HITL gate failed with %s: %#v", failure.Type, failure.Payload)
		}
		if hasType(events, domain.EvAgentMessage) && latestIdleReason(events) == "end_turn" {
			return events
		}
		time.Sleep(100 * time.Millisecond)
	}
	events, _ := store.EventsAfter(context.Background(), sessionID, 0, 300)
	t.Fatalf("timed out waiting for HITL gate completion; events=%s", typeList(events))
	return nil
}

func latestRequiresActionIDs(events []domain.Event) ([]string, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != domain.EvSessionStatusIdle {
			continue
		}
		stopReason, _ := events[index].Payload["stop_reason"].(map[string]any)
		if stopReason["type"] != "requires_action" {
			return nil, false
		}
		raw, _ := stopReason["event_ids"].([]any)
		ids := make([]string, 0, len(raw))
		for _, value := range raw {
			id, ok := value.(string)
			if !ok || id == "" {
				return nil, false
			}
			ids = append(ids, id)
		}
		return ids, true
	}
	return nil, false
}

func latestIdleReason(events []domain.Event) string {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != domain.EvSessionStatusIdle {
			continue
		}
		stopReason, _ := events[index].Payload["stop_reason"].(map[string]any)
		reason, _ := stopReason["type"].(string)
		return reason
	}
	return ""
}

func gateResolutionDrafts(actions []domain.Event) []domain.EventDraft {
	drafts := make([]domain.EventDraft, 0, len(actions))
	for _, action := range actions {
		input, _ := action.Payload["input"].(map[string]any)
		receiptID, _ := input["receipt_id"].(string)
		drafts = append(drafts, domain.EventDraft{
			Type: domain.EvUserCustomToolResult,
			Payload: map[string]any{
				"custom_tool_use_id": action.ID,
				"content": []any{map[string]any{
					"type": "text", "text": `{"recorded":true,"receipt_id":"` + receiptID + `"}`,
				}},
			},
		})
	}
	return drafts
}

func assertGatePendingState(
	t *testing.T,
	store *pg.Store,
	sessionID string,
	wantTotal int,
	wantClaimed int,
) {
	t.Helper()
	pending, err := store.UnresolvedPendingActions(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("list HITL pending actions: %v", err)
	}
	claimed := 0
	for _, action := range pending {
		if action.ResolvingEventID != nil {
			claimed++
		}
	}
	if len(pending) != wantTotal || claimed != wantClaimed {
		t.Fatalf(
			"pending state total=%d claimed=%d, want %d/%d: %+v",
			len(pending), claimed, wantTotal, wantClaimed, pending,
		)
	}
}

func assertGateActions(t *testing.T, actions []domain.Event) {
	t.Helper()
	receipts := make([]string, 0, len(actions))
	names := make(map[string]int)
	for _, action := range actions {
		name, _ := action.Payload["name"].(string)
		input, _ := action.Payload["input"].(map[string]any)
		receiptID, _ := input["receipt_id"].(string)
		if (name != "decide" && name != "escalate") || receiptID == "" {
			t.Fatalf("invalid HITL action: %+v", action)
		}
		names[name]++
		receipts = append(receipts, receiptID)
	}
	sort.Strings(receipts)
	wantReceipts := make([]string, 0, gateActionCount)
	for index := 1; index <= gateActionCount; index++ {
		wantReceipts = append(wantReceipts, fmt.Sprintf("r%02d", index))
	}
	if strings.Join(receipts, ",") != strings.Join(wantReceipts, ",") {
		t.Fatalf("HITL receipt ids = %v, want %v", receipts, wantReceipts)
	}
	if names["decide"] == 0 || names["escalate"] == 0 {
		t.Fatalf("HITL actions did not exercise both lanes: %v", names)
	}
}

func assertLiveGateActions(t *testing.T, actions []domain.Event) {
	t.Helper()
	want := map[string]string{"r01": "decide", "r02": "escalate"}
	seen := make(map[string]string, len(actions))
	for _, action := range actions {
		name, _ := action.Payload["name"].(string)
		input, _ := action.Payload["input"].(map[string]any)
		receiptID, _ := input["receipt_id"].(string)
		if receiptID == "" || name != want[receiptID] {
			t.Fatalf("unexpected live HITL action: %+v", action)
		}
		if _, duplicate := seen[receiptID]; duplicate {
			t.Fatalf("duplicate live HITL action for %s: %+v", receiptID, actions)
		}
		seen[receiptID] = name
	}
	if len(seen) != len(want) {
		t.Fatalf("live HITL actions = %v, want %v", seen, want)
	}
}

func liveGateResolutionDrafts(t *testing.T, actions []domain.Event) []domain.EventDraft {
	t.Helper()
	drafts := make([]domain.EventDraft, 0, len(actions))
	for _, action := range actions {
		name, _ := action.Payload["name"].(string)
		input, _ := action.Payload["input"].(map[string]any)
		receiptID, _ := input["receipt_id"].(string)
		result := map[string]any{"recorded": true, "receipt_id": receiptID}
		switch name {
		case "decide":
			result["decision"] = "approve"
		case "escalate":
			result["human_decision"] = "reject"
		default:
			t.Fatalf("cannot resolve unexpected live HITL tool %q", name)
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("encode live HITL result: %v", err)
		}
		drafts = append(drafts, domain.EventDraft{
			Type: domain.EvUserCustomToolResult,
			Payload: map[string]any{
				"custom_tool_use_id": action.ID,
				"content": []any{map[string]any{
					"type": "text", "text": string(encoded),
				}},
			},
		})
	}
	return drafts
}

func mustGateEvents(t *testing.T, store *pg.Store, sessionID string) []domain.Event {
	t.Helper()
	events, err := store.EventsAfter(context.Background(), sessionID, 0, 200)
	if err != nil {
		t.Fatalf("list HITL gate events: %v", err)
	}
	return events
}

func gateAgentMessages(events []domain.Event) string {
	messages := make([]string, 0)
	for _, event := range eventsOfType(events, domain.EvAgentMessage) {
		text, _, ok := eventText(event)
		if ok && strings.TrimSpace(text) != "" {
			messages = append(messages, strings.TrimSpace(text))
		}
	}
	return strings.Join(messages, " | ")
}

func eventIDs(events []domain.Event) []string {
	ids := make([]string, 0, len(events))
	for _, event := range events {
		ids = append(ids, event.ID)
	}
	return ids
}

func assertStringSetEqual(t *testing.T, got []string, want []string, label string) {
	t.Helper()
	got = append([]string(nil), got...)
	want = append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

func gateCustomTools() []any {
	return []any{
		map[string]any{
			"type": "custom", "name": "decide",
			"description": "Record a final approve or reject decision for a clear expense.",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"receipt_id": map[string]any{"type": "string"},
					"action":     map[string]any{"type": "string", "enum": []any{"approve", "reject"}},
					"reason":     map[string]any{"type": "string"},
				},
				"required": []any{"receipt_id", "action", "reason"},
			},
		},
		map[string]any{
			"type": "custom", "name": "escalate",
			"description": "Request a human decision for an ambiguous expense.",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"receipt_id": map[string]any{"type": "string"},
					"question":   map[string]any{"type": "string"},
				},
				"required": []any{"receipt_id", "question"},
			},
		},
	}
}

// gateProbeModel derives its response from the durable provider transcript so
// a retried model Activity returns the same seven tool calls or final answer.
type gateProbeModel struct {
	mu    sync.Mutex
	calls int
}

func (m *gateProbeModel) CreateMessage(
	_ context.Context,
	request model.Request,
) (model.Response, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	if !requestHasTool(request, "decide") || !requestHasTool(request, "escalate") {
		return model.Response{}, errors.New("HITL gate custom tools were not offered to the model")
	}
	results := make([]domain.ContentBlock, 0, gateActionCount)
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_result" {
				results = append(results, block)
			}
		}
	}
	if len(results) == 0 {
		content := make([]domain.ContentBlock, 0, gateActionCount)
		for index := 1; index <= gateActionCount; index++ {
			receiptID := fmt.Sprintf("r%02d", index)
			block := domain.ContentBlock{
				Type: "tool_use", ToolUseID: "gate_" + receiptID,
				ToolName: "decide",
				Input: map[string]any{
					"receipt_id": receiptID,
					"action":     "approve",
					"reason":     "within policy",
				},
			}
			if index > 5 {
				block.ToolName = "escalate"
				block.Input = map[string]any{
					"receipt_id": receiptID,
					"question":   "Does a reviewer approve this ambiguous expense?",
				}
			}
			content = append(content, block)
		}
		return model.Response{Content: content, StopReason: "tool_use"}, nil
	}
	if len(results) != gateActionCount {
		return model.Response{}, fmt.Errorf(
			"HITL gate resumed with %d tool results, want %d",
			len(results), gateActionCount,
		)
	}
	seen := make(map[string]struct{}, gateActionCount)
	for _, result := range results {
		if result.IsError || !strings.Contains(result.Text, `"recorded":true`) {
			return model.Response{}, fmt.Errorf("invalid HITL result: %+v", result)
		}
		if result.ToolResultFor == "" {
			return model.Response{}, errors.New("HITL result lost its provider tool-use correlation")
		}
		if _, duplicate := seen[result.ToolResultFor]; duplicate {
			return model.Response{}, errors.New("HITL result was duplicated in the provider transcript")
		}
		seen[result.ToolResultFor] = struct{}{}
	}
	return textModelResponse("All seven expense decisions were recorded exactly once."), nil
}

func (m *gateProbeModel) CreateMessageStream(
	ctx context.Context,
	request model.Request,
	onDelta func(index int, text string),
) (model.Response, error) {
	response, err := m.CreateMessage(ctx, request)
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

func (m *gateProbeModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}
