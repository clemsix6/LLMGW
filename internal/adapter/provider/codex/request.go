package codex

import (
	"encoding/json"
	"fmt"

	"github.com/clemsix6/LLMGW/internal/domain/llm"
)

// compile-time assertion that openaiRequest satisfies the domain port.
var _ llm.Request = openaiRequest{}

// openaiRequest is the OpenAI Chat Completions wire type as seen by the gateway: a light
// parse of model and stream for routing, carrying the raw body for the provider's translation
// step. The HTTP wire (a later task) produces it; the codex provider consumes it.
type openaiRequest struct {
	model  string // model from the "model" JSON field.
	stream bool   // stream from the "stream" JSON field.
	raw    []byte // raw is the full request body passed unchanged to the translation step.
}

// openaiEnvelope is a minimal Chat Completions body used only to extract model and stream
// during the light-parse step. All other fields are preserved in raw.
type openaiEnvelope struct {
	Model  string `json:"model"`  // Model is the requested model id.
	Stream bool   `json:"stream"` // Stream indicates whether the client wants an SSE response.
}

// ParseOpenAIRequest decodes a Chat Completions body into an openaiRequest, extracting
// model and stream. The raw body is retained unchanged for the provider's translate step.
func ParseOpenAIRequest(data []byte) (openaiRequest, error) {
	var env openaiEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return openaiRequest{}, fmt.Errorf("parse openai request envelope:\n%w", err)
	}
	return openaiRequest{model: env.Model, stream: env.Stream, raw: data}, nil
}

// Model returns the requested model id.
func (r openaiRequest) Model() string { return r.model }

// Stream reports whether the consumer requested a streamed (SSE) response.
func (r openaiRequest) Stream() bool { return r.stream }

// Bytes returns the raw Chat Completions body for the provider's translation step.
func (r openaiRequest) Bytes() []byte { return r.raw }
