package domain

import (
	"context"
	"net/http"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/llm"
	"github.com/clemsix6/LLMGW/internal/domain/usage"
)

// Clock abstracts the current time so windowed budget logic can be tested deterministically.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
}

// BudgetLimit is a single configured spending limit for a (project, tag).
type BudgetLimit struct {
	Tag *string // Tag is the bucket the limit applies to; nil means the whole project.

	Dimension string // Dimension is the metered quantity: "calls", "tokens", or "cost_usd".

	Window string // Window is the sliding window: "hour" or "day".

	MaxValue float64 // MaxValue is the threshold for the dimension within the window.

	Action string // Action is "block" (reject) or "warn" (record only).
}

// Totals is an aggregate of metered usage over a window or a set of in-flight reservations.
type Totals struct {
	Calls int64 // Calls is the number of requests.

	InputTokens int64 // InputTokens is the summed prompt tokens.

	OutputTokens int64 // OutputTokens is the summed generated tokens.

	CostUSD float64 // CostUSD is the summed notional cost in US dollars.
}

// UsageEvent is a single recorded LLM call, the row written to usage_event.
type UsageEvent struct {
	Timestamp time.Time // Timestamp is when the call was recorded.

	ProjectID int64 // ProjectID is the owning project.

	Tag string // Tag is the budget bucket the call is attributed to.

	Model string // Model is the requested model id.

	Provider string // Provider is the backend that served the call.

	InputTokens int // InputTokens is the prompt tokens consumed.

	OutputTokens int // OutputTokens is the generated tokens.

	CostUSD float64 // CostUSD is the notional cost in US dollars.

	Status string // Status is the outcome of the call (e.g. "ok", "error").

	LatencyMS int64 // LatencyMS is the wall-clock duration in milliseconds.

	Error string // Error is a short error description, empty on success.
}

// Token is an account's OAuth credential set.
type Token struct {
	AccessToken string // AccessToken is the current bearer token.

	RefreshToken string // RefreshToken is the rotating refresh credential.

	ExpiresAt time.Time // ExpiresAt is when AccessToken stops being valid.
}

// Provider forwards a request to an upstream LLM backend.
type Provider interface {
	// Send forwards req upstream. For non-streaming it writes the JSON body to out and
	// returns the Usage; for streaming it relays SSE to out while accumulating Usage.
	Send(ctx context.Context, req llm.ChatRequest, out http.ResponseWriter) (usage.Usage, error)
}

// Store is the persistence port: configuration, usage counters, reservations, and tokens.
type Store interface {
	// EnsureProject returns the id of the named project, creating it if absent (idempotent).
	EnsureProject(ctx context.Context, name string) (projectID int64, err error)

	// LimitsFor returns the budget limits configured for the (project, tag).
	LimitsFor(ctx context.Context, projectID int64, tag string) ([]BudgetLimit, error)

	// PriceFor returns the notional per-million-token input/output USD prices for a model.
	PriceFor(ctx context.Context, model string) (in, out float64, ok bool, err error)

	// DefaultRoute resolves the provider serving requests in V1 (the single default route).
	DefaultRoute(ctx context.Context) (Provider, error)

	// RecordUsage persists a completed call as a usage_event row.
	RecordUsage(ctx context.Context, e UsageEvent) error

	// WindowedTotals sums usage for a (project, tag) over events since the given time.
	WindowedTotals(ctx context.Context, projectID int64, tag string, since time.Time) (Totals, error)

	// Reserve records an in-flight call for a (project, tag), returning the reservation id.
	Reserve(ctx context.Context, projectID int64, tag string, ttl time.Duration) (reservationID int64, err error)

	// ReleaseReservation removes a previously created reservation.
	ReleaseReservation(ctx context.Context, reservationID int64) error

	// InflightTotals aggregates the non-expired reservations for a (project, tag).
	InflightTotals(ctx context.Context, projectID int64, tag string) (Totals, error)

	// LoadToken returns the persisted OAuth token for an account label.
	LoadToken(ctx context.Context, account string) (Token, error)

	// SaveToken persists the OAuth token for an account label.
	SaveToken(ctx context.Context, account string, t Token) error
}
