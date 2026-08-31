package mango

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSSEChunkBoundariesCommentsMultilineCRLFAndID(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		for _, chunk := range []string{"\uFEFF:hello\r", "\nid: evt_1\r\nevent: agent.message\r\nretry: 1200\r\ndata: {\"id\":\"evt_1\",\r\n", "data: \"ok\":true}\r\n\r", "\ndata: {}\n\nid: ignored\x00bad\n", "data: []\r\r", "data: incomplete"} {
			fmt.Fprint(w, chunk)
			w.(http.Flusher).Flush()
		}
	})
	stream, err := client.StreamSessionEvents(context.Background(), "s", StreamSessionEventsParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Next() {
		t.Fatalf("no event: %v", stream.Err())
	}
	event := stream.Event()
	if event.ID != "evt_1" || event.Event != "agent.message" || string(event.Data) != "{\"id\":\"evt_1\",\n\"ok\":true}" || event.Retry != 1200*time.Millisecond {
		t.Fatalf("event %#v", event)
	}
	if !stream.Next() || stream.Event().Event != "message" || stream.Event().ID != "evt_1" {
		t.Fatal("default event or sticky ID missing")
	}
	if !stream.Next() || stream.Event().ID != "evt_1" {
		t.Fatal("NUL id was not ignored")
	}
	if stream.Next() {
		t.Fatal("unterminated final event should be discarded")
	}
	if stream.Err() != nil {
		t.Fatal(stream.Err())
	}
}

func TestSSEBOMIsStrippedOnlyAtStart(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "\uFEFFdata: first\n\n\uFEFFdata: ignored\n\ndata: last\n\n")
	})
	stream, err := client.StreamSessionEvents(context.Background(), "s", StreamSessionEventsParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Next() || string(stream.Event().Data) != "first" {
		t.Fatal("initial BOM not stripped")
	}
	if !stream.Next() || string(stream.Event().Data) != "last" {
		t.Fatal("noninitial BOM was incorrectly stripped")
	}
	if stream.Next() || stream.Err() != nil {
		t.Fatalf("unexpected end: %v", stream.Err())
	}
}

func TestSSESupportsLargeAdmittedEvents(t *testing.T) {
	const size = 17 << 20
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: ", strings.Repeat("x", size), "\n\n")
	})
	stream, err := client.StreamSessionEvents(context.Background(), "s", StreamSessionEventsParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Next() || len(stream.Event().Data) != size {
		t.Fatalf("large event rejected: %v", stream.Err())
	}
}

func TestSSEIgnoresFiniteTimeoutAndCancels(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		select {
		case <-time.After(40 * time.Millisecond):
			fmt.Fprint(w, "data: {}\n\n")
			w.(http.Flusher).Flush()
		case <-r.Context().Done():
			return
		}
		<-r.Context().Done()
	})
	client.requestTimeout = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.StreamSessionEvents(ctx, "s", StreamSessionEventsParams{})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if !stream.Next() {
		t.Fatalf("finite timeout interrupted SSE: %v", stream.Err())
	}
	cancel()
	if stream.Next() {
		t.Error("received event after cancellation")
	}
	if !errors.Is(stream.Err(), context.Canceled) {
		t.Fatalf("cancel error: %v", stream.Err())
	}
}

func TestSSERejectsWrongContentType(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "{}") })
	if _, err := client.StreamSessionEvents(context.Background(), "s", StreamSessionEventsParams{}); err == nil {
		t.Fatal("accepted non SSE response")
	}
}
