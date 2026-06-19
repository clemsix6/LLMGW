package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
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

// Provider forwards Chat Completions requests to the ChatGPT Codex subscription over OAuth,
// translating to/from the Responses API and applying the Codex client spoof. It holds a pool of
// accounts and rotates across them round-robin, putting an account on cooldown when the upstream
// rate-limits or rejects it.
type Provider struct {
	tokens *tokenManager // tokens hands out valid OAuth access tokens and the account id.

	spoof spoof // spoof sets the Codex client request headers.

	store accountStore // store lists the pool's accounts and persists their cooldown state.

	providerName string // providerName scopes every store call to the Codex provider.

	httpClient *http.Client // httpClient performs the upstream request.

	baseURL string // baseURL is the Codex Responses base; injectable for tests.

	next atomic.Uint64 // next is the round-robin cursor, advanced once per Send.
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

// Send forwards a request through the next non-cooling account, rotating round-robin. On an
// upstream 429 it cools that account (honoring the reset header, else a short default) and retries
// the next account. Auth or 5xx failures are also cooled and failed over. When every account is
// cooling it returns *AllCoolingError so the handler can reply 503. Once bytes reach out the relay
// cannot be retried — the non-2xx detection in handleResp happens before any write to out.
func (p *Provider) Send(ctx context.Context, req llm.Request, out domain.StreamSink) (usage.Usage, error) {
	accounts, err := p.store.LoadAccounts(ctx, p.providerName)
	if err != nil {
		return usage.Usage{}, fmt.Errorf("load accounts:\n%w", err)
	}

	now := time.Now()
	for _, account := range p.selectOrder(accounts, now) {
		metered, err := p.sendVia(ctx, account, req, out)

		if until, retry := cooldownFor(err, now); retry {
			p.cool(ctx, account, until)
			continue
		}
		return metered, err
	}

	return usage.Usage{}, p.allCooling(ctx, now)
}

// sendVia forwards the request through one account: it obtains a valid access token, translates
// the request, and relays the upstream response. An upstream non-200 surfaces as a typed error
// before any byte is written to out, preserving the failover contract.
func (p *Provider) sendVia(ctx context.Context, account string, req llm.Request, out domain.StreamSink) (usage.Usage, error) {
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
