package postgres

import (
	"context"
	"fmt"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/jackc/pgx/v5"
)

// Confirm Store implements the usage reporting port.
var _ governance.UsageRepository = (*Store)(nil)

var groupColumns = map[string]string{
	"key":      "client_key.name",
	"model":    "COALESCE(usage_attempt.resolved_model, request_event.requested_model, '')",
	"provider": "usage_attempt.provider",
}

// QueryUsage returns grouped generation calls and their normalized attempt accounting.
func (s *Store) QueryUsage(ctx context.Context, query governance.UsageQuery) ([]governance.UsageSummary, error) {
	groupColumn, ok := groupColumns[query.GroupBy]
	if !ok {
		return nil, fmt.Errorf("query usage: unsupported grouping %q", query.GroupBy)
	}
	rows, err := s.pool.Query(ctx, usageReportingSQL(groupColumn), query.Project, query.Since.UTC())
	if err != nil {
		return nil, fmt.Errorf("query usage for project %q:\n%w", query.Project, err)
	}
	defer rows.Close()
	return scanUsageSummaries(rows)
}

// ProjectExists reports whether the exact named project has already been created.
func (s *Store) ProjectExists(ctx context.Context, name string) (bool, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM project WHERE name = $1)`, name).Scan(&exists); err != nil {
		return false, fmt.Errorf("check project %q:\n%w", name, err)
	}
	return exists, nil
}

// usageReportingSQL embeds only an audited expression selected from groupColumns.
func usageReportingSQL(groupColumn string) string {
	return fmt.Sprintf(`
SELECT COALESCE(%s, ''),
       COUNT(DISTINCT request_event.id)::bigint,
       COALESCE(SUM(usage_attempt.total_tokens), 0)::bigint,
       COALESCE(SUM(usage_attempt.cost_usd), 0)::double precision,
       COUNT(usage_attempt.id) FILTER (WHERE usage_attempt.failed)::bigint,
       COUNT(usage_attempt.id) FILTER (WHERE usage_attempt.pricing_state = 'unknown_pricing')::bigint,
       COUNT(DISTINCT request_event.id) FILTER (WHERE request_event.accounting_state = 'accounting_unknown')::bigint
FROM request_event
JOIN project ON project.id = request_event.project_id
JOIN client_key ON client_key.id = request_event.client_key_id
LEFT JOIN usage_attempt ON usage_attempt.request_id = request_event.id
WHERE project.name = $1
  AND request_event.operation = 'generation'
  AND request_event.requested_at >= $2
GROUP BY COALESCE(%s, '')
ORDER BY COALESCE(%s, '')`, groupColumn, groupColumn, groupColumn)
}

// scanUsageSummaries converts each database reporting row into its non-secret representation.
func scanUsageSummaries(rows pgx.Rows) ([]governance.UsageSummary, error) {
	var summaries []governance.UsageSummary
	for rows.Next() {
		var summary governance.UsageSummary
		if err := rows.Scan(&summary.Group, &summary.Calls, &summary.Tokens, &summary.CostUSD,
			&summary.FailedAttempts, &summary.UnknownPricing, &summary.UnknownAccounting); err != nil {
			return nil, fmt.Errorf("scan usage summary:\n%w", err)
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage summaries:\n%w", err)
	}
	return summaries, nil
}
