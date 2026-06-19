package codex

import (
	"fmt"
	"net/http"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// compile-time assertions that the codex error types satisfy the domain port the handler maps.
var (
	_ domain.ProviderError = (*RateLimitError)(nil)
	_ domain.ProviderError = (*UpstreamError)(nil)
	_ domain.ProviderError = (*DeadRefreshTokenError)(nil)
)

// RateLimitError reports that the Codex backend rate-limited the request. ResetAt carries the
// time the limit clears when the upstream provides it, otherwise it is the zero time.
type RateLimitError struct {
	ResetAt time.Time // ResetAt is when the rate limit clears; zero when unknown.
}

// Error implements the error interface.
func (e *RateLimitError) Error() string {
	if e.ResetAt.IsZero() {
		return "codex upstream rate limited (no reset time provided)"
	}
	return fmt.Sprintf("codex upstream rate limited until %s", e.ResetAt.Format(time.RFC3339))
}

// HTTPStatus returns 503 Service Unavailable for a rate limit.
func (e *RateLimitError) HTTPStatus() int { return 503 }

// ErrorType returns the OpenAI-shaped classifier "rate_limit_exceeded".
func (e *RateLimitError) ErrorType() string { return "rate_limit_exceeded" }

// RetryAfter returns the duration until ResetAt when known, otherwise (0, false).
func (e *RateLimitError) RetryAfter() (time.Duration, bool) {
	if e.ResetAt.IsZero() {
		return 0, false
	}
	return time.Until(e.ResetAt), true
}

// UpstreamError reports a non-2xx Codex response other than a rate limit.
type UpstreamError struct {
	Status int // Status is the HTTP status code returned by the upstream.

	Body string // Body is the raw upstream response body, for diagnostics.
}

// Error implements the error interface.
func (e *UpstreamError) Error() string {
	return fmt.Sprintf("codex upstream returned status %d: %s", e.Status, e.Body)
}

// HTTPStatus echoes the upstream status for 4xx/5xx, falling back to 502 for nonsensical codes.
func (e *UpstreamError) HTTPStatus() int {
	if e.Status >= 400 && e.Status <= 599 {
		return e.Status
	}
	return 502
}

// ErrorType returns the stable classifier "upstream_error".
func (e *UpstreamError) ErrorType() string { return "upstream_error" }

// RetryAfter returns (0, false); upstream errors carry no retry hint.
func (e *UpstreamError) RetryAfter() (time.Duration, bool) { return 0, false }

// DeadRefreshTokenError signals that an account's refresh token is no longer accepted (the OAuth
// endpoint returned invalid_grant). Recovery requires a manual re-seed; Codex has no session-key
// self-heal path.
type DeadRefreshTokenError struct {
	Account string // Account is the label of the account whose refresh token is dead.
}

// Error implements the error interface.
func (e *DeadRefreshTokenError) Error() string {
	return fmt.Sprintf("codex refresh token for account %q is dead (invalid_grant); re-seed required", e.Account)
}

// HTTPStatus returns 502 Bad Gateway; the upstream credential is dead.
func (e *DeadRefreshTokenError) HTTPStatus() int { return http.StatusBadGateway }

// ErrorType returns the stable classifier "dead_refresh_token".
func (e *DeadRefreshTokenError) ErrorType() string { return "dead_refresh_token" }

// RetryAfter returns (0, false); no retry hint applies for a dead token.
func (e *DeadRefreshTokenError) RetryAfter() (time.Duration, bool) { return 0, false }
