package integration

import (
	"fmt"
	"net/http"
)

// toolPrefixHistoryBody declares one tool, forces its choice, and replays an
// assistant turn that called it — the three outbound locations the rewrite
// must cover together (spec §7).
const toolPrefixHistoryBody = `{
	"model": "test-model",
	"max_tokens": 16,
	"tools": [
		{
			"name": "search_web",
			"description": "search the web",
			"input_schema": {"type": "object", "properties": {"query": {"type": "string"}}}
		}
	],
	"tool_choice": {"type": "tool", "name": "search_web"},
	"messages": [
		{"role": "user", "content": "fixture-prompt"},
		{
			"role": "assistant",
			"content": [
				{"type": "tool_use", "id": "toolu_prefix_history", "name": "search_web", "input": {"query": "weather"}}
			]
		},
		{
			"role": "user",
			"content": [
				{"type": "tool_result", "tool_use_id": "toolu_prefix_history", "content": "fixture-tool-result"}
			]
		}
	]
}`

// toolPrefixDeclarationBody declares one tool with no history, for inbound
// (response-side) rewrite scenarios that only need a well-formed request.
const toolPrefixDeclarationBody = `{
	"model": "test-model",
	"max_tokens": 16,
	"tools": [
		{"name": "search_web", "description": "search the web", "input_schema": {"type": "object", "properties": {}}}
	],
	"messages": [{"role": "user", "content": "fixture-prompt"}]
}`

// toolPrefixExemptToolsBody declares both exempt kinds — an Anthropic-defined
// tool, recognised by its "type" and not its name, and a client MCP tool the
// embedded SDK already forwards verbatim — alongside a plain client tool, so
// one request carries every side of the exemption.
const toolPrefixExemptToolsBody = `{
	"model": "test-model",
	"max_tokens": 16,
	"tools": [
		{"type": "web_search_20260209", "name": "web_search"},
		{
			"name": "mcp__notion__search",
			"description": "search notion",
			"input_schema": {"type": "object", "properties": {}}
		},
		{
			"name": "search_web",
			"description": "search the web",
			"input_schema": {"type": "object", "properties": {}}
		}
	],
	"messages": [{"role": "user", "content": "fixture-prompt"}]
}`

// toolPrefixCountBody returns one count_tokens payload declaring a single tool
// under the given name, so two payloads built from it differ by that name and
// nothing else.
func toolPrefixCountBody(name string) string {
	return fmt.Sprintf(`{
	"model": "test-model",
	"max_tokens": 16,
	"tools": [
		{"name": %q, "description": "search the web", "input_schema": {"type": "object", "properties": {}}}
	],
	"messages": [{"role": "user", "content": "fixture-prompt"}]
}`, name)
}

// toolPrefixNoToolBody is toolPrefixCountBody's payload with the declaration
// removed, the baseline proving tool declarations reach the token count at all.
const toolPrefixNoToolBody = `{
	"model": "test-model",
	"max_tokens": 16,
	"messages": [{"role": "user", "content": "fixture-prompt"}]
}`

// toolPrefixStreamingDeclarationBody is toolPrefixDeclarationBody with streaming enabled.
const toolPrefixStreamingDeclarationBody = `{
	"model": "test-model",
	"max_tokens": 16,
	"stream": true,
	"tools": [
		{"name": "search_web", "description": "search the web", "input_schema": {"type": "object", "properties": {}}}
	],
	"messages": [{"role": "user", "content": "fixture-prompt"}]
}`

// toolCallResponse returns a non-streamed OpenAI chat completion whose only
// choice is a single tool call naming the given function.
func toolCallResponse(name string) StubResponse {
	return StubResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body: `{
			"id": "chatcmpl-toolprefix",
			"object": "chat.completion",
			"created": 1,
			"model": "upstream-model",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": null,
					"tool_calls": [{
						"id": "call_toolprefix",
						"type": "function",
						"function": {"name": "` + name + `", "arguments": "{}"}
					}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 4, "completion_tokens": 3, "total_tokens": 7}
		}`,
	}
}

// toolCallStreamResponse returns a streamed OpenAI chat completion whose tool
// call carries its name and id in the first delta, exactly as the real
// upstream protocol shapes a single-chunk tool call.
func toolCallStreamResponse(name string) StubResponse {
	return StubResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: `data: {"id":"chatcmpl-toolprefix-stream","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_toolprefix_stream","type":"function","function":{"name":"` + name + `","arguments":"{}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-toolprefix-stream","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}

data: [DONE]

`,
	}
}

// splitToolCallStreamResponse returns a streamed OpenAI chat completion whose
// tool call announces its name in the first delta and spreads its arguments
// across two further deltas — the standard OpenAI incremental tool-call shape
// that must still surface as one coherent content_block_start.
func splitToolCallStreamResponse(name string) StubResponse {
	return StubResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: `data: {"id":"chatcmpl-toolprefix-split","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_toolprefix_split","type":"function","function":{"name":"` + name + `","arguments":""}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-toolprefix-split","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-toolprefix-split","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"weather\"}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-toolprefix-split","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":5,"total_tokens":9}}

data: [DONE]

`,
	}
}

// invalidRequestUpstreamError returns a client-error response the SDK
// classifies as non-retryable (isRequestInvalidError), so exactly one script
// is consumed and no account failover changes what the client observes.
func invalidRequestUpstreamError() StubResponse {
	return StubResponse{
		Status:  http.StatusBadRequest,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body:    `{"error":{"type":"invalid_request_error","message":"` + upstreamFailureSecret + `"}}`,
	}
}
