package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// RecordUsage persists a completed (or failed) call as a usage_event row. The event's own
// timestamp is stored verbatim so windowed aggregation can be exercised deterministically.
func (s *Store) RecordUsage(ctx context.Context, e domain.UsageEvent) error {
	const query = `
INSERT INTO usage_event
    (ts, project_id, tag, model, provider, input_tokens, output_tokens, cost_usd, status, latency_ms, error)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`

	_, err := s.pool.Exec(ctx, query,
		e.Timestamp, e.ProjectID, e.Tag, e.Model, e.Provider,
		e.InputTokens, e.OutputTokens, e.CostUSD, e.Status, e.LatencyMS, nullableError(e.Error),
	)
	if err != nil {
		return fmt.Errorf("record usage event (project=%d tag=%q):\n%w", e.ProjectID, e.Tag, err)
	}
	return nil
}

// nullableError maps an empty error message to a NULL column so successful rows carry no error.
func nullableError(message string) *string {
	if message == "" {
		return nil
	}
	return &message
}

// WindowedTotals sums usage for a project over the usage_event rows recorded since the given
// time. A specific tag restricts the aggregate to that bucket; the domain.WholeProjectTag
// sentinel aggregates across every tag of the project.
func (s *Store) WindowedTotals(ctx context.Context, projectID int64, tag string, since time.Time) (domain.Totals, error) {
	return windowedTotals(ctx, s.pool, projectID, tag, since)
}

// windowedTotals runs the windowed SUM aggregate against q, which is the pool on the public path
// and the admission transaction on the concurrency-safe path. Sharing the SQL keeps the two
// readers in lockstep.
func windowedTotals(ctx context.Context, q rowQuerier, projectID int64, tag string, since time.Time) (domain.Totals, error) {
	query := `
SELECT
    COUNT(*)::bigint,
    COALESCE(SUM(input_tokens), 0)::bigint,
    COALESCE(SUM(output_tokens), 0)::bigint,
    COALESCE(SUM(cost_usd), 0)::double precision
FROM usage_event
WHERE project_id = $1 AND ts >= $2`

	args := []any{projectID, since}
	if tag != domain.WholeProjectTag {
		query += ` AND tag = $3`
		args = append(args, tag)
	}

	var totals domain.Totals
	err := q.QueryRow(ctx, query, args...).
		Scan(&totals.Calls, &totals.InputTokens, &totals.OutputTokens, &totals.CostUSD)
	if err != nil {
		return domain.Totals{}, fmt.Errorf("windowed totals for project %d:\n%w", projectID, err)
	}
	return totals, nil
}
