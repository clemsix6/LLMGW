package budget

import "github.com/clemsix6/LLMGW/internal/domain"

// Dimension and action identifiers, mirrored from the budget_limit CHECK constraints (schema §8).
const (
	dimensionCalls   = "calls"    // dimensionCalls counts requests.
	dimensionTokens  = "tokens"   // dimensionTokens counts input + output tokens.
	dimensionCostUSD = "cost_usd" // dimensionCostUSD sums the notional cost in US dollars.

	actionWarn = "warn" // actionWarn records the breach but never blocks the request.
)

// Decision is the outcome of evaluating a (project, tag)'s budget limits against the current
// windowed usage, the in-flight reservations, and the incoming request. The HTTP handler uses
// it to decide whether to forward the request and what to put in a 402 body.
type Decision struct {
	// Blocked reports whether a block-action limit was breached; the request must not be forwarded.
	Blocked bool

	// Limit is the block-action limit that set Blocked. It is the zero value when Blocked is
	// false. Its Tag, Dimension, Window, and MaxValue populate the 402 body.
	Limit domain.BudgetLimit

	// Warnings lists the warn-action limits that were breached. A warn limit never sets Blocked;
	// it is surfaced for logging and observability only.
	Warnings []domain.BudgetLimit
}

// Evaluate decides whether a request may proceed under the configured limits, given the current
// windowed totals, the in-flight reservation totals, and this request's known input tokens.
//
// It is a pure comparison: every limit in limits is checked against the single current/inflight
// totals provided. The window and tag on each limit are metadata only — they are not used to
// select totals. The caller MUST therefore pass totals that match the window and tag scope of
// the limits it passes (the handler groups limits per window and tag scope and calls Evaluate
// once per group).
//
// The first breached block-action limit wins deterministically (in slice order); warn-action
// breaches are always collected. An empty limits slice allows the request.
func Evaluate(limits []domain.BudgetLimit, current, inflight domain.Totals, reqInputTokens int) Decision {
	var decision Decision

	for _, limit := range limits {
		if exceeds(limit, current, inflight, reqInputTokens) {
			decision.record(limit)
		}
	}

	return decision
}

// record applies a breached limit to the decision: warn limits append to Warnings, while the
// first block limit sets Blocked (later block limits do not override it — first blocking wins).
func (d *Decision) record(limit domain.BudgetLimit) {
	if limit.Action == actionWarn {
		d.Warnings = append(d.Warnings, limit)
		return
	}

	if !d.Blocked {
		d.Blocked = true
		d.Limit = limit
	}
}

// exceeds reports whether limit's threshold is breached by the projected total in its window.
//
// The projection is current usage + in-flight reservations + this request's known pre-call
// contribution. Pre-call dimensions (calls, input tokens) breach strictly (projected > max):
// the contribution is counted exactly before forwarding. The cost dimension is unknown pre-call
// and breaches at crossing (projected >= max): the call that crosses the limit completes and is
// recorded, and the next call is then blocked. Output tokens are likewise unknown pre-call, so
// the tokens dimension enforces its output portion at crossing (via the recorded total).
func exceeds(limit domain.BudgetLimit, current, inflight domain.Totals, reqInputTokens int) bool {
	switch limit.Dimension {
	case dimensionCalls:
		projected := float64(current.Calls + inflight.Calls + 1)
		return projected > limit.MaxValue

	case dimensionTokens:
		return tokensProjection(current, inflight, reqInputTokens) > limit.MaxValue

	case dimensionCostUSD:
		return current.CostUSD+inflight.CostUSD >= limit.MaxValue

	default:
		// Unknown dimension: impossible under the schema CHECK, so never block on it.
		return false
	}
}

// tokensProjection returns the projected total-token count for a tokens limit: the recorded
// input + output tokens, plus any in-flight reservation tokens, plus this request's known input
// tokens. This request's output tokens are unknown before the call completes and so are not
// projected here — they are enforced at crossing once recorded.
func tokensProjection(current, inflight domain.Totals, reqInputTokens int) float64 {
	recorded := current.InputTokens + current.OutputTokens
	reserved := inflight.InputTokens + inflight.OutputTokens

	return float64(recorded+reserved) + float64(reqInputTokens)
}
