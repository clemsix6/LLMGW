package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/jackc/pgx/v5"
)

// RecoverInterrupted completes requests left in flight by a terminated process.
func (s *Store) RecoverInterrupted(ctx context.Context, recoveredAt time.Time) (int64, error) {
	const query = `
UPDATE request_event r
SET state = 'completed',
    completed_at = $1,
    accounting_state = CASE
        WHEN r.operation = 'metadata' THEN 'not_applicable'
        ELSE 'accounting_unknown'
    END,
    accounting_resolved_at = NULL
WHERE r.state = 'in_flight'`
	tag, err := s.pool.Exec(ctx, query, recoveredAt.UTC())
	if err != nil {
		return 0, fmt.Errorf("recover interrupted requests:\n%w", err)
	}
	return tag.RowsAffected(), nil
}

// ReconcileAccounting resolves delayed usage and stale in-flight generation requests.
func (s *Store) ReconcileAccounting(
	ctx context.Context,
	now time.Time,
	settlementDelay time.Duration,
	staleInFlightAge time.Duration,
) (governance.ReconcileResult, error) {
	observed, err := s.reconcileObserved(ctx, now, settlementDelay)
	if err != nil {
		return governance.ReconcileResult{}, err
	}
	unknownCompleted, err := s.reconcileUnknownCompleted(ctx, now, settlementDelay)
	if err != nil {
		return governance.ReconcileResult{}, err
	}
	stale, err := s.reconcileStale(ctx, now, staleInFlightAge)
	if err != nil {
		return governance.ReconcileResult{}, err
	}
	return governance.ReconcileResult{
		Observed: observed + stale.Observed,
		Unknown:  unknownCompleted + stale.Unknown,
	}, nil
}

// ResolveUnknownAsZero records an operator's explicit zero-usage resolution.
func (s *Store) ResolveUnknownAsZero(ctx context.Context, requestID string, resolvedAt time.Time) error {
	const query = `
UPDATE request_event
SET accounting_state = 'resolved_zero', accounting_resolved_at = $2
WHERE id = $1
  AND operation = 'generation'
  AND accounting_state = 'accounting_unknown'
RETURNING id`
	var resolvedID string
	err := s.pool.QueryRow(ctx, query, requestID, resolvedAt.UTC()).Scan(&resolvedID)
	if err != nil {
		if err == pgx.ErrNoRows {
			return fmt.Errorf("resolve unknown request %q as zero: request is not an unknown generation", requestID)
		}
		return fmt.Errorf("resolve unknown request %q as zero:\n%w", requestID, err)
	}
	return nil
}

// reconcileObserved marks settled completed requests with durable usage as observed.
func (s *Store) reconcileObserved(ctx context.Context, now time.Time, delay time.Duration) (int64, error) {
	const query = `
UPDATE request_event r
SET accounting_state = 'observed', accounting_resolved_at = NULL
WHERE r.operation = 'generation'
  AND r.state = 'completed'
  AND r.accounting_state = 'pending'
  AND r.completed_at < $1::timestamptz - make_interval(secs => $2::double precision)
  AND EXISTS (SELECT 1 FROM usage_attempt a WHERE a.request_id = r.id)`
	return s.reconcileRows(ctx, query, now, delay, "reconcile observed accounting")
}

// reconcileUnknownCompleted marks settled completed requests without usage as unknown.
func (s *Store) reconcileUnknownCompleted(ctx context.Context, now time.Time, delay time.Duration) (int64, error) {
	const query = `
UPDATE request_event r
SET accounting_state = 'accounting_unknown', accounting_resolved_at = NULL
WHERE r.operation = 'generation'
  AND r.state = 'completed'
  AND r.accounting_state = 'pending'
  AND r.completed_at < $1::timestamptz - make_interval(secs => $2::double precision)
  AND NOT EXISTS (SELECT 1 FROM usage_attempt a WHERE a.request_id = r.id)`
	return s.reconcileRows(ctx, query, now, delay, "reconcile unknown completed accounting")
}

// reconcileStale completes every stale generation after locking it against attempt persistence.
func (s *Store) reconcileStale(
	ctx context.Context,
	now time.Time,
	age time.Duration,
) (governance.ReconcileResult, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return governance.ReconcileResult{}, fmt.Errorf("begin stale accounting reconciliation:\n%w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockStaleGenerations(ctx, tx, now, age); err != nil {
		return governance.ReconcileResult{}, err
	}
	result, err := transitionStaleGenerations(ctx, tx, now, age)
	if err != nil {
		return governance.ReconcileResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return governance.ReconcileResult{}, fmt.Errorf("commit stale accounting reconciliation:\n%w", err)
	}
	return result, nil
}

// lockStaleGenerations serializes stale resolution with RecordAttempt parent locking.
func lockStaleGenerations(
	ctx context.Context,
	tx pgx.Tx,
	now time.Time,
	age time.Duration,
) error {
	const query = `
SELECT id
FROM request_event
WHERE operation = 'generation'
  AND state = 'in_flight'
  AND requested_at < $1::timestamptz - make_interval(secs => $2::double precision)
FOR UPDATE`
	rows, err := tx.Query(ctx, query, now.UTC(), age.Seconds())
	if err != nil {
		return fmt.Errorf("lock stale generation requests:\n%w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var requestID string
		if err := rows.Scan(&requestID); err != nil {
			return fmt.Errorf("scan stale generation request:\n%w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate stale generation requests:\n%w", err)
	}
	return nil
}

// transitionStaleGenerations classifies locked stale rows from their durable attempt set.
func transitionStaleGenerations(
	ctx context.Context,
	tx pgx.Tx,
	now time.Time,
	age time.Duration,
) (governance.ReconcileResult, error) {
	const query = `
WITH transitioned AS (
    UPDATE request_event r
    SET state = 'completed',
        completed_at = $1,
        accounting_state = CASE
            WHEN EXISTS (SELECT 1 FROM usage_attempt a WHERE a.request_id = r.id)
                THEN 'observed'
            ELSE 'accounting_unknown'
        END,
        accounting_resolved_at = NULL
    WHERE r.operation = 'generation'
      AND r.state = 'in_flight'
      AND r.requested_at < $1::timestamptz - make_interval(secs => $2::double precision)
    RETURNING accounting_state
)
SELECT count(*) FILTER (WHERE accounting_state = 'observed')::bigint,
       count(*) FILTER (WHERE accounting_state = 'accounting_unknown')::bigint
FROM transitioned`
	var result governance.ReconcileResult
	err := tx.QueryRow(ctx, query, now.UTC(), age.Seconds()).Scan(&result.Observed, &result.Unknown)
	if err != nil {
		return governance.ReconcileResult{}, fmt.Errorf("transition stale generation requests:\n%w", err)
	}
	return result, nil
}

// reconcileRows executes one guarded accounting state transition.
func (s *Store) reconcileRows(
	ctx context.Context,
	query string,
	now time.Time,
	age time.Duration,
	operation string,
) (int64, error) {
	tag, err := s.pool.Exec(ctx, query, now.UTC(), age.Seconds())
	if err != nil {
		return 0, fmt.Errorf("%s:\n%w", operation, err)
	}
	return tag.RowsAffected(), nil
}
