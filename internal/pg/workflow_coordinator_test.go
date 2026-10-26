package pg

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/pg/pgstore"
)

func TestWorkflowCoordinatorOwnershipAndHandoff(t *testing.T) {
	store, session, run, _ := workflowReceiptFixture(t)
	ctx := t.Context()
	first := WorkflowCoordinatorClaim{ChainID: "physical-a", ExecutionID: "physical-a"}
	require.NoError(t, store.BindWorkflowCoordinator(ctx, session.ID, run.ID, first))
	require.NoError(t, store.BindWorkflowCoordinator(ctx, session.ID, run.ID, first), "bind ACK loss")
	// Even a completed first pump may still be at cursor zero. A new logical
	// chain must never get that old cached claim after Temporal forgets history.
	_, err := store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "bypass", 0)
	require.ErrorContains(t, err, "coordinator")
	_, err = store.AdmitWorkflowInvocation(ctx, session.ID, run.ID, 0, WorkflowInvocationCommand{AgentName: "reviewer", Prompt: "bypass"})
	require.Error(t, err)
	claim, err := store.ClaimWorkflowCoordinatorEvaluator(ctx, session.ID, run.ID, "pump/0", 0, first.ExecutionID)
	require.NoError(t, err)
	fresh := WorkflowCoordinatorClaim{ChainID: "fresh-after-expiry", ExecutionID: "fresh-after-expiry"}
	require.ErrorContains(t, store.BindWorkflowCoordinator(ctx, session.ID, run.ID, fresh), "chain")
	// A fake waiting claim cannot justify handoff merely by epoch arithmetic.
	next := WorkflowCoordinatorClaim{ChainID: first.ChainID, ExecutionID: "physical-b", PredecessorID: first.ExecutionID}
	next.PreviousEpoch = 2
	require.Error(t, store.BindWorkflowCoordinator(ctx, session.ID, run.ID, next))
	next.PreviousEpoch = 0
	require.NoError(t, store.BindWorkflowCoordinator(ctx, session.ID, run.ID, next), "valid waiting CAN checkpoint")
	require.Error(t, store.BindWorkflowCoordinator(ctx, session.ID, run.ID, first), "late old bind cannot take ownership back")
	_, err = store.ClaimWorkflowCoordinatorEvaluator(ctx, session.ID, run.ID, "pump/0", 0, first.ExecutionID)
	require.ErrorContains(t, err, "coordinator", "cached claim is fenced")
	in := WorkflowPumpRequest{RequestID: "pump/0", Epoch: claim.Epoch, CoordinatorExecutionID: first.ExecutionID, Command: WorkflowPumpCommand{Kind: "unsupported"}}
	_, err = store.LoadWorkflowPumpSnapshot(ctx, session.ID, run.ID, in)
	require.ErrorContains(t, err, "coordinator")
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, in)
	require.ErrorContains(t, err, "coordinator")
	in.CoordinatorExecutionID = next.ExecutionID
	decision, err := store.CommitWorkflowPump(ctx, session.ID, run.ID, in)
	require.NoError(t, err)
	require.EqualValues(t, 1, decision.Cursor)
	third := WorkflowCoordinatorClaim{ChainID: first.ChainID, ExecutionID: "physical-c", PredecessorID: next.ExecutionID, Cursor: 1, PreviousEpoch: 1}
	require.NoError(t, store.BindWorkflowCoordinator(ctx, session.ID, run.ID, third), "committed CAN checkpoint")
	_, err = store.LoadWorkflowPumpSnapshot(ctx, session.ID, run.ID, in)
	require.ErrorContains(t, err, "coordinator", "cached decision remains owner-fenced")
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, in)
	require.ErrorContains(t, err, "coordinator", "cached commit remains owner-fenced")
	// Closed PG Run never accepts any binding, even its prior owner.
	_, err = store.pool.Exec(ctx, `UPDATE workflow_runs SET state='closed' WHERE id=$1`, run.ID)
	require.NoError(t, err)
	require.Error(t, store.BindWorkflowCoordinator(ctx, session.ID, run.ID, third))
}

func TestWorkflowCoordinatorConcurrentChains(t *testing.T) {
	store, session, run, _ := workflowReceiptFixture(t)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []string{"chain-a", "chain-b"} {
		wg.Go(func() {
			results <- store.BindWorkflowCoordinator(t.Context(), session.ID, run.ID, WorkflowCoordinatorClaim{ChainID: id, ExecutionID: id})
		})
	}
	wg.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		}
	}
	require.Equal(t, 1, wins)
	var chain, owner string
	require.NoError(t, store.pool.QueryRow(t.Context(), `SELECT coordinator_chain_id,coordinator_execution_id FROM workflow_runs WHERE id=$1`, run.ID).Scan(&chain, &owner))
	require.Equal(t, chain, owner)
}

func TestWorkflowRunDeliveryFairnessLeasesAndExhaustion(t *testing.T) {
	store, session, first, request := workflowReceiptFixture(t)
	ctx := t.Context()
	expire := func() {
		t.Helper()
		_, err := store.pool.Exec(ctx, `UPDATE workflow_run_outbox SET available_at=statement_timestamp()-interval '1 second' WHERE run_id=$1`, first.ID)
		require.NoError(t, err)
	}
	checkBackoff := func(wake WorkflowRunWakeup, delay time.Duration) {
		t.Helper()
		var before, after, available time.Time
		require.NoError(t, store.pool.QueryRow(ctx, `SELECT statement_timestamp()`).Scan(&before))
		require.NoError(t, store.DeferWorkflowRunWakeup(ctx, wake))
		require.NoError(t, store.pool.QueryRow(ctx, `SELECT statement_timestamp(),available_at FROM workflow_run_outbox WHERE run_id=$1`, wake.RunID).Scan(&after, &available))
		require.False(t, available.Before(before.Add(delay)))
		require.False(t, available.After(after.Add(delay)))
	}
	runs := []WorkflowRun{first}
	for n := 1; n <= 100; n++ {
		run, err := store.AdmitWorkflowRun(ctx, session.ID, fmt.Sprintf("delivery-%03d", n), request)
		require.NoError(t, err)
		runs = append(runs, run)
	}
	// A deterministic age order independently fixes the formerly starving prefix.
	now := store.clock.Now()
	for n, run := range runs {
		_, err := store.pool.Exec(ctx, `UPDATE workflow_run_outbox SET enqueued_at=$2,available_at=$2 WHERE run_id=$1`, run.ID, now.Add(time.Duration(n-101)*time.Second))
		require.NoError(t, err)
	}
	for n := 0; n < 100; n++ {
		rows, err := store.ClaimWorkflowRunWakeups(ctx, 1)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, runs[n].ID, rows[0].RunID)
		if n == 0 {
			checkBackoff(rows[0], time.Second)
		} else {
			require.NoError(t, store.DeferWorkflowRunWakeup(ctx, rows[0]))
		}
	}
	rows, err := store.ClaimWorkflowRunWakeups(ctx, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	healthy := rows[0]
	require.Equal(t, runs[100].ID, healthy.RunID)
	removed, err := store.DeleteWorkflowRunWakeupIfUnchanged(ctx, WorkflowRunWakeup{SessionID: healthy.SessionID, RunID: healthy.RunID, Version: healthy.Version})
	require.NoError(t, err)
	require.False(t, removed, "unleased ACK cannot delete a lease")
	removed, err = store.DeleteWorkflowRunWakeupIfUnchanged(ctx, healthy)
	require.NoError(t, err)
	require.True(t, removed)
	_, err = store.pool.Exec(ctx, `DELETE FROM workflow_runs WHERE id<>$1`, first.ID)
	require.NoError(t, err)
	// Advance only the fixture's due timestamp, without sleeping out backoff.
	expire()
	rows, err = store.ClaimWorkflowRunWakeups(ctx, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	checkBackoff(rows[0], 2*time.Second)
	rows, err = store.ClaimWorkflowRunWakeups(ctx, 1)
	require.NoError(t, err)
	require.Empty(t, rows, "failure defers eligibility")
	expire()
	rows, err = store.ClaimWorkflowRunWakeups(ctx, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	old := rows[0]
	// Local process time cannot reclaim a still-active DB lease.
	oldClock := store.clock
	store.clock = workflowReceiptClock{time.Date(3000, 1, 1, 0, 0, 0, 0, time.UTC)}
	rows, err = store.ClaimWorkflowRunWakeups(ctx, 1)
	require.NoError(t, err)
	require.Empty(t, rows, "lease eligibility uses DB time despite host clock skew")
	store.clock = oldClock
	expire()
	rows, err = store.ClaimWorkflowRunWakeups(ctx, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	current := rows[0]
	require.NotEqual(t, old.LeaseToken, current.LeaseToken)
	require.NoError(t, store.DeferWorkflowRunWakeup(ctx, old))
	removed, err = store.DeleteWorkflowRunWakeupIfUnchanged(ctx, old)
	require.NoError(t, err)
	require.False(t, removed, "expired worker cannot ACK replacement")
	for attempt := current.DeliveryAttempt + 1; attempt <= WorkflowRunDeliveryLimit; attempt++ {
		expire()
		rows, err = store.ClaimWorkflowRunWakeups(ctx, 1)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		require.Equal(t, attempt, rows[0].DeliveryAttempt)
		current = rows[0]
	}
	checkBackoff(current, time.Minute)
	expire()
	rows, err = store.ClaimWorkflowRunWakeups(ctx, 1)
	require.NoError(t, err)
	require.Empty(t, rows, "eight reserved attempts quarantine the durable row")
	var attempts int
	require.NoError(t, store.pool.QueryRow(ctx, `SELECT delivery_attempts FROM workflow_run_outbox WHERE run_id=$1`, first.ID).Scan(&attempts))
	require.Equal(t, 8, attempts)
	require.NoError(t, store.SetWorkflowRunPause(ctx, session.ID, first.ID, "capability", true))
	require.NoError(t, store.DeferWorkflowRunWakeup(ctx, current))
	removed, err = store.DeleteWorkflowRunWakeupIfUnchanged(ctx, current)
	require.NoError(t, err)
	require.False(t, removed)
	rows, err = store.ClaimWorkflowRunWakeups(ctx, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, 1, rows[0].DeliveryAttempt)
	require.EqualValues(t, 2, rows[0].Version)
	require.NoError(t, store.withPGXTx(ctx, func(tx pgx.Tx, _ *pgstore.Queries) error {
		return store.enqueueWorkflowRunLocked(ctx, tx, session.ID, first.ID, 2)
	}))
	require.NoError(t, store.pool.QueryRow(ctx, `SELECT delivery_attempts FROM workflow_run_outbox WHERE run_id=$1`, first.ID).Scan(&attempts))
	require.Equal(t, 1, attempts, "equal version does not erase a lease")
}

func TestWorkflowRunDeliveryConcurrentClaims(t *testing.T) {
	store, _, _, _ := workflowReceiptFixture(t)
	ctx := t.Context()
	now := store.clock.Now()
	store.clock = workflowReceiptClock{now}
	var wg sync.WaitGroup
	claimed := make(chan []WorkflowRunWakeup, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() { rows, err := store.ClaimWorkflowRunWakeups(ctx, 1); claimed <- rows; errs <- err })
	}
	wg.Wait()
	close(claimed)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	count := 0
	for rows := range claimed {
		count += len(rows)
	}
	require.Equal(t, 1, count)
	leases, err := store.WorkflowRunWakeups(ctx)
	require.NoError(t, err)
	require.Len(t, leases, 1)
	var active WorkflowRunWakeup
	require.NoError(t, store.pool.QueryRow(ctx, `SELECT session_id,run_id,version,lease_token FROM workflow_run_outbox`).Scan(&active.SessionID, &active.RunID, &active.Version, &active.LeaseToken))
	starts := make(chan bool, 2)
	preparationErrors := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			allowed, err := store.PrepareWorkflowRunStart(ctx, active)
			starts <- allowed
			preparationErrors <- err
		})
	}
	wg.Wait()
	close(starts)
	close(preparationErrors)
	allowedCount := 0
	for allowed := range starts {
		if allowed {
			allowedCount++
		}
	}
	for err := range preparationErrors {
		require.NoError(t, err)
	}
	require.Equal(t, 1, allowedCount, "one concurrent first-start reservation")
	_, err = store.pool.Exec(ctx, `DELETE FROM sessions`)
	require.NoError(t, err)
	require.NoError(t, store.pool.QueryRow(ctx, `SELECT count(*) FROM workflow_run_outbox`).Scan(&count))
	require.Zero(t, count, "Session deletion cascades leased wakes and ownership rows")
}

func TestWorkflowRunStartReservationSurvivesUncertaintyAndNewWake(t *testing.T) {
	store, session, run, _ := workflowReceiptFixture(t)
	ctx := t.Context()
	rows, err := store.ClaimWorkflowRunWakeups(ctx, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	lost := rows[0]
	// Simulate loss of the claim's SQL ACK before preparation or any RPC.
	_, err = store.pool.Exec(ctx, `UPDATE workflow_run_outbox SET available_at=statement_timestamp()-interval '1 second' WHERE run_id=$1`, run.ID)
	require.NoError(t, err)
	rows, err = store.ClaimWorkflowRunWakeups(ctx, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	old := rows[0]
	_, err = store.PrepareWorkflowRunStart(ctx, lost)
	require.Error(t, err)
	allowed, err := store.PrepareWorkflowRunStart(ctx, old)
	require.NoError(t, err)
	require.True(t, allowed)
	// The prepare commit succeeded but its caller lost the ACK before doing IO.
	allowed, err = store.PrepareWorkflowRunStart(ctx, old)
	require.NoError(t, err)
	require.False(t, allowed, "cached preparation never reissues a start allowance")
	require.NoError(t, store.SetWorkflowRunPause(ctx, session.ID, run.ID, "capability", true))
	require.NoError(t, store.SetWorkflowRunPause(ctx, session.ID, run.ID, "capability", false))
	_, err = store.PrepareWorkflowRunStart(ctx, old)
	require.Error(t, err, "stale lease cannot reserve")
	rows, err = store.ClaimWorkflowRunWakeups(ctx, 1)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	allowed, err = store.PrepareWorkflowRunStart(ctx, rows[0])
	require.NoError(t, err)
	require.False(t, allowed, "higher versions do not grant another logical start")
	var attempted bool
	var chain string
	require.NoError(t, store.pool.QueryRow(ctx, `SELECT coordinator_start_attempted,coordinator_chain_id FROM workflow_runs WHERE id=$1`, run.ID).Scan(&attempted, &chain))
	require.True(t, attempted)
	require.Empty(t, chain)
}
