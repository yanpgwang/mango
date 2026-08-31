package pg

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yanpgwang/mango/internal/agentruntime"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestCoordinatorDelegationRunsAsPersistentIndependentThread(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_coordinator_runtime")
	session.AgentSnapshot = domain.Agent{
		ID: session.AgentID, Version: session.AgentVersion, Name: "coordinator",
		Model: domain.NormalizeModel(domain.Model{ID: "claude-test"}),
		Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{
			Type: "agent", ID: "agent_reviewer", Version: 3,
		}}},
	}
	description := "Reviews a focused change."
	session.MultiagentRoster = []domain.Agent{{
		ID: "agent_reviewer", Version: 3, Name: "reviewer",
		Description: &description,
		Model:       domain.NormalizeModel(domain.Model{ID: "claude-reviewer"}),
	}}
	created, err := store.CreateSession(ctx, session, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{map[string]any{
			"type": "text", "text": "delegate the review",
		}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	var trigger domain.Event
	for _, event := range created.Events {
		if event.Type == domain.EvUserMessage {
			trigger = event
		}
	}
	threads, err := store.ListSessionThreads(
		ctx, session.ID, app.SessionThreadListQuery{Limit: 10},
	)
	if err != nil || len(threads) != 1 {
		t.Fatalf("primary Threads = %+v, err=%v", threads, err)
	}
	primary := threads[0]

	if _, err := store.EnsureAttempt(ctx, session.ID, trigger.ID, "ratm_delegate"); err != nil {
		t.Fatal(err)
	}
	input := map[string]any{
		"agent_name": "reviewer", "message": "Review internal/auth.go and report issues.",
	}
	if _, err := store.EnsureToolStep(
		ctx, "ratm_delegate", "tstep_delegate", 0,
		"sevt_private_delegate", agentruntime.SendToAgentToolName, input,
	); err != nil {
		t.Fatal(err)
	}
	executed, err := store.ExecuteCoordinatorToolStep(
		ctx, session.ID, primary.ID, trigger.ID, "tstep_delegate",
		agentruntime.SendToAgentToolName, input,
	)
	if err != nil || executed.Result.IsError || executed.WakeThreadID == "" {
		t.Fatalf("delegation = %+v, err=%v", executed, err)
	}
	threads, err = store.ListSessionThreads(
		ctx, session.ID, app.SessionThreadListQuery{Limit: 10},
	)
	if err != nil || len(threads) != 2 {
		t.Fatalf("delegated Threads = %+v, err=%v", threads, err)
	}
	child := threads[1]
	if child.ID != executed.WakeThreadID || child.Status != domain.StatusRunning ||
		child.Agent.Name != "reviewer" {
		t.Fatalf("child projection = %+v", child)
	}
	if _, err := store.EnsureToolStep(
		ctx, "ratm_delegate", "tstep_list_agents", 1,
		"sevt_private_list_agents", agentruntime.ListAgentsToolName,
		map[string]any{},
	); err != nil {
		t.Fatal(err)
	}
	listed, err := store.ExecuteCoordinatorToolStep(
		ctx, session.ID, primary.ID, trigger.ID, "tstep_list_agents",
		agentruntime.ListAgentsToolName, map[string]any{},
	)
	if err != nil || listed.Result.IsError || len(listed.Result.Content) != 1 {
		t.Fatalf("list_agents = %+v, err=%v", listed, err)
	}
	listedBlock, _ := listed.Result.Content[0].(map[string]any)
	listedJSON, _ := listedBlock["text"].(string)
	var roster struct {
		Agents []struct {
			Name    string `json:"name"`
			Threads []struct {
				ID     string `json:"session_thread_id"`
				Status string `json:"status"`
			} `json:"threads"`
		} `json:"agents"`
	}
	if err := json.Unmarshal([]byte(listedJSON), &roster); err != nil {
		t.Fatal(err)
	}
	if len(roster.Agents) != 1 || roster.Agents[0].Name != "reviewer" ||
		len(roster.Agents[0].Threads) != 1 ||
		roster.Agents[0].Threads[0].ID != child.ID ||
		roster.Agents[0].Threads[0].Status != string(domain.StatusRunning) {
		t.Fatalf("list_agents roster = %+v", roster)
	}
	if err := store.RecordWorkflowRetry(
		ctx, session.ID, trigger.ID,
		"sevt_primary_retry_error", "sevt_primary_rescheduled",
		map[string]any{
			"type": "model_overloaded_error", "message": "retry",
			"retry_status": map[string]any{"type": "retrying"},
		},
	); err != nil {
		t.Fatal(err)
	}
	duringPrimaryRetry, err := store.GetSession(ctx, session.ID)
	if err != nil || duringPrimaryRetry.Status != domain.StatusRunning {
		t.Fatalf("aggregate during coordinator retry = %+v, err=%v", duringPrimaryRetry, err)
	}
	primary, err = store.GetSessionThread(ctx, session.ID, primary.ID)
	if err != nil || primary.Status != domain.StatusRescheduling {
		t.Fatalf("rescheduling primary = %+v, err=%v", primary, err)
	}
	if err := store.ResumeWorkflowRetry(
		ctx, session.ID, trigger.ID, "sevt_primary_retry_running",
	); err != nil {
		t.Fatal(err)
	}
	primary, err = store.GetSessionThread(ctx, session.ID, primary.ID)
	if err != nil || primary.Status != domain.StatusRunning {
		t.Fatalf("resumed primary = %+v, err=%v", primary, err)
	}
	childEvents, err := store.ThreadEventsAfter(ctx, session.ID, child.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	childTrigger := eventOfType(t, childEvents, domain.EvAgentThreadMessageReceived)
	eventOfType(t, childEvents, domain.EvSessionThreadStatusRunning)
	primaryEvents, err := store.QueryEvents(
		ctx, session.ID, app.EventQuery{Limit: 30},
	)
	if err != nil {
		t.Fatal(err)
	}
	eventOfType(t, primaryEvents, domain.EvSessionThreadCreated)
	eventOfType(t, primaryEvents, domain.EvAgentThreadMessageSent)
	eventOfType(t, primaryEvents, domain.EvSessionThreadStatusRunning)
	var childWakeups int
	if err := store.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM thread_orchestration_outbox
WHERE session_id = $1 AND thread_id = $2`, session.ID, child.ID).Scan(&childWakeups); err != nil || childWakeups != 1 {
		t.Fatalf("child wakeup count = %d, err=%v", childWakeups, err)
	}
	if err := store.RecordThreadWorkflowRetry(
		ctx, session.ID, child.ID, childTrigger.ID,
		"sevt_child_retry_error", "sevt_child_rescheduled",
		map[string]any{
			"type": "model_overloaded_error", "message": "retry",
			"retry_status": map[string]any{"type": "retrying"},
		},
	); err != nil {
		t.Fatal(err)
	}
	child, err = store.GetSessionThread(ctx, session.ID, child.ID)
	if err != nil || child.Status != domain.StatusRescheduling {
		t.Fatalf("rescheduling child = %+v, err=%v", child, err)
	}
	duringRetry, err := store.GetSession(ctx, session.ID)
	if err != nil || duringRetry.Status != domain.StatusRunning {
		t.Fatalf("aggregate during child retry = %+v, err=%v", duringRetry, err)
	}
	if err := store.ResumeThreadWorkflowRetry(
		ctx, session.ID, child.ID, childTrigger.ID, "sevt_child_retry_running",
	); err != nil {
		t.Fatal(err)
	}
	child, err = store.GetSessionThread(ctx, session.ID, child.ID)
	if err != nil || child.Status != domain.StatusRunning {
		t.Fatalf("resumed child = %+v, err=%v", child, err)
	}

	completed, err := store.CompleteWorkflowTurnWithUsage(
		ctx, session.ID, trigger.ID,
		[]domain.EventDraft{{
			Type:    domain.EvSessionStatusIdle,
			Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}},
		}},
		domain.StatusIdle, "ratm_delegate", domain.RunAttemptCompleted,
		nil, nil, nil, domain.TokenUsage{InputTokens: 5},
	)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Session.Status != domain.StatusRunning {
		t.Fatalf("aggregate status after coordinator wait = %s", completed.Session.Status)
	}
	primary, err = store.GetSessionThread(ctx, session.ID, primary.ID)
	if err != nil || primary.Status != domain.StatusIdle || primary.Usage.InputTokens != 5 {
		t.Fatalf("independent primary after delegation = %+v, err=%v", primary, err)
	}

	if _, err := store.EnsureAttempt(
		ctx, session.ID, childTrigger.ID, "ratm_child_report",
	); err != nil {
		t.Fatal(err)
	}
	report := []any{map[string]any{"type": "text", "text": "No blocking issues found."}}
	childCompletion, err := store.CompleteThreadWorkflowTurn(
		ctx, session.ID, child.ID, childTrigger.ID,
		[]domain.EventDraft{
			{Type: domain.EvAgentMessage, Payload: map[string]any{"content": report}},
			{Type: domain.EvSessionThreadStatusIdle, Payload: map[string]any{
				"stop_reason": map[string]any{"type": "end_turn"},
			}},
		},
		domain.StatusIdle, "ratm_child_report", domain.RunAttemptCompleted,
		nil, nil, nil, nil, nil, domain.TokenUsage{OutputTokens: 7},
	)
	if err != nil {
		t.Fatal(err)
	}
	if childCompletion.Session.Status != domain.StatusRunning {
		t.Fatalf("aggregate status after report = %s", childCompletion.Session.Status)
	}
	child, err = store.GetSessionThread(ctx, session.ID, child.ID)
	if err != nil || child.Status != domain.StatusIdle || child.Usage.OutputTokens != 7 {
		t.Fatalf("completed child = %+v, err=%v", child, err)
	}
	childEvents, err = store.ThreadEventsAfter(ctx, session.ID, child.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	eventOfType(t, childEvents, domain.EvAgentThreadMessageSent)
	if hasEventType(childEvents, domain.EvAgentMessage) {
		t.Fatalf("child report must not be duplicated as agent.message: %+v", childEvents)
	}
	primaryEvents, err = store.QueryEvents(
		ctx, session.ID, app.EventQuery{Limit: 50},
	)
	if err != nil {
		t.Fatal(err)
	}
	primaryReport := eventOfType(t, primaryEvents, domain.EvAgentThreadMessageReceived)
	if primaryReport.Payload["from_session_thread_id"] != child.ID {
		t.Fatalf("primary report = %+v", primaryReport)
	}
	primary, err = store.GetSessionThread(ctx, session.ID, primary.ID)
	if err != nil || primary.Status != domain.StatusRunning {
		t.Fatalf("woken primary = %+v, err=%v", primary, err)
	}

	if _, err := store.EnsureAttempt(
		ctx, session.ID, primaryReport.ID, "ratm_primary_synthesis",
	); err != nil {
		t.Fatal(err)
	}
	final, err := store.CompleteWorkflowTurnWithUsage(
		ctx, session.ID, primaryReport.ID,
		[]domain.EventDraft{
			{Type: domain.EvAgentMessage, Payload: map[string]any{"content": report}},
			{Type: domain.EvSessionStatusIdle, Payload: map[string]any{
				"stop_reason": map[string]any{"type": "end_turn"},
			}},
		},
		domain.StatusIdle, "ratm_primary_synthesis", domain.RunAttemptCompleted,
		nil, nil, nil, domain.TokenUsage{OutputTokens: 3},
	)
	if err != nil {
		t.Fatal(err)
	}
	if final.Session.Status != domain.StatusIdle {
		t.Fatalf("final aggregate status = %s", final.Session.Status)
	}

	// A child report that races with a coordinator model retry must queue the
	// later synthesis turn without canceling the coordinator's rescheduling
	// ownership. Once the child idles, the Session aggregate is rescheduling
	// until that exact coordinator retry resumes.
	childCursor := childEvents[len(childEvents)-1].Sequence
	followup, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{map[string]any{
			"type": "text", "text": "delegate one more check",
		}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	followupTrigger := eventOfType(t, followup.Events, domain.EvUserMessage)
	if _, err := store.EnsureAttempt(
		ctx, session.ID, followupTrigger.ID, "ratm_followup_delegate",
	); err != nil {
		t.Fatal(err)
	}
	followupInput := map[string]any{
		"agent_name": "reviewer", "message": "Check the final diff again.",
		"session_thread_id": child.ID,
	}
	if _, err := store.EnsureToolStep(
		ctx, "ratm_followup_delegate", "tstep_followup_delegate", 0,
		"sevt_private_followup", agentruntime.SendToAgentToolName,
		followupInput,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExecuteCoordinatorToolStep(
		ctx, session.ID, primary.ID, followupTrigger.ID,
		"tstep_followup_delegate", agentruntime.SendToAgentToolName,
		followupInput,
	); err != nil {
		t.Fatal(err)
	}
	followupChildEvents, err := store.ThreadEventsAfter(
		ctx, session.ID, child.ID, childCursor, 20,
	)
	if err != nil {
		t.Fatal(err)
	}
	followupChildTrigger := eventOfType(
		t, followupChildEvents, domain.EvAgentThreadMessageReceived,
	)
	if _, err := store.EnsureAttempt(
		ctx, session.ID, followupChildTrigger.ID, "ratm_followup_child",
	); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordWorkflowRetry(
		ctx, session.ID, followupTrigger.ID,
		"sevt_followup_retry_error", "sevt_followup_rescheduled",
		map[string]any{
			"type": "model_overloaded_error", "message": "retry",
			"retry_status": map[string]any{"type": "retrying"},
		},
	); err != nil {
		t.Fatal(err)
	}
	raceReport := []any{map[string]any{"type": "text", "text": "Follow-up complete."}}
	raceCompletion, err := store.CompleteThreadWorkflowTurn(
		ctx, session.ID, child.ID, followupChildTrigger.ID,
		[]domain.EventDraft{
			{Type: domain.EvAgentMessage, Payload: map[string]any{"content": raceReport}},
			{Type: domain.EvSessionThreadStatusIdle, Payload: map[string]any{
				"stop_reason": map[string]any{"type": "end_turn"},
			}},
		},
		domain.StatusIdle, "ratm_followup_child", domain.RunAttemptCompleted,
		nil, nil, nil, nil, nil, domain.TokenUsage{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if raceCompletion.Session.Status != domain.StatusRescheduling {
		t.Fatalf("aggregate after racing child report = %s", raceCompletion.Session.Status)
	}
	primary, err = store.GetSessionThread(ctx, session.ID, primary.ID)
	if err != nil || primary.Status != domain.StatusRescheduling {
		t.Fatalf("coordinator retry was overwritten by child report: %+v, err=%v", primary, err)
	}
	primaryEvents, err = store.QueryEvents(
		ctx, session.ID, app.EventQuery{Limit: 100},
	)
	if err != nil {
		t.Fatal(err)
	}
	eventOfType(t, primaryEvents, domain.EvSessionStatusRescheduling)
	if err := store.ResumeWorkflowRetry(
		ctx, session.ID, followupTrigger.ID, "sevt_followup_retry_running",
	); err != nil {
		t.Fatal(err)
	}
	resumedSession, err := store.GetSession(ctx, session.ID)
	if err != nil || resumedSession.Status != domain.StatusRunning {
		t.Fatalf("aggregate after coordinator retry resume = %+v, err=%v", resumedSession, err)
	}
}

func TestTerminalChildFailureReportsAndWakesCoordinator(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_terminal_child")
	session.AgentSnapshot = domain.Agent{
		ID: session.AgentID, Version: session.AgentVersion, Name: "coordinator",
		Model: domain.NormalizeModel(domain.Model{ID: "claude-test"}),
		Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{
			Type: "agent", ID: "agent_reviewer", Version: 1,
		}}},
	}
	session.MultiagentRoster = []domain.Agent{{
		ID: "agent_reviewer", Version: 1, Name: "reviewer",
		Model: domain.NormalizeModel(domain.Model{ID: "claude-reviewer"}),
	}}
	created, err := store.CreateSession(ctx, session, []domain.EventDraft{{
		Type: domain.EvUserMessage,
		Payload: map[string]any{"content": []any{map[string]any{
			"type": "text", "text": "delegate a review",
		}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	trigger := eventOfType(t, created.Events, domain.EvUserMessage)
	threads, err := store.ListSessionThreads(
		ctx, session.ID, app.SessionThreadListQuery{Limit: 10},
	)
	if err != nil || len(threads) != 1 {
		t.Fatalf("primary Threads = %+v, err=%v", threads, err)
	}
	primary := threads[0]
	if _, err := store.EnsureAttempt(
		ctx, session.ID, trigger.ID, "ratm_terminal_delegate",
	); err != nil {
		t.Fatal(err)
	}
	delegateInput := map[string]any{
		"agent_name": "reviewer", "message": "review the change",
	}
	if _, err := store.EnsureToolStep(
		ctx, "ratm_terminal_delegate", "tstep_terminal_delegate", 0,
		"sevt_private_terminal_delegate", agentruntime.SendToAgentToolName,
		delegateInput,
	); err != nil {
		t.Fatal(err)
	}
	delegated, err := store.ExecuteCoordinatorToolStep(
		ctx, session.ID, primary.ID, trigger.ID, "tstep_terminal_delegate",
		agentruntime.SendToAgentToolName, delegateInput,
	)
	if err != nil || delegated.WakeThreadID == "" {
		t.Fatalf("delegate = %+v, err=%v", delegated, err)
	}
	child, err := store.GetSessionThread(ctx, session.ID, delegated.WakeThreadID)
	if err != nil {
		t.Fatal(err)
	}
	childEvents, err := store.ThreadEventsAfter(ctx, session.ID, child.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	childTrigger := eventOfType(t, childEvents, domain.EvAgentThreadMessageReceived)

	// The coordinator yields while the delegated turn is active. Its initial
	// wakeup is consumed so the assertion below proves terminal completion
	// durably schedules a fresh coordinator synthesis turn.
	if _, err := store.CompleteWorkflowTurn(
		ctx, session.ID, trigger.ID,
		[]domain.EventDraft{{
			Type: domain.EvSessionStatusIdle,
			Payload: map[string]any{
				"stop_reason": map[string]any{"type": "end_turn"},
			},
		}},
		domain.StatusIdle, "ratm_terminal_delegate", domain.RunAttemptCompleted,
		nil, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	wakeup, ok, err := store.PendingWakeup(ctx, session.ID)
	if err != nil || !ok {
		t.Fatalf("initial wakeup = %+v ok=%v err=%v", wakeup, ok, err)
	}
	if removed, err := store.DeleteWakeupIfUnchanged(
		ctx, session.ID, wakeup.MaxEventSeq,
	); err != nil || !removed {
		t.Fatalf("consume initial wakeup: removed=%v err=%v", removed, err)
	}

	const failureMessage = "provider rejected the child request"
	if _, err := store.EnsureAttempt(
		ctx, session.ID, childTrigger.ID, "ratm_terminal_child",
	); err != nil {
		t.Fatal(err)
	}
	attemptError := failureMessage
	completion, err := store.CompleteThreadWorkflowTurn(
		ctx, session.ID, child.ID, childTrigger.ID,
		[]domain.EventDraft{
			{Type: domain.EvSessionError, Payload: map[string]any{
				"error": map[string]any{
					"type": "model_request_failed_error", "message": failureMessage,
					"retry_status": map[string]any{"type": "terminal"},
				},
			}},
			{Type: domain.EvSessionStatusTerminated, Payload: map[string]any{}},
		},
		domain.StatusTerminated, "ratm_terminal_child", domain.RunAttemptFailed,
		&attemptError, nil, nil, nil, nil, domain.TokenUsage{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if completion.Session.Status != domain.StatusRunning {
		t.Fatalf("aggregate status = %s, want running", completion.Session.Status)
	}
	child, err = store.GetSessionThread(ctx, session.ID, child.ID)
	if err != nil || child.Status != domain.StatusTerminated {
		t.Fatalf("terminated child = %+v, err=%v", child, err)
	}
	primary, err = store.GetSessionThread(ctx, session.ID, primary.ID)
	if err != nil || primary.Status != domain.StatusRunning {
		t.Fatalf("woken coordinator = %+v, err=%v", primary, err)
	}
	primaryEvents, err := store.QueryEvents(
		ctx, session.ID, app.EventQuery{Limit: 100},
	)
	if err != nil {
		t.Fatal(err)
	}
	report := eventOfType(t, primaryEvents, domain.EvAgentThreadMessageReceived)
	content, _ := report.Payload["content"].([]any)
	block, _ := content[0].(map[string]any)
	text, _ := block["text"].(string)
	if report.Payload["from_session_thread_id"] != child.ID ||
		!strings.Contains(text, "reviewer") ||
		!strings.Contains(text, "model_request_failed_error") ||
		!strings.Contains(text, failureMessage) {
		t.Fatalf("terminal child report = %+v", report)
	}
	eventOfType(t, primaryEvents, domain.EvSessionThreadStatusTerminated)
	terminalWakeup, ok, err := store.PendingWakeup(ctx, session.ID)
	if err != nil || !ok || terminalWakeup.MaxEventSeq < report.Sequence {
		t.Fatalf("terminal wakeup = %+v ok=%v err=%v", terminalWakeup, ok, err)
	}
}

func TestCoordinatorCanAcceptChildReport(t *testing.T) {
	archivedAt := newSession("sesn_report_archived").CreatedAt
	for _, test := range []struct {
		name   string
		thread domain.SessionThread
		want   bool
	}{
		{name: "idle", thread: domain.SessionThread{Status: domain.StatusIdle}, want: true},
		{name: "running", thread: domain.SessionThread{Status: domain.StatusRunning}, want: true},
		{name: "rescheduling", thread: domain.SessionThread{Status: domain.StatusRescheduling}, want: true},
		{name: "terminated", thread: domain.SessionThread{Status: domain.StatusTerminated}},
		{name: "archived", thread: domain.SessionThread{
			Status: domain.StatusIdle, ArchivedAt: &archivedAt,
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := coordinatorCanAcceptReport(test.thread); got != test.want {
				t.Fatalf("coordinatorCanAcceptReport() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestChildPendingActionCrossPostsAndRoutesResolution(t *testing.T) {
	runChildPendingActionRouting(t, false)
}

func TestChildExternalApprovalCrossPostsAndRoutesResolution(t *testing.T) {
	runChildPendingActionRouting(t, true)
}

func runChildPendingActionRouting(t *testing.T, external bool) {
	t.Helper()
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_child_pending_route")
	session.AgentSnapshot = domain.Agent{
		ID: session.AgentID, Version: session.AgentVersion, Name: "coordinator",
		Model: domain.NormalizeModel(domain.Model{ID: "claude-test"}),
		Multiagent: &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{
			Type: "agent", ID: "agent_reviewer", Version: 1,
		}}},
	}
	session.MultiagentRoster = []domain.Agent{{
		ID: "agent_reviewer", Version: 1, Name: "reviewer",
		Model: domain.NormalizeModel(domain.Model{ID: "claude-reviewer"}),
		Tools: []any{map[string]any{"type": "agent_toolset", "configs": []any{
			map[string]any{"name": "bash", "permission": "always_ask"},
		}}},
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
	trigger := eventOfType(t, created.Events, domain.EvUserMessage)
	threads, err := store.ListSessionThreads(
		ctx, session.ID, app.SessionThreadListQuery{Limit: 10},
	)
	if err != nil {
		t.Fatal(err)
	}
	primary := threads[0]
	if _, err := store.EnsureAttempt(ctx, session.ID, trigger.ID, "ratm_pending_delegate"); err != nil {
		t.Fatal(err)
	}
	delegateInput := map[string]any{
		"agent_name": "reviewer", "message": "Run the gated check.",
	}
	if _, err := store.EnsureToolStep(
		ctx, "ratm_pending_delegate", "tstep_pending_delegate", 0,
		"sevt_private_pending_delegate", agentruntime.SendToAgentToolName,
		delegateInput,
	); err != nil {
		t.Fatal(err)
	}
	delegated, err := store.ExecuteCoordinatorToolStep(
		ctx, session.ID, primary.ID, trigger.ID, "tstep_pending_delegate",
		agentruntime.SendToAgentToolName, delegateInput,
	)
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.GetSessionThread(
		ctx, session.ID, delegated.WakeThreadID,
	)
	if err != nil {
		t.Fatal(err)
	}
	childEvents, err := store.ThreadEventsAfter(ctx, session.ID, child.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	childTrigger := eventOfType(t, childEvents, domain.EvAgentThreadMessageReceived)

	// The coordinator can yield while the child remains active; the aggregate
	// Session stays running until the child parks.
	if _, err := store.CompleteWorkflowTurn(
		ctx, session.ID, trigger.ID,
		[]domain.EventDraft{{
			Type:    domain.EvSessionStatusIdle,
			Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}},
		}},
		domain.StatusIdle, "ratm_pending_delegate", domain.RunAttemptCompleted,
		nil, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnsureAttempt(
		ctx, session.ID, childTrigger.ID, "ratm_child_pending",
	); err != nil {
		t.Fatal(err)
	}
	actionID := "sevt_child_ask"
	customActionID := "sevt_child_custom"
	childPendingDrafts := []domain.EventDraft{
		{
			ID: actionID, Type: domain.EvAgentToolUse,
			Payload: map[string]any{
				"name": "bash", "input": map[string]any{"command": "pwd"},
				"evaluated_permission": "ask",
			},
		},
		{
			ID: customActionID, Type: domain.EvAgentCustomToolUse,
			Payload: map[string]any{
				"name": "request_note", "input": map[string]any{"prompt": "note"},
			},
		},
		{
			Type: domain.EvSessionThreadStatusIdle,
			Payload: map[string]any{"stop_reason": map[string]any{
				"type": "requires_action", "event_ids": []string{actionID, customActionID},
			}},
		},
	}
	if external {
		childPendingDrafts[0].Payload[domain.InternalToolExecutionOwner] = "self_hosted"
	}
	parked, err := store.CompleteThreadWorkflowTurn(
		ctx, session.ID, child.ID, childTrigger.ID,
		childPendingDrafts,
		domain.StatusIdle, "ratm_child_pending", domain.RunAttemptCompleted,
		nil, []string{actionID, customActionID}, nil, nil, nil, domain.TokenUsage{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !parked.Parked || parked.Session.Status != domain.StatusIdle {
		t.Fatalf("parked child completion = %+v", parked)
	}
	replayed, err := store.CompleteThreadWorkflowTurn(
		ctx, session.ID, child.ID, childTrigger.ID,
		childPendingDrafts,
		domain.StatusIdle, "ratm_child_pending", domain.RunAttemptCompleted,
		nil, []string{actionID, customActionID}, nil, nil, nil, domain.TokenUsage{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Applied || !replayed.Parked {
		t.Fatalf("replayed child completion = %+v", replayed)
	}

	childEvents, err = store.ThreadEventsAfter(ctx, session.ID, child.ID, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	childAction := eventOfType(t, childEvents, domain.EvAgentToolUse)
	if childAction.ID != actionID || childAction.Payload["session_thread_id"] != nil {
		t.Fatalf("child-local action = %+v", childAction)
	}
	childCustomAction := eventOfType(t, childEvents, domain.EvAgentCustomToolUse)
	if childCustomAction.ID != customActionID ||
		childCustomAction.Payload["session_thread_id"] != nil {
		t.Fatalf("child-local custom action = %+v", childCustomAction)
	}
	childIdle := eventOfType(t, childEvents, domain.EvSessionThreadStatusIdle)
	childStop, _ := childIdle.Payload["stop_reason"].(map[string]any)
	childIDs, _ := stringList(childStop["event_ids"])
	if len(childIDs) != 2 || childIDs[0] != actionID || childIDs[1] != customActionID {
		t.Fatalf("child requires_action ids = %+v", childIDs)
	}

	primaryEvents, err := store.QueryEvents(
		ctx, session.ID, app.EventQuery{Limit: 100},
	)
	if err != nil {
		t.Fatal(err)
	}
	clientAction := eventOfType(t, primaryEvents, domain.EvAgentToolUse)
	if clientAction.ID == actionID || clientAction.Payload["session_thread_id"] != child.ID {
		t.Fatalf("primary cross-posted action = %+v", clientAction)
	}
	clientCustomAction := eventOfType(t, primaryEvents, domain.EvAgentCustomToolUse)
	if clientCustomAction.ID == customActionID ||
		clientCustomAction.Payload["session_thread_id"] != child.ID {
		t.Fatalf("primary cross-posted custom action = %+v", clientCustomAction)
	}
	threadIdle := eventOfType(t, primaryEvents, domain.EvSessionThreadStatusIdle)
	threadStop, _ := threadIdle.Payload["stop_reason"].(map[string]any)
	threadIDs, _ := stringList(threadStop["event_ids"])
	if len(threadIDs) != 2 || threadIDs[0] != clientAction.ID ||
		threadIDs[1] != clientCustomAction.ID {
		t.Fatalf("cross-posted Thread stop ids = %+v", threadIDs)
	}
	sessionIdle := eventOfType(t, primaryEvents, domain.EvSessionStatusIdle)
	sessionStop, _ := sessionIdle.Payload["stop_reason"].(map[string]any)
	sessionIDs, _ := stringList(sessionStop["event_ids"])
	if len(sessionIDs) != 2 || sessionIDs[0] != clientAction.ID ||
		sessionIDs[1] != clientCustomAction.ID {
		t.Fatalf("aggregate stop ids = %+v", sessionIDs)
	}
	primaryPending, err := store.UnresolvedPendingActions(ctx, session.ID)
	if err != nil || len(primaryPending) != 0 {
		t.Fatalf("primary pending actions = %+v, err=%v", primaryPending, err)
	}
	childPending, err := store.UnresolvedThreadPendingActions(
		ctx, session.ID, child.ID,
	)
	if err != nil || len(childPending) != 2 ||
		childPending[0].ActionEventID != actionID ||
		childPending[0].ClientActionEventID != clientAction.ID ||
		childPending[1].ActionEventID != customActionID ||
		childPending[1].ClientActionEventID != clientCustomAction.ID {
		t.Fatalf("child pending actions = %+v, err=%v", childPending, err)
	}

	if _, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserToolConfirmation,
		Payload: map[string]any{
			"tool_use_id": clientAction.ID, "result": "allow",
			"session_thread_id": "sthr_wrong",
		},
	}}); err == nil {
		t.Fatal("expected mismatched session_thread_id to be rejected")
	}
	partial, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{
		{
			Type: domain.EvUserCustomToolResult,
			Payload: map[string]any{
				"custom_tool_use_id": clientCustomAction.ID,
				"content": []any{
					map[string]any{"type": "text", "text": "noted"},
				},
			},
		},
		{
			Type: domain.EvSystemMessage,
			Payload: map[string]any{
				"content": []any{map[string]any{"type": "text", "text": "use UTC"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	customResult := eventOfType(t, partial.Events, domain.EvUserCustomToolResult)
	if customResult.ThreadID != child.ID ||
		customResult.Payload["session_thread_id"] != child.ID {
		t.Fatalf("routed custom result = %+v", customResult)
	}
	companion := eventOfType(t, partial.Events, domain.EvSystemMessage)
	if companion.ThreadID != child.ID {
		t.Fatalf("routed companion system message = %+v", companion)
	}
	child, err = store.GetSessionThread(ctx, session.ID, child.ID)
	if err != nil || child.Status != domain.StatusIdle || partial.Enqueued ||
		partial.Session.Status != domain.StatusIdle {
		t.Fatalf("partial child barrier = %+v/%+v, err=%v", child, partial, err)
	}
	admitted, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserToolConfirmation,
		Payload: map[string]any{
			"tool_use_id": clientAction.ID, "result": "allow",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	confirmation := eventOfType(t, admitted.Events, domain.EvUserToolConfirmation)
	if confirmation.ThreadID != child.ID ||
		confirmation.Payload["session_thread_id"] != child.ID {
		t.Fatalf("routed confirmation = %+v", confirmation)
	}
	if external {
		child, err = store.GetSessionThread(ctx, session.ID, child.ID)
		if err != nil || child.Status != domain.StatusIdle || admitted.Enqueued {
			t.Fatalf("external approval reopened child: child=%+v admission=%+v err=%v", child, admitted, err)
		}
		pending, err := store.UnresolvedThreadPendingActions(ctx, session.ID, child.ID)
		if err != nil || pending[0].ApprovalEventID == nil || *pending[0].ApprovalEventID != confirmation.ID {
			t.Fatalf("external approval receipt not persisted on child: %+v err=%v", pending, err)
		}
		admitted, err = store.AdmitEvents(ctx, session.ID, []domain.EventDraft{externalResult(clientAction.ID)})
		if err != nil {
			t.Fatal(err)
		}
		confirmation = eventOfType(t, admitted.Events, domain.EvUserToolResult)
		if confirmation.ThreadID != child.ID || confirmation.Payload["session_thread_id"] != child.ID {
			t.Fatalf("external result routed incorrectly: %+v", confirmation)
		}
	}
	child, err = store.GetSessionThread(ctx, session.ID, child.ID)
	if err != nil || child.Status != domain.StatusRunning ||
		admitted.Session.Status != domain.StatusRunning {
		t.Fatalf("resumed child/session = %+v/%+v, err=%v", child, admitted.Session, err)
	}

	report := []any{map[string]any{"type": "text", "text": "Approved check complete."}}
	output := []domain.EventDraft{
		{Type: domain.EvAgentMessage, Payload: map[string]any{"content": report}},
		{Type: domain.EvSessionThreadStatusIdle, Payload: map[string]any{
			"stop_reason": map[string]any{"type": "end_turn"},
		}},
	}
	if !external {
		output = append([]domain.EventDraft{{
			Type: domain.EvAgentToolResult, Payload: map[string]any{
				"tool_use_id": actionID,
				"content":     []any{map[string]any{"type": "text", "text": "/workspace"}},
				"is_error":    false,
			},
		}}, output...)
	}
	resumed, err := store.CompleteThreadWorkflowTurn(
		ctx, session.ID, child.ID, confirmation.ID,
		output,
		domain.StatusIdle, "", "", nil, nil,
		[]string{customResult.ID, confirmation.ID}, nil, nil, domain.TokenUsage{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Parked || resumed.Session.Status != domain.StatusRunning {
		t.Fatalf("resumed completion = %+v", resumed)
	}
	companion, err = store.GetEvent(ctx, session.ID, companion.ID)
	if err != nil || companion.ProcessedAt == nil {
		t.Fatalf("processed companion system message = %+v, err=%v", companion, err)
	}
	childPending, err = store.UnresolvedThreadPendingActions(
		ctx, session.ID, child.ID,
	)
	if err != nil || len(childPending) != 0 {
		t.Fatalf("resolved child pending actions = %+v, err=%v", childPending, err)
	}
}

func hasEventType(events []domain.Event, eventType string) bool {
	for _, event := range events {
		if event.Type == eventType {
			return true
		}
	}
	return false
}

func eventOfType(t *testing.T, events []domain.Event, eventType string) domain.Event {
	t.Helper()
	for _, event := range events {
		if event.Type == eventType {
			return event
		}
	}
	t.Fatalf("event %s not found in %+v", eventType, events)
	return domain.Event{}
}
