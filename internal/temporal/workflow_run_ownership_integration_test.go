package temporal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	workflowservice "go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

type ownershipWaitEvaluator struct{ calls atomic.Int64 }

func (e *ownershipWaitEvaluator) Next(context.Context, pg.WorkflowPumpSnapshot) (*pg.WorkflowPumpCommand, error) {
	e.calls.Add(1)
	return nil, nil
}

// History-absence cases start a fresh physical chain under a fixture-only
// Temporal ID against the same PG Run after its previous coordinator closes.
// This models an empty Temporal deduplication slot, not elapsed retention or
// actual server history deletion. Production continues to use its stable ID.
// The completed fixture ends after the real production Pump at cursor zero;
// it does not implement a product Run terminal protocol.
func TestWorkflowRunOwnershipAfterAbsentHistoryAndResetRealTemporal(t *testing.T) {
	database, address := os.Getenv("MANGO_TEST_DATABASE_URL"), os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if database == "" || address == "" {
		t.Skip("set PostgreSQL and Temporal service-test addresses")
	}
	for _, mode := range []string{"completed-history-absent", "terminated-history-absent", "unsupported-reset", "blocked-prefix-relay"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
			defer cancel()
			ids := domain.NewRandomIDGen()
			schema := "run_ownership_" + ids.NewID("")
			cfg, err := pgxpool.ParseConfig(database)
			require.NoError(t, err)
			cfg.ConnConfig.RuntimeParams["search_path"] = schema
			pool, err := pgxpool.NewWithConfig(ctx, cfg)
			require.NoError(t, err)
			defer pool.Close()
			_, err = pool.Exec(ctx, "CREATE SCHEMA "+schema)
			require.NoError(t, err)
			defer func() {
				_, err := pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
				require.NoError(t, err)
			}()
			require.NoError(t, pg.Migrate(ctx, pool))
			var clock domain.Clock = payloadOutcomeClock{}
			if mode == "blocked-prefix-relay" {
				clock = ownershipFixedClock{time.Now().UTC()}
			}
			store := pg.NewDefaultWorkspaceStore(pool, ids, clock)
			c, err := client.Dial(client.Options{HostPort: address})
			require.NoError(t, err)
			defer c.Close()
			queue := "run-ownership-" + ids.NewID("")
			w := worker.New(c, queue, worker.Options{})
			evaluator := &ownershipWaitEvaluator{}
			acts := &WorkflowRunActivities{Store: store, Evaluator: evaluator}
			RegisterWorkflowRunCoordinator(w, acts)
			if mode == "completed-history-absent" {
				w.RegisterWorkflowWithOptions(func(wctx workflow.Context, in WorkflowRunWorkflowInput) error {
					info := workflow.GetInfo(wctx)
					owner := pg.WorkflowCoordinatorClaim{ChainID: info.FirstRunID, ExecutionID: info.WorkflowExecution.RunID}
					actx := workflow.WithActivityOptions(wctx, workflow.ActivityOptions{StartToCloseTimeout: time.Minute})
					return workflow.ExecuteActivity(actx, ActivityWorkflowRunPump, WorkflowRunPumpInput{SessionID: in.SessionID, RunID: in.RunID, Owner: owner}).Get(actx, nil)
				}, workflow.RegisterOptions{Name: "ownership-complete-fixture"})
			}
			require.NoError(t, w.Start())
			defer w.Stop()
			now := time.Now().UTC()
			session := domain.Session{ID: ids.NewID(domain.PrefixSession), AgentID: "primary", AgentVersion: 1, EnvironmentID: "env_ownership", Status: domain.StatusIdle, Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now, AgentSnapshot: domain.Agent{ID: "primary", Version: 1, Model: domain.Model{ID: "must-not-run"}}, MultiagentRoster: []domain.Agent{{ID: "reviewer", Version: 1, Name: "reviewer", Model: domain.Model{ID: "must-not-run"}}}}
			_, err = store.CreateSession(ctx, session, nil)
			require.NoError(t, err)
			run, err := store.AdmitWorkflowRun(ctx, session.ID, "ownership", pg.WorkflowRunRequest{Source: "test-only/unexecuted", RuntimeProfile: "test-only/wait", Input: []byte(`{}`), Deadline: now.Add(time.Hour), MaxInvocations: 1, MaxResultBytes: 32})
			require.NoError(t, err)
			id := workflowRunWorkflowID(run.ID)
			defer func() {
				err := c.TerminateWorkflow(context.Background(), id, "", "ownership fixture cleanup")
				var missing *serviceerror.NotFound
				if !errors.As(err, &missing) {
					require.NoError(t, err)
				}
			}()
			kind := WorkflowRunWorkflowType
			if mode == "completed-history-absent" {
				kind = "ownership-complete-fixture"
			}
			original, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: queue}, kind, WorkflowRunWorkflowInput{SessionID: session.ID, RunID: run.ID})
			require.NoError(t, err)
			if mode == "completed-history-absent" {
				require.NoError(t, original.Get(ctx, nil))
			} else {
				require.Eventually(t, func() bool {
					return coordinatorEventCount(coordinatorHistory(t, ctx, c, id, original.GetRunID()), enumspb.EVENT_TYPE_TIMER_STARTED) == 1
				}, 10*time.Second, 20*time.Millisecond)
			}
			require.EqualValues(t, 1, evaluator.calls.Load())
			var chain, owner string
			var epoch, cursor int64
			assertPG := func() {
				t.Helper()
				require.NoError(t, pool.QueryRow(ctx, `SELECT coordinator_chain_id,coordinator_execution_id,evaluator_epoch,journal_cursor FROM workflow_runs WHERE id=$1`, run.ID).Scan(&chain, &owner, &epoch, &cursor))
				require.Equal(t, original.GetRunID(), chain)
				require.Equal(t, original.GetRunID(), owner)
				require.EqualValues(t, 1, epoch)
				require.Zero(t, cursor)
				var decisions, invocations int
				require.NoError(t, pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM workflow_pump_decisions),(SELECT count(*) FROM workflow_invocations)`).Scan(&decisions, &invocations))
				require.Zero(t, decisions)
				require.Zero(t, invocations)
				require.EqualValues(t, 1, evaluator.calls.Load())
			}
			assertPG()
			if mode == "blocked-prefix-relay" {
				for n := 0; n < 100; n++ {
					blocked, err := store.AdmitWorkflowRun(ctx, session.ID, fmt.Sprintf("blocked-%03d", n), pg.WorkflowRunRequest{Source: "test-only/unexecuted", RuntimeProfile: "test-only/wait", Input: []byte(`{}`), Deadline: now.Add(time.Hour), MaxInvocations: 1, MaxResultBytes: 32})
					require.NoError(t, err)
					_, err = pool.Exec(ctx, `UPDATE workflow_run_outbox SET available_at=$2,enqueued_at=$2 WHERE run_id=$1`, blocked.ID, clock.Now().Add(-time.Duration(101-n)*time.Second))
					require.NoError(t, err)
				}
				deliverer := &ownershipBlockedDeliverer{Signaler: NewSignalerOnTaskQueue(c, queue), healthy: run.ID}
				relay := NewWorkflowRunRelay(store, deliverer, RelayConfig{BatchSize: 100})
				n, err := relay.RunOnce(ctx)
				require.Error(t, err)
				require.Zero(t, n)
				require.EqualValues(t, 100, deliverer.blocked)
				n, err = relay.RunOnce(ctx)
				require.NoError(t, err)
				require.Equal(t, 1, n)
				require.Eventually(t, func() bool {
					return coordinatorEventCount(coordinatorHistory(t, ctx, c, id, original.GetRunID()), enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED) == 1
				}, 10*time.Second, 20*time.Millisecond)
				var remaining int
				require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM workflow_run_outbox WHERE delivery_attempts=1`).Scan(&remaining))
				require.Equal(t, 100, remaining)
				return
			}

			if mode == "unsupported-reset" {
				// Reset after the completed pump Activity: its existing result is in the
				// replay prefix. A current physical RunID check must not authorize reuse.
				var resetPoint int64
				for _, event := range coordinatorHistory(t, ctx, c, id, original.GetRunID()) {
					if event.EventType == enumspb.EVENT_TYPE_WORKFLOW_TASK_COMPLETED {
						resetPoint = event.EventId
					}
				}
				require.Positive(t, resetPoint)
				reset, err := c.WorkflowService().ResetWorkflowExecution(ctx, &workflowservice.ResetWorkflowExecutionRequest{Namespace: "default", WorkflowExecution: &commonpb.WorkflowExecution{WorkflowId: id, RunId: original.GetRunID()}, Reason: "isolated unsupported-reset probe", WorkflowTaskFinishEventId: resetPoint, RequestId: ids.NewID("")})
				require.NoError(t, err)
				require.NotEqual(t, original.GetRunID(), reset.RunId)
				require.Eventually(t, func() bool {
					events := coordinatorHistory(t, ctx, c, id, reset.RunId)
					for _, event := range events {
						if failed := event.GetWorkflowTaskFailedEventAttributes(); failed != nil && failed.Cause == enumspb.WORKFLOW_TASK_FAILED_CAUSE_NON_DETERMINISTIC_ERROR {
							t.Log("Reset fenced by replay nondeterminism")
							return true
						}
						if failed := event.GetWorkflowExecutionFailedEventAttributes(); failed != nil && failed.Failure.GetApplicationFailureInfo().GetType() == "WorkflowPumpFence" {
							t.Log("Reset fenced by nonretryable coordinator rejection")
							return true
						}
					}
					return false
				}, 10*time.Second, 20*time.Millisecond)
				assertPG()
				// Observed reset fencing may fail a replay task (nondeterminism), rather
				// than close the execution. Cleanup explicitly terminates the test run.
				return
			}
			if mode == "terminated-history-absent" {
				require.NoError(t, c.TerminateWorkflow(ctx, id, original.GetRunID(), "test closure before history removal"))
			}
			// The fixture chooses a fresh Temporal deduplication slot to model
			// absent retained history. PG business Run identity is unchanged.
			freshID := id + ".history-absent-fixture"
			defer func() {
				err := c.TerminateWorkflow(context.Background(), freshID, "", "fresh chain fixture cleanup")
				var missing *serviceerror.NotFound
				if !errors.As(err, &missing) {
					require.NoError(t, err)
				}
			}()
			restarted, err := c.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: freshID, TaskQueue: queue, WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE}, WorkflowRunWorkflowType, WorkflowRunWorkflowInput{SessionID: session.ID, RunID: run.ID})
			require.NoError(t, err)
			err = restarted.Get(ctx, nil)
			require.ErrorContains(t, err, "reconciliation")
			require.NotEqual(t, original.GetRunID(), restarted.GetRunID())
			assertPG()
		})
	}
}

type ownershipFixedClock struct{ now time.Time }

func (c ownershipFixedClock) Now() time.Time { return c.now }

type ownershipBlockedDeliverer struct {
	*Signaler
	healthy string
	blocked int
}

func (d *ownershipBlockedDeliverer) WakeWorkflowRun(ctx context.Context, wake pg.WorkflowRunWakeup) error {
	if wake.RunID != d.healthy {
		d.blocked++
		return errors.New("permanently blocked test target")
	}
	return d.Signaler.WakeWorkflowRun(ctx, wake)
}
