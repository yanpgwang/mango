package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

type sessionListEnvelope struct {
	Data     []map[string]any `json:"data"`
	NextPage *string          `json:"next_page"`
	PrevPage *string          `json:"prev_page"`
}

func decodeSessionList(t *testing.T, h http.Handler, path string) sessionListEnvelope {
	t.Helper()
	rec := do(h, "GET", path, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s -> %d: %s", path, rec.Code, rec.Body)
	}
	var envelope sessionListEnvelope
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode GET %s: %v", path, err)
	}
	return envelope
}

func sessionIDs(data []map[string]any) []string {
	ids := make([]string, len(data))
	for index, session := range data {
		ids[index], _ = session["id"].(string)
	}
	return ids
}

func TestListSessions_BidirectionalStablePagination(t *testing.T) {
	h := NewTestHandler(t)
	agentID := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	environmentID := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"self_hosted"}}`)
	created := make([]string, 0, 5)
	for range 5 {
		created = append(created, createID(t, h, "POST", "/v1/sessions",
			`{"agent":"`+agentID+`","environment_id":"`+environmentID+`"}`))
	}

	first := decodeSessionList(t, h, "/v1/sessions?limit=2")
	if got, want := sessionIDs(first.Data), []string{created[4], created[3]}; !sameStrings(got, want) {
		t.Fatalf("first page ids = %v, want %v", got, want)
	}
	if first.NextPage == nil || first.PrevPage != nil {
		t.Fatalf("first page cursors: next=%v prev=%v", first.NextPage, first.PrevPage)
	}

	second := decodeSessionList(t, h, "/v1/sessions?limit=2&page="+url.QueryEscape(*first.NextPage))
	if got, want := sessionIDs(second.Data), []string{created[2], created[1]}; !sameStrings(got, want) {
		t.Fatalf("second page ids = %v, want %v", got, want)
	}
	if second.NextPage == nil || second.PrevPage == nil {
		t.Fatalf("second page cursors: next=%v prev=%v", second.NextPage, second.PrevPage)
	}

	back := decodeSessionList(t, h, "/v1/sessions?limit=2&page="+url.QueryEscape(*second.PrevPage))
	if got, want := sessionIDs(back.Data), []string{created[4], created[3]}; !sameStrings(got, want) {
		t.Fatalf("previous page ids = %v, want %v", got, want)
	}
	if back.NextPage == nil || back.PrevPage != nil {
		t.Fatalf("previous page cursors: next=%v prev=%v", back.NextPage, back.PrevPage)
	}

	last := decodeSessionList(t, h, "/v1/sessions?limit=2&page="+url.QueryEscape(*second.NextPage))
	if got, want := sessionIDs(last.Data), []string{created[0]}; !sameStrings(got, want) {
		t.Fatalf("last page ids = %v, want %v", got, want)
	}
	if last.NextPage != nil || last.PrevPage == nil {
		t.Fatalf("last page cursors: next=%v prev=%v", last.NextPage, last.PrevPage)
	}

	// A session cursor is scoped to the sort order and normalized filter set.
	for _, path := range []string{
		"/v1/sessions?limit=2&order=asc&page=" + url.QueryEscape(*first.NextPage),
		"/v1/sessions?limit=2&include_archived=true&page=" + url.QueryEscape(*first.NextPage),
		"/v1/sessions?page=" + url.QueryEscape(encodeEventCursor(eventCursor{
			Order: "desc", SessionID: "sesn_other", Filter: "filter",
			Unprocessed: true, Sequence: 1,
		})),
	} {
		rec := do(h, "GET", path, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("GET %s -> %d, want 400: %s", path, rec.Code, rec.Body)
		}
	}
}

func TestListSessions_FiltersAndArchivedDefault(t *testing.T) {
	h := NewTestHandler(t)
	agentID := createID(t, h, "POST", "/v1/agents", `{"name":"a","model":"claude-opus-4-8"}`)
	otherAgentID := createID(t, h, "POST", "/v1/agents", `{"name":"b","model":"claude-opus-4-8"}`)
	environmentID := createID(t, h, "POST", "/v1/environments", `{"name":"e","config":{"type":"self_hosted"}}`)

	versionOne := createID(t, h, "POST", "/v1/sessions",
		`{"agent":{"type":"agent","id":"`+agentID+`","version":1},"environment_id":"`+environmentID+`"}`)
	other := createID(t, h, "POST", "/v1/sessions",
		`{"agent":"`+otherAgentID+`","environment_id":"`+environmentID+`"}`)
	if rec := do(h, "POST", "/v1/sessions/"+versionOne+"/archive", ""); rec.Code != http.StatusOK {
		t.Fatalf("archive -> %d: %s", rec.Code, rec.Body)
	}

	if got := sessionIDs(decodeSessionList(t, h, "/v1/sessions").Data); !sameStrings(got, []string{other}) {
		t.Fatalf("default archived filter returned %v, want [%s]", got, other)
	}
	if got := len(decodeSessionList(t, h, "/v1/sessions?include_archived=true").Data); got != 2 {
		t.Fatalf("include_archived returned %d sessions, want 2", got)
	}

	agentQuery := url.Values{
		"agent_id":         {agentID},
		"agent_version":    {"1"},
		"include_archived": {"true"},
	}
	if got := sessionIDs(decodeSessionList(t, h, "/v1/sessions?"+agentQuery.Encode()).Data); !sameStrings(got, []string{versionOne}) {
		t.Fatalf("agent/version filter returned %v, want [%s]", got, versionOne)
	}

	statusQuery := url.Values{"statuses[]": {"idle"}, "include_archived": {"true"}}
	if got := len(decodeSessionList(t, h, "/v1/sessions?"+statusQuery.Encode()).Data); got != 2 {
		t.Fatalf("statuses[] filter returned %d sessions, want 2", got)
	}

	atCreation := "1970-01-01T00:16:40Z"
	for _, path := range []string{
		"/v1/sessions?created_at%5Bgt%5D=" + url.QueryEscape(atCreation),
		"/v1/sessions?deployment_id=deploy_x&include_archived=true",
		"/v1/sessions?memory_store_id=memory_x&include_archived=true",
	} {
		if got := len(decodeSessionList(t, h, path).Data); got != 0 {
			t.Errorf("GET %s returned %d sessions, want 0", path, got)
		}
	}
	inclusive := decodeSessionList(t, h,
		"/v1/sessions?created_at%5Bgte%5D="+url.QueryEscape(atCreation)+"&include_archived=true")
	if len(inclusive.Data) != 2 {
		t.Fatalf("created_at[gte] returned %d sessions, want 2", len(inclusive.Data))
	}
}

func TestListSessions_RejectsInvalidQueryValues(t *testing.T) {
	h := NewTestHandler(t)
	for _, query := range []string{
		"?order=sideways",
		"?limit=0",
		"?limit=many",
		"?include_archived=1",
		"?agent_version=1",
		"?agent_id=agent_x&agent_version=0",
		"?created_at%5Blt%5D=tomorrow",
		"?statuses%5B%5D=paused",
		"?page=not-a-cursor",
		"?limit=1001",
	} {
		rec := do(h, "GET", "/v1/sessions"+query, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("query %s -> %d, want 400: %s", query, rec.Code, rec.Body)
		}
	}
}

// TestListSessions_LimitBoundary proves the shared max page limit: limit=1000 is
// accepted (200) and limit=1001 is rejected (400).
func TestListSessions_LimitBoundary(t *testing.T) {
	h := NewTestHandler(t)
	if rec := do(h, "GET", "/v1/sessions?limit=1000", ""); rec.Code != http.StatusOK {
		t.Errorf("limit=1000 -> %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := do(h, "GET", "/v1/sessions?limit=1001", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("limit=1001 -> %d, want 400: %s", rec.Code, rec.Body)
	}
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
