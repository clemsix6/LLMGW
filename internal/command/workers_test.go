package command

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// stubWorkerRepository is a workerRepository double that records how many
// times ReconcileAccounting was called, guarded by a mutex since the caller
// runs it under the race detector.
type stubWorkerRepository struct {
	mu             sync.Mutex
	reconcileCalls int
}

// ExpiringKeys satisfies governance.KeyExpiryReader without returning any key.
func (s *stubWorkerRepository) ExpiringKeys(context.Context, time.Time, time.Time) ([]governance.KeyInfo, error) {
	return nil, nil
}

// ReconcileAccounting records its call count and reports no work done.
func (s *stubWorkerRepository) ReconcileAccounting(context.Context, time.Time, time.Duration, time.Duration) (governance.ReconcileResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconcileCalls++
	return governance.ReconcileResult{}, nil
}

// PruneCompletedRequests reports no rows deleted.
func (s *stubWorkerRepository) PruneCompletedRequests(context.Context, time.Duration) (int64, error) {
	return 0, nil
}

// calls returns how many times ReconcileAccounting was called so far.
func (s *stubWorkerRepository) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reconcileCalls
}

// TestStartWorkersRunsImmediateReconcileAndStopsOnCancel proves the widened
// workerRepository interface is actually reachable through StartWorkers: a
// double implementing all three methods runs an immediate reconcile and the
// returned done channel closes once its context is cancelled.
func TestStartWorkersRunsImmediateReconcileAndStopsOnCancel(t *testing.T) {
	repo := &stubWorkerRepository{}
	ctx, cancel := context.WithCancel(context.Background())

	done := StartWorkers(ctx, repo, time.Hour)
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("StartWorkers did not stop after context cancellation")
	}

	if calls := repo.calls(); calls < 1 {
		t.Fatalf("ReconcileAccounting calls = %d, want at least 1", calls)
	}
}
