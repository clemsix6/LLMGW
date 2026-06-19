package codex

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
)

const (
	// originator is the whitelisted client identifier the real Codex CLI sends; the backend
	// gates the codex Responses surface on it. Sourced from the Codex CLI (codex_cli_rs) and
	// corroborated by the opencode-openai-codex-auth reference.
	originator = "codex_cli_rs"

	// openAIBeta is the beta opt-in header the Codex client sends to reach the Responses API.
	openAIBeta = "responses=experimental"

	// accept is the response media type the streaming Responses surface returns.
	accept = "text/event-stream"
)

// spoof produces the Codex client request markers (HTTP headers) for the configured client
// version. It mirrors claudemax's spoof but speaks OpenAI's Codex wire instead of Anthropic's.
type spoof struct {
	version string // version is the spoofed Codex CLI client version (e.g. "0.40.0").
}

// decorate sets the Codex client spoof headers on an upstream request: the OAuth bearer, the
// per-account ChatGPT account id, the spoofed user agent and originator, the beta opt-in, and
// fresh per-request correlation ids the real client sends.
func (s spoof) decorate(req *http.Request, accessToken, accountID string) {
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("ChatGPT-Account-ID", accountID)
	req.Header.Set("User-Agent", s.userAgent())
	req.Header.Set("originator", originator)
	req.Header.Set("OpenAI-Beta", openAIBeta)
	req.Header.Set("session_id", newRequestID())
	req.Header.Set("x-client-request-id", newRequestID())
}

// userAgent builds the Codex CLI user-agent string for the configured version, matching the
// codex_cli_rs/<version> (<os>) shape the reference clients send.
func (s spoof) userAgent() string {
	return fmt.Sprintf("codex_cli_rs/%s (LLMGW; gateway)", s.version)
}

// newRequestID returns a random hex correlation id for the per-request session and request
// headers. A failure of the entropy source falls back to a fixed marker rather than erroring,
// since these ids are opaque to the backend.
func newRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "llmgw-codex"
	}
	return hex.EncodeToString(buf)
}
