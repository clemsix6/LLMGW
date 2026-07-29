package command

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestWorkers catches a worker that skips immediate reconciliation or outlives cancellation.
func TestWorkers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repository := newWorkerRepositoryStub()
	done := StartWorkers(ctx, repository, 24*time.Hour)

	call := waitForWorkerCall(t, repository.calls)
	if call.kind != "reconcile" || call.settlementDelay != 30*time.Second || call.staleInFlightAge != 6*time.Hour {
		t.Fatalf("first worker call = %#v, want immediate reconcile with production delays", call)
	}
	cancel()
	waitForWorkers(t, done)
}

// TestWorkersSeparateTickers catches a worker that omits either periodic reconciliation or pruning.
func TestWorkersSeparateTickers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	repository := newWorkerRepositoryStub()
	done := startWorkersWithIntervals(
		ctx,
		repository,
		24*time.Hour,
		10*time.Millisecond,
		15*time.Millisecond,
		30*time.Second,
		6*time.Hour,
	)

	first := waitForWorkerCall(t, repository.calls)
	if first.kind != "reconcile" {
		t.Fatalf("first worker call = %#v, want immediate reconcile", first)
	}
	seenReconcile, seenPrune := false, false
	deadline := time.After(time.Second)
	for !seenReconcile || !seenPrune {
		select {
		case call := <-repository.calls:
			seenReconcile = seenReconcile || call.kind == "reconcile"
			if call.kind == "prune" && call.retention != 24*time.Hour {
				t.Fatalf("prune retention = %s, want 24h", call.retention)
			}
			seenPrune = seenPrune || call.kind == "prune"
		case <-deadline:
			t.Fatalf("periodic calls reconcile=%t prune=%t, want both", seenReconcile, seenPrune)
		}
	}
	cancel()
	waitForWorkers(t, done)
}

// workerCall records one observable repository boundary call.
type workerCall struct {
	kind             string        // kind identifies the reconciliation or pruning operation.
	settlementDelay  time.Duration // settlementDelay is the reconciliation grace period.
	staleInFlightAge time.Duration // staleInFlightAge is the stale request threshold.
	retention        time.Duration // retention is the completed request retention period.
}

// workerRepositoryStub records worker boundary calls without replacing worker behavior.
type workerRepositoryStub struct {
	calls chan workerCall // calls preserves the observable background call order.
	mu    sync.Mutex      // mu serializes writes into calls during test shutdown.
}

// newWorkerRepositoryStub creates an observable repository boundary for worker timing tests.
func newWorkerRepositoryStub() *workerRepositoryStub {
	return &workerRepositoryStub{calls: make(chan workerCall, 16)}
}

// ReconcileAccounting records the arguments received from the worker.
func (s *workerRepositoryStub) ReconcileAccounting(
	_ context.Context,
	_ time.Time,
	settlementDelay time.Duration,
	staleInFlightAge time.Duration,
) (governance.ReconcileResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls <- workerCall{
		kind:             "reconcile",
		settlementDelay:  settlementDelay,
		staleInFlightAge: staleInFlightAge,
	}
	return governance.ReconcileResult{}, nil
}

// PruneCompletedRequests records the retention duration received from the worker.
func (s *workerRepositoryStub) PruneCompletedRequests(_ context.Context, retention time.Duration) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls <- workerCall{kind: "prune", retention: retention}
	return 0, nil
}

// waitForWorkerCall receives one bounded worker call.
func waitForWorkerCall(t *testing.T, calls <-chan workerCall) workerCall {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(time.Second):
		t.Fatal("worker did not call its repository")
		return workerCall{}
	}
}

// waitForWorkers verifies that cancellation closes the worker completion channel.
func waitForWorkers(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after cancellation")
	}
}
