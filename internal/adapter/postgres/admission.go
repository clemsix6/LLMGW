package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/clemsix6/LLMGW/internal/domain/governance/budget"
)

// Confirm Store implements the governed request repository port.
var _ governance.RequestRepository = (*Store)(nil)

// AdmitGeneration atomically evaluates project budgets and records an allowed generation.
func (s *Store) AdmitGeneration(
	ctx context.Context,
	request governance.RequestEvent,
	now time.Time,
) (governance.Admission, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return governance.Admission{}, fmt.Errorf("begin generation admission:\n%w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockAdmissionProject(ctx, tx, request.ProjectID); err != nil {
		return governance.Admission{}, err
	}
	limits, err := loadAdmissionLimits(ctx, tx, request.ProjectID)
	if err != nil {
		return governance.Admission{}, err
	}
	totals, err := loadAdmissionTotals(ctx, tx, request.ProjectID, limits, now.UTC())
	if err != nil {
		return governance.Admission{}, err
	}

	admission := budget.Evaluate(limits, totals)
	if admission.Allowed {
		request = generationRequest(request)
		if err := insertRequestEvent(ctx, tx, request); err != nil {
			return governance.Admission{}, err
		}
		admission.Request = request
	}
	if err := tx.Commit(ctx); err != nil {
		return governance.Admission{}, fmt.Errorf("commit generation admission:\n%w", err)
	}
	return admission, nil
}

// RecordMetadata records an unmetered metadata request in flight.
func (s *Store) RecordMetadata(ctx context.Context, request governance.RequestEvent) error {
	request.Operation = governance.OperationMetadata
	request.State = governance.RequestInFlight
	request.AccountingState = governance.AccountingNotApplicable

	if err := insertRequestEvent(ctx, s.pool, request); err != nil {
		return fmt.Errorf("record metadata request:\n%w", err)
	}
	return nil
}

// CompleteRequest records downstream completion for only the named request UUID.
func (s *Store) CompleteRequest(
	ctx context.Context,
	requestID string,
	downstreamStatus int,
	completedAt time.Time,
) error {
	const query = `
UPDATE request_event
SET completed_at = $2, state = $3, downstream_status = $4
WHERE id = $1`
	if _, err := s.pool.Exec(
		ctx,
		query,
		requestID,
		completedAt,
		governance.RequestCompleted,
		downstreamStatus,
	); err != nil {
		return fmt.Errorf("complete request %q:\n%w", requestID, err)
	}
	return nil
}

// lockAdmissionProject acquires the transaction-scoped lock for one project.
func lockAdmissionProject(ctx context.Context, tx pgx.Tx, projectID int64) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, projectID); err != nil {
		return fmt.Errorf("lock admission for project %d:\n%w", projectID, err)
	}
	return nil
}

// loadAdmissionLimits reads project limits in deterministic database order.
func loadAdmissionLimits(
	ctx context.Context,
	tx pgx.Tx,
	projectID int64,
) ([]governance.BudgetLimit, error) {
	const query = `
SELECT id, project_id, dimension, "window", max_value, action, created_at, updated_at
FROM budget_limit
WHERE project_id = $1
ORDER BY id`
	rows, err := tx.Query(ctx, query, projectID)
	if err != nil {
		return nil, fmt.Errorf("load admission limits for project %d:\n%w", projectID, err)
	}
	defer rows.Close()

	var limits []governance.BudgetLimit
	for rows.Next() {
		limit, err := scanBudgetLimit(rows)
		if err != nil {
			return nil, fmt.Errorf("scan admission limit:\n%w", err)
		}
		limits = append(limits, limit)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate admission limits:\n%w", err)
	}
	return limits, nil
}

// loadAdmissionTotals aggregates each distinct rolling window required by project limits.
func loadAdmissionTotals(
	ctx context.Context,
	tx pgx.Tx,
	projectID int64,
	limits []governance.BudgetLimit,
	now time.Time,
) (map[governance.Window]governance.WindowTotals, error) {
	totals := make(map[governance.Window]governance.WindowTotals)
	for _, limit := range limits {
		if _, loaded := totals[limit.Window]; loaded {
			continue
		}
		windowTotals, err := aggregateAdmissionWindow(ctx, tx, projectID, limit.Window, now)
		if err != nil {
			return nil, err
		}
		totals[limit.Window] = windowTotals
	}
	return totals, nil
}

// aggregateAdmissionWindow reads totals and reset contributors for one rolling window.
func aggregateAdmissionWindow(
	ctx context.Context,
	tx pgx.Tx,
	projectID int64,
	window governance.Window,
	now time.Time,
) (governance.WindowTotals, error) {
	duration := admissionWindowDuration(window)
	since := now.Add(-duration)

	raw, err := queryAdmissionWindow(ctx, tx, projectID, since)
	if err != nil {
		return governance.WindowTotals{}, fmt.Errorf(
			"aggregate %s admission window for project %d:\n%w",
			window,
			projectID,
			err,
		)
	}
	return raw.windowTotals(now, duration), nil
}

// admissionWindowDuration returns the duration of a supported rolling window.
func admissionWindowDuration(window governance.Window) time.Duration {
	if window == governance.WindowDay {
		return 24 * time.Hour
	}
	return time.Hour
}

// admissionWindowRows holds aggregate values and nullable reset contributors.
type admissionWindowRows struct {
	calls             int64      // calls is the generation request count.
	tokens            int64      // tokens is the summed total-token count.
	costUSD           float64    // costUSD is the sum of priced attempt costs.
	unknownPricing    int64      // unknownPricing counts unpriced attempts.
	unknownAccounting int64      // unknownAccounting counts unresolved generation requests.
	callsAt           *time.Time // callsAt is the oldest counted generation request.
	tokensAt          *time.Time // tokensAt is the oldest token-bearing attempt.
	pricedAt          *time.Time // pricedAt is the oldest priced attempt.
	unknownPricingAt  *time.Time // unknownPricingAt is the oldest unpriced attempt.
	unknownAccountAt  *time.Time // unknownAccountAt is the oldest unresolved request.
}

// queryAdmissionWindow executes the parameterized aggregate for one cutoff.
func queryAdmissionWindow(
	ctx context.Context,
	tx pgx.Tx,
	projectID int64,
	since time.Time,
) (admissionWindowRows, error) {
	const query = `
WITH requests AS (
    SELECT
        count(*)::bigint AS calls,
        min(requested_at) AS calls_at,
        count(*) FILTER (WHERE accounting_state = 'accounting_unknown')::bigint AS unknown_accounting,
        min(requested_at) FILTER (WHERE accounting_state = 'accounting_unknown') AS unknown_account_at
    FROM request_event
    WHERE project_id = $1 AND operation = 'generation' AND requested_at >= $2
),
attempts AS (
    SELECT
        COALESCE(sum(a.total_tokens), 0)::bigint AS tokens,
        COALESCE(sum(a.cost_usd) FILTER (WHERE a.pricing_state = 'priced'), 0)::double precision AS cost_usd,
        count(*) FILTER (WHERE a.pricing_state = 'unknown_pricing')::bigint AS unknown_pricing,
        min(a.created_at) FILTER (WHERE a.total_tokens > 0) AS tokens_at,
        min(a.created_at) FILTER (WHERE a.pricing_state = 'priced') AS priced_at,
        min(a.created_at) FILTER (WHERE a.pricing_state = 'unknown_pricing') AS unknown_pricing_at
    FROM usage_attempt a
    JOIN request_event r ON r.id = a.request_id
    WHERE r.project_id = $1 AND r.operation = 'generation' AND a.created_at >= $2
)
SELECT r.calls, a.tokens, a.cost_usd, a.unknown_pricing, r.unknown_accounting,
       r.calls_at, a.tokens_at, a.priced_at, a.unknown_pricing_at, r.unknown_account_at
FROM requests r CROSS JOIN attempts a`

	var rows admissionWindowRows
	err := tx.QueryRow(ctx, query, projectID, since).Scan(
		&rows.calls,
		&rows.tokens,
		&rows.costUSD,
		&rows.unknownPricing,
		&rows.unknownAccounting,
		&rows.callsAt,
		&rows.tokensAt,
		&rows.pricedAt,
		&rows.unknownPricingAt,
		&rows.unknownAccountAt,
	)
	return rows, err
}

// windowTotals converts nullable contributors into dimension-specific reset timestamps.
func (rows admissionWindowRows) windowTotals(now time.Time, duration time.Duration) governance.WindowTotals {
	return governance.WindowTotals{
		Calls:             rows.calls,
		Tokens:            rows.tokens,
		CostUSD:           rows.costUSD,
		UnknownPricing:    rows.unknownPricing,
		UnknownAccounting: rows.unknownAccounting,
		CallsResetAt:      resetFrom(now, duration, rows.callsAt),
		TokensResetAt:     resetFrom(now, duration, earliestTime(rows.tokensAt, rows.unknownAccountAt)),
		CostResetAt: resetFrom(
			now,
			duration,
			earliestTime(rows.pricedAt, rows.unknownPricingAt, rows.unknownAccountAt),
		),
	}
}

// resetFrom adds the rolling duration to a contributor or returns now when none exists.
func resetFrom(now time.Time, duration time.Duration, contributor *time.Time) time.Time {
	if contributor == nil {
		return now
	}
	return contributor.Add(duration)
}

// earliestTime returns the earliest non-nil timestamp.
func earliestTime(values ...*time.Time) *time.Time {
	var earliest *time.Time
	for _, value := range values {
		if value != nil && (earliest == nil || value.Before(*earliest)) {
			copy := *value
			earliest = &copy
		}
	}
	return earliest
}

// generationRequest normalizes lifecycle fields owned by generation admission.
func generationRequest(request governance.RequestEvent) governance.RequestEvent {
	request.Operation = governance.OperationGeneration
	request.State = governance.RequestInFlight
	request.AccountingState = governance.AccountingPending
	return request
}

// requestEventWriter is implemented by a pool and a transaction.
type requestEventWriter interface {
	// Exec executes a statement that returns no rows.
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// insertRequestEvent persists every request event field.
func insertRequestEvent(
	ctx context.Context,
	writer requestEventWriter,
	request governance.RequestEvent,
) error {
	const query = `
INSERT INTO request_event (
    id, project_id, client_key_id, operation, requested_at, completed_at,
    method, path, requested_model, state, accounting_state, downstream_status,
    accounting_resolved_at
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`
	_, err := writer.Exec(
		ctx,
		query,
		request.ID,
		request.ProjectID,
		request.ClientKeyID,
		request.Operation,
		request.RequestedAt,
		request.CompletedAt,
		request.Method,
		request.Path,
		request.RequestedModel,
		request.State,
		request.AccountingState,
		request.DownstreamStatus,
		request.AccountingResolvedAt,
	)
	if err != nil {
		return fmt.Errorf("insert request event %q:\n%w", request.ID, err)
	}
	return nil
}
