package pg

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func TestSessionThreadStatusFilterBeforePagination(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	session := newSession("sesn_thread_status_filter")
	if _, err := store.CreateSession(ctx, session, []domain.EventDraft{{
		Type:    domain.EvUserMessage,
		Payload: map[string]any{"content": []any{map[string]any{"type": "text", "text": "run"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	initial, err := store.ListSessionThreads(ctx, session.ID, app.SessionThreadListQuery{Limit: 10})
	if err != nil || len(initial) != 1 {
		t.Fatalf("initial threads = %+v, %v", initial, err)
	}
	primary := initial[0]
	if primary.Status != domain.StatusRunning {
		t.Fatalf("seeded primary status = %s, want running", primary.Status)
	}
	for index, status := range []domain.Status{domain.StatusTerminated, domain.StatusIdle, domain.StatusRescheduling, domain.StatusRunning} {
		created := primary.CreatedAt.Add(time.Duration(index+1) * time.Second)
		child := domain.SessionThread{
			ID: "sthr_filter_" + string(status), SessionID: session.ID, ParentThreadID: &primary.ID,
			Status: status, Agent: primary.Agent, CreatedAt: created, UpdatedAt: created,
		}
		if status == domain.StatusTerminated {
			child.ArchivedAt = &created
		}
		body, err := json.Marshal(child)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.pool.Exec(ctx, `
INSERT INTO session_threads (id, session_id, parent_thread_id, kind, status, body, created_at, updated_at, archived_at)
VALUES ($1, $2, $3, 'child', $4, $5, $6, $6, $7)`, child.ID, session.ID, primary.ID, status, body, created, child.ArchivedAt); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name     string
		statuses []domain.Status
		want     []string
	}{
		{"all", nil, []string{primary.ID, "sthr_filter_terminated", "sthr_filter_idle", "sthr_filter_rescheduling", "sthr_filter_running"}},
		{"primary and child", []domain.Status{domain.StatusRunning}, []string{primary.ID, "sthr_filter_running"}},
		{"gaps before limit", []domain.Status{domain.StatusIdle, domain.StatusRescheduling}, []string{"sthr_filter_idle", "sthr_filter_rescheduling"}},
		{"archived", []domain.Status{domain.StatusTerminated}, []string{"sthr_filter_terminated"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			query := app.SessionThreadListQuery{Limit: 1, Statuses: test.statuses}
			var ids []string
			for range 6 {
				page, err := store.ListSessionThreads(ctx, session.ID, query)
				if err != nil {
					t.Fatal(err)
				}
				if len(page) == 0 {
					break
				}
				if len(page) != 1 {
					t.Fatalf("limit ignored: %+v", page)
				}
				thread := page[0]
				ids = append(ids, thread.ID)
				if test.name == "archived" && thread.ArchivedAt == nil {
					t.Fatal("archived child not retained")
				}
				query.Boundary = &app.SessionThreadPageBoundary{CreatedAt: thread.CreatedAt, ID: thread.ID}
			}
			if !slices.Equal(ids, test.want) {
				t.Fatalf("page ids = %v, want %v", ids, test.want)
			}
		})
	}
	// A valid filter with no matches must not fall back to the unfiltered list.
	if _, err := store.pool.Exec(ctx, `DELETE FROM session_threads WHERE session_id = $1 AND kind = 'child'`, session.ID); err != nil {
		t.Fatal(err)
	}
	page, err := store.ListSessionThreads(ctx, session.ID, app.SessionThreadListQuery{Limit: 1, Statuses: []domain.Status{domain.StatusIdle}})
	if err != nil || len(page) != 0 {
		t.Fatalf("empty filter = %+v, %v", page, err)
	}
}
