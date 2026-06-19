package domain

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain/llm"
	"github.com/clemsix6/LLMGW/internal/domain/usage"
)

// ErrTokenNotFound is returned by Store.LoadToken when no token exists for the account.
// Callers use it to decide whether to seed a token from configuration.
var ErrTokenNotFound = errors.New("oauth token not found")

// WholeProjectTag is the tag sentinel that selects a project-wide aggregate across every tag
// in WindowedTotals and InflightTotals. Request tags are never empty (X-Tags defaults to a
// real tag), so the empty string unambiguously means "all tags of the project".
const WholeProjectTag = ""

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

// WindowRead identifies one set of usage totals to gather during budget admission: the tag scope
// to aggregate (a concrete tag, or WholeProjectTag for the project-wide aggregate) and the start
// of the window (events at or after Since are counted).
type WindowRead struct {
	Tag string // Tag is the scope to aggregate; WholeProjectTag aggregates across every tag.

	Since time.Time // Since is the window start; usage recorded at or after it is counted.
}

// WindowTotals bundles the recorded usage and the in-flight reservations gathered for one
// WindowRead, so the caller can evaluate a window's limits against a single snapshot.
type WindowTotals struct {
	Current Totals // Current is the recorded windowed usage.

	Inflight Totals // Inflight is the in-flight reservation total (Calls only).
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

	SessionKey string // SessionKey is the durable claude.ai cookie that bootstraps OAuth tokens; empty when seeded directly with a refresh token.

	ExpiresAt time.Time // ExpiresAt is when AccessToken stops being valid.
}

// Account is a provider account and its cooldown state. The Claude Max provider pool uses it to
// pick a non-cooling account round-robin and to compute the soonest retry when all are cooling.
type Account struct {
	Label string // Label identifies the account (the oauth_token account_label).

	CooldownUntil time.Time // CooldownUntil is when the account stops being rate-limited; zero when not cooling.
}

// StreamSink is the write target a Provider relays the upstream response into. The domain
// stays free of net/http: buffered (non-streaming) responses are written then flushed once,
// SSE responses flush after each event to preserve latency. The HTTP handler adapts its
// ResponseWriter to this interface and keeps HTTP status/header ownership to itself.
type StreamSink interface {
	io.Writer

	// Flush pushes buffered bytes to the consumer.
	Flush()
}

// Provider forwards a request to an upstream LLM backend.
type Provider interface {
	// Send forwards req upstream, writing the response to out and returning the Usage.
	Send(ctx context.Context, req llm.Request, out StreamSink) (usage.Usage, error)
}

// Store is the persistence port: configuration, usage counters, reservations, and tokens.
type Store interface {
	// EnsureProject returns the id of the named project, creating it if absent (idempotent).
	EnsureProject(ctx context.Context, name string) (projectID int64, err error)

	// LimitsFor returns the budget limits configured for the (project, tag).
	LimitsFor(ctx context.Context, projectID int64, tag string) ([]BudgetLimit, error)

	// PriceFor returns the notional per-million-token input/output USD prices for a model.
	PriceFor(ctx context.Context, model string) (in, out float64, ok bool, err error)

	// RecordUsage persists a completed call as a usage_event row.
	RecordUsage(ctx context.Context, e UsageEvent) error

	// ReserveIfAdmitted atomically admits and reserves a call for a (project, tag). It serialises
	// concurrent admissions for the whole project (across every tag), prunes the project's expired
	// reservations, gathers the requested window totals, and passes them (in the same order as
	// reads) to admit. Only when admit returns true is a reservation inserted, so two concurrent
	// near-limit requests cannot both be admitted — including two on different tags racing a
	// whole-project cap. It returns the reservation id and true when reserved, or (0, false) when
	// admit declined.
	ReserveIfAdmitted(ctx context.Context, projectID int64, tag string, ttl time.Duration, reads []WindowRead, admit func(totals []WindowTotals) bool) (reservationID int64, admitted bool, err error)

	// ReleaseReservation removes a previously created reservation.
	ReleaseReservation(ctx context.Context, reservationID int64) error
}
