package temporal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/pg"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"go.temporal.io/sdk/workflow"
	"google.golang.org/protobuf/encoding/protojson"
)

type coordinatorErrorStore struct{ err error }

func (coordinatorErrorStore) BindWorkflowCoordinator(context.Context, string, string, pg.WorkflowCoordinatorClaim) error {
	return nil
}
func (s coordinatorErrorStore) ClaimWorkflowCoordinatorEvaluator(context.Context, string, string, string, int64, string) (pg.WorkflowEvaluatorClaim, error) {
	return pg.WorkflowEvaluatorClaim{Epoch: 1}, s.err
}
func (coordinatorErrorStore) LoadWorkflowPumpSnapshot(context.Context, string, string, pg.WorkflowPumpRequest) (pg.WorkflowPumpSnapshot, error) {
	return pg.WorkflowPumpSnapshot{Version: 1}, nil
}
func (coordinatorErrorStore) CommitWorkflowPump(context.Context, string, string, pg.WorkflowPumpRequest) (pg.WorkflowPumpDecision, error) {
	panic("erroring evaluator must not admit a command")
}

type coordinatorErrorEvaluator struct{ err error }

func (e coordinatorErrorEvaluator) Next(context.Context, pg.WorkflowPumpSnapshot) (*pg.WorkflowPumpCommand, error) {
	return nil, e.err
}

func TestWorkflowRunErrorsDoNotEnterTemporalFailureHistory(t *testing.T) {
	const secret = "PRIVATE_SOURCE_PROMPT_RECEIPT_SENTINEL"
	for _, tc := range []struct {
		name                   string
		storeErr, evaluatorErr error
		nonRetryable           bool
	}{
		{"evaluator infrastructure", nil, errors.New(secret), false},
		{"evaluator fence", nil, domain.Conflict(secret), true},
		{"evaluator fatal", nil, temporal.NewNonRetryableApplicationError(secret, secret, errors.New(secret), secret), true},
		{"store fence", domain.Conflict(secret), nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acts := WorkflowRunActivities{Store: coordinatorErrorStore{tc.storeErr}, Evaluator: coordinatorErrorEvaluator{tc.evaluatorErr}}
			var suite testsuite.WorkflowTestSuite
			env := suite.NewTestActivityEnvironment()
			env.RegisterActivityWithOptions(func(ctx context.Context) (WorkflowRunPumpResult, error) {
				return acts.Pump(ctx, WorkflowRunPumpInput{SessionID: "session", RunID: "run", Owner: pg.WorkflowCoordinatorClaim{ExecutionID: activity.GetInfo(ctx).WorkflowExecution.RunID}})
			}, activity.RegisterOptions{Name: "probe"})
			_, err := env.ExecuteActivity("probe")
			require.Error(t, err)
			var activityErr *temporal.ActivityError
			require.ErrorAs(t, err, &activityErr)
			failure := temporal.GetDefaultFailureConverter().ErrorToFailure(activityErr.Unwrap())
			encoded, err := protojson.Marshal(failure)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), secret)
			require.Nil(t, failure.Cause)
			require.Equal(t, tc.nonRetryable, failure.GetApplicationFailureInfo().NonRetryable)
		})
	}
}

func TestWorkflowRunContinueAsNewCarriesOnlyBoundedReferences(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	var observed WorkflowRunPumpInput
	env.RegisterActivityWithOptions(func(_ context.Context, in WorkflowRunPumpInput) (WorkflowRunPumpResult, error) {
		observed = in
		return WorkflowRunPumpResult{Cursor: 1, Epoch: 1, Version: 1}, nil
	}, activity.RegisterOptions{Name: ActivityWorkflowRunPump})
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow(WorkflowRunWakeSignal, pg.WorkflowRunWakeup{SessionID: "session", RunID: "run", Version: 7})
	}, 0)
	env.ExecuteWorkflow(func(ctx workflow.Context) error {
		info := workflow.GetInfo(ctx)
		info.FirstRunID = info.WorkflowExecution.RunID
		info.OriginalRunID = info.WorkflowExecution.RunID
		return workflowRunWorkflow(ctx, WorkflowRunWorkflowInput{SessionID: "session", RunID: "run"}, 1, time.Minute)
	})
	var continued *workflow.ContinueAsNewError
	require.ErrorAs(t, env.GetWorkflowError(), &continued)
	require.Equal(t, WorkflowRunWorkflowType, continued.WorkflowType.Name)
	var next WorkflowRunWorkflowInput
	require.NoError(t, converter.GetDefaultDataConverter().FromPayloads(continued.Input, &next))
	require.Equal(t, WorkflowRunWorkflowInput{SessionID: "session", RunID: "run", Cursor: 1, PreviousEpoch: 1, HintVersion: 7}, next)
	require.Equal(t, "session", observed.SessionID)
	require.Equal(t, observed.Owner.ExecutionID, observed.Owner.ChainID)
	require.NotEmpty(t, observed.Owner.ExecutionID)
	require.Less(t, continued.Input.Size(), 1024)
}

type runRelayFixture struct {
	wake                   *pg.WorkflowRunWakeup
	failSignal, failDelete bool
	advanceDuringSignal    bool
	signals                int
}

func (f *runRelayFixture) ClaimWorkflowRunWakeups(context.Context, int) ([]pg.WorkflowRunWakeup, error) {
	if f.wake == nil {
		return nil, nil
	}
	return []pg.WorkflowRunWakeup{*f.wake}, nil
}

func (*runRelayFixture) PrepareWorkflowRunStart(context.Context, pg.WorkflowRunWakeup) (bool, error) {
	return true, nil
}
func (*runRelayFixture) DeferWorkflowRunWakeup(context.Context, pg.WorkflowRunWakeup) error {
	return nil
}

func (f *runRelayFixture) DeleteWorkflowRunWakeupIfUnchanged(_ context.Context, wake pg.WorkflowRunWakeup) (bool, error) {
	if f.failDelete {
		return false, errors.New("delete unavailable")
	}
	if f.wake == nil || f.wake.Version != wake.Version {
		return false, nil
	}
	f.wake = nil
	return true, nil
}

func (f *runRelayFixture) WakeWorkflowRun(context.Context, pg.WorkflowRunWakeup) error {
	f.signals++
	if f.advanceDuringSignal {
		f.wake.Version++
		f.advanceDuringSignal = false
	}
	if f.failSignal {
		return errors.New("signal ACK lost")
	}
	return nil
}

func TestWorkflowRunRelayRetainsUnacknowledgedOrNewerWake(t *testing.T) {
	f := &runRelayFixture{wake: &pg.WorkflowRunWakeup{SessionID: "session", RunID: "run", Version: 1}, failSignal: true}
	relay := NewWorkflowRunRelay(f, f, RelayConfig{BatchSize: 1})
	n, err := relay.RunOnce(t.Context())
	require.Error(t, err)
	require.Zero(t, n)
	require.NotNil(t, f.wake)
	f.failSignal, f.advanceDuringSignal = false, true
	n, err = relay.RunOnce(t.Context())
	require.NoError(t, err)
	require.Zero(t, n)
	require.EqualValues(t, 2, f.wake.Version)
	f.failDelete = true
	_, err = relay.RunOnce(t.Context())
	require.Error(t, err)
	require.NotNil(t, f.wake)
	f.failDelete = false
	n, err = relay.RunOnce(t.Context())
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Nil(t, f.wake)
	require.Equal(t, 4, f.signals, "duplicate delivery is permitted, duplicate removal is not required")
}

type runStartClient struct {
	client.Client
	options client.StartWorkflowOptions
	input   WorkflowRunWorkflowInput
	wake    pg.WorkflowRunWakeup
}

func (c *runStartClient) SignalWithStartWorkflow(_ context.Context, id, name string, signal interface{}, options client.StartWorkflowOptions, kind interface{}, args ...interface{}) (client.WorkflowRun, error) {
	c.options, c.input, c.wake = options, args[0].(WorkflowRunWorkflowInput), signal.(pg.WorkflowRunWakeup)
	if kind != WorkflowRunWorkflowType || name != WorkflowRunWakeSignal {
		return nil, errors.New("unexpected coordinator target")
	}
	return completedWorkflowRun{id: id}, nil
}

func TestWorkflowRunSignalerRejectsClosedIDsWhileHistoryRetained(t *testing.T) {
	c := &runStartClient{}
	s := NewSignalerOnTaskQueue(c, "queue")
	wake := pg.WorkflowRunWakeup{SessionID: "session", RunID: "run", Version: 3, StartAllowed: true}
	require.NoError(t, s.WakeWorkflowRun(t.Context(), wake))
	require.Equal(t, "mango.workflow-run.run", c.options.ID)
	require.Equal(t, enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, c.options.WorkflowIDReusePolicy)
	require.Equal(t, "queue", c.options.TaskQueue)
	require.Equal(t, WorkflowRunWorkflowInput{SessionID: "session", RunID: "run"}, c.input)
	wake.StartAllowed = false
	require.Equal(t, wake, c.wake)
}

// The real Activity metadata independently fences a replayed predecessor input,
// even if a Workflow-level guard or a completed bind was replayed from history.
func TestWorkflowRunPumpRejectsForeignPhysicalExecution(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestActivityEnvironment()
	acts := WorkflowRunActivities{Store: coordinatorUntouchedStore{}, Evaluator: coordinatorErrorEvaluator{}}
	env.RegisterActivity(acts.Pump)
	_, err := env.ExecuteActivity(acts.Pump, WorkflowRunPumpInput{SessionID: "session", RunID: "run", Owner: pg.WorkflowCoordinatorClaim{ChainID: "old-physical", ExecutionID: "old-physical"}})
	require.ErrorContains(t, err, "reconciliation")
}

type coordinatorUntouchedStore struct{ coordinatorErrorStore }

func (coordinatorUntouchedStore) BindWorkflowCoordinator(context.Context, string, string, pg.WorkflowCoordinatorClaim) error {
	panic("foreign physical Activity must not reach PG")
}

func (c *runStartClient) SignalWorkflow(_ context.Context, id, _ string, signalName string, arg interface{}) error {
	if id != "mango.workflow-run.run" || signalName != WorkflowRunWakeSignal {
		return errors.New("unexpected signal target")
	}
	c.wake = arg.(pg.WorkflowRunWakeup)
	return nil
}
func TestWorkflowRunRetryCannotStartAnotherChain(t *testing.T) {
	c := &runStartClient{}
	s := NewSignalerOnTaskQueue(c, "queue")
	require.NoError(t, s.WakeWorkflowRun(t.Context(), pg.WorkflowRunWakeup{SessionID: "session", RunID: "run", Version: 2, LeaseToken: "transport-only", DeliveryAttempt: 2}))
	require.EqualValues(t, 2, c.wake.Version)
	require.Empty(t, c.input.SessionID, "retry uses Signal only")
	require.Empty(t, c.wake.LeaseToken)
}
