package httpapi

import (
	"context"
	"net/http"
	"time"
)

const readinessTimeout = 2 * time.Second

func (s *Server) readiness(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ctx, cancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer cancel()
	if s.deps.Readiness == nil || s.deps.Readiness(ctx) != nil || ctx.Err() != nil {
		writeErrorEnvelope(w, http.StatusServiceUnavailable, "api_error", "API is not ready to accept durable requests")
		return
	}
	w.WriteHeader(http.StatusOK)
}
