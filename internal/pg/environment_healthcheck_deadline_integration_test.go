package pg

import (
	"context"
	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/workspace"
	"testing"
	"time"
)

func TestEnvironmentHealthcheckDeadlineAfterRowLock(t *testing.T) {
	for _, operation := range []string{"ack", "heartbeat", "heartbeat_precondition", "stop", "complete"} {
		t.Run(operation, func(t *testing.T) {
			store := testStore(t)
			clock := &environmentWorkTestClock{now: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)}
			store.clock = clock
			ctx := context.Background()
			envs := NewEnvironmentRepository(store)
			if err := envs.Put(ctx, domain.Environment{ID: "env_deadline_deadline", Name: "deadline", ConfigType: "self_hosted", Config: map[string]any{"type": "self_hosted"}, CreatedAt: clock.Now(), UpdatedAt: clock.Now()}); err != nil {
				t.Fatal(err)
			}
			repo := NewEnvironmentWorkRepository(store)
			work, err := repo.CreateHealthcheck(ctx, "env_deadline_deadline")
			if err != nil {
				t.Fatal(err)
			}
			clock.Advance(119 * time.Second)
			claimed, err := repo.PollWork(ctx, work.EnvironmentID, app.EnvironmentWorkPollInput{})
			if err != nil {
				t.Fatal(err)
			}
			if operation != "ack" {
				if _, err := repo.AckWork(ctx, work.EnvironmentID, work.ID); err != nil {
					t.Fatal(err)
				}
			}
			callCtx := ctx
			if operation == "complete" {
				if _, err := repo.HeartbeatWork(ctx, work.EnvironmentID, work.ID, nil, nil); err != nil {
					t.Fatal(err)
				}
				workspaceID, scope, err := store.AuthenticateSessionToken(ctx, decodeSessionsToken(t, claimed.Secret))
				if err != nil {
					t.Fatal(err)
				}
				callCtx = workspace.WithSessionScope(ctx, workspaceID, scope)
			}
			tx, err := store.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, "SELECT id FROM environment_work WHERE id=$1 FOR UPDATE", work.ID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				var err error
				switch operation {
				case "ack":
					_, err = repo.AckWork(ctx, work.EnvironmentID, work.ID)
				case "heartbeat":
					_, err = repo.HeartbeatWork(ctx, work.EnvironmentID, work.ID, nil, nil)
				case "heartbeat_precondition":
					expected := "stale"
					_, err = repo.HeartbeatWork(ctx, work.EnvironmentID, work.ID, &expected, nil)
				case "stop":
					err = repo.StopWork(ctx, work.EnvironmentID, work.ID, false)
				case "complete":
					_, err = repo.CompleteHealthcheck(callCtx, work.EnvironmentID, work.ID, domain.EnvironmentWorkResult{Status: "succeeded", Message: "ok"})
				}
				done <- err
			}()
			waitForCredentialSQLLock(t, ctx, store, "environment_work", done)
			clock.Advance(2 * time.Second)
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Logf("operation error: %v", err)
			}
			var state string
			var status *string
			if err := store.pool.QueryRow(ctx, "SELECT state, result->>'status' FROM environment_work WHERE id=$1", work.ID).Scan(&state, &status); err != nil {
				t.Fatal(err)
			}
			if state != "stopped" || status == nil || *status != "timed_out" {
				t.Fatalf("deadline passed under Work row lock: operation=%s state=%s result=%v (want stopped/timed_out)", operation, state, status)
			}
		})
	}
}

func TestEnvironmentHealthcheckPollDeadlineAfterPollerLock(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	clock := &environmentWorkTestClock{now: time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)}
	store.clock = clock
	envs := NewEnvironmentRepository(store)
	if err := envs.Put(ctx, domain.Environment{ID: "env_deadline_poll", Name: "deadline", ConfigType: "self_hosted", Config: map[string]any{"type": "self_hosted"}, CreatedAt: clock.Now(), UpdatedAt: clock.Now()}); err != nil {
		t.Fatal(err)
	}
	repo := NewEnvironmentWorkRepository(store)
	_, err := repo.PollWork(ctx, "env_deadline_poll", app.EnvironmentWorkPollInput{WorkerID: "poller"})
	if err != nil {
		t.Fatal(err)
	}
	work, err := repo.CreateHealthcheck(ctx, "env_deadline_poll")
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(119 * time.Second)
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT worker_id FROM environment_work_pollers WHERE environment_id=$1 FOR UPDATE", work.EnvironmentID); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	var claimed *domain.EnvironmentWork
	go func() {
		var err error
		claimed, err = repo.PollWork(ctx, work.EnvironmentID, app.EnvironmentWorkPollInput{WorkerID: "poller"})
		done <- err
	}()
	waitForCredentialSQLLock(t, ctx, store, "environment_work_pollers", done)
	clock.Advance(2 * time.Second)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if claimed != nil {
		t.Fatalf("expired healthcheck was returned with secret: id=%s", claimed.ID)
	}
	var state, status string
	if err := store.pool.QueryRow(ctx, "SELECT state,result->>'status' FROM environment_work WHERE id=$1", work.ID).Scan(&state, &status); err != nil {
		t.Fatal(err)
	}
	if state != "stopped" || status != "timed_out" {
		t.Fatalf("expired poll result state=%s result=%s", state, status)
	}
}
