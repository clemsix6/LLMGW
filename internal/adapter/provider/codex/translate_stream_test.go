package codex

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// captureSink is a test StreamSink that collects all written bytes.
type captureSink struct {
	buf bytes.Buffer // buf accumulates every Write call.
}

// Write appends p to the internal buffer.
func (c *captureSink) Write(p []byte) (int, error) { return c.buf.Write(p) }

// Flush is a no-op for the test capture sink.
func (c *captureSink) Flush() {}

// String returns the accumulated output as a string.
func (c *captureSink) String() string { return c.buf.String() }

// readTestdataReader returns the named testdata file as an io.Reader.
func readTestdataReader(t *testing.T, name string) io.Reader {
	t.Helper()
	return bytes.NewReader(readTestdata(t, name))
}

// TestRelayTranslatedStreamProducesCleanChunks is the canonical brief test: a realistic
// Responses SSE sequence (with instructions, reasoning, output text deltas, a function call,
// and response.completed with usage) must yield a well-formed chat.completion.chunk stream
// that ends in [DONE], contains no Codex prompt or reasoning, and accumulates usage.
func TestRelayTranslatedStreamProducesCleanChunks(t *testing.T) {
	upstream := readTestdataReader(t, "responses_stream.sse")
	sink := &captureSink{}
	u, err := relayTranslatedStream(upstream, sink, true)
	if err != nil {
		t.Fatal(err)
	}
	s := sink.String()
	if !strings.Contains(s, `"delta"`) || !strings.HasSuffix(strings.TrimSpace(s), "data: [DONE]") {
		t.Fatal("not a well-formed Chat Completions SSE stream")
	}
	if strings.Contains(s, "instructions") || strings.Contains(s, "reasoning") {
		t.Fatal("Codex prompt or reasoning leaked")
	}
	if u.OutputTokens == 0 {
		t.Fatal("usage not accumulated")
	}
}

// TestRelayTranslatedStreamUsageChunk verifies that includeUsage=true appends a usage-only
// chunk before [DONE] and includeUsage=false omits it.
func TestRelayTranslatedStreamUsageChunk(t *testing.T) {
	withUsage := &captureSink{}
	if _, err := relayTranslatedStream(readTestdataReader(t, "responses_stream.sse"), withUsage, true); err != nil {
		t.Fatal(err)
	}

	withoutUsage := &captureSink{}
	if _, err := relayTranslatedStream(readTestdataReader(t, "responses_stream.sse"), withoutUsage, false); err != nil {
		t.Fatal(err)
	}

	// The with-usage stream has one extra chunk containing "prompt_tokens".
	if !strings.Contains(withUsage.String(), "prompt_tokens") {
		t.Fatal("expected usage chunk when includeUsage=true")
	}
	if strings.Contains(withoutUsage.String(), "prompt_tokens") {
		t.Fatal("usage chunk leaked when includeUsage=false")
	}
}

// TestRelayTranslatedStreamFunctionCall verifies that the function-call streaming events
// produce a tool_calls delta chunk and set finish_reason to "tool_calls". The SSE fixture
// has the function call at output_index=2 (preceded by a reasoning item and a message item),
// so this test also asserts the emitted tool_calls index is 0-based (0, not 2).
func TestRelayTranslatedStreamFunctionCall(t *testing.T) {
	sink := &captureSink{}
	if _, err := relayTranslatedStream(readTestdataReader(t, "responses_stream.sse"), sink, false); err != nil {
		t.Fatal(err)
	}
	s := sink.String()
	if !strings.Contains(s, "tool_calls") {
		t.Fatal("expected tool_calls in output stream")
	}
	if !strings.Contains(s, "get_weather") {
		t.Fatal("expected function name in output stream")
	}
	// Verify finish_reason is "tool_calls" (present in the final chunk).
	if !strings.Contains(s, `"tool_calls"`) {
		t.Fatal("expected finish_reason tool_calls")
	}
	// Verify the first tool call is emitted with sequential index 0, not the Responses
	// output_index (2). An SDK that allocates a slot array by index would get wrong results
	// if the pass-through output_index were used.
	for _, line := range strings.Split(s, "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			continue
		}
		var chunk completionChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, choice := range chunk.Choices {
			for _, tc := range choice.Delta.ToolCalls {
				if tc.Function.Name == "get_weather" && tc.Index != 0 {
					t.Fatalf("tool_calls[].index = %d, want 0 (must be 0-based sequential, not output_index)", tc.Index)
				}
			}
		}
	}
}

// TestRelayTranslatedStreamLargeCreatedEvent is the >64 KB regression test: a response.created
// event with an 80 KB instructions field must be consumed and dropped without error. This
// test proves the bufio.ReadBytes path handles lines well above the 64 KB Scanner limit.
func TestRelayTranslatedStreamLargeCreatedEvent(t *testing.T) {
	filler := strings.Repeat("x", 80*1024) // 80 KB, exceeds the 64 KB Scanner cap.
	createdData, _ := json.Marshal(map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":           "resp_large",
			"model":        "gpt-5.5",
			"instructions": filler,
		},
	})
	completedData, _ := json.Marshal(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     "resp_large",
			"model":  "gpt-5.5",
			"output": []any{},
			"usage":  map[string]any{"input_tokens": 1, "output_tokens": 1},
		},
	})
	sse := "data: " + string(createdData) + "\n\ndata: " + string(completedData) + "\n\ndata: [DONE]\n"

	sink := &captureSink{}
	_, err := relayTranslatedStream(strings.NewReader(sse), sink, false)
	if err != nil {
		t.Fatalf("failed on >64 KB response.created event: %v", err)
	}
	out := sink.String()
	if strings.Contains(out, filler[:20]) {
		t.Fatal("large instructions field leaked into stream output")
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("stream did not end with [DONE], got: %s", out)
	}
}

// TestRelayTranslatedStreamRealToolStream drives the streaming path against the real-format
// tool stream (responses_tool.sse) and verifies: tool_calls chunk with name "get_weather"
// and arguments from deltas, finish_reason "tool_calls", stream ends with [DONE].
func TestRelayTranslatedStreamRealToolStream(t *testing.T) {
	sink := &captureSink{}
	u, err := relayTranslatedStream(readTestdataReader(t, "responses_tool.sse"), sink, true)
	if err != nil {
		t.Fatal(err)
	}
	s := sink.String()
	if !strings.Contains(s, "get_weather") {
		t.Fatal("expected function name get_weather in stream output")
	}
	if !strings.Contains(s, "Paris") {
		t.Fatal("expected argument Paris in stream output")
	}
	if !strings.Contains(s, `"tool_calls"`) {
		t.Fatal("expected finish_reason tool_calls in stream output")
	}
	if !strings.HasSuffix(strings.TrimSpace(s), "data: [DONE]") {
		t.Fatalf("stream did not end with [DONE]: %s", s)
	}
	if strings.Contains(s, "instructions") || strings.Contains(s, "reasoning") {
		t.Fatal("instructions or reasoning leaked into stream output")
	}
	if u.InputTokens != 60 || u.OutputTokens != 18 {
		t.Fatalf("wrong usage from tool stream: %+v", u)
	}
}

// TestParseIncludeUsage verifies that parseIncludeUsage extracts stream_options.include_usage
// correctly from a Chat Completions body.
func TestParseIncludeUsage(t *testing.T) {
	if parseIncludeUsage([]byte(`{"stream_options":{"include_usage":true}}`)) != true {
		t.Fatal("expected true")
	}
	if parseIncludeUsage([]byte(`{"stream_options":{"include_usage":false}}`)) != false {
		t.Fatal("expected false")
	}
	if parseIncludeUsage([]byte(`{}`)) != false {
		t.Fatal("expected false for absent field")
	}
}

// TestRelayTranslatedStreamChunkShape verifies that each emitted line is a valid SSE data line
// containing a parseable chat.completion.chunk JSON object.
func TestRelayTranslatedStreamChunkShape(t *testing.T) {
	sink := &captureSink{}
	if _, err := relayTranslatedStream(readTestdataReader(t, "responses_stream.sse"), sink, false); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(sink.String(), "\n") {
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimPrefix(line, "data:")
		payload = strings.TrimSpace(payload)
		if payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("non-JSON chunk line: %s — %v", line, err)
		}
		if chunk["object"] != "chat.completion.chunk" {
			t.Fatalf("unexpected object %v in line: %s", chunk["object"], line)
		}
	}
}
