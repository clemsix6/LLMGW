package postgres

import (
	"context"
	"fmt"
	"time"
)

// PruneCompletedRequests deletes completed request trees older than retention.
func (s *Store) PruneCompletedRequests(ctx context.Context, retention time.Duration) (int64, error) {
	const query = `
DELETE FROM request_event
WHERE state = 'completed'
  AND requested_at < now() - make_interval(secs => $1)`
	tag, err := s.pool.Exec(ctx, query, retention.Seconds())
	if err != nil {
		return 0, fmt.Errorf("prune completed requests older than %s:\n%w", retention, err)
	}
	return tag.RowsAffected(), nil
}
