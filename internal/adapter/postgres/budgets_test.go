package postgres

import (
	"context"
	"math"
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestBudgetSetListDelete verifies upsert identity and project-isolated budget management.
func TestBudgetSetListDelete(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	first, _ := createAdmissionProject(t, ctx, store, "budget-first")
	second, _ := createAdmissionProject(t, ctx, store, "budget-second")

	initial := mustSetBudget(
		t,
		ctx,
		store,
		first.Name,
		governance.DimensionCalls,
		governance.WindowHour,
		5,
		governance.ActionBlock,
	)
	updated := mustSetBudget(
		t,
		ctx,
		store,
		first.Name,
		governance.DimensionCalls,
		governance.WindowHour,
		7,
		governance.ActionBlock,
	)
	other := mustSetBudget(
		t,
		ctx,
		store,
		second.Name,
		governance.DimensionCalls,
		governance.WindowHour,
		3,
		governance.ActionBlock,
	)

	if initial.ID != updated.ID || updated.MaxValue != 7 || updated.ProjectID != first.ID {
		t.Fatalf("upsert initial=%#v updated=%#v", initial, updated)
	}
	assertListedBudgets(t, ctx, store, first.Name, []int64{updated.ID})
	assertListedBudgets(t, ctx, store, second.Name, []int64{other.ID})
	assertListedBudgets(t, ctx, store, "missing-project", nil)

	if err := store.DeleteBudget(ctx, updated.ID); err != nil {
		t.Fatalf("DeleteBudget: %v", err)
	}
	assertListedBudgets(t, ctx, store, first.Name, nil)
	assertListedBudgets(t, ctx, store, second.Name, []int64{other.ID})
}

// TestBudgetRejectsInvalidMaximum verifies unsafe and non-integral maxima never reach storage.
func TestBudgetRejectsInvalidMaximum(t *testing.T) {
	ctx := context.Background()
	store := newGovernanceStore(t)
	project, _ := createAdmissionProject(t, ctx, store, "budget-validation")

	tests := []struct {
		name      string
		dimension governance.Dimension
		maximum   float64
	}{
		{"nan", governance.DimensionCost, math.NaN()},
		{"positive infinity", governance.DimensionCost, math.Inf(1)},
		{"negative infinity", governance.DimensionCost, math.Inf(-1)},
		{"negative", governance.DimensionCost, -0.01},
		{"fractional calls", governance.DimensionCalls, 1.5},
		{"fractional tokens", governance.DimensionTokens, 99.5},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.SetBudget(
				ctx,
				project.Name,
				test.dimension,
				governance.WindowHour,
				test.maximum,
				governance.ActionBlock,
			)
			if err == nil {
				t.Fatalf("SetBudget(%s, %v) succeeded", test.dimension, test.maximum)
			}
		})
	}

	cost := mustSetBudget(
		t,
		ctx,
		store,
		project.Name,
		governance.DimensionCost,
		governance.WindowDay,
		1.25,
		governance.ActionWarn,
	)
	if cost.MaxValue != 1.25 {
		t.Fatalf("fractional cost maximum = %v, want 1.25", cost.MaxValue)
	}
	assertListedBudgets(t, ctx, store, project.Name, []int64{cost.ID})
}

// assertListedBudgets verifies exact IDs and project ownership in list order.
func assertListedBudgets(
	t *testing.T,
	ctx context.Context,
	store *Store,
	project string,
	wantIDs []int64,
) {
	t.Helper()

	limits, err := store.ListBudgets(ctx, project)
	if err != nil {
		t.Fatalf("ListBudgets(%q): %v", project, err)
	}
	if len(limits) != len(wantIDs) {
		t.Fatalf("ListBudgets(%q) = %#v, want IDs %v", project, limits, wantIDs)
	}
	for index, wantID := range wantIDs {
		if limits[index].ID != wantID {
			t.Fatalf("ListBudgets(%q)[%d].ID = %d, want %d", project, index, limits[index].ID, wantID)
		}
	}
}
