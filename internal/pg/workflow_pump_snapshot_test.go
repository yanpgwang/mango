package pg

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkflowPumpSnapshotRecoversDecisionsAndOwnedReceipts(t *testing.T) {
	store, session, run, request := workflowReceiptFixture(t)
	ctx := t.Context()
	claim, err := store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "pump/0", 0)
	require.NoError(t, err)
	in := WorkflowPumpRequest{RequestID: "pump/0", Epoch: claim.Epoch}
	snapshot, err := store.LoadWorkflowPumpSnapshot(ctx, session.ID, run.ID, in)
	require.NoError(t, err)
	require.JSONEq(t, string(request.Input), string(snapshot.Request.Input))
	request.Input = snapshot.Request.Input // request is persisted as jsonb.
	require.Equal(t, request, snapshot.Request)
	require.Empty(t, snapshot.Journal)
	require.EqualValues(t, 1, snapshot.Version)
	in.Command = WorkflowPumpCommand{Kind: "invoke", AgentName: "unknown", Prompt: "request"}
	decision, err := store.CommitWorkflowPump(ctx, session.ID, run.ID, in)
	require.NoError(t, err)
	require.Equal(t, "rejected", decision.Type)
	cached, err := store.LoadWorkflowPumpSnapshot(ctx, session.ID, run.ID, in)
	require.NoError(t, err)
	require.Equal(t, &decision, cached.Committed)
	require.Empty(t, cached.Request.Source, "ACK recovery does not need to evaluate source again")
	claim, err = store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "pump/1", claim.Epoch)
	require.NoError(t, err)
	in = WorkflowPumpRequest{RequestID: "pump/1", ExpectedCursor: 1, Epoch: claim.Epoch,
		Command: WorkflowPumpCommand{Kind: "invoke", AgentName: "reviewer", Prompt: "request"}}
	snapshot, err = store.LoadWorkflowPumpSnapshot(ctx, session.ID, run.ID, in)
	require.NoError(t, err)
	require.Len(t, snapshot.Journal, 1)
	require.Equal(t, "agent_not_admitted", snapshot.Journal[0].Decision.ErrorCode)
	decision, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, in)
	require.NoError(t, err)
	_, err = completeReceiptInvocation(ctx, store, run, *decision.Attempt, "sevt_return", "owned result")
	require.NoError(t, err)
	claim, err = store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "pump/2", claim.Epoch)
	require.NoError(t, err)
	snapshot, err = store.LoadWorkflowPumpSnapshot(ctx, session.ID, run.ID, WorkflowPumpRequest{RequestID: "pump/2", ExpectedCursor: 2, Epoch: claim.Epoch})
	require.NoError(t, err)
	require.Len(t, snapshot.Journal, 2)
	require.Equal(t, "pump/0", snapshot.Journal[0].RequestID)
	require.Nil(t, snapshot.Journal[0].Receipt)
	require.Equal(t, decision.Attempt.ID, snapshot.Journal[1].Decision.Attempt.ID)
	require.Equal(t, "owned result", snapshot.Journal[1].Receipt.Text)
	require.EqualValues(t, 4, snapshot.Version)
	_, err = store.LoadWorkflowPumpSnapshot(ctx, session.ID, run.ID, in)
	require.ErrorContains(t, err, "stale", "an old evaluator cannot even recover its cached decision")
	_, err = store.LoadWorkflowPumpSnapshot(ctx, session.ID, run.ID, WorkflowPumpRequest{RequestID: "pump/2", ExpectedCursor: 1, Epoch: claim.Epoch})
	require.ErrorContains(t, err, "frontier")
}
