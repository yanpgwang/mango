package pg

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/workspace"
)

func TestEnvironmentHealthcheckHTTPDurabilityAndClaimFencing(t *testing.T) {
	store := testStore(t)
	clock := &environmentWorkTestClock{now: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)}
	store.clock = clock
	ctx := context.Background()
	envs := NewEnvironmentRepository(store)
	if err := envs.Put(ctx, domain.Environment{ID: "env_check", Name: "check", ConfigType: "self_hosted", Config: map[string]any{"type": "self_hosted"}, CreatedAt: clock.Now(), UpdatedAt: clock.Now()}); err != nil {
		t.Fatal(err)
	}
	_, operator, err := store.CreateAPIKey(ctx, workspace.DefaultID, "healthcheck test")
	if err != nil {
		t.Fatal(err)
	}
	handler := httpapi.NewServer(httpapi.Deps{EnvironmentWork: app.NewEnvironmentWorkService(NewEnvironmentWorkRepository(store), envs)}, httpapi.Config{RequireAuth: true, Authenticator: store}).Handler()
	request := func(method, path, token, body string, status int) map[string]any {
		t.Helper()
		r := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s status %d, want %d: %s", method, path, w.Code, status, w.Body.String())
		}
		if w.Body.Len() == 0 {
			return nil
		}
		var value map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	const queue = "/v1/environments/env_check/work"
	request("POST", queue, operator, `{"data":{"type":"session","id":"sesn_fake"}}`, 400)
	request("POST", queue, operator, `{"data":{"type":"healthcheck","command":"uname -a"}}`, 400)
	created := request("POST", queue, operator, `{"data":{"type":"healthcheck"}}`, 201)
	id := created["id"].(string)
	path := queue + "/" + id
	data := created["data"].(map[string]any)
	if len(data) != 1 || data["type"] != "healthcheck" || created["result"] != nil || created["expires_at"] != "2026-09-29T00:02:00Z" || created["state"] != "queued" || created["secret"] != nil {
		t.Fatalf("created=%+v", created)
	}
	var count int
	if err := store.pool.QueryRow(ctx, "SELECT count(*) FROM sessions").Scan(&count); err != nil || count != 0 {
		t.Fatalf("synthetic Sessions=%d err=%v", count, err)
	}
	first := request("GET", queue+"/poll?worker_id=first", operator, "", 200)
	firstToken := decodeSessionsToken(t, first["secret"].(string))
	request("POST", path+"/result", firstToken, `{"status":"succeeded","message":"ok"}`, 401)
	request("POST", path+"/ack", operator, "", 200)
	request("POST", path+"/heartbeat", firstToken, "", 200)
	request("GET", "/v1/sessions/sesn_fake", firstToken, "", 403)
	request("POST", queue, firstToken, `{"data":{"type":"healthcheck"}}`, 403)
	request("POST", path+"/result", operator, `{"status":"succeeded","message":"ok"}`, 403)
	// Expiry of an active claim reuses the item and fences its former bearer.
	clock.Advance(31 * time.Second)
	next := request("GET", queue+"/poll?worker_id=second", operator, "", 200)
	if next["id"] != id {
		t.Fatalf("reclaim changed ID: %+v", next)
	}
	nextToken := decodeSessionsToken(t, next["secret"].(string))
	request("POST", path+"/ack", operator, "", 200)
	request("POST", path+"/result", firstToken, `{"status":"succeeded","message":"stale"}`, 401)
	request("POST", path+"/heartbeat", nextToken, "", 200)
	request("POST", path+"/result", nextToken, `{"status":"timed_out","message":"fake"}`, 400)
	done := request("POST", path+"/result", nextToken, `{"status":"succeeded","message":"ok"}`, 200)
	if done["state"] != "stopped" || done["result"].(map[string]any)["status"] != "succeeded" {
		t.Fatalf("result=%+v", done)
	}
	request("POST", path+"/result", nextToken, `{"status":"succeeded","message":"ok"}`, 200)
	request("POST", path+"/result", nextToken, `{"status":"failed","message":"changed"}`, 409)
	request("POST", path+"/heartbeat", nextToken, "", 403)
	request("POST", path+"/stop", nextToken, `{"force":true}`, 403)
	request("GET", queue+"/poll", nextToken, "", 403)
	clock.Advance(31 * time.Second)
	request("POST", path+"/result", nextToken, `{"status":"succeeded","message":"ok"}`, 401)
	durable := request("GET", path, operator, "", 200)
	if durable["result"].(map[string]any)["status"] != "succeeded" || durable["secret"] != nil {
		t.Fatalf("durable=%+v", durable)
	}

	// A check with no available worker still becomes visibly terminal on Get.
	queued := request("POST", queue, operator, `{"data":{"type":"healthcheck"}}`, 201)
	clock.Advance(120 * time.Second)
	expired := request("GET", queue+"/"+queued["id"].(string), operator, "", 200)
	if expired["state"] != "stopped" || expired["result"].(map[string]any)["status"] != "timed_out" {
		t.Fatalf("expired=%+v", expired)
	}
	request("GET", queue+"/poll", operator, "", 200)
	cancelled := request("POST", queue, operator, `{"data":{"type":"healthcheck"}}`, 201)
	cancelPath := queue + "/" + cancelled["id"].(string)
	request("POST", cancelPath+"/stop", operator, `{}`, 204)
	cancelled = request("GET", cancelPath, operator, "", 200)
	if cancelled["result"].(map[string]any)["status"] != "cancelled" {
		t.Fatalf("cancelled=%+v", cancelled)
	}
}

func TestEnvironmentHealthcheckStartingReclaimAndAuthenticatedRequestFences(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	clock := &environmentWorkTestClock{now: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)}
	store.clock = clock
	envs := NewEnvironmentRepository(store)
	if err := envs.Put(ctx, domain.Environment{ID: "env_fence", Name: "fence", ConfigType: "self_hosted", Config: map[string]any{"type": "self_hosted"}, CreatedAt: clock.Now(), UpdatedAt: clock.Now()}); err != nil {
		t.Fatal(err)
	}
	repo := NewEnvironmentWorkRepository(store)
	work, err := repo.CreateHealthcheck(ctx, "env_fence")
	if err != nil {
		t.Fatal(err)
	}
	first, err := repo.PollWork(ctx, "env_fence", app.EnvironmentWorkPollInput{WorkerID: "lost", ReclaimAge: time.Hour})
	if err != nil || first == nil {
		t.Fatalf("poll=%+v err=%v", first, err)
	}
	if _, err := repo.AckWork(ctx, "env_fence", work.ID); err != nil {
		t.Fatal(err)
	}
	workspaceID, firstScope, err := store.AuthenticateSessionToken(ctx, decodeSessionsToken(t, first.Secret))
	if err != nil {
		t.Fatal(err)
	}
	firstCtx := workspace.WithSessionScope(ctx, workspaceID, firstScope)
	if _, err := repo.CompleteHealthcheck(firstCtx, "env_fence", work.ID, domain.EnvironmentWorkResult{Status: "succeeded", Message: "before heartbeat"}); err == nil {
		t.Fatal("unstarted check completed")
	}
	clock.Advance(31 * time.Second)
	next, err := repo.PollWork(ctx, "env_fence", app.EnvironmentWorkPollInput{WorkerID: "replacement", ReclaimAge: time.Hour})
	if err != nil || next == nil || next.ID != work.ID {
		t.Fatalf("starting reclaim=%+v err=%v", next, err)
	}
	if _, err := repo.AckWork(ctx, "env_fence", work.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CompleteHealthcheck(firstCtx, "env_fence", work.ID, domain.EnvironmentWorkResult{Status: "succeeded", Message: "stale authentication"}); err == nil {
		t.Fatal("stale authenticated context completed successor")
	}
	workspaceID, nextScope, err := store.AuthenticateSessionToken(ctx, decodeSessionsToken(t, next.Secret))
	if err != nil {
		t.Fatal(err)
	}
	nextCtx := workspace.WithSessionScope(ctx, workspaceID, nextScope)
	if _, err := repo.HeartbeatWork(nextCtx, "env_fence", work.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	// A request authenticated just before the absolute deadline cannot commit
	// after it even though its context retains the formerly valid bearer digest.
	clock.Advance(89 * time.Second)
	if _, err := repo.CompleteHealthcheck(nextCtx, "env_fence", work.ID, domain.EnvironmentWorkResult{Status: "succeeded", Message: "after deadline"}); err == nil {
		t.Fatal("expired authenticated request completed")
	}
	got, err := repo.GetWork(ctx, "env_fence", work.ID)
	if err != nil || got.Result == nil || got.Result.Status != "timed_out" {
		t.Fatalf("deadline result=%+v err=%v", got, err)
	}
}
