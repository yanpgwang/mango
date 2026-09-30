package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/yanpgwang/mango/internal/workspace"
	mango "github.com/yanpgwang/mango/sdk/go"
)

type environmentKeyTestAuthenticator struct{}

func (environmentKeyTestAuthenticator) AuthenticateAPIKey(context.Context, string) (string, error) {
	return "", workspace.ErrInvalidAPIKey
}
func (environmentKeyTestAuthenticator) AuthenticateEnvironmentKey(_ context.Context, key string) (string, workspace.EnvironmentScope, error) {
	if key != "environment-key" {
		return "", workspace.EnvironmentScope{}, workspace.ErrInvalidEnvironmentKey
	}
	return "wrkspc_team", workspace.EnvironmentScope{EnvironmentID: "env_one", KeyID: "key_one", CredentialDigest: []byte("digest")}, nil
}

// Losing the default-deny scope or accidentally treating a scoped credential as
// a Workspace key makes these real HTTP authorization assertions fail.
func TestEnvironmentCredentialHTTPPermissions(t *testing.T) {
	service := newSDKEnvironmentWorkService()
	service.work.EnvironmentID = "env_one"
	handler := NewServer(Deps{EnvironmentWork: service}, Config{Authenticator: environmentKeyTestAuthenticator{}}).Handler()
	for _, test := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/v1/environments/env_one/work/poll", 200},
		{"POST", "/v1/environments/env_one/work/work_one/ack", 200},
		{"GET", "/v1/environments/env_one/work/stats", 200},
		{"GET", "/v1/environments/env_two/work/poll", 403},
		{"POST", "/v1/environments/env_two/work/work_one/ack", 403},
		{"GET", "/v1/environments/env_one/work", 403},
		{"GET", "/v1/environments/env_one/work/work_one", 403},
		{"POST", "/v1/environments/env_one/work/work_one/heartbeat", 403},
		{"POST", "/v1/environments/env_one/work/work_one/stop", 403},
		{"POST", "/v1/environments/env_one/work/work_one/fail", 403},
		{"POST", "/v1/environments/env_one/work/work_one", 403},
		{"GET", "/v1/environments/env_one", 403},
		{"GET", "/v1/sessions", 403},
		{"GET", "/v1/sessions/sesn_one", 403},
		{"POST", "/v1/sessions/sesn_one/events", 403},
		{"GET", "/v1/files", 403},
		{"POST", "/v1/files", 403},
		{"GET", "/v1/vaults", 403},
		{"GET", "/v1/memory_stores", 403},
		{"GET", "/v1/skills", 403},
	} {
		t.Run(test.method+test.path, func(t *testing.T) {
			req := httptest.NewRequest(test.method, test.path, nil)
			req.Header.Set("Authorization", "Bearer environment-key")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.status {
				t.Fatalf("status=%d, want %d: %s", rec.Code, test.status, rec.Body.String())
			}
			if test.status == 403 {
				var body struct {
					Type  string `json:"type"`
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
					} `json:"error"`
					RequestID string `json:"request_id"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatal(err)
				}
				if body.Type != "error" || body.Error.Type != "permission_error" || body.Error.Message != "environment credential is not authorized for this resource" || body.RequestID == "" {
					t.Fatalf("error envelope: %s", rec.Body.String())
				}
			}
		})
	}
	req := httptest.NewRequest("GET", "/v1/environments/env_one/work/poll", nil)
	req.Header.Set("Authorization", "Bearer revoked-key")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("invalid key status=%d", rec.Code)
	}
}

func TestMangoSDKEnvironmentCredential(t *testing.T) {
	service := newSDKEnvironmentWorkService()
	service.work.EnvironmentID = "env_one"
	server := httptest.NewServer(NewServer(Deps{EnvironmentWork: service}, Config{Authenticator: environmentKeyTestAuthenticator{}}).Handler())
	defer server.Close()
	client, err := mango.New(mango.Config{BaseURL: server.URL, APIKey: "environment-key"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	polled, err := client.Environments.Work.Poll(ctx, "env_one", mango.PollEnvironmentWorkParams{WorkerID: mango.Some("worker-local")})
	if err != nil || polled.EnvironmentWork == nil || polled.EnvironmentWork.Secret == nil {
		t.Fatalf("poll=%+v err=%v", polled, err)
	}
	ack, err := client.Environments.Work.Ack(ctx, "env_one", polled.EnvironmentWork.ID)
	if err != nil || ack.State != "starting" || ack.Secret != nil {
		t.Fatalf("ack=%+v err=%v", ack, err)
	}
	if _, err = client.Environments.Work.Stats(ctx, "env_one"); err != nil {
		t.Fatal(err)
	}
	if _, err = client.Sessions.Get(ctx, "sesn_one"); err == nil {
		t.Fatal("Environment key read Session")
	}
	if _, err = client.Environments.Work.Poll(ctx, "env_two", mango.PollEnvironmentWorkParams{}); err == nil {
		t.Fatal("Environment key polled another Environment")
	}
}
