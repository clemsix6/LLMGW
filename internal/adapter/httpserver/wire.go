package httpserver

import (
	"github.com/clemsix6/LLMGW/internal/domain/llm"
)

// Wire is the per-route body parser and default-tag provider. Each route registers its own
// Wire so the handler core stays generic: the Wire decodes the client body into the
// wire-agnostic llm.Request and supplies the default budget tag when X-Tags is absent.
type Wire interface {
	// Parse decodes a raw request body into a wire-agnostic Request.
	Parse(body []byte) (llm.Request, error)

	// DefaultTag returns the budget tag used when the X-Tags header is absent.
	DefaultTag() string
}

// AnthropicWire parses native Anthropic Messages request bodies.
type AnthropicWire struct{}

// Parse decodes an Anthropic Messages body via llm.ParseRequest.
func (AnthropicWire) Parse(body []byte) (llm.Request, error) {
	return llm.ParseRequest(body)
}

// DefaultTag returns the default budget tag for Anthropic routes.
func (AnthropicWire) DefaultTag() string {
	return "default"
}

// rawRequest is a lightweight llm.Request for wires that only need to extract model, stream
// and the raw body. The provider does the single authoritative full parse of Bytes() in its
// own wire; rawRequest carries just enough for the handler to route and meter the call.
// It is used by the OpenAI wire (added in a later task) and defined here so the type is
// available when that wire is wired in.
type rawRequest struct {
	model  string // model is the requested model id extracted from the body.
	stream bool   // stream reports whether the client asked for an SSE response.
	body   []byte // body is the raw client request body forwarded verbatim upstream.
}

// Model returns the requested model id.
func (r rawRequest) Model() string { return r.model }

// Stream reports whether the client requested a streamed response.
func (r rawRequest) Stream() bool { return r.stream }

// Bytes returns the raw client request body.
func (r rawRequest) Bytes() []byte { return r.body }
