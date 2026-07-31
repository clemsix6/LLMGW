package command

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

// stubWorkerRepository is a workerRepository double recording what the workers
// asked it for, guarded by a mutex since the caller runs it under the race
// detector.
type stubWorkerRepository struct {
	mu             sync.Mutex           // mu guards every recorded field against the worker goroutine.
	reconcileCalls int                  // reconcileCalls counts the accounting sweeps performed.
	expiryFrom     time.Time            // expiryFrom is the lower bound of the last swept window.
	expiryTo       time.Time            // expiryTo is the upper bound of the last swept window.
	expiryKeys     []governance.KeyInfo // expiryKeys is what the sweep returns.
	expiryErr      error                // expiryErr fails the sweep when set.
}

// ExpiringKeys records the swept window and returns the configured outcome.
func (s *stubWorkerRepository) ExpiringKeys(
	_ context.Context,
	from time.Time,
	to time.Time,
) ([]governance.KeyInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.expiryFrom, s.expiryTo = from, to
	return s.expiryKeys, s.expiryErr
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

// sweptWindow returns the bounds of the last expiry sweep.
func (s *stubWorkerRepository) sweptWindow() (time.Time, time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expiryFrom, s.expiryTo
}

// recordingNotifier collects the events one tracker delivered, behind a mutex:
// the worker goroutine emits while the test reads.
type recordingNotifier struct {
	mu     sync.Mutex    // mu guards events against the worker goroutine.
	events []alert.Event // events are the accepted events, in delivery order.
}

// Notify accepts and records one event.
func (r *recordingNotifier) Notify(event alert.Event) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.events = append(r.events, event)
	return true
}

// collected returns the events recorded so far.
func (r *recordingNotifier) collected() []alert.Event {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]alert.Event(nil), r.events...)
}

// workerTracker builds a tracker with no anti-flap window, so a second
// transition inside one test can never be suppressed.
func workerTracker() (*alert.Tracker, *recordingNotifier) {
	sink := &recordingNotifier{}
	return alert.New(sink, nil, 0, time.Now), sink
}

// idleWorkerSchedule keeps every ticker far away, so only the startup pass runs.
func idleWorkerSchedule() workerSchedule {
	return workerSchedule{
		reconcileEvery: time.Hour,
		pruneEvery:     time.Hour,
		sweepEvery:     time.Hour,
		settleAfter:    settlementDelay,
		staleAfter:     staleInFlightAge,
	}
}

// awaitWorkers fails the test unless the worker goroutine returned in time.
func awaitWorkers(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("workers did not stop after context cancellation")
	}
}

// waitForEvents polls until the sink collected at least count events, under a
// bounded deadline: the worker goroutine emits asynchronously.
func waitForEvents(t *testing.T, sink *recordingNotifier, count int) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(sink.collected()) >= count {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("collected %d events, want at least %d", len(sink.collected()), count)
}

// TestStartWorkersRunsImmediateReconcileAndStopsOnCancel proves the widened
// workerRepository interface is actually reachable through StartWorkers: a
// double implementing all three methods runs an immediate reconcile and the
// returned done channel closes once its context is cancelled.
func TestStartWorkersRunsImmediateReconcileAndStopsOnCancel(t *testing.T) {
	repo := &stubWorkerRepository{}
	ctx, cancel := context.WithCancel(context.Background())

	done := StartWorkers(ctx, repo, time.Hour, nil)
	cancel()
	awaitWorkers(t, done)

	if calls := repo.calls(); calls < 1 {
		t.Fatalf("ReconcileAccounting calls = %d, want at least 1", calls)
	}
}

// TestWorkerSweepAsksForTheDocumentedWindow proves the startup sweep queries the
// 30-day lookback and the 7-day horizon around its own clock, and forwards what
// the repository returned to the tracker.
func TestWorkerSweepAsksForTheDocumentedWindow(t *testing.T) {
	repo := &stubWorkerRepository{expiryKeys: []governance.KeyInfo{expiringKeyFixture()}}
	tracker, sink := workerTracker()

	earliest := time.Now().UTC()
	ctx, cancel := context.WithCancel(context.Background())
	done := startWorkersWithIntervals(ctx, repo, time.Hour, tracker, idleWorkerSchedule())
	cancel()
	awaitWorkers(t, done)
	latest := time.Now().UTC()

	from, to := repo.sweptWindow()
	assertWithin(t, "lookback", from, earliest.Add(-sweepKeyLookback), latest.Add(-sweepKeyLookback))
	assertWithin(t, "horizon", to, earliest.Add(sweepKeyHorizon), latest.Add(sweepKeyHorizon))
	assertSweptKey(t, sink, "pk_sweep")
}

// expiringKeyFixture builds one key the transition engine reports as expiring:
// long-lived enough to escape the short-lifetime skip, close enough to the
// horizon to be announced.
func expiringKeyFixture() governance.KeyInfo {
	now := time.Now().UTC()
	expiresAt := now.Add(48 * time.Hour)

	return governance.KeyInfo{
		ProjectName: "billing",
		Name:        "ci",
		PublicID:    "pk_sweep",
		CreatedAt:   now.Add(-60 * 24 * time.Hour),
		ExpiresAt:   &expiresAt,
	}
}

// TestWorkerFailureReportsOnlyOnALiveContext pins the guard that keeps a clean
// shutdown from looking like an outage: the same repository failure is a
// database outage while the worker context is live, and nothing once it is
// cancelled.
func TestWorkerFailureReportsOnlyOnALiveContext(t *testing.T) {
	tests := []struct {
		name          string
		cancelUpfront bool
		wantEvents    int
	}{
		{name: "live context reports the outage", cancelUpfront: false, wantEvents: 1},
		{name: "cancelled context stays quiet", cancelUpfront: true, wantEvents: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repo := &stubWorkerRepository{expiryErr: context.Canceled}
			tracker, sink := workerTracker()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.cancelUpfront {
				cancel()
			}
			done := startWorkersWithIntervals(ctx, repo, time.Hour, tracker, idleWorkerSchedule())
			if !test.cancelUpfront {
				waitForEvents(t, sink, 1)
				cancel()
			}
			awaitWorkers(t, done)

			assertDatabaseEvents(t, sink, test.wantEvents)
		})
	}
}

// assertWithin fails unless the bound the sweep asked for falls between the
// times the test observed around it, which is the only tolerance a wall clock
// read inside the worker allows.
func assertWithin(t *testing.T, name string, got, earliest, latest time.Time) {
	t.Helper()

	if got.Before(earliest) || got.After(latest) {
		t.Fatalf("%s bound = %s, want between %s and %s", name, got, earliest, latest)
	}
}

// assertSweptKey fails unless the sweep forwarded the key to the tracker.
func assertSweptKey(t *testing.T, sink *recordingNotifier, publicID string) {
	t.Helper()

	for _, event := range sink.collected() {
		if event.Kind != alert.KindProjectKeyExpiring {
			continue
		}
		if alertFieldValue(event.Fields, "Public ID") == publicID {
			return
		}
	}
	t.Fatalf("no %s event for key %q", alert.KindProjectKeyExpiring, publicID)
}

// assertDatabaseEvents fails unless the sink collected exactly the expected
// number of events, all of them the database outage.
func assertDatabaseEvents(t *testing.T, sink *recordingNotifier, want int) {
	t.Helper()

	events := sink.collected()
	if len(events) != want {
		t.Fatalf("events = %d, want %d", len(events), want)
	}
	for _, event := range events {
		if event.Kind != alert.KindDatabaseUnavailable {
			t.Fatalf("event kind = %s, want %s", event.Kind, alert.KindDatabaseUnavailable)
		}
	}
}
