package claudemax

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
	"github.com/clemsix6/LLMGW/internal/domain/llm"
	"github.com/clemsix6/LLMGW/internal/domain/usage"
)

// claudeMaxProviderName is the provider-table name for the Claude Max OAuth backend. It is passed
// to every store call so token and account queries are scoped to this provider row.
const claudeMaxProviderName = "claude_max"

// compile-time assertion that Provider satisfies the domain port.
var _ domain.Provider = (*Provider)(nil)

// RateLimitError reports that the upstream rate-limited the request. ResetAt carries the
// time the limit clears when the upstream provides it, otherwise it is the zero time.
type RateLimitError struct {
	ResetAt time.Time // ResetAt is when the rate limit clears; zero when unknown.
}

// Error implements the error interface.
func (e *RateLimitError) Error() string {
	if e.ResetAt.IsZero() {
		return "upstream rate limited (no reset time provided)"
	}
	return fmt.Sprintf("upstream rate limited until %s", e.ResetAt.Format(time.RFC3339))
}

// HTTPStatus returns 503 Service Unavailable for a rate limit.
func (e *RateLimitError) HTTPStatus() int { return 503 }

// ErrorType returns the stable classifier "rate_limited".
func (e *RateLimitError) ErrorType() string { return "rate_limited" }

// RetryAfter returns the duration until ResetAt when known, otherwise (0, false).
func (e *RateLimitError) RetryAfter() (time.Duration, bool) {
	if e.ResetAt.IsZero() {
		return 0, false
	}
	return time.Until(e.ResetAt), true
}

// UpstreamError reports a non-2xx upstream response other than a rate limit.
type UpstreamError struct {
	Status int // Status is the HTTP status code returned by the upstream.

	Body string // Body is the raw upstream response body, for diagnostics.
}

// Error implements the error interface.
func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream returned status %d: %s", e.Status, e.Body)
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

// UsageExhaustedError reports that an account's extra-usage (pay-as-you-go) budget is spent —
// Anthropic's "out of extra usage" 400. It is account-specific: another account may still serve,
// so Send cools this account and fails over rather than surfacing the error to the caller.
type UsageExhaustedError struct{}

// Error implements the error interface.
func (e *UsageExhaustedError) Error() string {
	return "account out of extra usage"
}

// HTTPStatus returns 503 Service Unavailable; the account's usage budget is spent.
func (e *UsageExhaustedError) HTTPStatus() int { return 503 }

// ErrorType returns the stable classifier "usage_exhausted".
func (e *UsageExhaustedError) ErrorType() string { return "usage_exhausted" }

// RetryAfter returns (0, false); no retry hint is available for usage exhaustion.
func (e *UsageExhaustedError) RetryAfter() (time.Duration, bool) { return 0, false }

// Provider forwards Anthropic Messages requests to Claude Max over OAuth, applying the Claude
// Code spoof. It holds a pool of accounts (the oauth_token rows) and rotates across them
// round-robin, putting an account on cooldown when the upstream rate-limits it.
type Provider struct {
	tokens *tokenManager // tokens hands out valid OAuth access tokens per account.

	spoof spoof // spoof builds the Claude Code billing header and request headers.

	store accountStore // store lists the pool's accounts and persists their cooldown state.

	providerName string // providerName is passed to every store call to scope queries to this provider.

	httpClient *http.Client // httpClient performs the upstream request (a plain net/http client).

	baseURL string // baseURL is the Anthropic API base; injectable for tests.

	next atomic.Uint64 // next is the round-robin cursor, advanced once per Send.
}

// New builds a Claude Max provider over the accounts persisted in store, spoofing claudeCodeVersion.
func New(store accountStore, claudeCodeVersion string) *Provider {
	return &Provider{
		tokens:       newTokenManager(store, claudeCodeVersion, claudeMaxProviderName),
		spoof:        spoof{version: claudeCodeVersion},
		store:        store,
		providerName: claudeMaxProviderName,
		httpClient:   &http.Client{},
		baseURL:      defaultAnthropicBaseURL,
	}
}

// Send forwards a request through the next non-cooling account, rotating round-robin. On an
// upstream 429 it cools that account (honoring the reset header, else a short default) and retries
// the next account. When every account is cooling it returns *AllCoolingError so the handler can
// reply 503. A success or any non-rate-limit error is returned immediately — once bytes reach out
// the relay cannot be retried. Both the non-streaming and streaming paths flow through here.
func (p *Provider) Send(ctx context.Context, req llm.Request, out domain.StreamSink) (usage.Usage, error) {
	chat, err := llm.ParseRequest(req.Bytes())
	if err != nil {
		return usage.Usage{}, fmt.Errorf("parse anthropic request:\n%w", err)
	}

	accounts, err := p.store.LoadAccounts(ctx, p.providerName)
	if err != nil {
		return usage.Usage{}, fmt.Errorf("load accounts:\n%w", err)
	}

	now := time.Now()
	for _, account := range p.selectOrder(accounts, now) {
		metered, err := p.sendVia(ctx, account, chat, out)

		if until, retry := cooldownFor(err, now); retry {
			p.cool(ctx, account, until)
			continue
		}
		return metered, err
	}

	return usage.Usage{}, p.allCooling(ctx, now)
}

// cooldownFor decides whether an error from one account's Send attempt should fail over to the
// next account, and until when the failing account is cooled. Account-specific or transient
// failures retry — a rate limit (429), extra-usage exhaustion, a dead credential, or an upstream
// 5xx/401/403 — each cooled so selectOrder skips the account meanwhile. Request-level errors (a
// malformed 4xx, a parse failure) return false: they would fail on every account, so they surface
// to the caller unchanged.
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

	var exhausted *UsageExhaustedError
	if errors.As(err, &exhausted) {
		return now.Add(usageExhaustedCooldown), true
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
// enough to try the next account: auth rejections (401/403) and server errors (5xx). Other 4xx
// are request-level and surface unchanged.
func shouldFailoverStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status >= 500
}

// sendVia forwards the request through one account: it obtains a valid access token, applies the
// Claude Code spoof, and relays the upstream response (buffered for non-streaming, SSE for
// streaming). An upstream non-200 surfaces as a typed error before any byte is written to out.
func (p *Provider) sendVia(ctx context.Context, account string, req llm.ChatRequest, out domain.StreamSink) (usage.Usage, error) {
	token, err := p.tokens.Valid(ctx, account)
	if err != nil {
		return usage.Usage{}, fmt.Errorf("obtain access token:\n%w", err)
	}

	httpReq, err := p.buildRequest(ctx, req, token)
	if err != nil {
		return usage.Usage{}, err
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return usage.Usage{}, fmt.Errorf("forward request:\n%w", err)
	}
	defer resp.Body.Close()

	if req.Stream() {
		return handleStreamResponse(resp, out)
	}
	return p.handleResponse(resp, out)
}

// buildRequest normalizes the request for the OAuth surface, injects the Claude Code system
// block, serialises the body, and applies the spoof headers and bearer token.
func (p *Provider) buildRequest(ctx context.Context, req llm.ChatRequest, token string) (*http.Request, error) {
	spoofed := req.Normalize().WithClaudeCodeSystem(p.spoof.billingHeader(req.FirstUserText()))

	endpoint := p.baseURL + "/v1/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(spoofed.Bytes()))
	if err != nil {
		return nil, fmt.Errorf("build messages request:\n%w", err)
	}

	p.spoof.decorate(httpReq, token)
	return httpReq, nil
}

// handleResponse reads the upstream response and maps it: 200 relays the body and meters
// usage, 429 becomes a RateLimitError, any other status an UpstreamError.
func (p *Provider) handleResponse(resp *http.Response, out domain.StreamSink) (usage.Usage, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return usage.Usage{}, fmt.Errorf("read response body:\n%w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return writeAndMeter(body, out)
	}
	return usage.Usage{}, classifyUpstream(resp.StatusCode, resp.Header, body)
}

// classifyUpstream maps a non-200 upstream response to a typed error so the buffered and streaming
// paths share one decision: a 429 becomes a RateLimitError (carrying the reset time when present),
// a 4xx whose body is Anthropic's "out of extra usage" billing rejection becomes a
// UsageExhaustedError (account-specific — Send cools and fails over), and anything else an
// UpstreamError (surfaced to the caller).
func classifyUpstream(status int, header http.Header, body []byte) error {
	if status == http.StatusTooManyRequests {
		return &RateLimitError{ResetAt: parseResetAt(header, time.Now())}
	}
	if status >= 400 && status < 500 && isUsageExhausted(body) {
		return &UsageExhaustedError{}
	}
	return &UpstreamError{Status: status, Body: string(body)}
}

// isUsageExhausted reports whether an upstream error body is the "out of extra usage" exhaustion
// of an account's pay-as-you-go budget (distinct from a malformed-request 4xx). It matches the
// stable phrase in the error message rather than the full text.
func isUsageExhausted(body []byte) bool {
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(parsed.Error.Message), "out of extra usage")
}

// messageResponse is the slice of a successful Messages response the gateway meters.
type messageResponse struct {
	Usage struct {
		InputTokens  int `json:"input_tokens"`  // InputTokens is the prompt tokens consumed.
		OutputTokens int `json:"output_tokens"` // OutputTokens is the generated tokens.
	} `json:"usage"`
}

// writeAndMeter relays the response body to out and parses the usage counters from it.
func writeAndMeter(body []byte, out domain.StreamSink) (usage.Usage, error) {
	if _, err := out.Write(body); err != nil {
		return usage.Usage{}, fmt.Errorf("write response body:\n%w", err)
	}
	out.Flush()

	var parsed messageResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return usage.Usage{}, fmt.Errorf("parse usage:\n%w", err)
	}

	return usage.Usage{
		InputTokens:  parsed.Usage.InputTokens,
		OutputTokens: parsed.Usage.OutputTokens,
	}, nil
}

// parseResetAt extracts the rate-limit reset time from the response headers, preferring
// Anthropic's unified-reset epoch over a Retry-After delta or HTTP date. It returns the
// zero time when no hint is present.
func parseResetAt(header http.Header, now time.Time) time.Time {
	if value := header.Get("anthropic-ratelimit-unified-reset"); value != "" {
		if epoch, err := strconv.ParseInt(value, 10, 64); err == nil {
			return time.Unix(epoch, 0)
		}
	}

	if value := header.Get("retry-after"); value != "" {
		if seconds, err := strconv.Atoi(value); err == nil {
			return now.Add(time.Duration(seconds) * time.Second)
		}
		if at, err := http.ParseTime(value); err == nil {
			return at
		}
	}
	return time.Time{}
}
