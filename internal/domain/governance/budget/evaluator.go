package budget

import (
	"sort"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// Evaluate decides whether current rolling totals admit another generation request.
func Evaluate(
	limits []governance.BudgetLimit,
	totals map[governance.Window]governance.WindowTotals,
) governance.Admission {
	ordered := append([]governance.BudgetLimit(nil), limits...)
	sort.SliceStable(ordered, func(left, right int) bool {
		return ordered[left].ID < ordered[right].ID
	})

	admission := governance.Admission{Allowed: true}
	for _, limit := range ordered {
		current := totals[limit.Window]
		if !isBreached(limit, current) {
			continue
		}

		breach := governance.BudgetBreach{
			Limit:   limit,
			ResetAt: resetAt(limit.Dimension, current),
		}
		if limit.Action == governance.ActionWarn {
			admission.Warnings = append(admission.Warnings, breach)
			continue
		}

		admission.Allowed = false
		admission.Blocks = append(admission.Blocks, breach)
	}
	return admission
}

// isBreached reports whether one limit is exhausted or unsafe to evaluate.
func isBreached(limit governance.BudgetLimit, totals governance.WindowTotals) bool {
	switch limit.Dimension {
	case governance.DimensionCalls:
		return float64(totals.Calls) >= limit.MaxValue
	case governance.DimensionTokens:
		return totals.UnknownAccounting > 0 || float64(totals.Tokens) >= limit.MaxValue
	case governance.DimensionCost:
		return totals.UnknownAccounting > 0 ||
			totals.UnknownPricing > 0 ||
			totals.CostUSD >= limit.MaxValue
	default:
		return false
	}
}

// resetAt selects the reset timestamp belonging to a budget dimension.
func resetAt(dimension governance.Dimension, totals governance.WindowTotals) time.Time {
	switch dimension {
	case governance.DimensionCalls:
		return totals.CallsResetAt
	case governance.DimensionTokens:
		return totals.TokensResetAt
	case governance.DimensionCost:
		return totals.CostResetAt
	default:
		return time.Time{}
	}
}
