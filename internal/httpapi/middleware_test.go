package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
)

type workspaceAuthenticatorFunc func(context.Context, string) (string, error)

func (f workspaceAuthenticatorFunc) AuthenticateAPIKey(ctx context.Context, key string) (string, error) {
	return f(ctx, key)
}

type sessionTokenTestAuthenticator struct{}

func (sessionTokenTestAuthenticator) AuthenticateAPIKey(context.Context, string) (string, error) {
	return "", workspace.ErrInvalidAPIKey
}

func (sessionTokenTestAuthenticator) AuthenticateSessionToken(
	_ context.Context,
	token string,
) (string, workspace.SessionScope, error) {
	if token != "session-token" {
		return "", workspace.SessionScope{}, workspace.ErrInvalidSessionToken
	}
	return "wrkspc_team", workspace.SessionScope{
		EnvironmentID: "env_one",
		WorkID:        "work_one",
		SessionID:     "sesn_one",
		Skills: map[workspace.SkillVersion]struct{}{
			{ID: "skill_one", Version: "v1"}: {},
		},
		Files: map[string]struct{}{"file_one": {}},
	}, nil
}

func TestWriteError_MapsKinds(t *testing.T) {
	cases := map[error]int{
		domain.Validation("x"):  400,
		domain.Conflict("x"):    409,
		domain.NotFound("x"):    404,
		domain.Unsupported("x"): 422,
		domain.TooLarge("x"):    413,
		domain.Permission("x"):  403,
	}
	for err, want := range cases {
		rec := httptest.NewRecorder()
		writeError(rec, err)
		if rec.Code != want {
			t.Errorf("err %v: got %d want %d", err, rec.Code, want)
		}
		env := decodeErrorEnvelope(t, rec.Body.Bytes())
		if env["message"] == "" || env["type"] == "" {
			t.Errorf("missing envelope fields: %v", env)
		}
		if rec.Header().Get("request-id") == "" {
			t.Errorf("missing request-id response header")
		}
	}
}

// decodeErrorEnvelope unwraps the standard {"type":"error","error":{...}}
// envelope into the inner {type,message} object.
func decodeErrorEnvelope(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var outer struct {
		Type  string            `json:"type"`
		Error map[string]string `json:"error"`
	}
	if err := json.Unmarshal(body, &outer); err != nil {
		t.Fatalf("unmarshal error envelope: %v (%s)", err, body)
	}
	if outer.Type != "error" {
		t.Fatalf("expected top-level type=error, got %q", outer.Type)
	}
	return outer.Error
}

func TestWriteError_PreservesMemoryPreconditionCode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, domain.MemoryPrecondition("stale Memory"))
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if got := decodeErrorEnvelope(t, rec.Body.Bytes())["type"]; got != "memory_precondition_failed_error" {
		t.Fatalf("error type = %q", got)
	}
}

func TestWriteError_EnvironmentWorkPreconditionUses412(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, domain.Precondition("work heartbeat precondition failed"))
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412", rec.Code)
	}
	if got := decodeErrorEnvelope(t, rec.Body.Bytes())["type"]; got != "precondition_failed_error" {
		t.Fatalf("error type = %v, want precondition_failed_error", got)
	}
}

func TestAuthMiddleware_RequiresBearerToken(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})

	h := authMiddleware(Config{RequireAuth: true}, ok)
	req := httptest.NewRequest("GET", "/v1/agents", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("expected 401 without bearer token, got %d", rec.Code)
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env["type"] == "" || env["message"] == "" {
		t.Errorf("expected non-empty type and message envelope, got %v", env)
	}

	req2 := httptest.NewRequest("GET", "/v1/agents", nil)
	req2.Header.Set("authorization", "Bearer sk-test")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("expected 200 with bearer token, got %d", rec2.Code)
	}

	xAPIKeyOnly := httptest.NewRequest("GET", "/v1/agents", nil)
	xAPIKeyOnly.Header.Set("x-api-key", "sk-test")
	xAPIKeyRec := httptest.NewRecorder()
	h.ServeHTTP(xAPIKeyRec, xAPIKeyOnly)
	if xAPIKeyRec.Code != http.StatusUnauthorized {
		t.Fatalf("x-api-key-only status = %d, want 401", xAPIKeyRec.Code)
	}

	malformed := httptest.NewRequest("GET", "/v1/agents", nil)
	malformed.Header.Set("authorization", "Basic sk-test")
	malformedRec := httptest.NewRecorder()
	h.ServeHTTP(malformedRec, malformed)
	if malformedRec.Code != http.StatusUnauthorized {
		t.Fatalf("malformed authorization status = %d, want 401", malformedRec.Code)
	}

	hNoAuth := authMiddleware(Config{RequireAuth: false}, ok)
	req3 := httptest.NewRequest("GET", "/v1/agents", nil)
	rec3 := httptest.NewRecorder()
	hNoAuth.ServeHTTP(rec3, req3)
	if rec3.Code != 200 {
		t.Fatalf("expected 200 when RequireAuth=false and no key, got %d", rec3.Code)
	}
}

func TestAuthMiddleware_AuthenticatesAndScopesWorkspace(t *testing.T) {
	authenticator := workspaceAuthenticatorFunc(func(_ context.Context, key string) (string, error) {
		if key != "sk-valid" {
			return "", workspace.ErrInvalidAPIKey
		}
		return "wrkspc_team", nil
	})
	h := authMiddleware(Config{Authenticator: authenticator}, http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			scope, err := workspace.Require(r.Context())
			if err != nil || scope.ID != "wrkspc_team" {
				t.Fatalf("scope = %+v, %v", scope, err)
			}
			w.WriteHeader(http.StatusNoContent)
		},
	))

	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	req.Header.Set("authorization", "Bearer sk-valid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}

	invalid := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	invalid.Header.Set("authorization", "Bearer sk-invalid")
	invalidRec := httptest.NewRecorder()
	h.ServeHTTP(invalidRec, invalid)
	if invalidRec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid status = %d, want 401", invalidRec.Code)
	}
}

func TestAuthMiddleware_SessionTokenIsResourceScoped(t *testing.T) {
	h := authMiddleware(Config{Authenticator: sessionTokenTestAuthenticator{}}, http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			scope, err := workspace.Require(r.Context())
			if err != nil || scope.ID != "wrkspc_team" || scope.Session == nil ||
				scope.Session.SessionID != "sesn_one" {
				t.Fatalf("scope = %+v, %v", scope, err)
			}
			w.WriteHeader(http.StatusNoContent)
		},
	))
	tests := []struct {
		name, method, path string
		want               int
	}{
		{name: "heartbeat", method: http.MethodPost, path: "/v1/environments/env_one/work/work_one/heartbeat", want: 204},
		{name: "stop", method: http.MethodPost, path: "/v1/environments/env_one/work/work_one/stop", want: 204},
		{name: "session", method: http.MethodGet, path: "/v1/sessions/sesn_one", want: 204},
		{name: "events list", method: http.MethodGet, path: "/v1/sessions/sesn_one/events", want: 204},
		{name: "events send", method: http.MethodPost, path: "/v1/sessions/sesn_one/events", want: 204},
		{name: "events stream", method: http.MethodGet, path: "/v1/sessions/sesn_one/events/stream", want: 204},
		{name: "skill content", method: http.MethodGet, path: "/v1/skills/skill_one/versions/v1/content", want: 204},
		{name: "file content", method: http.MethodGet, path: "/v1/files/file_one/content", want: 204},
		{name: "other session", method: http.MethodGet, path: "/v1/sessions/sesn_two", want: 403},
		{name: "session mutation", method: http.MethodDelete, path: "/v1/sessions/sesn_one", want: 403},
		{name: "other work", method: http.MethodPost, path: "/v1/environments/env_one/work/work_two/stop", want: 403},
		{name: "other skill", method: http.MethodGet, path: "/v1/skills/skill_two/versions/v1/content", want: 403},
		{name: "workspace list", method: http.MethodGet, path: "/v1/sessions", want: 403},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, nil)
			req.Header.Set("authorization", "Bearer session-token")
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != test.want {
				t.Fatalf("status = %d, want %d: %s", rec.Code, test.want, rec.Body.String())
			}
		})
	}
	invalid := httptest.NewRequest(http.MethodGet, "/v1/sessions/sesn_one", nil)
	invalid.Header.Set("authorization", "Bearer wrong-token")
	invalidRec := httptest.NewRecorder()
	h.ServeHTTP(invalidRec, invalid)
	if invalidRec.Code != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d, want 401", invalidRec.Code)
	}
}

func TestAuthMiddleware_DistinguishesStoreFailureAndPublicHealth(t *testing.T) {
	calls := 0
	authenticator := workspaceAuthenticatorFunc(func(context.Context, string) (string, error) {
		calls++
		return "", errors.New("database unavailable")
	})
	h := authMiddleware(Config{Authenticator: authenticator}, http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
	))

	req := httptest.NewRequest(http.MethodGet, "/v1/agents", nil)
	req.Header.Set("authorization", "Bearer sk-any")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("store failure status = %d, want 500", rec.Code)
	}

	health := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	healthRec := httptest.NewRecorder()
	h.ServeHTTP(healthRec, health)
	if healthRec.Code != http.StatusNoContent || calls != 1 {
		t.Fatalf("health status/calls = %d/%d, want 204/1", healthRec.Code, calls)
	}
}

func TestTransportMiddlewareDoesNotProtectPublicRoutes(t *testing.T) {
	cfg := Config{
		RequireAuth: true,
		Authenticator: workspaceAuthenticatorFunc(func(context.Context, string) (string, error) {
			return "", errors.New("authenticator must not be called")
		}),
	}
	h := authMiddleware(cfg, contentTypeMiddleware(http.HandlerFunc(
		func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
	)))
	for _, path := range []string{"/healthz", "/readyz", "/openapi.yaml"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("GET %s status = %d, want 204", path, rec.Code)
		}
	}
}

func TestContentTypeMiddlewareRequiresDocumentedMediaType(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	h := contentTypeMiddleware(ok)

	req := httptest.NewRequest("POST", "/v1/agents", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("expected 400 without application/json, got %d", rec.Code)
	}

	req = httptest.NewRequest("POST", "/v1/agents", strings.NewReader(`{}`))
	req.Header.Set("content-type", "application/json; charset=utf-8")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("expected 200 with application/json, got %d", rec.Code)
	}

	req = httptest.NewRequest("POST", "/v1/files", strings.NewReader("upload"))
	req.Header.Set("content-type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("file upload status = %d, want 400", rec.Code)
	}
}

func TestRequestIDMiddleware_AllResponses(t *testing.T) {
	h := requestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeError(w, domain.Validation("bad"))
	}))
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	headerID := rec.Header().Get("request-id")
	if headerID == "" {
		t.Fatal("missing request-id response header")
	}
	var body struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.RequestID != headerID {
		t.Fatalf("body request_id %q != header %q", body.RequestID, headerID)
	}
}

func TestUnknownRouteUsesJSONErrorEnvelope(t *testing.T) {
	h := NewTestHandler(t)
	rec := do(h, "GET", "/v1/not-a-resource", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	env := decodeErrorEnvelope(t, rec.Body.Bytes())
	if env["type"] != "not_found_error" {
		t.Fatalf("error type = %q, want not_found_error", env["type"])
	}
	if rec.Header().Get("request-id") == "" {
		t.Fatal("missing request-id header")
	}
}
