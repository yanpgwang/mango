package temporal_test

import (
	"context"
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
	temporalpkg "github.com/yanpgwang/mango/internal/temporal"
	"go.temporal.io/sdk/client"
)

const barrierActionCount = 7

// TestVerticalSlice_CustomToolBarrierSurvivesWorkerRestart exercises Mango's durable
// client-action boundary over real PostgreSQL and Temporal. One model response
// emits seven parallel custom-tool calls. Mango exposes the complete barrier,
// accepts partial results without resuming early, atomically rejects a
// duplicate, and resumes the whole result round after the execution worker is
// replaced.
func TestVerticalSlice_CustomToolBarrierSurvivesWorkerRestart(t *testing.T) {
	databaseURL := os.Getenv("MANGO_TEST_DATABASE_URL")
	temporalAddress := os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if databaseURL == "" || temporalAddress == "" {
		t.Skip("set MANGO_TEST_DATABASE_URL and MANGO_TEST_TEMPORAL_HOSTPORT to run the custom-tool barrier integration test")
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
	probe := &barrierProbeModel{}
	taskQueue := "mango-custom-tool-barrier-" + ids.NewID("")
	runtimeConfig := temporalpkg.RuntimeConfig{
		TemporalClient: temporalClient,
		Store:          store,
		ModelClient:    probe,
		IDGenerator:    ids,
		RelayConfig:    temporalpkg.RelayConfig{PollInterval: 50 * time.Millisecond},
		TaskQueue:      taskQueue,
	}

	runtimeOne := temporalpkg.NewRuntime(runtimeConfig)
	stopRuntimeOne := startIntegrationRuntime(t, ctx, runtimeOne)
	runtimeOneStopped := false
	defer func() {
		if !runtimeOneStopped {
			stopRuntimeOne()
		}
	}()

	system := "Call tool_a or tool_b once for each item and wait for every result."
	now := time.Now().UTC()
	environment := domain.Environment{
		ID: "env_custom_tool_barrier", Name: "custom-tool barrier", ConfigType: "self_hosted",
		Config: map[string]any{"type": "self_hosted"}, Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}
	if err := pg.NewEnvironmentRepository(store).Put(ctx, environment); err != nil {
		t.Fatalf("create custom-tool barrier Environment: %v", err)
	}
	session := domain.Session{
		ID:                "sesn_custom_tool_barrier_" + ids.NewID(""),
		AgentID:           "agent_custom_tool_barrier",
		AgentVersion:      1,
		EnvironmentID:     "env_custom_tool_barrier",
		EnvironmentType:   "self_hosted",
		EnvironmentConfig: map[string]any{"type": "self_hosted"},
		Status:            domain.StatusIdle,
		Metadata:          map[string]any{},
		AgentSnapshot: domain.Agent{
			ID: "agent_custom_tool_barrier", Version: 1, Name: "custom-tool-barrier",
			Model: domain.Model{ID: "barrier-probe"}, System: &system,
			Tools: barrierCustomTools(),
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
	orchestrator := runtimeOne.Orchestrator()
	if _, _, err := orchestrator.CreateSession(ctx, session, nil); err != nil {
		t.Fatalf("create custom-tool barrier Session: %v", err)
	}
	defer terminateIntegrationWorkflow(t, temporalClient, session.ID)

	if _, err := orchestrator.Admit(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{map[string]any{
			"type": "text", "text": "Process items item_01 through item_07 with one tool call for each.",
		}}},
	}}); err != nil {
		t.Fatalf("admit custom-tool barrier task: %v", err)
	}

	_, actions, actionIDs := waitForCustomToolBarrier(
		t, store, session.ID, barrierActionCount, 30*time.Second,
	)
	if got := probe.callCount(); got != 1 {
		t.Fatalf("model calls before resolution = %d, want 1", got)
	}
	assertBarrierActions(t, actions)
	assertStringSetEqual(t, actionIDs, eventIDs(actions), "requires_action event ids")

	const partialCount = 3
	partialDrafts := barrierResolutionDrafts(actions[:partialCount])
	partialEvents, err := orchestrator.Admit(ctx, session.ID, partialDrafts)
	if err != nil {
		t.Fatalf("admit partial custom-tool results: %v", err)
	}
	if len(partialEvents) != partialCount {
		t.Fatalf("partial result events = %d, want %d", len(partialEvents), partialCount)
	}
	assertBarrierPendingState(t, store, session.ID, barrierActionCount, partialCount)

	beforeDuplicate, err := store.EventsAfter(ctx, session.ID, 0, 200)
	if err != nil {
		t.Fatalf("events before duplicate: %v", err)
	}
	_, err = orchestrator.Admit(ctx, session.ID, barrierResolutionDrafts(actions[:1]))
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
	stopRuntimeTwo := startIntegrationRuntime(t, ctx, runtimeTwo)
	defer stopRuntimeTwo()

	rest := barrierResolutionDrafts(actions[partialCount:])
	finalResolutionEvents, err := runtimeTwo.Orchestrator().Admit(ctx, session.ID, rest)
	if err != nil {
		t.Fatalf("admit remaining custom-tool results after worker restart: %v", err)
	}
	if len(finalResolutionEvents) != barrierActionCount-partialCount {
		t.Fatalf(
			"remaining result events = %d, want %d",
			len(finalResolutionEvents), barrierActionCount-partialCount,
		)
	}

	events := waitForIntegrationCompletion(t, store, session.ID, 30*time.Second)
	if got := probe.callCount(); got != 2 {
		t.Fatalf("model calls after complete barrier = %d, want 2", got)
	}
	if got := len(eventsOfType(events, domain.EvAgentCustomToolUse)); got != barrierActionCount {
		t.Fatalf("custom tool uses = %d, want %d; events=%s", got, barrierActionCount, typeList(events))
	}
	if got := len(eventsOfType(events, domain.EvUserCustomToolResult)); got != barrierActionCount {
		t.Fatalf("custom tool results = %d, want %d; events=%s", got, barrierActionCount, typeList(events))
	}
	if got := len(eventsOfType(events, domain.EvSpanModelRequestStart)); got != 2 {
		t.Fatalf("model request starts = %d, want 2; events=%s", got, typeList(events))
	}
	if failures := eventsOfType(events, domain.EvSessionError); len(failures) != 0 {
		t.Fatalf("custom-tool barrier emitted Session errors: %+v", failures)
	}
	pending, err := store.UnresolvedPendingActions(ctx, session.ID)
	if err != nil {
		t.Fatalf("list final pending actions: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("resolved barrier retained pending actions: %+v", pending)
	}
	for _, resolution := range append(partialEvents, finalResolutionEvents...) {
		stored, getErr := store.GetEvent(ctx, session.ID, resolution.ID)
		if getErr != nil || stored.ProcessedAt == nil {
			t.Fatalf("resolution %s not durably processed: %+v err=%v", resolution.ID, stored, getErr)
		}
	}
	final, err := store.GetSession(ctx, session.ID)
	if err != nil || final.Status != domain.StatusIdle {
		t.Fatalf("completed custom-tool barrier Session = %+v, err=%v", final, err)
	}
}

func waitForCustomToolBarrier(
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
			t.Fatalf("list custom-tool barrier events: %v", err)
		}
		if failure, ok := firstFailureEvent(events); ok {
			t.Fatalf("custom-tool barrier failed with %s: %#v", failure.Type, failure.Payload)
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
	t.Fatalf("timed out waiting for custom-tool barrier; events=%s", typeList(events))
	return nil, nil, nil
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

func barrierResolutionDrafts(actions []domain.Event) []domain.EventDraft {
	drafts := make([]domain.EventDraft, 0, len(actions))
	for _, action := range actions {
		input, _ := action.Payload["input"].(map[string]any)
		itemID, _ := input["item_id"].(string)
		drafts = append(drafts, domain.EventDraft{
			Type: domain.EvUserCustomToolResult,
			Payload: map[string]any{
				"custom_tool_use_id": action.ID,
				"content": []any{map[string]any{
					"type": "text", "text": `{"ok":true,"item_id":"` + itemID + `"}`,
				}},
			},
		})
	}
	return drafts
}

func assertBarrierPendingState(
	t *testing.T,
	store *pg.Store,
	sessionID string,
	wantTotal int,
	wantClaimed int,
) {
	t.Helper()
	pending, err := store.UnresolvedPendingActions(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("list custom-tool pending actions: %v", err)
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

func assertBarrierActions(t *testing.T, actions []domain.Event) {
	t.Helper()
	items := make([]string, 0, len(actions))
	names := make(map[string]int)
	for _, action := range actions {
		name, _ := action.Payload["name"].(string)
		input, _ := action.Payload["input"].(map[string]any)
		itemID, _ := input["item_id"].(string)
		if (name != "tool_a" && name != "tool_b") || itemID == "" {
			t.Fatalf("invalid custom-tool action: %+v", action)
		}
		names[name]++
		items = append(items, itemID)
	}
	sort.Strings(items)
	wantItems := make([]string, 0, barrierActionCount)
	for index := 1; index <= barrierActionCount; index++ {
		wantItems = append(wantItems, fmt.Sprintf("item_%02d", index))
	}
	if strings.Join(items, ",") != strings.Join(wantItems, ",") {
		t.Fatalf("custom-tool item ids = %v, want %v", items, wantItems)
	}
	if names["tool_a"] == 0 || names["tool_b"] == 0 {
		t.Fatalf("custom-tool actions did not exercise both tool names: %v", names)
	}
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

func barrierCustomTools() []any {
	tools := make([]any, 0, 2)
	for _, name := range []string{"tool_a", "tool_b"} {
		tools = append(tools, map[string]any{
			"type": "custom", "name": name,
			"description": "Return a result for the identified test item.",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"item_id": map[string]any{"type": "string"},
				},
				"required": []any{"item_id"},
			},
		})
	}
	return tools
}

// barrierProbeModel derives its response from the durable provider transcript so
// a retried model Activity returns the same seven tool calls or final answer.
type barrierProbeModel struct {
	mu    sync.Mutex
	calls int
}

func (m *barrierProbeModel) CreateMessage(
	_ context.Context,
	request model.Request,
) (model.Response, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	if !requestHasTool(request, "tool_a") || !requestHasTool(request, "tool_b") {
		return model.Response{}, errors.New("barrier tools were not offered to the model")
	}
	results := make([]domain.ContentBlock, 0, barrierActionCount)
	for _, message := range request.Messages {
		for _, block := range message.Content {
			if block.Type == "tool_result" {
				results = append(results, block)
			}
		}
	}
	if len(results) == 0 {
		content := make([]domain.ContentBlock, 0, barrierActionCount)
		for index := 1; index <= barrierActionCount; index++ {
			itemID := fmt.Sprintf("item_%02d", index)
			block := domain.ContentBlock{
				Type: "tool_use", ToolUseID: "barrier_" + itemID,
				ToolName: "tool_a",
				Input: map[string]any{
					"item_id": itemID,
				},
			}
			if index > 5 {
				block.ToolName = "tool_b"
			}
			content = append(content, block)
		}
		return model.Response{Content: content, StopReason: "tool_use"}, nil
	}
	if len(results) != barrierActionCount {
		return model.Response{}, fmt.Errorf(
			"custom-tool barrier resumed with %d tool results, want %d",
			len(results), barrierActionCount,
		)
	}
	seen := make(map[string]struct{}, barrierActionCount)
	for _, result := range results {
		if result.IsError || !strings.Contains(result.Text, `"ok":true`) {
			return model.Response{}, fmt.Errorf("invalid custom-tool result: %+v", result)
		}
		if result.ToolResultFor == "" {
			return model.Response{}, errors.New("custom-tool result lost its provider tool-use correlation")
		}
		if _, duplicate := seen[result.ToolResultFor]; duplicate {
			return model.Response{}, errors.New("custom-tool result was duplicated in the provider transcript")
		}
		seen[result.ToolResultFor] = struct{}{}
	}
	return textModelResponse("All seven tool results were received exactly once."), nil
}

func (m *barrierProbeModel) CreateMessageStream(
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

func (m *barrierProbeModel) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}
