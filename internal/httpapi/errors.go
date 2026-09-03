package httpapi

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/yanpgwang/mango/internal/domain"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	ensureRequestID(w)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError renders Mango's current error envelope. Its shape was retained
// after early design research because it remains a concise tagged envelope:
//
//	{"type":"error","error":{"type":"invalid_request_error","message":"..."}}
//
// Mango owns this envelope. Some third-party clients can also decode it, which
// remains optional research evidence rather than a compatibility requirement.
func writeError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	typ := "api_error"
	var de *domain.DomainError
	if errors.As(err, &de) {
		switch de.Kind {
		case domain.KindValidation:
			status, typ = http.StatusBadRequest, "invalid_request_error"
		case domain.KindConflict:
			// Mango surfaces optimistic-concurrency and state conflicts
			// as 409; the closest documented error type is invalid_request_error,
			// but 409 status is what the SDK checks. Keep a distinct type string.
			status, typ = http.StatusConflict, "conflict_error"
		case domain.KindNotFound:
			status, typ = http.StatusNotFound, "not_found_error"
		case domain.KindUnsupported:
			status, typ = http.StatusUnprocessableEntity, "invalid_request_error"
		case domain.KindTooLarge:
			status, typ = http.StatusRequestEntityTooLarge, "request_too_large"
		case domain.KindPrecondition:
			status, typ = http.StatusPreconditionFailed, "precondition_failed_error"
		case domain.KindPermission:
			status, typ = http.StatusForbidden, "permission_error"
		}
		if de.Code != "" {
			typ = de.Code
		}
	}
	writeErrorEnvelope(w, status, typ, err.Error())
}

func writeErrorEnvelope(w http.ResponseWriter, status int, typ, message string) {
	requestID := ensureRequestID(w)
	writeJSON(w, status, map[string]any{
		"type":       "error",
		"error":      map[string]any{"type": typ, "message": message},
		"request_id": requestID,
	})
}

func ensureRequestID(w http.ResponseWriter) string {
	if id := w.Header().Get("request-id"); id != "" {
		return id
	}
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failure is extraordinarily rare. Keep the contract intact
		// without surfacing implementation details to the caller.
		id := "req_unavailable"
		w.Header().Set("request-id", id)
		return id
	}
	id := fmt.Sprintf("req_%x", b[:])
	w.Header().Set("request-id", id)
	return id
}
