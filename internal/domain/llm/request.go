package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

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

// Bytes serialises the request body for forwarding upstream.
func (r ChatRequest) Bytes() []byte {
	encoded, _ := json.Marshal(r.body)
	return encoded
}
