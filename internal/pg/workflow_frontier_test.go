package pg

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func frontierRequest(id string, epoch, cursor int64) WorkflowPumpRequest {
	return WorkflowPumpRequest{RequestID: id, Epoch: epoch, ExpectedCursor: cursor, Command: WorkflowPumpCommand{Kind: "invoke", AgentName: "reviewer", Prompt: "same prompt"}}
}

func TestWorkflowFrontierStableDecisionsAndEpochReplacement(t *testing.T) {
	store, session, run, _ := workflowReceiptFixture(t)
	ctx := t.Context()
	claim, err := store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "claim-1", 0)
	require.NoError(t, err)
	require.Equal(t, WorkflowEvaluatorClaim{Epoch: 1, Cursor: 0}, claim)
	_, err = store.AdmitWorkflowInvocation(ctx, session.ID, run.ID, 0, WorkflowInvocationCommand{AgentName: "reviewer", Prompt: "bypass"})
	require.ErrorContains(t, err, "fenced frontier")
	bad := frontierRequest("pump-1", 1, 0)
	bad.Command.AgentName = "not admitted"
	rejected, err := store.CommitWorkflowPump(ctx, session.ID, run.ID, bad)
	require.NoError(t, err)
	require.Equal(t, WorkflowPumpDecision{Cursor: 1, Type: "rejected", ErrorCode: "agent_not_admitted"}, rejected)
	again, err := store.CommitWorkflowPump(ctx, session.ID, run.ID, bad)
	require.NoError(t, err)
	require.Equal(t, rejected, again)
	changed := bad
	changed.Command.Prompt = "different input"
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, changed)
	require.Error(t, err)
	replacement, err := store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "claim-2", 1)
	require.NoError(t, err)
	require.Equal(t, WorkflowEvaluatorClaim{Epoch: 2, Cursor: 1}, replacement)
	oldClaim, err := store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "claim-1", 0)
	require.NoError(t, err)
	require.Equal(t, claim, oldClaim)
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, bad)
	require.ErrorContains(t, err, "stale")
	bad.Epoch = 2
	again, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, bad)
	require.NoError(t, err)
	require.Equal(t, rejected, again, "ACK loss plus replacement returns the same committed decision")
	first, err := store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest("pump-2", 2, 1))
	require.NoError(t, err)
	require.Equal(t, "invocation", first.Type)
	require.NotNil(t, first.Attempt)
	again, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest("pump-2", 2, 1))
	require.NoError(t, err)
	require.Equal(t, first, again)
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest("different-pump", 2, 1))
	require.ErrorContains(t, err, "frontier")
	second, err := store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest("pump-3", 2, 2))
	require.NoError(t, err)
	require.NotEqual(t, first.Attempt.ThreadID, second.Attempt.ThreadID, "identical prompts remain different invocations")
	var invocations, decisions int
	var bytes int64
	require.NoError(t, store.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_invocations WHERE run_id=$1`, run.ID).Scan(&invocations))
	require.Equal(t, 2, invocations)
	require.NoError(t, store.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_pump_decisions WHERE run_id=$1`, run.ID).Scan(&decisions))
	require.Equal(t, 3, decisions)
	require.NoError(t, store.pool.QueryRow(ctx, `SELECT journal_bytes FROM workflow_runs WHERE id=$1`, run.ID).Scan(&bytes))
	require.Positive(t, bytes)
	var ordinal int
	require.NoError(t, store.pool.QueryRow(ctx, `SELECT ordinal FROM workflow_invocations WHERE id=$1`, first.Attempt.InvocationID).Scan(&ordinal))
	require.Equal(t, 1, ordinal, "a rejected call consumes a journal position without a fake invocation")
}

func TestWorkflowFrontierRollbackAndConcurrentACKRetries(t *testing.T) {
	store, session, run, _ := workflowReceiptFixture(t)
	ctx := t.Context()
	_, err := store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "claim", 0)
	require.NoError(t, err)
	for _, table := range []string{"workflow_pump_decisions", "workflow_run_outbox"} {
		t.Run(table, func(t *testing.T) {
			constraint := "CHECK (false) NOT VALID"
			if table == "workflow_run_outbox" {
				constraint = "CHECK (version < 2) NOT VALID"
			}
			_, err := store.pool.Exec(ctx, "ALTER TABLE "+table+" ADD CONSTRAINT frontier_fault "+constraint)
			require.NoError(t, err)
			_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest("pump", 1, 0))
			require.Error(t, err)
			var count int
			var cursor, bytes int64
			require.NoError(t, store.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_invocations WHERE run_id=$1`, run.ID).Scan(&count))
			require.Zero(t, count)
			require.NoError(t, store.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_pump_decisions WHERE run_id=$1`, run.ID).Scan(&count))
			require.Zero(t, count)
			require.NoError(t, store.pool.QueryRow(ctx, `SELECT journal_cursor,journal_bytes FROM workflow_runs WHERE id=$1`, run.ID).Scan(&cursor, &bytes))
			require.Zero(t, cursor)
			require.Zero(t, bytes)
			require.NoError(t, store.pool.QueryRow(ctx, `SELECT count(*) FROM session_threads WHERE session_id=$1`, session.ID).Scan(&count))
			require.Equal(t, 1, count)
			_, err = store.pool.Exec(ctx, "ALTER TABLE "+table+" DROP CONSTRAINT frontier_fault")
			require.NoError(t, err)
		})
	}
	var wg sync.WaitGroup
	results := make(chan WorkflowPumpDecision, 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			d, e := store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest("pump", 1, 0))
			results <- d
			errs <- e
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var first *WorkflowPumpDecision
	for result := range results {
		if first == nil {
			first = &result
		} else {
			require.Equal(t, *first, result)
		}
	}
	var count int
	require.NoError(t, store.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_invocations WHERE run_id=$1`, run.ID).Scan(&count))
	require.Equal(t, 1, count)
	// Distinct logical requests compete for one cursor, rather than becoming
	// two invocations merely because they reached different Activity workers.
	wins := make(chan bool, 2)
	for _, id := range []string{"next-a", "next-b"} {
		wg.Go(func() {
			_, err := store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest(id, 1, 1))
			wins <- err == nil
		})
	}
	wg.Wait()
	close(wins)
	successes := 0
	for won := range wins {
		if won {
			successes++
		}
	}
	require.Equal(t, 1, successes)
	claims := make(chan bool, 2)
	for _, id := range []string{"replacement-a", "replacement-b"} {
		wg.Go(func() { _, err := store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, id, 1); claims <- err == nil })
	}
	wg.Wait()
	close(claims)
	successes = 0
	for won := range claims {
		if won {
			successes++
		}
	}
	require.Equal(t, 1, successes)
}

func TestWorkflowFrontierFencesOldEffectsAndByteBudget(t *testing.T) {
	t.Run("unfenced prefix", func(t *testing.T) {
		store, session, run, _ := workflowReceiptFixture(t)
		admitReceiptInvocation(t, store, run, 0)
		_, err := store.ClaimWorkflowEvaluator(t.Context(), session.ID, run.ID, "claim", 0)
		require.ErrorContains(t, err, "reconciliation")
	})
	t.Run("byte budget rolls back effects", func(t *testing.T) {
		store, session, run, _ := workflowReceiptFixture(t)
		_, err := store.ClaimWorkflowEvaluator(t.Context(), session.ID, run.ID, "claim", 0)
		require.NoError(t, err)
		_, err = store.pool.Exec(t.Context(), `UPDATE workflow_runs SET journal_bytes=8388608 WHERE id=$1`, run.ID)
		require.NoError(t, err)
		_, err = store.CommitWorkflowPump(t.Context(), session.ID, run.ID, frontierRequest("pump", 1, 0))
		require.ErrorContains(t, err, "byte budget")
		var count int
		require.NoError(t, store.pool.QueryRow(t.Context(), `SELECT count(*) FROM workflow_invocations WHERE run_id=$1`, run.ID).Scan(&count))
		require.Zero(t, count)
	})
	t.Run("closed owner", func(t *testing.T) {
		store, session, run, _ := workflowReceiptFixture(t)
		_, err := store.ClaimWorkflowEvaluator(t.Context(), session.ID, run.ID, "claim", 0)
		require.NoError(t, err)
		_, err = store.pool.Exec(t.Context(), `UPDATE workflow_runs SET state='closed' WHERE id=$1`, run.ID)
		require.NoError(t, err)
		_, err = store.CommitWorkflowPump(t.Context(), session.ID, run.ID, frontierRequest("pump", 1, 0))
		require.ErrorContains(t, err, "owner")
	})
}

func TestWorkflowFrontierControlPausesAndDeadline(t *testing.T) {
	store, session, run, request := workflowReceiptFixture(t)
	ctx := t.Context()
	_, err := store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "claim", 0)
	require.NoError(t, err)
	require.NoError(t, store.SetWorkflowRunPause(ctx, session.ID, run.ID, "approval", true))
	_, err = store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{Type: domain.EvUserInterrupt, Payload: map[string]any{}}})
	require.NoError(t, err)
	require.NoError(t, store.SetWorkflowRunPause(ctx, session.ID, run.ID, "interrupt", false))
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest("pump", 1, 0))
	require.ErrorContains(t, err, "paused")
	require.NoError(t, store.SetWorkflowRunPause(ctx, session.ID, run.ID, "approval", false))
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest("pump", 1, 0))
	require.NoError(t, err)
	require.NoError(t, store.SetWorkflowRunPause(ctx, session.ID, run.ID, "capability", true))
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest("pump", 1, 0))
	require.ErrorContains(t, err, "paused", "cached decisions cannot bypass current controls")
	require.NoError(t, store.SetWorkflowRunPause(ctx, session.ID, run.ID, "capability", false))
	store.clock = workflowReceiptClock{now: request.Deadline.Add(time.Second)}
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest("next", 1, 1))
	require.ErrorContains(t, err, "deadline")
	_, err = store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "late-claim", 1)
	require.Error(t, err)
}

func TestWorkflowFrontierBudgetIdleInterruptRetainsIndependentPause(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	session := newSession("sesn_frontier_budget")
	session.ListCostKnown = true
	session.Budget = &domain.SessionBudget{MaxListCostCents: 1}
	session.AgentSnapshot.Model = domain.NormalizeModel(domain.Model{ID: "claude-opus-4-8"})
	session.AgentSnapshot.Multiagent = &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{Type: "agent", ID: "agent_reviewer", Version: 1}}}
	session.MultiagentRoster = []domain.Agent{{ID: "agent_reviewer", Version: 1, Name: "reviewer"}}
	_, err := store.CreateSession(ctx, session, nil)
	require.NoError(t, err)
	run, err := store.AdmitWorkflowRun(ctx, session.ID, "run", WorkflowRunRequest{Source: "unexecuted fixture", RuntimeProfile: "test/no-executor", Input: json.RawMessage(`{}`), Deadline: store.clock.Now().Add(time.Hour), MaxInvocations: 1, MaxResultBytes: 32})
	require.NoError(t, err)
	_, err = store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "claim", 0)
	require.NoError(t, err)
	primaryID, err := store.q.GetPrimarySessionThreadID(ctx, session.ID)
	require.NoError(t, err)
	require.NoError(t, store.AccountModelRequest(ctx, session.ID, primaryID, "sevt_budget_crossing", session.AgentSnapshot.Model, domain.TokenUsage{InputTokens: 2000}, "end_turn"))
	allowed, err := store.AdmitModelRequest(ctx, session.ID, primaryID)
	require.NoError(t, err)
	require.False(t, allowed)
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest("pump", 1, 0))
	require.ErrorContains(t, err, "paused")
	_, err = store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{Type: domain.EvUserInterrupt, Payload: map[string]any{}}})
	require.NoError(t, err)
	_, err = store.UpdateSession(ctx, session.ID, domain.SessionUpdate{Budget: &domain.SessionBudgetUpdate{Budget: nil}})
	require.NoError(t, err)
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest("pump", 1, 0))
	require.ErrorContains(t, err, "paused", "lifting budget must not erase the budget-idle interrupt")
	require.NoError(t, store.SetWorkflowRunPause(ctx, session.ID, run.ID, "interrupt", false))
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest("pump", 1, 0))
	require.NoError(t, err)
}

func TestWorkflowFrontierCreatesSuccessorWorkAfterRetirement(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_frontier_work")
	session.EnvironmentID, session.EnvironmentType = "env_frontier_work", "self_hosted"
	session.AgentSnapshot.Multiagent = &domain.Multiagent{Type: "coordinator", Agents: []domain.AgentReference{{Type: "agent", ID: "agent_reviewer", Version: 1}}}
	session.MultiagentRoster = []domain.Agent{{ID: "agent_reviewer", Version: 1, Name: "reviewer"}}
	require.NoError(t, NewEnvironmentRepository(store).Put(ctx, domain.Environment{ID: session.EnvironmentID, Name: "frontier-test", ConfigType: "self_hosted", Config: map[string]any{"type": "self_hosted"}, CreatedAt: store.clock.Now(), UpdatedAt: store.clock.Now()}))
	created, err := store.CreateSession(ctx, session, []domain.EventDraft{{Type: domain.EvUserMessage, Payload: map[string]any{"content": "initial work"}}})
	require.NoError(t, err)
	repo := NewEnvironmentWorkRepository(store)
	work, err := repo.PollWork(ctx, session.EnvironmentID, app.EnvironmentWorkPollInput{})
	require.NoError(t, err)
	require.NotNil(t, work)
	_, err = repo.AckWork(ctx, work.EnvironmentID, work.ID)
	require.NoError(t, err)
	_, err = repo.HeartbeatWork(ctx, work.EnvironmentID, work.ID, nil, nil)
	require.NoError(t, err)
	completed, err := store.CompleteWorkflowTurn(ctx, session.ID, created.SubmittedEvents[0].ID, []domain.EventDraft{{Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}}}}, domain.StatusIdle, "", "", nil, nil, nil)
	require.NoError(t, err)
	idleID := completed.Events[len(completed.Events)-1].ID
	require.NoError(t, repo.StopWork(ctx, work.EnvironmentID, work.ID, true, &idleID))
	run, err := store.AdmitWorkflowRun(ctx, session.ID, "run", WorkflowRunRequest{Source: "unexecuted fixture", RuntimeProfile: "test/no-executor", Input: json.RawMessage(`{}`), Deadline: store.clock.Now().Add(time.Hour), MaxInvocations: 1, MaxResultBytes: 32})
	require.NoError(t, err)
	_, err = store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "claim", 0)
	require.NoError(t, err)
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, frontierRequest("pump", 1, 0))
	require.NoError(t, err)
	next, err := repo.PollWork(ctx, work.EnvironmentID, app.EnvironmentWorkPollInput{})
	require.NoError(t, err)
	require.NotNil(t, next)
	require.NotEqual(t, work.ID, next.ID)
}
