package integration

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
)

// markupGuardRejectionBody is the stable envelope the gateway substitutes for
// a screened-out corrupted success.
const markupGuardRejectionBody = `{"error":{"type":"upstream_protocol_error"}}`

// corruptedToolCallResponse returns a non-streamed OpenAI chat completion
// whose tool-call arguments carry the leaked function-call markup observed in
// production: a mangled closing tag followed by the next parameter's opening.
func corruptedToolCallResponse() StubResponse {
	return StubResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
		Body: `{
			"id": "chatcmpl-markupguard",
			"object": "chat.completion",
			"created": 1,
			"model": "upstream-model",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": null,
					"tool_calls": [{
						"id": "call_markupguard",
						"type": "function",
						"function": {"name": "search_web", "arguments": "{\"query\":\"</antml railway> <parameter name=\\\"body\\\">Druck\"}"}
					}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {"prompt_tokens": 4, "completion_tokens": 3, "total_tokens": 7}
		}`,
	}
}

// corruptedToolCallStreamResponse returns the same leak in streamed form.
func corruptedToolCallStreamResponse() StubResponse {
	return StubResponse{
		Status:  http.StatusOK,
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body: `data: {"id":"chatcmpl-markupguard-stream","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_markupguard_stream","type":"function","function":{"name":"search_web","arguments":"{\"query\":\"</antml railway>\"}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-markupguard-stream","object":"chat.completion.chunk","created":1,"model":"upstream-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":4,"completion_tokens":3,"total_tokens":7}}

data: [DONE]

`,
	}
}

// enableMarkupGuard flips one project's markup-guard flag through the real
// store method, so the suite exercises the operator surface rather than
// setting up fixtures with raw SQL.
func (h *Harness) enableMarkupGuard(t *testing.T, created governance.CreatedKey) {
	t.Helper()
	if err := h.Store.SetProjectMarkupGuard(context.Background(), created.Key.ProjectName, true); err != nil {
		t.Fatal("enable integration markup guard failed")
	}
}

// TestMarkupGuardRejectsCorruptedSuccess proves a flagged project's corrupted
// 200 becomes the stable retryable 502, while the same upstream response
// reaches an unflagged project untouched — current behaviour stays the
// default.
func TestMarkupGuardRejectsCorruptedSuccess(t *testing.T) {
	t.Run("flag on turns the corrupted success into a 502", func(t *testing.T) {
		created := testHarness.createKey(t, "markupguard-on")
		testHarness.enableMarkupGuard(t, created)
		testHarness.Upstream.Enqueue(corruptedToolCallResponse())
		status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
			bytes.NewBufferString(toolPrefixDeclarationBody),
			requestHeaders{authorization: "Bearer " + created.Plaintext})
		if status != http.StatusBadGateway {
			t.Fatalf("status = %d, want 502; body=%s", status, safeBodySummary(body))
		}
		if string(body) != markupGuardRejectionBody {
			t.Fatalf("body = %q, want the rejection envelope", body)
		}
	})

	t.Run("flag off forwards the corrupted success unchanged", func(t *testing.T) {
		created := testHarness.createKey(t, "markupguard-off")
		testHarness.Upstream.Enqueue(corruptedToolCallResponse())
		status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
			bytes.NewBufferString(toolPrefixDeclarationBody),
			requestHeaders{authorization: "Bearer " + created.Plaintext})
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", status, safeBodySummary(body))
		}
		if !bytes.Contains(body, []byte("</antml")) {
			t.Fatalf("body = %q, want the leak forwarded to the unflagged project", safeBodySummary(body))
		}
	})
}

// TestMarkupGuardForwardsCleanResponses proves the screen is invisible on the
// path it exists for: a clean tool call reaches the flagged client intact.
func TestMarkupGuardForwardsCleanResponses(t *testing.T) {
	created := testHarness.createKey(t, "markupguard-clean")
	testHarness.enableMarkupGuard(t, created)
	testHarness.Upstream.Enqueue(toolCallResponse("search_web"))

	status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
		bytes.NewBufferString(toolPrefixDeclarationBody),
		requestHeaders{authorization: "Bearer " + created.Plaintext})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, safeBodySummary(body))
	}
	if got := toolUseName(t, decodeJSON(t, body)); got != "search_web" {
		t.Fatalf("client tool_use name = %q, want %q", got, "search_web")
	}
}

// TestMarkupGuardLeavesStreamsAlone pins the documented streaming limit: a
// flagged project's streamed response flows through unscreened, leak included.
func TestMarkupGuardLeavesStreamsAlone(t *testing.T) {
	created := testHarness.createKey(t, "markupguard-stream")
	testHarness.enableMarkupGuard(t, created)
	testHarness.Upstream.Enqueue(corruptedToolCallStreamResponse())

	status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
		bytes.NewBufferString(toolPrefixStreamingDeclarationBody),
		requestHeaders{authorization: "Bearer " + created.Plaintext})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, safeBodySummary(body))
	}
	// The stream translation may JSON-escape the leaked "</" as "<\/", so the
	// namespace alone is what proves the leak reached the client.
	if !bytes.Contains(body, []byte("antml")) {
		t.Fatalf("stream = %q, want the leak forwarded verbatim", safeBodySummary(body))
	}
}

// TestMarkupGuardPassesUpstreamErrorsThrough proves an upstream failure is
// unaffected by the flag: error bodies are never screened or rewritten.
func TestMarkupGuardPassesUpstreamErrorsThrough(t *testing.T) {
	created := testHarness.createKey(t, "markupguard-error")
	testHarness.enableMarkupGuard(t, created)
	testHarness.Upstream.Enqueue(invalidRequestUpstreamError())

	status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
		bytes.NewBufferString(toolPrefixDeclarationBody),
		requestHeaders{authorization: "Bearer " + created.Plaintext})
	if status == http.StatusOK || status == http.StatusBadGateway {
		t.Fatalf("status = %d, want the upstream error status forwarded", status)
	}
	if !bytes.Contains(body, []byte("invalid_request_error")) {
		t.Fatalf("body = %q, want the upstream error body forwarded", safeBodySummary(body))
	}
}
