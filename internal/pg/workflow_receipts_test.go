package pg

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/agentruntime"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
	"github.com/yanpgwang/mango/internal/workspace"
)

func workflowReceiptFixture(t *testing.T) (*Store, domain.Session, WorkflowRun, WorkflowRunRequest) {
	t.Helper()
	store := testStore(t)
	session := newSession("sesn_workflow_receipts")
	system := "frozen reviewer"
	session.MultiagentRoster = []domain.Agent{{ID: "agent_reviewer", Version: 1, Name: "reviewer", Model: domain.Model{ID: "receipt-model"}, System: &system}}
	session.AgentSnapshot.Multiagent = &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{Type: "agent", ID: "agent_reviewer", Version: 1}}}
	_, err := store.CreateSession(t.Context(), session, nil)
	require.NoError(t, err)
	request := WorkflowRunRequest{Source: "unexecuted test fixture", RuntimeProfile: "test-only/no-executor",
		Input: json.RawMessage(`{"task":"review"}`), Deadline: store.clock.Now().Add(time.Hour), MaxInvocations: 2, MaxResultBytes: 32}
	run, err := store.AdmitWorkflowRun(t.Context(), session.ID, "admission-1", request)
	require.NoError(t, err)
	return store, session, run, request
}

func admitReceiptInvocation(t *testing.T, store *Store, run WorkflowRun, ordinal int) WorkflowInvocationAttempt {
	t.Helper()
	attempt, err := store.AdmitWorkflowInvocation(t.Context(), run.SessionID, run.ID, ordinal, WorkflowInvocationCommand{AgentName: "reviewer", Prompt: "same prompt"})
	require.NoError(t, err)
	return attempt
}

func completeReceiptInvocation(ctx context.Context, store *Store, run WorkflowRun, attempt WorkflowInvocationAttempt, messageID, text string) (TurnCompletion, error) {
	return store.CompleteThreadWorkflowTurn(ctx, run.SessionID, attempt.ThreadID, attempt.RootTriggerID,
		[]domain.EventDraft{{ID: messageID, Type: domain.EvAgentMessage, Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}},
			{Type: domain.EvSessionThreadStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}}}},
		domain.StatusIdle, "", "", nil, nil, nil,
		[]domain.Message{{Role: domain.RoleAssistant, Content: []domain.ContentBlock{{Type: "text", Text: text}}}}, nil,
		domain.TokenUsage{InputTokens: 7, OutputTokens: 3}, domain.ThreadReturnCandidate{MessageEventID: messageID, StopReason: "end_turn", TextOnly: true})
}

func TestWorkflowReceiptOwnershipIdentityAndReplay(t *testing.T) {
	store, session, run, request := workflowReceiptFixture(t)
	ctx := t.Context()
	retry, err := store.AdmitWorkflowRun(ctx, session.ID, "admission-1", request)
	require.NoError(t, err)
	require.Equal(t, run, retry)
	request.Input = json.RawMessage(`{"task":"changed"}`)
	_, err = store.AdmitWorkflowRun(ctx, session.ID, "admission-1", request)
	require.Error(t, err)
	first := admitReceiptInvocation(t, store, run, 0)
	replayed := admitReceiptInvocation(t, store, run, 0)
	require.Equal(t, first, replayed)
	second := admitReceiptInvocation(t, store, run, 1)
	require.NotEqual(t, first.ID, second.ID)
	require.NotEqual(t, first.ThreadID, second.ThreadID)
	_, err = store.AdmitWorkflowInvocation(ctx, session.ID, run.ID, 0, WorkflowInvocationCommand{AgentName: "reviewer", Prompt: "different"})
	require.Error(t, err)
	_, err = store.AdmitWorkflowInvocation(ctx, session.ID, run.ID, 2, WorkflowInvocationCommand{AgentName: "reviewer", Prompt: "third"})
	require.Error(t, err)
	var tools, turns int
	require.NoError(t, store.pool.QueryRow(ctx, "SELECT count(*) FROM tool_steps").Scan(&tools))
	require.NoError(t, store.pool.QueryRow(ctx, "SELECT count(*) FROM turn_attempts").Scan(&turns))
	require.Zero(t, tools, "no fabricated primary tool call")
	require.Zero(t, turns, "business attempt is separate from turn attempts")
	wakes, err := store.WorkflowRunWakeups(ctx)
	require.NoError(t, err)
	require.Len(t, wakes, 1)
	oldWake := wakes[0]
	completed, err := completeReceiptInvocation(ctx, store, run, first, "sevt_owned_return", "complete")
	require.NoError(t, err)
	require.True(t, completed.Applied)
	stored, err := store.GetWorkflowInvocationAttempt(ctx, session.ID, first.ID)
	require.NoError(t, err)
	require.Equal(t, &WorkflowInvocationReceipt{AdapterVersion: 1, Type: "text", Text: "complete"}, stored.Receipt)
	deleted, err := store.DeleteWorkflowRunWakeupIfUnchanged(ctx, oldWake)
	require.NoError(t, err)
	require.False(t, deleted, "a newer result wake cannot be deleted by the older pump acknowledgement")
	_, primaryWake, err := store.PendingWakeup(ctx, session.ID)
	require.NoError(t, err)
	require.False(t, primaryWake, "owned result must wake its Run rather than primary synthesis")
	primaryEvents, err := store.EventsAfter(ctx, session.ID, 0, 200)
	require.NoError(t, err)
	for _, event := range primaryEvents {
		require.NotEqual(t, domain.EvAgentThreadMessageReceived, event.Type)
	}
	before, err := store.ThreadEventsAfter(ctx, session.ID, first.ThreadID, 0, 200)
	require.NoError(t, err)
	duplicate, err := completeReceiptInvocation(ctx, store, run, first, "sevt_ignored_retry", "different retry output")
	require.NoError(t, err)
	require.False(t, duplicate.Applied)
	after, err := store.ThreadEventsAfter(ctx, session.ID, first.ThreadID, 0, 200)
	require.NoError(t, err)
	require.Equal(t, before, after)
	thread, err := store.GetSessionThread(ctx, session.ID, first.ThreadID)
	require.NoError(t, err)
	require.EqualValues(t, 7, thread.Usage.InputTokens)
	wrongOwner := first
	wrongOwner.ThreadID = second.ThreadID
	_, err = completeReceiptInvocation(ctx, store, run, wrongOwner, "sevt_wrong_owner", "wrong")
	require.Error(t, err)
	secondStored, err := store.GetWorkflowInvocationAttempt(ctx, session.ID, second.ID)
	require.NoError(t, err)
	require.Nil(t, secondStored.Receipt)
	_, err = store.ArchiveSessionThread(ctx, session.ID, first.ThreadID)
	require.ErrorContains(t, err, "Run lifecycle")
	// An ordinary coordinator follow-up cannot reuse the Run-owned Thread.
	admitted, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{Type: domain.EvUserMessage, Payload: map[string]any{"content": "follow up"}}})
	require.NoError(t, err)
	trigger := admitted.SubmittedEvents[0]
	_, err = store.EnsureAttempt(ctx, session.ID, trigger.ID, "ratm_followup")
	require.NoError(t, err)
	input := map[string]any{"agent_name": "reviewer", "session_thread_id": first.ThreadID, "message": "ordinary follow-up"}
	_, err = store.EnsureToolStep(ctx, "ratm_followup", "tstep_followup", 0, "sevt_followup_tool", agentruntime.SendToAgentToolName, input)
	require.NoError(t, err)
	threads, err := store.ListSessionThreads(ctx, session.ID, app.SessionThreadListQuery{Limit: 10})
	require.NoError(t, err)
	var primaryID string
	for _, thread := range threads {
		if thread.ParentThreadID == nil {
			primaryID = thread.ID
		}
	}
	execution, err := store.ExecuteCoordinatorToolStep(ctx, session.ID, primaryID, trigger.ID, "tstep_followup", agentruntime.SendToAgentToolName, input)
	require.NoError(t, err)
	require.True(t, execution.Result.IsError)
	require.Contains(t, execution.Result.Content, map[string]any{"type": "text", "text": "Workflow-owned Threads cannot receive ordinary follow-ups"})
}

func TestWorkflowReceiptWakeFailureRollsBackCompletion(t *testing.T) {
	store, _, run, _ := workflowReceiptFixture(t)
	ctx := t.Context()
	attempt := admitReceiptInvocation(t, store, run, 0)
	wakes, err := store.WorkflowRunWakeups(ctx)
	require.NoError(t, err)
	require.Len(t, wakes, 1)
	deleted, err := store.DeleteWorkflowRunWakeupIfUnchanged(ctx, wakes[0])
	require.NoError(t, err)
	require.True(t, deleted)
	_, err = store.pool.Exec(ctx, "ALTER TABLE workflow_run_outbox ADD CONSTRAINT fail_run_wake CHECK (version < 0)")
	require.NoError(t, err)
	_, err = completeReceiptInvocation(ctx, store, run, attempt, "sevt_rollback", "valid")
	require.ErrorContains(t, err, "fail_run_wake")
	stored, err := store.GetWorkflowInvocationAttempt(ctx, run.SessionID, attempt.ID)
	require.NoError(t, err)
	require.Nil(t, stored.Receipt)
	trigger, err := store.GetEvent(ctx, run.SessionID, attempt.RootTriggerID)
	require.NoError(t, err)
	require.Nil(t, trigger.ProcessedAt)
	var transcripts, outputs int
	require.NoError(t, store.pool.QueryRow(ctx, "SELECT count(*) FROM provider_transcript_turns WHERE trigger_event_id=$1", attempt.RootTriggerID).Scan(&transcripts))
	require.NoError(t, store.pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE id='sevt_rollback'").Scan(&outputs))
	require.Zero(t, transcripts)
	require.Zero(t, outputs)
	thread, err := store.GetSessionThread(ctx, run.SessionID, attempt.ThreadID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusRunning, thread.Status)
	require.Zero(t, thread.Usage.InputTokens)
	_, err = store.pool.Exec(ctx, "ALTER TABLE workflow_run_outbox DROP CONSTRAINT fail_run_wake")
	require.NoError(t, err)
	_, err = completeReceiptInvocation(ctx, store, run, attempt, "sevt_after_rollback", "valid")
	require.NoError(t, err)
	stored, err = store.GetWorkflowInvocationAttempt(ctx, run.SessionID, attempt.ID)
	require.NoError(t, err)
	require.Equal(t, "valid", stored.Receipt.Text)
	require.NoError(t, Migrate(ctx, store.pool), "reapplication must preserve receipts")
}

func TestWorkflowReceiptAdmissionRollbackAndConcurrentRetry(t *testing.T) {
	store, _, run, _ := workflowReceiptFixture(t)
	ctx := t.Context()
	_, err := store.pool.Exec(ctx, "ALTER TABLE thread_orchestration_outbox ADD CONSTRAINT fail_owned_spawn CHECK (max_event_seq < 0)")
	require.NoError(t, err)
	_, err = store.AdmitWorkflowInvocation(ctx, run.SessionID, run.ID, 0, WorkflowInvocationCommand{AgentName: "reviewer", Prompt: "same prompt"})
	require.ErrorContains(t, err, "fail_owned_spawn")
	threads, err := store.ListSessionThreads(ctx, run.SessionID, app.SessionThreadListQuery{Limit: 10})
	require.NoError(t, err)
	require.Len(t, threads, 1, "failed spawn cannot retain a child")
	var invocations int
	require.NoError(t, store.pool.QueryRow(ctx, "SELECT count(*) FROM workflow_invocations").Scan(&invocations))
	require.Zero(t, invocations)
	_, err = store.pool.Exec(ctx, "ALTER TABLE thread_orchestration_outbox DROP CONSTRAINT fail_owned_spawn")
	require.NoError(t, err)
	var wg sync.WaitGroup
	results := make(chan WorkflowInvocationAttempt, 8)
	errors := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			attempt, err := store.AdmitWorkflowInvocation(ctx, run.SessionID, run.ID, 0, WorkflowInvocationCommand{AgentName: "reviewer", Prompt: "same prompt"})
			results <- attempt
			errors <- err
		})
	}
	wg.Wait()
	close(results)
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	var identity string
	for result := range results {
		if identity == "" {
			identity = result.ID
		}
		require.Equal(t, identity, result.ID)
	}
	require.NoError(t, store.pool.QueryRow(ctx, "SELECT count(*) FROM workflow_invocations").Scan(&invocations))
	require.Equal(t, 1, invocations)
}

func TestWorkflowReceiptRejectsEarlierMessagesAndBoundsText(t *testing.T) {
	for _, test := range []struct {
		name, code string
		candidate  domain.ThreadReturnCandidate
		text       string
	}{
		{"earlier_message", "missing_output", domain.ThreadReturnCandidate{MessageEventID: "sevt_absent_terminal", StopReason: "end_turn", TextOnly: true}, "earlier tool round"},
		{"oversized", "output_too_large", domain.ThreadReturnCandidate{MessageEventID: "sevt_candidate", StopReason: "end_turn", TextOnly: true}, strings.Repeat("x", 33)},
		{"refusal", "nonterminal_output", domain.ThreadReturnCandidate{MessageEventID: "sevt_candidate", StopReason: "refusal", TextOnly: true}, "refused"},
		{"unsupported", "unsupported_output", domain.ThreadReturnCandidate{MessageEventID: "sevt_candidate", StopReason: "end_turn"}, "text alongside unsupported content"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _, run, _ := workflowReceiptFixture(t)
			attempt := admitReceiptInvocation(t, store, run, 0)
			_, err := store.CompleteThreadWorkflowTurn(t.Context(), run.SessionID, attempt.ThreadID, attempt.RootTriggerID,
				[]domain.EventDraft{{ID: "sevt_candidate", Type: domain.EvAgentMessage, Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": test.text}}}}},
				domain.StatusIdle, "", "", nil, nil, nil, nil, nil, domain.TokenUsage{}, test.candidate)
			require.NoError(t, err)
			stored, err := store.GetWorkflowInvocationAttempt(t.Context(), run.SessionID, attempt.ID)
			require.NoError(t, err)
			require.Equal(t, &WorkflowInvocationReceipt{AdapterVersion: 1, Type: "failure", ErrorCode: test.code}, stored.Receipt)
		})
	}
}

func TestWorkflowReceiptClosedRunCannotAcceptBusinessSuccess(t *testing.T) {
	store, _, run, _ := workflowReceiptFixture(t)
	attempt := admitReceiptInvocation(t, store, run, 0)
	// This injects an authoritative closed state; this kernel does not implement
	// a user-facing Run stop operation or its physical child cleanup.
	_, err := store.pool.Exec(t.Context(), "UPDATE workflow_runs SET state='closed' WHERE id=$1", run.ID)
	require.NoError(t, err)
	_, err = completeReceiptInvocation(t.Context(), store, run, attempt, "sevt_late", "late success")
	require.NoError(t, err)
	stored, err := store.GetWorkflowInvocationAttempt(t.Context(), run.SessionID, attempt.ID)
	require.NoError(t, err)
	require.Equal(t, "run_closed", stored.Receipt.ErrorCode)
	_, err = store.AdmitWorkflowInvocation(t.Context(), run.SessionID, run.ID, 1, WorkflowInvocationCommand{AgentName: "reviewer", Prompt: "new effect"})
	require.Error(t, err)
}

func TestWorkflowAdmissionJSONIdentityUsesExactStorageNumbers(t *testing.T) {
	store, session, _, request := workflowReceiptFixture(t)
	for _, input := range []string{`{"n":1e3}`, `{"n":1e-3}`, `{"a":1,"b":2}`, `{"n":9007199254740992}`} {
		request.Input = json.RawMessage(input)
		first, err := store.AdmitWorkflowRun(t.Context(), session.ID, input, request)
		require.NoError(t, err)
		retry, err := store.AdmitWorkflowRun(t.Context(), session.ID, input, request)
		require.NoError(t, err, "the exact same input must retry after jsonb normalization")
		require.Equal(t, first, retry)
	}
	request.Input = json.RawMessage(`{"n":1000}`)
	_, err := store.AdmitWorkflowRun(t.Context(), session.ID, `{"n":1e3}`, request)
	require.NoError(t, err, "identity uses exact jsonb value equality")
	request.Input = json.RawMessage(`{"n":9007199254740993}`)
	_, err = store.AdmitWorkflowRun(t.Context(), session.ID, `{"n":9007199254740992}`, request)
	require.Error(t, err, "distinct large integers must not collapse through float64")
}

func TestWorkflowReceiptScopeFrozenRosterAndDeadline(t *testing.T) {
	store, session, run, request := workflowReceiptFixture(t)
	other := workspace.WithScope(t.Context(), "wrkspc_other")
	_, err := store.AdmitWorkflowRun(other, session.ID, "foreign", request)
	require.Error(t, err)
	_, err = store.AdmitWorkflowInvocation(other, session.ID, run.ID, 0, WorkflowInvocationCommand{AgentName: "reviewer", Prompt: "foreign"})
	require.Error(t, err)
	wakes, err := store.WorkflowRunWakeups(other)
	require.NoError(t, err)
	require.Empty(t, wakes)
	current, err := store.GetSession(t.Context(), session.ID)
	require.NoError(t, err)
	changed := "later roster edit"
	current.MultiagentRoster[0].System = &changed
	body, err := json.Marshal(current)
	require.NoError(t, err)
	_, err = store.pool.Exec(t.Context(), "UPDATE sessions SET body=$2 WHERE id=$1", session.ID, body)
	require.NoError(t, err)
	attempt := admitReceiptInvocation(t, store, run, 0)
	thread, err := store.GetSessionThread(t.Context(), session.ID, attempt.ThreadID)
	require.NoError(t, err)
	require.Equal(t, "frozen reviewer", *thread.Agent.System)
	_, err = store.GetWorkflowInvocationAttempt(other, session.ID, attempt.ID)
	require.Error(t, err)
	store.clock = workflowReceiptClock{now: request.Deadline.Add(time.Second)}
	_, err = completeReceiptInvocation(t.Context(), store, run, attempt, "sevt_expired", "late")
	require.NoError(t, err)
	stored, err := store.GetWorkflowInvocationAttempt(t.Context(), session.ID, attempt.ID)
	require.NoError(t, err)
	require.Equal(t, "deadline_exceeded", stored.Receipt.ErrorCode)
}

func TestWorkflowReceiptWaitDoesNotSettleAndStaleTurnCannotReplace(t *testing.T) {
	store, _, run, _ := workflowReceiptFixture(t)
	attempt := admitReceiptInvocation(t, store, run, 0)
	_, err := store.CompleteThreadWorkflowTurn(t.Context(), run.SessionID, attempt.ThreadID, attempt.RootTriggerID,
		[]domain.EventDraft{{Type: domain.EvSessionThreadStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "budget_reached"}}}},
		domain.StatusIdle, "", "", nil, nil, nil, nil, nil, domain.TokenUsage{})
	require.NoError(t, err)
	stored, err := store.GetWorkflowInvocationAttempt(t.Context(), run.SessionID, attempt.ID)
	require.NoError(t, err)
	require.Nil(t, stored.Receipt, "a budget wait is not a return")
	second := admitReceiptInvocation(t, store, run, 1)
	_, err = completeReceiptInvocation(t.Context(), store, run, second, "sevt_first_result", "first")
	require.NoError(t, err)
	var staleTrigger string
	err = store.withTx(t.Context(), func(q *pgstore.Queries) error {
		max, err := q.MaxEventSeq(t.Context(), run.SessionID)
		if err != nil {
			return err
		}
		events, _, err := store.appendThreadDrafts(t.Context(), q, run.SessionID, second.ThreadID,
			[]domain.EventDraft{{Type: domain.EvAgentThreadMessageReceived, Payload: map[string]any{"content": "injected stale turn"}}}, max, nil)
		if err == nil {
			staleTrigger = events[0].ID
		}
		return err
	})
	require.NoError(t, err)
	stale := second
	stale.RootTriggerID = staleTrigger
	_, err = completeReceiptInvocation(t.Context(), store, run, stale, "sevt_stale_result", "replacement")
	require.ErrorContains(t, err, "terminal receipt")
	stored, err = store.GetWorkflowInvocationAttempt(t.Context(), run.SessionID, second.ID)
	require.NoError(t, err)
	require.Equal(t, "first", stored.Receipt.Text)
}

type workflowReceiptClock struct{ now time.Time }

func (c workflowReceiptClock) Now() time.Time { return c.now }
