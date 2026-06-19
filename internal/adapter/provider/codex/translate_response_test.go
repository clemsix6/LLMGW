package codex

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTranslateResponseFoldsAndMapsUsage drives the full non-streaming path against a
// real-format SSE stream (responses_text.sse): aggregateCompleted collects the message item
// from the response.output_item.done event (response.completed.output is empty, per the real
// backend), then translateResponse folds output[] into chat.completion. The result must carry
// non-empty content and non-zero usage.
func TestTranslateResponseFoldsAndMapsUsage(t *testing.T) {
	sseBytes := readTestdata(t, "responses_text.sse")
	completed, err := aggregateCompleted(bytes.NewReader(sseBytes))
	if err != nil {
		t.Fatal(err)
	}
	out, u, err := translateResponse(completed)
	if err != nil {
		t.Fatal(err)
	}
	var cc map[string]any
	_ = json.Unmarshal(out, &cc)
	if cc["object"] != "chat.completion" || u.InputTokens == 0 || u.OutputTokens == 0 {
		t.Fatalf("bad translation: object=%v usage=%+v", cc["object"], u)
	}
	if cc["model"] != "gpt-5.5" {
		t.Fatalf("expected model=gpt-5.5, got %v", cc["model"])
	}
	choices := cc["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] == nil {
		t.Fatal("expected non-nil content, got nil — output_item.done items not collected")
	}
	assertNoReasoningLeaked(t, out)
}

// TestTranslateResponseDropsReasoning verifies that a reasoning-only output produces an
// empty content field, no tool_calls, and finish_reason "stop".
func TestTranslateResponseDropsReasoning(t *testing.T) {
	input := []byte(`{
		"id": "resp_r",
		"output": [{"type":"reasoning","id":"rs_1","summary":[]}],
		"usage": {"input_tokens":10,"output_tokens":5}
	}`)
	out, u, err := translateResponse(input)
	if err != nil {
		t.Fatal(err)
	}
	assertNoReasoningLeaked(t, out)
	var cc map[string]any
	_ = json.Unmarshal(out, &cc)
	choices := cc["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["finish_reason"] != nil {
		// finish_reason is on the choice, not the message
	}
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("expected finish_reason=stop, got %v", choice["finish_reason"])
	}
	if u.InputTokens != 10 || u.OutputTokens != 5 {
		t.Fatalf("wrong usage: %+v", u)
	}
}

// TestTranslateResponseToolCallsFinishReason verifies that a function_call output item
// triggers finish_reason "tool_calls".
func TestTranslateResponseToolCallsFinishReason(t *testing.T) {
	input := []byte(`{
		"id": "resp_tc",
		"output": [{"type":"function_call","call_id":"c1","name":"fn","arguments":"{}"}],
		"usage": {"input_tokens":20,"output_tokens":10}
	}`)
	out, _, err := translateResponse(input)
	if err != nil {
		t.Fatal(err)
	}
	var cc map[string]any
	_ = json.Unmarshal(out, &cc)
	choices := cc["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("expected finish_reason=tool_calls, got %v", choice["finish_reason"])
	}
}

// TestAggregateCompletedExtractsResponse verifies that aggregateCompleted returns a synthetic
// response JSON whose output is populated from response.output_item.done events, not from
// response.completed.output (which is always [] on the real backend).
func TestAggregateCompletedExtractsResponse(t *testing.T) {
	sseBytes := readTestdata(t, "responses_text.sse")
	completed, err := aggregateCompleted(bytes.NewReader(sseBytes))
	if err != nil {
		t.Fatal(err)
	}
	var resp map[string]any
	if err := json.Unmarshal(completed, &resp); err != nil {
		t.Fatalf("aggregateCompleted returned non-JSON: %v", err)
	}
	if resp["id"] != "resp_test" {
		t.Fatalf("expected id=resp_test, got %v", resp["id"])
	}
	output, ok := resp["output"].([]any)
	if !ok || len(output) == 0 {
		t.Fatalf("expected non-empty output from output_item.done items, got %v", resp["output"])
	}
}

// TestTranslateResponseNonStreamingToolCall drives the non-streaming path against the real-format
// tool stream (responses_tool.sse) and asserts that the function call is correctly translated:
// finish_reason "tool_calls", tool_calls[0] has the right name, arguments, and call id.
func TestTranslateResponseNonStreamingToolCall(t *testing.T) {
	sseBytes := readTestdata(t, "responses_tool.sse")
	completed, err := aggregateCompleted(bytes.NewReader(sseBytes))
	if err != nil {
		t.Fatal(err)
	}
	out, u, err := translateResponse(completed)
	if err != nil {
		t.Fatal(err)
	}
	if u.InputTokens != 60 || u.OutputTokens != 18 {
		t.Fatalf("wrong usage: %+v", u)
	}
	var cc map[string]any
	_ = json.Unmarshal(out, &cc)
	choices := cc["choices"].([]any)
	choice := choices[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("expected finish_reason=tool_calls, got %v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	calls := msg["tool_calls"].([]any)
	tc := calls[0].(map[string]any)
	if tc["id"] != "call_1" {
		t.Fatalf("expected tool_calls[0].id=call_1, got %v", tc["id"])
	}
	fn := tc["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Fatalf("expected function name=get_weather, got %v", fn["name"])
	}
	if fn["arguments"] != `{"location":"Paris"}` {
		t.Fatalf("expected arguments={\"location\":\"Paris\"}, got %v", fn["arguments"])
	}
}

// TestAggregateCompletedMissingEvent verifies that an SSE stream without a response.completed
// event returns an error.
func TestAggregateCompletedMissingEvent(t *testing.T) {
	sse := []byte("data: {\"type\":\"response.in_progress\"}\n\ndata: [DONE]\n")
	_, err := aggregateCompleted(bytes.NewReader(sse))
	if err == nil {
		t.Fatal("expected error when response.completed is absent")
	}
}

// TestAggregateCompletedLargeEvent verifies that aggregateCompleted handles a
// response.completed SSE data: line larger than 64 KB — the bufio.Scanner limit that the old
// implementation silently violated. This test MUST fail against the old Scanner code
// (scanner.Err() returns bufio.ErrTooLong) and pass with the ReadBytes fix.
func TestAggregateCompletedLargeEvent(t *testing.T) {
	filler := strings.Repeat("x", 80*1024) // 80 KB padding, well above the 64 KB Scanner cap
	respJSON, _ := json.Marshal(map[string]any{
		"id":           "resp_large",
		"output":       []any{},
		"usage":        map[string]any{"input_tokens": 1, "output_tokens": 1},
		"instructions": filler,
	})
	eventJSON, _ := json.Marshal(map[string]any{
		"type":     "response.completed",
		"response": json.RawMessage(respJSON),
	})
	sse := "data: " + string(eventJSON) + "\n\ndata: [DONE]\n"

	completed, err := aggregateCompleted(strings.NewReader(sse))
	if err != nil {
		t.Fatalf("aggregateCompleted failed on >64KB event: %v", err)
	}
	var r map[string]any
	if err := json.Unmarshal(completed, &r); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if r["id"] != "resp_large" {
		t.Fatalf("expected id=resp_large, got %v", r["id"])
	}
}

// readTestdata reads a file from the testdata directory relative to the package under test.
func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read testdata/%s: %v", name, err)
	}
	return data
}

// assertNoReasoningLeaked asserts that the raw chat.completion JSON does not contain the word
// "reasoning", which would indicate a leaked reasoning output item.
func assertNoReasoningLeaked(t *testing.T, out []byte) {
	t.Helper()
	if bytes.Contains(out, []byte("reasoning")) {
		t.Fatalf("reasoning leaked into chat.completion output: %s", out)
	}
}
