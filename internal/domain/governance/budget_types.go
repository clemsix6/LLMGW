package governance

import "time"

// Dimension identifies the quantity controlled by a budget.
type Dimension string

const (
	// DimensionCalls controls the number of requests.
	DimensionCalls Dimension = "calls"
	// DimensionTokens controls the total observed tokens.
	DimensionTokens Dimension = "tokens"
	// DimensionCost controls the priced USD cost.
	DimensionCost Dimension = "cost"
)

// Window identifies a rolling budget time window.
type Window string

const (
	// WindowHour is a one-hour budget window.
	WindowHour Window = "hour"
	// WindowDay is a one-day budget window.
	WindowDay Window = "day"
)

// Action identifies how a budget breach affects admission.
type Action string

const (
	// ActionBlock rejects requests that breach the budget.
	ActionBlock Action = "block"
	// ActionWarn admits requests while reporting the breach.
	ActionWarn Action = "warn"
)

// BudgetLimit defines one project-wide governance limit.
type BudgetLimit struct {
	ID        int64     // ID is the database identifier.
	ProjectID int64     // ProjectID identifies the governed project.
	Dimension Dimension // Dimension identifies the governed quantity.
	Window    Window    // Window identifies the rolling time window.
	MaxValue  float64   // MaxValue is the maximum allowed quantity.
	Action    Action    // Action identifies the breach behavior.
	CreatedAt time.Time // CreatedAt is the UTC creation time.
	UpdatedAt time.Time // UpdatedAt is the UTC last-update time.
}

// WindowTotals contains current project usage and reset times.
type WindowTotals struct {
	Calls             int64     // Calls is the number of admitted generations.
	Tokens            int64     // Tokens is the total observed token count.
	CostUSD           float64   // CostUSD is the total priced cost in USD.
	UnknownPricing    int64     // UnknownPricing is the number of unpriced attempts.
	UnknownAccounting int64     // UnknownAccounting is the number of unresolved requests.
	CallsResetAt      time.Time // CallsResetAt is the UTC calls-window reset time.
	TokensResetAt     time.Time // TokensResetAt is the UTC tokens-window reset time.
	CostResetAt       time.Time // CostResetAt is the UTC cost-window reset time.
}

// BudgetBreach describes a breached limit and its reset time.
type BudgetBreach struct {
	Limit   BudgetLimit // Limit is the breached budget.
	ResetAt time.Time   // ResetAt is the UTC time at which the breach clears.
}

// Admission is the atomic result of generation admission.
type Admission struct {
	Allowed  bool           // Allowed reports whether generation may proceed.
	Request  RequestEvent   // Request is the persisted request event.
	Blocks   []BudgetBreach // Blocks contains budget breaches that deny admission.
	Warnings []BudgetBreach // Warnings contains non-blocking budget breaches.
}
