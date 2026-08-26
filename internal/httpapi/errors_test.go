package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/yanpgwang/mango/internal/app"
)

func TestSessionResourceRunClassificationDoesNotChangeHTTPErrorContract(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, app.SessionResourceNotFoundError("repository no longer exists"))
	})
	response := do(handler, http.MethodPost, "/v1/sessions", `{}`)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Type != "error" || body.Error.Type != "invalid_request_error" ||
		body.Error.Message != "repository no longer exists" {
		t.Fatalf("error envelope = %+v", body)
	}
}
