package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/domain"
	"github.com/clemsix6/LLMGW/internal/domain/llm"
	"github.com/clemsix6/LLMGW/internal/domain/usage"
)

// compile-time assertion that Provider satisfies the domain port.
var _ domain.Provider = (*Provider)(nil)

const (
	// defaultResponsesBaseURL is the base of the Codex Responses surface on the ChatGPT backend.
	// Sourced and corroborated across ChatMock, opencode-openai-codex-auth, and codex-proxy.
	defaultResponsesBaseURL = "https://chatgpt.com/backend-api/codex"

	// responsesPath is the path of the Responses endpoint under the base.
	responsesPath = "/responses"

	// smokeModel is the model the skeleton's hardcoded probe targets; the codex backend serves
	// the GPT-5 family on the subscription path. Translation (Tasks 9-11) replaces this with the
	// caller's requested model.
	smokeModel = "gpt-5"

	// smokePrompt is the tiny user message the skeleton sends to prove the OAuth + spoof path
	// reaches a 200. It is not the real translated request body.
	smokePrompt = "Reply with a single word."
)

// accountStore is the persistence the provider needs: per-account token access (for the token
// manager) plus the account roster so the single-account skeleton can pick an account to serve.
type accountStore interface {
	tokenStore

	// LoadAccounts returns every account for the named provider with its cooldown state.
	LoadAccounts(ctx context.Context, providerName string) ([]domain.Account, error)
}

// Provider forwards requests to the ChatGPT Codex subscription over OAuth, applying the Codex
// client spoof. For this task it serves a single account and forwards a hardcoded minimal
// Responses probe to prove the subscription path; translation and account rotation come later.
type Provider struct {
	tokens *tokenManager // tokens hands out valid OAuth access tokens and the account id.

	spoof spoof // spoof sets the Codex client request headers.

	store accountStore // store lists the pool's accounts.

	providerName string // providerName scopes every store call to the Codex provider.

	httpClient *http.Client // httpClient performs the upstream request.

	baseURL string // baseURL is the Codex Responses base; injectable for tests.
}

// New builds a Codex provider over the accounts persisted in store, spoofing version.
func New(store accountStore, version string) *Provider {
	return &Provider{
		tokens:       newTokenManager(store, postgres.CodexProviderName),
		spoof:        spoof{version: version},
		store:        store,
		providerName: postgres.CodexProviderName,
		httpClient:   &http.Client{},
		baseURL:      defaultResponsesBaseURL,
	}
}

// Send forwards a hardcoded minimal Responses request through the first seeded account, proving
// the OAuth refresh + Codex spoof reach a 200. The caller's req is ignored for this task; on 200
// the upstream body is relayed verbatim and a zero Usage is returned (metering is added with
// translation). A non-200 maps to a typed error.
func (p *Provider) Send(ctx context.Context, _ llm.Request, out domain.StreamSink) (usage.Usage, error) {
	account, err := p.pickAccount(ctx)
	if err != nil {
		return usage.Usage{}, err
	}

	accessToken, accountID, err := p.tokens.Valid(ctx, account)
	if err != nil {
		return usage.Usage{}, fmt.Errorf("obtain access token:\n%w", err)
	}

	httpReq, err := p.buildRequest(ctx, accessToken, accountID)
	if err != nil {
		return usage.Usage{}, err
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return usage.Usage{}, fmt.Errorf("forward request:\n%w", err)
	}
	defer resp.Body.Close()

	return handleResponse(resp, out)
}

// pickAccount returns the first seeded account label, the one the skeleton serves. An empty pool
// is a dead credential from the caller's perspective.
func (p *Provider) pickAccount(ctx context.Context) (string, error) {
	accounts, err := p.store.LoadAccounts(ctx, p.providerName)
	if err != nil {
		return "", fmt.Errorf("load accounts:\n%w", err)
	}
	if len(accounts) == 0 {
		return "", &DeadRefreshTokenError{Account: "(none seeded)"}
	}
	return accounts[0].Label, nil
}

// buildRequest serialises the hardcoded probe body and applies the Codex spoof and bearer token.
func (p *Provider) buildRequest(ctx context.Context, accessToken, accountID string) (*http.Request, error) {
	body, err := json.Marshal(probeBody())
	if err != nil {
		return nil, fmt.Errorf("encode probe body:\n%w", err)
	}

	endpoint := p.baseURL + responsesPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build responses request:\n%w", err)
	}

	p.spoof.decorate(httpReq, accessToken, accountID)
	return httpReq, nil
}

// responsesRequest is the minimal Codex Responses body the skeleton sends. store:false and
// stream:true are required by the subscription surface; instructions carries the Codex system
// prompt candidate.
type responsesRequest struct {
	Model        string         `json:"model"`        // Model is the requested model id.
	Instructions string         `json:"instructions"` // Instructions is the Codex system prompt.
	Input        []responseItem `json:"input"`        // Input is the conversation, a single user message here.
	Store        bool           `json:"store"`        // Store is always false on the subscription path.
	Stream       bool           `json:"stream"`       // Stream is always true on the subscription path.
}

// responseItem is one input item: a message with typed content parts.
type responseItem struct {
	Type    string            `json:"type"`    // Type is the item type ("message").
	Role    string            `json:"role"`    // Role is the speaker ("user").
	Content []responseContent `json:"content"` // Content is the typed content parts.
}

// responseContent is one typed content part of an input message.
type responseContent struct {
	Type string `json:"type"` // Type is the content type ("input_text").
	Text string `json:"text"` // Text is the message text.
}

// probeBody builds the hardcoded minimal Responses request proving the subscription path.
func probeBody() responsesRequest {
	return responsesRequest{
		Model:        smokeModel,
		Instructions: instructions(),
		Input: []responseItem{{
			Type:    "message",
			Role:    "user",
			Content: []responseContent{{Type: "input_text", Text: smokePrompt}},
		}},
		Store:  false,
		Stream: true,
	}
}

// handleResponse maps the upstream response: 200 relays the body verbatim and returns a zero
// Usage (metering deferred to translation), 429 becomes a RateLimitError, any other status an
// UpstreamError.
func handleResponse(resp *http.Response, out domain.StreamSink) (usage.Usage, error) {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return usage.Usage{}, fmt.Errorf("read response body:\n%w", err)
	}
	if resp.StatusCode == http.StatusOK {
		return relay(body, out)
	}
	return usage.Usage{}, classifyUpstream(resp.StatusCode, resp.Header, body)
}

// relay writes the upstream body to out and flushes it, returning a zero Usage.
func relay(body []byte, out domain.StreamSink) (usage.Usage, error) {
	if _, err := out.Write(body); err != nil {
		return usage.Usage{}, fmt.Errorf("write response body:\n%w", err)
	}
	out.Flush()
	return usage.Usage{}, nil
}

// classifyUpstream maps a non-200 Codex response to a typed error: a 429 becomes a RateLimitError
// (carrying the reset time when present), anything else an UpstreamError.
func classifyUpstream(status int, header http.Header, body []byte) error {
	if status == http.StatusTooManyRequests {
		return &RateLimitError{ResetAt: parseResetAt(header, time.Now())}
	}
	return &UpstreamError{Status: status, Body: string(body)}
}

// parseResetAt extracts the rate-limit reset time from a Retry-After header (delta seconds or HTTP
// date), returning the zero time when no hint is present.
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
