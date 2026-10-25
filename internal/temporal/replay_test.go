package temporal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	taskqueuepb "go.temporal.io/api/taskqueue/v1"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

func durationProto(d time.Duration) *durationpb.Duration {
	return durationpb.New(d)
}

func timestampProto(at time.Time) *timestamppb.Timestamp {
	return timestamppb.New(at)
}

// replayWorkflowName is the registered name the synthetic histories below carry.
// The turn harness is the replay subject for the ordered turn-level version
// gates inside runWorkflowTurnInternal. Registering under a stable name keeps
// the fixtures independent of Go function naming.
const replayWorkflowName = "SessionAgentTurn"

const replaySessionWorkflowName = SessionWorkflowType
const replaySessionThreadWorkflowName = SessionThreadWorkflowType

const replayTaskQueue = "replay-task-queue"

type replayVersionGate struct {
	changeID string
	version  workflow.Version
}

// turnReplayVersionGates is intentionally in Workflow command order. A rolling
// upgrade can leave history at every prefix, so the replay suite exercises each
// deployment boundary rather than only the oldest and newest shapes.
var turnReplayVersionGates = []replayVersionGate{
	{liveModelSpanStartChangeID, liveModelSpanStartVersion},
	{mcpToolEventsChangeID, mcpToolEventsVersion},
	{terminalSessionErrorChangeID, terminalSessionErrorVersion},
	{outcomeEvaluationHeartbeatChangeID, outcomeEvaluationHeartbeatVersion},
	{modelRetryLifecycleChangeID, modelRetryLifecycleVersion},
}

// historyFixture builds a Temporal Workflow history by hand so replay
// compatibility can be asserted offline, with no Temporal service and no
// recorded production history to keep in the repository. Only the structure the
// replayer needs is populated: the workflow-task envelope, one
// scheduled/started/completed triple per Activity, and any version markers the
// original execution recorded.
type historyFixture struct {
	t       *testing.T
	events  []*historypb.HistoryEvent
	eventID int64
	// lastCompletedTask is the event id of the WorkflowTaskCompleted that
	// produced the commands currently being appended.
	lastCompletedTask int64
	scheduledTaskID   int64
	startedTaskID     int64
	now               time.Time
}

func newHistoryFixture(t *testing.T, input any) *historyFixture {
	return newNamedHistoryFixture(t, replayWorkflowName, input)
}

func newNamedHistoryFixture(t *testing.T, workflowName string, input any) *historyFixture {
	t.Helper()
	h := &historyFixture{
		t:   t,
		now: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
	}
	h.append(
		enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_STARTED,
		&historypb.HistoryEvent_WorkflowExecutionStartedEventAttributes{
			WorkflowExecutionStartedEventAttributes: &historypb.WorkflowExecutionStartedEventAttributes{
				WorkflowType: &commonpb.WorkflowType{Name: workflowName},
				TaskQueue: &taskqueuepb.TaskQueue{
					Name: replayTaskQueue,
					Kind: enumspb.TASK_QUEUE_KIND_NORMAL,
				},
				Input:                    h.payloads(input),
				WorkflowRunTimeout:       durationProto(time.Hour),
				WorkflowExecutionTimeout: durationProto(time.Hour),
				WorkflowTaskTimeout:      durationProto(time.Minute),
				Attempt:                  1,
			},
		},
	)
	h.scheduleWorkflowTask()
	return h
}

func (h *historyFixture) payloads(values ...any) *commonpb.Payloads {
	h.t.Helper()
	encoded, err := converter.GetDefaultDataConverter().ToPayloads(values...)
	require.NoError(h.t, err)
	return encoded
}

func (h *historyFixture) payload(value any) *commonpb.Payload {
	h.t.Helper()
	encoded, err := converter.GetDefaultDataConverter().ToPayload(value)
	require.NoError(h.t, err)
	return encoded
}

func (h *historyFixture) append(
	eventType enumspb.EventType,
	attributes any,
) *historypb.HistoryEvent {
	h.eventID++
	h.now = h.now.Add(time.Millisecond)
	event := &historypb.HistoryEvent{
		EventId:   h.eventID,
		EventTime: timestampProto(h.now),
		EventType: eventType,
	}
	switch typed := attributes.(type) {
	case *historypb.HistoryEvent_WorkflowExecutionStartedEventAttributes:
		event.Attributes = typed
	case *historypb.HistoryEvent_WorkflowTaskScheduledEventAttributes:
		event.Attributes = typed
	case *historypb.HistoryEvent_WorkflowTaskStartedEventAttributes:
		event.Attributes = typed
	case *historypb.HistoryEvent_WorkflowTaskCompletedEventAttributes:
		event.Attributes = typed
	case *historypb.HistoryEvent_ActivityTaskScheduledEventAttributes:
		event.Attributes = typed
	case *historypb.HistoryEvent_ActivityTaskStartedEventAttributes:
		event.Attributes = typed
	case *historypb.HistoryEvent_ActivityTaskCompletedEventAttributes:
		event.Attributes = typed
	case *historypb.HistoryEvent_MarkerRecordedEventAttributes:
		event.Attributes = typed
	case *historypb.HistoryEvent_UpsertWorkflowSearchAttributesEventAttributes:
		event.Attributes = typed
	case *historypb.HistoryEvent_WorkflowExecutionCompletedEventAttributes:
		event.Attributes = typed
	case *historypb.HistoryEvent_WorkflowExecutionSignaledEventAttributes:
		event.Attributes = typed
	default:
		h.t.Fatalf("unsupported history attributes %T", attributes)
	}
	h.events = append(h.events, event)
	return event
}

func (h *historyFixture) scheduleWorkflowTask() {
	scheduled := h.append(
		enumspb.EVENT_TYPE_WORKFLOW_TASK_SCHEDULED,
		&historypb.HistoryEvent_WorkflowTaskScheduledEventAttributes{
			WorkflowTaskScheduledEventAttributes: &historypb.WorkflowTaskScheduledEventAttributes{
				TaskQueue: &taskqueuepb.TaskQueue{
					Name: replayTaskQueue,
					Kind: enumspb.TASK_QUEUE_KIND_NORMAL,
				},
				StartToCloseTimeout: durationProto(time.Minute),
				Attempt:             1,
			},
		},
	)
	started := h.append(
		enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED,
		&historypb.HistoryEvent_WorkflowTaskStartedEventAttributes{
			WorkflowTaskStartedEventAttributes: &historypb.WorkflowTaskStartedEventAttributes{
				ScheduledEventId: scheduled.EventId,
			},
		},
	)
	h.scheduledTaskID = scheduled.EventId
	h.startedTaskID = started.EventId
}

func (h *historyFixture) completeWorkflowTask() {
	completed := h.append(
		enumspb.EVENT_TYPE_WORKFLOW_TASK_COMPLETED,
		&historypb.HistoryEvent_WorkflowTaskCompletedEventAttributes{
			WorkflowTaskCompletedEventAttributes: &historypb.WorkflowTaskCompletedEventAttributes{
				ScheduledEventId: h.scheduledTaskID,
				StartedEventId:   h.startedTaskID,
			},
		},
	)
	h.lastCompletedTask = completed.EventId
}

// versionMarker records the marker pair a workflow.GetVersion call wrote in the
// original execution: the Version marker itself plus the TemporalChangeVersion
// search-attribute upsert that accompanies it. Both occupy predictable history
// event ids, which is exactly why adding a second gate has to be replay-tested:
// the SDK derives every later Activity id from those positions. A history with
// no marker for a change id replays that change as workflow.DefaultVersion.
func (h *historyFixture) versionMarker(changeID string, version workflow.Version) *historyFixture {
	h.openWorkflowTask()
	h.append(
		enumspb.EVENT_TYPE_MARKER_RECORDED,
		&historypb.HistoryEvent_MarkerRecordedEventAttributes{
			MarkerRecordedEventAttributes: &historypb.MarkerRecordedEventAttributes{
				MarkerName: "Version",
				Details: map[string]*commonpb.Payloads{
					"change-id": h.payloads(changeID),
					"version":   h.payloads(version),
				},
				WorkflowTaskCompletedEventId: h.lastCompletedTask,
			},
		},
	)
	h.append(
		enumspb.EVENT_TYPE_UPSERT_WORKFLOW_SEARCH_ATTRIBUTES,
		&historypb.HistoryEvent_UpsertWorkflowSearchAttributesEventAttributes{
			UpsertWorkflowSearchAttributesEventAttributes: &historypb.UpsertWorkflowSearchAttributesEventAttributes{
				SearchAttributes: &commonpb.SearchAttributes{
					IndexedFields: map[string]*commonpb.Payload{
						"TemporalChangeVersion": h.payload(
							[]string{changeID + "-" + itoaTest(int64(version))},
						),
					},
				},
				WorkflowTaskCompletedEventId: h.lastCompletedTask,
			},
		},
	)
	return h
}

// openWorkflowTask closes the pending workflow task so the events that follow
// are the commands it produced.
func (h *historyFixture) openWorkflowTask() {
	if len(h.events) > 0 && h.events[len(h.events)-1].EventType ==
		enumspb.EVENT_TYPE_WORKFLOW_TASK_STARTED {
		h.completeWorkflowTask()
	}
}

// activity records one completed Activity: the schedule command the Workflow
// issued and the result the worker reported. The Go SDK derives an Activity id
// from the event id its schedule command will occupy, so the fixture uses the
// same rule.
func (h *historyFixture) activity(activityType string, result any) *historyFixture {
	h.openWorkflowTask()
	activityID := itoaTest(h.eventID + 1)
	scheduled := h.append(
		enumspb.EVENT_TYPE_ACTIVITY_TASK_SCHEDULED,
		&historypb.HistoryEvent_ActivityTaskScheduledEventAttributes{
			ActivityTaskScheduledEventAttributes: &historypb.ActivityTaskScheduledEventAttributes{
				ActivityId:   activityID,
				ActivityType: &commonpb.ActivityType{Name: activityType},
				TaskQueue: &taskqueuepb.TaskQueue{
					Name: replayTaskQueue,
					Kind: enumspb.TASK_QUEUE_KIND_NORMAL,
				},
				ScheduleToCloseTimeout:       durationProto(time.Minute),
				ScheduleToStartTimeout:       durationProto(time.Minute),
				StartToCloseTimeout:          durationProto(time.Minute),
				WorkflowTaskCompletedEventId: h.lastCompletedTask,
			},
		},
	)
	started := h.append(
		enumspb.EVENT_TYPE_ACTIVITY_TASK_STARTED,
		&historypb.HistoryEvent_ActivityTaskStartedEventAttributes{
			ActivityTaskStartedEventAttributes: &historypb.ActivityTaskStartedEventAttributes{
				ScheduledEventId: scheduled.EventId,
				Attempt:          1,
			},
		},
	)
	var payload *commonpb.Payloads
	if result != nil {
		payload = h.payloads(result)
	} else {
		payload = h.payloads()
	}
	h.append(
		enumspb.EVENT_TYPE_ACTIVITY_TASK_COMPLETED,
		&historypb.HistoryEvent_ActivityTaskCompletedEventAttributes{
			ActivityTaskCompletedEventAttributes: &historypb.ActivityTaskCompletedEventAttributes{
				Result:           payload,
				ScheduledEventId: scheduled.EventId,
				StartedEventId:   started.EventId,
			},
		},
	)
	h.scheduleWorkflowTask()
	return h
}

func (h *historyFixture) signal(name string, value any) *historyFixture {
	h.openWorkflowTask()
	h.append(
		enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED,
		&historypb.HistoryEvent_WorkflowExecutionSignaledEventAttributes{
			WorkflowExecutionSignaledEventAttributes: &historypb.WorkflowExecutionSignaledEventAttributes{
				SignalName: name,
				Input:      h.payloads(value),
			},
		},
	)
	h.scheduleWorkflowTask()
	return h
}

func (h *historyFixture) partial() *historypb.History {
	h.openWorkflowTask()
	return &historypb.History{Events: h.events}
}

func (h *historyFixture) finish(result any) *historypb.History {
	h.openWorkflowTask()
	h.append(
		enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_COMPLETED,
		&historypb.HistoryEvent_WorkflowExecutionCompletedEventAttributes{
			WorkflowExecutionCompletedEventAttributes: &historypb.WorkflowExecutionCompletedEventAttributes{
				Result:                       h.payloads(result),
				WorkflowTaskCompletedEventId: h.lastCompletedTask,
			},
		},
	)
	return &historypb.History{Events: h.events}
}

func replayTurnHistory(t *testing.T, history *historypb.History) error {
	t.Helper()
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflowWithOptions(
		workflowTurnHarness,
		workflow.RegisterOptions{Name: replayWorkflowName},
	)
	return replayer.ReplayWorkflowHistory(nil, history)
}

func replaySessionHistory(t *testing.T, history *historypb.History) error {
	t.Helper()
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflowWithOptions(
		SessionWorkflow,
		workflow.RegisterOptions{Name: replaySessionWorkflowName},
	)
	return replayer.ReplayWorkflowHistory(nil, history)
}

func replaySessionThreadHistory(t *testing.T, history *historypb.History) error {
	t.Helper()
	replayer := worker.NewWorkflowReplayer()
	replayer.RegisterWorkflowWithOptions(
		SessionThreadWorkflow,
		workflow.RegisterOptions{Name: replaySessionThreadWorkflowName},
	)
	return replayer.ReplayWorkflowHistory(nil, history)
}

// mcpToolRoundPrepared is the PrepareTurn result an MCP turn recorded in every
// fixture below: one always_allow remote tool pinned for the session.
func mcpToolRoundPrepared() PrepareTurnResult {
	return PrepareTurnResult{
		AttemptID: "ratm_replay",
		Request: model.Request{
			Model:    "test-model",
			Messages: []domain.Message{{Role: domain.RoleUser}},
			Tools:    []model.ToolSchema{{Name: "mcp__github__list_issues"}},
		},
		Tools: []TurnTool{mcpTurnTool("always_allow")},
	}
}

func mcpToolRoundCall() CallModelResult {
	return CallModelResult{
		ToolSteps: []PlannedToolStep{{
			ToolUseEventID: "sevt_mcp_use", ToolStepID: "tstep_mcp",
		}},
		Response: model.Response{
			StopReason: "tool_use",
			Content: []domain.ContentBlock{{
				Type:      "tool_use",
				ToolUseID: "sevt_mcp_use",
				ToolName:  "mcp__github__list_issues",
				Input:     map[string]any{"repo": "mango"},
			}},
		},
	}
}

func mcpToolRoundFinalCall() CallModelResult {
	return CallModelResult{
		MessageEventID: "sevt_answer",
		Response: model.Response{
			StopReason: "end_turn",
			Content:    []domain.ContentBlock{{Type: "text", Text: "two issues"}},
		},
	}
}

func mcpToolRoundExecuted() ExecuteToolResult {
	return ExecuteToolResult{Result: domain.ToolStepResult{
		Content: []any{map[string]any{"type": "text", "text": "#1, #2"}},
	}}
}

// A Workflow started before any of this package's version gates existed must
// still replay against the current code. The mcp-tool-event-types gate has no
// marker in this history, so workflow.GetVersion returns DefaultVersion and the
// turn keeps the legacy agent.tool_use / agent.tool_result naming without
// issuing an extra command.
func TestReplay_MCPToolTurnRecordedBeforeAnyVersionGate(t *testing.T) {
	input := PrepareTurnInput{
		SessionID: "sess_replay", TriggerEventID: "sevt_trigger",
	}
	history := newHistoryFixture(t, input).
		activity(ActivityPrepareTurn, mcpToolRoundPrepared()).
		activity(ActivityCallModel, mcpToolRoundCall()).
		activity(ActivityExecuteTool, mcpToolRoundExecuted()).
		activity(ActivityCallModel, mcpToolRoundFinalCall()).
		activity(ActivityCompleteWorkflowTurn, RunTurnResult{
			Disposition: TurnCompleted,
		}).
		finish(RunTurnResult{Disposition: TurnCompleted})

	require.NoError(t, replayTurnHistory(t, history))
}

// The realistic in-flight case: a Workflow that already recorded the
// live-model-request-span-start marker but predates mcp-tool-event-types. The
// two gates are evaluated in a fixed order, so the older marker must still be
// found and the newer one must resolve to DefaultVersion without perturbing the
// recorded command stream.
func TestReplay_MCPToolTurnRecordedBeforeMCPEventTypesGate(t *testing.T) {
	input := PrepareTurnInput{
		SessionID: "sess_replay_span", TriggerEventID: "sevt_trigger",
	}
	history := newHistoryFixture(t, input).
		activity(ActivityPrepareTurn, mcpToolRoundPrepared()).
		versionMarker(liveModelSpanStartChangeID, liveModelSpanStartVersion).
		activity(ActivityStartModelRequest, nil).
		activity(ActivityCallModel, mcpToolRoundCall()).
		activity(ActivityExecuteTool, mcpToolRoundExecuted()).
		activity(ActivityAppendWorkflowEvents, nil).
		activity(ActivityStartModelRequest, nil).
		activity(ActivityCallModel, mcpToolRoundFinalCall()).
		activity(ActivityCompleteWorkflowTurn, RunTurnResult{
			Disposition: TurnCompleted,
		}).
		finish(RunTurnResult{Disposition: TurnCompleted})

	require.NoError(t, replayTurnHistory(t, history))
}

// Each prefix represents a real rolling-upgrade boundary: the execution was
// created after that prefix shipped but before the next gate existed. All of
// them must replay on the current worker, including the full current marker set.
func TestReplay_MCPToolTurnAcrossVersionGatePrefixes(t *testing.T) {
	for prefix := 2; prefix <= len(turnReplayVersionGates); prefix++ {
		lastGate := turnReplayVersionGates[prefix-1]
		t.Run(lastGate.changeID, func(t *testing.T) {
			input := PrepareTurnInput{
				SessionID:      "sess_replay_prefix_" + itoaTest(int64(prefix)),
				TriggerEventID: "sevt_trigger",
			}
			fixture := newHistoryFixture(t, input).
				activity(ActivityPrepareTurn, mcpToolRoundPrepared())
			for _, gate := range turnReplayVersionGates[:prefix] {
				fixture.versionMarker(gate.changeID, gate.version)
			}
			history := fixture.
				activity(ActivityStartModelRequest, nil).
				activity(ActivityCallModel, mcpToolRoundCall()).
				activity(ActivityExecuteTool, mcpToolRoundExecuted()).
				activity(ActivityAppendWorkflowEvents, nil).
				activity(ActivityStartModelRequest, nil).
				activity(ActivityCallModel, mcpToolRoundFinalCall()).
				activity(ActivityCompleteWorkflowTurn, RunTurnResult{
					Disposition: TurnCompleted,
				}).
				finish(RunTurnResult{Disposition: TurnCompleted})

			require.NoError(t, replayTurnHistory(t, history))
		})
	}
}

// SessionWorkflow has its own durable-interrupt gate outside the turn harness.
// Both a history recorded before that gate and one carrying its current marker
// must resume draining the PostgreSQL ledger and then return to its signal wait.
func TestReplay_SessionWorkflowAcrossInterruptGate(t *testing.T) {
	for _, test := range []struct {
		name          string
		currentMarker bool
	}{
		{name: "before durable interrupts"},
		{name: "current durable interrupts", currentMarker: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := SessionWorkflowInput{SessionID: "sess_replay_session"}
			fixture := newNamedHistoryFixture(t, replaySessionWorkflowName, input)
			if test.currentMarker {
				fixture.versionMarker(durableInterruptChangeID, durableInterruptVersion)
			}
			history := fixture.
				signal(WakeupSignalName, WakeupSignal{MaxEventSeq: 1}).
				activity(ActivityLoadPendingActions, LoadPendingActionsResult{}).
				activity(ActivityLoadEvents, LoadEventsResult{}).
				partial()

			require.NoError(t, replaySessionHistory(t, history))
		})
	}
}

// Child Workflows that parked before the message-drain boundary shipped
// recorded one more LoadPendingActions command after turn completion. The new
// version stops immediately. Both histories must replay across a rolling
// worker upgrade without changing their recorded command streams.
func TestReplay_SessionThreadWorkflowAcrossParkDrainBoundary(t *testing.T) {
	for _, test := range []struct {
		name          string
		currentMarker bool
	}{
		{name: "before parked message drain boundary"},
		{name: "current parked message drain boundary", currentMarker: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const (
				sessionID = "sesn_replay_child"
				threadID  = "sthr_replay_child"
				messageID = "sevt_replay_child_message"
			)
			input := SessionThreadWorkflowInput{
				SessionID: sessionID, ThreadID: threadID,
			}
			prepared := mcpToolRoundPrepared()
			prepared.ThreadID = threadID
			prepared.IsChild = true
			prepared.Tools = []TurnTool{mcpTurnTool("always_ask")}
			fixture := newNamedHistoryFixture(
				t, replaySessionThreadWorkflowName, input,
			).versionMarker(
				childPendingActionRoutingChangeID,
				childPendingActionRoutingVersion,
			)
			if test.currentMarker {
				fixture.versionMarker(
					childParkedDrainBoundaryChangeID,
					childParkedDrainBoundaryVersion,
				)
			}
			fixture.signal(
				WakeupSignalName, WakeupSignal{MaxEventSeq: 1},
			).activity(
				ActivityLoadPendingActions, LoadPendingActionsResult{},
			).activity(
				ActivityLoadEvents, LoadEventsResult{Events: []EventRef{{
					ID: messageID, Seq: 1,
					Type: domain.EvAgentThreadMessageReceived,
				}}},
			).activity(
				ActivityPrepareTurn, prepared,
			)
			for _, gate := range turnReplayVersionGates {
				fixture.versionMarker(gate.changeID, gate.version)
			}
			fixture.activity(
				ActivityStartModelRequest, nil,
			).activity(
				ActivityCallModel, mcpToolRoundCall(),
			).activity(
				ActivityCompleteWorkflowTurn,
				RunTurnResult{Disposition: TurnParked},
			)
			if !test.currentMarker {
				fixture.activity(
					ActivityLoadPendingActions,
					LoadPendingActionsResult{Actions: []PendingActionRef{{
						ActionEventID:  "sevt_mcp_use",
						ActionEventSeq: 2,
						Kind:           domain.PendingToolConfirmation,
					}}},
				)
			}

			require.NoError(
				t,
				replaySessionThreadHistory(t, fixture.partial()),
			)
		})
	}
}

// An always_ask MCP call recorded before this change parked the run on an
// agent.tool_use. Replaying that history must keep the same barrier shape: the
// gate is closed, so the resumed naming stays legacy and the recorded command
// stream is reproduced exactly.
func TestReplay_AlwaysAskMCPParkRecordedBeforeMCPEventTypesGate(t *testing.T) {
	prepared := mcpToolRoundPrepared()
	prepared.Tools = []TurnTool{mcpTurnTool("always_ask")}
	input := PrepareTurnInput{
		SessionID: "sess_replay_park", TriggerEventID: "sevt_trigger",
	}
	history := newHistoryFixture(t, input).
		activity(ActivityPrepareTurn, prepared).
		versionMarker(liveModelSpanStartChangeID, liveModelSpanStartVersion).
		activity(ActivityStartModelRequest, nil).
		activity(ActivityCallModel, mcpToolRoundCall()).
		activity(ActivityCompleteWorkflowTurn, RunTurnResult{
			Disposition: TurnCompleted,
		}).
		finish(RunTurnResult{Disposition: TurnCompleted})

	require.NoError(t, replayTurnHistory(t, history))
}

// The cross-execution case. SessionWorkflow continues-as-new, so a barrier
// parked by an earlier execution is resumed by a fresh history that records the
// mcp-tool-event-types marker at its current version. The resume must still
// replay deterministically while answering the legacy agent.tool_use park it was
// handed, which is exactly why the result variant comes from
// ResumeAction.ActionEventType and not from the gate.
func TestReplay_LegacyParkResumedByExecutionWithMCPEventTypesGate(t *testing.T) {
	prepared := mcpToolRoundPrepared()
	prepared.Tools = []TurnTool{mcpTurnTool("always_ask")}
	prepared.Request.Messages = nil
	prepared.ResumeActions = []ResumeAction{{
		ActionEventID:       "sevt_legacy_park",
		ActionEventType:     domain.EvAgentToolUse,
		Kind:                domain.PendingToolConfirmation,
		EvaluatedPermission: "ask",
		ToolName:            "mcp__github__list_issues",
		Input:               map[string]any{"repo": "mango"},
		ResolutionEventID:   "sevt_confirmation",
		Confirmation:        "allow",
		ToolStepID:          "tstep_legacy_resume",
	}}
	input := PrepareTurnInput{
		SessionID:          "sess_replay_legacy_resume",
		TriggerEventID:     "sevt_confirmation",
		ResolutionEventIDs: []string{"sevt_confirmation"},
	}
	history := newHistoryFixture(t, input).
		activity(ActivityPrepareTurn, prepared).
		versionMarker(liveModelSpanStartChangeID, liveModelSpanStartVersion).
		versionMarker(mcpToolEventsChangeID, mcpToolEventsVersion).
		activity(ActivityExecuteTool, mcpToolRoundExecuted()).
		activity(ActivityAppendWorkflowEvents, nil).
		activity(ActivityStartModelRequest, nil).
		activity(ActivityCallModel, mcpToolRoundFinalCall()).
		activity(ActivityCompleteWorkflowTurn, RunTurnResult{
			Disposition: TurnCompleted,
		}).
		finish(RunTurnResult{Disposition: TurnCompleted})

	require.NoError(t, replayTurnHistory(t, history))
}

// A history whose Activity sequence no longer matches the Workflow code must be
// rejected. Without this the fixtures above could pass vacuously.
func TestReplay_DetectsNonDeterministicHistory(t *testing.T) {
	input := PrepareTurnInput{
		SessionID: "sess_replay_bad", TriggerEventID: "sevt_trigger",
	}
	history := newHistoryFixture(t, input).
		activity(ActivityPrepareTurn, mcpToolRoundPrepared()).
		// The recorded turn called the model twice with no tool execution in
		// between, which the current code cannot produce for this response.
		activity(ActivityCallModel, mcpToolRoundCall()).
		activity(ActivityCallModel, mcpToolRoundFinalCall()).
		activity(ActivityCompleteWorkflowTurn, RunTurnResult{
			Disposition: TurnCompleted,
		}).
		finish(RunTurnResult{Disposition: TurnCompleted})

	require.Error(t, replayTurnHistory(t, history))
}
