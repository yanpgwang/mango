package pg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/workspace"
)

func environmentCredentialFixture(t *testing.T) (*Store, context.Context, *EnvironmentWorkRepository, APIKey, string) {
	t.Helper()
	store := testStore(t)
	ctx := workspace.WithScope(context.Background(), workspace.DefaultID)
	now := store.clock.Now().UTC()
	for _, id := range []string{"env_key", "env_other"} {
		if err := NewEnvironmentRepository(store).Put(ctx, domain.Environment{ID: id, Name: id, ConfigType: "self_hosted", Config: map[string]any{"type": "self_hosted"}, Metadata: map[string]any{}, CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	session := newSession("sesn_env_key")
	session.EnvironmentID = "env_key"
	session.EnvironmentType = "self_hosted"
	if _, err := store.CreateSession(ctx, session, []domain.EventDraft{{Type: domain.EvUserMessage, Payload: map[string]any{"content": "run"}}}); err != nil {
		t.Fatal(err)
	}
	key, secret, err := store.CreateEnvironmentKey(ctx, workspace.DefaultID, "env_key", "supervisor")
	if err != nil {
		t.Fatal(err)
	}
	return store, ctx, NewEnvironmentWorkRepository(store), key, secret
}

func TestEnvironmentKeyLifecycleAndScope(t *testing.T) {
	store, ctx, repo, key, secret := environmentCredentialFixture(t)
	if count, err := store.CountActiveAPIKeys(ctx); err != nil || count != 0 {
		t.Fatalf("scoped key counted as an administrator: count=%d err=%v", count, err)
	}

	if _, err := store.AuthenticateAPIKey(ctx, secret); !errors.Is(err, workspace.ErrInvalidAPIKey) {
		t.Fatalf("scoped key gained Workspace scope: %v", err)
	}
	id, scope, err := store.AuthenticateEnvironmentKey(ctx, secret)
	if err != nil || id != workspace.DefaultID || scope.EnvironmentID != "env_key" || scope.KeyID != key.ID {
		t.Fatalf("wrong scope: %+v, %v", scope, err)
	}
	var stored []byte
	if err := store.pool.QueryRow(ctx, `SELECT secret_hash FROM api_keys WHERE id=$1`, key.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(secret))
	if !bytes.Equal(stored, digest[:]) || bytes.Contains(stored, []byte(secret)) {
		t.Fatal("credential was not hashed")
	}
	listed, err := store.ListAPIKeys(ctx, workspace.DefaultID)
	if err != nil || len(listed) != 1 || listed[0].EnvironmentID != "env_key" {
		t.Fatalf("list=%+v err=%v", listed, err)
	}
	scopedCtx := workspace.WithEnvironmentScope(ctx, id, scope)
	if _, err := repo.PollWork(scopedCtx, "env_other", app.EnvironmentWorkPollInput{}); err == nil {
		t.Fatal("polled another Environment")
	}
	other, err := store.CreateWorkspace(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateEnvironmentKey(ctx, other.ID, "env_key", "wrong workspace"); err == nil {
		t.Fatal("bound key to another Workspace's Environment")
	}
	if _, _, err := store.CreateEnvironmentKey(ctx, workspace.DefaultID, "missing", "missing environment"); err == nil {
		t.Fatal("created key for missing Environment")
	}
	if _, _, err := store.CreateEnvironmentKey(ctx, workspace.DefaultID, "env_key", " "); err == nil {
		t.Fatal("accepted empty label")
	}
	_, replacement, err := store.CreateEnvironmentKey(ctx, workspace.DefaultID, "env_key", "replacement")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AuthenticateEnvironmentKey(ctx, secret); !errors.Is(err, workspace.ErrInvalidEnvironmentKey) {
		t.Fatalf("revoked authentication=%v", err)
	}
	if _, err := repo.PollWork(scopedCtx, "env_key", app.EnvironmentWorkPollInput{}); !errors.Is(err, workspace.ErrInvalidEnvironmentKey) {
		t.Fatalf("stale authenticated poll=%v", err)
	}
	if _, _, err := store.AuthenticateEnvironmentKey(ctx, replacement); err != nil {
		t.Fatalf("replacement=%v", err)
	}
}

func TestEnvironmentKeyRevocationPreservesAcknowledgedWorkLease(t *testing.T) {
	store, ctx, repo, key, secret := environmentCredentialFixture(t)
	id, scope, err := store.AuthenticateEnvironmentKey(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	supervisorCtx := workspace.WithEnvironmentScope(ctx, id, scope)
	work, err := repo.PollWork(supervisorCtx, "env_key", app.EnvironmentWorkPollInput{ReclaimAge: time.Hour})
	if err != nil || work == nil {
		t.Fatalf("poll=%v", err)
	}
	if _, err := repo.AckWork(supervisorCtx, "env_key", work.ID); err != nil {
		t.Fatal(err)
	}
	token := decodeSessionsToken(t, work.Secret)
	if err := store.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.AckWork(supervisorCtx, "env_key", work.ID); !errors.Is(err, workspace.ErrInvalidEnvironmentKey) {
		t.Fatalf("stale authenticated Ack=%v", err)
	}
	id, itemScope, err := store.AuthenticateSessionToken(ctx, token)
	if err != nil {
		t.Fatalf("revocation killed active token: %v", err)
	}
	itemCtx := workspace.WithSessionScope(ctx, id, itemScope)
	heartbeat, err := repo.HeartbeatWork(itemCtx, "env_key", work.ID, nil, nil)
	if err != nil || !heartbeat.LeaseExtended {
		t.Fatalf("active heartbeat=%+v, %v", heartbeat, err)
	}
	if err := repo.StopWork(itemCtx, "env_key", work.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.AuthenticateSessionToken(ctx, token); !errors.Is(err, workspace.ErrInvalidSessionToken) {
		t.Fatalf("stopped token=%v", err)
	}
}

// Hold the revocation row lock before Poll/Ack enters its transaction. Both
// operations must wait, then observe the committed revoke rather than using
// their earlier successful authentication. This exercises a real SQL race.
func TestEnvironmentKeyRevocationFencesConcurrentPollAndAck(t *testing.T) {
	for _, operation := range []string{"poll", "ack"} {
		t.Run(operation, func(t *testing.T) {
			store, ctx, repo, key, secret := environmentCredentialFixture(t)
			id, scope, err := store.AuthenticateEnvironmentKey(ctx, secret)
			if err != nil {
				t.Fatal(err)
			}
			scopedCtx := workspace.WithEnvironmentScope(ctx, id, scope)
			pending, err := repo.PollWork(ctx, "env_key", app.EnvironmentWorkPollInput{})
			if err != nil || pending == nil {
				t.Fatalf("prepare pending: %v", err)
			}
			tx, err := store.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at=now() WHERE id=$1`, key.ID); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				if operation == "poll" {
					_, err = repo.PollWork(scopedCtx, "env_key", app.EnvironmentWorkPollInput{})
				} else {
					_, err = repo.AckWork(scopedCtx, "env_key", pending.ID)
				}
				done <- err
			}()
			deadline := time.Now().Add(5 * time.Second)
			for {
				var waiting bool
				if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=current_setting('application_name') AND wait_event_type='Lock' AND query LIKE '%api_keys%' AND pid<>pg_backend_pid())`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				select {
				case err := <-done:
					t.Fatalf("operation did not wait for revoke: %v", err)
				default:
				}
				if time.Now().After(deadline) {
					t.Fatal("operation did not reach key row lock")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, workspace.ErrInvalidEnvironmentKey) {
					t.Fatalf("revocation race=%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("operation remained blocked")
			}
			got, err := repo.GetWork(ctx, "env_key", pending.ID)
			if err != nil || got.State != domain.EnvironmentWorkQueued {
				t.Fatalf("revoked operation mutated Work: %+v %v", got, err)
			}
			// Rotation does not strand a queued claim: a replacement key can Ack it.
			_, replacement, err := store.CreateEnvironmentKey(ctx, workspace.DefaultID, "env_key", "replacement")
			if err != nil {
				t.Fatal(err)
			}
			id, replacementScope, err := store.AuthenticateEnvironmentKey(ctx, replacement)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repo.AckWork(workspace.WithEnvironmentScope(ctx, id, replacementScope), "env_key", pending.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEnvironmentKeyCreationRequiresSelfHostedEnvironment(t *testing.T) {
	store, ctx, _, _, _ := environmentCredentialFixture(t)
	if _, err := store.pool.Exec(ctx, `UPDATE environments SET config_type='cloud' WHERE id='env_other'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateEnvironmentKey(ctx, workspace.DefaultID, "env_other", "invalid"); err == nil || !strings.Contains(err.Error(), "self-hosted") {
		t.Fatalf("cloud Environment key=%v", err)
	}
}

func TestEnvironmentKeyRevocationInterruptsAuthenticatedLongPoll(t *testing.T) {
	store, ctx, repo, key, secret := environmentCredentialFixture(t)
	page, err := repo.ListWork(ctx, "env_key", app.EnvironmentWorkListQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.StopWork(ctx, "env_key", page.Work[0].ID, true); err != nil {
		t.Fatal(err)
	}
	service := app.NewEnvironmentWorkService(repo, NewEnvironmentRepository(store))
	handler := httpapi.NewServer(httpapi.Deps{EnvironmentWork: service}, httpapi.Config{Authenticator: store}).Handler()
	request := httptest.NewRequest("GET", "/v1/environments/env_key/work/poll?block_ms=999&worker_id=long-poller", nil)
	request.Header.Set("Authorization", "Bearer "+secret)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { handler.ServeHTTP(response, request); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var polled bool
		if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM environment_work_pollers WHERE worker_id='long-poller')`).Scan(&polled); err != nil {
			t.Fatal(err)
		}
		if polled {
			break
		}
		select {
		case <-done:
			t.Fatal("long poll returned before first empty attempt")
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("no initial poll")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := store.RevokeAPIKey(ctx, key.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("revoked long poll did not finish")
	}
	if response.Code != 401 {
		t.Fatalf("revoked long-poll response=%d %s", response.Code, response.Body.String())
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
	if body.Type != "error" || body.Error.Type != "authentication_error" || body.Error.Message != "invalid credential" {
		t.Fatalf("wrong error: %s", response.Body.String())
	}
}

func TestEnvironmentKeyRevokeWaitsForAcknowledgementCommit(t *testing.T) {
	store, ctx, repo, key, secret := environmentCredentialFixture(t)
	id, scope, err := store.AuthenticateEnvironmentKey(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	scopedCtx := workspace.WithEnvironmentScope(ctx, id, scope)
	pending, err := repo.PollWork(scopedCtx, "env_key", app.EnvironmentWorkPollInput{})
	if err != nil || pending == nil {
		t.Fatalf("prepare pending: %v", err)
	}
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT id FROM environment_work WHERE id=$1 FOR UPDATE`, pending.ID); err != nil {
		t.Fatal(err)
	}
	acked := make(chan error, 1)
	go func() { _, err := repo.AckWork(scopedCtx, "env_key", pending.ID); acked <- err }()
	waitForCredentialSQLLock(t, ctx, store, "environment_work", acked)
	revoked := make(chan error, 1)
	go func() { revoked <- store.RevokeAPIKey(ctx, key.ID) }()
	waitForCredentialSQLLock(t, ctx, store, "api_keys", revoked)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, done := range []<-chan error{acked, revoked} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("transaction remained blocked")
		}
	}
	if _, _, err := store.AuthenticateSessionToken(ctx, decodeSessionsToken(t, pending.Secret)); err != nil {
		t.Fatalf("Ack ordered before revoke lost its lease: %v", err)
	}
}

func waitForCredentialSQLLock(t *testing.T, ctx context.Context, store *Store, table string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := store.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE application_name=current_setting('application_name') AND wait_event_type='Lock' AND query LIKE $1 AND pid<>pg_backend_pid())`, "%"+table+"%").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case err := <-done:
			t.Fatalf("operation did not wait for %s lock: %v", table, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation did not reach %s row lock", table)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestEnvironmentKeyPollOrdersLocksBeforeEnvironmentDeletion(t *testing.T) {
	store, ctx, repo, _, _ := environmentCredentialFixture(t)
	_, secret, err := store.CreateEnvironmentKey(ctx, workspace.DefaultID, "env_other", "empty-queue")
	if err != nil {
		t.Fatal(err)
	}
	id, scope, err := store.AuthenticateEnvironmentKey(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	scopedCtx := workspace.WithEnvironmentScope(ctx, id, scope)
	deleting, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = deleting.Rollback(ctx) }()
	if _, err := deleting.Exec(ctx, `SELECT id FROM environments WHERE id='env_other' FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	polled := make(chan error, 1)
	go func() {
		_, err := repo.PollWork(scopedCtx, "env_other", app.EnvironmentWorkPollInput{WorkerID: "new-poller"})
		polled <- err
	}()
	waitForCredentialSQLLock(t, ctx, store, "environment", polled)
	// Poll must be waiting on the parent before taking the child key lock.
	// Otherwise cascade key deletion and the new poller FK form a deadlock.
	if _, err := deleting.Exec(ctx, `DELETE FROM environments WHERE id='env_other'`); err != nil {
		t.Fatalf("Environment deletion deadlocked with new poller: %v", err)
	}
	if err := deleting.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-polled:
		if !errors.Is(err, workspace.ErrInvalidEnvironmentKey) {
			t.Fatalf("deleted Environment poll=%v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poll remained blocked")
	}
	if _, _, err := store.AuthenticateEnvironmentKey(ctx, secret); !errors.Is(err, workspace.ErrInvalidEnvironmentKey) {
		t.Fatalf("deleted Environment kept its key: %v", err)
	}
}
