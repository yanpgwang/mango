package temporal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	historypb "go.temporal.io/api/history/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"
)

// The only evaluator is a test-only two-step protocol. Production Run/Thread
// Workflows, Activities, PG frontier, owned receipts and both outboxes execute
// the journey. There is no JS, public admission or substitute agent loop.
func TestWorkflowRunCoordinatorRecoveryRealTemporal(t *testing.T) {
	database, address := os.Getenv("MANGO_TEST_DATABASE_URL"), os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if database == "" || address == "" {
		t.Skip("set PostgreSQL and Temporal service-test addresses")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	ids := domain.NewRandomIDGen()
	schema := "run_coordinator_" + ids.NewID("")
	config, err := pgxpool.ParseConfig(database)
	require.NoError(t, err)
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Exec(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	defer func() {
		_, err := pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		require.NoError(t, err)
	}()
	require.NoError(t, pg.Migrate(ctx, pool))
	store := pg.NewDefaultWorkspaceStore(pool, ids, payloadOutcomeClock{})
	c, err := client.Dial(client.Options{HostPort: address})
	require.NoError(t, err)
	defer c.Close()
	queue := "run-coordinator-" + ids.NewID("")
	evaluator := &coordinatorSequentialTestEvaluator{commands: map[int]int{}, quiescent: make(chan struct{})}
	probe := &coordinatorOracleModel{releaseSecond: make(chan struct{})}
	faults := &coordinatorRecoveryFaults{committed: make(chan coordinatorPumpCall, 1), beforeWait: make(chan coordinatorPumpCall, 1),
		secondCommit: make(chan coordinatorPumpCall, 1), parked: make(chan coordinatorPumpCall, 1), releaseWait: make(chan struct{})}
	source := storeSource{store: store}
	threadActs := NewActivities(probe, source, source, ids)
	newWorker := func(identity string) worker.Worker {
		w := NewWorkerOnTaskQueue(c, threadActs, queue)
		pumpStore := WorkflowRunStore(store)
		if identity == "before-replacement" {
			pumpStore = &coordinatorLossStore{Store: store, faults: faults}
		}
		acts := &WorkflowRunActivities{Store: pumpStore, Evaluator: evaluator}
		RegisterWorkflowRunCoordinator(w, acts)
		w.RegisterWorkflowWithOptions(func(wctx workflow.Context, in WorkflowRunWorkflowInput) error {
			return workflowRunWorkflow(wctx, in, 1, 250*time.Millisecond)
		}, workflow.RegisterOptions{Name: WorkflowRunWorkflowType, DisableAlreadyRegisteredCheck: true})
		w.RegisterActivityWithOptions(func(actx context.Context, in WorkflowRunPumpInput) (WorkflowRunPumpResult, error) {
			info := activity.GetInfo(actx)
			call := coordinatorPumpCall{identity: identity, input: in, activityID: info.ActivityID, runID: info.WorkflowExecution.RunID, attempt: info.Attempt}
			faults.mu.Lock()
			faults.calls = append(faults.calls, call)
			faults.mu.Unlock()
			out, err := acts.Pump(actx, in)
			call.result = out
			if identity == "before-replacement" && faults.decisionLost.Load() {
				faults.commitNotice.Do(func() { faults.committed <- call })
			}
			if err == nil && out.Waiting && in.Cursor == 1 && faults.holdWait.CompareAndSwap(false, true) {
				faults.beforeWait <- call
				select {
				case <-faults.releaseWait:
				case <-actx.Done():
					return WorkflowRunPumpResult{}, actx.Err()
				}
			}
			if err == nil && !out.Waiting && in.Cursor == 1 {
				faults.secondNotice.Do(func() { faults.secondCommit <- call })
			}
			if err == nil && out.Waiting && in.Cursor == 2 {
				faults.parkNotice.Do(func() { faults.parked <- call })
			}
			return out, err
		}, activity.RegisterOptions{Name: ActivityWorkflowRunPump, DisableAlreadyRegisteredCheck: true})
		w.RegisterActivityWithOptions(func(actx context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
			faults.completions.Add(1)
			out, err := threadActs.CompleteWorkflowTurn(actx, in)
			if err == nil && in.IsChild && faults.childAckLost.CompareAndSwap(false, true) {
				return RunTurnResult{}, errors.New("injected owned receipt ACK loss")
			}
			return out, err
		}, activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn, DisableAlreadyRegisteredCheck: true})
		return w
	}
	w := newWorker("before-replacement")
	require.NoError(t, w.Start())
	defer func() { w.Stop() }()
	now := time.Now().UTC()
	session := domain.Session{ID: ids.NewID(domain.PrefixSession), AgentID: "primary", AgentVersion: 1,
		EnvironmentID: "env_coordinator_proof", Status: domain.StatusIdle, Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now,
		AgentSnapshot:    domain.Agent{ID: "primary", Version: 1, Model: domain.Model{ID: "must-not-run"}},
		MultiagentRoster: []domain.Agent{{ID: "reviewer", Version: 1, Name: "reviewer", Model: domain.Model{ID: "coordinator-oracle"}}},
	}
	_, err = store.CreateSession(ctx, session, nil)
	require.NoError(t, err)
	run, err := store.AdmitWorkflowRun(ctx, session.ID, "coordinator-test", pg.WorkflowRunRequest{
		Source: "PRIVATE_PROGRAM_SOURCE_COORDINATOR_SENTINEL", RuntimeProfile: "test-only/sequential-protocol",
		Input: []byte(`{}`), Deadline: now.Add(10 * time.Minute), MaxInvocations: 2, MaxResultBytes: 1024,
	})
	require.NoError(t, err)
	id := workflowRunWorkflowID(run.ID)
	defer func() {
		threadIDs, err := store.ListChildSessionThreadIDs(context.Background(), session.ID)
		require.NoError(t, err)
		workflowIDs := []string{id}
		for _, thread := range threadIDs {
			workflowIDs = append(workflowIDs, sessionThreadWorkflowID(thread))
		}
		for _, workflowID := range workflowIDs {
			err := c.TerminateWorkflow(context.Background(), workflowID, "", "internal coordinator proof cleanup")
			var missing *serviceerror.NotFound
			if !errors.As(err, &missing) {
				require.NoError(t, err)
			}
		}
	}()
	signaler := NewSignalerOnTaskQueue(c, queue)
	deliverer := &coordinatorSignalLoss{Signaler: signaler}
	runRelay := NewWorkflowRunRelay(store, deliverer, RelayConfig{})
	threadRelay := NewRelay(store, signaler, RelayConfig{})
	n, err := runRelay.RunOnce(ctx)
	require.ErrorContains(t, err, "ACK lost")
	require.Zero(t, n)
	first := threadRecoveryReceive(t, ctx, faults.committed)
	var firstAttempt, firstThread string
	require.NoError(t, pool.QueryRow(ctx, `SELECT attempt.id,attempt.thread_id FROM workflow_invocations invocation JOIN workflow_invocation_attempts attempt ON attempt.invocation_id=invocation.id WHERE invocation.run_id=$1 AND invocation.ordinal=0`, run.ID).Scan(&firstAttempt, &firstThread))
	w.Stop()
	worker.PurgeStickyWorkflowCache()
	w = newWorker("after-replacement")
	require.NoError(t, w.Start())
	beforeWait := threadRecoveryReceive(t, ctx, faults.beforeWait)
	require.NotEqual(t, first.runID, beforeWait.runID, "actual Continue-As-New must replace Temporal Run ID")
	require.EqualValues(t, 2, beforeWait.result.Epoch)
	faults.mu.Lock()
	var replay *coordinatorPumpCall
	for i := range faults.calls {
		call := &faults.calls[i]
		if call.identity == "after-replacement" && call.input.Cursor == 0 {
			replay = call
			break
		}
	}
	require.NotNil(t, replay)
	require.Equal(t, first.input, replay.input)
	require.Equal(t, first.activityID, replay.activityID)
	require.Equal(t, first.runID, replay.runID)
	require.Greater(t, replay.attempt, first.attempt)
	faults.mu.Unlock()
	_, err = (&WorkflowRunActivities{Store: store, Evaluator: evaluator}).Pump(ctx, first.input)
	var fenced *temporal.ApplicationError
	require.ErrorAs(t, err, &fenced)
	require.True(t, fenced.NonRetryable(), "the old evaluator must not read/advance after ownership rotated")
	_, err = threadRelay.RunOnce(ctx)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		attempt, err := store.GetWorkflowInvocationAttempt(ctx, session.ID, firstAttempt)
		return err == nil && attempt.Receipt != nil
	}, 20*time.Second, 25*time.Millisecond)
	n, err = runRelay.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n, "new receipt wake accepted before the old waiting Activity returns")
	wakes, err := store.WorkflowRunWakeups(ctx)
	require.NoError(t, err)
	require.Empty(t, wakes)
	close(faults.releaseWait)
	second := threadRecoveryReceive(t, ctx, faults.secondCommit)
	require.Equal(t, beforeWait.runID, second.runID, "buffered signal wins before timer/wait or another history rollover")
	parked := threadRecoveryReceive(t, ctx, faults.parked)
	require.NotEqual(t, second.runID, parked.runID)
	firstWaitHistory := coordinatorHistory(t, ctx, c, id, beforeWait.runID)
	require.Zero(t, coordinatorEventCount(firstWaitHistory, enumspb.EVENT_TYPE_TIMER_STARTED), "signal-before-wait must not rely on a reconciliation timer")
	_, err = threadRelay.RunOnce(ctx)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return coordinatorEventCount(coordinatorHistory(t, ctx, c, id, parked.runID), enumspb.EVENT_TYPE_TIMER_STARTED) > 0
	}, 10*time.Second, 25*time.Millisecond)
	// Withhold the Run relay entirely. The second receipt and its outbox row
	// commit, but no hint is delivered. Actual Temporal timer reconciliation must
	// observe it; this does not fake a successful signal or erase the durable wake.
	close(probe.releaseSecond)
	threadRecoveryReceive(t, ctx, evaluator.quiescent)
	wakes, err = store.WorkflowRunWakeups(ctx)
	require.NoError(t, err)
	require.Len(t, wakes, 1)
	require.EqualValues(t, 5, wakes[0].Version)
	lastHistory := coordinatorHistory(t, ctx, c, id, parked.runID)
	require.Positive(t, coordinatorEventCount(lastHistory, enumspb.EVENT_TYPE_TIMER_FIRED))
	require.Zero(t, coordinatorEventCount(lastHistory, enumspb.EVENT_TYPE_WORKFLOW_EXECUTION_SIGNALED))
	n, err = runRelay.RunOnce(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Eventually(t, func() bool {
		state, err := c.DescribeWorkflowExecution(ctx, id, "")
		if err != nil || len(state.PendingActivities) != 0 {
			return false
		}
		rows, err := pool.Query(ctx, `SELECT thread_id FROM workflow_invocation_attempts WHERE run_id=$1`, run.ID)
		if err != nil {
			return false
		}
		defer rows.Close()
		for rows.Next() {
			var threadID string
			if rows.Scan(&threadID) != nil {
				return false
			}
			state, err := c.DescribeWorkflowExecution(ctx, sessionThreadWorkflowID(threadID), "")
			if err != nil || len(state.PendingActivities) != 0 {
				return false
			}
		}
		return rows.Err() == nil && faults.completions.Load() >= 3
	}, 20*time.Second, 25*time.Millisecond)
	require.EqualValues(t, 2, probe.calls.Load())
	evaluator.mu.Lock()
	require.Equal(t, map[int]int{0: 1, 1: 1}, evaluator.commands, "SQL ACK loss must recover a decision without evaluating/admitting again")
	evaluator.mu.Unlock()
	var version, cursor, epoch, decisions, claims int64
	var state string
	require.NoError(t, pool.QueryRow(ctx, `SELECT state,version,journal_cursor,evaluator_epoch FROM workflow_runs WHERE id=$1`, run.ID).Scan(&state, &version, &cursor, &epoch))
	require.Equal(t, "open", state, "test quiescence is not product Run completion")
	require.EqualValues(t, 5, version)
	require.EqualValues(t, 2, cursor)
	require.EqualValues(t, 3, epoch)
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM workflow_pump_decisions WHERE run_id=$1`, run.ID).Scan(&decisions))
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM workflow_evaluator_claims WHERE run_id=$1`, run.ID).Scan(&claims))
	require.EqualValues(t, 2, decisions)
	require.EqualValues(t, 3, claims)
	rows, err := pool.Query(ctx, `SELECT invocation.ordinal,attempt.id,attempt.thread_id,attempt.receipt,attempt.terminal_event_id,attempt.attempt_no FROM workflow_invocations invocation JOIN workflow_invocation_attempts attempt ON attempt.invocation_id=invocation.id WHERE invocation.run_id=$1 ORDER BY invocation.ordinal`, run.ID)
	require.NoError(t, err)
	count := 0
	for rows.Next() {
		var ordinal, attemptNo int
		var attemptID, threadID, terminal string
		var encoded []byte
		require.NoError(t, rows.Scan(&ordinal, &attemptID, &threadID, &encoded, &terminal, &attemptNo))
		require.Equal(t, count, ordinal)
		require.Equal(t, 1, attemptNo)
		if count == 0 {
			require.Equal(t, firstAttempt, attemptID)
			require.Equal(t, firstThread, threadID)
		}
		var receipt pg.WorkflowInvocationReceipt
		require.NoError(t, json.Unmarshal(encoded, &receipt))
		require.Equal(t, pg.WorkflowInvocationReceipt{AdapterVersion: 1, Type: "text", Text: []string{"alpha-result", "beta-result"}[count]}, receipt)
		event, err := store.GetEvent(ctx, session.ID, terminal)
		require.NoError(t, err)
		require.Equal(t, threadID, event.ThreadID)
		require.Equal(t, domain.EvAgentMessage, event.Type)
		count++
	}
	require.NoError(t, rows.Err())
	rows.Close()
	require.Equal(t, 2, count)
	got, err := store.GetSession(ctx, session.ID)
	require.NoError(t, err)
	require.EqualValues(t, 14, got.Usage.InputTokens)
	require.EqualValues(t, 8, got.Usage.OutputTokens)
	for _, generation := range []string{first.runID, beforeWait.runID, parked.runID} {
		coordinatorAssertSmallHistory(t, coordinatorHistory(t, ctx, c, id, generation))
	}
	// A terminated coordinator whose history is retained rejects a later start.
	// This is not a durable fence after history expiration, terminal Run delivery
	// or a product stop/resume implementation.
	require.NoError(t, c.TerminateWorkflow(ctx, id, "", "test explicit reconciliation boundary"))
	require.NoError(t, store.SetWorkflowRunPause(ctx, session.ID, run.ID, "capability", true))
	n, err = runRelay.RunOnce(ctx)
	require.Error(t, err)
	require.Zero(t, n)
	wakes, err = store.WorkflowRunWakeups(ctx)
	require.NoError(t, err)
	require.Len(t, wakes, 1)
	require.EqualValues(t, 6, wakes[0].Version)
	closed, err := c.DescribeWorkflowExecution(ctx, id, "")
	require.NoError(t, err)
	require.Equal(t, enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED, closed.WorkflowExecutionInfo.Status)
	require.Equal(t, parked.runID, closed.WorkflowExecutionInfo.Execution.RunId)
}

type coordinatorPumpCall struct {
	identity, activityID, runID string
	attempt                     int32
	input                       WorkflowRunPumpInput
	result                      WorkflowRunPumpResult
}

type coordinatorRecoveryFaults struct {
	mu                                              sync.Mutex
	calls                                           []coordinatorPumpCall
	claimLost, decisionLost, holdWait, childAckLost atomic.Bool
	completions                                     atomic.Int64
	commitNotice, secondNotice, parkNotice          sync.Once
	committed, beforeWait, secondCommit, parked     chan coordinatorPumpCall
	releaseWait                                     chan struct{}
}

type coordinatorLossStore struct {
	*pg.Store
	faults *coordinatorRecoveryFaults
}

func (s *coordinatorLossStore) ClaimWorkflowEvaluator(ctx context.Context, sessionID, runID, claim string, previous int64) (pg.WorkflowEvaluatorClaim, error) {
	out, err := s.Store.ClaimWorkflowEvaluator(ctx, sessionID, runID, claim, previous)
	if err == nil && claim == "pump/0" && s.faults.claimLost.CompareAndSwap(false, true) {
		return pg.WorkflowEvaluatorClaim{}, errors.New("injected claim SQL ACK loss")
	}
	return out, err
}

func (s *coordinatorLossStore) CommitWorkflowPump(ctx context.Context, sessionID, runID string, in pg.WorkflowPumpRequest) (pg.WorkflowPumpDecision, error) {
	out, err := s.Store.CommitWorkflowPump(ctx, sessionID, runID, in)
	if err == nil && in.ExpectedCursor == 0 {
		s.faults.decisionLost.Store(true)
		return pg.WorkflowPumpDecision{}, errors.New("injected decision SQL ACK loss")
	}
	return out, err
}

type coordinatorSignalLoss struct {
	*Signaler
	lost atomic.Bool
}

func (s *coordinatorSignalLoss) WakeWorkflowRun(ctx context.Context, wake pg.WorkflowRunWakeup) error {
	err := s.Signaler.WakeWorkflowRun(ctx, wake)
	if err == nil && s.lost.CompareAndSwap(false, true) {
		return errors.New("injected signal ACK lost")
	}
	return err
}

type coordinatorSequentialTestEvaluator struct {
	mu        sync.Mutex
	commands  map[int]int
	quiescent chan struct{}
	done      sync.Once
}

func (e *coordinatorSequentialTestEvaluator) Next(_ context.Context, in pg.WorkflowPumpSnapshot) (*pg.WorkflowPumpCommand, error) {
	if in.Request.RuntimeProfile != "test-only/sequential-protocol" {
		return nil, domain.Unsupported("test evaluator only")
	}
	ordinal := len(in.Journal)
	if ordinal > 2 {
		return nil, domain.Conflict("unexpected program prefix")
	}
	for i, entry := range in.Journal {
		if entry.Decision.Type != "invocation" {
			return nil, domain.Conflict("unexpected rejected command")
		}
		if entry.Receipt == nil {
			return nil, nil
		}
		if entry.Receipt.Type != "text" || entry.Receipt.Text != []string{"alpha-result", "beta-result"}[i] {
			return nil, domain.Conflict("owned result violated test protocol")
		}
	}
	if ordinal == 2 {
		e.done.Do(func() { close(e.quiescent) })
		return nil, nil
	}
	e.mu.Lock()
	e.commands[ordinal]++
	e.mu.Unlock()
	prompt := "coordinator-first-prompt"
	if ordinal == 1 {
		prompt = "coordinator-second-prompt:" + in.Journal[0].Receipt.Text
	}
	return &pg.WorkflowPumpCommand{Kind: "invoke", AgentName: "reviewer", Prompt: prompt}, nil
}

type coordinatorOracleModel struct {
	calls         atomic.Int64
	releaseSecond chan struct{}
}

func (m *coordinatorOracleModel) CreateMessage(ctx context.Context, request model.Request) (model.Response, error) {
	if request.Model != "coordinator-oracle" {
		return model.Response{}, errors.New("primary must not run")
	}
	index := m.calls.Add(1) - 1
	if index > 1 {
		return model.Response{}, errors.New("repeated inference")
	}
	expected := []string{"coordinator-first-prompt", "coordinator-second-prompt:alpha-result"}[index]
	found := false
	for _, message := range request.Messages {
		for _, block := range message.Content {
			found = found || strings.Contains(block.Text, expected)
		}
	}
	if !found {
		return model.Response{}, errors.New("actual owning Thread lost the independently expected input")
	}
	if index == 1 {
		select {
		case <-m.releaseSecond:
		case <-ctx.Done():
			return model.Response{}, ctx.Err()
		}
	}
	return model.Response{StopReason: "end_turn", Content: []domain.ContentBlock{{Type: "text", Text: []string{"alpha-result", "beta-result"}[index]}}, Usage: domain.TokenUsage{InputTokens: 7, OutputTokens: 4}}, nil
}

func (m *coordinatorOracleModel) CreateMessageStream(ctx context.Context, request model.Request, _ func(int, string)) (model.Response, error) {
	return m.CreateMessage(ctx, request)
}

func coordinatorHistory(t *testing.T, ctx context.Context, c client.Client, id, runID string) []*historypb.HistoryEvent {
	t.Helper()
	var events []*historypb.HistoryEvent
	iter := c.GetWorkflowHistory(ctx, id, runID, false, enumspb.HISTORY_EVENT_FILTER_TYPE_ALL_EVENT)
	for iter.HasNext() {
		event, err := iter.Next()
		require.NoError(t, err)
		events = append(events, event)
	}
	return events
}

func coordinatorEventCount(events []*historypb.HistoryEvent, kind enumspb.EventType) int {
	count := 0
	for _, event := range events {
		if event.EventType == kind {
			count++
		}
	}
	return count
}

func coordinatorAssertSmallHistory(t *testing.T, events []*historypb.HistoryEvent) {
	t.Helper()
	for _, event := range events {
		batches := []*commonpb.Payloads{
			event.GetWorkflowExecutionStartedEventAttributes().GetInput(),
			event.GetWorkflowExecutionContinuedAsNewEventAttributes().GetInput(),
			event.GetActivityTaskScheduledEventAttributes().GetInput(),
			event.GetActivityTaskCompletedEventAttributes().GetResult(),
			event.GetWorkflowExecutionSignaledEventAttributes().GetInput(),
		}
		for _, batch := range batches {
			for _, payload := range batch.GetPayloads() {
				require.Less(t, len(payload.Data), 1024, "coordinator history carries references only")
				for _, marker := range []string{"PRIVATE_PROGRAM_SOURCE_COORDINATOR_SENTINEL", "coordinator-first-prompt", "coordinator-second-prompt", "alpha-result", "beta-result"} {
					require.NotContains(t, string(payload.Data), marker)
				}
			}
		}
	}
}
