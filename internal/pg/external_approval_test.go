package pg

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/domain"
)

func parkExternalApproval(t *testing.T, store *Store, extraCustom bool) (string, string) {
	t.Helper()
	session, trigger := pendingTurn(t, store, "sesn_external_approval")
	id := "sevt_external_read"
	ids := []string{id}
	drafts := []domain.EventDraft{{ID: id, Type: domain.EvAgentToolUse, Payload: map[string]any{
		"name": "read", "input": map[string]any{"path": "report.md"},
		"evaluated_permission": "ask", domain.InternalToolExecutionOwner: "self_hosted",
	}}}
	if extraCustom {
		ids = append(ids, "sevt_custom")
		drafts = append(drafts, domain.EventDraft{ID: "sevt_custom", Type: domain.EvAgentCustomToolUse,
			Payload: map[string]any{"name": "custom", "input": map[string]any{}}})
	}
	drafts = append(drafts, requiresActionDraft(ids))
	_, err := store.CompleteWorkflowTurn(context.Background(), session.ID, trigger, drafts,
		domain.StatusIdle, "", "", nil, ids, nil)
	require.NoError(t, err)
	return session.ID, id
}

func externalConfirmation(id, verdict string) domain.EventDraft {
	return domain.EventDraft{Type: domain.EvUserToolConfirmation,
		Payload: map[string]any{"tool_use_id": id, "result": verdict}}
}

func externalResult(id string) domain.EventDraft {
	return domain.EventDraft{Type: domain.EvUserToolResult,
		Payload: map[string]any{"tool_use_id": id, "content": []any{map[string]any{"type": "text", "text": "contents"}}}}
}

func TestExternalApprovalWaitsForResultAndCompleteBarrier(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	sessionID, actionID := parkExternalApproval(t, store, true)
	_, err := store.AdmitEvents(ctx, sessionID, []domain.EventDraft{externalResult(actionID)})
	require.ErrorContains(t, err, "resolution kind does not match")
	approval, err := store.AdmitEvents(ctx, sessionID, []domain.EventDraft{externalConfirmation(actionID, "allow")})
	require.NoError(t, err)
	require.Equal(t, domain.StatusIdle, approval.Session.Status)
	require.Len(t, approval.Events, 1)
	require.NotNil(t, approval.SubmittedEvents[0].ProcessedAt)
	stored, err := store.GetEvent(ctx, sessionID, approval.Events[0].ID)
	require.NoError(t, err)
	require.Equal(t, stored.ProcessedAt, approval.SubmittedEvents[0].ProcessedAt)
	pending, err := store.UnresolvedPendingActions(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	for _, row := range pending {
		if row.ActionEventID == actionID {
			require.Equal(t, domain.PendingToolResult, row.Kind)
			require.Equal(t, &stored.ID, row.ApprovalEventID)
			require.Nil(t, row.ResolvingEventID)
		}
	}
	for _, verdict := range []string{"allow", "deny"} {
		_, err := store.AdmitEvents(ctx, sessionID, []domain.EventDraft{externalConfirmation(actionID, verdict)})
		var domainErr *domain.DomainError
		require.ErrorAs(t, err, &domainErr)
		require.Equal(t, domain.KindConflict, domainErr.Kind)
	}
	result, err := store.AdmitEvents(ctx, sessionID, []domain.EventDraft{externalResult(actionID)})
	require.NoError(t, err)
	require.Equal(t, domain.StatusIdle, result.Session.Status, "a partial result cannot resume the model")
	history, err := store.HistoryThrough(ctx, sessionID, result.Events[0].ID, 100)
	require.NoError(t, err)
	for _, event := range history {
		require.NotEqual(t, stored.ID, event.ID, "approval is not a model-driving turn")
	}
	last, err := store.AdmitEvents(ctx, sessionID, []domain.EventDraft{{Type: domain.EvUserCustomToolResult,
		Payload: map[string]any{"custom_tool_use_id": "sevt_custom"}}})
	require.NoError(t, err)
	require.Equal(t, domain.StatusRunning, last.Session.Status)
	resolutions := []string{result.Events[0].ID, last.Events[0].ID}
	_, err = store.CompleteWorkflowTurn(ctx, sessionID, last.Events[0].ID,
		[]domain.EventDraft{{Type: domain.EvSessionStatusIdle, Payload: map[string]any{"stop_reason": map[string]any{"type": "end_turn"}}}},
		domain.StatusIdle, "", "", nil, nil, resolutions)
	require.NoError(t, err)
	pending, err = store.UnresolvedPendingActions(ctx, sessionID)
	require.NoError(t, err)
	require.Empty(t, pending)
	next, err := store.AdmitEvents(ctx, sessionID, []domain.EventDraft{textMsg("follow up")})
	require.NoError(t, err)
	history, err = store.HistoryThrough(ctx, sessionID, next.Events[0].ID, 100)
	require.NoError(t, err)
	for _, event := range history {
		require.NotEqual(t, stored.ID, event.ID, "completed approval must not invalidate later provider context")
	}
}

func TestExternalApprovalBatchOrderAndRollback(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	sessionID, id := parkExternalApproval(t, store, false)
	before, err := store.EventsAfter(ctx, sessionID, 0, 100)
	require.NoError(t, err)
	for _, drafts := range [][]domain.EventDraft{
		{externalResult(id), externalConfirmation(id, "allow")},
		{externalConfirmation(id, "allow"), externalResult(id), externalResult(id)},
		{externalConfirmation(id, "allow"), externalConfirmation(id, "deny")},
	} {
		_, err := store.AdmitEvents(ctx, sessionID, drafts)
		require.Error(t, err)
		after, err := store.EventsAfter(ctx, sessionID, 0, 100)
		require.NoError(t, err)
		require.Len(t, after, len(before), "failing batch must roll back approval and events")
		pending, err := store.UnresolvedPendingActions(ctx, sessionID)
		require.NoError(t, err)
		require.Nil(t, pending[0].ApprovalEventID)
		require.Equal(t, domain.PendingToolConfirmation, pending[0].Kind)
	}
	admitted, err := store.AdmitEvents(ctx, sessionID, []domain.EventDraft{externalConfirmation(id, "allow"), externalResult(id)})
	require.NoError(t, err)
	require.Equal(t, domain.StatusRunning, admitted.Session.Status)
	require.Len(t, admitted.SubmittedEvents, 2)
	require.NotNil(t, admitted.SubmittedEvents[0].ProcessedAt)
}

func TestExternalDenialRejectsLaterResult(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	sessionID, id := parkExternalApproval(t, store, false)
	denial, err := store.AdmitEvents(ctx, sessionID, []domain.EventDraft{externalConfirmation(id, "deny")})
	require.NoError(t, err)
	require.Equal(t, domain.StatusRunning, denial.Session.Status)
	pending, err := store.UnresolvedPendingActions(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, domain.PendingToolConfirmation, pending[0].Kind)
	require.Nil(t, pending[0].ApprovalEventID)
	require.Equal(t, &denial.Events[0].ID, pending[0].ResolvingEventID)
	_, err = store.AdmitEvents(ctx, sessionID, []domain.EventDraft{externalResult(id)})
	require.Error(t, err)
}
