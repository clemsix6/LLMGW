package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/jackc/pgx/v5"
)

// validateUsageCorrelation locks and validates the authenticated request/key
// parent before any attempt or parent mutation.
func validateUsageCorrelation(
	ctx context.Context,
	tx pgx.Tx,
	attempt governance.UsageAttempt,
) error {
	const query = `
SELECT true
FROM request_event r
JOIN client_key ck ON ck.id = r.client_key_id
WHERE r.id = $1 AND ck.public_id = $2
FOR KEY SHARE OF r, ck`
	var valid bool
	err := tx.QueryRow(ctx, query, attempt.RequestID, attempt.ClientKeyPublicID).Scan(&valid)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && !valid {
		return fmt.Errorf(
			"validate usage correlation for request %q:\n%w",
			attempt.RequestID,
			governance.ErrUsageCorrelation,
		)
	}
	if err != nil {
		return fmt.Errorf("validate usage correlation for request %q:\n%w", attempt.RequestID, err)
	}
	return nil
}

// insertUsageAttempt inserts one idempotent normalized attempt.
func insertUsageAttempt(
	ctx context.Context,
	tx pgx.Tx,
	attempt governance.UsageAttempt,
) (bool, error) {
	const query = `
INSERT INTO usage_attempt (
    id, request_id, provider, executor_type, resolved_model, requested_alias,
    upstream_auth_id, upstream_auth_type, input_tokens, output_tokens,
    reasoning_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
    unclassified_tokens, service_tier, response_service_tier, failed,
    upstream_status, latency_ms, ttft_ms, cost_usd, pricing_state, created_at
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
    $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24
)
ON CONFLICT (id) DO NOTHING`
	tag, err := tx.Exec(
		ctx,
		query,
		attempt.ID,
		attempt.RequestID,
		attempt.Provider,
		attempt.ExecutorType,
		attempt.ResolvedModel,
		attempt.RequestedAlias,
		attempt.UpstreamAuthID,
		attempt.UpstreamAuthType,
		attempt.Tokens.UncachedInput,
		attempt.Tokens.Output,
		attempt.Tokens.Reasoning,
		attempt.Tokens.CacheRead,
		attempt.Tokens.CacheCreation,
		attempt.Tokens.Total,
		attempt.Tokens.Unclassified,
		attempt.ServiceTier,
		attempt.ResponseServiceTier,
		attempt.Failed,
		attempt.UpstreamStatus,
		attempt.Latency.Milliseconds(),
		attempt.TTFT.Milliseconds(),
		attempt.CostUSD,
		attempt.PricingState,
		attempt.CreatedAt,
	)
	if err != nil {
		return false, fmt.Errorf("insert usage attempt %q:\n%w", attempt.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// observeAttemptParent marks real late generation usage as observed.
func observeAttemptParent(
	ctx context.Context,
	tx pgx.Tx,
	attempt governance.UsageAttempt,
) error {
	const query = `
UPDATE request_event
SET accounting_state = 'observed',
    requested_model = COALESCE(requested_model, $2),
    accounting_resolved_at = NULL
WHERE id = $1 AND operation = 'generation'`
	if _, err := tx.Exec(ctx, query, attempt.RequestID, attempt.RequestedAlias); err != nil {
		return fmt.Errorf("observe usage parent %q:\n%w", attempt.RequestID, err)
	}
	return nil
}
