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
)

// accountStore is the persistence the provider needs: per-account token access (for the token
// manager) plus the account roster so the single-account skeleton can pick an account to serve.
type accountStore interface {
	tokenStore

	// LoadAccounts returns every account for the named provider with its cooldown state.
	LoadAccounts(ctx context.Context, providerName string) ([]domain.Account, error)
}

// Provider forwards Chat Completions requests to the ChatGPT Codex subscription over OAuth,
// translating to/from the Responses API and applying the Codex client spoof.
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

// Send translates the Chat Completions request to the Responses API, calls the Codex backend,
// and writes the result to out. Non-streaming: the upstream SSE is aggregated and a single
// chat.completion object is emitted. Streaming is reserved for Task 11.
func (p *Provider) Send(ctx context.Context, req llm.Request, out domain.StreamSink) (usage.Usage, error) {
	account, err := p.pickAccount(ctx)
	if err != nil {
		return usage.Usage{}, err
	}

	accessToken, accountID, err := p.tokens.Valid(ctx, account)
	if err != nil {
		return usage.Usage{}, fmt.Errorf("obtain access token:\n%w", err)
	}

	body, err := translateRequest(req.Bytes(), instructions())
	if err != nil {
		return usage.Usage{}, fmt.Errorf("translate request:\n%w", err)
	}

	httpReq, err := p.newRequest(ctx, accessToken, accountID, body)
	if err != nil {
		return usage.Usage{}, err
	}

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return usage.Usage{}, fmt.Errorf("forward request:\n%w", err)
	}
	defer resp.Body.Close()

	return p.handleResp(resp, req.Stream(), out, parseIncludeUsage(req.Bytes()))
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

// newRequest builds an HTTP request with the translated Responses body and Codex spoof headers.
func (p *Provider) newRequest(ctx context.Context, accessToken, accountID string, body []byte) (*http.Request, error) {
	endpoint := p.baseURL + responsesPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build responses request:\n%w", err)
	}
	p.spoof.decorate(httpReq, accessToken, accountID)
	return httpReq, nil
}

// responsesRequest is the Codex Responses request body. store:false and stream:true are
// required by the subscription surface; instructions carries the Codex system prompt.
// MaxOutputTokens, Tools, and ToolChoice are optional and omitted when zero/nil.
type responsesRequest struct {
	Model           string         `json:"model"`                       // Model is the requested model id.
	Instructions    string         `json:"instructions"`                 // Instructions is the Codex system prompt.
	Input           []responseItem `json:"input"`                        // Input is the conversation items.
	Store           bool           `json:"store"`                        // Store is always false on the subscription path.
	Stream          bool           `json:"stream"`                       // Stream is always true on the subscription path.
	MaxOutputTokens int            `json:"max_output_tokens,omitempty"` // MaxOutputTokens caps the response length; mapped from max_tokens.
	Tools           []responseTool `json:"tools,omitempty"`              // Tools is the callable functions list.
	ToolChoice      any            `json:"tool_choice,omitempty"`        // ToolChoice controls function-selection behaviour.
}

// responseItem is one input item in the Responses conversation. Depending on Type it is:
// a "message" (Role+Content), a "function_call" (CallID+Name+Arguments), or a
// "function_call_output" (CallID+Output).
type responseItem struct {
	Type      string            `json:"type"`                // Type is the item type.
	Role      string            `json:"role,omitempty"`      // Role is the speaker for message items.
	Content   []responseContent `json:"content,omitempty"`   // Content is the typed parts for message items.
	CallID    string            `json:"call_id,omitempty"`   // CallID links a function_call to its function_call_output.
	Name      string            `json:"name,omitempty"`      // Name is the function name for function_call items.
	Arguments string            `json:"arguments,omitempty"` // Arguments is the JSON arguments string for function_call items.
	Output    string            `json:"output,omitempty"`    // Output is the function result for function_call_output items.
}

// responseContent is one typed content part of an input message.
type responseContent struct {
	Type string `json:"type"` // Type is the content type ("input_text" or "output_text").
	Text string `json:"text"` // Text is the message text.
}

// responseTool is a callable function in the Responses API tools array.
type responseTool struct {
	Type        string          `json:"type"`                  // Type is always "function".
	Name        string          `json:"name"`                  // Name is the function identifier.
	Description string          `json:"description,omitempty"` // Description explains what the function does.
	Parameters  json.RawMessage `json:"parameters,omitempty"`  // Parameters is the JSON Schema for function arguments.
}

// handleResp dispatches the upstream response. Non-200 → typed error; 200 → streaming or
// non-streaming path depending on stream. includeUsage is forwarded to the streaming path to
// control the optional usage-only chunk at the end of the SSE stream.
func (p *Provider) handleResp(resp *http.Response, stream bool, out domain.StreamSink, includeUsage bool) (usage.Usage, error) {
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return usage.Usage{}, classifyUpstream(resp.StatusCode, resp.Header, body)
	}
	if stream {
		return relayTranslatedStream(resp.Body, out, includeUsage)
	}
	return handleNonStreaming(resp.Body, out)
}

// handleNonStreaming aggregates the upstream Responses SSE, translates to chat.completion,
// writes the JSON to out, and returns the usage from the real Responses usage block.
func handleNonStreaming(body io.Reader, out domain.StreamSink) (usage.Usage, error) {
	completed, err := aggregateCompleted(body)
	if err != nil {
		return usage.Usage{}, fmt.Errorf("aggregate completed response:\n%w", err)
	}
	ccJSON, u, err := translateResponse(completed)
	if err != nil {
		return usage.Usage{}, fmt.Errorf("translate response:\n%w", err)
	}
	if _, err := out.Write(ccJSON); err != nil {
		return usage.Usage{}, fmt.Errorf("write chat completion:\n%w", err)
	}
	out.Flush()
	return u, nil
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
