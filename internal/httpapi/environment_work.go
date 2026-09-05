package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
)

func (s *Server) environmentWorkConfigured(w http.ResponseWriter) bool {
	if s.deps.EnvironmentWork != nil {
		return true
	}
	writeError(w, domain.Unsupported("Environment Work is unavailable for the configured server"))
	return false
}

func environmentWorkToJSON(work domain.EnvironmentWork) map[string]any {
	metadata := work.Metadata
	if metadata == nil {
		metadata = map[string]string{}
	}
	var secret any
	if work.Secret != "" {
		secret = work.Secret
	}
	out := map[string]any{
		"id": work.ID, "type": "work", "environment_id": work.EnvironmentID,
		"data":  map[string]any{"type": "session", "id": work.SessionID},
		"state": work.State, "metadata": metadata, "secret": secret,
		"created_at": work.CreatedAt.Format(timeFmt),
	}
	setWorkTime := func(name string, value *time.Time) {
		if value == nil {
			out[name] = nil
		} else {
			out[name] = value.Format(timeFmt)
		}
	}
	setWorkTime("acknowledged_at", work.AcknowledgedAt)
	setWorkTime("started_at", work.StartedAt)
	setWorkTime("latest_heartbeat_at", work.LatestHeartbeatAt)
	setWorkTime("stop_requested_at", work.StopRequestedAt)
	setWorkTime("stopped_at", work.StoppedAt)
	return out
}

func (s *Server) getEnvironmentWork(w http.ResponseWriter, r *http.Request) {
	if !s.environmentWorkConfigured(w) {
		return
	}
	work, err := s.deps.EnvironmentWork.Get(
		r.Context(), r.PathValue("environment_id"), r.PathValue("work_id"),
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, environmentWorkToJSON(work))
}

func (s *Server) updateEnvironmentWork(w http.ResponseWriter, r *http.Request) {
	if !s.environmentWorkConfigured(w) {
		return
	}
	var body struct {
		Metadata optionalJSONField[map[string]*string] `json:"metadata"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if !body.Metadata.Present || body.Metadata.Null {
		writeError(w, domain.Validation("metadata is required and cannot be null"))
		return
	}
	work, err := s.deps.EnvironmentWork.Update(
		r.Context(), r.PathValue("environment_id"), r.PathValue("work_id"),
		body.Metadata.Value,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, environmentWorkToJSON(work))
}

func (s *Server) listEnvironmentWork(w http.ResponseWriter, r *http.Request) {
	if !s.environmentWorkConfigured(w) {
		return
	}
	limit := app.DefaultEnvironmentWorkListLimit
	var err error
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err = parseResourceListLimit(raw)
		if err != nil || limit > app.MaxEnvironmentWorkListLimit {
			if err == nil {
				err = domain.Validation("limit must not exceed 1000")
			}
			writeError(w, err)
			return
		}
	}
	environmentID := r.PathValue("environment_id")
	filter := environmentWorkCursorFilter{EnvironmentID: environmentID}
	query := app.EnvironmentWorkListQuery{Limit: limit}
	if token := r.URL.Query().Get("page"); token != "" {
		cursor, ok := decodeResourceCursor(token, environmentWorkListCursorKind)
		createdAt, timeErr := time.Parse(timeFmt, cursor.CreatedAt)
		if !ok || timeErr != nil || cursor.Filter != filter.fingerprint() {
			writeError(w, domain.Validation("page is invalid for this Environment Work query"))
			return
		}
		query.After = &app.ResourcePageBoundary{CreatedAt: createdAt, ID: cursor.ID}
	}
	page, err := s.deps.EnvironmentWork.List(r.Context(), environmentID, query)
	if err != nil {
		writeError(w, err)
		return
	}
	data := make([]map[string]any, len(page.Work))
	for index := range page.Work {
		data[index] = environmentWorkToJSON(page.Work[index])
	}
	var next any
	if page.HasNext && len(page.Work) > 0 {
		last := page.Work[len(page.Work)-1]
		next = encodeResourceCursor(resourceCursor{
			Kind: environmentWorkListCursorKind, CreatedAt: last.CreatedAt.Format(timeFmt),
			ID: last.ID, Filter: filter.fingerprint(),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": data, "next_page": next})
}

func (s *Server) ackEnvironmentWork(w http.ResponseWriter, r *http.Request) {
	if !s.environmentWorkConfigured(w) {
		return
	}
	work, err := s.deps.EnvironmentWork.Ack(
		r.Context(), r.PathValue("environment_id"), r.PathValue("work_id"),
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, environmentWorkToJSON(work))
}

func (s *Server) heartbeatEnvironmentWork(w http.ResponseWriter, r *http.Request) {
	if !s.environmentWorkConfigured(w) {
		return
	}
	var expected *string
	if values, present := r.URL.Query()["expected_last_heartbeat"]; present {
		if len(values) != 1 || values[0] == "" {
			writeError(w, domain.Validation("expected_last_heartbeat must be a non-empty string"))
			return
		}
		expected = &values[0]
	}
	var desired *int64
	if values, present := r.URL.Query()["desired_ttl_seconds"]; present {
		if len(values) != 1 {
			writeError(w, domain.Validation("desired_ttl_seconds must be an integer from 1 through 300"))
			return
		}
		value, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || value <= 0 || value > app.MaxEnvironmentWorkTTLSeconds {
			writeError(w, domain.Validation("desired_ttl_seconds must be an integer from 1 through 300"))
			return
		}
		desired = &value
	}
	heartbeat, err := s.deps.EnvironmentWork.Heartbeat(
		r.Context(), r.PathValue("environment_id"), r.PathValue("work_id"),
		expected, desired,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": "work_heartbeat", "last_heartbeat": heartbeat.LastHeartbeat.Format(timeFmt),
		"lease_extended": heartbeat.LeaseExtended, "state": heartbeat.State,
		"ttl_seconds": heartbeat.TTLSeconds,
	})
}

func (s *Server) pollEnvironmentWork(w http.ResponseWriter, r *http.Request) {
	if !s.environmentWorkConfigured(w) {
		return
	}
	var block time.Duration
	workerID := ""
	if values, present := r.URL.Query()["worker_id"]; present {
		if len(values) != 1 || values[0] == "" {
			writeError(w, domain.Validation("worker_id must be a non-empty string"))
			return
		}
		workerID = values[0]
	}
	if values, present := r.URL.Query()["block_ms"]; present {
		if len(values) != 1 {
			writeError(w, domain.Validation("block_ms must be an integer from 1 through 999"))
			return
		}
		value, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || value < 1 || value > 999 {
			writeError(w, domain.Validation("block_ms must be an integer from 1 through 999"))
			return
		}
		block = time.Duration(value) * time.Millisecond
	}
	var reclaim *time.Duration
	if values, present := r.URL.Query()["reclaim_older_than_ms"]; present {
		if len(values) != 1 {
			writeError(w, domain.Validation("reclaim_older_than_ms must be a non-negative integer"))
			return
		}
		value, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || value < 0 {
			writeError(w, domain.Validation("reclaim_older_than_ms must be a non-negative integer"))
			return
		}
		duration := time.Duration(value) * time.Millisecond
		if duration < 0 || int64(duration/time.Millisecond) != value {
			writeError(w, domain.Validation("reclaim_older_than_ms is too large"))
			return
		}
		reclaim = &duration
	}
	work, err := s.deps.EnvironmentWork.Poll(
		r.Context(), r.PathValue("environment_id"), workerID,
		block, reclaim,
	)
	if err != nil {
		writeError(w, err)
		return
	}
	if work == nil {
		// Poll differs from Stop: the official WorkPoller decodes an empty 200
		// object and treats its missing ID as an empty queue. A 204 currently
		// trips the Go SDK's strict response decoder before the helper can drain.
		writeJSON(w, http.StatusOK, map[string]any{})
		return
	}
	writeJSON(w, http.StatusOK, environmentWorkToJSON(*work))
}

func (s *Server) environmentWorkStats(w http.ResponseWriter, r *http.Request) {
	if !s.environmentWorkConfigured(w) {
		return
	}
	stats, err := s.deps.EnvironmentWork.Stats(r.Context(), r.PathValue("environment_id"))
	if err != nil {
		writeError(w, err)
		return
	}
	var oldest any
	if stats.OldestQueuedAt != nil {
		oldest = stats.OldestQueuedAt.Format(timeFmt)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"type": "work_queue_stats", "depth": stats.Depth, "pending": stats.Pending,
		"oldest_queued_at": oldest, "workers_polling": stats.WorkersPolling,
	})
}

func (s *Server) stopEnvironmentWork(w http.ResponseWriter, r *http.Request) {
	if !s.environmentWorkConfigured(w) {
		return
	}
	var body struct {
		Force optionalJSONField[bool] `json:"force"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if body.Force.Null {
		writeError(w, domain.Validation("force cannot be null"))
		return
	}
	if err := s.deps.EnvironmentWork.Stop(
		r.Context(), r.PathValue("environment_id"), r.PathValue("work_id"),
		body.Force.Value,
	); err != nil {
		writeError(w, err)
		return
	}
	ensureRequestID(w)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) failEnvironmentWork(w http.ResponseWriter, r *http.Request) {
	if !s.environmentWorkConfigured(w) {
		return
	}
	var body struct {
		Message string `json:"message"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeError(w, err)
		return
	}
	if err := s.deps.EnvironmentWork.Fail(
		r.Context(), r.PathValue("environment_id"), r.PathValue("work_id"), body.Message,
	); err != nil {
		writeError(w, err)
		return
	}
	ensureRequestID(w)
	w.WriteHeader(http.StatusNoContent)
}
