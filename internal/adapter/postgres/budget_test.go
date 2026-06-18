package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// TestLimitsFor proves LimitsFor returns the concrete-tag rows plus the whole-project rows
// (tag IS NULL) for a project, and excludes limits configured for a different tag.
func TestLimitsFor(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	projectID, err := store.EnsureProject(ctx, "limits")
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	news := "news"
	feed := "feed"
	insertLimit(t, ctx, store, projectID, &news, "calls", "hour", 100, "block") // concrete: matches
	insertLimit(t, ctx, store, projectID, nil, "cost_usd", "day", 5, "block")   // whole-project: matches
	insertLimit(t, ctx, store, projectID, &feed, "tokens", "hour", 9, "warn")   // other tag: excluded

	limits, err := store.LimitsFor(ctx, projectID, news)
	if err != nil {
		t.Fatalf("LimitsFor: %v", err)
	}

	if len(limits) != 2 {
		t.Fatalf("LimitsFor returned %d limits, want 2 (concrete + whole-project): %+v", len(limits), limits)
	}
	assertHasLimit(t, limits, &news, "calls", "hour")
	assertHasLimit(t, limits, nil, "cost_usd", "day")
}

// TestReserveInflightRelease proves a reservation is counted by InflightTotals (Calls only) and
// removed by ReleaseReservation.
func TestReserveInflightRelease(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	projectID, err := store.EnsureProject(ctx, "reservations")
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	id, err := store.Reserve(ctx, projectID, "news", 2*time.Minute)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	assertInflightCalls(t, ctx, store, projectID, "news", 1)

	if _, err := store.Reserve(ctx, projectID, "news", 2*time.Minute); err != nil {
		t.Fatalf("Reserve (second): %v", err)
	}
	assertInflightCalls(t, ctx, store, projectID, "news", 2)

	// A reservation reserves only the Calls dimension; tokens and cost stay zero.
	totals, err := store.InflightTotals(ctx, projectID, "news")
	if err != nil {
		t.Fatalf("InflightTotals: %v", err)
	}
	if totals.InputTokens != 0 || totals.OutputTokens != 0 || totals.CostUSD != 0 {
		t.Errorf("InflightTotals non-call dimensions = %+v, want all zero", totals)
	}

	if err := store.ReleaseReservation(ctx, id); err != nil {
		t.Fatalf("ReleaseReservation: %v", err)
	}
	assertInflightCalls(t, ctx, store, projectID, "news", 1)
}

// TestInflightPrunesExpired proves InflightTotals drops expired reservations from the count and
// deletes their rows, while keeping non-expired ones.
func TestInflightPrunesExpired(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	projectID, err := store.EnsureProject(ctx, "expiry")
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	// Negative TTL: expires_at is in the past, so this reservation is already expired.
	if _, err := store.Reserve(ctx, projectID, "news", -time.Minute); err != nil {
		t.Fatalf("Reserve (expired): %v", err)
	}
	if _, err := store.Reserve(ctx, projectID, "news", 2*time.Minute); err != nil {
		t.Fatalf("Reserve (live): %v", err)
	}

	assertInflightCalls(t, ctx, store, projectID, "news", 1) // only the live one
	if n := countReservations(t, ctx, store, projectID); n != 1 {
		t.Errorf("reservation rows after prune = %d, want 1 (expired row deleted)", n)
	}
}

// TestInflightWholeProject proves the WholeProjectTag sentinel aggregates reservations across
// every tag of the project.
func TestInflightWholeProject(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	projectID, err := store.EnsureProject(ctx, "whole")
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	if _, err := store.Reserve(ctx, projectID, "news", time.Minute); err != nil {
		t.Fatalf("Reserve news: %v", err)
	}
	if _, err := store.Reserve(ctx, projectID, "feed", time.Minute); err != nil {
		t.Fatalf("Reserve feed: %v", err)
	}

	assertInflightCalls(t, ctx, store, projectID, "news", 1)
	assertInflightCalls(t, ctx, store, projectID, domain.WholeProjectTag, 2)
}

// insertLimit writes one budget_limit row; a nil tag stores a whole-project (NULL) limit.
func insertLimit(t *testing.T, ctx context.Context, store *Store, projectID int64, tag *string, dimension, window string, max float64, action string) {
	t.Helper()

	const query = `
INSERT INTO budget_limit (project_id, tag, dimension, "window", max_value, action)
VALUES ($1, $2, $3, $4, $5, $6)`

	if _, err := store.pool.Exec(ctx, query, projectID, tag, dimension, window, max, action); err != nil {
		t.Fatalf("insert budget_limit: %v", err)
	}
}

// assertHasLimit fails unless limits contains exactly one row matching the tag, dimension, and window.
func assertHasLimit(t *testing.T, limits []domain.BudgetLimit, tag *string, dimension, window string) {
	t.Helper()

	for _, limit := range limits {
		if limit.Dimension == dimension && limit.Window == window && tagsEqual(limit.Tag, tag) {
			return
		}
	}
	t.Errorf("limits %+v missing (tag=%v, dimension=%s, window=%s)", limits, tag, dimension, window)
}

// tagsEqual reports whether two tag pointers denote the same tag (both nil, or equal values).
func tagsEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// assertInflightCalls fails unless InflightTotals reports want in-flight calls for the (project, tag).
func assertInflightCalls(t *testing.T, ctx context.Context, store *Store, projectID int64, tag string, want int64) {
	t.Helper()

	totals, err := store.InflightTotals(ctx, projectID, tag)
	if err != nil {
		t.Fatalf("InflightTotals(tag=%q): %v", tag, err)
	}
	if totals.Calls != want {
		t.Errorf("InflightTotals(tag=%q).Calls = %d, want %d", tag, totals.Calls, want)
	}
}

// countReservations returns the number of reservation rows for the project.
func countReservations(t *testing.T, ctx context.Context, store *Store, projectID int64) int64 {
	t.Helper()

	var n int64
	if err := store.pool.QueryRow(ctx, `SELECT COUNT(*) FROM reservation WHERE project_id = $1`, projectID).Scan(&n); err != nil {
		t.Fatalf("count reservations: %v", err)
	}
	return n
}
