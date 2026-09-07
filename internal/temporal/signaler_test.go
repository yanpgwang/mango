package temporal

import (
	"context"
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/pg"
	"go.temporal.io/sdk/client"
)

type capturingWorkflowClient struct {
	client.Client
	options      client.StartWorkflowOptions
	workflow     interface{}
	signals      []capturedSignalWithStart
	terminations []string
}

type capturedSignalWithStart struct {
	workflowID string
	workflow   interface{}
	args       []interface{}
}

func (c *capturingWorkflowClient) TerminateWorkflow(
	_ context.Context,
	workflowID string,
	_ string,
	_ string,
	_ ...interface{},
) error {
	c.terminations = append(c.terminations, workflowID)
	return nil
}

func (c *capturingWorkflowClient) ExecuteWorkflow(
	_ context.Context,
	options client.StartWorkflowOptions,
	workflow interface{},
	_ ...interface{},
) (client.WorkflowRun, error) {
	c.options = options
	c.workflow = workflow
	return completedWorkflowRun{id: options.ID}, nil
}

func (c *capturingWorkflowClient) SignalWithStartWorkflow(
	_ context.Context,
	workflowID string,
	_ string,
	_ interface{},
	_ client.StartWorkflowOptions,
	workflow interface{},
	args ...interface{},
) (client.WorkflowRun, error) {
	c.signals = append(c.signals, capturedSignalWithStart{
		workflowID: workflowID, workflow: workflow, args: args,
	})
	return completedWorkflowRun{id: workflowID}, nil
}

type completedWorkflowRun struct {
	id string
}

func (r completedWorkflowRun) GetID() string  { return r.id }
func (completedWorkflowRun) GetRunID() string { return "run" }
func (completedWorkflowRun) Get(context.Context, interface{}) error {
	return nil
}
func (completedWorkflowRun) GetWithOptions(
	context.Context,
	interface{},
	client.WorkflowRunGetOptions,
) error {
	return nil
}

func TestTerminateSession_StopsPrimaryWorkflow(t *testing.T) {
	c := &capturingWorkflowClient{}
	signaler := NewSignalerOnTaskQueue(c, "test-queue")

	require.NoError(t, signaler.TerminateSession(context.Background(), "sesn_delete"))
	require.Equal(t, []string{"sesn_delete"}, c.terminations)
}

func TestNewSignaler_CleanupUsesDefaultTaskQueue(t *testing.T) {
	c := &capturingWorkflowClient{}
	require.NoError(t, NewSignaler(c).TerminateSession(context.Background(), "sesn_default"))
	require.Equal(t, []string{"sesn_default"}, c.terminations)
}

func TestTerminateThread_UsesStableChildWorkflowID(t *testing.T) {
	c := &capturingWorkflowClient{}
	signaler := NewSignaler(c)

	require.NoError(t, signaler.TerminateThread(
		context.Background(), "sesn_parent", "sthr_child",
	))
	require.Equal(t, []string{"session-thread:sthr_child"}, c.terminations)
}

func TestTerminateSessionWorkflows_StopsEveryChildBeforePrimaryCleanup(t *testing.T) {
	c := &capturingWorkflowClient{}
	signaler := NewSignaler(c)

	require.NoError(t, terminateSessionWorkflows(
		context.Background(), signaler, "sesn_multi",
		[]string{"sthr_first", "sthr_second"},
	))
	require.Equal(t, []string{
		"session-thread:sthr_first",
		"session-thread:sthr_second",
		"sesn_multi",
	}, c.terminations)
}

func TestOrchestratorFastPath_TargetedInterruptWakesOnlyOwningThread(t *testing.T) {
	c := &capturingWorkflowClient{}
	orchestrator := NewOrchestrator(nil, NewSignaler(c))
	orchestrator.fastPathWake(context.Background(), "sesn_targeted", pg.Admission{
		MaxSeq: 17, WakeThreadIDs: []string{"sthr_targeted"},
	})

	require.Len(t, c.signals, 1)
	require.Equal(t, "session-thread:sthr_targeted", c.signals[0].workflowID)
	require.Equal(t,
		reflect.ValueOf(SessionThreadWorkflow).Pointer(),
		reflect.ValueOf(c.signals[0].workflow).Pointer(),
	)
	require.Equal(t, []interface{}{SessionThreadWorkflowInput{
		SessionID: "sesn_targeted", ThreadID: "sthr_targeted", StartCursor: 0,
	}}, c.signals[0].args)
}

func TestOrchestratorFastPath_GlobalInterruptWakesPrimaryAndChildren(t *testing.T) {
	c := &capturingWorkflowClient{}
	orchestrator := NewOrchestrator(nil, NewSignaler(c))
	orchestrator.fastPathWake(context.Background(), "sesn_global", pg.Admission{
		MaxSeq: 23, PrimaryEnqueued: true,
		WakeThreadIDs: []string{"sthr_a", "sthr_b"},
	})

	require.Len(t, c.signals, 3)
	require.Equal(t, "sesn_global", c.signals[0].workflowID)
	require.Equal(t,
		reflect.ValueOf(SessionWorkflow).Pointer(),
		reflect.ValueOf(c.signals[0].workflow).Pointer(),
	)
	require.Equal(t, "session-thread:sthr_a", c.signals[1].workflowID)
	require.Equal(t, "session-thread:sthr_b", c.signals[2].workflowID)
}
