package temporal

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"go.temporal.io/sdk/workflow"

	"github.com/yanpgwang/mango/internal/agentruntime"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
	"github.com/yanpgwang/mango/internal/pg"
)

// This is a recovery experiment on the existing coordinator/Thread path, not a
// Run implementation. There is no substitute scheduler, study schema, JS engine
// or model service. Only inference and post-commit transport failures are faked.
// The Workflow bodies are production code with a one-turn Continue-As-New limit.
func TestWorkflowThreadRecoveryRealTemporal(t *testing.T) {
	database, address := os.Getenv("MANGO_TEST_DATABASE_URL"), os.Getenv("MANGO_TEST_TEMPORAL_HOSTPORT")
	if database == "" || address == "" {
		t.Skip("set PostgreSQL and Temporal service-test addresses")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	ids := domain.NewRandomIDGen()
	schema := "workflow_recovery_" + ids.NewID("")
	config, err := pgxpool.ParseConfig(database)
	require.NoError(t, err)
	config.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, config)
	require.NoError(t, err)
	defer pool.Close()
	_, err = pool.Exec(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err, "explicit service invocation must fail if PostgreSQL is unavailable")
	defer func() {
		_, dropErr := pool.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE")
		require.NoError(t, dropErr)
	}()
	require.NoError(t, pg.Migrate(ctx, pool))
	store := pg.NewDefaultWorkspaceStore(pool, ids, payloadOutcomeClock{})
	connection, err := client.Dial(client.Options{HostPort: address})
	require.NoError(t, err)
	defer connection.Close()
	queue := "workflow-recovery-" + ids.NewID("")
	probe := &threadRecoveryModel{release: [2]chan struct{}{make(chan struct{}), make(chan struct{})}}
	faults := &threadRecoveryFaults{
		delegation: make(chan ExecuteToolInput, 1), rollback: make(chan CompleteWorkflowTurnInput, 1),
		completion: make(chan CompleteWorkflowTurnInput, 1),
	}
	source := storeSource{store: store}
	acts := NewActivities(probe, source, source, ids)
	newWorker := func(identity string) worker.Worker {
		w := NewWorkerOnTaskQueue(connection, acts, queue)
		// Override only thresholds and Activity transport boundaries. Registration
		// names remain those used by the actual outbox Signaler.
		w.RegisterWorkflowWithOptions(func(wctx workflow.Context, in SessionWorkflowInput) error {
			return sessionWorkflow(wctx, in, 1)
		}, workflow.RegisterOptions{Name: "SessionWorkflow", DisableAlreadyRegisteredCheck: true})
		w.RegisterWorkflowWithOptions(func(wctx workflow.Context, in SessionThreadWorkflowInput) error {
			return sessionThreadWorkflow(wctx, in, 1)
		}, workflow.RegisterOptions{Name: SessionThreadWorkflowType, DisableAlreadyRegisteredCheck: true})
		w.RegisterActivityWithOptions(func(actx context.Context, input ExecuteToolInput) (ExecuteToolResult, error) {
			info := activity.GetInfo(actx)
			out, executeErr := acts.ExecuteTool(actx, input)
			if input.ToolName == agentruntime.SendToAgentToolName && executeErr == nil {
				faults.mu.Lock()
				faults.calls = append(faults.calls, threadRecoveryCall{identity, info.ActivityID, info.WorkflowExecution.RunID, info.Attempt, input})
				if identity == "before-replacement" && faults.spawnStep == "" {
					faults.spawnStep = input.ToolStepID
					faults.delegation <- input
				}
				loseReply := identity == "before-replacement" && input.ToolStepID == faults.spawnStep
				faults.mu.Unlock()
				// Keep retries on the old worker unacknowledged so replacement
				// does not depend on beating an Activity retry timer.
				if loseReply {
					return ExecuteToolResult{}, errors.New("injected acknowledgement loss after delegation commit")
				}
			}
			return out, executeErr
		}, activity.RegisterOptions{Name: ActivityExecuteTool, DisableAlreadyRegisteredCheck: true})
		w.RegisterActivityWithOptions(func(actx context.Context, input CompleteWorkflowTurnInput) (RunTurnResult, error) {
			out, completeErr := acts.CompleteWorkflowTurn(actx, input)
			if input.IsChild {
				info := activity.GetInfo(actx)
				faults.mu.Lock()
				faults.completions = append(faults.completions, threadRecoveryCompletion{info.ActivityID, info.Attempt, input})
				faults.mu.Unlock()
				if completeErr != nil && faults.rollbackSeen.CompareAndSwap(false, true) {
					faults.rollback <- input
				}
				if completeErr == nil && faults.resultLost.CompareAndSwap(false, true) {
					faults.completion <- input
					return RunTurnResult{}, errors.New("injected acknowledgement loss after child completion commit")
				}
			}
			return out, completeErr
		}, activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn, DisableAlreadyRegisteredCheck: true})
		return w
	}
	w := newWorker("before-replacement")
	require.NoError(t, w.Start())
	defer func() { w.Stop() }()
	now := time.Now().UTC()
	session := domain.Session{
		ID: ids.NewID(domain.PrefixSession), AgentID: "agent_recovery", AgentVersion: 1,
		EnvironmentID: "env_recovery", Status: domain.StatusIdle, Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
		AgentSnapshot: domain.Agent{ID: "agent_recovery", Version: 1, Name: "coordinator",
			Model: domain.Model{ID: "recovery-primary"}, Multiagent: &domain.Multiagent{
				Type: "coordinator", Agents: []domain.AgentReference{{Type: "agent", ID: "agent_reviewer", Version: 1}},
			}},
		MultiagentRoster: []domain.Agent{{ID: "agent_reviewer", Version: 1, Name: "reviewer", Model: domain.Model{ID: "recovery-child"}}},
	}
	_, err = store.CreateSession(ctx, session, nil)
	require.NoError(t, err)
	defer func() {
		threadIDs, listErr := store.ListChildSessionThreadIDs(context.Background(), session.ID)
		require.NoError(t, listErr)
		for _, id := range append(threadIDs, "") {
			workflowID := session.ID
			if id != "" {
				workflowID = sessionThreadWorkflowID(id)
			}
			terminateErr := connection.TerminateWorkflow(context.Background(), workflowID, "", "recovery test cleanup")
			var missing *serviceerror.NotFound
			if !errors.As(terminateErr, &missing) {
				require.NoError(t, terminateErr)
			}
		}
	}()
	_, err = store.AdmitEvents(ctx, session.ID, []domain.EventDraft{threadRecoveryMessage("start")})
	require.NoError(t, err)
	signaler := NewSignalerOnTaskQueue(connection, queue)
	// The real relay sees a failed signal acknowledgement after Temporal accepted
	// it. It must retain the actual outbox row, not rely on an API fast path.
	deliverer := &threadRecoveryDeliverer{Signaler: signaler}
	relay := NewRelay(store, deliverer, RelayConfig{})
	removed, err := relay.RunOnce(ctx)
	require.NoError(t, err)
	require.Zero(t, removed)
	_, exists, err := store.PendingWakeup(ctx, session.ID)
	require.NoError(t, err)
	require.True(t, exists)
	firstInput := threadRecoveryReceive(t, ctx, faults.delegation)
	w.Stop()
	worker.PurgeStickyWorkflowCache()
	w = newWorker("after-delegation-loss")
	require.NoError(t, w.Start())
	_, err = relay.RunOnce(ctx)
	require.NoError(t, err)

	var children []domain.SessionThread
	require.Eventually(t, func() bool {
		threads, listErr := store.ListSessionThreads(ctx, session.ID, app.SessionThreadListQuery{Limit: 10})
		if listErr != nil {
			return false
		}
		children = nil
		for _, thread := range threads {
			if thread.ParentThreadID != nil {
				children = append(children, thread)
			}
		}
		_, relayErr := relay.RunOnce(ctx)
		return relayErr == nil && len(children) == 2 && probe.childCalls.Load() == 2 && probe.primaryCalls.Load() >= 2
	}, 20*time.Second, 50*time.Millisecond)
	var steps int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM tool_steps WHERE tool_name=$1", agentruntime.SendToAgentToolName).Scan(&steps))
	require.Equal(t, 2, steps, "identical calls are distinct; retry must not create a third invocation")
	faults.mu.Lock()
	var original, retried *threadRecoveryCall
	for i := range faults.calls {
		call := &faults.calls[i]
		if call.input.ToolStepID == firstInput.ToolStepID {
			if original == nil {
				original = call
			} else if call.worker == "after-delegation-loss" {
				retried = call
			}
		}
	}
	faults.mu.Unlock()
	require.NotNil(t, original)
	require.NotNil(t, retried, "replacement worker must replay the Activity")
	require.Equal(t, original.input, retried.input)
	require.Equal(t, original.activityID, retried.activityID)
	require.Equal(t, original.runID, retried.runID)
	require.Greater(t, retried.attempt, original.attempt)

	// Primary conversation remains usable while children run. A real low-threshold
	// Continue-As-New must carry its cursor without replaying the delegation.
	threadRecoveryWaitContinued(t, ctx, connection, session.ID)
	progressGeneration := threadRecoveryGeneration(t, ctx, connection, session.ID)
	_, err = store.AdmitEvents(ctx, session.ID, []domain.EventDraft{threadRecoveryMessage("progress")})
	require.NoError(t, err)
	_, err = relay.RunOnce(ctx)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return probe.primaryCalls.Load() >= 3 }, 10*time.Second, 25*time.Millisecond)
	events, err := store.EventsAfter(ctx, session.ID, 0, 500)
	require.NoError(t, err)
	for _, event := range events {
		require.NotEqual(t, domain.EvSessionStatusIdle, event.Type, "active children must prevent aggregate end-turn")
	}
	// Wait for the primary turn to commit before injecting a failure into the
	// child's completion. There is no primary outbox consumer during this phase.
	require.Eventually(t, func() bool {
		var unprocessed int
		queryErr := pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE session_id=$1 AND type=$2 AND processed_at IS NULL", session.ID, domain.EvUserMessage).Scan(&unprocessed)
		return queryErr == nil && unprocessed == 0
	}, 10*time.Second, 25*time.Millisecond)
	threadRecoveryWaitGeneration(t, ctx, connection, session.ID, progressGeneration)
	_, err = relay.RunOnce(ctx)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, "ALTER TABLE orchestration_outbox ADD CONSTRAINT recovery_fail_wake CHECK (max_event_seq < 0)")
	require.NoError(t, err)
	close(probe.release[0])
	rolledBack := threadRecoveryReceive(t, ctx, faults.rollback)
	var reports, processed int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE session_id=$1 AND type=$2 AND payload->>'from_session_thread_id'=$3", session.ID, domain.EvAgentThreadMessageReceived, rolledBack.ThreadID).Scan(&reports))
	require.Zero(t, reports, "failed wake insertion must roll back the report")
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE id=$1 AND processed_at IS NOT NULL", rolledBack.TriggerEventID).Scan(&processed))
	require.Zero(t, processed, "failed wake insertion must roll back consumption")
	_, err = pool.Exec(ctx, "ALTER TABLE orchestration_outbox DROP CONSTRAINT recovery_fail_wake")
	require.NoError(t, err)
	committed := threadRecoveryReceive(t, ctx, faults.completion)
	// Observe the real completion transaction with the relay paused: report,
	// processed trigger, transcript and wake must all be durable. The completion
	// Activity may retry while we inspect it; that must be a read-only replay.
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM events WHERE session_id=$1 AND type=$2 AND payload->>'from_session_thread_id'=$3", session.ID, domain.EvAgentThreadMessageReceived, committed.ThreadID).Scan(&reports))
	require.Equal(t, 1, reports)
	_, exists, err = store.PendingWakeup(ctx, session.ID)
	require.NoError(t, err)
	require.True(t, exists, "completion receipt and wake must commit together")
	trigger, err := store.GetEvent(ctx, session.ID, committed.TriggerEventID)
	require.NoError(t, err)
	require.NotNil(t, trigger.ProcessedAt)
	var transcriptCount int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM provider_transcript_turns WHERE trigger_event_id=$1", committed.TriggerEventID).Scan(&transcriptCount))
	require.Equal(t, 1, transcriptCount)
	before, err := store.EventsAfter(ctx, session.ID, 0, 500)
	require.NoError(t, err)
	duplicate, err := threadRecoveryComplete(ctx, store, committed)
	require.NoError(t, err)
	require.False(t, duplicate.Applied)
	wrongOwner := committed
	for _, child := range children {
		if child.ID != committed.ThreadID {
			wrongOwner.ThreadID = child.ID
		}
	}
	_, err = threadRecoveryComplete(ctx, store, wrongOwner)
	var conflict *domain.DomainError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, domain.KindConflict, conflict.Kind)
	after, err := store.EventsAfter(ctx, session.ID, 0, 500)
	require.NoError(t, err)
	require.Equal(t, before, after, "duplicate/wrong-owner completion must not alter committed history")

	reportGeneration := threadRecoveryGeneration(t, ctx, connection, session.ID)
	_, err = relay.RunOnce(ctx)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return probe.primaryCalls.Load() >= 4 }, 10*time.Second, 25*time.Millisecond)
	threadRecoveryWaitGeneration(t, ctx, connection, session.ID, reportGeneration)
	current, err := store.GetSession(ctx, session.ID)
	require.NoError(t, err)
	require.Equal(t, domain.StatusRunning, current.Status, "second child still holds the aggregate barrier")
	close(probe.release[1])
	// Do not deliver the second report yet. Replace the worker only after both
	// child model/turn Activities have durably completed, so this fixture does
	// not confuse unknown in-flight model responses with committed result replay.
	for _, child := range children {
		threadRecoveryWaitContinued(t, ctx, connection, sessionThreadWorkflowID(child.ID))
	}
	w.Stop()
	worker.PurgeStickyWorkflowCache()
	w = newWorker("after-results-before-delivery")
	require.NoError(t, w.Start())
	var finalEvents []domain.Event
	require.Eventually(t, func() bool {
		_, relayErr := relay.RunOnce(ctx)
		if relayErr != nil {
			return false
		}
		current, getErr := store.GetSession(ctx, session.ID)
		finalEvents, err = store.EventsAfter(ctx, session.ID, 0, 500)
		return getErr == nil && err == nil && current.Status == domain.StatusIdle
	}, 20*time.Second, 50*time.Millisecond)
	var lastReport, finalIdle int64
	reports = 0
	for _, event := range finalEvents {
		if event.Type == domain.EvAgentThreadMessageReceived {
			reports++
			lastReport = event.Sequence
		}
		if event.Type == domain.EvSessionStatusIdle {
			finalIdle = event.Sequence
		}
		require.NotEqual(t, domain.EvSessionError, event.Type)
	}
	require.Equal(t, 2, reports)
	require.Greater(t, finalIdle, lastReport, "primary end-turn must follow all reports")
	require.EqualValues(t, 2, probe.childCalls.Load(), "completion retry must not repeat inference")
	require.True(t, probe.sawBoth.Load(), "primary synthesis must receive both committed child reports")
	faults.mu.Lock()
	completions := append([]threadRecoveryCompletion(nil), faults.completions...)
	faults.mu.Unlock()
	var retriedCompletions []threadRecoveryCompletion
	for _, completion := range completions {
		if completion.input.TriggerEventID == committed.TriggerEventID {
			require.Equal(t, committed, completion.input)
			retriedCompletions = append(retriedCompletions, completion)
		}
	}
	require.GreaterOrEqual(t, len(retriedCompletions), 3, "rollback, post-commit ACK loss and committed replay must execute")
	for _, completion := range retriedCompletions[1:] {
		require.Equal(t, retriedCompletions[0].activityID, completion.activityID)
		require.Greater(t, completion.attempt, retriedCompletions[0].attempt)
	}
	for _, child := range children {
		threadRecoveryWaitContinued(t, ctx, connection, sessionThreadWorkflowID(child.ID))
		thread, getErr := store.GetSessionThread(ctx, session.ID, child.ID)
		require.NoError(t, getErr)
		require.EqualValues(t, 7, thread.Usage.InputTokens)
		require.EqualValues(t, 3, thread.Usage.OutputTokens)
	}
	_, err = store.ArchiveSessionThread(ctx, session.ID, committed.ThreadID)
	require.NoError(t, err)
	before, err = store.EventsAfter(ctx, session.ID, 0, 500)
	require.NoError(t, err)
	late, err := threadRecoveryComplete(ctx, store, committed)
	require.NoError(t, err)
	require.False(t, late.Applied)
	require.Equal(t, domain.StatusTerminated, late.ThreadStatus)
	after, err = store.EventsAfter(ctx, session.ID, 0, 500)
	require.NoError(t, err)
	require.Equal(t, before, after, "late completion cannot reopen an archived owner")
}

type threadRecoveryCall struct {
	worker, activityID, runID string
	attempt                   int32
	input                     ExecuteToolInput
}

type threadRecoveryFaults struct {
	spawnStep                string
	resultLost, rollbackSeen atomic.Bool
	delegation               chan ExecuteToolInput
	rollback, completion     chan CompleteWorkflowTurnInput
	mu                       sync.Mutex
	calls                    []threadRecoveryCall
	completions              []threadRecoveryCompletion
}

type threadRecoveryCompletion struct {
	activityID string
	attempt    int32
	input      CompleteWorkflowTurnInput
}

type threadRecoveryDeliverer struct {
	*Signaler
	lost atomic.Bool
}

func (d *threadRecoveryDeliverer) Wake(ctx context.Context, id string, seq int64) error {
	if err := d.Signaler.Wake(ctx, id, seq); err != nil {
		return err
	}
	if d.lost.CompareAndSwap(false, true) {
		return errors.New("injected signal acknowledgement loss")
	}
	return nil
}

type threadRecoveryModel struct {
	primaryCalls, childCalls atomic.Int64
	sawBoth                  atomic.Bool
	release                  [2]chan struct{}
}

func (m *threadRecoveryModel) CreateMessage(ctx context.Context, request model.Request) (model.Response, error) {
	if request.Model == "recovery-child" {
		index := m.childCalls.Add(1) - 1
		if index >= 2 {
			return model.Response{}, errors.New("unexpected repeated child inference")
		}
		select {
		case <-m.release[index]:
		case <-ctx.Done():
			return model.Response{}, ctx.Err()
		}
		return model.Response{StopReason: "end_turn", Content: []domain.ContentBlock{{Type: "text", Text: fmt.Sprintf("review-%d", index)}}, Usage: domain.TokenUsage{InputTokens: 7, OutputTokens: 3}}, nil
	}
	if request.Model != "recovery-primary" {
		return model.Response{}, errors.New("unexpected model")
	}
	if m.primaryCalls.Add(1) == 1 {
		blocks := make([]domain.ContentBlock, 2)
		for i := range blocks {
			blocks[i] = domain.ContentBlock{Type: "tool_use", ToolUseID: fmt.Sprintf("delegate-%d", i), ToolName: agentruntime.SendToAgentToolName,
				Input: map[string]any{"agent_name": "reviewer", "message": "same review prompt"}}
		}
		return model.Response{StopReason: "tool_use", Content: blocks}, nil
	}
	var history strings.Builder
	for _, message := range request.Messages {
		for _, block := range message.Content {
			history.WriteString(block.Text)
		}
	}
	if strings.Contains(history.String(), "review-0") && strings.Contains(history.String(), "review-1") {
		m.sawBoth.Store(true)
	}
	return model.Response{StopReason: "end_turn", Content: []domain.ContentBlock{{Type: "text", Text: "progress or synthesis"}}}, nil
}

func (m *threadRecoveryModel) CreateMessageStream(ctx context.Context, request model.Request, _ func(int, string)) (model.Response, error) {
	return m.CreateMessage(ctx, request)
}

func threadRecoveryMessage(text string) domain.EventDraft {
	return domain.EventDraft{Type: domain.EvUserMessage, Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}}
}

func threadRecoveryReceive[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatal("timed out waiting for recovery boundary")
		var zero T
		return zero
	}
}

func threadRecoveryComplete(ctx context.Context, store *pg.Store, in CompleteWorkflowTurnInput) (pg.TurnCompletion, error) {
	return store.CompleteThreadWorkflowTurn(ctx, in.SessionID, in.ThreadID, in.TriggerEventID, in.Output, in.Status,
		in.AttemptID, in.AttemptState, in.AttemptError, in.PendingActionEventIDs, in.ResolutionEventIDs, in.TranscriptDelta, in.ToolUseMappings, domain.TokenUsage{})
}

func threadRecoveryWaitContinued(t *testing.T, ctx context.Context, c client.Client, id string) {
	t.Helper()
	run, err := c.DescribeWorkflowExecution(ctx, id, "")
	require.NoError(t, err)
	firstID := run.WorkflowExecutionInfo.FirstRunId
	require.NotEmpty(t, firstID)
	require.Eventually(t, func() bool {
		first, getErr := c.DescribeWorkflowExecution(ctx, id, firstID)
		return getErr == nil && first.WorkflowExecutionInfo.Status == enumspb.WORKFLOW_EXECUTION_STATUS_CONTINUED_AS_NEW
	}, 10*time.Second, 25*time.Millisecond)
}

func threadRecoveryGeneration(t *testing.T, ctx context.Context, c client.Client, id string) string {
	t.Helper()
	run, err := c.DescribeWorkflowExecution(ctx, id, "")
	require.NoError(t, err)
	return run.WorkflowExecutionInfo.Execution.RunId
}

func threadRecoveryWaitGeneration(t *testing.T, ctx context.Context, c client.Client, id, previous string) {
	t.Helper()
	require.Eventually(t, func() bool {
		run, err := c.DescribeWorkflowExecution(ctx, id, "")
		return err == nil && run.WorkflowExecutionInfo.Execution.RunId != previous
	}, 10*time.Second, 25*time.Millisecond, "turn must commit and Continue-As-New before the next delivery")
}
