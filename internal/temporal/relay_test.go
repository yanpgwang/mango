package temporal

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/yanpgwang/mango/internal/pg"
)

// fakeOutbox is an in-memory OutboxStore for relay crash-boundary tests. It
// models a single coalescible wakeup per session, exactly like the PostgreSQL
// table's primary-key + GREATEST semantics.
type fakeOutbox struct {
	mu       sync.Mutex
	rows     map[string]*pg.OutboxWakeup
	attempts map[string]int
}

func newFakeOutbox() *fakeOutbox {
	return &fakeOutbox{rows: map[string]*pg.OutboxWakeup{}, attempts: map[string]int{}}
}

// enqueue models an admission coalescing into the outbox.
func (f *fakeOutbox) enqueue(sessionID string, maxSeq int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if row, ok := f.rows[sessionID]; ok {
		if maxSeq > row.MaxEventSeq {
			row.MaxEventSeq = maxSeq
		}
		return
	}
	f.rows[sessionID] = &pg.OutboxWakeup{SessionID: sessionID, MaxEventSeq: maxSeq}
}

func (f *fakeOutbox) ListWakeupsForDelivery(_ context.Context, limit int) ([]pg.OutboxWakeup, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]pg.OutboxWakeup, 0, len(f.rows))
	for _, row := range f.rows {
		out = append(out, *row)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (f *fakeOutbox) DeleteWakeupIfUnchanged(_ context.Context, sessionID string, maxSeq int64) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[sessionID]
	if !ok || row.MaxEventSeq != maxSeq {
		return false, nil
	}
	delete(f.rows, sessionID)
	return true, nil
}

func (f *fakeOutbox) RecordAttempt(_ context.Context, sessionID string, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts[sessionID]++
	return nil
}

func (f *fakeOutbox) pending(sessionID string) (pg.OutboxWakeup, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row, ok := f.rows[sessionID]
	if !ok {
		return pg.OutboxWakeup{}, false
	}
	return *row, true
}

// fakeDeliverer records deliveries and can be made to fail (Worker unavailable).
type fakeDeliverer struct {
	mu        sync.Mutex
	delivered []WakeupSignal // in call order, tagged by session below
	sessions  []string
	failUntil int // fail this many calls before succeeding
	calls     int
}

type fakeThreadOutbox struct {
	*fakeOutbox
	thread pg.OutboxWakeup
}

func (f *fakeThreadOutbox) ListWakeupsForDelivery(
	_ context.Context,
	_ int,
) ([]pg.OutboxWakeup, error) {
	if f.thread.ThreadID == "" {
		return nil, nil
	}
	return []pg.OutboxWakeup{f.thread}, nil
}

func (f *fakeThreadOutbox) DeleteThreadWakeupIfUnchanged(
	_ context.Context,
	sessionID string,
	threadID string,
	maxSeq int64,
	intent pg.OrchestrationIntent,
) (bool, error) {
	if f.thread.SessionID != sessionID || f.thread.ThreadID != threadID ||
		f.thread.MaxEventSeq != maxSeq || f.thread.Intent != intent {
		return false, nil
	}
	f.thread = pg.OutboxWakeup{}
	return true, nil
}

func (f *fakeThreadOutbox) RecordThreadAttempt(
	_ context.Context,
	sessionID string,
	threadID string,
	_ string,
) error {
	f.attempts[sessionID+"/"+threadID]++
	return nil
}

type fakeThreadDeliverer struct {
	fakeDeliverer
	threadIDs     []string
	terminatedIDs []string
}

func (d *fakeThreadDeliverer) WakeThread(
	_ context.Context,
	sessionID string,
	threadID string,
	maxSeq int64,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.calls <= d.failUntil {
		return errors.New("temporal unavailable")
	}
	d.delivered = append(d.delivered, WakeupSignal{MaxEventSeq: maxSeq})
	d.sessions = append(d.sessions, sessionID)
	d.threadIDs = append(d.threadIDs, threadID)
	return nil
}

func (d *fakeThreadDeliverer) TerminateThread(
	_ context.Context,
	sessionID string,
	threadID string,
) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.calls <= d.failUntil {
		return errors.New("temporal unavailable")
	}
	d.sessions = append(d.sessions, sessionID)
	d.terminatedIDs = append(d.terminatedIDs, threadID)
	return nil
}

func (d *fakeDeliverer) Wake(_ context.Context, sessionID string, maxSeq int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if d.calls <= d.failUntil {
		return errors.New("temporal unavailable")
	}
	d.delivered = append(d.delivered, WakeupSignal{MaxEventSeq: maxSeq})
	d.sessions = append(d.sessions, sessionID)
	return nil
}

func (d *fakeDeliverer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.delivered)
}

// TestRelay_DeliversAndRemoves proves the happy path: a claimed wakeup is
// signaled once and then removed from the outbox.
func TestRelay_DeliversAndRemoves(t *testing.T) {
	outbox := newFakeOutbox()
	deliverer := &fakeDeliverer{}
	relay := NewRelay(outbox, deliverer, RelayConfig{})

	outbox.enqueue("sess_a", 5)
	removed, err := relay.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removed, got %d", removed)
	}
	if deliverer.count() != 1 {
		t.Fatalf("expected 1 delivery, got %d", deliverer.count())
	}
	if _, ok := outbox.pending("sess_a"); ok {
		t.Fatal("wakeup should have been removed")
	}
}

func TestRelay_DeliversChildWakeupToIndependentWorkflow(t *testing.T) {
	outbox := &fakeThreadOutbox{
		fakeOutbox: newFakeOutbox(),
		thread: pg.OutboxWakeup{
			SessionID: "sess_child", ThreadID: "sthr_child", MaxEventSeq: 11,
			Intent: pg.OrchestrationWake,
		},
	}
	deliverer := &fakeThreadDeliverer{}
	relay := NewRelay(outbox, deliverer, RelayConfig{})

	removed, err := relay.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if removed != 1 || outbox.thread.ThreadID != "" {
		t.Fatalf("child wakeup removed=%d row=%+v", removed, outbox.thread)
	}
	if deliverer.count() != 1 || len(deliverer.threadIDs) != 1 ||
		deliverer.threadIDs[0] != "sthr_child" ||
		deliverer.delivered[0].MaxEventSeq != 11 {
		t.Fatalf(
			"child deliveries sessions=%v threads=%v signals=%v",
			deliverer.sessions, deliverer.threadIDs, deliverer.delivered,
		)
	}
}

func TestRelay_DeliversChildTerminationWithoutRestartingWorkflow(t *testing.T) {
	outbox := &fakeThreadOutbox{
		fakeOutbox: newFakeOutbox(),
		thread: pg.OutboxWakeup{
			SessionID: "sess_child", ThreadID: "sthr_child", MaxEventSeq: 19,
			Intent: pg.OrchestrationTerminate,
		},
	}
	deliverer := &fakeThreadDeliverer{}
	relay := NewRelay(outbox, deliverer, RelayConfig{})

	removed, err := relay.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("run once: %v", err)
	}
	if removed != 1 || outbox.thread.ThreadID != "" {
		t.Fatalf("child termination removed=%d row=%+v", removed, outbox.thread)
	}
	if len(deliverer.threadIDs) != 0 || len(deliverer.terminatedIDs) != 1 ||
		deliverer.terminatedIDs[0] != "sthr_child" {
		t.Fatalf(
			"wake threads=%v terminated threads=%v",
			deliverer.threadIDs, deliverer.terminatedIDs,
		)
	}
}

func TestRelay_RetriesChildTerminationUntilTemporalAcceptsIt(t *testing.T) {
	outbox := &fakeThreadOutbox{
		fakeOutbox: newFakeOutbox(),
		thread: pg.OutboxWakeup{
			SessionID: "sess_child", ThreadID: "sthr_child", MaxEventSeq: 21,
			Intent: pg.OrchestrationTerminate,
		},
	}
	deliverer := &fakeThreadDeliverer{fakeDeliverer: fakeDeliverer{failUntil: 1}}
	relay := NewRelay(outbox, deliverer, RelayConfig{})

	removed, err := relay.RunOnce(context.Background())
	if err != nil || removed != 0 || outbox.thread.ThreadID == "" ||
		outbox.attempts["sess_child/sthr_child"] != 1 {
		t.Fatalf(
			"failed termination removed=%d row=%+v attempts=%v err=%v",
			removed, outbox.thread, outbox.attempts, err,
		)
	}
	removed, err = relay.RunOnce(context.Background())
	if err != nil || removed != 1 || outbox.thread.ThreadID != "" ||
		len(deliverer.terminatedIDs) != 1 {
		t.Fatalf(
			"retried termination removed=%d row=%+v terminated=%v err=%v",
			removed, outbox.thread, deliverer.terminatedIDs, err,
		)
	}
}

// TestRelay_WorkerUnavailableLeavesWakeup proves admission can outrun the
// execution plane: when delivery fails, the wakeup stays for retry and later
// succeeds, without losing work.
func TestRelay_WorkerUnavailableLeavesWakeup(t *testing.T) {
	outbox := newFakeOutbox()
	deliverer := &fakeDeliverer{failUntil: 2} // first two attempts fail
	relay := NewRelay(outbox, deliverer, RelayConfig{})

	outbox.enqueue("sess_b", 3)

	// First two cycles: delivery fails, wakeup remains.
	for i := 0; i < 2; i++ {
		removed, err := relay.RunOnce(context.Background())
		if err != nil {
			t.Fatalf("run once %d: %v", i, err)
		}
		if removed != 0 {
			t.Fatalf("cycle %d: expected 0 removed, got %d", i, removed)
		}
		if _, ok := outbox.pending("sess_b"); !ok {
			t.Fatalf("cycle %d: wakeup must remain while worker unavailable", i)
		}
	}
	// Third cycle succeeds.
	removed, err := relay.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("final run once: %v", err)
	}
	if removed != 1 {
		t.Fatalf("expected 1 removed after recovery, got %d", removed)
	}
	if _, ok := outbox.pending("sess_b"); ok {
		t.Fatal("wakeup should be removed after successful delivery")
	}
}

// TestRelay_CrashAfterSignalBeforeDeleteRedelivers proves the crash boundary
// between signaling and deleting is harmless: the wakeup survives the crash and
// is re-delivered, which the workflow deduplicates by sequence. We model the
// crash by delivering (signal succeeds) then NOT calling delete — i.e. a second
// RunOnce sees the same row and delivers again.
func TestRelay_CrashAfterSignalBeforeDeleteRedelivers(t *testing.T) {
	outbox := newFakeOutbox()
	// A deliverer that always succeeds but we simulate a crash by using a store
	// whose delete is a no-op on the first observation.
	deliverer := &fakeDeliverer{}
	crashOutbox := &crashAfterSignalOutbox{fakeOutbox: outbox, skipDeletes: 1}
	relay := NewRelay(crashOutbox, deliverer, RelayConfig{})

	outbox.enqueue("sess_c", 7)

	// First cycle: signal succeeds, but the delete is "lost" (crash before delete
	// committed). The row remains.
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	if _, ok := outbox.pending("sess_c"); !ok {
		t.Fatal("wakeup must survive a crash before delete")
	}
	// Second cycle: re-delivers the same wakeup (harmless duplicate) and now the
	// delete lands.
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if deliverer.count() != 2 {
		t.Fatalf("expected 2 (duplicate) deliveries, got %d", deliverer.count())
	}
	if _, ok := outbox.pending("sess_c"); ok {
		t.Fatal("wakeup should be removed after the redelivery")
	}
}

// TestRelay_CoalescedAdmissionRaceKeepsWakeup proves that when a new admission
// raises the sequence after the relay read the row (but before it deletes), the
// delete is a no-op and the higher sequence is delivered on the next cycle.
func TestRelay_CoalescedAdmissionRaceKeepsWakeup(t *testing.T) {
	outbox := newFakeOutbox()
	deliverer := &fakeDeliverer{}
	// raceOutbox bumps the sequence between claim and delete, modeling an
	// admission that coalesced into the row mid-delivery.
	race := &raceOutbox{fakeOutbox: outbox, bumpTo: 9}
	relay := NewRelay(race, deliverer, RelayConfig{})

	outbox.enqueue("sess_d", 4)
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatalf("cycle 1: %v", err)
	}
	// The delete saw seq 9 (bumped) != 4 (delivered), so it did not remove.
	row, ok := outbox.pending("sess_d")
	if !ok {
		t.Fatal("wakeup must remain when a later admission raised its sequence")
	}
	if row.MaxEventSeq != 9 {
		t.Fatalf("expected coalesced seq 9, got %d", row.MaxEventSeq)
	}
	// Next cycle delivers the higher sequence and removes it.
	race.bumpTo = 0 // stop bumping
	if _, err := relay.RunOnce(context.Background()); err != nil {
		t.Fatalf("cycle 2: %v", err)
	}
	if _, ok := outbox.pending("sess_d"); ok {
		t.Fatal("wakeup should be removed once no further admission raced")
	}
	if got := deliverer.delivered[len(deliverer.delivered)-1].MaxEventSeq; got != 9 {
		t.Fatalf("expected final delivery at seq 9, got %d", got)
	}
}

// crashAfterSignalOutbox drops the first N delete calls to model a crash between
// a successful signal and the delete commit.
type crashAfterSignalOutbox struct {
	*fakeOutbox
	skipDeletes int
}

func (c *crashAfterSignalOutbox) DeleteWakeupIfUnchanged(ctx context.Context, sessionID string, maxSeq int64) (bool, error) {
	if c.skipDeletes > 0 {
		c.skipDeletes--
		return false, nil // delete "lost" to the crash
	}
	return c.fakeOutbox.DeleteWakeupIfUnchanged(ctx, sessionID, maxSeq)
}

// raceOutbox raises the stored sequence right before a delete, modeling an
// admission that coalesced into the row after the relay read it.
type raceOutbox struct {
	*fakeOutbox
	bumpTo int64
}

func (r *raceOutbox) DeleteWakeupIfUnchanged(ctx context.Context, sessionID string, maxSeq int64) (bool, error) {
	if r.bumpTo > 0 {
		r.enqueue(sessionID, r.bumpTo)
	}
	return r.fakeOutbox.DeleteWakeupIfUnchanged(ctx, sessionID, maxSeq)
}
