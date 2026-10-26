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

func (s coordinatorErrorStore) ClaimWorkflowEvaluator(context.Context, string, string, string, int64) (pg.WorkflowEvaluatorClaim, error) {
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
			_, err := acts.Pump(t.Context(), WorkflowRunPumpInput{SessionID: "session", RunID: "run"})
			require.Error(t, err)
			failure := temporal.GetDefaultFailureConverter().ErrorToFailure(err)
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
	env.ExecuteWorkflow(workflowRunWorkflow, WorkflowRunWorkflowInput{SessionID: "session", RunID: "run"}, 1, time.Minute)
	var continued *workflow.ContinueAsNewError
	require.ErrorAs(t, env.GetWorkflowError(), &continued)
	require.Equal(t, WorkflowRunWorkflowType, continued.WorkflowType.Name)
	var next WorkflowRunWorkflowInput
	require.NoError(t, converter.GetDefaultDataConverter().FromPayloads(continued.Input, &next))
	require.Equal(t, WorkflowRunWorkflowInput{SessionID: "session", RunID: "run", Cursor: 1, PreviousEpoch: 1, HintVersion: 7}, next)
	require.Equal(t, WorkflowRunPumpInput{SessionID: "session", RunID: "run"}, observed)
	require.Less(t, continued.Input.Size(), 1024)
}

type runRelayFixture struct {
	wake                   *pg.WorkflowRunWakeup
	failSignal, failDelete bool
	advanceDuringSignal    bool
	signals                int
}

func (f *runRelayFixture) WorkflowRunWakeups(context.Context) ([]pg.WorkflowRunWakeup, error) {
	if f.wake == nil {
		return nil, nil
	}
	return []pg.WorkflowRunWakeup{*f.wake}, nil
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
	relay := NewWorkflowRunRelay(f, f, RelayConfig{})
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
	wake := pg.WorkflowRunWakeup{SessionID: "session", RunID: "run", Version: 3}
	require.NoError(t, s.WakeWorkflowRun(t.Context(), wake))
	require.Equal(t, "mango.workflow-run.run", c.options.ID)
	require.Equal(t, enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE, c.options.WorkflowIDReusePolicy)
	require.Equal(t, "queue", c.options.TaskQueue)
	require.Equal(t, WorkflowRunWorkflowInput{SessionID: "session", RunID: "run"}, c.input)
	require.Equal(t, wake, c.wake)
}
