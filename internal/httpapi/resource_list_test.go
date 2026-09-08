package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

func decodeResourceList(t *testing.T, handler http.Handler, path string) map[string]any {
	t.Helper()
	recorder := do(handler, http.MethodGet, path, "")
	if recorder.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, recorder.Code, recorder.Body)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return body
}

func resourceListIDs(t *testing.T, body map[string]any) []string {
	t.Helper()
	data, ok := body["data"].([]any)
	if !ok {
		t.Fatalf("data is not an array: %#v", body)
	}
	ids := make([]string, 0, len(data))
	for _, item := range data {
		object, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("resource is not an object: %#v", item)
		}
		id, _ := object["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func resourceNextPage(t *testing.T, body map[string]any) string {
	t.Helper()
	value, present := body["next_page"]
	if !present {
		t.Fatalf("next_page is absent: %#v", body)
	}
	if value == nil {
		return ""
	}
	token, ok := value.(string)
	if !ok {
		t.Fatalf("next_page is not a string: %#v", value)
	}
	return token
}

func createListAgents(t *testing.T, handler http.Handler, count int) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for index := range count {
		recorder := do(handler, http.MethodPost, "/v1/agents", fmt.Sprintf(
			`{"name":"agent %d","model":"claude-opus-4-8"}`, index,
		))
		if recorder.Code != http.StatusOK {
			t.Fatalf("create agent %d = %d: %s", index, recorder.Code, recorder.Body)
		}
		var created map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode agent %d: %v", index, err)
		}
		ids = append(ids, created["id"].(string))
	}
	return ids
}

func createListEnvironments(t *testing.T, handler http.Handler, count int) []string {
	t.Helper()
	ids := make([]string, 0, count)
	for index := range count {
		recorder := do(handler, http.MethodPost, "/v1/environments", fmt.Sprintf(
			`{"name":"environment %d","config":{"type":"self_hosted"}}`, index,
		))
		if recorder.Code != http.StatusOK {
			t.Fatalf("create environment %d = %d: %s", index, recorder.Code, recorder.Body)
		}
		var created map[string]any
		if err := json.Unmarshal(recorder.Body.Bytes(), &created); err != nil {
			t.Fatalf("decode environment %d: %v", index, err)
		}
		ids = append(ids, created["id"].(string))
	}
	return ids
}

func TestListAgents_DefaultLimitEnvelopeAndPagination(t *testing.T) {
	server := newTestServer(t)
	created := createListAgents(t, server, 21)

	first := decodeResourceList(t, server, "/v1/agents")
	if len(first) != 2 {
		t.Fatalf("envelope = %#v, want data and next_page only", first)
	}
	if got := len(resourceListIDs(t, first)); got != 20 {
		t.Fatalf("default page size = %d, want 20", got)
	}
	token := resourceNextPage(t, first)
	if token == "" {
		t.Fatal("first page has no next_page")
	}
	second := decodeResourceList(t, server, "/v1/agents?page="+url.QueryEscape(token))
	if got := len(resourceListIDs(t, second)); got != 1 {
		t.Fatalf("second page size = %d, want 1", got)
	}
	if resourceNextPage(t, second) != "" {
		t.Fatal("terminal page has a next_page")
	}

	seen := make(map[string]int, len(created))
	for _, body := range []map[string]any{first, second} {
		for _, id := range resourceListIDs(t, body) {
			seen[id]++
		}
	}
	for _, id := range created {
		if seen[id] != 1 {
			t.Errorf("agent %s appeared %d times", id, seen[id])
		}
	}
}

func TestListAgents_ParametersAndCursorBinding(t *testing.T) {
	server := newTestServer(t)
	ids := createListAgents(t, server, 4)
	if recorder := do(server, http.MethodPost, "/v1/agents/"+ids[0]+"/archive", ""); recorder.Code != http.StatusOK {
		t.Fatalf("archive = %d: %s", recorder.Code, recorder.Body)
	}

	active := decodeResourceList(t, server, "/v1/agents?limit=1")
	if got := len(resourceListIDs(t, active)); got != 1 {
		t.Fatalf("limit=1 returned %d agents", got)
	}
	token := resourceNextPage(t, active)
	if token == "" {
		t.Fatal("limit=1 has no cursor")
	}
	if recorder := do(server, http.MethodGet,
		"/v1/agents?limit=1&page="+url.QueryEscape(token), ""); recorder.Code != http.StatusOK {
		t.Fatalf("same-filter cursor = %d: %s", recorder.Code, recorder.Body)
	}
	for _, changed := range []string{
		"include_archived=true",
		"created_at[gte]=1970-01-01T00:00:00Z",
		"created_at[lte]=2030-01-01T00:00:00Z",
	} {
		recorder := do(server, http.MethodGet,
			"/v1/agents?limit=1&"+changed+"&page="+url.QueryEscape(token), "")
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("changed filter %s = %d, want 400", changed, recorder.Code)
		}
	}

	if got := resourceListIDs(t, decodeResourceList(t, server,
		"/v1/agents?include_archived=true")); len(got) != 4 {
		t.Fatalf("include_archived returned %v, want 4 agents", got)
	}
	if got := resourceListIDs(t, decodeResourceList(t, server,
		"/v1/agents?created_at[gte]=2020-01-01T00:00:00Z")); len(got) != 0 {
		t.Fatalf("future created_at[gte] returned %v", got)
	}
}

func TestListAgents_ValidatesDocumentedParameters(t *testing.T) {
	server := newTestServer(t)
	for _, query := range []string{
		"limit=0", "limit=-1", "limit=101", "limit=many",
		"include_archived=yes", "created_at[gte]=yesterday",
		"page=not-a-cursor",
	} {
		recorder := do(server, http.MethodGet, "/v1/agents?"+query, "")
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400: %s", query, recorder.Code, recorder.Body)
		}
	}
	if recorder := do(server, http.MethodGet, "/v1/agents?limit=100", ""); recorder.Code != http.StatusOK {
		t.Fatalf("limit=100 = %d: %s", recorder.Code, recorder.Body)
	}
}

func TestListEnvironments_PaginationAndArchiveFilter(t *testing.T) {
	server := newTestServer(t)
	created := createListEnvironments(t, server, 5)
	if recorder := do(server, http.MethodPost,
		"/v1/environments/"+created[0]+"/archive", ""); recorder.Code != http.StatusOK {
		t.Fatalf("archive = %d: %s", recorder.Code, recorder.Body)
	}

	seen := map[string]int{}
	path := "/v1/environments?limit=2"
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber > 5 {
			t.Fatal("pagination did not terminate")
		}
		body := decodeResourceList(t, server, path)
		for _, id := range resourceListIDs(t, body) {
			seen[id]++
		}
		token := resourceNextPage(t, body)
		if token == "" {
			break
		}
		path = "/v1/environments?limit=2&page=" + url.QueryEscape(token)
	}
	if len(seen) != 4 {
		t.Fatalf("default list saw %d active environments, want 4", len(seen))
	}
	if seen[created[0]] != 0 {
		t.Fatal("archived environment appeared in the default list")
	}
	if got := resourceListIDs(t, decodeResourceList(t, server,
		"/v1/environments?include_archived=true")); len(got) != 5 {
		t.Fatalf("include_archived returned %v, want 5 environments", got)
	}
}

func TestListEnvironments_ValidatesParametersAndCursorKind(t *testing.T) {
	server := newTestServer(t)
	createListAgents(t, server, 3)
	createListEnvironments(t, server, 3)

	agentCursor := resourceNextPage(t,
		decodeResourceList(t, server, "/v1/agents?limit=1"))
	for _, query := range []string{
		"limit=0", "limit=-1", "limit=1001", "limit=many",
		"include_archived=maybe", "page=not-a-cursor",
		"page=" + url.QueryEscape(agentCursor),
	} {
		recorder := do(server, http.MethodGet, "/v1/environments?"+query, "")
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400: %s", query, recorder.Code, recorder.Body)
		}
	}
	if recorder := do(server, http.MethodGet, "/v1/environments?limit=1000", ""); recorder.Code != http.StatusOK {
		t.Fatalf("limit=1000 = %d: %s", recorder.Code, recorder.Body)
	}
}

func TestListEnvironments_CursorRejectsFilterChange(t *testing.T) {
	server := newTestServer(t)
	createListEnvironments(t, server, 4)
	token := resourceNextPage(t,
		decodeResourceList(t, server, "/v1/environments?limit=1"))
	if token == "" {
		t.Fatal("first page has no cursor")
	}
	if recorder := do(server, http.MethodGet,
		"/v1/environments?limit=1&page="+url.QueryEscape(token), ""); recorder.Code != http.StatusOK {
		t.Fatalf("same-filter cursor = %d: %s", recorder.Code, recorder.Body)
	}
	if recorder := do(server, http.MethodGet,
		"/v1/environments?limit=1&include_archived=true&page="+url.QueryEscape(token), ""); recorder.Code != http.StatusBadRequest {
		t.Fatalf("changed-filter cursor = %d, want 400", recorder.Code)
	}
}
