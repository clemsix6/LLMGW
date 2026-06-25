package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Request is the wire-agnostic view the gateway needs to meter and route a call, regardless
// of the provider's wire format. The HTTP wire resolves model/stream with a light parse and
// carries the raw body; the provider does the single full parse of Bytes() in its own wire.
type Request interface {
	// Model returns the requested model id, used for usage rows and routing.
	Model() string

	// Stream reports whether the consumer asked for a streamed (SSE) response.
	Stream() bool

	// Bytes returns the raw client request body, parsed by the provider's wire.
	Bytes() []byte
}

// ChatRequest is a single Anthropic Messages request flowing through the gateway.
// V1 keeps the body unchanged except for a prepended Claude Code system block, so the
// request stays compatible with any Anthropic SDK. Unknown fields (tools, metadata, ...)
// are preserved verbatim and forwarded.
type ChatRequest struct {
	body map[string]any // body is the full parsed Anthropic request, preserved across the gateway.
}

// ParseRequest decodes an Anthropic Messages request body, keeping every field.
func ParseRequest(data []byte) (ChatRequest, error) {
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		return ChatRequest{}, fmt.Errorf("parse chat request:\n%w", err)
	}
	return ChatRequest{body: body}, nil
}

// Model returns the requested model id, or "" when absent.
func (r ChatRequest) Model() string {
	model, _ := r.body["model"].(string)
	return model
}

// Stream reports whether the consumer asked for a streamed response.
func (r ChatRequest) Stream() bool {
	stream, _ := r.body["stream"].(bool)
	return stream
}

// FirstUserText returns the text of the first user message: its string content, or the
// first text block when the content is an array. It returns "" when none is found.
// This mirrors clewdr's first_user_message_text and feeds the billing-header sampling.
func (r ChatRequest) FirstUserText() string {
	messages, ok := r.body["messages"].([]any)
	if !ok {
		return ""
	}

	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := message["role"].(string); role != "user" {
			continue
		}
		return firstText(message["content"])
	}
	return ""
}

// firstText extracts the text of a message content: the string itself, or the first
// "text" block of a content array.
func firstText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		return firstTextBlock(value)
	default:
		return ""
	}
}

// firstTextBlock returns the text of the first "text" block in a content array.
func firstTextBlock(blocks []any) string {
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if blockType, _ := block["type"].(string); blockType != "text" {
			continue
		}
		if text, ok := block["text"].(string); ok {
			return text
		}
	}
	return ""
}

// WithClaudeCodeSystem returns a copy of the request with block prepended as the first
// system text block. It handles an absent, string, or array system, mirroring clewdr's
// prepend_system_blocks. An empty/whitespace string system is treated as absent.
func (r ChatRequest) WithClaudeCodeSystem(block string) ChatRequest {
	body := cloneTopLevel(r.body)
	body["system"] = prependSystem(block, r.body["system"])
	return ChatRequest{body: body}
}

// prependSystem builds the new system array with block first, then the existing system.
func prependSystem(block string, existing any) []any {
	system := []any{textBlock(block)}

	switch value := existing.(type) {
	case string:
		if strings.TrimSpace(value) != "" {
			system = append(system, textBlock(value))
		}
	case []any:
		system = append(system, value...)
	case nil:
		// No existing system: nothing to append.
	default:
		system = append(system, value)
	}
	return system
}

// textBlock builds an Anthropic system text block.
func textBlock(text string) map[string]any {
	return map[string]any{"type": "text", "text": text}
}

// cloneTopLevel shallow-copies a body map so injecting a system block does not mutate
// the original request's map (nested values are shared but never modified in place).
func cloneTopLevel(body map[string]any) map[string]any {
	clone := make(map[string]any, len(body)+1)
	for key, value := range body {
		clone[key] = value
	}
	return clone
}

// Normalize returns a copy of the request with the minimal transforms the OAuth surface requires
// before forwarding, mirroring clewdr's drop_empty_system + strip_ephemeral_scope_from_system:
// whitespace-only system text blocks are dropped and the ephemeral cache_control "scope" is
// stripped from the remaining blocks. It also drops the top-level context_management parameter,
// which Claude Code 2.1.x sends but the OAuth surface rejects as an unknown field
// ("context_management: Extra inputs are not permitted"). Consumers using prompt caching or
// context editing send these and Anthropic rejects them otherwise. Everything else is left
// untouched, and the original request is never mutated.
func (r ChatRequest) Normalize() ChatRequest {
	body := cloneTopLevel(r.body)
	delete(body, "context_management")

	if system, ok := body["system"].([]any); ok {
		body["system"] = normalizeSystem(system)
	}

	return ChatRequest{body: body}
}

// normalizeSystem returns a new system array with blank text blocks removed and the ephemeral
// scope stripped from the remaining blocks.
func normalizeSystem(blocks []any) []any {
	normalized := make([]any, 0, len(blocks))
	for _, raw := range blocks {
		block, ok := raw.(map[string]any)
		if !ok {
			normalized = append(normalized, raw)
			continue
		}
		if isBlankTextBlock(block) {
			continue
		}
		normalized = append(normalized, stripEphemeralScope(block))
	}
	return normalized
}

// isBlankTextBlock reports whether a system block is a text block whose text is empty or only
// whitespace (clewdr drop_empty_system parity, applied per block).
func isBlankTextBlock(block map[string]any) bool {
	if blockType, _ := block["type"].(string); blockType != "text" {
		return false
	}
	text, _ := block["text"].(string)
	return strings.TrimSpace(text) == ""
}

// stripEphemeralScope returns the block with the ephemeral cache_control "scope" removed,
// covering both clewdr shapes: cache_control.ephemeral.scope and cache_control with
// type "ephemeral" carrying a top-level scope. The block (and the affected sub-maps) are
// cloned only when a scope is actually present, so the original request stays untouched and
// the rest of cache_control is preserved.
func stripEphemeralScope(block map[string]any) map[string]any {
	cacheControl, ok := block["cache_control"].(map[string]any)
	if !ok {
		return block
	}

	cleaned, changed := cacheControlWithoutScope(cacheControl)
	if !changed {
		return block
	}

	out := cloneTopLevel(block)
	out["cache_control"] = cleaned
	return out
}

// cacheControlWithoutScope returns a copy of a cache_control map with the ephemeral scope
// removed and reports whether anything was removed.
func cacheControlWithoutScope(cacheControl map[string]any) (map[string]any, bool) {
	cleaned := cloneTopLevel(cacheControl)
	changed := false

	if ephemeral, ok := cleaned["ephemeral"].(map[string]any); ok {
		if _, has := ephemeral["scope"]; has {
			ephemeral = cloneTopLevel(ephemeral)
			delete(ephemeral, "scope")
			cleaned["ephemeral"] = ephemeral
			changed = true
		}
	}

	if controlType, _ := cleaned["type"].(string); controlType == "ephemeral" {
		if _, has := cleaned["scope"]; has {
			delete(cleaned, "scope")
			changed = true
		}
	}

	return cleaned, changed
}

// Bytes serialises the request body for forwarding upstream.
func (r ChatRequest) Bytes() []byte {
	encoded, _ := json.Marshal(r.body)
	return encoded
}
