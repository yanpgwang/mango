package temporal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/model"
)

// fakeSource is an in-memory EventSource for workflow tests. It records how many
// times each trigger's turn was completed so duplicate/gap protection can be
// asserted (a well-behaved workflow completes each user.message turn exactly
// once even under duplicate wakeups).
type fakeSource struct {
	mu               sync.Mutex
	events           []domain.Event
	completes        map[string]int            // triggerEventID -> times CompleteTurn appended output
	byTurn           map[string][]domain.Event // triggerEventID -> committed output events
	pending          map[string][]string       // triggerEventID -> pending action ids forwarded by Activity
	resolved         map[string][]string       // triggerEventID -> barrier resolution ids forwarded by Activity
	pendingActions   []domain.PendingAction
	completionParked *bool
	maxSeq           int64
}

func newFakeSource(events []domain.Event) *fakeSource {
	var max int64
	for _, e := range events {
		if e.Sequence > max {
			max = e.Sequence
		}
	}
	return &fakeSource{
		events: events, completes: map[string]int{}, byTurn: map[string][]domain.Event{},
		pending: map[string][]string{}, resolved: map[string][]string{}, maxSeq: max,
	}
}

func (f *fakeSource) EventsAfter(_ context.Context, _ string, cursor int64, limit int) ([]domain.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []domain.Event{}
	for _, e := range f.events {
		if e.Sequence > cursor {
			out = append(out, e)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (f *fakeSource) FirstUnprocessedInterruptAfter(
	_ context.Context,
	_ string,
	afterSeq int64,
) (*domain.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, event := range f.events {
		if event.Sequence > afterSeq &&
			event.Type == domain.EvUserInterrupt &&
			event.ProcessedAt == nil {
			copy := event
			return &copy, nil
		}
	}
	return nil, nil
}

func (f *fakeSource) HistoryThrough(_ context.Context, _ string, triggerEventID string, _ int) ([]domain.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Find the trigger's sequence, then return every event at or below it. The
	// workflow tests here exercise ordering/dedup, not causal reconstruction, so a
	// simple sequence bound suffices for the fake.
	var triggerSeq int64
	for _, e := range f.events {
		if e.ID == triggerEventID {
			triggerSeq = e.Sequence
		}
	}
	out := []domain.Event{}
	for _, e := range f.events {
		if e.Sequence <= triggerSeq {
			out = append(out, e)
		}
	}
	return out, nil
}

func (f *fakeSource) GetSession(_ context.Context, id string) (domain.Session, error) {
	return domain.Session{ID: id, Status: domain.StatusRunning}, nil
}

func (f *fakeSource) GetEvent(_ context.Context, _ string, id string) (domain.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.events {
		if e.ID == id {
			return e, nil
		}
	}
	return domain.Event{}, domain.NotFound("event not found")
}

func (f *fakeSource) UnresolvedPendingActions(
	_ context.Context,
	_ string,
) ([]domain.PendingAction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.PendingAction(nil), f.pendingActions...), nil
}

func (f *fakeSource) CompleteTurn(_ context.Context, sessionID, triggerEventID string, output []domain.EventDraft, status domain.Status) (TurnCompletionResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Idempotent: if this trigger's turn already committed, replay its output
	// events without appending again — exactly the pg.Store contract.
	if f.completes[triggerEventID] > 0 {
		return TurnCompletionResult{
			Events:  f.byTurn[triggerEventID],
			Applied: false,
			Status:  status,
			Parked:  f.completionParked,
		}, nil
	}
	f.completes[triggerEventID]++
	committed := make([]domain.Event, 0, len(output))
	for _, d := range output {
		f.maxSeq++
		e := domain.Event{ID: d.ID, SessionID: sessionID, Sequence: f.maxSeq, Type: d.Type, Payload: d.Payload}
		if e.ID == "" {
			e.ID = "out_" + itoaTest(f.maxSeq)
		}
		f.events = append(f.events, e)
		committed = append(committed, e)
	}
	f.byTurn[triggerEventID] = committed
	return TurnCompletionResult{
		Events:  committed,
		Applied: true,
		Status:  status,
		Parked:  f.completionParked,
	}, nil
}

func (f *fakeSource) CompleteWorkflowTurn(
	ctx context.Context,
	sessionID string,
	triggerEventID string,
	output []domain.EventDraft,
	status domain.Status,
	_ string,
	_ domain.RunAttemptState,
	_ *string,
	pendingActionEventIDs []string,
	resolutionEventIDs []string,
) (TurnCompletionResult, error) {
	result, err := f.CompleteTurn(ctx, sessionID, triggerEventID, output, status)
	f.mu.Lock()
	f.pending[triggerEventID] = append([]string(nil), pendingActionEventIDs...)
	f.resolved[triggerEventID] = append([]string(nil), resolutionEventIDs...)
	f.mu.Unlock()
	return result, err
}

func (f *fakeSource) completions(triggerID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.completes[triggerID]
}

func itoaTest(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// testIDGen is a trivial deterministic id generator for workflow tests.
type testIDGen struct {
	mu sync.Mutex
	n  int64
}

func (g *testIDGen) NewID(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return prefix + itoaTest(g.n)
}

func userMsg(id string, seq int64) domain.Event {
	return domain.Event{ID: id, Sequence: seq, Type: domain.EvUserMessage, Payload: map[string]any{"content": "hi"}}
}

type turnRecorder struct {
	mu    sync.Mutex
	order []string
}

func (r *turnRecorder) append(triggerEventID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.order = append(r.order, triggerEventID)
}

func (r *turnRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

// registerCurrentTurnActivities wires the granular Workflow-owned turn used in
// production. Tests can override the disposition returned by the completion
// Activity to make the long-lived session workflow reach a terminal boundary.
func registerCurrentTurnActivities(
	env *testsuite.TestWorkflowEnvironment,
	source *fakeSource,
	disposition func(triggerEventID string) TurnDisposition,
) *turnRecorder {
	acts := NewActivities(nil, source, nil, nil, &testIDGen{})
	recorder := &turnRecorder{}
	registerBudgetTestActivities(env)
	env.RegisterActivityWithOptions(
		acts.LoadEvents,
		activity.RegisterOptions{Name: ActivityLoadEvents},
	)
	env.RegisterActivityWithOptions(
		acts.LoadInterrupt,
		activity.RegisterOptions{Name: ActivityLoadInterrupt},
	)
	env.RegisterActivityWithOptions(
		acts.LoadPendingActions,
		activity.RegisterOptions{Name: ActivityLoadPendingActions},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, in PrepareTurnInput) (PrepareTurnResult, error) {
			recorder.append(in.TriggerEventID)
			return acts.PrepareTurn(ctx, in)
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
		func(context.Context, CallModelInput) (CallModelResult, error) {
			return CallModelResult{Response: model.Response{StopReason: "end_turn"}}, nil
		},
		activity.RegisterOptions{Name: ActivityCallModel},
	)
	env.RegisterActivityWithOptions(
		func(ctx context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
			result, err := acts.CompleteWorkflowTurn(ctx, in)
			if err != nil {
				return RunTurnResult{}, err
			}
			if disposition != nil {
				result.Disposition = disposition(in.TriggerEventID)
			}
			return result, nil
		},
		activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn},
	)
	return recorder
}

// sessionWorkflowExitChecked turns a buffered wakeup at a close boundary into a
// test failure. Production relies on Temporal rejecting such a close and
// replaying; this wrapper asserts that the workflow proactively consumes every
// currently visible wakeup before returning or Continue-As-New.
func sessionWorkflowExitChecked(ctx workflow.Context, in SessionWorkflowInput, threshold int) error {
	err := sessionWorkflow(ctx, in, threshold)
	for _, name := range workflow.GetUnhandledSignalNames(ctx) {
		if name == WakeupSignalName {
			return errors.New("session workflow exited with a buffered wakeup")
		}
	}
	return err
}

func TestSessionWorkflow_UsesWorkflowOwnedLoop(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	source := newFakeSource([]domain.Event{userMsg("evt_1", 1)})
	recorder := registerCurrentTurnActivities(
		env,
		source,
		func(string) TurnDisposition { return TurnTerminated },
	)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 1})
	}, time.Millisecond)

	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(SessionWorkflow, SessionWorkflowInput{
		SessionID: "sess_1", StartCursor: 0,
	})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{"evt_1"}, recorder.snapshot())
	require.Equal(t, 1, source.completions("evt_1"))
}

func TestSessionWorkflowExitsWhenProjectionWasExternallyTerminated(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	source := newFakeSource([]domain.Event{{
		ID: "sevt_terminated", Sequence: 1, Type: domain.EvSessionStatusTerminated,
	}})
	recorder := registerCurrentTurnActivities(env, source, nil)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 1})
	}, time.Millisecond)
	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(sessionWorkflow, SessionWorkflowInput{SessionID: "sesn_failed_input"}, 100)
	require.NoError(t, env.GetWorkflowError())
	require.Empty(t, recorder.snapshot())
}

func TestSessionWorkflow_ParksUntilFullBarrierThenPreservesQueuedMessage(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	registerBudgetTestActivities(env)

	events := []EventRef{
		{ID: "sevt_original", Seq: 1, Type: domain.EvUserMessage},
		{ID: "sevt_queued", Seq: 2, Type: domain.EvUserMessage},
	}
	env.RegisterActivityWithOptions(
		func(_ context.Context, in LoadEventsInput) (LoadEventsResult, error) {
			var out []EventRef
			for _, event := range events {
				if event.Seq > in.Cursor {
					out = append(out, event)
				}
			}
			return LoadEventsResult{Events: out}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadEvents},
	)
	env.RegisterActivityWithOptions(
		func(context.Context, LoadInterruptInput) (LoadInterruptResult, error) {
			return LoadInterruptResult{}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadInterrupt},
	)

	var pendingMu sync.Mutex
	var pending []PendingActionRef
	env.RegisterActivityWithOptions(
		func(context.Context, LoadPendingActionsInput) (LoadPendingActionsResult, error) {
			pendingMu.Lock()
			defer pendingMu.Unlock()
			return LoadPendingActionsResult{
				Actions: append([]PendingActionRef(nil), pending...),
			}, nil
		},
		activity.RegisterOptions{Name: ActivityLoadPendingActions},
	)

	var prepareMu sync.Mutex
	var prepared []PrepareTurnInput
	env.RegisterActivityWithOptions(
		func(_ context.Context, in PrepareTurnInput) (PrepareTurnResult, error) {
			prepareMu.Lock()
			prepared = append(prepared, in)
			prepareMu.Unlock()
			return PrepareTurnResult{Request: model.Request{Model: "fake"}}, nil
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
		func(context.Context, CallModelInput) (CallModelResult, error) {
			return CallModelResult{Response: model.Response{StopReason: "end_turn"}}, nil
		},
		activity.RegisterOptions{Name: ActivityCallModel},
	)
	env.RegisterActivityWithOptions(
		func(_ context.Context, in CompleteWorkflowTurnInput) (RunTurnResult, error) {
			switch in.TriggerEventID {
			case "sevt_original":
				pendingMu.Lock()
				pending = []PendingActionRef{
					{
						ActionEventID: "sevt_action_1", ActionEventSeq: 10,
						Kind: domain.PendingCustomToolResult,
					},
					{
						ActionEventID: "sevt_action_2", ActionEventSeq: 11,
						Kind: domain.PendingToolConfirmation,
					},
				}
				pendingMu.Unlock()
				return RunTurnResult{Disposition: TurnParked}, nil
			case "sevt_result_2":
				require.Equal(t, []string{"sevt_result_1", "sevt_result_2"}, in.ResolutionEventIDs)
				pendingMu.Lock()
				pending = nil
				pendingMu.Unlock()
				return RunTurnResult{Disposition: TurnCompleted}, nil
			case "sevt_queued":
				return RunTurnResult{Disposition: TurnTerminated}, nil
			default:
				return RunTurnResult{}, errors.New("unexpected trigger: " + in.TriggerEventID)
			}
		},
		activity.RegisterOptions{Name: ActivityCompleteWorkflowTurn},
	)

	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 2})
	}, time.Millisecond)
	env.RegisterDelayedCallback(func() {
		pendingMu.Lock()
		pending[0].ResolutionEventID = "sevt_result_1"
		pending[0].ResolutionEventSeq = 3
		pendingMu.Unlock()
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 3})
	}, time.Minute)
	env.RegisterDelayedCallback(func() {
		pendingMu.Lock()
		pending[1].ResolutionEventID = "sevt_result_2"
		pending[1].ResolutionEventSeq = 4
		pendingMu.Unlock()
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 4})
	}, 2*time.Minute)

	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(SessionWorkflow, SessionWorkflowInput{SessionID: "sess_barrier"})
	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())

	prepareMu.Lock()
	defer prepareMu.Unlock()
	require.Equal(t, []PrepareTurnInput{
		{SessionID: "sess_barrier", TriggerEventID: "sevt_original"},
		{
			SessionID: "sess_barrier", TriggerEventID: "sevt_result_2",
			ResolutionEventIDs: []string{"sevt_result_1", "sevt_result_2"},
		},
		{SessionID: "sess_barrier", TriggerEventID: "sevt_queued"},
	}, prepared, "partial resolution must not run and queued work must follow the full resume")
}

func TestSessionWorkflow_DuplicateWakeupsProcessOnce(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	source := newFakeSource([]domain.Event{userMsg("evt_1", 1)})
	registerCurrentTurnActivities(env, source, nil)

	// Coalesce a burst that contains duplicate and out-of-order metadata.
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 1})
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 1})
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 0})
	}, time.Millisecond)

	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(
		sessionWorkflowExitChecked,
		SessionWorkflowInput{SessionID: "sess_dup"},
		1,
	)
	var canErr *workflow.ContinueAsNewError
	require.ErrorAs(t, env.GetWorkflowError(), &canErr)
	require.Equal(t, 1, source.completions("evt_1"))
}

func TestSessionWorkflow_ConsumesMessagesInReceiptOrder(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	source := newFakeSource([]domain.Event{
		userMsg("evt_1", 1),
		userMsg("evt_2", 2),
	})
	recorder := registerCurrentTurnActivities(
		env,
		source,
		func(trigger string) TurnDisposition {
			if trigger == "evt_2" {
				return TurnTerminated
			}
			return TurnCompleted
		},
	)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 2})
	}, time.Millisecond)

	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(SessionWorkflow, SessionWorkflowInput{SessionID: "sess_order"})

	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{"evt_1", "evt_2"}, recorder.snapshot())
}

func TestSessionWorkflow_ContinueAsNewCarriesCursor(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	source := newFakeSource([]domain.Event{userMsg("evt_1", 1)})
	registerCurrentTurnActivities(env, source, nil)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 1})
	}, time.Millisecond)

	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(
		sessionWorkflowExitChecked,
		SessionWorkflowInput{SessionID: "sess_can"},
		1,
	)

	var canErr *workflow.ContinueAsNewError
	require.ErrorAs(t, env.GetWorkflowError(), &canErr)
	require.Equal(t, SessionWorkflowType, canErr.WorkflowType.Name)
	require.Equal(t, 1, source.completions("evt_1"))
}

func TestSessionWorkflow_DrainsBufferedWakeupBeforeCloseBoundary(t *testing.T) {
	tests := []struct {
		name        string
		disposition TurnDisposition
		threshold   int
		wantCAN     bool
	}{
		{name: "continue-as-new", disposition: TurnCompleted, threshold: 1, wantCAN: true},
		{name: "terminal-completion", disposition: TurnTerminated, threshold: continueAsNewThreshold},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var ts testsuite.WorkflowTestSuite
			env := ts.NewTestWorkflowEnvironment()
			source := newFakeSource([]domain.Event{userMsg("evt_1", 1)})
			registerCurrentTurnActivities(
				env,
				source,
				func(string) TurnDisposition { return tc.disposition },
			)
			env.OnActivity(ActivityPrepareTurn, mock.Anything, PrepareTurnInput{
				SessionID: "sess_close_boundary", TriggerEventID: "evt_1",
			}).After(time.Hour).Return(PrepareTurnResult{
				Request: model.Request{Model: "fake"},
			}, nil).Once()

			env.RegisterDelayedCallback(func() {
				env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 1})
			}, time.Minute)
			env.RegisterDelayedCallback(func() {
				env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 1})
			}, 30*time.Minute)

			env.SetTestTimeout(10 * time.Second)
			env.ExecuteWorkflow(
				sessionWorkflowExitChecked,
				SessionWorkflowInput{SessionID: "sess_close_boundary"},
				tc.threshold,
			)

			err := env.GetWorkflowError()
			if tc.wantCAN {
				var canErr *workflow.ContinueAsNewError
				require.ErrorAs(t, err, &canErr)
			} else {
				require.NoError(t, err)
			}
			env.AssertExpectations(t)
		})
	}
}

func TestSessionWorkflow_TerminationStopsLoadedBatch(t *testing.T) {
	var ts testsuite.WorkflowTestSuite
	env := ts.NewTestWorkflowEnvironment()
	source := newFakeSource([]domain.Event{
		userMsg("evt_1", 1),
		userMsg("evt_2", 2),
	})
	recorder := registerCurrentTurnActivities(
		env,
		source,
		func(string) TurnDisposition { return TurnTerminated },
	)
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WakeupSignalName, WakeupSignal{MaxEventSeq: 2})
	}, time.Millisecond)

	env.SetTestTimeout(10 * time.Second)
	env.ExecuteWorkflow(SessionWorkflow, SessionWorkflowInput{SessionID: "sess_term"})

	require.True(t, env.IsWorkflowCompleted())
	require.NoError(t, env.GetWorkflowError())
	require.Equal(t, []string{"evt_1"}, recorder.snapshot())
}
