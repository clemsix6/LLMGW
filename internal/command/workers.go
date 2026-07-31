package command

import (
	"context"
	"log"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/governance/alert"
)

const (
	reconcileInterval = 5 * time.Second
	settlementDelay   = 30 * time.Second
	staleInFlightAge  = 6 * time.Hour
	pruneInterval     = time.Hour
	sweepInterval     = time.Hour
)

// Project-key sweep window. The lookback keeps a key that expired while nobody
// was watching reportable; the horizon is how far ahead an expiry is announced.
const (
	sweepKeyLookback = 30 * 24 * time.Hour
	sweepKeyHorizon  = 7 * 24 * time.Hour
)

// workerRepository contains only the lifecycle methods used by background workers.
type workerRepository interface {
	governance.KeyExpiryReader

	// ReconcileAccounting resolves delayed and stale request accounting.
	ReconcileAccounting(context.Context, time.Time, time.Duration, time.Duration) (governance.ReconcileResult, error)
	// PruneCompletedRequests deletes completed request trees older than retention.
	PruneCompletedRequests(context.Context, time.Duration) (int64, error)
}

// workerSchedule groups the independently scheduled worker delays, so the two
// worker entry points stay at a readable parameter count.
type workerSchedule struct {
	reconcileEvery time.Duration // reconcileEvery is the accounting reconciliation period.
	pruneEvery     time.Duration // pruneEvery is the completed-request retention period.
	sweepEvery     time.Duration // sweepEvery is the project-key expiry sweep period.
	settleAfter    time.Duration // settleAfter is how long an unobserved request may wait before reconciliation.
	staleAfter     time.Duration // staleAfter is the age at which an in-flight request is abandoned.
}

// StartWorkers starts background accounting reconciliation, completed request
// retention, and the project-key expiry sweep.
//
// tracker may be nil: every observation it receives is then a no-op.
func StartWorkers(
	ctx context.Context,
	repo workerRepository,
	retention time.Duration,
	tracker *alert.Tracker,
) <-chan struct{} {
	return startWorkersWithIntervals(ctx, repo, retention, tracker, workerSchedule{
		reconcileEvery: reconcileInterval,
		pruneEvery:     pruneInterval,
		sweepEvery:     sweepInterval,
		settleAfter:    settlementDelay,
		staleAfter:     staleInFlightAge,
	})
}

// startWorkersWithIntervals runs one worker goroutine with independently scheduled jobs.
func startWorkersWithIntervals(
	ctx context.Context,
	repo workerRepository,
	retention time.Duration,
	tracker *alert.Tracker,
	schedule workerSchedule,
) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		reconcile(ctx, repo, tracker, schedule.settleAfter, schedule.staleAfter)
		sweepProjectKeys(ctx, repo, tracker)
		runWorkerTicks(ctx, repo, retention, tracker, schedule)
	}()
	return done
}

// runWorkerTicks waits for cancellation or one of the three independent schedules.
func runWorkerTicks(
	ctx context.Context,
	repo workerRepository,
	retention time.Duration,
	tracker *alert.Tracker,
	schedule workerSchedule,
) {
	reconcileTicker := time.NewTicker(schedule.reconcileEvery)
	defer reconcileTicker.Stop()
	pruneTicker := time.NewTicker(schedule.pruneEvery)
	defer pruneTicker.Stop()
	sweepTicker := time.NewTicker(schedule.sweepEvery)
	defer sweepTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-reconcileTicker.C:
			reconcile(ctx, repo, tracker, schedule.settleAfter, schedule.staleAfter)
		case <-pruneTicker.C:
			pruneCompleted(ctx, repo, tracker, retention)
		case <-sweepTicker.C:
			sweepProjectKeys(ctx, repo, tracker)
		}
	}
}

// reconcile performs one accounting sweep and logs only aggregate outcomes.
func reconcile(
	ctx context.Context,
	repo workerRepository,
	tracker *alert.Tracker,
	settleAfter time.Duration,
	staleAfter time.Duration,
) {
	result, err := repo.ReconcileAccounting(ctx, time.Now().UTC(), settleAfter, staleAfter)
	if err != nil {
		log.Printf("governance reconciliation failed: %v", err)
		observeWorkerFailure(ctx, tracker)
		return
	}
	tracker.ObserveDatabase(true)
	log.Printf("governance reconciliation: observed=%d unknown=%d", result.Observed, result.Unknown)
}

// pruneCompleted performs one completed request retention sweep and logs its aggregate outcome.
func pruneCompleted(
	ctx context.Context,
	repo workerRepository,
	tracker *alert.Tracker,
	retention time.Duration,
) {
	deleted, err := repo.PruneCompletedRequests(ctx, retention)
	if err != nil {
		log.Printf("governance retention failed: %v", err)
		observeWorkerFailure(ctx, tracker)
		return
	}
	tracker.ObserveDatabase(true)
	log.Printf("governance retention: deleted=%d", deleted)
}

// sweepProjectKeys reports the expiry state of every key whose expiry falls in
// the swept window, from the recent past to the announced horizon.
func sweepProjectKeys(ctx context.Context, repo workerRepository, tracker *alert.Tracker) {
	now := time.Now().UTC()
	keys, err := repo.ExpiringKeys(ctx, now.Add(-sweepKeyLookback), now.Add(sweepKeyHorizon))
	if err != nil {
		log.Printf("governance key expiry sweep failed: %v", err)
		observeWorkerFailure(ctx, tracker)
		return
	}
	tracker.ObserveProjectKeys(keys, now)
}

// observeWorkerFailure reports a repository error as a database outage only
// while the worker context is still live: shutdown cancels the query in flight,
// and a cancelled caller says nothing about PostgreSQL's health.
func observeWorkerFailure(ctx context.Context, tracker *alert.Tracker) {
	if ctx.Err() != nil {
		return
	}
	tracker.ObserveDatabase(false)
}
