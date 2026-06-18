package postgres

import (
	"context"
	"testing"
	"time"
)

// TestPruneOlderThan proves the retention sweep removes only aged usage_event rows (older than the
// retention window) and only expired reservations, leaving recent usage and live reservations.
func TestPruneOlderThan(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	projectID, err := store.EnsureProject(ctx, "retention")
	if err != nil {
		t.Fatalf("EnsureProject: %v", err)
	}

	const retention = 35 * 24 * time.Hour
	now := time.Now().UTC()

	seedUsage(t, ctx, store, projectID, "news", now.Add(-40*24*time.Hour), 100, 40, 0.10) // older than retention
	seedUsage(t, ctx, store, projectID, "news", now.Add(-24*time.Hour), 200, 80, 0.20)    // recent

	if _, err := store.Reserve(ctx, projectID, "news", time.Minute); err != nil {
		t.Fatalf("Reserve(live): %v", err)
	}
	if _, err := store.Reserve(ctx, projectID, "news", -time.Hour); err != nil { // already expired
		t.Fatalf("Reserve(expired): %v", err)
	}

	usageDeleted, resvDeleted, err := store.PruneOlderThan(ctx, retention)
	if err != nil {
		t.Fatalf("PruneOlderThan: %v", err)
	}
	if usageDeleted != 1 {
		t.Errorf("usageDeleted = %d, want 1", usageDeleted)
	}
	if resvDeleted != 1 {
		t.Errorf("resvDeleted = %d, want 1", resvDeleted)
	}

	// Only the recent usage_event survives (wide window so the cutoff is the only filter).
	got, err := store.WindowedTotals(ctx, projectID, "news", now.Add(-100*24*time.Hour))
	if err != nil {
		t.Fatalf("WindowedTotals: %v", err)
	}
	if got.Calls != 1 {
		t.Errorf("surviving usage events = %d, want 1", got.Calls)
	}

	// Only the live reservation survives (countReservations is defined in budget_test.go).
	if remaining := countReservations(t, ctx, store, projectID); remaining != 1 {
		t.Errorf("surviving reservations = %d, want 1", remaining)
	}
}
