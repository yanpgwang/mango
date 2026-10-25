package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func sessionThreadToJSON(thread domain.SessionThread) map[string]any {
	activeSeconds, durationSeconds := thread.ObservableStats(time.Now())
	var agent any
	if thread.Advisor != nil {
		agent = map[string]any{
			"type": "advisor", "model": thread.Advisor.Model,
		}
	} else {
		snapshot := agentSnapshotJSON(thread.Agent)
		// The thread snapshot describes only the executing Agent. The coordinator's
		// resolved roster remains available on Session.agent and is not repeated.
		delete(snapshot, "multiagent")
		agent = snapshot
	}
	listCost := any(nil)
	if thread.ListCostKnown {
		listCost = domain.MonetaryAmountJSON(thread.ModelListCostNanoUSD)
	}
	out := map[string]any{
		"id": thread.ID, "type": "session_thread",
		"session_id": thread.SessionID, "parent_thread_id": thread.ParentThreadID,
		"agent": agent, "status": string(thread.Status),
		"created_at": thread.CreatedAt.Format(timeFmt),
		"updated_at": thread.UpdatedAt.Format(timeFmt),
		"stats": map[string]any{
			"active_seconds": activeSeconds, "duration_seconds": durationSeconds,
			"startup_seconds": thread.StartupSeconds,
		},
		"usage": map[string]any{
			"active_seconds": activeSeconds,
			"cache_creation": map[string]any{
				"ephemeral_1h_input_tokens": thread.Usage.CacheCreation.Ephemeral1hInputTokens,
				"ephemeral_5m_input_tokens": thread.Usage.CacheCreation.Ephemeral5mInputTokens,
			},
			"cache_read_input_tokens": thread.Usage.CacheReadInputTokens,
			"input_tokens":            thread.Usage.InputTokens,
			"list_cost":               listCost,
			"output_tokens":           thread.Usage.OutputTokens,
			"server_tool_use": map[string]any{
				"web_fetch_requests":  thread.Usage.ServerToolUse.WebFetchRequests,
				"web_search_requests": thread.Usage.ServerToolUse.WebSearchRequests,
			},
		},
	}
	if thread.ArchivedAt == nil {
		out["archived_at"] = nil
	} else {
		out["archived_at"] = thread.ArchivedAt.Format(timeFmt)
	}
	return out
}

func (s *Server) getSessionThread(w http.ResponseWriter, r *http.Request) {
	if s.deps.Threads == nil {
		writeError(w, domain.Unsupported("Session Threads are not configured"))
		return
	}
	thread, err := s.deps.Threads.Get(
		r.Context(), r.PathValue("id"), r.PathValue("thread_id"),
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionThreadToJSON(thread))
}

func (s *Server) listSessionThreads(w http.ResponseWriter, r *http.Request) {
	if s.deps.Threads == nil {
		writeError(w, domain.Unsupported("Session Threads are not configured"))
		return
	}
	values := r.URL.Query()
	limit := app.DefaultSessionThreadListLimit
	if raw := values.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, domain.Validation("limit must be a positive integer"))
			return
		}
		if parsed > maxPageLimit {
			writeError(w, domain.Validation("limit must not exceed 1000"))
			return
		}
		limit = parsed
	}

	sessionID := r.PathValue("id")
	statuses, err := parseSessionStatusFilter(values["statuses[]"])
	if err != nil {
		writeError(w, err)
		return
	}
	fingerprint := resourceFilterFingerprint(map[string]string{
		"session_id": sessionID, "statuses": strings.Join(statuses, ","),
	})
	query := app.SessionThreadListQuery{Limit: limit + 1}
	for _, status := range statuses {
		query.Statuses = append(query.Statuses, domain.Status(status))
	}
	if token := values.Get("page"); token != "" {
		cursor, ok := decodeResourceCursor(token, sessionThreadListCursorKind)
		if !ok || cursor.Filter != fingerprint {
			writeError(w, domain.Validation("invalid page cursor"))
			return
		}
		createdAt, ok := parseTimeParam(cursor.CreatedAt)
		if !ok {
			writeError(w, domain.Validation("invalid page cursor"))
			return
		}
		query.Boundary = &app.SessionThreadPageBoundary{CreatedAt: *createdAt, ID: cursor.ID}
	}
	threads, err := s.deps.Threads.List(r.Context(), sessionID, query)
	if err != nil {
		writeError(w, err)
		return
	}
	var nextPage any
	if len(threads) > limit {
		threads = threads[:limit]
		last := threads[len(threads)-1]
		nextPage = encodeResourceCursor(resourceCursor{
			Kind:      sessionThreadListCursorKind,
			CreatedAt: last.CreatedAt.Format(timeFmt), ID: last.ID, Filter: fingerprint,
		})
	}
	data := make([]any, 0, len(threads))
	for _, thread := range threads {
		data = append(data, sessionThreadToJSON(thread))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "next_page": nextPage})
}

func (s *Server) archiveSessionThread(w http.ResponseWriter, r *http.Request) {
	if s.deps.Threads == nil {
		writeError(w, domain.Unsupported("Session Threads are not configured"))
		return
	}
	thread, err := s.deps.Threads.Archive(
		r.Context(), r.PathValue("id"), r.PathValue("thread_id"),
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, sessionThreadToJSON(thread))
}

func (s *Server) listSessionThreadEvents(w http.ResponseWriter, r *http.Request) {
	if s.deps.Threads == nil || s.deps.Events == nil {
		writeError(w, domain.Unsupported("Session Thread Events are not configured"))
		return
	}
	sessionID, threadID := r.PathValue("id"), r.PathValue("thread_id")
	thread, err := s.deps.Threads.Get(r.Context(), sessionID, threadID)
	if err != nil {
		writeError(w, err)
		return
	}
	limit := defaultEventLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed <= 0 {
			writeError(w, domain.Validation("limit must be a positive integer"))
			return
		}
		if parsed > maxPageLimit {
			writeError(w, domain.Validation("limit must not exceed 1000"))
			return
		}
		limit = parsed
	}
	query := app.EventQuery{ThreadID: thread.ID, Limit: limit + 1}
	fingerprint := eventCursorFilter{}.fingerprint()
	if token := r.URL.Query().Get("page"); token != "" {
		cursor, ok := decodeEventCursor(token)
		if !ok || cursor.Order != "asc" || cursor.SessionID != sessionID ||
			cursor.ThreadID != threadID || cursor.Filter != fingerprint {
			writeError(w, domain.Validation("invalid page cursor"))
			return
		}
		boundary := &app.EventPageBoundary{Sequence: cursor.Sequence}
		if !cursor.Unprocessed {
			processedAt, ok := parseTimeParam(cursor.ProcessedAt)
			if !ok {
				writeError(w, domain.Validation("invalid page cursor"))
				return
			}
			boundary.ProcessedAt = processedAt
		}
		query.Boundary = boundary
	}
	history, err := s.deps.Events.Query(r.Context(), sessionID, query)
	if err != nil {
		writeError(w, err)
		return
	}
	var nextPage any
	if len(history) > limit {
		history = history[:limit]
		last := history[len(history)-1]
		cursor := eventCursor{
			Order: "asc", SessionID: sessionID, ThreadID: threadID,
			Filter: fingerprint, Sequence: last.Sequence,
		}
		if last.ProcessedAt == nil {
			cursor.Unprocessed = true
		} else {
			cursor.ProcessedAt = last.ProcessedAt.UTC().Format(timeFmt)
		}
		nextPage = encodeEventCursor(cursor)
	}
	data := make([]any, 0, len(history))
	for _, event := range history {
		data = append(data, eventToJSON(event))
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "next_page": nextPage})
}

func (s *Server) streamSessionThreadEvents(w http.ResponseWriter, r *http.Request) {
	if s.deps.Threads == nil || s.deps.Stream == nil {
		writeError(w, domain.Unsupported("Session Thread event streaming is not configured"))
		return
	}
	optIn, err := parseEventDeltaOptIn(r)
	if err != nil {
		writeError(w, err)
		return
	}
	sessionID, threadID := r.PathValue("id"), r.PathValue("thread_id")
	// Subscribe first, then validate identity, matching the Session stream's
	// open-before-read deletion race guarantee.
	stream, ok := s.deps.Stream.(ThreadEventSubscriber)
	if !ok {
		writeError(w, domain.Unsupported(
			"Session Thread event streaming is not configured",
		))
		return
	}
	frames, cancel, err := stream.SubscribeThreadContext(
		r.Context(), sessionID, threadID, optIn,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	defer cancel()
	thread, err := s.deps.Threads.Get(r.Context(), sessionID, threadID)
	if err != nil {
		writeError(w, err)
		return
	}
	_ = thread
	writeEventStream(w, r, frames)
}

func parseEventDeltaOptIn(r *http.Request) (map[string]bool, error) {
	values := append([]string(nil), r.URL.Query()["event_deltas[]"]...)
	values = append(values, r.URL.Query()["event_deltas"]...)
	if len(values) > maxDeltaOptIn {
		return nil, domain.Validation(fmt.Sprintf(
			"event_deltas[] accepts at most %d values", maxDeltaOptIn,
		))
	}
	var result map[string]bool
	for _, value := range values {
		if !deltaOptInTypes[value] {
			return nil, domain.Validation("event_deltas[] value not accepted: " + value)
		}
		if result == nil {
			result = map[string]bool{}
		}
		result[value] = true
	}
	return result, nil
}

func writeEventStream(w http.ResponseWriter, r *http.Request, frames <-chan app.Frame) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, domain.Validation("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case frame, open := <-frames:
			if !open {
				return
			}
			switch {
			case frame.Event != nil:
				event := *frame.Event
				payload, _ := json.Marshal(eventToJSON(event))
				_, _ = fmt.Fprintf(w, "event: %s\n", event.Type)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
				flusher.Flush()
			case frame.Preview != nil:
				payload, _ := json.Marshal(frame.Preview.WireJSON())
				_, _ = fmt.Fprintf(w, "event: %s\n", frame.Preview.Kind)
				_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
				flusher.Flush()
			}
		}
	}
}
