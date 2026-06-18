package postgres

import (
	"context"
	"fmt"
	"time"
)

// PruneOlderThan removes usage_event rows older than usageRetention and every expired reservation,
// returning how many of each were deleted. It satisfies the design §6 retention policy: keeping
// usage_event bounded so the windowed-aggregate hot path stays cheap, and sweeping leaked
// reservations as a periodic backstop to the per-admission prune. Cutoffs use the database clock
// to avoid app/DB skew.
func (s *Store) PruneOlderThan(ctx context.Context, usageRetention time.Duration) (usageDeleted, resvDeleted int64, err error) {
	usageDeleted, err = s.pruneUsageEvents(ctx, usageRetention)
	if err != nil {
		return 0, 0, err
	}

	resvDeleted, err = s.pruneExpiredReservationRows(ctx)
	if err != nil {
		return 0, 0, err
	}
	return usageDeleted, resvDeleted, nil
}

// pruneUsageEvents deletes usage_event rows recorded before now() - retention and returns the
// count removed.
func (s *Store) pruneUsageEvents(ctx context.Context, retention time.Duration) (int64, error) {
	const query = `DELETE FROM usage_event WHERE ts < now() - make_interval(secs => $1)`

	tag, err := s.pool.Exec(ctx, query, retention.Seconds())
	if err != nil {
		return 0, fmt.Errorf("prune usage events older than %s:\n%w", retention, err)
	}
	return tag.RowsAffected(), nil
}

// pruneExpiredReservationRows deletes every reservation whose TTL has elapsed, across all
// projects, and returns the count removed.
func (s *Store) pruneExpiredReservationRows(ctx context.Context) (int64, error) {
	const query = `DELETE FROM reservation WHERE expires_at < now()`

	tag, err := s.pool.Exec(ctx, query)
	if err != nil {
		return 0, fmt.Errorf("prune expired reservations:\n%w", err)
	}
	return tag.RowsAffected(), nil
}
