package pg

import (
	"context"
	"errors"
	"testing"

	"github.com/yanpgwang/mango/internal/agentruntime"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

type multiagentInterruptFixture struct {
	store        *Store
	ctx          context.Context
	session      domain.Session
	primary      domain.SessionThread
	child        domain.SessionThread
	childTrigger domain.Event
}

func newMultiagentInterruptFixture(t *testing.T, suffix string) multiagentInterruptFixture {
	t.Helper()
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_interrupt_" + suffix)
	session.AgentSnapshot = domain.Agent{
		ID: session.AgentID, Version: session.AgentVersion, Name: "coordinator",
		Model: domain.NormalizeModel(domain.Model{ID: "claude-test"}),
		Multiagent: &domain.Multiagent{
			Type: "coordinator",
			Agents: []domain.AgentReference{{
				Type: "agent", ID: "agent_interrupt_reviewer", Version: 1,
			}},
		},
	}
	session.MultiagentRoster = []domain.Agent{{
		ID: "agent_interrupt_reviewer", Version: 1, Name: "reviewer",
		Model: domain.NormalizeModel(domain.Model{ID: "claude-test"}),
	}}
	created, err := store.CreateSession(ctx, session, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{map[string]any{
			"type": "text", "text": "delegate",
		}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var primaryTrigger domain.Event
	for _, event := range created.Events {
		if event.Type == domain.EvUserMessage {
			primaryTrigger = event
		}
	}
	threads, err := store.ListSessionThreads(
		ctx, session.ID, app.SessionThreadListQuery{Limit: 10},
	)
	if err != nil || len(threads) != 1 {
		t.Fatalf("primary Threads = %+v, err=%v", threads, err)
	}
	primary := threads[0]
	attemptID := "ratm_interrupt_delegate_" + suffix
	stepID := "tstep_interrupt_delegate_" + suffix
	if _, err := store.EnsureAttempt(ctx, session.ID, primaryTrigger.ID, attemptID); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{
		"agent_name": "reviewer", "message": "review the change",
	}
	if _, err := store.EnsureToolStep(
		ctx, attemptID, stepID, 0, "sevt_interrupt_delegate_"+suffix,
		agentruntime.SendToAgentToolName, input,
	); err != nil {
		t.Fatal(err)
	}
	delegated, err := store.ExecuteCoordinatorToolStep(
		ctx, session.ID, primary.ID, primaryTrigger.ID, stepID,
		agentruntime.SendToAgentToolName, input,
	)
	if err != nil || delegated.Result.IsError || delegated.WakeThreadID == "" {
		t.Fatalf("delegate = %+v, err=%v", delegated, err)
	}
	child, err := store.GetSessionThread(ctx, session.ID, delegated.WakeThreadID)
	if err != nil {
		t.Fatal(err)
	}
	childEvents, err := store.ThreadEventsAfter(ctx, session.ID, child.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var childTrigger domain.Event
	for _, event := range childEvents {
		if event.Type == domain.EvAgentThreadMessageReceived {
			childTrigger = event
			break
		}
	}
	if childTrigger.ID == "" {
		t.Fatalf("child events contain no trigger: %+v", childEvents)
	}
	if _, err := store.CompleteWorkflowTurn(
		ctx, session.ID, primaryTrigger.ID,
		[]domain.EventDraft{{
			Type: domain.EvSessionStatusIdle,
			Payload: map[string]any{
				"stop_reason": map[string]any{"type": "end_turn"},
			},
		}},
		domain.StatusIdle, attemptID, domain.RunAttemptCompleted,
		nil, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	return multiagentInterruptFixture{
		store: store, ctx: ctx, session: session,
		primary: primary, child: child, childTrigger: childTrigger,
	}
}

func TestAdmitEvents_TargetedInterruptRoutesOnlyNamedThread(t *testing.T) {
	fixture := newMultiagentInterruptFixture(t, "targeted")
	admission, err := fixture.store.AdmitEvents(
		fixture.ctx,
		fixture.session.ID,
		[]domain.EventDraft{{
			Type:    domain.EvUserInterrupt,
			Payload: map[string]any{"session_thread_id": fixture.child.ID},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !admission.Enqueued || admission.PrimaryEnqueued ||
		len(admission.WakeThreadIDs) != 1 ||
		admission.WakeThreadIDs[0] != fixture.child.ID {
		t.Fatalf("targeted wake routing = %+v", admission)
	}
	if len(admission.SubmittedEvents) != 1 ||
		admission.SubmittedEvents[0].ThreadID != fixture.child.ID ||
		admission.SubmittedEvents[0].Payload["session_thread_id"] != fixture.child.ID {
		t.Fatalf("submitted targeted event = %+v", admission.SubmittedEvents)
	}
	childInterrupt, err := fixture.store.FirstUnprocessedThreadInterruptAfter(
		fixture.ctx, fixture.session.ID, fixture.child.ID, fixture.childTrigger.Sequence,
	)
	if err != nil || childInterrupt == nil ||
		childInterrupt.ID != admission.SubmittedEvents[0].ID {
		t.Fatalf("child interrupt = %+v, err=%v", childInterrupt, err)
	}
	primaryInterrupt, err := fixture.store.FirstUnprocessedInterruptAfter(
		fixture.ctx, fixture.session.ID, 0,
	)
	if err != nil || primaryInterrupt != nil {
		t.Fatalf("primary interrupt = %+v, err=%v", primaryInterrupt, err)
	}
}

func TestAdmitEvents_TargetedPrimaryInterruptDoesNotWakeChildren(t *testing.T) {
	fixture := newMultiagentInterruptFixture(t, "targeted_primary")
	admission, err := fixture.store.AdmitEvents(
		fixture.ctx,
		fixture.session.ID,
		[]domain.EventDraft{{
			Type: domain.EvUserInterrupt,
			Payload: map[string]any{
				"session_thread_id": fixture.primary.ID,
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !admission.Enqueued || !admission.PrimaryEnqueued ||
		len(admission.WakeThreadIDs) != 0 ||
		len(admission.SubmittedEvents) != 1 ||
		admission.SubmittedEvents[0].ThreadID != fixture.primary.ID {
		t.Fatalf("targeted primary routing = %+v", admission)
	}
	childInterrupt, err := fixture.store.FirstUnprocessedThreadInterruptAfter(
		fixture.ctx, fixture.session.ID, fixture.child.ID, fixture.childTrigger.Sequence,
	)
	if err != nil || childInterrupt != nil {
		t.Fatalf("unexpected child interrupt = %+v, err=%v", childInterrupt, err)
	}
}

func TestAdmitEvents_GlobalInterruptFansOutToEveryActiveThread(t *testing.T) {
	fixture := newMultiagentInterruptFixture(t, "global")
	admission, err := fixture.store.AdmitEvents(
		fixture.ctx,
		fixture.session.ID,
		[]domain.EventDraft{{Type: domain.EvUserInterrupt, Payload: map[string]any{}}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !admission.PrimaryEnqueued || len(admission.WakeThreadIDs) != 1 ||
		admission.WakeThreadIDs[0] != fixture.child.ID {
		t.Fatalf("global wake routing = %+v", admission)
	}
	if len(admission.SubmittedEvents) != 1 || len(admission.Events) != 2 {
		t.Fatalf("global fan-out events = %+v", admission.Events)
	}
	primaryInterrupt, err := fixture.store.FirstUnprocessedInterruptAfter(
		fixture.ctx, fixture.session.ID, 0,
	)
	if err != nil || primaryInterrupt == nil {
		t.Fatalf("primary interrupt = %+v, err=%v", primaryInterrupt, err)
	}
	childInterrupt, err := fixture.store.FirstUnprocessedThreadInterruptAfter(
		fixture.ctx, fixture.session.ID, fixture.child.ID, fixture.childTrigger.Sequence,
	)
	if err != nil || childInterrupt == nil {
		t.Fatalf("child interrupt = %+v, err=%v", childInterrupt, err)
	}
	if primaryInterrupt.ID == childInterrupt.ID ||
		primaryInterrupt.ThreadID != fixture.primary.ID ||
		childInterrupt.ThreadID != fixture.child.ID {
		t.Fatalf("global interrupt copies = primary=%+v child=%+v", primaryInterrupt, childInterrupt)
	}
}

func TestAdmitEvents_GlobalInterruptPreservesBatchCausality(t *testing.T) {
	fixture := newMultiagentInterruptFixture(t, "global_batch")
	admission, err := fixture.store.AdmitEvents(
		fixture.ctx,
		fixture.session.ID,
		[]domain.EventDraft{
			{Type: domain.EvUserInterrupt, Payload: map[string]any{}},
			{Type: domain.EvUserMessage, Payload: map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "redirect"}},
			}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(admission.SubmittedEvents) != 2 || len(admission.Events) < 3 {
		t.Fatalf("batch admission = %+v", admission)
	}
	primaryInterrupt := admission.SubmittedEvents[0]
	redirect := admission.SubmittedEvents[1]
	childInterrupt := admission.Events[1]
	if primaryInterrupt.Type != domain.EvUserInterrupt ||
		childInterrupt.Type != domain.EvUserInterrupt ||
		childInterrupt.ThreadID != fixture.child.ID ||
		redirect.Type != domain.EvUserMessage ||
		primaryInterrupt.Sequence >= childInterrupt.Sequence ||
		childInterrupt.Sequence >= redirect.Sequence {
		t.Fatalf(
			"global batch order = primary=%+v child=%+v redirect=%+v",
			primaryInterrupt, childInterrupt, redirect,
		)
	}
}

func TestAdmitEvents_TargetedInterruptRejectsUnknownThread(t *testing.T) {
	fixture := newMultiagentInterruptFixture(t, "unknown_target")
	_, err := fixture.store.AdmitEvents(
		fixture.ctx,
		fixture.session.ID,
		[]domain.EventDraft{{
			Type: domain.EvUserInterrupt,
			Payload: map[string]any{
				"session_thread_id": "sthr_not_in_this_session",
			},
		}},
	)
	var domainErr *domain.DomainError
	if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindValidation {
		t.Fatalf("unknown target error = %v", err)
	}
}

func TestAdmitEvents_TargetedInterruptRejectsTerminatedThread(t *testing.T) {
	fixture := newMultiagentInterruptFixture(t, "terminated_target")
	if _, err := fixture.store.CompleteThreadWorkflowTurn(
		fixture.ctx, fixture.session.ID, fixture.child.ID, fixture.childTrigger.ID,
		[]domain.EventDraft{
			{Type: domain.EvSessionError, Payload: map[string]any{
				"error": map[string]any{
					"type": "model_request_failed_error", "message": "terminal failure",
					"retry_status": map[string]any{"type": "terminal"},
				},
			}},
			{Type: domain.EvSessionStatusTerminated, Payload: map[string]any{}},
		},
		domain.StatusTerminated, "", "", nil, nil, nil, nil, nil,
		domain.TokenUsage{},
	); err != nil {
		t.Fatal(err)
	}
	_, err := fixture.store.AdmitEvents(
		fixture.ctx,
		fixture.session.ID,
		[]domain.EventDraft{{
			Type: domain.EvUserInterrupt,
			Payload: map[string]any{
				"session_thread_id": fixture.child.ID,
			},
		}},
	)
	var domainErr *domain.DomainError
	if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindConflict {
		t.Fatalf("terminated target error = %v", err)
	}
}

func TestCompleteThreadWorkflowTurn_IdleInterruptPreservesPendingBarrier(t *testing.T) {
	fixture := newMultiagentInterruptFixture(t, "pending_barrier")
	const actionID = "sevt_child_pending_interrupt_action"
	if _, err := fixture.store.CompleteThreadWorkflowTurn(
		fixture.ctx, fixture.session.ID, fixture.child.ID, fixture.childTrigger.ID,
		[]domain.EventDraft{
			{ID: actionID, Type: domain.EvAgentCustomToolUse, Payload: map[string]any{
				"name": "review_result", "input": map[string]any{},
			}},
			{Type: domain.EvSessionStatusIdle, Payload: map[string]any{
				"stop_reason": map[string]any{
					"type": "requires_action", "event_ids": []string{actionID},
				},
			}},
		},
		domain.StatusIdle, "", "", nil, []string{actionID}, nil, nil, nil,
		domain.TokenUsage{},
	); err != nil {
		t.Fatal(err)
	}
	admission, err := fixture.store.AdmitEvents(
		fixture.ctx,
		fixture.session.ID,
		[]domain.EventDraft{{
			Type: domain.EvUserInterrupt,
			Payload: map[string]any{
				"session_thread_id": fixture.child.ID,
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	ack, err := fixture.store.CompleteThreadWorkflowTurn(
		fixture.ctx, fixture.session.ID, fixture.child.ID,
		admission.SubmittedEvents[0].ID, nil,
		domain.StatusIdle, "", "", nil, nil, nil, nil, nil, domain.TokenUsage{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(ack.Events) != 0 {
		t.Fatalf("parked interrupt emitted projection events: %+v", ack.Events)
	}
	pending, err := fixture.store.UnresolvedThreadPendingActions(
		fixture.ctx, fixture.session.ID, fixture.child.ID,
	)
	if err != nil || len(pending) != 1 || pending[0].ActionEventID != actionID ||
		pending[0].ResolvingEventID != nil || pending[0].ResolvedAt != nil {
		t.Fatalf("pending barrier after interrupt = %+v, err=%v", pending, err)
	}
	interrupt, err := fixture.store.GetEvent(
		fixture.ctx, fixture.session.ID, admission.SubmittedEvents[0].ID,
	)
	if err != nil || interrupt.ProcessedAt == nil {
		t.Fatalf("parked interrupt = %+v, err=%v", interrupt, err)
	}
}

func TestCompleteThreadWorkflowTurn_InterruptWinsAndReopensQueuedFollowup(t *testing.T) {
	fixture := newMultiagentInterruptFixture(t, "interrupt_wins")
	queued, err := fixture.store.AppendThreadEvents(
		fixture.ctx, fixture.session.ID, fixture.child.ID,
		[]domain.EventDraft{{
			Type: domain.EvAgentThreadMessageReceived,
			Payload: map[string]any{
				"from_session_thread_id": fixture.primary.ID,
				"from_agent_name":        "coordinator",
				"content": []any{map[string]any{
					"type": "text", "text": "follow up",
				}},
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	attemptID := "ratm_child_interrupt_wins"
	if _, err := fixture.store.EnsureAttempt(
		fixture.ctx, fixture.session.ID, fixture.childTrigger.ID, attemptID,
	); err != nil {
		t.Fatal(err)
	}
	interruptAdmission, err := fixture.store.AdmitEvents(
		fixture.ctx, fixture.session.ID,
		[]domain.EventDraft{{
			Type:    domain.EvUserInterrupt,
			Payload: map[string]any{"session_thread_id": fixture.child.ID},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	completion, err := fixture.store.CompleteThreadWorkflowTurn(
		fixture.ctx, fixture.session.ID, fixture.child.ID, fixture.childTrigger.ID,
		[]domain.EventDraft{
			{Type: domain.EvAgentMessage, Payload: map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "partial"}},
			}},
			{Type: domain.EvSessionStatusIdle, Payload: map[string]any{
				"stop_reason": map[string]any{"type": "end_turn"},
			}},
		},
		domain.StatusIdle, attemptID, domain.RunAttemptCompleted,
		nil, nil, nil, nil, nil, domain.TokenUsage{},
	)
	if err != nil {
		t.Fatal(err)
	}
	child, err := fixture.store.GetSessionThread(
		fixture.ctx, fixture.session.ID, fixture.child.ID,
	)
	if err != nil || child.Status != domain.StatusRunning {
		t.Fatalf("child after interrupt redirect = %+v, err=%v", child, err)
	}
	interrupt, err := fixture.store.GetEvent(
		fixture.ctx, fixture.session.ID, interruptAdmission.SubmittedEvents[0].ID,
	)
	if err != nil || interrupt.ProcessedAt == nil {
		t.Fatalf("processed interrupt = %+v, err=%v", interrupt, err)
	}
	queuedEvent, err := fixture.store.GetEvent(
		fixture.ctx, fixture.session.ID, queued[0].ID,
	)
	if err != nil || queuedEvent.ProcessedAt != nil {
		t.Fatalf("queued follow-up = %+v, err=%v", queuedEvent, err)
	}
	var childIdle, childRunning, coordinatorReport bool
	for _, event := range completion.Events {
		if event.ThreadID == fixture.child.ID {
			childIdle = childIdle || event.Type == domain.EvSessionThreadStatusIdle
			childRunning = childRunning || event.Type == domain.EvSessionThreadStatusRunning
		}
		if event.ThreadID == fixture.primary.ID &&
			event.Type == domain.EvAgentThreadMessageReceived {
			coordinatorReport = true
		}
	}
	if !childIdle || !childRunning || coordinatorReport {
		t.Fatalf(
			"interrupt lifecycle idle=%v running=%v report=%v events=%v",
			childIdle, childRunning, coordinatorReport, eventTypes(completion.Events),
		)
	}
	var attemptState string
	if err := fixture.store.pool.QueryRow(
		fixture.ctx, `SELECT state FROM turn_attempts WHERE id = $1`, attemptID,
	).Scan(&attemptState); err != nil ||
		attemptState != string(domain.RunAttemptInterrupted) {
		t.Fatalf("attempt state = %q, err=%v", attemptState, err)
	}
}

func TestCompleteThreadWorkflowTurn_QueuedFollowupSuppressesIntermediateIdle(t *testing.T) {
	fixture := newMultiagentInterruptFixture(t, "queued_followup")
	if _, err := fixture.store.AppendThreadEvents(
		fixture.ctx, fixture.session.ID, fixture.child.ID,
		[]domain.EventDraft{{
			Type: domain.EvAgentThreadMessageReceived,
			Payload: map[string]any{
				"from_session_thread_id": fixture.primary.ID,
				"from_agent_name":        "coordinator",
				"content": []any{map[string]any{
					"type": "text", "text": "queued",
				}},
			},
		}},
	); err != nil {
		t.Fatal(err)
	}
	completion, err := fixture.store.CompleteThreadWorkflowTurn(
		fixture.ctx, fixture.session.ID, fixture.child.ID, fixture.childTrigger.ID,
		[]domain.EventDraft{
			{Type: domain.EvAgentMessage, Payload: map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "first report"}},
			}},
			{Type: domain.EvSessionStatusIdle, Payload: map[string]any{
				"stop_reason": map[string]any{"type": "end_turn"},
			}},
		},
		domain.StatusIdle, "", "", nil, nil, nil, nil, nil, domain.TokenUsage{},
	)
	if err != nil {
		t.Fatal(err)
	}
	child, err := fixture.store.GetSessionThread(
		fixture.ctx, fixture.session.ID, fixture.child.ID,
	)
	if err != nil || child.Status != domain.StatusRunning {
		t.Fatalf("child = %+v, err=%v", child, err)
	}
	for _, event := range completion.Events {
		if event.ThreadID == fixture.child.ID &&
			event.Type == domain.EvSessionThreadStatusIdle {
			t.Fatalf("queued follow-up exposed intermediate idle: %+v", completion.Events)
		}
	}
}

func TestCompleteThreadWorkflowTurn_RetriesExhaustedFlushesQueuedFollowup(t *testing.T) {
	fixture := newMultiagentInterruptFixture(t, "retry_flush")
	queued, err := fixture.store.AppendThreadEvents(
		fixture.ctx, fixture.session.ID, fixture.child.ID,
		[]domain.EventDraft{{
			Type: domain.EvAgentThreadMessageReceived,
			Payload: map[string]any{
				"from_session_thread_id": fixture.primary.ID,
				"from_agent_name":        "coordinator",
				"content": []any{map[string]any{
					"type": "text", "text": "queued after failure",
				}},
			},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.CompleteThreadWorkflowTurn(
		fixture.ctx, fixture.session.ID, fixture.child.ID, fixture.childTrigger.ID,
		[]domain.EventDraft{
			{Type: domain.EvSessionError, Payload: map[string]any{"error": map[string]any{
				"type": "model_request_failed_error", "message": "retry budget exhausted",
				"retry_status": map[string]any{"type": "exhausted"},
			}}},
			{Type: domain.EvSessionStatusIdle, Payload: map[string]any{
				"stop_reason": map[string]any{"type": "retries_exhausted"},
			}},
		},
		domain.StatusIdle, "", "", nil, nil, nil, nil, nil, domain.TokenUsage{},
	); err != nil {
		t.Fatal(err)
	}
	queuedEvent, err := fixture.store.GetEvent(
		fixture.ctx, fixture.session.ID, queued[0].ID,
	)
	if err != nil || queuedEvent.ProcessedAt == nil {
		t.Fatalf("queued follow-up after exhausted retry = %+v, err=%v", queuedEvent, err)
	}
	child, err := fixture.store.GetSessionThread(
		fixture.ctx, fixture.session.ID, fixture.child.ID,
	)
	if err != nil || child.Status != domain.StatusIdle {
		t.Fatalf("child after exhausted retry = %+v, err=%v", child, err)
	}
}

func TestCompleteThreadWorkflowTurn_LateInterruptIsIdleNoop(t *testing.T) {
	fixture := newMultiagentInterruptFixture(t, "late_interrupt")
	if _, err := fixture.store.CompleteThreadWorkflowTurn(
		fixture.ctx, fixture.session.ID, fixture.child.ID, fixture.childTrigger.ID,
		[]domain.EventDraft{{
			Type: domain.EvSessionStatusIdle,
			Payload: map[string]any{
				"stop_reason": map[string]any{"type": "end_turn"},
			},
		}},
		domain.StatusIdle, "", "", nil, nil, nil, nil, nil, domain.TokenUsage{},
	); err != nil {
		t.Fatal(err)
	}
	admission, err := fixture.store.AdmitEvents(
		fixture.ctx, fixture.session.ID,
		[]domain.EventDraft{{
			Type:    domain.EvUserInterrupt,
			Payload: map[string]any{"session_thread_id": fixture.child.ID},
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if admission.Session.Status != domain.StatusIdle {
		t.Fatalf("idle interrupt reopened aggregate Session: %+v", admission.Session)
	}
	for _, event := range admission.Events {
		if event.Type == domain.EvSessionStatusRunning {
			t.Fatalf("idle interrupt emitted running lifecycle: %+v", admission.Events)
		}
	}
	ack, err := fixture.store.CompleteThreadWorkflowTurn(
		fixture.ctx, fixture.session.ID, fixture.child.ID,
		admission.SubmittedEvents[0].ID, nil,
		domain.StatusIdle, "", "", nil, nil, nil, nil, nil, domain.TokenUsage{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(ack.Events) != 0 {
		t.Fatalf("late interrupt emitted duplicate lifecycle: %+v", ack.Events)
	}
	interrupt, err := fixture.store.GetEvent(
		fixture.ctx, fixture.session.ID, admission.SubmittedEvents[0].ID,
	)
	if err != nil || interrupt.ProcessedAt == nil {
		t.Fatalf("late interrupt = %+v, err=%v", interrupt, err)
	}
}

func TestChildPendingActionsExposeSourceEventReference(t *testing.T) {
	for _, actionType := range []string{domain.EvAgentToolUse, domain.EvAgentMcpToolUse, domain.EvAgentCustomToolUse} {
		t.Run(actionType, func(t *testing.T) {
			fixture := newMultiagentInterruptFixture(t, "source_reference_"+actionType)
			const sourceID = "sevt_child_source"
			payload := map[string]any{"name": "lookup", "input": map[string]any{}}
			if actionType != domain.EvAgentCustomToolUse {
				payload["evaluated_permission"] = "ask"
			}
			if actionType == domain.EvAgentMcpToolUse {
				payload["mcp_server_name"] = "catalog"
			}
			_, err := fixture.store.CompleteThreadWorkflowTurn(fixture.ctx, fixture.session.ID, fixture.child.ID, fixture.childTrigger.ID,
				[]domain.EventDraft{{ID: sourceID, Type: actionType, Payload: payload}, {Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "requires_action", "event_ids": []string{sourceID}}}}},
				domain.StatusIdle, "", "", nil, []string{sourceID}, nil, nil, nil, domain.TokenUsage{})
			if err != nil {
				t.Fatal(err)
			}
			primaryEvents, err := fixture.store.QueryEvents(fixture.ctx, fixture.session.ID, app.EventQuery{Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			relayed := eventOfType(t, primaryEvents, actionType)
			if relayed.ID == sourceID || relayed.Payload["source_event_id"] != sourceID || relayed.Payload["session_thread_id"] != fixture.child.ID {
				t.Fatalf("relayed action lost source identity: %+v", relayed)
			}
			childEvents, err := fixture.store.ThreadEventsAfter(fixture.ctx, fixture.session.ID, fixture.child.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			original := eventOfType(t, childEvents, actionType)
			if original.ID != sourceID {
				t.Fatalf("source event not present on child: %+v", original)
			}
			// Both current public action references route to the same child barrier.
			resolutionType, referenceKey := domain.EvUserToolConfirmation, "tool_use_id"
			resolution := map[string]any{"result": "deny", referenceKey: sourceID}
			if actionType == domain.EvAgentCustomToolUse {
				resolutionType = domain.EvUserCustomToolResult
				resolution = map[string]any{"custom_tool_use_id": sourceID}
			}
			admission, err := fixture.store.AdmitEvents(fixture.ctx, fixture.session.ID, []domain.EventDraft{{Type: resolutionType, Payload: resolution}})
			if err != nil || len(admission.SubmittedEvents) != 1 || admission.SubmittedEvents[0].ThreadID != fixture.child.ID {
				t.Fatalf("source-reference resolution failed: %+v,err=%v", admission, err)
			}
		})
	}
}
