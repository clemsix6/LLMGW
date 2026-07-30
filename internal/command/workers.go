package command

import (
	"context"
	"log"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

const (
	reconcileInterval = 5 * time.Second
	settlementDelay   = 30 * time.Second
	staleInFlightAge  = 6 * time.Hour
	pruneInterval     = time.Hour
)

// workerRepository contains only the lifecycle methods used by background workers.
type workerRepository interface {
	governance.KeyExpiryReader

	// ReconcileAccounting resolves delayed and stale request accounting.
	ReconcileAccounting(context.Context, time.Time, time.Duration, time.Duration) (governance.ReconcileResult, error)
	// PruneCompletedRequests deletes completed request trees older than retention.
	PruneCompletedRequests(context.Context, time.Duration) (int64, error)
}

// StartWorkers starts background accounting reconciliation and completed request retention.
func StartWorkers(ctx context.Context, repo workerRepository, retention time.Duration) <-chan struct{} {
	return startWorkersWithIntervals(
		ctx,
		repo,
		retention,
		reconcileInterval,
		pruneInterval,
		settlementDelay,
		staleInFlightAge,
	)
}

// startWorkersWithIntervals runs one worker goroutine with independently scheduled jobs.
func startWorkersWithIntervals(
	ctx context.Context,
	repo workerRepository,
	retention time.Duration,
	reconcileEvery time.Duration,
	pruneEvery time.Duration,
	settleAfter time.Duration,
	staleAfter time.Duration,
) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		reconcile(ctx, repo, settleAfter, staleAfter)
		runWorkerTicks(ctx, repo, retention, reconcileEvery, pruneEvery, settleAfter, staleAfter)
	}()
	return done
}

// runWorkerTicks waits for cancellation or the independent reconciliation and retention schedules.
func runWorkerTicks(
	ctx context.Context,
	repo workerRepository,
	retention time.Duration,
	reconcileEvery time.Duration,
	pruneEvery time.Duration,
	settleAfter time.Duration,
	staleAfter time.Duration,
) {
	reconcileTicker := time.NewTicker(reconcileEvery)
	defer reconcileTicker.Stop()
	pruneTicker := time.NewTicker(pruneEvery)
	defer pruneTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-reconcileTicker.C:
			reconcile(ctx, repo, settleAfter, staleAfter)
		case <-pruneTicker.C:
			pruneCompleted(ctx, repo, retention)
		}
	}
}

// reconcile performs one accounting sweep and logs only aggregate outcomes.
func reconcile(ctx context.Context, repo workerRepository, settleAfter time.Duration, staleAfter time.Duration) {
	result, err := repo.ReconcileAccounting(ctx, time.Now().UTC(), settleAfter, staleAfter)
	if err != nil {
		log.Printf("governance reconciliation failed: %v", err)
		return
	}
	log.Printf("governance reconciliation: observed=%d unknown=%d", result.Observed, result.Unknown)
}

// pruneCompleted performs one completed request retention sweep and logs its aggregate outcome.
func pruneCompleted(ctx context.Context, repo workerRepository, retention time.Duration) {
	deleted, err := repo.PruneCompletedRequests(ctx, retention)
	if err != nil {
		log.Printf("governance retention failed: %v", err)
		return
	}
	log.Printf("governance retention: deleted=%d", deleted)
}
