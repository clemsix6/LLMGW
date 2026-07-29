package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestUsageReportingCountsCallsFromRequestsAndAttemptsSeparately catches a reporting query
// mutation that counts joined attempts as calls, loses unknown accounting, or drops unpriced
// attempts from totals.
func TestUsageReportingCountsCallsFromRequestsAndAttemptsSeparately(t *testing.T) {
	store := newGovernanceStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	project, keyID := createAdmissionProject(t, ctx, store, "reporting")

	requestOne := seedRequestEvent(t, ctx, store, project.ID, keyID,
		governance.OperationGeneration, now.Add(-time.Hour),
		governance.RequestCompleted, governance.AccountingObserved)
	seedUsageAttempt(t, ctx, store, requestOne, 10, 0, floatPointer(0.25), governance.PricingPriced, now)
	seedFailedReportingAttempt(t, ctx, store, requestOne, 20, now)
	seedRequestEvent(t, ctx, store, project.ID, keyID,
		governance.OperationGeneration, now.Add(-30*time.Minute),
		governance.RequestCompleted, governance.AccountingUnknown)

	summaries, err := store.QueryUsage(ctx, governance.UsageQuery{
		Project: project.Name,
		Since:   now.Add(-2 * time.Hour),
		GroupBy: "key",
	})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("summary count = %d, want 1", len(summaries))
	}
	summary := summaries[0]
	if summary.Calls != 2 || summary.Tokens != 30 || summary.CostUSD != 0.25 ||
		summary.FailedAttempts != 1 || summary.UnknownPricing != 1 || summary.UnknownAccounting != 1 {
		t.Fatalf("summary = %#v, want calls=2 tokens=30 cost=.25 failed=1 unknown pricing=1 accounting=1", summary)
	}
}

// TestUsageReportingRejectsUnknownGrouping catches a mutation that interpolates an operator's
// --by value into SQL instead of selecting only an audited grouping expression.
func TestUsageReportingRejectsUnknownGrouping(t *testing.T) {
	store := newGovernanceStore(t)
	_, err := store.QueryUsage(context.Background(), governance.UsageQuery{
		Project: "anything", Since: time.Now().UTC(), GroupBy: "key; DROP TABLE project;--",
	})
	if err == nil {
		t.Fatal("QueryUsage accepted an unrecognised grouping")
	}
}

// TestUsageReportingUsesEmptyProviderForCallsWithoutAttempts catches a query mutation that
// leaves the provider group NULL and makes unknown-accounting-only reports unreadable.
func TestUsageReportingUsesEmptyProviderForCallsWithoutAttempts(t *testing.T) {
	store := newGovernanceStore(t)
	ctx := context.Background()
	project, keyID := createAdmissionProject(t, ctx, store, "provider-empty")
	seedRequestEvent(t, ctx, store, project.ID, keyID, governance.OperationGeneration,
		time.Now().UTC(), governance.RequestCompleted, governance.AccountingUnknown)

	summaries, err := store.QueryUsage(ctx, governance.UsageQuery{Project: project.Name, Since: time.Now().UTC().Add(-time.Hour), GroupBy: "provider"})
	if err != nil {
		t.Fatalf("QueryUsage: %v", err)
	}
	if len(summaries) != 1 || summaries[0].Group != "" || summaries[0].UnknownAccounting != 1 {
		t.Fatalf("provider summaries = %#v, want one empty group with unknown accounting", summaries)
	}
}

// seedFailedReportingAttempt inserts an unpriced failed attempt for the reporting aggregate.
func seedFailedReportingAttempt(t *testing.T, ctx context.Context, store *Store, requestID string, tokens int64, createdAt time.Time) {
	t.Helper()
	const query = `
INSERT INTO usage_attempt (
    id, request_id, provider, executor_type, resolved_model, requested_alias,
    upstream_auth_id, upstream_auth_type, input_tokens, output_tokens,
    reasoning_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
    unclassified_tokens, service_tier, response_service_tier, failed,
    latency_ms, ttft_ms, cost_usd, pricing_state, created_at
) VALUES (
    $1, $2, 'provider-a', 'test', 'model-a', 'alias', 'never-print',
    'auth-type', 0, 0, 0, 0, 0, $3, 0, 'standard', 'standard',
    true, 0, 0, NULL, 'unknown_pricing', $4
)`
	if _, err := store.pool.Exec(ctx, query, nextAdmissionUUID(), requestID, tokens, createdAt); err != nil {
		t.Fatalf("seed failed reporting attempt: %v", err)
	}
}
