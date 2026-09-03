package pg

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/yanpgwang/mango/internal/app"
	"github.com/yanpgwang/mango/internal/domain"
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
		claims int
		mu     sync.Mutex
		wg     sync.WaitGroup
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
				mu.Unlock()
			}
		}(worker)
	}
	wg.Wait()
	if claims != 1 {
		t.Fatalf("concurrent claims = %d, want 1", claims)
	}

	workID := listed.Work[0].ID
	acked, err := repository.AckWork(ctx, session.EnvironmentID, workID)
	if err != nil || acked.State != domain.EnvironmentWorkStarting || acked.AcknowledgedAt == nil {
		t.Fatalf("Ack = %+v, err=%v", acked, err)
	}
	ttl := int64(45)
	expected := "NO_HEARTBEAT"
	heartbeat, err := repository.HeartbeatWork(
		ctx, session.EnvironmentID, workID, &expected, &ttl,
	)
	if err != nil || !heartbeat.LeaseExtended ||
		heartbeat.State != domain.EnvironmentWorkActive || heartbeat.TTLSeconds != ttl {
		t.Fatalf("first Heartbeat = %+v, err=%v", heartbeat, err)
	}
	if _, err := repository.HeartbeatWork(
		ctx, session.EnvironmentID, workID, &expected, nil,
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
	if err := repository.StopWork(ctx, session.EnvironmentID, workID, false); err != nil {
		t.Fatalf("graceful Stop: %v", err)
	}
	if successor, err := repository.PollWork(ctx, session.EnvironmentID, app.EnvironmentWorkPollInput{
		WorkerID: "worker-successor", ReclaimAge: time.Hour,
	}); err != nil || successor != nil {
		t.Fatalf("successor escaped before predecessor stopped: work=%+v err=%v", successor, err)
	}
	last := heartbeat.LastHeartbeat.Format(time.RFC3339Nano)
	stopping, err := repository.HeartbeatWork(
		ctx, session.EnvironmentID, workID, &last, nil,
	)
	if err != nil || stopping.LeaseExtended || stopping.State != domain.EnvironmentWorkStopping {
		t.Fatalf("stopping Heartbeat = %+v, err=%v", stopping, err)
	}
	if err := repository.StopWork(ctx, session.EnvironmentID, workID, true); err != nil {
		t.Fatalf("force Stop: %v", err)
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
	acked, err := repository.AckWork(ctx, session.EnvironmentID, first.ID)
	if err != nil || acked.State != domain.EnvironmentWorkStarting {
		t.Fatalf("Ack = %+v, err=%v", acked, err)
	}

	// Model a committed Ack whose HTTP response never reached the worker. The
	// worker must not call Stop because it cannot distinguish that outcome from
	// an uncommitted Ack. Once the starting lease expires, another worker claims
	// the same activation instead of losing it.
	clock.Advance(31 * time.Second)
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
	if _, err := repository.AckWork(ctx, session.EnvironmentID, reclaimed.ID); err != nil {
		t.Fatalf("recovery Ack: %v", err)
	}
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
