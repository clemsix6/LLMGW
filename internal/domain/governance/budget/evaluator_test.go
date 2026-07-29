package budget

import (
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// TestEvaluate verifies every budget dimension at its admission boundary.
func TestEvaluate(t *testing.T) {
	hourReset := time.Date(2026, 7, 27, 11, 15, 0, 0, time.UTC)
	dayReset := time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)

	tests := []struct {
		name      string
		limit     governance.BudgetLimit
		totals    governance.WindowTotals
		blocked   bool
		warned    bool
		wantReset time.Time
	}{
		{"call below cap", testLimit(1, governance.DimensionCalls, 5, governance.ActionBlock), testTotals(4, 0, 0, hourReset, time.Time{}, time.Time{}), false, false, time.Time{}},
		{"call at cap", testLimit(1, governance.DimensionCalls, 5, governance.ActionBlock), testTotals(5, 0, 0, hourReset, time.Time{}, time.Time{}), true, false, hourReset},
		{"tokens at cap", testLimit(1, governance.DimensionTokens, 100, governance.ActionBlock), testTotals(0, 100, 0, time.Time{}, hourReset, time.Time{}), true, false, hourReset},
		{"cost at cap", testLimit(1, governance.DimensionCost, 2.5, governance.ActionBlock), testTotals(0, 0, 2.5, time.Time{}, time.Time{}, hourReset), true, false, hourReset},
		{"unknown accounting blocks tokens", testLimit(1, governance.DimensionTokens, 100, governance.ActionBlock), unknownAccountingTotals(dayReset), true, false, dayReset},
		{"unknown accounting blocks cost", testLimit(1, governance.DimensionCost, 2.5, governance.ActionBlock), unknownAccountingTotals(dayReset), true, false, dayReset},
		{"unknown pricing blocks cost", testLimit(1, governance.DimensionCost, 2.5, governance.ActionBlock), unknownPricingTotals(dayReset), true, false, dayReset},
		{"unknown pricing does not block tokens", testLimit(1, governance.DimensionTokens, 100, governance.ActionBlock), unknownPricingTotals(dayReset), false, false, time.Time{}},
		{"calls ignore unknown accounting", testLimit(1, governance.DimensionCalls, 5, governance.ActionBlock), unknownAccountingTotals(dayReset), false, false, time.Time{}},
		{"calls ignore unknown pricing", testLimit(1, governance.DimensionCalls, 5, governance.ActionBlock), unknownPricingTotals(dayReset), false, false, time.Time{}},
		{"warning never blocks", testLimit(1, governance.DimensionCalls, 1, governance.ActionWarn), testTotals(1, 0, 0, hourReset, time.Time{}, time.Time{}), false, true, hourReset},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			totals := map[governance.Window]governance.WindowTotals{
				test.limit.Window: test.totals,
			}

			got := Evaluate([]governance.BudgetLimit{test.limit}, totals)

			if got.Allowed == test.blocked {
				t.Fatalf("Allowed = %v, want %v", got.Allowed, !test.blocked)
			}
			assertBreach(t, got.Blocks, test.blocked, test.limit, test.wantReset)
			assertBreach(t, got.Warnings, test.warned, test.limit, test.wantReset)
		})
	}
}

// TestEvaluateUsesEachRollingWindow verifies hour and day totals do not leak into each other.
func TestEvaluateUsesEachRollingWindow(t *testing.T) {
	hourReset := time.Date(2026, 7, 27, 11, 15, 0, 0, time.UTC)
	dayReset := time.Date(2026, 7, 28, 9, 30, 0, 0, time.UTC)
	limits := []governance.BudgetLimit{
		{ID: 3, Dimension: governance.DimensionCalls, Window: governance.WindowHour, MaxValue: 5, Action: governance.ActionBlock},
		{ID: 4, Dimension: governance.DimensionCalls, Window: governance.WindowDay, MaxValue: 10, Action: governance.ActionWarn},
	}
	totals := map[governance.Window]governance.WindowTotals{
		governance.WindowHour: testTotals(4, 0, 0, hourReset, time.Time{}, time.Time{}),
		governance.WindowDay:  testTotals(10, 0, 0, dayReset, time.Time{}, time.Time{}),
	}

	got := Evaluate(limits, totals)

	if !got.Allowed || len(got.Blocks) != 0 {
		t.Fatalf("admission = %#v, want allowed with no blocks", got)
	}
	assertBreach(t, got.Warnings, true, limits[1], dayReset)
}

// TestEvaluateOrdersBreachesByDatabaseID verifies the first block is deterministic.
func TestEvaluateOrdersBreachesByDatabaseID(t *testing.T) {
	reset := time.Date(2026, 7, 27, 11, 15, 0, 0, time.UTC)
	limits := []governance.BudgetLimit{
		{ID: 9, Dimension: governance.DimensionTokens, Window: governance.WindowHour, MaxValue: 10, Action: governance.ActionBlock},
		{ID: 2, Dimension: governance.DimensionCalls, Window: governance.WindowHour, MaxValue: 1, Action: governance.ActionBlock},
	}
	totals := map[governance.Window]governance.WindowTotals{
		governance.WindowHour: testTotals(1, 10, 0, reset, reset, time.Time{}),
	}

	got := Evaluate(limits, totals)

	if got.Allowed || len(got.Blocks) != 2 {
		t.Fatalf("admission = %#v, want two blocks", got)
	}
	if got.Blocks[0].Limit.ID != 2 || got.Blocks[1].Limit.ID != 9 {
		t.Fatalf("block IDs = [%d %d], want [2 9]", got.Blocks[0].Limit.ID, got.Blocks[1].Limit.ID)
	}
}

// testLimit returns one hour budget limit for evaluator table tests.
func testLimit(id int64, dimension governance.Dimension, maximum float64, action governance.Action) governance.BudgetLimit {
	return governance.BudgetLimit{
		ID:        id,
		Dimension: dimension,
		Window:    governance.WindowHour,
		MaxValue:  maximum,
		Action:    action,
	}
}

// testTotals returns literal window usage and reset values for evaluator tests.
func testTotals(
	calls int64,
	tokens int64,
	cost float64,
	callsReset time.Time,
	tokensReset time.Time,
	costReset time.Time,
) governance.WindowTotals {
	return governance.WindowTotals{
		Calls:         calls,
		Tokens:        tokens,
		CostUSD:       cost,
		CallsResetAt:  callsReset,
		TokensResetAt: tokensReset,
		CostResetAt:   costReset,
	}
}

// unknownAccountingTotals returns one unresolved request with literal reset values.
func unknownAccountingTotals(reset time.Time) governance.WindowTotals {
	return governance.WindowTotals{
		UnknownAccounting: 1,
		TokensResetAt:     reset,
		CostResetAt:       reset,
	}
}

// unknownPricingTotals returns one unpriced attempt with a literal cost reset.
func unknownPricingTotals(reset time.Time) governance.WindowTotals {
	return governance.WindowTotals{
		UnknownPricing: 1,
		CostResetAt:    reset,
	}
}

// assertBreach verifies whether a breach list contains the one expected limit and reset.
func assertBreach(
	t *testing.T,
	breaches []governance.BudgetBreach,
	present bool,
	wantLimit governance.BudgetLimit,
	wantReset time.Time,
) {
	t.Helper()

	if !present {
		if len(breaches) != 0 {
			t.Fatalf("breaches = %#v, want none", breaches)
		}
		return
	}
	if len(breaches) != 1 {
		t.Fatalf("breaches = %#v, want one", breaches)
	}
	if breaches[0].Limit != wantLimit || !breaches[0].ResetAt.Equal(wantReset) {
		t.Fatalf("breach = %#v, want limit %#v reset %v", breaches[0], wantLimit, wantReset)
	}
}
