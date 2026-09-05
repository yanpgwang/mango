package pg

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
	"github.com/yanpgwang/mango/internal/httpapi"
	"github.com/yanpgwang/mango/internal/workspace"
)

func TestEnvironmentWorkActivatesSelfHostedSessionAndFollowsLeaseLifecycle(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	if err := NewEnvironmentRepository(store).Put(ctx, domain.Environment{
		ID: "env_self_hosted", Name: "Self hosted", ConfigType: "self_hosted",
		Config: map[string]any{"type": "self_hosted"}, Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	session := newSession("sesn_environment_work")
	session.EnvironmentID = "env_self_hosted"
	session.EnvironmentType = "self_hosted"
	if _, err := store.CreateSession(ctx, session, []domain.EventDraft{{
		Type: domain.EvUserMessage, Payload: map[string]any{"content": "run locally"},
	}}); err != nil {
		t.Fatalf("create self-hosted Session: %v", err)
	}

	repository := NewEnvironmentWorkRepository(store)
	listed, err := repository.ListWork(ctx, session.EnvironmentID, app.EnvironmentWorkListQuery{Limit: 10})
	if err != nil || len(listed.Work) != 1 || listed.Work[0].State != domain.EnvironmentWorkQueued {
		t.Fatalf("initial Work = %+v, err=%v", listed, err)
	}

	// Concurrent workers cannot claim the same fresh item.
	var (
		claims      int
		claimSecret string
		mu          sync.Mutex
		wg          sync.WaitGroup
	)
	for _, worker := range []string{"worker-a", "worker-b"} {
		wg.Add(1)
		go func(workerID string) {
			defer wg.Done()
			work, pollErr := repository.PollWork(ctx, session.EnvironmentID, app.EnvironmentWorkPollInput{
				WorkerID: workerID, ReclaimAge: time.Hour,
			})
			if pollErr != nil {
				t.Errorf("poll %s: %v", workerID, pollErr)
				return
			}
			if work != nil {
				mu.Lock()
				claims++
				claimSecret = work.Secret
				mu.Unlock()
			}
		}(worker)
	}
	wg.Wait()
	if claims != 1 {
		t.Fatalf("concurrent claims = %d, want 1", claims)
	}
	if claimSecret == "" {
		t.Fatal("Poll did not return a Work secret")
	}
	sessionsToken := decodeSessionsToken(t, claimSecret)
	if _, _, err := store.AuthenticateSessionToken(ctx, sessionsToken); !errors.Is(
		err, workspace.ErrInvalidSessionToken,
	) {
		t.Fatalf("pre-Ack token authentication error = %T %v", err, err)
	}
	stored, err := repository.GetWork(ctx, session.EnvironmentID, listed.Work[0].ID)
	if err != nil || stored.Secret != "" {
		t.Fatalf("Get Work exposed claim secret: work=%+v err=%v", stored, err)
	}
	var storedHash []byte
	if err := store.pool.QueryRow(ctx, `SELECT sessions_token_hash FROM environment_work WHERE id = $1`,
		listed.Work[0].ID).Scan(&storedHash); err != nil {
		t.Fatalf("read stored Work secret digest: %v", err)
	}
	if !bytes.Equal(storedHash, hashSessionsToken(sessionsToken)) ||
		bytes.Contains(storedHash, []byte(sessionsToken)) {
		t.Fatalf("stored Work secret digest is invalid")
	}

	workID := listed.Work[0].ID
	acked, err := repository.AckWork(ctx, session.EnvironmentID, workID)
	if err != nil || acked.State != domain.EnvironmentWorkStarting || acked.AcknowledgedAt == nil {
		t.Fatalf("Ack = %+v, err=%v", acked, err)
	}
	if acked.Secret != "" {
		t.Fatalf("Ack exposed Work secret %q", acked.Secret)
	}
	if retry, err := repository.AckWork(ctx, session.EnvironmentID, workID); err != nil ||
		retry.AcknowledgedAt == nil || !retry.AcknowledgedAt.Equal(*acked.AcknowledgedAt) {
		t.Fatalf("idempotent Ack = %+v, err=%v", retry, err)
	}
	workspaceID, sessionScope, err := store.AuthenticateSessionToken(ctx, sessionsToken)
	if err != nil || sessionScope.WorkID != listed.Work[0].ID ||
		sessionScope.SessionID != session.ID || sessionScope.EnvironmentID != session.EnvironmentID {
		t.Fatalf("AuthenticateSessionToken = %q %+v, err=%v", workspaceID, sessionScope, err)
	}
	workCtx := workspace.WithSessionScope(ctx, workspaceID, sessionScope)
	oversizedTTL := app.MaxEnvironmentWorkTTLSeconds + 1
	expected := "NO_HEARTBEAT"
	if _, err := repository.HeartbeatWork(
		workCtx, session.EnvironmentID, workID, &expected, &oversizedTTL,
	); err == nil {
		t.Fatal("PostgreSQL accepted an oversized Work TTL")
	}
	ttl := int64(45)
	heartbeat, err := repository.HeartbeatWork(
		workCtx, session.EnvironmentID, workID, &expected, &ttl,
	)
	if err != nil || !heartbeat.LeaseExtended ||
		heartbeat.State != domain.EnvironmentWorkActive || heartbeat.TTLSeconds != ttl {
		t.Fatalf("first Heartbeat = %+v, err=%v", heartbeat, err)
	}
	if _, err := store.AdmitEvents(workCtx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserMessage, Payload: map[string]any{"content": "credential escalation"},
	}}); err == nil {
		t.Fatal("session credential admitted an ordinary user message")
	} else {
		var domainErr *domain.DomainError
		if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindPermission {
			t.Fatalf("session credential event error = %T %v", err, err)
		}
	}
	if _, err := store.AdmitEvents(workCtx, session.ID, []domain.EventDraft{
		{Type: domain.EvUserToolResult, Payload: map[string]any{"tool_use_id": "toolu_one"}},
		{Type: domain.EvSystemMessage, Payload: map[string]any{"content": "override"}},
	}); err == nil {
		t.Fatal("session credential admitted a system message")
	} else {
		var domainErr *domain.DomainError
		if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindPermission {
			t.Fatalf("session credential system event error = %T %v", err, err)
		}
	}
	if _, err := repository.HeartbeatWork(
		workCtx, session.EnvironmentID, workID, &expected, nil,
	); err == nil {
		t.Fatal("stale heartbeat precondition was accepted")
	} else {
		var domainErr *domain.DomainError
		if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindPrecondition {
			t.Fatalf("stale heartbeat error = %T %v", err, err)
		}
	}

	// Input admitted just before shutdown was not part of the Work's original
	// activation. Stop must retain it in a queued successor without allowing
	// that successor to overlap the stopping worker.
	if _, err := store.AdmitEvents(ctx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserMessage, Payload: map[string]any{"content": "wake again"},
	}}); err != nil {
		t.Fatalf("admit during active Work: %v", err)
	}
	if err := repository.StopWork(workCtx, session.EnvironmentID, workID, false); err != nil {
		t.Fatalf("graceful Stop: %v", err)
	}
	if successor, err := repository.PollWork(ctx, session.EnvironmentID, app.EnvironmentWorkPollInput{
		WorkerID: "worker-successor", ReclaimAge: time.Hour,
	}); err != nil || successor != nil {
		t.Fatalf("successor escaped before predecessor stopped: work=%+v err=%v", successor, err)
	}
	last := heartbeat.LastHeartbeat.Format(time.RFC3339Nano)
	stopping, err := repository.HeartbeatWork(
		workCtx, session.EnvironmentID, workID, &last, nil,
	)
	if err != nil || stopping.LeaseExtended || stopping.State != domain.EnvironmentWorkStopping {
		t.Fatalf("stopping Heartbeat = %+v, err=%v", stopping, err)
	}
	if err := repository.StopWork(workCtx, session.EnvironmentID, workID, true); err != nil {
		t.Fatalf("force Stop: %v", err)
	}
	if _, _, err := store.AuthenticateSessionToken(ctx, sessionsToken); !errors.Is(err, workspace.ErrInvalidSessionToken) {
		t.Fatalf("stopped token authentication error = %T %v", err, err)
	}
	successor, err := repository.PollWork(ctx, session.EnvironmentID, app.EnvironmentWorkPollInput{
		WorkerID: "worker-successor", ReclaimAge: time.Hour,
	})
	if err != nil || successor == nil || successor.ID == workID {
		t.Fatalf("successor after predecessor Stop = %+v, err=%v", successor, err)
	}

	// The predecessor remains as control-plane history beside the successor.
	listed, err = repository.ListWork(ctx, session.EnvironmentID, app.EnvironmentWorkListQuery{Limit: 10})
	if err != nil || len(listed.Work) != 2 || listed.Work[0].State != domain.EnvironmentWorkQueued ||
		listed.Work[0].ID == workID {
		t.Fatalf("Work after reactivation = %+v, err=%v", listed, err)
	}
}

func TestEnvironmentWorkForceStopDoesNotRequeueOriginalActivation(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	if err := NewEnvironmentRepository(store).Put(ctx, domain.Environment{
		ID: "env_stop_discard", Name: "Self hosted", ConfigType: "self_hosted",
		Config: map[string]any{"type": "self_hosted"}, Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	session := newSession("sesn_stop_discard")
	session.EnvironmentID = "env_stop_discard"
	session.EnvironmentType = "self_hosted"
	if _, err := store.CreateSession(ctx, session, []domain.EventDraft{{
		Type: domain.EvUserMessage, Payload: map[string]any{"content": "bad activation"},
	}}); err != nil {
		t.Fatal(err)
	}
	repository := NewEnvironmentWorkRepository(store)
	work, err := repository.PollWork(ctx, session.EnvironmentID, app.EnvironmentWorkPollInput{
		WorkerID: "discarding-worker", ReclaimAge: time.Hour,
	})
	if err != nil || work == nil {
		t.Fatalf("Poll = %+v, err=%v", work, err)
	}
	if err := repository.StopWork(ctx, session.EnvironmentID, work.ID, true); err != nil {
		t.Fatalf("force Stop: %v", err)
	}
	page, err := repository.ListWork(ctx, session.EnvironmentID, app.EnvironmentWorkListQuery{Limit: 10})
	if err != nil || len(page.Work) != 1 || page.Work[0].State != domain.EnvironmentWorkStopped {
		t.Fatalf("Work after discard = %+v, err=%v", page, err)
	}
	if requeued, err := repository.PollWork(ctx, session.EnvironmentID, app.EnvironmentWorkPollInput{
		WorkerID: "next-worker", ReclaimAge: time.Hour,
	}); err != nil || requeued != nil {
		t.Fatalf("discarded activation requeued: work=%+v err=%v", requeued, err)
	}
}

func TestEnvironmentWorkFailureDurablyTerminatesSession(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	if err := NewEnvironmentRepository(store).Put(ctx, domain.Environment{
		ID: "env_input_failure", Name: "Self hosted", ConfigType: "self_hosted",
		Config: map[string]any{"type": "self_hosted"}, Metadata: map[string]any{},
		CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	session := newSession("sesn_input_failure")
	session.EnvironmentID = "env_input_failure"
	session.EnvironmentType = "self_hosted"
	if _, err := store.CreateSession(ctx, session, []domain.EventDraft{{
		Type: domain.EvUserMessage, Payload: map[string]any{"content": "run"},
	}}); err != nil {
		t.Fatal(err)
	}
	repository := NewEnvironmentWorkRepository(store)
	work, err := repository.PollWork(ctx, session.EnvironmentID, app.EnvironmentWorkPollInput{
		WorkerID: "worker-bad-input", ReclaimAge: time.Hour,
	})
	if err != nil || work == nil {
		t.Fatalf("Poll = %+v, err=%v", work, err)
	}
	if _, err := repository.AckWork(ctx, session.EnvironmentID, work.ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	token := decodeSessionsToken(t, work.Secret)
	workspaceID, scope, err := store.AuthenticateSessionToken(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	workCtx := workspace.WithSessionScope(ctx, workspaceID, scope)
	expected := "NO_HEARTBEAT"
	if _, err := repository.HeartbeatWork(
		workCtx, session.EnvironmentID, work.ID, &expected, nil,
	); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if err := repository.FailWork(
		workCtx, session.EnvironmentID, work.ID, "stored Skill archive checksum mismatch",
	); err != nil {
		t.Fatalf("Fail Work: %v", err)
	}

	failed, err := store.GetSession(ctx, session.ID)
	if err != nil || failed.Status != domain.StatusTerminated || failed.TerminatedAt == nil {
		t.Fatalf("failed Session = %+v, err=%v", failed, err)
	}
	storedWork, err := repository.GetWork(ctx, session.EnvironmentID, work.ID)
	if err != nil || storedWork.State != domain.EnvironmentWorkStopped || storedWork.StoppedAt == nil {
		t.Fatalf("failed Work = %+v, err=%v", storedWork, err)
	}
	if _, _, err := store.AuthenticateSessionToken(ctx, token); !errors.Is(err, workspace.ErrInvalidSessionToken) {
		t.Fatalf("failed Work token authentication error = %T %v", err, err)
	}
	events, err := store.EventsAfter(ctx, session.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) < 3 || events[len(events)-2].Type != domain.EvSessionError ||
		events[len(events)-1].Type != domain.EvSessionStatusTerminated {
		t.Fatalf("terminal events = %+v", events)
	}
	errorPayload, _ := events[len(events)-2].Payload["error"].(map[string]any)
	if errorPayload["type"] != "session_input_failed_error" ||
		errorPayload["message"] != "stored Skill archive checksum mismatch" {
		t.Fatalf("Session failure payload = %#v", errorPayload)
	}
	if wakeup, ok, err := store.PendingWakeup(ctx, session.ID); err != nil || !ok ||
		wakeup.MaxEventSeq != events[len(events)-1].Sequence {
		t.Fatalf("terminal wakeup = %+v, exists=%t, err=%v", wakeup, ok, err)
	}
	if next, err := repository.PollWork(ctx, session.EnvironmentID, app.EnvironmentWorkPollInput{
		WorkerID: "worker-next", ReclaimAge: 0,
	}); err != nil || next != nil {
		t.Fatalf("terminal Session requeued Work = %+v, err=%v", next, err)
	}
}

func TestEnvironmentWorkAmbiguousAckIsReclaimedAfterStartingTTL(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	clock := &environmentWorkTestClock{now: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)}
	store.clock = clock
	if err := NewEnvironmentRepository(store).Put(ctx, domain.Environment{
		ID: "env_ack_reclaim", Name: "Self hosted", ConfigType: "self_hosted",
		Config: map[string]any{"type": "self_hosted"}, Metadata: map[string]any{},
		CreatedAt: clock.Now(), UpdatedAt: clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	session := newSession("sesn_ack_reclaim")
	session.EnvironmentID = "env_ack_reclaim"
	session.EnvironmentType = "self_hosted"
	if _, err := store.CreateSession(ctx, session, []domain.EventDraft{{
		Type: domain.EvUserMessage, Payload: map[string]any{"content": "survive an ambiguous Ack"},
	}}); err != nil {
		t.Fatal(err)
	}

	repository := NewEnvironmentWorkRepository(store)
	first, err := repository.PollWork(ctx, session.EnvironmentID, app.EnvironmentWorkPollInput{
		WorkerID: "worker-lost-response", ReclaimAge: time.Hour,
	})
	if err != nil || first == nil {
		t.Fatalf("first Poll = %+v, err=%v", first, err)
	}
	firstToken := decodeSessionsToken(t, first.Secret)
	acked, err := repository.AckWork(ctx, session.EnvironmentID, first.ID)
	if err != nil || acked.State != domain.EnvironmentWorkStarting {
		t.Fatalf("Ack = %+v, err=%v", acked, err)
	}
	workspaceID, firstScope, err := store.AuthenticateSessionToken(ctx, firstToken)
	if err != nil {
		t.Fatalf("authenticate first claim: %v", err)
	}
	firstCtx := workspace.WithSessionScope(ctx, workspaceID, firstScope)

	// Model a committed Ack whose HTTP response never reached the worker. The
	// worker must not call Stop because it cannot distinguish that outcome from
	// an uncommitted Ack. Once the starting lease expires, another worker claims
	// the same activation instead of losing it.
	clock.Advance(31 * time.Second)
	if _, err := repository.HeartbeatWork(
		firstCtx, session.EnvironmentID, first.ID, nil, nil,
	); err == nil {
		t.Fatal("expired starting lease was revived before reclaim")
	} else {
		var domainErr *domain.DomainError
		if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindPrecondition {
			t.Fatalf("expired pre-reclaim heartbeat error = %T %v", err, err)
		}
	}
	if err := repository.StopWork(firstCtx, session.EnvironmentID, first.ID, true); err == nil {
		t.Fatal("expired starting lease was stopped before reclaim")
	} else {
		var domainErr *domain.DomainError
		if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindPrecondition {
			t.Fatalf("expired pre-reclaim Stop error = %T %v", err, err)
		}
	}
	reclaimed, err := repository.PollWork(ctx, session.EnvironmentID, app.EnvironmentWorkPollInput{
		WorkerID: "worker-recovery", ReclaimAge: time.Hour,
	})
	if err != nil || reclaimed == nil {
		t.Fatalf("recovery Poll = %+v, err=%v", reclaimed, err)
	}
	if reclaimed.ID != first.ID || reclaimed.State != domain.EnvironmentWorkQueued ||
		reclaimed.AcknowledgedAt != nil {
		t.Fatalf("reclaimed Work = %+v, want reset activation %s", reclaimed, first.ID)
	}
	if reclaimed.Secret == "" || reclaimed.Secret == first.Secret {
		t.Fatalf("reclaimed Work secret was not rotated: first=%q reclaimed=%q", first.Secret, reclaimed.Secret)
	}
	if _, _, err := store.AuthenticateSessionToken(ctx, firstToken); !errors.Is(err, workspace.ErrInvalidSessionToken) {
		t.Fatalf("expired token authentication error = %T %v", err, err)
	}
	if _, err := repository.HeartbeatWork(
		firstCtx, session.EnvironmentID, first.ID, nil, nil,
	); err == nil {
		t.Fatal("expired worker heartbeat mutated a reclaimed lease")
	} else {
		var domainErr *domain.DomainError
		if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindPrecondition {
			t.Fatalf("expired heartbeat error = %T %v", err, err)
		}
	}
	if err := repository.StopWork(firstCtx, session.EnvironmentID, first.ID, true); err == nil {
		t.Fatal("expired worker stopped a reclaimed lease")
	} else {
		var domainErr *domain.DomainError
		if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindPrecondition {
			t.Fatalf("expired Stop error = %T %v", err, err)
		}
	}
	if _, err := store.AdmitEvents(firstCtx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserToolResult, Payload: map[string]any{"tool_use_id": "toolu_stale"},
	}}); err == nil {
		t.Fatal("expired worker admitted a tool result after reclaim")
	} else {
		var domainErr *domain.DomainError
		if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindPrecondition {
			t.Fatalf("expired tool-result error = %T %v", err, err)
		}
	}
	if _, err := repository.AckWork(ctx, session.EnvironmentID, reclaimed.ID); err != nil {
		t.Fatalf("recovery Ack: %v", err)
	}
	reclaimedToken := decodeSessionsToken(t, reclaimed.Secret)
	workspaceID, reclaimedScope, err := store.AuthenticateSessionToken(ctx, reclaimedToken)
	if err != nil {
		t.Fatalf("authenticate reclaimed claim: %v", err)
	}
	reclaimedCtx := workspace.WithSessionScope(ctx, workspaceID, reclaimedScope)
	expected := "NO_HEARTBEAT"
	heartbeat, err := repository.HeartbeatWork(
		reclaimedCtx, session.EnvironmentID, reclaimed.ID, &expected, nil,
	)
	if err != nil || !heartbeat.LeaseExtended {
		t.Fatalf("recovery Heartbeat = %+v, err=%v", heartbeat, err)
	}
	clock.Advance(31 * time.Second)
	last := heartbeat.LastHeartbeat.Format(time.RFC3339Nano)
	if _, err := repository.HeartbeatWork(
		reclaimedCtx, session.EnvironmentID, reclaimed.ID, &last, nil,
	); err == nil {
		t.Fatal("expired active lease was revived before reclaim")
	} else {
		var domainErr *domain.DomainError
		if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindPrecondition {
			t.Fatalf("expired active heartbeat error = %T %v", err, err)
		}
	}
}

func TestEnvironmentWorkStoppingCredentialExpiresWithoutAnotherPoller(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	clock := &environmentWorkTestClock{now: time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC)}
	store.clock = clock
	if err := NewEnvironmentRepository(store).Put(ctx, domain.Environment{
		ID: "env_stopping_expiry", Name: "Self hosted", ConfigType: "self_hosted",
		Config: map[string]any{"type": "self_hosted"}, Metadata: map[string]any{},
		CreatedAt: clock.Now(), UpdatedAt: clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	session := newSession("sesn_stopping_expiry")
	session.EnvironmentID = "env_stopping_expiry"
	session.EnvironmentType = "self_hosted"
	if _, err := store.CreateSession(ctx, session, []domain.EventDraft{{
		Type: domain.EvUserMessage, Payload: map[string]any{"content": "stop cleanly"},
	}}); err != nil {
		t.Fatal(err)
	}

	repository := NewEnvironmentWorkRepository(store)
	work, err := repository.PollWork(ctx, session.EnvironmentID, app.EnvironmentWorkPollInput{
		WorkerID: "worker-stopping", ReclaimAge: time.Hour,
	})
	if err != nil || work == nil {
		t.Fatalf("Poll = %+v, err=%v", work, err)
	}
	if _, err := repository.AckWork(ctx, session.EnvironmentID, work.ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	token := decodeSessionsToken(t, work.Secret)
	workspaceID, sessionScope, err := store.AuthenticateSessionToken(ctx, token)
	if err != nil {
		t.Fatalf("authenticate acknowledged Work: %v", err)
	}
	workCtx := workspace.WithSessionScope(ctx, workspaceID, sessionScope)
	expected := "NO_HEARTBEAT"
	if _, err := repository.HeartbeatWork(
		workCtx, session.EnvironmentID, work.ID, &expected, nil,
	); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if err := repository.StopWork(workCtx, session.EnvironmentID, work.ID, false); err != nil {
		t.Fatalf("graceful Stop: %v", err)
	}
	if err := store.ValidateSessionScope(ctx, sessionScope); err != nil {
		t.Fatalf("fresh stopping scope: %v", err)
	}

	clock.Advance(31 * time.Second)
	if _, _, err := store.AuthenticateSessionToken(ctx, token); !errors.Is(
		err, workspace.ErrInvalidSessionToken,
	) {
		t.Fatalf("expired stopping authentication error = %T %v", err, err)
	}
	if err := store.ValidateSessionScope(ctx, sessionScope); err == nil {
		t.Fatal("expired stopping scope remained valid")
	}
	if _, err := store.AdmitEvents(workCtx, session.ID, []domain.EventDraft{{
		Type: domain.EvUserToolResult, Payload: map[string]any{"tool_use_id": "toolu_late"},
	}}); err == nil {
		t.Fatal("expired stopping scope admitted a tool result")
	}
	if err := repository.StopWork(workCtx, session.EnvironmentID, work.ID, true); err == nil {
		t.Fatal("expired stopping scope force-stopped Work")
	}
	if _, err := repository.PollWork(ctx, session.EnvironmentID, app.EnvironmentWorkPollInput{
		WorkerID: "worker-cleanup", ReclaimAge: time.Hour,
	}); err != nil {
		t.Fatalf("cleanup Poll: %v", err)
	}
	stopped, err := repository.GetWork(ctx, session.EnvironmentID, work.ID)
	if err != nil || stopped.State != domain.EnvironmentWorkStopped {
		t.Fatalf("Work after cleanup Poll = %+v, err=%v", stopped, err)
	}
}

func TestEnvironmentWorkSessionStreamClosesAfterLeaseEnds(t *testing.T) {
	for _, test := range []struct {
		name string
		slug string
		end  func(context.Context, *Store, *EnvironmentWorkRepository, *environmentWorkTestClock, domain.EnvironmentWork, context.Context) error
	}{
		{
			name: "force stop",
			slug: "force_stop",
			end: func(
				_ context.Context, _ *Store, repository *EnvironmentWorkRepository,
				_ *environmentWorkTestClock, work domain.EnvironmentWork, workCtx context.Context,
			) error {
				return repository.StopWork(workCtx, work.EnvironmentID, work.ID, true)
			},
		},
		{
			name: "reclaim",
			slug: "reclaim",
			end: func(
				ctx context.Context, _ *Store, repository *EnvironmentWorkRepository,
				clock *environmentWorkTestClock, work domain.EnvironmentWork, _ context.Context,
			) error {
				clock.Advance(31 * time.Second)
				_, err := repository.PollWork(ctx, work.EnvironmentID, app.EnvironmentWorkPollInput{
					WorkerID: "replacement-worker", ReclaimAge: time.Hour,
				})
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			clock := &environmentWorkTestClock{now: time.Date(2026, 9, 3, 15, 0, 0, 0, time.UTC)}
			store.clock = clock
			environmentID := "env_stream_" + test.slug
			if err := NewEnvironmentRepository(store).Put(ctx, domain.Environment{
				ID: environmentID, Name: "Self hosted", ConfigType: "self_hosted",
				Config: map[string]any{"type": "self_hosted"}, Metadata: map[string]any{},
				CreatedAt: clock.Now(), UpdatedAt: clock.Now(),
			}); err != nil {
				t.Fatal(err)
			}
			session := newSession("sesn_stream_" + test.slug)
			session.EnvironmentID = environmentID
			session.EnvironmentType = "self_hosted"
			if _, err := store.CreateSession(ctx, session, []domain.EventDraft{{
				Type: domain.EvUserMessage, Payload: map[string]any{"content": "stream"},
			}}); err != nil {
				t.Fatal(err)
			}
			repository := NewEnvironmentWorkRepository(store)
			work, err := repository.PollWork(ctx, environmentID, app.EnvironmentWorkPollInput{
				WorkerID: "stream-worker", ReclaimAge: time.Hour,
			})
			if err != nil || work == nil {
				t.Fatalf("Poll = %+v, err=%v", work, err)
			}
			if _, err := repository.AckWork(ctx, environmentID, work.ID); err != nil {
				t.Fatalf("Ack: %v", err)
			}
			token := decodeSessionsToken(t, work.Secret)
			workspaceID, scope, err := store.AuthenticateSessionToken(ctx, token)
			if err != nil {
				t.Fatalf("authenticate: %v", err)
			}
			workCtx := workspace.WithSessionScope(ctx, workspaceID, scope)
			expected := "NO_HEARTBEAT"
			if _, err := repository.HeartbeatWork(
				workCtx, environmentID, work.ID, &expected, nil,
			); err != nil {
				t.Fatalf("Heartbeat: %v", err)
			}

			hub := app.NewHub(8)
			server := httptest.NewServer(httpapi.NewServer(httpapi.Deps{
				Sessions: environmentWorkSessionService{session: session}, Stream: hub,
			}, httpapi.Config{RequireAuth: true, Authenticator: store}).Handler())
			t.Cleanup(server.Close)
			request, err := http.NewRequest(http.MethodGet,
				server.URL+"/v1/sessions/"+session.ID+"/events/stream", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("authorization", "Bearer "+token)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != http.StatusOK {
				t.Fatalf("stream status = %d", response.StatusCode)
			}

			if err := test.end(ctx, store, repository, clock, *work, workCtx); err != nil {
				t.Fatalf("end lease: %v", err)
			}
			closed := make(chan error, 1)
			go func() {
				buffer := make([]byte, 1)
				_, readErr := response.Body.Read(buffer)
				closed <- readErr
			}()
			select {
			case readErr := <-closed:
				if readErr == nil {
					t.Fatal("ended lease stream returned data instead of closing")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("ended lease stream remained open")
			}
		})
	}
}

func TestSessionCredentialAdmissionUsesSessionFirstLockOrder(t *testing.T) {
	store := testStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clock := &environmentWorkTestClock{now: time.Date(2026, 9, 3, 16, 0, 0, 0, time.UTC)}
	store.clock = clock
	environmentID := "env_admission_lock_order"
	if err := NewEnvironmentRepository(store).Put(ctx, domain.Environment{
		ID: environmentID, Name: "Self hosted", ConfigType: "self_hosted",
		Config: map[string]any{"type": "self_hosted"}, Metadata: map[string]any{},
		CreatedAt: clock.Now(), UpdatedAt: clock.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	session := newSession("sesn_admission_lock_order")
	session.EnvironmentID = environmentID
	session.EnvironmentType = "self_hosted"
	if _, err := store.CreateSession(ctx, session, []domain.EventDraft{{
		Type: domain.EvUserMessage, Payload: map[string]any{"content": "lock order"},
	}}); err != nil {
		t.Fatal(err)
	}
	repository := NewEnvironmentWorkRepository(store)
	work, err := repository.PollWork(ctx, environmentID, app.EnvironmentWorkPollInput{
		WorkerID: "lock-order-worker", ReclaimAge: time.Hour,
	})
	if err != nil || work == nil {
		t.Fatalf("Poll = %+v, err=%v", work, err)
	}
	if _, err := repository.AckWork(ctx, environmentID, work.ID); err != nil {
		t.Fatalf("Ack: %v", err)
	}
	token := decodeSessionsToken(t, work.Secret)
	workspaceID, scope, err := store.AuthenticateSessionToken(ctx, token)
	if err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	workCtx := workspace.WithSessionScope(ctx, workspaceID, scope)
	expected := "NO_HEARTBEAT"
	if _, err := repository.HeartbeatWork(
		workCtx, environmentID, work.ID, &expected, nil,
	); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	// Hold the same Session root lock that FinalizeSessionDeletion takes, then
	// start scoped admission. Admission must block before it acquires the Work
	// lease lock, leaving the deleting transaction free to cascade into Work.
	tx, err := store.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var lockedID string
	if err := tx.QueryRow(ctx, `SELECT id FROM sessions WHERE id = $1 FOR UPDATE`,
		session.ID).Scan(&lockedID); err != nil {
		t.Fatalf("lock Session: %v", err)
	}
	admitted := make(chan error, 1)
	go func() {
		_, admissionErr := store.AdmitEvents(workCtx, session.ID, []domain.EventDraft{{
			Type: domain.EvUserToolResult, Payload: map[string]any{"tool_use_id": "toolu_lock"},
		}})
		admitted <- admissionErr
	}()

	waiting := false
	for !waiting {
		if err := store.pool.QueryRow(ctx, `
SELECT EXISTS (
    SELECT 1
    FROM pg_stat_activity
    WHERE query LIKE '-- name: LockSession :one%'
      AND wait_event_type = 'Lock'
)`).Scan(&waiting); err != nil {
			t.Fatalf("inspect admission lock wait: %v", err)
		}
		if waiting {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("scoped admission did not wait on the Session lock")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, session.ID); err != nil {
		t.Fatalf("cascade delete while admission waits: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit cascade delete: %v", err)
	}
	select {
	case err := <-admitted:
		var domainErr *domain.DomainError
		if !errors.As(err, &domainErr) || domainErr.Kind != domain.KindNotFound {
			t.Fatalf("admission after concurrent delete = %T %v, want not found", err, err)
		}
	case <-ctx.Done():
		t.Fatal("scoped admission remained blocked after deletion committed")
	}
}

type environmentWorkSessionService struct{ session domain.Session }

func (s environmentWorkSessionService) Create(
	context.Context, app.CreateSessionInput,
) (domain.Session, error) {
	return s.session, nil
}

func (s environmentWorkSessionService) Get(_ context.Context, id string) (domain.Session, error) {
	if id != s.session.ID {
		return domain.Session{}, domain.NotFound("session not found")
	}
	return s.session, nil
}

func (environmentWorkSessionService) List(
	context.Context, app.ListPage,
) (app.SessionListPage, error) {
	return app.SessionListPage{}, nil
}

func (environmentWorkSessionService) SendEvent(
	context.Context, string, []domain.EventDraft,
) ([]domain.Event, error) {
	return nil, nil
}

func (s environmentWorkSessionService) Update(
	context.Context, string, domain.SessionUpdate,
) (domain.Session, error) {
	return s.session, nil
}

func (s environmentWorkSessionService) Archive(context.Context, string) (domain.Session, error) {
	return s.session, nil
}

func (environmentWorkSessionService) Delete(context.Context, string) error { return nil }

func decodeSessionsToken(t *testing.T, secret string) string {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil {
		t.Fatalf("decode Work secret: %v", err)
	}
	var payload struct {
		SessionsToken string `json:"sessions_token"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.SessionsToken == "" {
		t.Fatalf("parse Work secret = %+v, err=%v", payload, err)
	}
	return payload.SessionsToken
}

type environmentWorkTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *environmentWorkTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *environmentWorkTestClock) Advance(duration time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(duration)
}
