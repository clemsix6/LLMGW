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
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
	"github.com/clemsix6/LLMGW/internal/domain/llm"
	"github.com/clemsix6/LLMGW/internal/domain/usage"
)

// errStreamingUnsupported is returned for stream:true requests until the streaming relay
// is added in a later batch.
var errStreamingUnsupported = errors.New("streaming not yet supported")

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

// UpstreamError reports a non-2xx upstream response other than a rate limit.
type UpstreamError struct {
	Status int // Status is the HTTP status code returned by the upstream.

	Body string // Body is the raw upstream response body, for diagnostics.
}

// Error implements the error interface.
func (e *UpstreamError) Error() string {
	return fmt.Sprintf("upstream returned status %d: %s", e.Status, e.Body)
}

// Provider forwards Anthropic Messages requests to Claude Max over OAuth, applying the
// Claude Code spoof. V1 serves a single account.
type Provider struct {
	tokens *tokenManager // tokens hands out valid OAuth access tokens for the account.

	spoof spoof // spoof builds the Claude Code billing header and request headers.

	account string // account is the label of the OAuth account this provider serves.

	httpClient *http.Client // httpClient performs the upstream request (a plain net/http client).

	baseURL string // baseURL is the Anthropic API base; injectable for tests.
}

// New builds a Claude Max provider for the given account, spoofing claudeCodeVersion.
func New(store tokenStore, account, claudeCodeVersion string) *Provider {
	return &Provider{
		tokens:     newTokenManager(store),
		spoof:      spoof{version: claudeCodeVersion},
		account:    account,
		httpClient: &http.Client{},
		baseURL:    defaultAnthropicBaseURL,
	}
}

// Send forwards a non-streaming request upstream, writes the response body to out, and
// returns the metered usage. Streaming requests are rejected until a later batch adds the
// SSE relay.
func (p *Provider) Send(ctx context.Context, req llm.ChatRequest, out domain.StreamSink) (usage.Usage, error) {
	if req.Stream() {
		return usage.Usage{}, errStreamingUnsupported
	}

	token, err := p.tokens.Valid(ctx, p.account)
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

	return p.handleResponse(resp, out)
}

// buildRequest injects the Claude Code system block, serialises the body, and applies the
// spoof headers and bearer token.
func (p *Provider) buildRequest(ctx context.Context, req llm.ChatRequest, token string) (*http.Request, error) {
	spoofed := req.WithClaudeCodeSystem(p.spoof.billingHeader(req.FirstUserText()))

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

	switch resp.StatusCode {
	case http.StatusOK:
		return writeAndMeter(body, out)
	case http.StatusTooManyRequests:
		return usage.Usage{}, &RateLimitError{ResetAt: parseResetAt(resp.Header, time.Now())}
	default:
		return usage.Usage{}, &UpstreamError{Status: resp.StatusCode, Body: string(body)}
	}
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
