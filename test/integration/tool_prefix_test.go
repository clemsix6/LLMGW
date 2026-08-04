package integration

import (
	"bytes"
	"context"
	"net/http"
	"testing"

	"github.com/clemsix6/LLMGW/internal/domain/governance"
	"github.com/tidwall/gjson"
)

// TestToolPrefixOutboundRewrite proves the flag namespaces every outbound
// location together — declarations, forced choice, and replayed history — and
// that a project without the flag reaches the stub unmodified. Assertions read
// the OpenAI chat-completions shape the stub actually receives after
// translation (spec §11), not the Anthropic shape the client sent.
func TestToolPrefixOutboundRewrite(t *testing.T) {
	t.Run("flag off leaves tool names untouched", func(t *testing.T) {
		created := testHarness.createKey(t, "toolprefix-outbound-off")
		testHarness.Upstream.Enqueue(defaultStubResponse())
		status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
			bytes.NewBufferString(toolPrefixHistoryBody),
			requestHeaders{authorization: "Bearer " + created.Plaintext})
		if status != http.StatusOK {
			t.Fatalf("flag-off status = %d, want 200; body=%s", status, safeBodySummary(body))
		}
		assertUpstreamToolNames(t, "search_web")
	})

	t.Run("flag on namespaces declarations, choice, and history", func(t *testing.T) {
		created := testHarness.createKey(t, "toolprefix-outbound-on")
		testHarness.enableToolPrefix(t, created)
		testHarness.Upstream.Enqueue(defaultStubResponse())
		status, body := gatewayRequest(t, http.MethodPost, "/v1/messages",
			bytes.NewBufferString(toolPrefixHistoryBody),
			requestHeaders{authorization: "Bearer " + created.Plaintext})
		if status != http.StatusOK {
			t.Fatalf("flag-on status = %d, want 200; body=%s", status, safeBodySummary(body))
		}
		assertUpstreamToolNames(t, "new_search_web")
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
		testHarness.enableToolPrefix(t, created)
		testHarness.Upstream.Enqueue(toolCallResponse("new_search_web"))
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
		testHarness.enableToolPrefix(t, created)
		testHarness.Upstream.Enqueue(toolCallStreamResponse("new_search_web"))
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
		testHarness.enableToolPrefix(t, created)
		testHarness.Upstream.Enqueue(splitToolCallStreamResponse("new_search_web"))
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
		testHarness.enableToolPrefix(t, created)
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
// rewritten payload: token counting is answered locally and issues no
// upstream call, so a larger count from the flagged project is the only
// observable proof the rewrite reached it (spec §11).
func TestToolPrefixCountTokensReflectsRewrite(t *testing.T) {
	unflagged := testHarness.createKey(t, "toolprefix-count-off")
	flagged := testHarness.createKey(t, "toolprefix-count-on")
	testHarness.enableToolPrefix(t, flagged)

	offCount := toolPrefixInputTokens(t, unflagged.Plaintext)
	onCount := toolPrefixInputTokens(t, flagged.Plaintext)
	if onCount <= offCount {
		t.Fatalf("flagged input_tokens = %v, want greater than unflagged %v", onCount, offCount)
	}
}

// TestToolPrefixNonOKPassesThroughUnchanged proves a non-200 upstream response
// is unaffected by the flag: the same upstream failure produces an identical
// client-visible status and body whether or not the project namespaces tool
// names, because error bodies are never rewritten.
func TestToolPrefixNonOKPassesThroughUnchanged(t *testing.T) {
	unflagged := testHarness.createKey(t, "toolprefix-nonok-off")
	flagged := testHarness.createKey(t, "toolprefix-nonok-on")
	testHarness.enableToolPrefix(t, flagged)

	testHarness.Upstream.Enqueue(invalidRequestUpstreamError())
	offStatus, offBody := gatewayRequest(t, http.MethodPost, "/v1/messages",
		bytes.NewBufferString(toolPrefixDeclarationBody),
		requestHeaders{authorization: "Bearer " + unflagged.Plaintext})

	testHarness.Upstream.Enqueue(invalidRequestUpstreamError())
	onStatus, onBody := gatewayRequest(t, http.MethodPost, "/v1/messages",
		bytes.NewBufferString(toolPrefixDeclarationBody),
		requestHeaders{authorization: "Bearer " + flagged.Plaintext})

	if offStatus == http.StatusOK || onStatus != offStatus {
		t.Fatalf("non-200 status diverged: off=%d on=%d", offStatus, onStatus)
	}
	if !bytes.Equal(offBody, onBody) {
		t.Fatalf("non-200 body diverged with the flag on:\noff=%s\non=%s", offBody, onBody)
	}
}

// enableToolPrefix flips one project's tool-name-prefix flag through the real
// store method, so the suite exercises the operator surface rather than
// setting up fixtures with raw SQL.
func (h *Harness) enableToolPrefix(t *testing.T, created governance.CreatedKey) {
	t.Helper()
	if err := h.Store.SetProjectToolPrefix(context.Background(), created.Key.ProjectName, true); err != nil {
		t.Fatal("enable integration tool-name prefix failed")
	}
}

// assertUpstreamToolNames checks the most recently captured upstream request
// body carries the expected name at all three outbound locations translation
// produces: the tool declaration, the forced choice, and the replayed
// assistant tool call.
func assertUpstreamToolNames(t *testing.T, want string) {
	t.Helper()
	bodies := testHarness.Upstream.Bodies()
	if len(bodies) == 0 {
		t.Fatal("upstream captured no request body")
	}
	body := bodies[len(bodies)-1]

	if got := gjson.GetBytes(body, "tools.0.function.name").String(); got != want {
		t.Fatalf("tools.0.function.name = %q, want %q", got, want)
	}
	if got := gjson.GetBytes(body, "tool_choice.function.name").String(); got != want {
		t.Fatalf("tool_choice.function.name = %q, want %q", got, want)
	}
	history := gjson.GetBytes(body, `messages.#(role=="assistant").tool_calls.0.function.name`)
	if got := history.String(); got != want {
		t.Fatalf("replayed history tool name = %q, want %q", got, want)
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

// toolPrefixInputTokens issues one count_tokens request declaring a tool and
// returns the reported input_tokens value.
func toolPrefixInputTokens(t *testing.T, plaintext string) float64 {
	t.Helper()
	status, body := gatewayRequest(t, http.MethodPost, "/v1/messages/count_tokens",
		bytes.NewBufferString(toolPrefixDeclarationBody),
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
