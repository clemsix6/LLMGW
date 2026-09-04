package integration

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/tidwall/gjson"
)

// TestToolPrefixOutboundRewrite proves every project's outbound tool names are
// rewritten at all three locations together — declarations, forced choice, and
// replayed history — with no opt-in anywhere. Assertions read the OpenAI
// chat-completions shape the stub actually receives after translation (spec
// §11), not the Anthropic shape the client sent.
func TestToolPrefixOutboundRewrite(t *testing.T) {
	created := testHarness.createKey(t, "toolprefix-outbound")
	testHarness.Upstream.Enqueue(defaultStubResponse())

	status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
		bytes.NewBufferString(toolPrefixHistoryBody),
		requestHeaders{authorization: "Bearer " + created.Plaintext})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, safeBodySummary(body))
	}

	assertUpstreamToolNames(t, "mcp__llmgw__search_web")
}

// TestToolPrefixExemptions proves the names the rewrite must leave alone reach
// the upstream exactly as the client sent them: a tool Anthropic defines,
// recognised by its "type" and known upstream under one name, and a client's
// own MCP tool, which the embedded SDK already forwards verbatim. The client's
// plain tool in the same request is still rewritten.
func TestToolPrefixExemptions(t *testing.T) {
	created := testHarness.createKey(t, "toolprefix-exemptions")
	testHarness.Upstream.Enqueue(defaultStubResponse())

	status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
		bytes.NewBufferString(toolPrefixExemptToolsBody),
		requestHeaders{authorization: "Bearer " + created.Plaintext})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, safeBodySummary(body))
	}

	assertUpstreamNamesAt(t, map[string]string{
		"tools.0.function.name": "web_search",
		"tools.1.function.name": "mcp__notion__search",
		"tools.2.function.name": "mcp__llmgw__search_web",
	})
}

// TestToolPrefixInboundRewrite proves the response-side strip is the exact
// inverse of the outbound rewrite: a prefixed name upstream returns comes back
// stripped to the client in both non-streamed and streamed form, a split
// streamed tool call still surfaces as one coherent content_block_start, and a
// name that never carried the prefix is forwarded unchanged.
func TestToolPrefixInboundRewrite(t *testing.T) {
	t.Run("non-streamed prefixed name is stripped", func(t *testing.T) {
		created := testHarness.createKey(t, "toolprefix-inbound-nonstream")
		testHarness.Upstream.Enqueue(toolCallResponse("mcp__llmgw__search_web"))
		status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
			bytes.NewBufferString(toolPrefixDeclarationBody),
			requestHeaders{authorization: "Bearer " + created.Plaintext})
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", status, safeBodySummary(body))
		}
		if got := toolUseName(t, decodeJSON(t, body)); got != "search_web" {
			t.Fatalf("client tool_use name = %q, want %q", got, "search_web")
		}
	})

	t.Run("streamed prefixed name is stripped", func(t *testing.T) {
		created := testHarness.createKey(t, "toolprefix-inbound-stream")
		testHarness.Upstream.Enqueue(toolCallStreamResponse("mcp__llmgw__search_web"))
		status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
			bytes.NewBufferString(toolPrefixStreamingDeclarationBody),
			requestHeaders{authorization: "Bearer " + created.Plaintext})
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", status, safeBodySummary(body))
		}
		assertSingleToolUseStart(t, body, "search_web")
	})

	t.Run("split streamed tool call still starts once", func(t *testing.T) {
		created := testHarness.createKey(t, "toolprefix-inbound-split")
		testHarness.Upstream.Enqueue(splitToolCallStreamResponse("mcp__llmgw__search_web"))
		status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
			bytes.NewBufferString(toolPrefixStreamingDeclarationBody),
			requestHeaders{authorization: "Bearer " + created.Plaintext})
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", status, safeBodySummary(body))
		}
		assertSingleToolUseStart(t, body, "search_web")
	})

	t.Run("name without the prefix is forwarded unchanged", func(t *testing.T) {
		created := testHarness.createKey(t, "toolprefix-inbound-unprefixed")
		testHarness.Upstream.Enqueue(toolCallResponse("server_tool"))
		status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
			bytes.NewBufferString(toolPrefixDeclarationBody),
			requestHeaders{authorization: "Bearer " + created.Plaintext})
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", status, safeBodySummary(body))
		}
		if got := toolUseName(t, decodeJSON(t, body)); got != "server_tool" {
			t.Fatalf("client tool_use name = %q, want unchanged %q", got, "server_tool")
		}
	})
}

// TestToolPrefixCountTokensReflectsRewrite proves count_tokens counts the
// rewritten payload: token counting is answered locally and issues no upstream
// call, so the count is the only observable. A plain name and the same name
// already carrying the prefix must count identically, since the rewrite makes
// the two payloads the same one — and both must exceed the count of the same
// request declaring no tool at all, without which that equality would hold
// whether or not the declarations were counted.
func TestToolPrefixCountTokensReflectsRewrite(t *testing.T) {
	created := testHarness.createKey(t, "toolprefix-count")

	plain := toolPrefixInputTokens(t, created.Plaintext, toolPrefixCountBody("search_web"))
	prefixed := toolPrefixInputTokens(t, created.Plaintext, toolPrefixCountBody("mcp__llmgw__search_web"))
	if plain != prefixed {
		t.Fatalf("input_tokens = %v for a plain name and %v for an already-prefixed one, want equal",
			plain, prefixed)
	}

	withoutTools := toolPrefixInputTokens(t, created.Plaintext, toolPrefixNoToolBody)
	if plain <= withoutTools {
		t.Fatalf("input_tokens = %v with a tool and %v without, want the declaration counted",
			plain, withoutTools)
	}
}

// TestToolPrefixNonOKPassesThroughUnchanged proves the response wrapper now
// installed on every generation leaves an upstream failure alone: the client
// still receives a non-200 carrying the upstream error envelope, not a body
// the wrapper held, truncated, or rewrote.
func TestToolPrefixNonOKPassesThroughUnchanged(t *testing.T) {
	created := testHarness.createKey(t, "toolprefix-nonok")
	testHarness.Upstream.Enqueue(invalidRequestUpstreamError())

	status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
		bytes.NewBufferString(toolPrefixDeclarationBody),
		requestHeaders{authorization: "Bearer " + created.Plaintext})
	if status == http.StatusOK {
		t.Fatalf("status = %d, want an upstream failure", status)
	}
	if !gjson.GetBytes(body, "error").Exists() {
		t.Fatalf("client body = %s, want an error envelope", safeBodySummary(body))
	}
}

// assertUpstreamToolNames checks the most recently captured upstream request
// body carries the expected name at all three outbound locations translation
// produces: the tool declaration, the forced choice, and the replayed
// assistant tool call.
func assertUpstreamToolNames(t *testing.T, want string) {
	t.Helper()
	const historyPath = `messages.#(role=="assistant").tool_calls.0.function.name`
	assertUpstreamNamesAt(t, map[string]string{
		"tools.0.function.name":     want,
		"tool_choice.function.name": want,
		historyPath:                 want,
	})
}

// assertUpstreamNamesAt checks the most recently captured upstream request
// body against the expected name at each given path — the form the assertion
// takes when one request's tool names are not all rewritten the same way.
func assertUpstreamNamesAt(t *testing.T, want map[string]string) {
	t.Helper()
	bodies := testHarness.Upstream.Bodies()
	if len(bodies) == 0 {
		t.Fatal("upstream captured no request body")
	}
	body := bodies[len(bodies)-1]

	for path, expected := range want {
		if got := gjson.GetBytes(body, path).String(); got != expected {
			t.Fatalf("%s = %q, want %q", path, got, expected)
		}
	}
}

// toolUseName returns the name of the first tool_use content block in a
// decoded Anthropic-format response.
func toolUseName(t *testing.T, object map[string]any) string {
	t.Helper()
	content, ok := object["content"].([]any)
	if !ok {
		t.Fatalf("response content is not an array: %#v", object["content"])
	}
	for _, item := range content {
		block, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if block["type"] == "tool_use" {
			name, _ := block["name"].(string)
			return name
		}
	}
	t.Fatal("response carries no tool_use content block")
	return ""
}

// assertSingleToolUseStart checks a streamed Anthropic response carries
// exactly one content_block_start of type tool_use, naming want — proving
// both that the name was stripped and that a multi-delta upstream tool call
// still surfaces as one coherent start event.
func assertSingleToolUseStart(t *testing.T, body []byte, want string) {
	t.Helper()
	frames, err := parseSSEFrames(body)
	if err != nil {
		t.Fatalf("parse tool-prefix stream: %v", err)
	}
	found := 0
	for _, frame := range frames {
		if frame.Event != "content_block_start" {
			continue
		}
		block, ok := frame.Data["content_block"].(map[string]any)
		if !ok || block["type"] != "tool_use" {
			continue
		}
		found++
		if got, _ := block["name"].(string); got != want {
			t.Fatalf("content_block_start name = %q, want %q", got, want)
		}
	}
	if found != 1 {
		t.Fatalf("content_block_start(tool_use) count = %d, want 1", found)
	}
}

// toolPrefixInputTokens issues one count_tokens request and returns the
// reported input_tokens value.
func toolPrefixInputTokens(t *testing.T, plaintext string, payload string) float64 {
	t.Helper()
	status, body := gatewayRequest(t, http.MethodPost, "/v1/messages/count_tokens",
		bytes.NewBufferString(payload),
		requestHeaders{authorization: "Bearer " + plaintext})
	if status != http.StatusOK {
		t.Fatalf("count_tokens status = %d, want 200; body=%s", status, safeBodySummary(body))
	}
	object := decodeJSON(t, body)
	value, ok := object["input_tokens"].(float64)
	if !ok {
		t.Fatalf("count_tokens response missing input_tokens: %v", object)
	}
	return value
}
