package mango

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"testing"
)

func TestAutoPaginationPreservesFiltersAndEscapesCursor(t *testing.T) {
	var calls int
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("limit") != "1" || r.URL.Query().Get("include_archived") != "false" {
			t.Error("filters lost")
		}
		if calls == 1 {
			fmt.Fprint(w, `{"data":[{"id":"a"}],"next_page":"next/?=&"}`)
		} else {
			if r.URL.Query().Get("page") != "next/?=&" {
				t.Errorf("cursor %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"data":[{"id":"b"}],"next_page":null}`)
		}
	})
	iterator := client.ListAgentsAutoPaging(context.Background(), ListAgentsParams{Limit: Some(int64(1)), IncludeArchived: Some(false)})
	var ids []string
	for iterator.Next() {
		ids = append(ids, iterator.Value().ID)
	}
	if iterator.Err() != nil {
		t.Fatal(iterator.Err())
	}
	if !reflect.DeepEqual(ids, []string{"a", "b"}) || calls != 2 {
		t.Fatalf("ids %v calls %d", ids, calls)
	}
}

func TestFilesPaginationUsesAfterID(t *testing.T) {
	var calls int
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			fmt.Fprint(w, `{"data":[{"id":"file_a"}],"has_more":true,"first_id":"file_a","last_id":"file_a"}`)
		} else {
			if r.URL.Query().Get("after_id") != "file_a" || r.URL.Query().Has("page") {
				t.Errorf("wrong Files cursor %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `{"data":[{"id":"file_b"}],"has_more":false,"first_id":"file_b","last_id":"file_b"}`)
		}
	})
	iterator := client.ListFilesAutoPaging(context.Background(), ListFilesParams{})
	var ids []string
	for iterator.Next() {
		ids = append(ids, iterator.Value().ID)
	}
	if iterator.Err() != nil {
		t.Fatal(iterator.Err())
	}
	if !reflect.DeepEqual(ids, []string{"file_a", "file_b"}) {
		t.Fatal(ids)
	}
}

func TestPaginationDetectsStuckCursorAndCancellation(t *testing.T) {
	iterator := NewPageIterator(context.Background(), "", func(_ context.Context, _ string) (Page[int], error) { return Page[int]{Next: "loop"}, nil })
	if iterator.Next() || iterator.Err() == nil {
		t.Fatal("repeated cursor not rejected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	iterator = NewPageIterator(ctx, "", func(_ context.Context, _ string) (Page[int], error) {
		t.Fatal("fetch after cancel")
		return Page[int]{}, nil
	})
	if iterator.Next() || iterator.Err() == nil {
		t.Fatal("cancel not returned")
	}
}
