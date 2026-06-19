package httpserver

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
	"github.com/clemsix6/LLMGW/internal/domain/budget"
	"github.com/clemsix6/LLMGW/internal/domain/llm"
)

const (
	// dimensionCalls, dimensionTokens, dimensionCostUSD mirror the budget_limit dimensions; they
	// select which gathered total to report as "current" in a 402 body.
	dimensionCalls   = "calls"
	dimensionTokens  = "tokens"
	dimensionCostUSD = "cost_usd"

	// windowDay is the daily sliding window; any other window value is treated as hourly.
	windowDay = "day"

	// reservationTTL bounds how long an in-flight reservation counts before it is reaped. It only
	// needs to outlive a single upstream call; a crashed request's reservation expires after it.
	reservationTTL = 2 * time.Minute

	// preCallInputTokens is the request's known input-token contribution to a tokens limit. V1 has
	// no pre-call tokenizer, so it is zero: the tokens dimension is enforced at crossing via the
	// recorded totals (output tokens are likewise only known once a call completes).
	preCallInputTokens = 0
)

// admit enforces the (project, tag)'s budget before forwarding. It returns a reservation id and
// true when the request may proceed (the id is 0 when no limits are configured and no reservation
// was taken), or writes a 402/5xx and returns false. The caller releases a non-zero reservation
// after forwarding.
func (h *handler) admit(w http.ResponseWriter, r *http.Request, req llm.Request, project string, projectID int64, tag string) (int64, bool) {
	ctx := r.Context()

	limits, err := h.store.LimitsFor(ctx, projectID, tag)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "load budget limits")
		return 0, false
	}
	if len(limits) == 0 {
		return 0, true
	}

	if h.blockUnknownModel(ctx, w, limits, project, tag, req.Model()) {
		return 0, false
	}

	return h.reserveOrBlock(ctx, w, limits, project, projectID, tag)
}

// blockUnknownModel fails closed: when a cost_usd limit applies but the model has no price row,
// the cost cannot be computed, so the request is blocked with a 402. Calls and tokens limits are
// unaffected by an unpriced model. It returns true when it wrote a response (block or error).
func (h *handler) blockUnknownModel(ctx context.Context, w http.ResponseWriter, limits []domain.BudgetLimit, project, tag, model string) bool {
	costLimit, hasCostLimit := firstCostLimit(limits)
	if !hasCostLimit {
		return false
	}

	_, _, priced, err := h.store.PriceFor(ctx, model)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "price lookup")
		return true
	}
	if priced {
		return false
	}

	writeBudgetExceeded(w, project, tag, costLimit, 0)
	return true
}

// reserveOrBlock runs the atomic admission: it groups the limits by window and tag scope, gathers
// each group's totals under the per-(project, tag) lock, and reserves a call only if no block
// limit is breached. On a block it writes the typed 402; on a store failure a 500.
func (h *handler) reserveOrBlock(ctx context.Context, w http.ResponseWriter, limits []domain.BudgetLimit, project string, projectID int64, tag string) (int64, bool) {
	groups := groupLimits(limits, tag, time.Now().UTC())

	var decision budget.Decision
	var current float64

	reservationID, admitted, err := h.store.ReserveIfAdmitted(ctx, projectID, tag, reservationTTL, readsOf(groups),
		func(totals []domain.WindowTotals) bool {
			decision, current = evaluateGroups(groups, totals)
			return !decision.Blocked
		})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", "budget admission")
		return 0, false
	}

	logWarnings(project, tag, decision.Warnings)
	if !admitted {
		writeBudgetExceeded(w, project, tag, decision.Limit, current)
		return 0, false
	}
	return reservationID, true
}

// release removes an in-flight reservation after a call. It uses a detached context so the
// reservation is freed even when the request context was cancelled (e.g. a streaming client
// disconnect); a failure is logged but harmless (the TTL reaps the row anyway).
func (h *handler) release(reservationID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := h.store.ReleaseReservation(ctx, reservationID); err != nil {
		log.Printf("llmgw: release reservation %d: %v", reservationID, err)
	}
}

// limitGroup is the set of limits sharing a (tag scope, window): they are evaluated together
// against the single totals snapshot read for that scope and window.
type limitGroup struct {
	limits []domain.BudgetLimit // limits is the subset of limits in this (scope, window) group.

	read domain.WindowRead // read identifies the totals to gather for the group.
}

// groupLimits buckets limits by (tag scope, window) so each bucket can be evaluated against the
// matching totals. Whole-project limits (nil tag) read the project-wide aggregate; concrete-tag
// limits read the request tag. Buckets keep the first-seen order (limits arrive ordered by id),
// so the first blocking group wins deterministically.
func groupLimits(limits []domain.BudgetLimit, requestTag string, now time.Time) []limitGroup {
	type key struct {
		wholeProject bool
		window       string
	}

	var groups []limitGroup
	index := make(map[key]int)

	for _, limit := range limits {
		k := key{wholeProject: limit.Tag == nil, window: limit.Window}

		i, seen := index[k]
		if !seen {
			groups = append(groups, limitGroup{read: domain.WindowRead{
				Tag:   scopeTag(k.wholeProject, requestTag),
				Since: now.Add(-windowDuration(limit.Window)),
			}})
			i = len(groups) - 1
			index[k] = i
		}
		groups[i].limits = append(groups[i].limits, limit)
	}
	return groups
}

// readsOf extracts each group's WindowRead, preserving order so a totals slice gathered from it
// maps back to the groups by index.
func readsOf(groups []limitGroup) []domain.WindowRead {
	reads := make([]domain.WindowRead, len(groups))
	for i, group := range groups {
		reads[i] = group.read
	}
	return reads
}

// evaluateGroups evaluates each group against its totals and returns the first blocking decision
// (groups are in first-seen order) plus the current usage in the breached dimension for the 402
// body. Warnings from every group are collected.
func evaluateGroups(groups []limitGroup, totals []domain.WindowTotals) (budget.Decision, float64) {
	var result budget.Decision
	var current float64

	for i, group := range groups {
		decision := budget.Evaluate(group.limits, totals[i].Current, totals[i].Inflight, preCallInputTokens)
		result.Warnings = append(result.Warnings, decision.Warnings...)

		if decision.Blocked && !result.Blocked {
			result.Blocked = true
			result.Limit = decision.Limit
			current = currentForDimension(decision.Limit.Dimension, totals[i])
		}
	}
	return result, current
}

// currentForDimension returns the usage already counted toward a limit's dimension (recorded plus
// in-flight, excluding this request), reported as "current" in the 402 body.
func currentForDimension(dimension string, totals domain.WindowTotals) float64 {
	switch dimension {
	case dimensionCalls:
		return float64(totals.Current.Calls + totals.Inflight.Calls)
	case dimensionTokens:
		recorded := totals.Current.InputTokens + totals.Current.OutputTokens
		reserved := totals.Inflight.InputTokens + totals.Inflight.OutputTokens
		return float64(recorded + reserved)
	case dimensionCostUSD:
		return totals.Current.CostUSD + totals.Inflight.CostUSD
	default:
		return 0
	}
}

// firstCostLimit returns the first cost_usd limit in the slice, reporting whether one was found.
func firstCostLimit(limits []domain.BudgetLimit) (domain.BudgetLimit, bool) {
	for _, limit := range limits {
		if limit.Dimension == dimensionCostUSD {
			return limit, true
		}
	}
	return domain.BudgetLimit{}, false
}

// scopeTag maps a group's scope to the tag to aggregate: the whole-project sentinel for nil-tag
// limits, otherwise the request tag.
func scopeTag(wholeProject bool, requestTag string) string {
	if wholeProject {
		return domain.WholeProjectTag
	}
	return requestTag
}

// windowDuration converts a limit's window to its sliding-window length: 24h for a day, 1h
// otherwise.
func windowDuration(window string) time.Duration {
	if window == windowDay {
		return 24 * time.Hour
	}
	return time.Hour
}

// logWarnings records breached warn-action limits for observability; they never block a request.
func logWarnings(project, tag string, warnings []domain.BudgetLimit) {
	for _, warning := range warnings {
		log.Printf("llmgw: budget warning (project=%q tag=%q dimension=%s window=%s limit=%v)",
			project, tag, warning.Dimension, warning.Window, warning.MaxValue)
	}
}
