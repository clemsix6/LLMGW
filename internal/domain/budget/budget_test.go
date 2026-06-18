package budget

import (
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// lim builds a BudgetLimit for the table. A nil tag denotes a whole-project limit.
func lim(tag *string, dimension, window string, max float64, action string) domain.BudgetLimit {
	return domain.BudgetLimit{Tag: tag, Dimension: dimension, Window: window, MaxValue: max, Action: action}
}

// tagOf returns a pointer to s, for building concrete-tag limits in the table.
func tagOf(s string) *string { return &s }

// evalCase is one row of the Evaluate truth table.
type evalCase struct {
	name     string               // name describes the scenario.
	limits   []domain.BudgetLimit // limits are the configured limits, evaluated in order.
	current  domain.Totals        // current is the recorded windowed usage.
	inflight domain.Totals        // inflight is the in-flight reservation total.
	reqInput int                  // reqInput is this request's known input tokens.
	block    bool                 // block is the expected Decision.Blocked.
	blockIdx int                  // blockIdx indexes the expected blocking limit (-1 if none).
	warnIdxs []int                // warnIdxs index the expected warnings, in order.
}

func evalCases() []evalCase {
	return append(append(append(
		callsCases(),
		tokensCases()...),
		costCases()...),
		mixedCases()...)
}

// callsCases covers the calls dimension (exact pre-call, projected > max) over both windows.
func callsCases() []evalCase {
	return []evalCase{
		{
			name:     "empty limits allows",
			limits:   nil,
			block:    false,
			blockIdx: -1,
		},
		{
			name:     "calls hour under",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "calls", "hour", 3, "block")},
			current:  domain.Totals{Calls: 1},
			block:    false,
			blockIdx: -1,
		},
		{
			name:     "calls hour at limit allows (the Nth call)",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "calls", "hour", 3, "block")},
			current:  domain.Totals{Calls: 2},
			block:    false,
			blockIdx: -1,
		},
		{
			name:     "calls hour over blocks (the N+1th call)",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "calls", "hour", 3, "block")},
			current:  domain.Totals{Calls: 3},
			block:    true,
			blockIdx: 0,
		},
		{
			name:     "calls day over blocks, window carried",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "calls", "day", 5, "block")},
			current:  domain.Totals{Calls: 5},
			block:    true,
			blockIdx: 0,
		},
		{
			name:     "calls hour with inflight at limit allows",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "calls", "hour", 5, "block")},
			current:  domain.Totals{Calls: 2},
			inflight: domain.Totals{Calls: 2},
			block:    false,
			blockIdx: -1,
		},
		{
			name:     "calls hour with inflight over blocks",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "calls", "hour", 5, "block")},
			current:  domain.Totals{Calls: 2},
			inflight: domain.Totals{Calls: 3},
			block:    true,
			blockIdx: 0,
		},
	}
}

// tokensCases covers the tokens dimension (input known pre-call, projected > max) over windows.
func tokensCases() []evalCase {
	return []evalCase{
		{
			name:     "tokens hour under",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "tokens", "hour", 1000, "block")},
			current:  domain.Totals{InputTokens: 300, OutputTokens: 200},
			reqInput: 100,
			block:    false,
			blockIdx: -1,
		},
		{
			name:     "tokens hour at limit allows",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "tokens", "hour", 1000, "block")},
			current:  domain.Totals{InputTokens: 600, OutputTokens: 300},
			reqInput: 100,
			block:    false,
			blockIdx: -1,
		},
		{
			name:     "tokens hour over blocks (input pushes past)",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "tokens", "hour", 1000, "block")},
			current:  domain.Totals{InputTokens: 600, OutputTokens: 350},
			reqInput: 100,
			block:    true,
			blockIdx: 0,
		},
		{
			name:     "tokens day over blocks at crossing (recorded output past max)",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "tokens", "day", 1000, "block")},
			current:  domain.Totals{InputTokens: 700, OutputTokens: 400},
			reqInput: 0,
			block:    true,
			blockIdx: 0,
		},
		{
			name:     "tokens day counts inflight reservation tokens",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "tokens", "day", 1000, "block")},
			current:  domain.Totals{InputTokens: 300, OutputTokens: 200},
			inflight: domain.Totals{InputTokens: 300, OutputTokens: 200},
			reqInput: 100,
			block:    true,
			blockIdx: 0,
		},
	}
}

// costCases covers the cost_usd dimension (unknown pre-call, at-crossing, projected >= max).
func costCases() []evalCase {
	return []evalCase{
		{
			name:     "cost hour under",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "cost_usd", "hour", 1.0, "block")},
			current:  domain.Totals{CostUSD: 0.5},
			block:    false,
			blockIdx: -1,
		},
		{
			name:     "cost hour just below allows",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "cost_usd", "hour", 1.0, "block")},
			current:  domain.Totals{CostUSD: 0.99},
			block:    false,
			blockIdx: -1,
		},
		{
			name:     "cost hour at limit blocks (crossing reached)",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "cost_usd", "hour", 1.0, "block")},
			current:  domain.Totals{CostUSD: 1.0},
			block:    true,
			blockIdx: 0,
		},
		{
			name:     "cost day over blocks, window carried",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "cost_usd", "day", 1.0, "block")},
			current:  domain.Totals{CostUSD: 2.5},
			block:    true,
			blockIdx: 0,
		},
	}
}

// mixedCases covers warn vs block, multiple simultaneous limits, and whole-project limits.
func mixedCases() []evalCase {
	return []evalCase{
		{
			name:     "warn limit over never blocks",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "calls", "hour", 3, "warn")},
			current:  domain.Totals{Calls: 5},
			block:    false,
			blockIdx: -1,
			warnIdxs: []int{0},
		},
		{
			name:     "warn limit under raises nothing",
			limits:   []domain.BudgetLimit{lim(tagOf("news"), "calls", "hour", 3, "warn")},
			current:  domain.Totals{Calls: 1},
			block:    false,
			blockIdx: -1,
		},
		{
			name: "warn and block both over: block wins, warn surfaced",
			limits: []domain.BudgetLimit{
				lim(tagOf("news"), "calls", "hour", 3, "warn"),
				lim(tagOf("news"), "cost_usd", "hour", 1.0, "block"),
			},
			current:  domain.Totals{Calls: 5, CostUSD: 2.0},
			block:    true,
			blockIdx: 1,
			warnIdxs: []int{0},
		},
		{
			name: "multiple block limits over: first in order wins",
			limits: []domain.BudgetLimit{
				lim(tagOf("news"), "calls", "hour", 1, "block"),
				lim(tagOf("news"), "cost_usd", "hour", 0.1, "block"),
			},
			current:  domain.Totals{Calls: 5, CostUSD: 1.0},
			block:    true,
			blockIdx: 0,
		},
		{
			name: "first limit under, second block over: second wins",
			limits: []domain.BudgetLimit{
				lim(tagOf("news"), "calls", "hour", 100, "block"),
				lim(tagOf("news"), "cost_usd", "hour", 0.1, "block"),
			},
			current:  domain.Totals{Calls: 5, CostUSD: 1.0},
			block:    true,
			blockIdx: 1,
		},
		{
			name:     "whole-project (tag nil) calls over blocks, nil tag carried",
			limits:   []domain.BudgetLimit{lim(nil, "calls", "hour", 2, "block")},
			current:  domain.Totals{Calls: 5},
			block:    true,
			blockIdx: 0,
		},
		{
			name: "whole-project block beside an under tag limit",
			limits: []domain.BudgetLimit{
				lim(tagOf("news"), "calls", "hour", 100, "block"),
				lim(nil, "cost_usd", "day", 1.0, "block"),
			},
			current:  domain.Totals{Calls: 5, CostUSD: 2.0},
			block:    true,
			blockIdx: 1,
		},
	}
}

func TestEvaluate(t *testing.T) {
	for _, c := range evalCases() {
		t.Run(c.name, func(t *testing.T) {
			got := Evaluate(c.limits, c.current, c.inflight, c.reqInput)

			if got.Blocked != c.block {
				t.Fatalf("Blocked = %v, want %v", got.Blocked, c.block)
			}
			if c.block {
				assertSameLimit(t, "blocking limit", got.Limit, c.limits[c.blockIdx])
			}
			assertWarnings(t, got.Warnings, c.limits, c.warnIdxs)
		})
	}
}

// assertWarnings checks the decision's warnings match the limits at wantIdxs, in order.
func assertWarnings(t *testing.T, got []domain.BudgetLimit, limits []domain.BudgetLimit, wantIdxs []int) {
	t.Helper()

	if len(got) != len(wantIdxs) {
		t.Fatalf("warnings count = %d, want %d", len(got), len(wantIdxs))
	}
	for i, idx := range wantIdxs {
		assertSameLimit(t, "warning", got[i], limits[idx])
	}
}

// assertSameLimit fails unless two limits carry identical fields, including tag (by value).
func assertSameLimit(t *testing.T, what string, got, want domain.BudgetLimit) {
	t.Helper()

	if got.Dimension != want.Dimension || got.Window != want.Window ||
		got.MaxValue != want.MaxValue || got.Action != want.Action || !sameTag(got.Tag, want.Tag) {
		t.Errorf("%s = %+v, want %+v", what, got, want)
	}
}

// sameTag reports whether two tag pointers denote the same tag (both nil, or equal values).
func sameTag(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
