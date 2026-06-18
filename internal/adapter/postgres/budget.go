package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// LimitsFor returns the budget limits that apply to a (project, tag): the rows whose tag matches
// the concrete request tag, plus the whole-project rows (tag IS NULL) that apply across every
// tag. Rows are ordered by id so limit evaluation is deterministic (first blocking limit wins).
func (s *Store) LimitsFor(ctx context.Context, projectID int64, tag string) ([]domain.BudgetLimit, error) {
	const query = `
SELECT tag, dimension, "window", max_value, action
FROM budget_limit
WHERE project_id = $1 AND (tag = $2 OR tag IS NULL)
ORDER BY id`

	rows, err := s.pool.Query(ctx, query, projectID, tag)
	if err != nil {
		return nil, fmt.Errorf("query limits for project %d tag %q:\n%w", projectID, tag, err)
	}
	defer rows.Close()

	// A NULL tag scans into a nil *string, preserving the whole-project sentinel for the domain.
	var limits []domain.BudgetLimit

	for rows.Next() {
		var limit domain.BudgetLimit
		if err := rows.Scan(&limit.Tag, &limit.Dimension, &limit.Window, &limit.MaxValue, &limit.Action); err != nil {
			return nil, fmt.Errorf("scan budget limit:\n%w", err)
		}
		limits = append(limits, limit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate budget limits:\n%w", err)
	}
	return limits, nil
}

// Reserve records an in-flight call for a (project, tag) and returns the reservation id. The row
// expires after ttl (computed from the database clock to avoid app/DB skew); InflightTotals
// counts it until then or until ReleaseReservation removes it.
func (s *Store) Reserve(ctx context.Context, projectID int64, tag string, ttl time.Duration) (int64, error) {
	const query = `
INSERT INTO reservation (project_id, tag, expires_at)
VALUES ($1, $2, now() + make_interval(secs => $3))
RETURNING id`

	var id int64
	if err := s.pool.QueryRow(ctx, query, projectID, tag, ttl.Seconds()).Scan(&id); err != nil {
		return 0, fmt.Errorf("reserve for project %d tag %q:\n%w", projectID, tag, err)
	}
	return id, nil
}

// ReleaseReservation removes a previously created reservation. Releasing an absent id is a no-op.
func (s *Store) ReleaseReservation(ctx context.Context, reservationID int64) error {
	const query = `DELETE FROM reservation WHERE id = $1`

	if _, err := s.pool.Exec(ctx, query, reservationID); err != nil {
		return fmt.Errorf("release reservation %d:\n%w", reservationID, err)
	}
	return nil
}

// InflightTotals aggregates the non-expired reservations for a (project, tag) into a Totals whose
// only populated dimension is Calls — each reservation counts as one in-flight call. Tokens and
// cost are NOT reserved pre-call (output tokens and cost are unknown until the call completes), so
// Calls is the dimension the concurrency guard relies on to stop simultaneous requests from
// collectively overshooting a calls limit. The domain.WholeProjectTag sentinel aggregates across
// every tag of the project, mirroring WindowedTotals.
//
// Expired reservations are pruned first, so a request that crashed between Reserve and
// ReleaseReservation cannot inflate the count beyond its TTL.
func (s *Store) InflightTotals(ctx context.Context, projectID int64, tag string) (domain.Totals, error) {
	if err := s.pruneExpiredReservations(ctx); err != nil {
		return domain.Totals{}, err
	}

	query := `SELECT COUNT(*)::bigint FROM reservation WHERE project_id = $1 AND expires_at >= now()`
	args := []any{projectID}
	if tag != domain.WholeProjectTag {
		query += ` AND tag = $2`
		args = append(args, tag)
	}

	var totals domain.Totals
	if err := s.pool.QueryRow(ctx, query, args...).Scan(&totals.Calls); err != nil {
		return domain.Totals{}, fmt.Errorf("inflight totals for project %d tag %q:\n%w", projectID, tag, err)
	}
	return totals, nil
}

// pruneExpiredReservations deletes every reservation whose TTL has elapsed. It runs before each
// InflightTotals read so the concurrency guard never counts leaked (expired) reservations.
func (s *Store) pruneExpiredReservations(ctx context.Context) error {
	const query = `DELETE FROM reservation WHERE expires_at < now()`

	if _, err := s.pool.Exec(ctx, query); err != nil {
		return fmt.Errorf("prune expired reservations:\n%w", err)
	}
	return nil
}
