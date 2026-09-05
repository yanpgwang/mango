package temporal

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestPrepareTurnWebToolsCannotBecomeSandboxOrExternalCalls(t *testing.T) {
	for _, environment := range []string{"cloud", "self_hosted"} {
		t.Run(environment, func(t *testing.T) {
			source := &mcpPrepareSource{
				fakeSource: newFakeSource([]domain.Event{{
					ID: "sevt_user", Sequence: 1, Type: domain.EvUserMessage,
					Payload: map[string]any{"content": []any{
						map[string]any{"type": "text", "text": "research and read the report"},
					}},
				}}),
				session: domain.Session{
					ID: "sesn_web", Status: domain.StatusRunning, EnvironmentType: environment,
					AgentSnapshot: domain.Agent{Model: domain.Model{ID: "model"},
						Tools: []any{map[string]any{"type": domain.BuiltinToolsetType,
							"configs": []any{map[string]any{"name": "read",
								"permission_policy": map[string]any{"type": "always_ask"}}},
						}},
					},
				},
			}
			prepared, err := NewActivities(nil, source, nil, nil, &testIDGen{}).PrepareTurn(
				context.Background(), PrepareTurnInput{SessionID: "sesn_web", TriggerEventID: "sevt_user"},
			)
			require.NoError(t, err)
			require.Empty(t, prepared.FatalError)
			require.Contains(t, summarizeModelTools(prepared.Request.Tools), modelToolSummary{
				Name: "web_search", Type: "web_search_20260318",
			})
			require.Contains(t, summarizeModelTools(prepared.Request.Tools), modelToolSummary{
				Name: "web_fetch", Type: "web_fetch_20260318",
			})
			tools := indexTurnTools(prepared.Tools)
			require.Len(t, tools, 6)
			require.NotContains(t, tools, "web_search")
			require.NotContains(t, tools, "web_fetch")
			kind := TurnToolBuiltin
			if environment == "self_hosted" {
				kind = TurnToolSelfHosted
			}
			for _, name := range []string{"bash", "read", "write", "edit", "glob", "grep"} {
				require.Equal(t, kind, tools[name].Kind, name)
			}
			require.Equal(t, "always_ask", tools["read"].Permission.Type)

			for _, name := range []string{"web_search", "web_fetch"} {
				// A misbehaving model must not redirect a provider-owned call
				// into the shell/file executor or create a worker result wait.
				plan, failure := planToolBatch([]domain.ContentBlock{{
					Type: "tool_use", ToolUseID: "provider_web", ToolName: name,
					Input: map[string]any{"query": "report"},
				}}, tools, map[string]PlannedToolStep{
					"provider_web": {ToolStepID: "step_web", ToolUseEventID: "sevt_web"},
				}, false)
				require.Contains(t, failure, "not enabled: "+name)
				require.Empty(t, plan.actionDrafts)
				require.Empty(t, plan.executable)
				require.Empty(t, plan.pendingActionEventIDs)
			}
		})
	}
}
