package temporal

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeDeletionStore struct {
	mu              sync.Mutex
	pending         []string
	memoryFinalized []string
	finalized       []string
	memoryErr       map[string]error
	finalizeErr     map[string]error
}

func (s *fakeDeletionStore) ListDeletingSessionIDs(_ context.Context, limit int) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) < limit {
		limit = len(s.pending)
	}
	return append([]string(nil), s.pending[:limit]...), nil
}

func (s *fakeDeletionStore) FinalizeSessionMemoryResources(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.memoryErr[id]; err != nil {
		return err
	}
	s.memoryFinalized = append(s.memoryFinalized, id)
	return nil
}

func (s *fakeDeletionStore) FinalizeSessionDeletion(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.finalizeErr[id]; err != nil {
		return err
	}
	s.finalized = append(s.finalized, id)
	for index, pending := range s.pending {
		if pending == id {
			s.pending = append(s.pending[:index], s.pending[index+1:]...)
			break
		}
	}
	return nil
}

type fakeSessionTerminator struct {
	mu    sync.Mutex
	calls []string
	fail  map[string]error
}

func (t *fakeSessionTerminator) TerminateSession(_ context.Context, id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls = append(t.calls, id)
	return t.fail[id]
}

func TestLifecycleReconciler_ResumesFencedDeletion(t *testing.T) {
	store := &fakeDeletionStore{pending: []string{"sesn_a", "sesn_b"}}
	terminator := &fakeSessionTerminator{}
	reconciler := NewLifecycleReconciler(
		store, terminator,
		LifecycleReconcilerConfig{BatchSize: 10, AttemptTimeout: time.Second},
	)
	result, err := reconciler.RunOnce(context.Background())
	if err != nil || result.Deletions != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !equalStrings(terminator.calls, []string{"sesn_a", "sesn_b"}) ||
		!equalStrings(store.memoryFinalized, []string{"sesn_a", "sesn_b"}) ||
		!equalStrings(store.finalized, []string{"sesn_a", "sesn_b"}) {
		t.Fatalf("terminate=%v memory=%v finalized=%v", terminator.calls, store.memoryFinalized, store.finalized)
	}
}

func TestLifecycleReconciler_FailureDoesNotBlockBatch(t *testing.T) {
	store := &fakeDeletionStore{pending: []string{"sesn_stuck", "sesn_ready"}}
	terminator := &fakeSessionTerminator{fail: map[string]error{"sesn_stuck": errors.New("temporal unavailable")}}
	reconciler := NewLifecycleReconciler(
		store, terminator,
		LifecycleReconcilerConfig{BatchSize: 10, AttemptTimeout: time.Second},
	)
	result, err := reconciler.RunOnce(context.Background())
	if err == nil || result.Deletions != 1 || !equalStrings(store.pending, []string{"sesn_stuck"}) {
		t.Fatalf("result=%+v pending=%v err=%v", result, store.pending, err)
	}
}

func TestLifecycleReconciler_MemoryCleanupFailurePreventsDeletion(t *testing.T) {
	store := &fakeDeletionStore{
		pending:   []string{"sesn_memory"},
		memoryErr: map[string]error{"sesn_memory": errors.New("postgres unavailable")},
	}
	reconciler := NewLifecycleReconciler(store, &fakeSessionTerminator{}, LifecycleReconcilerConfig{})
	result, err := reconciler.RunOnce(context.Background())
	if err == nil || result.Deletions != 0 || len(store.finalized) != 0 {
		t.Fatalf("result=%+v finalized=%v err=%v", result, store.finalized, err)
	}
}

func TestLifecycleReconciler_DrainDelayHasPositiveFloor(t *testing.T) {
	reconciler := NewLifecycleReconciler(nil, nil, LifecycleReconcilerConfig{PollInterval: 5 * time.Second})
	if got := reconciler.nextDelay(LifecycleReconcileResult{Deletions: 1}); got != lifecycleDrainDelay {
		t.Fatalf("active delay = %v, want %v", got, lifecycleDrainDelay)
	}
	if got := reconciler.nextDelay(LifecycleReconcileResult{}); got != 5*time.Second {
		t.Fatalf("idle delay = %v, want 5s", got)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}
