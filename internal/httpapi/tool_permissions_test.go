package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestAutomaticPermissionRawHTTPAgentAdmission(t *testing.T) {
	handler := NewTestHandler(t)
	for _, body := range []string{
		`{"name":"automatic local","model":"test-model","tools":[{"type":"agent_toolset_20260401","default_config":{"enabled":false},"configs":[{"name":"read","enabled":true,"permission_policy":{"type":"auto"}}]}]}`,
		`{"name":"automatic MCP","model":"test-model","tools":[{"type":"mcp_toolset","mcp_server_name":"issues","default_config":{"permission_policy":{"type":"auto"}}}],"mcp_servers":[{"type":"url","name":"issues","url":"https://mcp.example.test"}]}`,
	} {
		response := do(handler, "POST", "/v1/agents", body)
		require.Equal(t, 200, response.Code, response.Body.String())
		var agent map[string]any
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &agent))
		require.NotEmpty(t, agent["id"])
	}
	rejected := do(handler, "POST", "/v1/agents", `{"name":"automatic web","model":"test-model","tools":[{"type":"agent_toolset_20260401","default_config":{"permission_policy":{"type":"auto"}}}]}`)
	require.Equal(t, 400, rejected.Code)
}

func TestOpenAPIAutomaticPermissionIncludesTypedEvaluation(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	schemas := openAPIMap(t, openAPIMap(t, doc["components"], "components")["schemas"], "schemas")
	policy := openAPIMap(t, schemas["PermissionPolicy"], "policy")
	properties := openAPIMap(t, policy["properties"], "policy properties")
	kind := openAPIMap(t, properties["type"], "policy type")
	require.Contains(t, kind["enum"], "auto")
	require.NotNil(t, schemas["ToolPermissionEvaluation"])
	for _, name := range []string{"AgentToolUseEvent", "AgentMCPToolUseEvent"} {
		shape := openAPIMap(t, openAPIMap(t, schemas[name], name)["allOf"].([]any)[1], name+" fields")
		require.Contains(t, shape["required"], "evaluation")
		require.Contains(t, shape["required"], "evaluated_permission")
		fields := openAPIMap(t, shape["properties"], name+" properties")
		assertOpenAPIRef(t, fields["evaluation"], "#/components/schemas/ToolPermissionEvaluation")
	}
}

// Literal provider-independent expected events, served by Mango's real DTOs.
// This fixture is also consumed by all three independently packaged SDKs.
type automaticPermissionContractEvents struct{}

func (automaticPermissionContractEvents) Query(_ context.Context, _ string, _ app.EventQuery) ([]domain.Event, error) {
	var events []domain.Event
	for _, kind := range []string{domain.EvAgentToolUse, domain.EvAgentMcpToolUse} {
		for _, decision := range []string{"allow", "ask", "deny"} {
			nested := map[string]any{"type": decision}
			if decision == "ask" {
				nested["reason_code"] = "indeterminate"
			}
			if decision == "deny" {
				nested["reason_code"] = "high_risk"
			}
			payload := map[string]any{"name": "read", "input": map[string]any{"path": "README.md"}, "evaluated_permission": decision, "evaluation": map[string]any{"type": "auto", "evaluated_permission": nested}, domain.InternalPermissionToolName: "private alias", domain.InternalOriginTriggerEventID: "sevt_private_origin"}
			if kind == domain.EvAgentMcpToolUse {
				payload["mcp_server_name"] = "issues"
			}
			events = append(events, domain.Event{ID: fmt.Sprintf("sevt_auto_%d", len(events)), SessionID: "sesn_auto_fixture", Type: kind, Payload: payload})
		}
	}
	return events, nil
}

type automaticPermissionContractSessions struct{ SessionService }

func (automaticPermissionContractSessions) Get(_ context.Context, id string) (domain.Session, error) {
	if id != "sesn_auto_fixture" {
		return domain.Session{}, domain.NotFound("Session not found")
	}
	return domain.Session{ID: id, Status: domain.StatusIdle}, nil
}

func TestAutomaticPermissionRawHTTPEventContract(t *testing.T) {
	handler := NewServer(Deps{Events: automaticPermissionContractEvents{}, Sessions: automaticPermissionContractSessions{}}, Config{}).Handler()
	response := do(handler, "GET", "/v1/sessions/sesn_auto_fixture/events", "")
	require.Equal(t, 200, response.Code)
	var page struct {
		Data []map[string]any `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &page))
	require.Len(t, page.Data, 6)
	for i, event := range page.Data {
		want := []string{"allow", "ask", "deny"}[i%3]
		require.Equal(t, want, event["evaluated_permission"])
		evaluation := event["evaluation"].(map[string]any)
		require.Equal(t, "auto", evaluation["type"])
		require.Equal(t, want, evaluation["evaluated_permission"].(map[string]any)["type"])
		require.NotContains(t, event, domain.InternalPermissionToolName)
		require.NotContains(t, event, domain.InternalOriginTriggerEventID)
	}
}
