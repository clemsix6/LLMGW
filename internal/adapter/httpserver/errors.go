package httpserver

import (
	"encoding/json"
	"net/http"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// budgetExceededType is the stable classifier consumers match to detect a budget block. It is a
// 402 (not 429) so agent SDKs do not treat it as a retryable provider rate limit.
const budgetExceededType = "budget_exceeded"

// budgetExceededBody is the JSON envelope returned with a 402 when a budget limit blocks a
// request. It names the exact (project, tag, dimension, window) limit that was hit and the
// current usage so the operator can see why the call was refused.
type budgetExceededBody struct {
	Error budgetExceededDetail `json:"error"` // Error holds the budget-block detail.
}

// budgetExceededDetail describes the budget limit that blocked a request.
type budgetExceededDetail struct {
	Type string `json:"type"` // Type is always budgetExceededType.

	Project string `json:"project"` // Project is the request's project name.

	Tag string `json:"tag"` // Tag is the request's budget bucket.

	Dimension string `json:"dimension"` // Dimension is the breached metric: calls, tokens, or cost_usd.

	Window string `json:"window"` // Window is the breached limit's window: hour or day.

	Limit float64 `json:"limit"` // Limit is the configured threshold for the dimension in the window.

	Current float64 `json:"current"` // Current is the usage already counted toward the limit.
}

// writeBudgetExceeded writes the typed 402 for a blocked request: the limit identifies the rule
// and current is the usage already counted in its dimension and window.
func writeBudgetExceeded(w http.ResponseWriter, project, tag string, limit domain.BudgetLimit, current float64) {
	body := budgetExceededBody{Error: budgetExceededDetail{
		Type:      budgetExceededType,
		Project:   project,
		Tag:       tag,
		Dimension: limit.Dimension,
		Window:    limit.Window,
		Limit:     limit.MaxValue,
		Current:   current,
	}}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(w).Encode(body)
}
