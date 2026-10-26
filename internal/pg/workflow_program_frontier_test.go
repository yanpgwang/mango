package pg

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestWorkflowProgramDeliveryAndReturnUseOwnedFrontier(t *testing.T) {
	store, session, run, _ := workflowReceiptFixture(t)
	ctx := t.Context()
	claim, err := store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "program", 0)
	require.NoError(t, err)
	cursor := int64(0)
	commit := func(id string, command WorkflowPumpCommand) (WorkflowPumpDecision, error) {
		return store.CommitWorkflowPump(ctx, session.ID, run.ID, WorkflowPumpRequest{RequestID: id, Epoch: claim.Epoch, ExpectedCursor: cursor, Command: command})
	}
	first, err := commit("invoke-a", WorkflowPumpCommand{Kind: "invoke", AgentName: "reviewer", Prompt: "A"})
	require.NoError(t, err)
	cursor++
	_, err = commit("early-delivery", WorkflowPumpCommand{Kind: "deliver", InvocationCursor: 0})
	require.ErrorContains(t, err, "settled")
	_, err = commit("early-return", WorkflowPumpCommand{Kind: "return", Value: json.RawMessage(`{"done":true}`)})
	require.ErrorContains(t, err, "undelivered")
	_, err = completeReceiptInvocation(ctx, store, run, *first.Attempt, "sevt_program_a", `{"count":7}`)
	require.NoError(t, err)
	// A fault after the delivery insert must roll back cursor, journal and wake.
	_, err = store.pool.Exec(ctx, `ALTER TABLE workflow_runs ADD CONSTRAINT program_fault CHECK(journal_cursor<2)`)
	require.NoError(t, err)
	_, err = commit("delivery-a", WorkflowPumpCommand{Kind: "deliver", InvocationCursor: 0})
	require.Error(t, err)
	_, err = store.pool.Exec(ctx, `ALTER TABLE workflow_runs DROP CONSTRAINT program_fault`)
	require.NoError(t, err)
	delivery, err := commit("delivery-a", WorkflowPumpCommand{Kind: "deliver", InvocationCursor: 0})
	require.NoError(t, err)
	require.Equal(t, "delivery", delivery.Type)
	require.Equal(t, `{"count":7}`, delivery.Delivery.Receipt.Text)
	again, err := commit("delivery-a", WorkflowPumpCommand{Kind: "deliver", InvocationCursor: 0})
	require.NoError(t, err)
	require.Equal(t, delivery, again, "lost delivery ACK recovers immutable receipt")
	cursor++
	_, err = commit("duplicate-delivery", WorkflowPumpCommand{Kind: "deliver", InvocationCursor: 0})
	require.ErrorContains(t, err, "already delivered")
	_, err = commit("wrong-delivery", WorkflowPumpCommand{Kind: "deliver", InvocationCursor: 1})
	require.ErrorContains(t, err, "does not own")
	second, err := commit("invoke-b", WorkflowPumpCommand{Kind: "invoke", AgentName: "reviewer", Prompt: "B:7"})
	require.NoError(t, err)
	cursor++
	var ordinal int
	require.NoError(t, store.pool.QueryRow(ctx, `SELECT ordinal FROM workflow_invocations WHERE id=$1`, second.Attempt.InvocationID).Scan(&ordinal))
	require.Equal(t, 2, ordinal, "invocation cursor diverges from JS call ordinal")
	_, err = completeReceiptInvocation(ctx, store, run, *second.Attempt, "sevt_program_b", "beta")
	require.NoError(t, err)
	_, err = commit("delivery-b", WorkflowPumpCommand{Kind: "deliver", InvocationCursor: 2})
	require.NoError(t, err)
	cursor++
	result, err := commit("return", WorkflowPumpCommand{Kind: "return", Value: json.RawMessage(`{"total":12,"note":"beta"}`)})
	require.NoError(t, err)
	require.Equal(t, "return", result.Type)
	again, err = commit("return", WorkflowPumpCommand{Kind: "return", Value: json.RawMessage(`{"total":12,"note":"beta"}`)})
	require.NoError(t, err)
	require.JSONEq(t, string(result.Value), string(again.Value))
	again.Value = result.Value
	require.Equal(t, result, again)
	cursor++
	_, err = commit("after-return", WorkflowPumpCommand{Kind: "invoke", AgentName: "reviewer", Prompt: "must-not-run"})
	require.ErrorContains(t, err, "already committed")
	replacement, err := store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "replacement", claim.Epoch)
	require.NoError(t, err)
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, WorkflowPumpRequest{RequestID: "return", Epoch: claim.Epoch, ExpectedCursor: 4, Command: WorkflowPumpCommand{Kind: "return", Value: result.Value}})
	require.ErrorContains(t, err, "stale")
	snapshot, err := store.LoadWorkflowPumpSnapshot(ctx, session.ID, run.ID, WorkflowPumpRequest{RequestID: "next", Epoch: replacement.Epoch, ExpectedCursor: 5})
	require.NoError(t, err)
	require.Len(t, snapshot.Journal, 5)
	require.Equal(t, []string{"invocation", "delivery", "invocation", "delivery", "return"}, []string{snapshot.Journal[0].Decision.Type, snapshot.Journal[1].Decision.Type, snapshot.Journal[2].Decision.Type, snapshot.Journal[3].Decision.Type, snapshot.Journal[4].Decision.Type})
	require.JSONEq(t, `{"total":12,"note":"beta"}`, string(snapshot.Journal[4].Decision.Value))
	var state string
	require.NoError(t, store.pool.QueryRow(ctx, `SELECT state FROM workflow_runs WHERE id=$1`, run.ID).Scan(&state))
	require.Equal(t, "open", state, "program candidate is not product Run closure")
}

func TestWorkflowProgramReturnRejectsInvalidAndUndeliveredOutcomes(t *testing.T) {
	store, session, run, request := workflowReceiptFixture(t)
	ctx := t.Context()
	claim, err := store.ClaimWorkflowEvaluator(ctx, session.ID, run.ID, "program", 0)
	require.NoError(t, err)
	in := WorkflowPumpRequest{RequestID: "rejected", Epoch: claim.Epoch, Command: WorkflowPumpCommand{Kind: "invoke", AgentName: "absent", Prompt: "no"}}
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, in)
	require.NoError(t, err)
	in.RequestID = "early"
	in.ExpectedCursor = 1
	in.Command = WorkflowPumpCommand{Kind: "return", Value: json.RawMessage(`null`)}
	_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, in)
	require.ErrorContains(t, err, "undelivered")
	in.RequestID = "deliver-rejection"
	in.Command = WorkflowPumpCommand{Kind: "deliver"}
	delivered, err := store.CommitWorkflowPump(ctx, session.ID, run.ID, in)
	require.NoError(t, err)
	require.Equal(t, "agent_not_admitted", delivered.Delivery.Receipt.ErrorCode)
	for _, value := range []json.RawMessage{json.RawMessage(`"this return value exceeds the admitted result byte bound"`), json.RawMessage(`{"invalid":`)} {
		in.RequestID = "bad-value"
		in.ExpectedCursor = 2
		in.Command = WorkflowPumpCommand{Kind: "return", Value: value}
		_, err = store.CommitWorkflowPump(ctx, session.ID, run.ID, in)
		require.Error(t, err)
	}
	// A foreign Run's receipt cannot be delivered even with a matching cursor.
	foreign, err := store.AdmitWorkflowRun(ctx, session.ID, "foreign", request)
	require.NoError(t, err)
	foreignClaim, err := store.ClaimWorkflowEvaluator(ctx, session.ID, foreign.ID, "program", 0)
	require.NoError(t, err)
	_, err = store.CommitWorkflowPump(ctx, session.ID, foreign.ID, WorkflowPumpRequest{RequestID: "foreign-delivery", Epoch: foreignClaim.Epoch, Command: WorkflowPumpCommand{Kind: "deliver"}})
	require.Error(t, err)
}
