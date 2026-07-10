package codex

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
)

// compile-time assertions that the codex error types satisfy the domain port the handler maps.
var (
	_ domain.ProviderError = (*RateLimitError)(nil)
	_ domain.ProviderError = (*UpstreamError)(nil)
	_ domain.ProviderError = (*DeadRefreshTokenError)(nil)
	_ domain.ProviderError = (*AllCoolingError)(nil)
	_ domain.ProviderError = (*InvalidModelError)(nil)
)

// InvalidModelError reports that the requested model is not served by the Codex subscription
// backend. It is a permanent request-level error: a bad model fails on every account, so no
// account failover is attempted. HTTPStatus returns 400 so retry-aware clients do not retry.
type InvalidModelError struct {
	Model string // Model is the unrecognised model id.
}

// Error implements the error interface.
func (e *InvalidModelError) Error() string {
	return fmt.Sprintf("unknown Codex model %q: must be one of %s", e.Model, strings.Join(Models(), ", "))
}

// HTTPStatus returns 400 Bad Request; the model is not served by Codex.
func (e *InvalidModelError) HTTPStatus() int { return http.StatusBadRequest }

// ErrorType returns the stable classifier "invalid_request".
func (e *InvalidModelError) ErrorType() string { return "invalid_request" }

// RetryAfter returns (0, false); a bad model never becomes valid.
func (e *InvalidModelError) RetryAfter() (time.Duration, bool) { return 0, false }

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

// AllCoolingError reports that every account in the pool is on cooldown, so no request can be
// served right now. After is the delay until the soonest account becomes available; the handler
// maps it to a 503 with a Retry-After header.
type AllCoolingError struct {
	After time.Duration // After is the wait until the earliest account's cooldown clears.
}

// Error implements the error interface.
func (e *AllCoolingError) Error() string {
	return fmt.Sprintf("codex: all accounts cooling; retry after %s", e.After)
}

// HTTPStatus returns 503 Service Unavailable; all accounts are cooling.
func (e *AllCoolingError) HTTPStatus() int { return 503 }

// ErrorType returns the stable classifier "all_cooling".
func (e *AllCoolingError) ErrorType() string { return "all_cooling" }

// RetryAfter returns the known backoff duration (always present for AllCoolingError).
func (e *AllCoolingError) RetryAfter() (time.Duration, bool) { return e.After, true }

// cooldownFor decides whether an error from one account's Send attempt should fail over to the
// next account, and until when the failing account is cooled. Account-specific or transient
// failures retry — a rate limit (429), a dead credential, or an upstream 5xx/401/403 — each
// cooled so selectOrder skips the account meanwhile. Request-level errors (a malformed 4xx, a
// parse failure) return false: they would fail on every account, so they surface to the caller.
func cooldownFor(err error, now time.Time) (time.Time, bool) {
	if err == nil {
		return time.Time{}, false
	}

	var rate *RateLimitError
	if errors.As(err, &rate) {
		if !rate.ResetAt.IsZero() {
			return rate.ResetAt, true
		}
		return now.Add(defaultCooldown), true
	}

	var dead *DeadRefreshTokenError
	if errors.As(err, &dead) {
		return now.Add(deadTokenCooldown), true
	}

	var upstream *UpstreamError
	if errors.As(err, &upstream) && shouldFailoverStatus(upstream.Status) {
		return now.Add(defaultCooldown), true
	}

	return time.Time{}, false
}

// shouldFailoverStatus reports whether an upstream HTTP status is account-specific or transient
// enough to try the next account: auth rejections (401/403, e.g. Cloudflare originator blocks)
// and server errors (5xx). Other 4xx are request-level and surface unchanged.
func shouldFailoverStatus(status int) bool {
	return status == http.StatusUnauthorized ||
		status == http.StatusForbidden ||
		status >= 500
}

// classifyUpstream maps a non-200 Codex Responses response to a typed error: a 429 becomes a
// RateLimitError (carrying the reset time when present from Retry-After), anything else an
// UpstreamError (surfaced or failed over depending on the HTTP status).
func classifyUpstream(status int, header http.Header, body []byte) error {
	if status == http.StatusTooManyRequests {
		return &RateLimitError{ResetAt: parseResetAt(header, time.Now())}
	}
	return &UpstreamError{Status: status, Body: string(body)}
}

// parseResetAt extracts the rate-limit reset time from a Retry-After header (delta seconds or
// HTTP date), returning the zero time when no hint is present.
func parseResetAt(header http.Header, now time.Time) time.Time {
	value := header.Get("retry-after")
	if value == "" {
		return time.Time{}
	}
	if seconds, err := strconv.Atoi(value); err == nil {
		return now.Add(time.Duration(seconds) * time.Second)
	}
	if at, err := http.ParseTime(value); err == nil {
		return at
	}
	return time.Time{}
}
