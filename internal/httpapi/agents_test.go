package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/domain"
)

func TestAgents_CreateGetVersionArchive(t *testing.T) {
	srv := newTestServer(t)
	// create
	body := `{"name":"SRE Agent","model":"claude-opus-4-8","system":"help"}`
	rec := do(srv, "POST", "/v1/agents", body)
	if rec.Code != 200 {
		t.Fatalf("create status %d: %s", rec.Code, rec.Body)
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	if created["version"].(float64) != 1 || created["type"] != "agent" {
		t.Fatalf("bad create body: %v", created)
	}
	id := created["id"].(string)
	// update -> version 2
	rec = do(srv, "POST", "/v1/agents/"+id, `{"name":"SRE v2"}`)
	var up map[string]any
	json.Unmarshal(rec.Body.Bytes(), &up)
	if up["version"].(float64) != 2 {
		t.Fatalf("expected v2, got %v", up["version"])
	}
	// versions list has 2
	rec = do(srv, "GET", "/v1/agents/"+id+"/versions", "")
	var vs map[string]any
	json.Unmarshal(rec.Body.Bytes(), &vs)
	if len(vs["data"].([]any)) != 2 {
		t.Fatalf("expected 2 versions, got %v", vs["data"])
	}
	// archive
	rec = do(srv, "POST", "/v1/agents/"+id+"/archive", "")
	if rec.Code != 200 {
		t.Fatalf("archive status %d", rec.Code)
	}
	var archived map[string]any
	json.Unmarshal(rec.Body.Bytes(), &archived)
	if archived["version"].(float64) != 2 {
		t.Fatalf("archive created a configuration version: %v", archived["version"])
	}
	rec = do(srv, "GET", "/v1/agents/"+id+"/versions", "")
	json.Unmarshal(rec.Body.Bytes(), &vs)
	if len(vs["data"].([]any)) != 2 {
		t.Fatalf("archive appended version history: %v", vs["data"])
	}
	rec = do(srv, "POST", "/v1/agents/"+id, `{"name":"must fail"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("archived agent update status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

// TestAgents_ClearSystemWithNull verifies that sending {"system":null} clears the
// system field, that the version bumps, and that a subsequent update without the
// system key does NOT resurrect the field.
func TestAgents_ClearSystemWithNull(t *testing.T) {
	srv := newTestServer(t)

	// create agent with system="help"
	rec := do(srv, "POST", "/v1/agents", `{"name":"Agent","model":"claude-opus-4-8","system":"help"}`)
	if rec.Code != 200 {
		t.Fatalf("create status %d: %s", rec.Code, rec.Body)
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	// update with explicit null -> should clear system
	rec = do(srv, "POST", "/v1/agents/"+id, `{"system":null}`)
	if rec.Code != 200 {
		t.Fatalf("update (null system) status %d: %s", rec.Code, rec.Body)
	}
	var up1 map[string]any
	json.Unmarshal(rec.Body.Bytes(), &up1)
	if up1["version"].(float64) != 2 {
		t.Fatalf("expected version 2 after clearing system, got %v", up1["version"])
	}
	if up1["system"] != nil {
		t.Fatalf("expected system=null after clearing, got %v", up1["system"])
	}

	// GET to confirm persisted state
	rec = do(srv, "GET", "/v1/agents/"+id, "")
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["system"] != nil {
		t.Fatalf("GET: expected system=null, got %v", got["system"])
	}

	// update without system key -> system must stay null, no resurrection
	rec = do(srv, "POST", "/v1/agents/"+id, `{"name":"renamed"}`)
	if rec.Code != 200 {
		t.Fatalf("name-only update status %d: %s", rec.Code, rec.Body)
	}
	var up2 map[string]any
	json.Unmarshal(rec.Body.Bytes(), &up2)
	if up2["system"] != nil {
		t.Fatalf("absent system key resurrected system field; got %v", up2["system"])
	}
}

// TestAgents_UpdateModelNullRejected verifies that model, unlike nullable
// system/description, cannot be cleared.
func TestAgents_UpdateModelNullRejected(t *testing.T) {
	srv := newTestServer(t)

	// create agent with a named model
	rec := do(srv, "POST", "/v1/agents", `{"name":"Agent","model":"claude-opus-4-8"}`)
	if rec.Code != 200 {
		t.Fatalf("create status %d: %s", rec.Code, rec.Body)
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	// update with model:null must be rejected
	rec = do(srv, "POST", "/v1/agents/"+id, `{"model":null,"name":"y"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("update status %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestAgents_UpdateArrayNullClears(t *testing.T) {
	srv := newTestServer(t)
	rec := do(srv, "POST", "/v1/agents",
		`{"name":"Agent","model":"claude-opus-4-8",`+
			`"tools":[{"type":"agent_toolset_20260401"},{"type":"custom","name":"x","description":"x",`+
			`"input_schema":{"type":"object"}},{"type":"mcp_toolset","mcp_server_name":"m"}],`+
			`"mcp_servers":[{"type":"url","name":"m","url":"https://example.com"}],`+
			`"skills":[{"type":"custom","skill_id":"skill_clear","version":"1"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d: %s", rec.Code, rec.Body)
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	rec = do(srv, "POST", "/v1/agents/"+id,
		`{"tools":null,"mcp_servers":null,"skills":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear: %d: %s", rec.Code, rec.Body)
	}
	var updated map[string]any
	json.Unmarshal(rec.Body.Bytes(), &updated)
	for _, field := range []string{"tools", "mcp_servers", "skills"} {
		values, ok := updated[field].([]any)
		if !ok || len(values) != 0 {
			t.Errorf("%s was not cleared: %#v", field, updated[field])
		}
	}

	rec = do(srv, "POST", "/v1/agents/"+id, `{"tools":"not-an-array"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid tools status = %d, want 400: %s", rec.Code, rec.Body)
	}
}

func TestAgents_SkillReferencesAreStrictResolvedAndProviderAware(t *testing.T) {
	srv := newTestServer(t)
	rec := do(srv, "POST", "/v1/agents",
		`{"name":"Agent","model":"claude-opus-4-8",`+
			`"tools":[{"type":"agent_toolset_20260401"}],`+
			`"skills":[{"type":"custom","skill_id":"skill_reports","version":"latest"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create custom Skill reference: %d: %s", rec.Code, rec.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	resolved := created["skills"].([]any)[0].(map[string]any)
	if resolved["version"] != "1759178010641129" {
		t.Fatalf("resolved Skill = %#v", resolved)
	}

	cases := []struct {
		name   string
		skills string
		status int
	}{
		{"anthropic unsupported", `[{"type":"anthropic","skill_id":"xlsx"}]`, http.StatusUnprocessableEntity},
		{"missing custom", `[{"type":"custom","skill_id":"skill_missing"}]`, http.StatusBadRequest},
		{"unknown provider", `[{"type":"third_party","skill_id":"x"}]`, http.StatusBadRequest},
		{"unknown field", `[{"type":"custom","skill_id":"skill_x","extra":true}]`, http.StatusBadRequest},
		{"numeric version", `[{"type":"custom","skill_id":"skill_x","version":1}]`, http.StatusBadRequest},
		{"empty version", `[{"type":"custom","skill_id":"skill_x","version":""}]`, http.StatusBadRequest},
		{"not an array", `{}`, http.StatusBadRequest},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			rec := do(srv, "POST", "/v1/agents",
				`{"name":"Agent","model":"claude-opus-4-8",`+
					`"tools":[{"type":"agent_toolset_20260401"}],`+
					`"skills":`+test.skills+`}`)
			if rec.Code != test.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, test.status, rec.Body)
			}
		})
	}
}

func TestAgents_LegacySkillResponsePreservesOpaqueValues(t *testing.T) {
	var skills []domain.SkillReference
	if err := json.Unmarshal([]byte(`[
        "former-provider-value",
        {"type":"custom","skill_id":"skill_old","version":"1","extension":true}
    ]`), &skills); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(agentToJSON(domain.Agent{
		ID: "agent_legacy", Version: 1, Name: "legacy",
		Model: domain.Model{ID: "claude-test"}, Skills: skills,
		CreatedAt: time.Unix(1, 0).UTC(), UpdatedAt: time.Unix(1, 0).UTC(),
	}))
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatal(err)
	}
	values, ok := response["skills"].([]any)
	if !ok || len(values) != 2 || values[0] != "former-provider-value" {
		t.Fatalf("legacy Skill response = %#v", response["skills"])
	}
	object, ok := values[1].(map[string]any)
	if !ok || object["extension"] != true {
		t.Fatalf("legacy Skill response lost extension: %#v", response["skills"])
	}
}

func TestAgents_MultiagentObjectPersistsAndReplaces(t *testing.T) {
	srv := newTestServer(t)
	createPeer := func(name string) string {
		t.Helper()
		rec := do(srv, "POST", "/v1/agents", `{"name":"`+name+`","model":"claude-opus-4-8"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("create peer status %d: %s", rec.Code, rec.Body)
		}
		var peer map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &peer); err != nil {
			t.Fatal(err)
		}
		return peer["id"].(string)
	}
	firstID := createPeer("First")
	secondID := createPeer("Second")
	rec := do(srv, "POST", "/v1/agents",
		`{"name":"Coordinator","model":"claude-opus-4-8","multiagent":{"type":"coordinator","agents":["`+firstID+`"]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status %d: %s", rec.Code, rec.Body)
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)
	multiagent, ok := created["multiagent"].(map[string]any)
	if !ok || multiagent["type"] != "coordinator" {
		t.Fatalf("create response lost multiagent: %#v", created["multiagent"])
	}
	createdEntry := multiagent["agents"].([]any)[0].(map[string]any)
	if createdEntry["type"] != "agent" || createdEntry["id"] != firstID || createdEntry["version"] != float64(1) {
		t.Fatalf("create response did not resolve roster: %#v", multiagent)
	}

	rec = do(srv, "POST", "/v1/agents/"+id,
		`{"multiagent":{"type":"coordinator","agents":["`+secondID+`"]}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("update status %d: %s", rec.Code, rec.Body)
	}
	var updated map[string]any
	json.Unmarshal(rec.Body.Bytes(), &updated)
	updatedMultiagent := updated["multiagent"].(map[string]any)
	agents := updatedMultiagent["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("multiagent was not replaced: %#v", updatedMultiagent)
	}
	entry := agents[0].(map[string]any)
	if entry["id"] != secondID || entry["version"] != float64(1) {
		t.Fatalf("multiagent was not replaced: %#v", updatedMultiagent)
	}

	rec = do(srv, "POST", "/v1/agents/"+id, `{"name":"renamed"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("name-only update status %d: %s", rec.Code, rec.Body)
	}
	rec = do(srv, "GET", "/v1/agents/"+id, "")
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	gotAgents := got["multiagent"].(map[string]any)["agents"].([]any)
	if len(gotAgents) != 1 {
		t.Fatalf("omitted multiagent did not preserve stored value: %#v", got["multiagent"])
	}
	gotEntry := gotAgents[0].(map[string]any)
	if gotEntry["id"] != secondID || gotEntry["version"] != float64(1) {
		t.Fatalf("omitted multiagent did not preserve stored value: %#v", got["multiagent"])
	}

	rec = do(srv, "POST", "/v1/agents/"+id, `{"multiagent":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("clear status %d: %s", rec.Code, rec.Body)
	}
	var cleared map[string]any
	json.Unmarshal(rec.Body.Bytes(), &cleared)
	if cleared["multiagent"] != nil {
		t.Fatalf("explicit null did not clear multiagent: %#v", cleared["multiagent"])
	}
	rec = do(srv, "GET", "/v1/agents/"+id, "")
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["multiagent"] != nil {
		t.Fatalf("cleared multiagent was not persisted: %#v", got["multiagent"])
	}
}

func TestAgents_MultiagentNullAndInvalidShapes(t *testing.T) {
	for _, invalid := range []string{
		`[]`, `"coordinator"`, "1", "true", `{}`,
		`{"type":"worker","agents":[{"type":"self"}]}`,
		`{"type":"coordinator","agents":[]}`,
		`{"type":"coordinator","agents":[null]}`,
		`{"type":"coordinator","agents":[{"type":"agent","id":""}]}`,
		`{"type":"coordinator","agents":[{"type":"agent","id":"agent_x","version":0}]}`,
		`{"type":"coordinator","agents":[{"type":"self","version":1}]}`,
		`{"type":"coordinator","agents":[{"type":"other"}]}`,
		`{"type":"coordinator","agents":[{"type":"self"}],"extension":true}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			srv := newTestServer(t)
			body := `{"name":"Agent","model":"claude-opus-4-8","multiagent":` + invalid + `}`
			rec := do(srv, "POST", "/v1/agents", body)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
			}
		})
	}

	srv := newTestServer(t)
	rec := do(srv, "POST", "/v1/agents",
		`{"name":"Agent","model":"claude-opus-4-8","multiagent":null}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create null status = %d, want 200: %s", rec.Code, rec.Body)
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	if created["multiagent"] != nil {
		t.Fatalf("create null should leave multiagent unset: %#v", created["multiagent"])
	}
}

func TestAgents_ModelEffortAcceptsOfficialInputShapes(t *testing.T) {
	srv := newTestServer(t)
	asString := do(srv, "POST", "/v1/agents",
		`{"name":"Agent","model":{"id":"claude-opus-4-8","effort":"high"}}`)
	if asString.Code != http.StatusOK {
		t.Fatalf("string effort status = %d, want 200: %s", asString.Code, asString.Body)
	}
	var stringResult map[string]any
	if err := json.Unmarshal(asString.Body.Bytes(), &stringResult); err != nil {
		t.Fatal(err)
	}
	effort := stringResult["model"].(map[string]any)["effort"].(map[string]any)
	if effort["type"] != "high" {
		t.Fatalf("canonical effort response = %#v", effort)
	}

	asObject := do(srv, "POST", "/v1/agents",
		`{"name":"Agent","model":{"id":"claude-opus-4-8","effort":{"type":"high"},"speed":"standard"}}`)
	if asObject.Code != http.StatusOK {
		t.Fatalf("tagged effort status = %d, want 200: %s", asObject.Code, asObject.Body)
	}

	invalid := do(srv, "POST", "/v1/agents",
		`{"name":"Agent","model":{"id":"claude-opus-4-8","effort":{"type":"high","extra":true}}}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid effort object status = %d, want 400: %s", invalid.Code, invalid.Body)
	}
}

func TestAgents_InferenceGeoIsNotPartOfModelConfiguration(t *testing.T) {
	srv := newTestServer(t)
	rec := do(srv, http.MethodPost, "/v1/agents",
		`{"name":"Agent","model":{"id":"claude-opus-4-8","inference_geo":"us"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body)
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "error" || body.Error.Type != "invalid_request_error" ||
		body.Error.Message != `unknown model field "inference_geo"` {
		t.Fatalf("error envelope = %+v", body)
	}
}

func TestAgents_MetadataValidationUsesResultingBag(t *testing.T) {
	srv := newTestServer(t)
	rec := do(srv, "POST", "/v1/agents",
		`{"name":"Agent","model":"claude-opus-4-8","metadata":{"bad":1}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-string metadata status = %d, want 400: %s", rec.Code, rec.Body)
	}

	metadata := make(map[string]string, 16)
	for i := 0; i < 16; i++ {
		metadata[string(rune('a'+i))] = "v"
	}
	body, err := json.Marshal(map[string]any{
		"name": "Agent", "model": "claude-opus-4-8", "metadata": metadata,
	})
	if err != nil {
		t.Fatal(err)
	}
	rec = do(srv, "POST", "/v1/agents", string(body))
	if rec.Code != http.StatusOK {
		t.Fatalf("create at metadata limit status %d: %s", rec.Code, rec.Body)
	}
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	id := created["id"].(string)

	rec = do(srv, "POST", "/v1/agents/"+id, `{"metadata":{"overflow":"v"}}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("overflow patch status = %d, want 400: %s", rec.Code, rec.Body)
	}
	rec = do(srv, "POST", "/v1/agents/"+id, `{"metadata":{"a":null,"replacement":"v"}}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete-and-add patch status %d: %s", rec.Code, rec.Body)
	}
	var updated map[string]any
	json.Unmarshal(rec.Body.Bytes(), &updated)
	gotMetadata := updated["metadata"].(map[string]any)
	if len(gotMetadata) != 16 || gotMetadata["replacement"] != "v" {
		t.Fatalf("metadata patch result = %#v", gotMetadata)
	}
}

func TestAgents_RejectsUnsupportedMCPServerFieldsWithoutPersisting(t *testing.T) {
	srv := newTestServer(t)
	const secret = "sk-must-not-be-stored"
	invalidServers := `[{"type":"url","name":"github",` +
		`"url":"https://example.com/mcp","authorization_token":"` + secret + `"}]`
	tools := `[{"type":"mcp_toolset","mcp_server_name":"github"}]`

	rec := do(srv, "POST", "/v1/agents", `{"name":"Agent","model":"claude-opus-4-8",`+
		`"tools":`+tools+`,"mcp_servers":`+invalidServers+`}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid create status = %d, want 400: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("create error echoed rejected value: %s", rec.Body)
	}

	rec = do(srv, "POST", "/v1/agents", `{"name":"Agent","model":"claude-opus-4-8",`+
		`"tools":`+tools+`,"mcp_servers":[{"type":"url","name":"github",`+
		`"url":"https://example.com/mcp"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid create status = %d: %s", rec.Code, rec.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created["id"].(string)

	rec = do(srv, "POST", "/v1/agents/"+id, `{"mcp_servers":`+invalidServers+`}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid update status = %d, want 400: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("update error echoed rejected value: %s", rec.Body)
	}

	for _, path := range []string{
		"/v1/agents", "/v1/agents/" + id, "/v1/agents/" + id + "/versions",
	} {
		read := do(srv, "GET", path, "")
		if read.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d: %s", path, read.Code, read.Body)
		}
		if strings.Contains(read.Body.String(), secret) {
			t.Fatalf("GET %s exposed rejected value: %s", path, read.Body)
		}
	}

	versions := do(srv, "GET", "/v1/agents/"+id+"/versions", "")
	var page map[string]any
	if err := json.Unmarshal(versions.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if data, _ := page["data"].([]any); len(data) != 1 {
		t.Fatalf("rejected update created an Agent version: %#v", page["data"])
	}
}

func TestAgents_RejectsUnsupportedToolFieldsWithoutCreatingVersion(t *testing.T) {
	srv := newTestServer(t)
	rec := do(srv, "POST", "/v1/agents", `{"name":"Agent","model":"claude-opus-4-8",`+
		`"tools":[{"type":"agent_toolset_20260401"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created["id"].(string)

	rec = do(srv, "POST", "/v1/agents/"+id,
		`{"tools":[{"type":"agent_toolset_20260401","default_config":{`+
			`"enabled":true,"authorization_token":"must-not-be-stored"}}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid update status = %d, want 400: %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "must-not-be-stored") {
		t.Fatalf("error echoed rejected value: %s", rec.Body)
	}

	versions := do(srv, "GET", "/v1/agents/"+id+"/versions", "")
	var page map[string]any
	if err := json.Unmarshal(versions.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if data, _ := page["data"].([]any); len(data) != 1 {
		t.Fatalf("rejected update created an Agent version: %#v", page["data"])
	}
}

func TestAgents_RejectsMalformedToolValuesWithoutCreatingVersion(t *testing.T) {
	srv := newTestServer(t)
	rec := do(srv, "POST", "/v1/agents", `{"name":"Agent","model":"claude-opus-4-8",`+
		`"tools":[{"type":"agent_toolset_20260401"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created["id"].(string)

	rec = do(srv, "POST", "/v1/agents/"+id,
		`{"tools":[{"type":"agent_toolset_20260401",`+
			`"default_config":{"enabled":"yes"}}]}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid update status = %d, want 400: %s", rec.Code, rec.Body)
	}

	versions := do(srv, "GET", "/v1/agents/"+id+"/versions", "")
	var page map[string]any
	if err := json.Unmarshal(versions.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if data, _ := page["data"].([]any); len(data) != 1 {
		t.Fatalf("rejected update created an Agent version: %#v", page["data"])
	}
}

func TestAgents_RejectsInvalidCustomToolContractWithoutCreatingVersion(t *testing.T) {
	srv := newTestServer(t)
	invalidTools := []string{
		`[{"type":"custom","name":"weather","input_schema":{"type":"object"}}]`,
		`[{"type":"custom","name":"weather","description":"Look up weather.","input_schema":{}}]`,
		`[{"type":"custom","name":"weather.lookup","description":"Look up weather.",` +
			`"input_schema":{"type":"object"}}]`,
	}
	for _, tools := range invalidTools {
		rec := do(srv, "POST", "/v1/agents", `{"name":"Agent","model":"claude-opus-4-8",`+
			`"tools":`+tools+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid create status = %d, want 400: %s", rec.Code, rec.Body)
		}
	}

	rec := do(srv, "POST", "/v1/agents", `{"name":"Agent","model":"claude-opus-4-8"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created["id"].(string)

	for _, tools := range invalidTools {
		rec = do(srv, "POST", "/v1/agents/"+id, `{"tools":`+tools+`}`)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid update status = %d, want 400: %s", rec.Code, rec.Body)
		}
	}

	versions := do(srv, "GET", "/v1/agents/"+id+"/versions", "")
	var page map[string]any
	if err := json.Unmarshal(versions.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if data, _ := page["data"].([]any); len(data) != 1 {
		t.Fatalf("rejected updates created Agent versions: %#v", page["data"])
	}
}

func TestAgents_RejectsToolCollectionAboveLimitWithoutCreatingVersion(t *testing.T) {
	srv := newTestServer(t)
	tools := make([]map[string]any, 129)
	for index := range tools {
		tools[index] = map[string]any{
			"type": "custom", "name": fmt.Sprintf("tool_%d", index), "description": "d",
			"input_schema": map[string]any{"type": "object"},
		}
	}
	encodedTools, err := json.Marshal(tools)
	if err != nil {
		t.Fatal(err)
	}

	rec := do(srv, "POST", "/v1/agents", `{"name":"Agent","model":"claude-opus-4-8",`+
		`"tools":`+string(encodedTools)+`}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over-limit create status = %d, want 400: %s", rec.Code, rec.Body)
	}

	rec = do(srv, "POST", "/v1/agents", `{"name":"Agent","model":"claude-opus-4-8"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", rec.Code, rec.Body)
	}
	var created map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	id := created["id"].(string)

	rec = do(srv, "POST", "/v1/agents/"+id, `{"tools":`+string(encodedTools)+`}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("over-limit update status = %d, want 400: %s", rec.Code, rec.Body)
	}

	versions := do(srv, "GET", "/v1/agents/"+id+"/versions", "")
	var page map[string]any
	if err := json.Unmarshal(versions.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if data, _ := page["data"].([]any); len(data) != 1 {
		t.Fatalf("rejected update created an Agent version: %#v", page["data"])
	}
}

// helpers used across httpapi black-box tests
func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	return NewTestHandler(t)
}

func do(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
