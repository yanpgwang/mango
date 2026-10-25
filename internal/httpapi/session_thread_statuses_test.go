package httpapi

import (
	"context"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

const threadFilterSessionID = "sesn_thread_filter"

// The fixture is independent of the generated SDK contract. It represents
// repository results; PostgreSQL tests separately prove filtering before LIMIT.
type threadStatusFixture struct {
	SessionThreadService
	threads   []domain.SessionThread
	lastQuery app.SessionThreadListQuery
}

func newThreadStatusFixture() *threadStatusFixture {
	now := time.Date(2026, 10, 18, 0, 0, 0, 0, time.UTC)
	primary := "sthr_filter_primary"
	fixture := &threadStatusFixture{}
	for index, item := range []struct {
		id     string
		status domain.Status
	}{
		{primary, domain.StatusIdle},
		{"sthr_filter_archived", domain.StatusTerminated},
		{"sthr_filter_running", domain.StatusRunning},
		{"sthr_filter_queued", domain.StatusRescheduling},
		{"sthr_filter_waiting", domain.StatusIdle},
	} {
		thread := domain.SessionThread{
			ID: item.id, SessionID: threadFilterSessionID, Status: item.status,
			CreatedAt: now.Add(time.Duration(index) * time.Second), UpdatedAt: now,
			Agent: domain.Agent{ID: "agent_filter", Version: 1, Name: "filter", Model: domain.Model{ID: "test"}},
		}
		if index > 0 {
			thread.ParentThreadID = &primary
		}
		if item.status == domain.StatusTerminated {
			thread.ArchivedAt = &now
		}
		fixture.threads = append(fixture.threads, thread)
	}
	return fixture
}

func (f *threadStatusFixture) List(_ context.Context, sessionID string, query app.SessionThreadListQuery) ([]domain.SessionThread, error) {
	if sessionID != threadFilterSessionID {
		return nil, domain.NotFound("session not found")
	}
	f.lastQuery = query
	result := []domain.SessionThread{}
	for _, thread := range f.threads {
		if len(query.Statuses) > 0 && !slices.Contains(query.Statuses, thread.Status) {
			continue
		}
		if b := query.Boundary; b != nil && (thread.CreatedAt.Before(b.CreatedAt) || (thread.CreatedAt.Equal(b.CreatedAt) && thread.ID <= b.ID)) {
			continue
		}
		result = append(result, thread)
		if len(result) == query.Limit {
			break
		}
	}
	return result, nil
}

func threadStatusHandler(fixture *threadStatusFixture) http.Handler {
	return NewServer(Deps{
		Sessions: &testSessionService{sessions: map[string]domain.Session{threadFilterSessionID: {ID: threadFilterSessionID}}},
		Threads:  fixture,
	}, Config{}).Handler()
}

func TestListSessionThreadsRejectsInvalidStatuses(t *testing.T) {
	t.Parallel()
	h := threadStatusHandler(newThreadStatusFixture())
	for _, query := range []string{
		"statuses[]=unknown", "statuses[]=", "statuses[]=idle&statuses[]=unknown", "statuses[]=IDLE",
	} {
		rec := do(h, http.MethodGet, "/v1/sessions/"+threadFilterSessionID+"/threads?"+query, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("query %q returned %d, want 400: %s", query, rec.Code, rec.Body)
		}
	}
}

func TestListSessionThreadsStatusFilterAndCursor(t *testing.T) {
	t.Parallel()
	fixture := newThreadStatusFixture()
	h := threadStatusHandler(fixture)
	path := "/v1/sessions/" + threadFilterSessionID + "/threads"
	all := decodeSessionList(t, h, path)
	if len(all.Data) != 5 {
		t.Fatalf("omitted statuses: %v", sessionIDs(all.Data))
	}
	first := decodeSessionList(t, h, path+"?limit=1&statuses[]=running&statuses[]=idle&statuses[]=running")
	if !slices.Equal(fixture.lastQuery.Statuses, []domain.Status{domain.StatusIdle, domain.StatusRunning}) {
		t.Fatalf("normalized statuses = %v", fixture.lastQuery.Statuses)
	}
	if !slices.Equal(sessionIDs(first.Data), []string{"sthr_filter_primary"}) || first.NextPage == nil {
		t.Fatalf("first page = %+v", first)
	}
	if first.Data[0]["parent_thread_id"] != nil || first.Data[0]["status"] != "idle" {
		t.Fatalf("primary = %v", first.Data[0])
	}
	second := decodeSessionList(t, h, path+"?limit=1&statuses[]=idle&statuses[]=running&page="+url.QueryEscape(*first.NextPage))
	if !slices.Equal(sessionIDs(second.Data), []string{"sthr_filter_running"}) || second.NextPage == nil {
		t.Fatalf("second page = %+v", second)
	}
	last := decodeSessionList(t, h, path+"?limit=1&statuses[]=running&statuses[]=idle&page="+url.QueryEscape(*second.NextPage))
	if !slices.Equal(sessionIDs(last.Data), []string{"sthr_filter_waiting"}) || last.NextPage != nil {
		t.Fatalf("last page = %+v", last)
	}
	for _, target := range []string{
		path + "?statuses[]=idle&page=", path + "?page=", "/v1/sessions/sesn_other/threads?statuses[]=idle&statuses[]=running&page=",
	} {
		rec := do(h, http.MethodGet, target+url.QueryEscape(*first.NextPage), "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("changed filter/session returned %d: %s", rec.Code, rec.Body)
		}
	}
	for _, item := range []struct {
		status string
		ids    []string
	}{
		{"idle", []string{"sthr_filter_primary", "sthr_filter_waiting"}},
		{"running", []string{"sthr_filter_running"}},
		{"rescheduling", []string{"sthr_filter_queued"}},
		{"terminated", []string{"sthr_filter_archived"}},
	} {
		page := decodeSessionList(t, h, path+"?statuses[]="+item.status)
		if !slices.Equal(sessionIDs(page.Data), item.ids) || page.NextPage != nil {
			t.Fatalf("status %s = %+v", item.status, page)
		}
		if item.status == "terminated" && page.Data[0]["archived_at"] == nil {
			t.Fatal("archived terminated child omitted")
		}
	}
	fixture.threads = fixture.threads[:1]
	empty := decodeSessionList(t, h, path+"?statuses[]=running")
	if empty.Data == nil || len(empty.Data) != 0 || empty.NextPage != nil {
		t.Fatalf("empty filtered page = %+v", empty)
	}
}
