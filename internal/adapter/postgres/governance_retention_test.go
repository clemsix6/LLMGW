package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestPruneCompletedRequests catches retention that deletes active or legacy governance state.
func TestPruneCompletedRequests(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	now := time.Now().UTC()
	project, keyID := createAdmissionProject(t, ctx, store, "prune-completed")

	old := completedPendingRequest(t, ctx, store, project.ID, keyID, now.Add(-2*time.Hour))
	seedUsageAttempt(t, ctx, store, old, 4, 0, floatPointer(1), governance.PricingPriced, now.Add(-2*time.Hour))
	recent := completedPendingRequest(t, ctx, store, project.ID, keyID, now.Add(-30*time.Minute))
	inFlight := seedRequestEvent(
		t, ctx, store, project.ID, keyID, governance.OperationGeneration,
		now.Add(-2*time.Hour), governance.RequestInFlight, governance.AccountingPending,
	)
	seedRetentionProtectedRows(t, ctx, store, project.ID)
	protectedBefore := retentionProtectedCounts(t, ctx, store)

	deleted, err := store.PruneCompletedRequests(ctx, time.Hour)
	if err != nil {
		t.Fatalf("PruneCompletedRequests: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("PruneCompletedRequests deleted = %d, want 1", deleted)
	}
	assertRequestMissing(t, ctx, store, old)
	assertUsageAttemptCount(t, ctx, store, old, 0)
	assertRequestPresent(t, ctx, store, recent)
	assertRequestPresent(t, ctx, store, inFlight)
	if protectedAfter := retentionProtectedCounts(t, ctx, store); protectedAfter != protectedBefore {
		t.Fatalf("protected row counts = %#v, want %#v", protectedAfter, protectedBefore)
	}
}

// seedRetentionProtectedRows inserts legacy and configuration records the pruner must not touch.
func seedRetentionProtectedRows(t *testing.T, ctx context.Context, store *Store, projectID int64) {
	t.Helper()
	const budgetQuery = `
INSERT INTO budget_limit (project_id, dimension, "window", max_value, action)
VALUES ($1, 'calls', 'hour', 1, 'warn')`
	const legacyUsageQuery = `
INSERT INTO legacy_usage_event (project_id, tag, model, provider, status)
VALUES ($1, 'legacy', 'legacy-model', 'legacy-provider', 'ok')`
	const legacyBudgetQuery = `
INSERT INTO legacy_budget_limit (project_id, tag, dimension, "window", max_value, action)
VALUES ($1, 'legacy', 'calls', 'hour', 1, 'warn')`
	for _, query := range []string{budgetQuery, legacyUsageQuery, legacyBudgetQuery} {
		if _, err := store.pool.Exec(ctx, query, projectID); err != nil {
			t.Fatalf("seed retention protected rows: %v", err)
		}
	}
}

// retentionCounts records each table that governance request retention must leave unchanged.
type retentionCounts struct {
	clientKeys        int64
	projects          int64
	budgets           int64
	prices            int64
	legacyUsageEvents int64
	legacyBudgets     int64
}

// retentionProtectedCounts loads every non-request table protected from this retention sweep.
func retentionProtectedCounts(t *testing.T, ctx context.Context, store *Store) retentionCounts {
	t.Helper()
	var counts retentionCounts
	const query = `
SELECT
    (SELECT count(*) FROM client_key),
    (SELECT count(*) FROM project),
    (SELECT count(*) FROM budget_limit),
    (SELECT count(*) FROM model_price),
    (SELECT count(*) FROM legacy_usage_event),
    (SELECT count(*) FROM legacy_budget_limit)`
	if err := store.pool.QueryRow(ctx, query).Scan(
		&counts.clientKeys,
		&counts.projects,
		&counts.budgets,
		&counts.prices,
		&counts.legacyUsageEvents,
		&counts.legacyBudgets,
	); err != nil {
		t.Fatalf("count retention protected rows: %v", err)
	}
	return counts
}

// assertRequestMissing verifies a pruned request tree no longer has a request row.
func assertRequestMissing(t *testing.T, ctx context.Context, store *Store, requestID string) {
	t.Helper()
	var count int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM request_event WHERE id = $1`, requestID).Scan(&count); err != nil {
		t.Fatalf("count pruned request: %v", err)
	}
	if count != 0 {
		t.Fatalf("pruned request count = %d, want 0", count)
	}
}

// assertRequestPresent verifies retention keeps a request row.
func assertRequestPresent(t *testing.T, ctx context.Context, store *Store, requestID string) {
	t.Helper()
	var count int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM request_event WHERE id = $1`, requestID).Scan(&count); err != nil {
		t.Fatalf("count retained request: %v", err)
	}
	if count != 1 {
		t.Fatalf("retained request count = %d, want 1", count)
	}
}

// assertUsageAttemptCount verifies PostgreSQL cascade behavior for one request tree.
func assertUsageAttemptCount(t *testing.T, ctx context.Context, store *Store, requestID string, want int64) {
	t.Helper()
	var count int64
	if err := store.pool.QueryRow(ctx, `SELECT count(*) FROM usage_attempt WHERE request_id = $1`, requestID).Scan(&count); err != nil {
		t.Fatalf("count request attempts: %v", err)
	}
	if count != want {
		t.Fatalf("usage attempt count = %d, want %d", count, want)
	}
}
