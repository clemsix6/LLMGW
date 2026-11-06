package integration

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestResponsesStayIdentityEncoded proves a client advertising compression
// still receives a plain response its tool names were stripped from. The
// embedded SDK negotiates compression beneath LLMGW's response wrappers, so a
// compressed body would reach the client with the prefix still on every name.
// The request sets Accept-Encoding itself, which stops the Go transport from
// decompressing transparently: a compressed response stays visible here.
func TestResponsesStayIdentityEncoded(t *testing.T) {
	t.Run("streamed", func(t *testing.T) {
		created := testHarness.createKey(t, "encoding-stream")
		testHarness.Upstream.Enqueue(toolCallStreamResponse("mcp__llmgw__search_web"))

		encoding, body := gzipAcceptingRequest(t, created.Plaintext, toolPrefixStreamingDeclarationBody)
		if encoding != "" {
			t.Fatalf("Content-Encoding = %q, want identity", encoding)
		}
		assertSingleToolUseStart(t, body, "search_web")
	})

	t.Run("non-streamed above the compression threshold", func(t *testing.T) {
		created := testHarness.createKey(t, "encoding-nonstream")
		testHarness.Upstream.Enqueue(largeToolCallResponse("mcp__llmgw__search_web"))

		encoding, body := gzipAcceptingRequest(t, created.Plaintext, toolPrefixDeclarationBody)
		if encoding != "" {
			t.Fatalf("Content-Encoding = %q, want identity", encoding)
		}
		if got := toolUseName(t, decodeJSON(t, body)); got != "search_web" {
			t.Fatalf("client tool_use name = %q, want %q", got, "search_web")
		}
	})
}

// gzipAcceptingRequest sends one /v1/messages request advertising gzip and
// returns the response's Content-Encoding with its raw, undecoded body.
func gzipAcceptingRequest(t *testing.T, key string, payload string) (string, []byte) {
	t.Helper()
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodPost,
		testHarness.BaseURL+"/v1/messages",
		bytes.NewBufferString(payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Accept-Encoding", "gzip")

	response, err := testHarness.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", response.StatusCode, safeBodySummary(body))
	}
	return response.Header.Get("Content-Encoding"), body
}

// largeToolCallResponse is toolCallResponse with arguments long enough that
// any size threshold on response compression is crossed.
func largeToolCallResponse(name string) StubResponse {
	arguments := `{\"query\":\"` + strings.Repeat("weather ", 2048) + `\"}`
	response := toolCallResponse(name)
	response.Body = strings.Replace(response.Body, `"arguments": "{}"`, `"arguments": "`+arguments+`"`, 1)
	return response
}
