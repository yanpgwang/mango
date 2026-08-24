package httpapi

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/yanpgwang/mango/internal/app"
	"gopkg.in/yaml.v3"
)

func TestOpenAPIServed(t *testing.T) {
	h := NewTestHandler(t)
	rec := do(h, "GET", "/openapi.yaml", "")
	if rec.Code != 200 {
		t.Fatalf("openapi status %d", rec.Code)
	}
	if len(rec.Body.String()) < 50 {
		t.Fatal("openapi doc too short")
	}
}

func TestOpenAPITransportContract(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	security, ok := doc["security"].([]any)
	if !ok || len(security) != 1 {
		t.Fatalf("global security = %#v, want one bearer requirement", doc["security"])
	}
	requirement := openAPIMap(t, security[0], "global security requirement")
	if len(requirement) != 1 || requirement["BearerAuth"] == nil {
		t.Fatalf("global security requirement = %#v, want BearerAuth", requirement)
	}
	components := openAPIMap(t, doc["components"], "components")
	schemes := openAPIMap(t, components["securitySchemes"], "security schemes")
	if len(schemes) != 1 {
		t.Fatalf("security schemes = %#v, want only BearerAuth", schemes)
	}
	bearer := openAPIMap(t, schemes["BearerAuth"], "BearerAuth")
	if bearer["type"] != "http" || bearer["scheme"] != "bearer" {
		t.Fatalf("BearerAuth = %#v", bearer)
	}

	paths := openAPIMap(t, doc["paths"], "paths")
	for _, path := range []string{"/healthz", "/readyz", "/openapi.yaml"} {
		get := openAPIMap(t, openAPIMap(t, paths[path], path)["get"], "get "+path)
		public, ok := get["security"].([]any)
		if !ok || len(public) != 0 {
			t.Fatalf("%s security = %#v, want []", path, get["security"])
		}
	}

	pollPath := openAPIMap(t, paths["/v1/environments/{environment_id}/work/poll"], "poll path")
	poll := openAPIMap(t, pollPath["get"], "poll operation")
	parameters := poll["parameters"].([]any)
	var worker map[string]any
	for _, parameter := range parameters {
		resolved := resolveOpenAPIRef(t, doc, parameter)
		if resolved["name"] == "worker_id" {
			worker = resolved
		}
	}
	if worker == nil || worker["in"] != "query" {
		t.Fatalf("worker_id parameter = %#v, want query parameter", worker)
	}
}

func TestOpenAPIResourceLifecycleContract(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	paths := openAPIMap(t, doc["paths"], "paths")
	operations := map[string][]string{
		"/v1/agents":                                {"get", "post"},
		"/v1/agents/{agent_id}":                     {"get", "post"},
		"/v1/agents/{agent_id}/versions":            {"get"},
		"/v1/agents/{agent_id}/archive":             {"post"},
		"/v1/environments":                          {"get", "post"},
		"/v1/environments/{environment_id}":         {"delete", "get", "post"},
		"/v1/environments/{environment_id}/archive": {"post"},
		"/v1/sessions":                              {"get", "post"},
		"/v1/sessions/{session_id}":                 {"delete", "get", "post"},
		"/v1/sessions/{session_id}/archive":         {"post"},
	}
	requestBodies := map[string]bool{
		"post /v1/agents":                        true,
		"post /v1/agents/{agent_id}":             true,
		"post /v1/environments":                  true,
		"post /v1/environments/{environment_id}": true,
		"post /v1/sessions":                      true,
		"post /v1/sessions/{session_id}":         true,
	}

	seenOperationIDs := map[string]bool{}
	for path, methods := range operations {
		pathItem := openAPIMap(t, paths[path], "path "+path)
		for _, method := range methods {
			operation := openAPIMap(t, pathItem[method], method+" "+path)
			operationID, ok := operation["operationId"].(string)
			if !ok || operationID == "" || seenOperationIDs[operationID] {
				t.Fatalf("%s %s has missing or duplicate operationId %#v", method, path, operation["operationId"])
			}
			seenOperationIDs[operationID] = true

			responses := openAPIMap(t, operation["responses"], method+" "+path+" responses")
			success := resolveOpenAPIRef(t, doc, responses["200"])
			content := openAPIMap(t, success["content"], method+" "+path+" success content")
			media := openAPIMap(t, content["application/json"], method+" "+path+" JSON response")
			if _, ok := openAPIMap(t, media["schema"], method+" "+path+" response schema")["$ref"]; !ok {
				t.Fatalf("%s %s success response does not name a reusable schema", method, path)
			}

			key := method + " " + path
			_, hasBody := operation["requestBody"]
			if hasBody != requestBodies[key] {
				t.Fatalf("%s requestBody presence = %t", key, hasBody)
			}
			if hasBody {
				body := openAPIMap(t, operation["requestBody"], key+" requestBody")
				bodyContent := openAPIMap(t, body["content"], key+" request content")
				bodyJSON := openAPIMap(t, bodyContent["application/json"], key+" JSON request")
				if _, ok := openAPIMap(t, bodyJSON["schema"], key+" request schema")["$ref"]; !ok {
					t.Fatalf("%s request does not name a reusable schema", key)
				}
			}
		}
	}
	if len(seenOperationIDs) != 18 {
		t.Fatalf("resource lifecycle operation count = %d, want 18", len(seenOperationIDs))
	}
	validateOpenAPIRefs(t, doc, doc)
}

func TestOpenAPISessionEventContract(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	paths := openAPIMap(t, doc["paths"], "paths")
	eventsPath := openAPIMap(t, paths["/v1/sessions/{session_id}/events"], "events path")

	post := openAPIMap(t, eventsPath["post"], "send events operation")
	requestBody := openAPIMap(t, post["requestBody"], "send events request body")
	requestContent := openAPIMap(t, requestBody["content"], "send events request content")
	requestJSON := openAPIMap(t, requestContent["application/json"], "send events JSON request")
	assertOpenAPIRef(t, requestJSON["schema"], "#/components/schemas/SendSessionEventsRequest")
	postResponses := openAPIMap(t, post["responses"], "send events responses")
	assertOpenAPIRef(t, postResponses["200"], "#/components/responses/SessionEventBatchResponse")

	get := openAPIMap(t, eventsPath["get"], "list events operation")
	assertOpenAPIParameterNames(t, doc, get["parameters"], []string{
		"limit", "order", "page", "types[]", "created_at[gt]", "created_at[gte]",
		"created_at[lt]", "created_at[lte]",
	})
	getResponses := openAPIMap(t, get["responses"], "list events responses")
	assertOpenAPIRef(t, getResponses["200"], "#/components/responses/SessionEventListResponse")

	streamPath := openAPIMap(t, paths["/v1/sessions/{session_id}/events/stream"], "stream path")
	stream := openAPIMap(t, streamPath["get"], "stream events operation")
	assertOpenAPIParameterNames(t, doc, stream["parameters"], []string{"event_deltas[]"})
	streamResponses := openAPIMap(t, stream["responses"], "stream responses")
	streamSuccess := resolveOpenAPIRef(t, doc, streamResponses["200"])
	streamContent := openAPIMap(t, streamSuccess["content"], "stream success content")
	sse := openAPIMap(t, streamContent["text/event-stream"], "SSE media type")
	assertOpenAPIRef(t, sse["schema"], "#/components/schemas/EventStreamFrame")
	frames := openAPIMap(t, sse["x-sse-event-schemas"], "SSE event schemas")
	assertOpenAPIRef(t, frames["persisted"], "#/components/schemas/SessionEvent")
	assertOpenAPIRef(t, frames["event_start"], "#/components/schemas/EventStart")
	assertOpenAPIRef(t, frames["event_delta"], "#/components/schemas/EventDelta")

	assertOpenAPIEventUnion(t, doc, "ClientSessionEventInput", []string{
		"user.message", "user.interrupt", "user.tool_confirmation",
		"user.custom_tool_result", "user.define_outcome", "user.tool_result",
		"system.message",
	})
	assertOpenAPIEventUnion(t, doc, "SessionEvent", []string{
		"user.message", "user.interrupt", "user.tool_confirmation",
		"user.custom_tool_result", "user.tool_result", "user.define_outcome",
		"system.message", "agent.message", "agent.thinking", "agent.custom_tool_use", "agent.tool_use",
		"agent.tool_result", "agent.mcp_tool_use", "agent.mcp_tool_result",
		"session.status_idle", "session.status_running", "session.status_terminated",
		"session.status_rescheduled", "session.usage", "session.error", "session.updated", "session.deleted",
		"session.thread_created", "session.thread_status_idle",
		"session.thread_status_running", "session.thread_status_rescheduled",
		"session.thread_status_terminated", "agent.thread_message_received",
		"agent.thread_message_sent", "agent.thread_context_compacted",
		"span.outcome_evaluation_start", "span.outcome_evaluation_ongoing",
		"span.outcome_evaluation_end", "span.model_request_start", "span.model_request_end",
	})
	validateOpenAPIRefs(t, doc, doc)
}

func TestOpenAPISessionThreadContract(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	paths := openAPIMap(t, doc["paths"], "paths")
	operations := map[string]string{
		"/v1/sessions/{session_id}/threads":                     "get",
		"/v1/sessions/{session_id}/threads/{thread_id}":         "get",
		"/v1/sessions/{session_id}/threads/{thread_id}/archive": "post",
		"/v1/sessions/{session_id}/threads/{thread_id}/events":  "get",
		"/v1/sessions/{session_id}/threads/{thread_id}/stream":  "get",
	}
	for path, method := range operations {
		item := openAPIMap(t, paths[path], "thread path "+path)
		operation := openAPIMap(t, item[method], method+" "+path)
		if operation["operationId"] == "" {
			t.Fatalf("%s %s has no operationId", method, path)
		}
	}
	threadPath := openAPIMap(t,
		paths["/v1/sessions/{session_id}/threads/{thread_id}"], "thread get path")
	threadGet := openAPIMap(t, threadPath["get"], "get thread")
	threadResponses := openAPIMap(t, threadGet["responses"], "get thread responses")
	assertOpenAPIRef(t, threadResponses["200"], "#/components/responses/SessionThreadResponse")

	listPath := openAPIMap(t, paths["/v1/sessions/{session_id}/threads"], "thread list path")
	list := openAPIMap(t, listPath["get"], "list threads")
	assertOpenAPIParameterNames(t, doc, list["parameters"], []string{"limit", "page"})

	eventsPath := openAPIMap(t,
		paths["/v1/sessions/{session_id}/threads/{thread_id}/events"], "thread events path")
	events := openAPIMap(t, eventsPath["get"], "list thread events")
	assertOpenAPIParameterNames(t, doc, events["parameters"], []string{"limit", "page"})

	streamPath := openAPIMap(t,
		paths["/v1/sessions/{session_id}/threads/{thread_id}/stream"], "thread stream path")
	stream := openAPIMap(t, streamPath["get"], "stream thread events")
	assertOpenAPIParameterNames(t, doc, stream["parameters"], []string{"event_deltas[]"})
	validateOpenAPIRefs(t, doc, doc)
}

func TestOpenAPIFullManagedAgentsOperationInventory(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	paths := openAPIMap(t, doc["paths"], "paths")
	count := 0
	seen := map[string]bool{}
	for path, rawPathItem := range paths {
		if !strings.HasPrefix(path, "/v1/") {
			continue
		}
		pathItem := openAPIMap(t, rawPathItem, "path "+path)
		for _, method := range []string{"delete", "get", "post"} {
			rawOperation, ok := pathItem[method]
			if !ok {
				continue
			}
			operation := openAPIMap(t, rawOperation, method+" "+path)
			id, _ := operation["operationId"].(string)
			if id == "" || seen[id] {
				t.Fatalf("%s %s has missing or duplicate operationId %q", method, path, id)
			}
			seen[id] = true
			count++
		}
	}
	if count != 95 {
		t.Fatalf("Mango operation count = %d, want 95", count)
	}
}

func assertOpenAPIRef(t *testing.T, value any, want string) {
	t.Helper()
	ref, _ := openAPIMap(t, value, "reference")["$ref"].(string)
	if ref != want {
		t.Fatalf("OpenAPI reference = %q, want %q", ref, want)
	}
}

func assertOpenAPIParameterNames(t *testing.T, doc map[string]any, value any, want []string) {
	t.Helper()
	parameters, ok := value.([]any)
	if !ok {
		t.Fatalf("parameters are %T, want array", value)
	}
	got := make([]string, 0, len(parameters))
	for _, raw := range parameters {
		parameter := resolveOpenAPIRef(t, doc, raw)
		name, _ := parameter["name"].(string)
		got = append(got, name)
	}
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("parameter names = %v, want %v", got, want)
	}
}

func assertOpenAPIEventUnion(t *testing.T, doc map[string]any, name string, want []string) {
	t.Helper()
	components := openAPIMap(t, doc["components"], "components")
	schemas := openAPIMap(t, components["schemas"], "schemas")
	union := openAPIMap(t, schemas[name], name)
	variants, ok := union["oneOf"].([]any)
	if !ok {
		t.Fatalf("%s oneOf is %T, want array", name, union["oneOf"])
	}
	got := make([]string, 0, len(variants))
	for _, raw := range variants {
		variant := resolveOpenAPIRef(t, doc, raw)
		got = append(got, openAPIEventTypeConst(t, variant, name))
	}
	sort.Strings(got)
	want = append([]string(nil), want...)
	sort.Strings(want)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s variants = %v, want %v", name, got, want)
	}
}

func openAPIEventTypeConst(t *testing.T, schema map[string]any, context string) string {
	t.Helper()
	if properties, ok := schema["properties"].(map[string]any); ok {
		if typeSchema, ok := properties["type"].(map[string]any); ok {
			if value, ok := typeSchema["const"].(string); ok {
				return value
			}
		}
	}
	if allOf, ok := schema["allOf"].([]any); ok {
		for _, raw := range allOf {
			part := openAPIMap(t, raw, context+" allOf member")
			if _, isRef := part["$ref"]; isRef {
				continue
			}
			if value := openAPIEventTypeConst(t, part, context); value != "" {
				return value
			}
		}
	}
	t.Fatalf("%s has no event type const", context)
	return ""
}

func parseOpenAPIDocument(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(openapiDoc), &doc); err != nil {
		t.Fatalf("parse OpenAPI document: %v", err)
	}
	if doc["openapi"] != "3.1.0" {
		t.Fatalf("OpenAPI version = %#v", doc["openapi"])
	}
	return doc
}

func openAPIMap(t *testing.T, value any, context string) map[string]any {
	t.Helper()
	result, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want object", context, value)
	}
	return result
}

func resolveOpenAPIRef(t *testing.T, doc map[string]any, value any) map[string]any {
	t.Helper()
	object := openAPIMap(t, value, "reference")
	ref, ok := object["$ref"].(string)
	if !ok {
		return object
	}
	resolved, ok := lookupOpenAPIRef(doc, ref)
	if !ok {
		t.Fatalf("unresolved OpenAPI reference %q", ref)
	}
	return openAPIMap(t, resolved, ref)
}

func validateOpenAPIRefs(t *testing.T, doc map[string]any, value any) {
	t.Helper()
	switch value := value.(type) {
	case map[string]any:
		if ref, ok := value["$ref"].(string); ok {
			if _, found := lookupOpenAPIRef(doc, ref); !found {
				t.Errorf("unresolved OpenAPI reference %q", ref)
			}
		}
		for _, child := range value {
			validateOpenAPIRefs(t, doc, child)
		}
	case []any:
		for _, child := range value {
			validateOpenAPIRefs(t, doc, child)
		}
	}
}

func lookupOpenAPIRef(doc map[string]any, ref string) (any, bool) {
	if !strings.HasPrefix(ref, "#/") {
		return nil, false
	}
	var current any = doc
	for _, segment := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[strings.ReplaceAll(strings.ReplaceAll(segment, "~1", "/"), "~0", "~")]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func TestOpenAPICoreOperationInventory(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	paths := openAPIMap(t, doc["paths"], "paths")
	count := 0
	for path, rawPathItem := range paths {
		if !strings.HasPrefix(path, "/v1/") {
			continue
		}
		if strings.HasPrefix(path, "/v1/files") {
			continue
		}
		if strings.HasPrefix(path, "/v1/skills") {
			continue
		}
		if strings.HasPrefix(path, "/v1/memory_stores") {
			continue
		}
		if strings.HasPrefix(path, "/v1/vaults") {
			continue
		}
		if strings.HasPrefix(path, "/v1/deployments") ||
			strings.HasPrefix(path, "/v1/deployment_runs") {
			continue
		}
		if strings.HasPrefix(path, "/v1/environments/") && strings.Contains(path, "/work") {
			continue
		}
		if strings.Contains(path, "/resources") {
			continue
		}
		if strings.Contains(path, "/threads") {
			continue
		}
		pathItem := openAPIMap(t, rawPathItem, "path "+path)
		for _, method := range []string{"delete", "get", "post"} {
			if operation, ok := pathItem[method]; ok {
				count++
				if id, _ := openAPIMap(t, operation, fmt.Sprintf("%s %s", method, path))["operationId"].(string); id == "" {
					t.Errorf("%s %s has no operationId", method, path)
				}
			}
		}
	}
	if count != 27 {
		t.Fatalf("core operation count = %d, want 27", count)
	}
}

func TestOpenAPIEnvironmentWorkContract(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	paths := openAPIMap(t, doc["paths"], "paths")
	operations := map[string][]string{
		"/v1/environments/{environment_id}/work":                     {"get"},
		"/v1/environments/{environment_id}/work/poll":                {"get"},
		"/v1/environments/{environment_id}/work/stats":               {"get"},
		"/v1/environments/{environment_id}/work/{work_id}":           {"get", "post"},
		"/v1/environments/{environment_id}/work/{work_id}/ack":       {"post"},
		"/v1/environments/{environment_id}/work/{work_id}/heartbeat": {"post"},
		"/v1/environments/{environment_id}/work/{work_id}/stop":      {"post"},
	}
	seen := map[string]bool{}
	count := 0
	for path, methods := range operations {
		pathItem := openAPIMap(t, paths[path], "path "+path)
		for _, method := range methods {
			operation := openAPIMap(t, pathItem[method], method+" "+path)
			id, _ := operation["operationId"].(string)
			if id == "" || seen[id] {
				t.Fatalf("%s %s has missing or duplicate operationId %q", method, path, id)
			}
			seen[id] = true
			count++
		}
	}
	if count != 8 {
		t.Fatalf("Environment Work operation count = %d, want 8", count)
	}
	stop := openAPIMap(t,
		openAPIMap(t, paths["/v1/environments/{environment_id}/work/{work_id}/stop"], "Stop path")["post"],
		"Stop operation",
	)
	responses := openAPIMap(t, stop["responses"], "Stop responses")
	if _, ok := responses["204"]; !ok {
		t.Fatal("Environment Work Stop does not document its 204 response")
	}
	validateOpenAPIRefs(t, doc, doc)
}

func TestOpenAPIDeploymentContract(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	paths := openAPIMap(t, doc["paths"], "paths")
	operations := map[string][]string{
		"/v1/deployments":                         {"get", "post"},
		"/v1/deployments/{deployment_id}":         {"get", "post"},
		"/v1/deployments/{deployment_id}/archive": {"post"},
		"/v1/deployments/{deployment_id}/pause":   {"post"},
		"/v1/deployments/{deployment_id}/unpause": {"post"},
		"/v1/deployments/{deployment_id}/run":     {"post"},
		"/v1/deployment_runs":                     {"get"},
		"/v1/deployment_runs/{deployment_run_id}": {"get"},
	}
	count := 0
	for path, methods := range operations {
		pathItem := openAPIMap(t, paths[path], "path "+path)
		for _, method := range methods {
			operation := openAPIMap(t, pathItem[method], method+" "+path)
			if operation["operationId"] == nil {
				t.Fatalf("%s %s has no operationId", method, path)
			}
			count++
		}
	}
	if count != 10 {
		t.Fatalf("Deployment operation count = %d, want 10", count)
	}
	validateOpenAPIRefs(t, doc, doc)
}

func TestOpenAPIVaultContract(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	paths := openAPIMap(t, doc["paths"], "paths")
	operations := map[string][]string{
		"/v1/vaults":                                                           {"get", "post"},
		"/v1/vaults/{vault_id}":                                                {"delete", "get", "post"},
		"/v1/vaults/{vault_id}/archive":                                        {"post"},
		"/v1/vaults/{vault_id}/credentials":                                    {"get", "post"},
		"/v1/vaults/{vault_id}/credentials/{credential_id}":                    {"delete", "get", "post"},
		"/v1/vaults/{vault_id}/credentials/{credential_id}/archive":            {"post"},
		"/v1/vaults/{vault_id}/credentials/{credential_id}/mcp_oauth_validate": {"post"},
	}
	requestBodies := map[string]string{
		"post /v1/vaults":                                        "#/components/schemas/VaultCreateRequest",
		"post /v1/vaults/{vault_id}":                             "#/components/schemas/VaultUpdateRequest",
		"post /v1/vaults/{vault_id}/credentials":                 "#/components/schemas/VaultCredentialCreateRequest",
		"post /v1/vaults/{vault_id}/credentials/{credential_id}": "#/components/schemas/VaultCredentialUpdateRequest",
	}
	seenIDs := map[string]bool{}
	count := 0
	for path, methods := range operations {
		pathItem := openAPIMap(t, paths[path], "path "+path)
		for _, method := range methods {
			operation := openAPIMap(t, pathItem[method], method+" "+path)
			id, _ := operation["operationId"].(string)
			if id == "" || seenIDs[id] {
				t.Fatalf("%s %s has missing or duplicate operationId %q", method, path, id)
			}
			seenIDs[id] = true
			key := method + " " + path
			wantBody, shouldHaveBody := requestBodies[key]
			body, hasBody := operation["requestBody"]
			if hasBody != shouldHaveBody {
				t.Fatalf("%s requestBody presence = %t, want %t", key, hasBody, shouldHaveBody)
			}
			if hasBody {
				request := openAPIMap(t, body, key+" requestBody")
				content := openAPIMap(t, request["content"], key+" content")
				media := openAPIMap(t, content["application/json"], key+" JSON")
				assertOpenAPIRef(t, media["schema"], wantBody)
			}
			count++
		}
	}
	if count != 13 {
		t.Fatalf("Vault operation count = %d, want 13", count)
	}
	components := openAPIMap(t, doc["components"], "components")
	schemas := openAPIMap(t, components["schemas"], "schemas")
	for _, name := range []string{"VaultList", "VaultCredentialList"} {
		schema := openAPIMap(t, schemas[name], name)
		properties := openAPIMap(t, schema["properties"], name+" properties")
		if _, ok := properties["has_more"]; ok {
			t.Fatalf("%s includes non-contract has_more", name)
		}
		required := schema["required"].([]any)
		if len(required) != 2 || required[0] != "data" || required[1] != "next_page" {
			t.Fatalf("%s required = %#v, want [data next_page]", name, required)
		}
	}
	validateOpenAPIRefs(t, doc, doc)
}

func TestOpenAPISessionResourcesContract(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	paths := openAPIMap(t, doc["paths"], "paths")
	operations := map[string][]string{
		"/v1/sessions/{session_id}/resources":               {"get", "post"},
		"/v1/sessions/{session_id}/resources/{resource_id}": {"delete", "get"},
	}
	count := 0
	for path, methods := range operations {
		pathItem := openAPIMap(t, paths[path], "path "+path)
		for _, method := range methods {
			operation := openAPIMap(t, pathItem[method], method+" "+path)
			if id, _ := operation["operationId"].(string); id == "" {
				t.Fatalf("%s %s has no operationId", method, path)
			}
			count++
		}
	}
	if count != 4 {
		t.Fatalf("Session Resources operation count = %d, want 4", count)
	}
	schemas := openAPIMap(
		t,
		openAPIMap(t, doc["components"], "components")["schemas"],
		"schemas",
	)
	session := openAPIMap(t, schemas["Session"], "Session schema")
	properties := openAPIMap(t, session["properties"], "Session properties")
	resources := openAPIMap(t, properties["resources"], "Session resources")
	if fmt.Sprint(resources["maxItems"]) != "500" {
		t.Fatalf("Session resources maxItems = %v, want 500", resources["maxItems"])
	}
	assertOpenAPIRef(t, openAPIMap(t, resources["items"], "Session resource items"),
		"#/components/schemas/SessionResource")
	create := openAPIMap(t, schemas["SessionCreateRequest"], "SessionCreateRequest schema")
	createResources := openAPIMap(t,
		openAPIMap(t, create["properties"], "SessionCreateRequest properties")["resources"],
		"SessionCreateRequest resources",
	)
	assertOpenAPIRef(t, openAPIMap(t, createResources["items"], "Session resource input items"),
		"#/components/schemas/SessionResourceInput")
	for name, want := range map[string][]string{
		"SessionResourceInput": {
			"#/components/schemas/FileSessionResourceInput",
			"#/components/schemas/MemoryStoreSessionResourceInput",
			"#/components/schemas/GitRepositorySessionResourceInput",
		},
		"SessionResource": {
			"#/components/schemas/FileSessionResource",
			"#/components/schemas/MemoryStoreSessionResource",
			"#/components/schemas/GitRepositorySessionResource",
		},
	} {
		union := openAPIMap(t, schemas[name], name)
		variants, ok := union["oneOf"].([]any)
		if !ok || len(variants) != len(want) {
			t.Fatalf("%s variants = %#v, want %v", name, union["oneOf"], want)
		}
		for index, ref := range want {
			assertOpenAPIRef(t, variants[index], ref)
		}
	}
	validateOpenAPIRefs(t, doc, doc)
}

func TestOpenAPIMemoryContract(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	paths := openAPIMap(t, doc["paths"], "paths")
	operations := map[string][]string{
		"/v1/memory_stores":                                                {"get", "post"},
		"/v1/memory_stores/{store_id}":                                     {"delete", "get", "post"},
		"/v1/memory_stores/{store_id}/archive":                             {"post"},
		"/v1/memory_stores/{store_id}/memories":                            {"get", "post"},
		"/v1/memory_stores/{store_id}/memories/{memory_id}":                {"delete", "get", "post"},
		"/v1/memory_stores/{store_id}/memory_versions":                     {"get"},
		"/v1/memory_stores/{store_id}/memory_versions/{version_id}":        {"get"},
		"/v1/memory_stores/{store_id}/memory_versions/{version_id}/redact": {"post"},
	}
	requestBodies := map[string]string{
		"post /v1/memory_stores":                                 "#/components/schemas/MemoryStoreCreateRequest",
		"post /v1/memory_stores/{store_id}":                      "#/components/schemas/MemoryStoreUpdateRequest",
		"post /v1/memory_stores/{store_id}/memories":             "#/components/schemas/MemoryCreateRequest",
		"post /v1/memory_stores/{store_id}/memories/{memory_id}": "#/components/schemas/MemoryUpdateRequest",
	}
	count := 0
	seenIDs := map[string]bool{}
	for path, methods := range operations {
		pathItem := openAPIMap(t, paths[path], "path "+path)
		for _, method := range methods {
			operation := openAPIMap(t, pathItem[method], method+" "+path)
			id, _ := operation["operationId"].(string)
			if id == "" || seenIDs[id] {
				t.Fatalf("%s %s has missing or duplicate operationId %q", method, path, id)
			}
			seenIDs[id] = true
			responses := openAPIMap(t, operation["responses"], method+" "+path+" responses")
			assertOpenAPIRef(t, responses["200"], memorySuccessResponseRef(path, method))

			key := method + " " + path
			wantBody, shouldHaveBody := requestBodies[key]
			body, hasBody := operation["requestBody"]
			if hasBody != shouldHaveBody {
				t.Fatalf("%s requestBody presence = %t, want %t", key, hasBody, shouldHaveBody)
			}
			if hasBody {
				request := openAPIMap(t, body, key+" requestBody")
				content := openAPIMap(t, request["content"], key+" content")
				jsonMedia := openAPIMap(t, content["application/json"], key+" application/json")
				assertOpenAPIRef(t, jsonMedia["schema"], wantBody)
			}
			count++
		}
	}
	if count != 14 {
		t.Fatalf("Memory operation count = %d, want 14", count)
	}
	list := openAPIMap(t,
		openAPIMap(t, paths["/v1/memory_stores/{store_id}/memories"], "Memories path")["get"],
		"list Memories",
	)
	assertOpenAPIParameterNames(t, doc, list["parameters"],
		[]string{"depth", "limit", "page", "path_prefix", "view"})
	versions := openAPIMap(t,
		openAPIMap(t, paths["/v1/memory_stores/{store_id}/memory_versions"], "Memory Versions path")["get"],
		"list Memory Versions",
	)
	assertOpenAPIParameterNames(t, doc, versions["parameters"], []string{
		"api_key_id", "created_at[gte]", "created_at[lte]", "limit", "memory_id",
		"operation", "page", "session_id", "view",
	})
	validateOpenAPIRefs(t, doc, doc)
}

func memorySuccessResponseRef(path, method string) string {
	switch {
	case path == "/v1/memory_stores" && method == "get":
		return "#/components/responses/MemoryStoreListResponse"
	case path == "/v1/memory_stores/{store_id}" && method == "delete":
		return "#/components/responses/MemoryStoreDeletedResponse"
	case strings.HasSuffix(path, "/memories") && method == "get":
		return "#/components/responses/MemoryListResponse"
	case strings.HasSuffix(path, "/memories/{memory_id}") && method == "delete":
		return "#/components/responses/MemoryDeletedResponse"
	case strings.HasSuffix(path, "/memory_versions"):
		return "#/components/responses/MemoryVersionListResponse"
	case strings.Contains(path, "/memory_versions/"):
		return "#/components/responses/MemoryVersionResponse"
	case strings.Contains(path, "/memories"):
		return "#/components/responses/MemoryResponse"
	default:
		return "#/components/responses/MemoryStoreResponse"
	}
}

func TestOpenAPIFilesContract(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	paths := openAPIMap(t, doc["paths"], "paths")
	operations := map[string][]string{
		"/v1/files":                   {"get", "post"},
		"/v1/files/{file_id}":         {"delete", "get"},
		"/v1/files/{file_id}/content": {"get"},
	}
	count := 0
	for path, methods := range operations {
		pathItem := openAPIMap(t, paths[path], "path "+path)
		for _, method := range methods {
			operation := openAPIMap(t, pathItem[method], method+" "+path)
			if id, _ := operation["operationId"].(string); id == "" {
				t.Fatalf("%s %s has no operationId", method, path)
			}
			count++
		}
	}
	if count != 5 {
		t.Fatalf("Files operation count = %d, want 5", count)
	}
	upload := openAPIMap(t, openAPIMap(t, paths["/v1/files"], "Files path")["post"], "upload")
	request := openAPIMap(t, upload["requestBody"], "upload request")
	content := openAPIMap(t, request["content"], "upload content")
	assertOpenAPIRef(t, openAPIMap(t, content["multipart/form-data"], "multipart")["schema"],
		"#/components/schemas/FileUploadRequest")
	list := openAPIMap(t, openAPIMap(t, paths["/v1/files"], "Files path")["get"], "list")
	assertOpenAPIParameterNames(t, doc, list["parameters"],
		[]string{"after_id", "before_id", "limit", "scope_id"})
	validateOpenAPIRefs(t, doc, doc)
}

func TestOpenAPISkillsContract(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	paths := openAPIMap(t, doc["paths"], "paths")
	operations := map[string][]string{
		"/v1/skills":                                       {"get", "post"},
		"/v1/skills/{skill_id}":                            {"delete", "get"},
		"/v1/skills/{skill_id}/versions":                   {"get", "post"},
		"/v1/skills/{skill_id}/versions/{version}":         {"delete", "get"},
		"/v1/skills/{skill_id}/versions/{version}/content": {"get"},
	}
	count := 0
	for path, methods := range operations {
		pathItem := openAPIMap(t, paths[path], "path "+path)
		for _, method := range methods {
			operation := openAPIMap(t, pathItem[method], method+" "+path)
			if id, _ := operation["operationId"].(string); id == "" {
				t.Fatalf("%s %s has no operationId", method, path)
			}
			count++
		}
	}
	if count != 9 {
		t.Fatalf("Skills operation count = %d, want 9", count)
	}
	for path, schema := range map[string]string{
		"/v1/skills":                     "#/components/schemas/SkillUploadRequest",
		"/v1/skills/{skill_id}/versions": "#/components/schemas/SkillVersionUploadRequest",
	} {
		post := openAPIMap(t, openAPIMap(t, paths[path], path)["post"], "post "+path)
		request := openAPIMap(t, post["requestBody"], "request "+path)
		content := openAPIMap(t, request["content"], "content "+path)
		assertOpenAPIRef(t, openAPIMap(t, content["multipart/form-data"], "multipart")["schema"], schema)
	}
	list := openAPIMap(t, openAPIMap(t, paths["/v1/skills"], "Skills path")["get"], "list Skills")
	assertOpenAPIParameterNames(t, doc, list["parameters"], []string{"limit", "page", "source"})
	versions := openAPIMap(t,
		openAPIMap(t, paths["/v1/skills/{skill_id}/versions"], "Versions path")["get"],
		"list Versions",
	)
	assertOpenAPIParameterNames(t, doc, versions["parameters"], []string{"limit", "page"})
	validateOpenAPIRefs(t, doc, doc)
}

func TestOpenAPISkillReferenceContract(t *testing.T) {
	doc := parseOpenAPIDocument(t)
	components := openAPIMap(t, doc["components"], "components")
	schemas := openAPIMap(t, components["schemas"], "schemas")
	for _, name := range []string{"CustomSkillReferenceInput", "AnthropicSkillReferenceInput"} {
		schema := openAPIMap(t, schemas[name], name)
		if additional, ok := schema["additionalProperties"].(bool); !ok || additional {
			t.Fatalf("%s additionalProperties = %v, want false", name, schema["additionalProperties"])
		}
		required, _ := schema["required"].([]any)
		if fmt.Sprint(required) != "[type skill_id]" {
			t.Fatalf("%s required = %v", name, required)
		}
		version := openAPIMap(t, openAPIMap(t, schema["properties"], name+" properties")["version"], name+" version")
		if version["minLength"] != 1 || version["pattern"] != `^\S(?:[\s\S]*\S)?$` {
			t.Fatalf("%s Version constraints = %#v", name, version)
		}
	}
	resolved := openAPIMap(t, schemas["ResolvedSkillReference"], "resolved Skill reference")
	if required, _ := resolved["required"].([]any); fmt.Sprint(required) != "[type skill_id version]" {
		t.Fatalf("resolved Skill required = %v", required)
	}
	resolvedType := openAPIMap(t, openAPIMap(t, resolved["properties"], "resolved Skill properties")["type"], "resolved Skill type")
	if resolvedType["const"] != "custom" {
		t.Fatalf("resolved Skill type = %v, want custom", resolvedType["const"])
	}
	legacy := openAPIMap(t, schemas["LegacySkillReference"], "legacy Skill reference")
	assertOpenAPIRef(t, legacy["not"], "#/components/schemas/ResolvedSkillReference")
	response := openAPIMap(t, schemas["SkillReferenceResponse"], "Skill reference response")
	variants, _ := response["oneOf"].([]any)
	if len(variants) != 2 {
		t.Fatalf("Skill response variants = %v, want resolved and legacy", variants)
	}
	assertOpenAPIRef(t, variants[0], "#/components/schemas/ResolvedSkillReference")
	assertOpenAPIRef(t, variants[1], "#/components/schemas/LegacySkillReference")

	create := openAPIMap(t, schemas["AgentCreateRequest"], "Agent create")
	createSkills := openAPIMap(t, openAPIMap(t, create["properties"], "Agent create properties")["skills"], "Agent create skills")
	if max, _ := createSkills["maxItems"].(int); max != app.MaxSessionSkills {
		t.Fatalf("Agent create max Skills = %v, want %d", createSkills["maxItems"], app.MaxSessionSkills)
	}
	assertOpenAPIRef(t, createSkills["items"], "#/components/schemas/SkillReferenceInput")

	agent := openAPIMap(t, schemas["Agent"], "Agent")
	agentSkills := openAPIMap(t, openAPIMap(t, agent["properties"], "Agent properties")["skills"], "Agent skills")
	assertOpenAPIRef(t, agentSkills["items"], "#/components/schemas/SkillReferenceResponse")
	snapshot := openAPIMap(t, schemas["AgentSnapshot"], "Agent snapshot")
	snapshotSkills := openAPIMap(t, openAPIMap(t, snapshot["properties"], "Agent snapshot properties")["skills"], "Agent snapshot skills")
	assertOpenAPIRef(t, snapshotSkills["items"], "#/components/schemas/SkillReferenceResponse")
	validateOpenAPIRefs(t, doc, doc)
}
