package temporal

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

func registerNoThreadInterrupt(env *testsuite.TestWorkflowEnvironment) {
	registerBudgetTestActivities(env)
	env.RegisterActivityWithOptions(
		func(context.Context, LoadInterruptInput) (LoadInterruptResult, error) {
			return LoadInterruptResult{}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadInterrupt},
	)
}

func TestSessionThreadWorkflow_AcknowledgesIdleInterruptOnOwningThread(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	const (
		sessionID   = "sesn_child_idle_interrupt"
		threadID    = "sthr_child_idle_interrupt"
		interruptID = "sevt_child_idle_interrupt"
	)
	env.RegisterActivityWithOptions(
		func(context.Context, LoadPendingActionsInput) (LoadPendingActionsResult, error) {
			return LoadPendingActionsResult{}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadPendingActions},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in LoadEventsInput) (LoadEventsResult, error) {
			require.Equal(t, sessionID, in.SessionID)
			require.Equal(t, threadID, in.ThreadID)
			if in.Cursor < 1 {
				return LoadEventsResult{Events: []EventRef{{
					ID: interruptID, Seq: 1, Type: domain.EvUserInterrupt,
				}}}, nil
			}
			return LoadEventsResult{}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadEvents},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
			require.Equal(t, sessionID, in.SessionID)
			require.Equal(t, threadID, in.ThreadID)
			require.True(t, in.IsChild)
			require.Equal(t, interruptID, in.TriggerEventID)
			require.Equal(t, domain.StatusIdle, in.Status)
			return RunTurnResult{Disposition: TurnCompleted}, nil
		},
		activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn},
	)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 1})
	}, time.Millisecond)
	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(
		sessionThreadWorkflow,
		SessionThreadWorkflowInput{SessionID: sessionID, ThreadID: threadID},
		0,
	)

	var canErr *workflow.ContinueAsNewError
	require.ErrorAs(t, env.GetWorkflowError(), &canErr)
	require.Equal(t, SessionThreadWorkflowType, canErr.WorkflowType.Name)
}

func TestSessionThreadWorkflowExitsWhenProjectionWasExternallyTerminated(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(
		func(context.Context, LoadPendingActionsInput) (LoadPendingActionsResult, error) {
			return LoadPendingActionsResult{}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadPendingActions},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, LoadEventsInput) (LoadEventsResult, error) {
			return LoadEventsResult{Events: []EventRef{{
				ID: "sevt_thread_terminated", Seq: 1,
				Type: domain.EvSessionThreadStatusTerminated,
			}}}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadEvents},
	)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 1})
	}, time.Millisecond)
	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(
		sessionThreadWorkflow,
		SessionThreadWorkflowInput{SessionID: "sesn_failed", ThreadID: "sthr_failed"},
		100,
	)
	require.NoError(t, env.GetWorkflowError())
}

func TestSessionThreadWorkflowOwnsIndependentCursorAndTurn(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	registerNoThreadInterrupt(env)
	const (
		sessionID = "sesn_child_workflow"
		threadID  = "sthr_child_workflow"
	)
	events := []EventRef{{
		ID: "sevt_child_message", Seq: 7,
		Type: domain.EvAgentThreadMessageReceived,
	}}
	env.RegisterActivityWithOptions(
		func(_ context.Context, in LoadPendingActionsInput) (LoadPendingActionsResult, error) {
			require.Equal(t, sessionID, in.SessionID)
			require.Equal(t, threadID, in.ThreadID)
			return LoadPendingActionsResult{}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadPendingActions},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in LoadEventsInput) (LoadEventsResult, error) {
			require.Equal(t, sessionID, in.SessionID)
			require.Equal(t, threadID, in.ThreadID)
			var loaded []EventRef
			for _, event := range events {
				if event.Seq > in.Cursor {
					loaded = append(loaded, event)
				}
			}
			return LoadEventsResult{Events: loaded}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadEvents},
	)
	var prepareMu sync.Mutex
	var prepared []PrepareTurnInput
	env.RegisterActivityWithOptions(
		func(_ context.Context, in PrepareTurnInput) (PrepareTurnResult, error) {
			prepareMu.Lock()
			prepared = append(prepared, in)
			prepareMu.Unlock()
			return PrepareTurnResult{
				ThreadID: threadID, IsChild: true,
				Request: model.Request{Model: "fake"},
			}, nil
		},
		activity.RegisterOptions{Name: ActivityPrepareTurn},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, StartModelRequestInput) error { return nil },
		activity.RegisterOptions{Name: ActivityStartModelRequest},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, AppendWorkflowEventsInput) error { return nil },
		activity.RegisterOptions{Name: ActivityAppendWorkflowEvents},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in CallModelInput) (CallModelResult, error) {
			require.Equal(t, threadID, in.ThreadID)
			return CallModelResult{
				Response:            model.Response{StopReason: "end_turn"},
				ModelRequestStartID: in.ModelRequestStartID,
				ModelRequestEndID:   in.ModelRequestEndID,
			}, nil
		},
		activity.RegisterOptions{Name: ActivityCallModel},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
			require.Equal(t, sessionID, in.SessionID)
			require.Equal(t, threadID, in.ThreadID)
			require.True(t, in.IsChild)
			require.Equal(t, events[0].ID, in.TriggerEventID)
			return RunTurnResult{Disposition: TurnCompleted}, nil
		},
		activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn},
	)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 7})
	}, time.Millisecond)

	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(
		sessionThreadWorkflow,
		SessionThreadWorkflowInput{SessionID: sessionID, ThreadID: threadID},
		1,
	)

	var canErr *workflow.ContinueAsNewError
	require.ErrorAs(t, env.GetWorkflowError(), &canErr)
	require.Equal(t, SessionThreadWorkflowType, canErr.WorkflowType.Name)
	prepareMu.Lock()
	defer prepareMu.Unlock()
	require.Equal(t, []PrepareTurnInput{{
		SessionID: sessionID, TriggerEventID: events[0].ID,
	}}, prepared)
}

func TestSessionThreadWorkflowStopsAfterTerminalChildTurn(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	registerNoThreadInterrupt(env)
	events := []EventRef{
		{ID: "sevt_child_first", Seq: 1, Type: domain.EvAgentThreadMessageReceived},
		{ID: "sevt_child_second", Seq: 2, Type: domain.EvAgentThreadMessageReceived},
	}
	env.RegisterActivityWithOptions(
		func(context.Context, LoadPendingActionsInput) (LoadPendingActionsResult, error) {
			return LoadPendingActionsResult{}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadPendingActions},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in LoadEventsInput) (LoadEventsResult, error) {
			return LoadEventsResult{Events: events}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadEvents},
	)
	var prepared []string
	env.RegisterActivityWithOptions(
		func(_ context.Context, in PrepareTurnInput) (PrepareTurnResult, error) {
			prepared = append(prepared, in.TriggerEventID)
			return PrepareTurnResult{
				ThreadID: "sthr_child", IsChild: true,
				AlreadyCompleted: true, Terminated: true,
			}, nil
		},
		activity.RegisterOptions{Name: ActivityPrepareTurn},
	)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 2})
	}, time.Millisecond)
	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(SessionThreadWorkflow, SessionThreadWorkflowInput{
		SessionID: "sesn_child", ThreadID: "sthr_child",
	})

	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{events[0].ID}, prepared)
}

func TestSessionThreadWorkflowResumesBarrierBeforeQueuedMessage(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	registerNoThreadInterrupt(env)
	const (
		sessionID    = "sesn_child_barrier"
		threadID     = "sthr_child_barrier"
		resolutionID = "sevt_child_confirmation"
		queuedID     = "sevt_child_followup"
	)
	loadPendingCalls := 0
	env.RegisterActivityWithOptions(
		func(_ context.Context, in LoadPendingActionsInput) (LoadPendingActionsResult, error) {
			require.Equal(t, threadID, in.ThreadID)
			loadPendingCalls++
			if loadPendingCalls == 1 {
				return LoadPendingActionsResult{Actions: []PendingActionRef{{
					ActionEventID: "sevt_child_tool", ActionEventSeq: 7,
					Kind:              domain.PendingToolConfirmation,
					ResolutionEventID: resolutionID, ResolutionEventSeq: 12,
				}}}, nil
			}
			return LoadPendingActionsResult{}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadPendingActions},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in LoadEventsInput) (LoadEventsResult, error) {
			if in.Cursor < 8 {
				return LoadEventsResult{Events: []EventRef{{
					ID: queuedID, Seq: 8, Type: domain.EvAgentThreadMessageReceived,
				}}}, nil
			}
			return LoadEventsResult{}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadEvents},
	)
	var prepared []PrepareTurnInput
	env.RegisterActivityWithOptions(
		func(_ context.Context, in PrepareTurnInput) (PrepareTurnResult, error) {
			prepared = append(prepared, in)
			return PrepareTurnResult{
				AlreadyCompleted: true,
				Terminated:       in.TriggerEventID == queuedID,
			}, nil
		},
		activity.RegisterOptions{Name: ActivityPrepareTurn},
	)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 12})
	}, time.Millisecond)
	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(SessionThreadWorkflow, SessionThreadWorkflowInput{
		SessionID: sessionID, ThreadID: threadID,
	})

	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []PrepareTurnInput{
		{
			SessionID: sessionID, TriggerEventID: resolutionID,
			ResolutionEventIDs: []string{resolutionID},
		},
		{SessionID: sessionID, TriggerEventID: queuedID},
	}, prepared)
}

func TestSessionThreadWorkflowStopsDrainingAfterMessageTurnParks(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	registerNoThreadInterrupt(env)
	const (
		sessionID = "sesn_child_new_barrier"
		threadID  = "sthr_child_new_barrier"
		firstID   = "sevt_child_first"
		queuedID  = "sevt_child_queued"
	)
	events := []EventRef{
		{ID: firstID, Seq: 1, Type: domain.EvAgentThreadMessageReceived},
		{ID: queuedID, Seq: 2, Type: domain.EvAgentThreadMessageReceived},
	}
	env.RegisterActivityWithOptions(
		func(context.Context, LoadPendingActionsInput) (LoadPendingActionsResult, error) {
			return LoadPendingActionsResult{}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadPendingActions},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, LoadEventsInput) (LoadEventsResult, error) {
			return LoadEventsResult{Events: events}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadEvents},
	)
	var prepared []string
	env.RegisterActivityWithOptions(
		func(_ context.Context, in PrepareTurnInput) (PrepareTurnResult, error) {
			prepared = append(prepared, in.TriggerEventID)
			return PrepareTurnResult{
				ThreadID: threadID, IsChild: true,
				Request: model.Request{Model: "fake"},
			}, nil
		},
		activity.RegisterOptions{Name: ActivityPrepareTurn},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, StartModelRequestInput) error { return nil },
		activity.RegisterOptions{Name: ActivityStartModelRequest},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, AppendWorkflowEventsInput) error { return nil },
		activity.RegisterOptions{Name: ActivityAppendWorkflowEvents},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in CallModelInput) (CallModelResult, error) {
			return CallModelResult{
				Response:            model.Response{StopReason: "end_turn"},
				ModelRequestStartID: in.ModelRequestStartID,
				ModelRequestEndID:   in.ModelRequestEndID,
			}, nil
		},
		activity.RegisterOptions{Name: ActivityCallModel},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
			if in.TriggerEventID == firstID {
				return RunTurnResult{Disposition: TurnParked}, nil
			}
			return RunTurnResult{Disposition: TurnTerminated}, nil
		},
		activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn},
	)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 2})
	}, time.Millisecond)

	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(
		sessionThreadWorkflow,
		SessionThreadWorkflowInput{SessionID: sessionID, ThreadID: threadID},
		1,
	)

	var canErr *workflow.ContinueAsNewError
	require.ErrorAs(t, env.GetWorkflowError(), &canErr)
	var next SessionThreadWorkflowInput
	require.NoError(
		t,
		converter.GetDefaultDataConverter().FromPayloads(canErr.Input, &next),
	)
	require.Equal(t, int64(1), next.StartCursor)
	require.Equal(t, []string{firstID}, prepared)
}
