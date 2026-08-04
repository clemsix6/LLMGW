package integration

import "net/http"

// effortPlainBody is one Anthropic generation naming no effort of its own,
// addressed to the claude-format provider so the payload the stub captures is
// the one the gateway wrote (spec §10).
const effortPlainBody = `{
	"model": "anthropic-test-model",
	"max_tokens": 16,
	"messages": [{"role": "user", "content": "fixture-prompt"}]
}`

// effortClientLevelBody names an effort of the client's own, which the project
// default must never replace.
const effortClientLevelBody = `{
	"model": "anthropic-test-model",
	"max_tokens": 16,
	"output_config": {"effort": "low"},
	"messages": [{"role": "user", "content": "fixture-prompt"}]
}`

// effortDisabledThinkingBody disables thinking, which Anthropic accepts only
// at effort "high" or below: injecting a higher level beside it would return a
// 400 the client would never have received on its own.
const effortDisabledThinkingBody = `{
	"model": "anthropic-test-model",
	"max_tokens": 16,
	"thinking": {"type": "disabled"},
	"messages": [{"role": "user", "content": "fixture-prompt"}]
}`

// effortToolBody declares one tool and names no effort, so a project enabling
// both settings exercises both transformations in a single request.
const effortToolBody = `{
	"model": "anthropic-test-model",
	"max_tokens": 16,
	"tools": [
		{"name": "search_web", "description": "search the web", "input_schema": {"type": "object", "properties": {}}}
	],
	"messages": [{"role": "user", "content": "fixture-prompt"}]
}`

// anthropicStubResponse returns a valid non-streamed Anthropic message. The
// claude-format executor forwards the response without translating it, so the
// fixture must already carry the shape and usage the gateway accounts.
func anthropicStubResponse() StubResponse {
	return StubResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body: `{
			"id": "msg_integration_effort",
			"type": "message",
			"role": "assistant",
			"model": "claude-upstream-model",
			"content": [{"type": "text", "text": "fixture-response"}],
			"stop_reason": "end_turn",
			"stop_sequence": null,
			"usage": {"input_tokens": 3, "output_tokens": 2}
		}`,
	}
}
