package temporal

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestPrepareResumeActions_ExternalApprovalEvidence(t *testing.T) {
	for _, scenario := range []string{"valid", "missing", "denied", "wrong_thread", "unprocessed", "wrong_reference", "late_approval", "server_owned"} {
		t.Run(scenario, func(t *testing.T) {
			now := time.Now().UTC()
			action := domain.Event{ID: "sevt_tool", ThreadID: "sthr_child", Sequence: 1, Type: domain.EvAgentToolUse,
				Payload: map[string]any{"name": "read", "input": map[string]any{"path": "report.md"},
					"evaluated_permission": "ask", domain.InternalToolExecutionOwner: "self_hosted"}}
			approval := domain.Event{ID: "sevt_approval", ThreadID: action.ThreadID, Sequence: 2,
				Type: domain.EvUserToolConfirmation, ProcessedAt: &now,
				Payload: map[string]any{"tool_use_id": "sevt_public", "result": "allow"}}
			result := domain.Event{ID: "sevt_result", ThreadID: action.ThreadID, Sequence: 3, Type: domain.EvUserToolResult,
				Payload: map[string]any{"tool_use_id": "sevt_public", "content": []any{map[string]any{"type": "text", "text": "contents"}}}}
			row := domain.PendingAction{ActionEventID: action.ID, ClientActionEventID: "sevt_public", ThreadID: action.ThreadID,
				Kind: domain.PendingToolResult, ResolvingEventID: &result.ID, ApprovalEventID: &approval.ID}
			switch scenario {
			case "missing":
				row.ApprovalEventID = nil
			case "denied":
				approval.Payload["result"] = "deny"
			case "wrong_thread":
				approval.ThreadID = "sthr_other"
			case "unprocessed":
				approval.ProcessedAt = nil
			case "wrong_reference":
				approval.Payload["tool_use_id"] = "sevt_other"
			case "late_approval":
				approval.Sequence = 4
			case "server_owned":
				delete(action.Payload, domain.InternalToolExecutionOwner)
			}
			activities := NewActivities(nil, newFakeSource([]domain.Event{action, approval, result}), nil, nil, &testIDGen{})
			got, err := activities.prepareResumeActions(context.Background(), "sesn_test", result.ID,
				[]string{result.ID}, []domain.PendingAction{row})
			if scenario != "valid" {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, got, 1)
			require.Equal(t, domain.PendingToolResult, got[0].Kind)
			require.Equal(t, "allow", got[0].Confirmation)
			require.Empty(t, got[0].ToolStepID, "external approval must never allocate server execution")
			require.Equal(t, result.Payload["content"], got[0].Content)
		})
	}
}

func TestResumeWorkflowTurn_ExternalConfirmationNeverExecutesOnServer(t *testing.T) {
	tools := indexTurnTools([]TurnTool{{Name: "read", Kind: TurnToolSelfHosted,
		Permission: domain.PermissionPolicy{Type: "always_ask"}}})
	for _, verdict := range []string{"allow", "deny"} {
		t.Run(verdict, func(t *testing.T) {
			turn := &workflowTurnState{}
			messages, interrupted, failure, err := resumeWorkflowTurn(turn, PrepareTurnResult{
				ResumeActions: []ResumeAction{{ActionEventID: "sevt_tool", ActionEventType: domain.EvAgentToolUse,
					Kind: domain.PendingToolConfirmation, ToolName: "read", Confirmation: verdict, DenyMessage: "not permitted"}},
			}, tools, nil)
			require.NoError(t, err)
			require.False(t, interrupted)
			if verdict == "allow" {
				require.Contains(t, string(failure), "must wait for a client tool result")
				require.Empty(t, turn.output)
				return
			}
			require.Empty(t, failure)
			require.Len(t, turn.output, 1)
			require.Equal(t, domain.EvAgentToolResult, turn.output[0].Type)
			require.Equal(t, "sevt_tool", turn.output[0].Payload["tool_use_id"])
			require.Equal(t, true, turn.output[0].Payload["is_error"])
			require.Len(t, messages, 2)
			require.Contains(t, messages[1].Content[0].Text, "not permitted")
		})
	}
}
