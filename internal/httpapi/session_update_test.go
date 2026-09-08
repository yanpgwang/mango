package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
)

// These tests cover the four documented `POST /v1/sessions/{session_id}` body
// fields: `agent` (tools/mcp_servers only, idle-only), `metadata` (per-key
// patch), `title`, and the rejected `vault_ids`.
//
// Sources:
//   - api-reference/sessions/update.md
//   - guides/session-operations.md ("Updating the agent configuration")
//   - api-reference/sessions/events/list.md (BetaManagedAgentsSessionUpdatedEvent)

func updatableFixture(t *testing.T) (http.Handler, *testSessionService, string, string) {
	t.Helper()
	handler, sessions := newTestHandlerWithSessions(t, Config{}, false)
	agent := createID(t, handler, "POST", "/v1/agents",
		`{"name":"a","model":"claude-opus-4-8","tools":[{"type":"agent_toolset_20260401"}]}`)
	env := createID(t, handler, "POST", "/v1/environments",
		`{"name":"e","config":{"type":"self_hosted"}}`)
	session := createID(t, handler, "POST", "/v1/sessions",
		`{"agent":"`+agent+`","environment_id":"`+env+`","title":"t",`+
			`"metadata":{"keep":"yes","drop":"later"}}`)
	return handler, sessions, agent, session
}

func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode body %s: %v", raw, err)
	}
	return out
}

func sessionUpdatedEvents(t *testing.T, h http.Handler, id string) []map[string]any {
	t.Helper()
	rec := do(h, "GET", "/v1/sessions/"+id+"/events?types[]=session.updated", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list events -> %d: %s", rec.Code, rec.Body)
	}
	body := decodeBody(t, rec.Body.Bytes())
	raw, _ := body["data"].([]any)
	events := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		event, _ := item.(map[string]any)
		events = append(events, event)
	}
	return events
}

func TestUpdateSession_MetadataPatchesPerKey(t *testing.T) {
	h, _, _, id := updatableFixture(t)

	rec := do(h, "POST", "/v1/sessions/"+id,
		`{"metadata":{"keep":"still-yes","drop":null,"added":"new"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata patch -> %d: %s", rec.Code, rec.Body)
	}
	metadata, _ := decodeBody(t, rec.Body.Bytes())["metadata"].(map[string]any)
	if metadata["keep"] != "still-yes" || metadata["added"] != "new" {
		t.Fatalf("upsert did not apply: %#v", metadata)
	}
	if _, present := metadata["drop"]; present {
		t.Fatalf("null did not delete the key: %#v", metadata)
	}

	// Omitting metadata preserves the patched bag rather than clearing it.
	rec = do(h, "POST", "/v1/sessions/"+id, `{"title":"t2"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("title update -> %d: %s", rec.Code, rec.Body)
	}
	metadata, _ = decodeBody(t, rec.Body.Bytes())["metadata"].(map[string]any)
	if len(metadata) != 2 || metadata["keep"] != "still-yes" {
		t.Fatalf("omitted metadata was not preserved: %#v", metadata)
	}
}

func TestUpdateSession_RejectsNonStringMetadataValues(t *testing.T) {
	h, _, _, id := updatableFixture(t)
	rec := do(h, "POST", "/v1/sessions/"+id, `{"metadata":{"bad":1}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestUpdateSession_AgentToolsReplaceClearAndPreserve(t *testing.T) {
	h, _, _, id := updatableFixture(t)

	// Full replacement, including an MCP server the new toolset references.
	rec := do(h, "POST", "/v1/sessions/"+id, `{"agent":{`+
		`"tools":[{"type":"agent_toolset_20260401"},`+
		`{"type":"mcp_toolset","mcp_server_name":"linear"}],`+
		`"mcp_servers":[{"type":"url","name":"linear","url":"https://mcp.example.com/sse"}]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("replace tools -> %d: %s", rec.Code, rec.Body)
	}
	agent, _ := decodeBody(t, rec.Body.Bytes())["agent"].(map[string]any)
	tools, _ := agent["tools"].([]any)
	servers, _ := agent["mcp_servers"].([]any)
	if len(tools) != 2 || len(servers) != 1 {
		t.Fatalf("replacement snapshot = %#v", agent)
	}

	// Omitting the arrays preserves them.
	rec = do(h, "POST", "/v1/sessions/"+id, `{"title":"unrelated"}`)
	agent, _ = decodeBody(t, rec.Body.Bytes())["agent"].(map[string]any)
	if tools, _ = agent["tools"].([]any); len(tools) != 2 {
		t.Fatalf("omitted tools were not preserved: %#v", agent["tools"])
	}

	// An empty array clears.
	rec = do(h, "POST", "/v1/sessions/"+id, `{"agent":{"tools":[],"mcp_servers":[]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear tools -> %d: %s", rec.Code, rec.Body)
	}
	agent, _ = decodeBody(t, rec.Body.Bytes())["agent"].(map[string]any)
	tools, _ = agent["tools"].([]any)
	servers, _ = agent["mcp_servers"].([]any)
	if len(tools) != 0 || len(servers) != 0 {
		t.Fatalf("cleared snapshot = %#v", agent)
	}
}

func TestUpdateSession_AgentUpdateStaysSessionLocal(t *testing.T) {
	h, _, agentID, id := updatableFixture(t)

	rec := do(h, "POST", "/v1/sessions/"+id, `{"agent":{"tools":[]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear tools -> %d: %s", rec.Code, rec.Body)
	}
	sessionAgent, _ := decodeBody(t, rec.Body.Bytes())["agent"].(map[string]any)
	if tools, _ := sessionAgent["tools"].([]any); len(tools) != 0 {
		t.Fatalf("session snapshot still has tools: %#v", sessionAgent["tools"])
	}
	if sessionAgent["version"] != float64(1) {
		t.Fatalf("session-local update renumbered the snapshot: %#v", sessionAgent["version"])
	}

	// The underlying agent resource must be untouched.
	rec = do(h, "GET", "/v1/agents/"+agentID, "")
	resource := decodeBody(t, rec.Body.Bytes())
	if tools, _ := resource["tools"].([]any); len(tools) != 1 {
		t.Fatalf("session update propagated to the agent resource: %#v", resource["tools"])
	}
	if resource["version"] != float64(1) {
		t.Fatalf("agent version = %#v, want 1", resource["version"])
	}
}

func TestUpdateSession_AgentRequiresAnIdleSession(t *testing.T) {
	h, sessions, _, id := updatableFixture(t)
	sessions.forceStatus(id, domain.StatusRunning)

	rec := do(h, "POST", "/v1/sessions/"+id, `{"agent":{"tools":[]}}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("agent update while running -> %d, want 409: %s", rec.Code, rec.Body)
	}

	// Title and metadata carry no such precondition.
	rec = do(h, "POST", "/v1/sessions/"+id, `{"title":"while running"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("title update while running -> %d: %s", rec.Code, rec.Body)
	}
}

func TestUpdateSession_RejectsNonUpdatableAgentFields(t *testing.T) {
	h, _, _, id := updatableFixture(t)
	for _, body := range []string{
		`{"agent":{"model":"claude-opus-4-8"}}`,
		`{"agent":{"system":"new"}}`,
		`{"agent":{"skills":[]}}`,
		`{"agent":{"name":"nope"}}`,
	} {
		rec := do(h, "POST", "/v1/sessions/"+id, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s -> %d, want 400: %s", body, rec.Code, rec.Body)
		}
	}
}

func TestUpdateSession_RejectsVaultIDs(t *testing.T) {
	h, _, _, id := updatableFixture(t)
	for _, body := range []string{
		`{"vault_ids":["vlt_1"]}`,
		`{"vault_ids":[]}`,
		`{"vault_ids":null}`,
	} {
		rec := do(h, "POST", "/v1/sessions/"+id, body)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("%s -> %d, want 422: %s", body, rec.Code, rec.Body)
		}
		envelope := decodeBody(t, rec.Body.Bytes())
		failure, _ := envelope["error"].(map[string]any)
		message, _ := failure["message"].(string)
		if message != vaultIDsUpdateRejectedMessage {
			t.Fatalf("vault_ids message = %q", message)
		}
	}
}

func TestCreateSession_AcceptsAndEchoesVaultIDs(t *testing.T) {
	h := NewTestHandler(t)
	agent := createID(t, h, "POST", "/v1/agents",
		`{"name":"a","model":"claude-opus-4-8"}`)
	environment := createID(t, h, "POST", "/v1/environments",
		`{"name":"e","config":{"type":"self_hosted"}}`)
	rec := do(h, "POST", "/v1/sessions", `{"agent":"`+agent+`",`+
		`"environment_id":"`+environment+`","vault_ids":["vlt_1"]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("vault_ids -> %d, want 200: %s", rec.Code, rec.Body)
	}
	response := decodeBody(t, rec.Body.Bytes())
	vaultIDs, _ := response["vault_ids"].([]any)
	if len(vaultIDs) != 1 || vaultIDs[0] != "vlt_1" {
		t.Fatalf("vault_ids = %#v", response["vault_ids"])
	}
}

func TestUpdateSession_EmitsOnlyChangedFields(t *testing.T) {
	h, _, _, id := updatableFixture(t)

	if rec := do(h, "POST", "/v1/sessions/"+id, `{"title":"changed"}`); rec.Code != 200 {
		t.Fatalf("title update -> %d: %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", "/v1/sessions/"+id, `{"agent":{"tools":[]}}`); rec.Code != 200 {
		t.Fatalf("agent update -> %d: %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", "/v1/sessions/"+id, `{"metadata":{"added":"v"}}`); rec.Code != 200 {
		t.Fatalf("metadata update -> %d: %s", rec.Code, rec.Body)
	}
	// A request that changes nothing must not emit a fourth event.
	if rec := do(h, "POST", "/v1/sessions/"+id, `{"title":"changed"}`); rec.Code != 200 {
		t.Fatalf("no-op update -> %d: %s", rec.Code, rec.Body)
	}

	events := sessionUpdatedEvents(t, h, id)
	if len(events) != 3 {
		t.Fatalf("session.updated count = %d, want 3", len(events))
	}
	for index, want := range []string{"title", "agent", "metadata"} {
		event := events[index]
		if _, present := event[want]; !present {
			t.Fatalf("event %d missing %q: %#v", index, want, event)
		}
		for _, other := range []string{"title", "agent", "metadata"} {
			if other == want {
				continue
			}
			if _, present := event[other]; present {
				t.Fatalf("event %d carries unchanged %q: %#v", index, other, event)
			}
		}
		// Envelope fields documented on every event, and never vault_ids.
		if event["type"] != "session.updated" || event["id"] == nil {
			t.Fatalf("event %d envelope = %#v", index, event)
		}
		if _, present := event["vault_ids"]; present {
			t.Fatalf("event %d carries vault_ids: %#v", index, event)
		}
	}
	agent, _ := events[1]["agent"].(map[string]any)
	if agent["type"] != "agent" {
		t.Fatalf("agent payload is not a resolved session agent: %#v", events[1]["agent"])
	}
	if tools, _ := agent["tools"].([]any); len(tools) != 0 {
		t.Fatalf("agent payload tools = %#v, want the cleared list", agent["tools"])
	}
}

func TestUpdateSession_MissingSessionIsNotFound(t *testing.T) {
	h := NewTestHandler(t)
	rec := do(h, "POST", "/v1/sessions/sesn_missing", `{"title":"x"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body)
	}
}
