package codex

import (
	"encoding/json"
	"testing"
)

// TestTranslateRequestMapsCoreFields is the canonical failing test from the task brief.
// It asserts that translateRequest produces store:false, max_output_tokens mapped,
// instructions set, a developer input item, and a function tool.
func TestTranslateRequestMapsCoreFields(t *testing.T) {
	in := []byte(`{"model":"gpt-5","max_tokens":256,"messages":[
		{"role":"system","content":"be terse"},{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`)
	out, err := translateRequest(in, "CODEX_MIN")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["store"] != false || got["max_output_tokens"].(float64) != 256 || got["instructions"] != "CODEX_MIN" {
		t.Fatalf("core fields wrong: %v", got)
	}
	assertHasDeveloperInput(t, got)
	assertHasFunctionTool(t, got)
}

// TestTranslateRequestAssistantToolCall verifies that assistant tool_calls are mapped to
// function_call input items and that a following tool-role message becomes
// a function_call_output item.
func TestTranslateRequestAssistantToolCall(t *testing.T) {
	in := []byte(`{"model":"gpt-5","messages":[
		{"role":"user","content":"call f"},
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`)
	out, err := translateRequest(in, "inst")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	input := got["input"].([]any)
	assertInputContainsType(t, input, "function_call")
	assertInputContainsType(t, input, "function_call_output")
}

// TestTranslateRequestInvalidModel verifies that an unknown model id causes an error.
func TestTranslateRequestInvalidModel(t *testing.T) {
	in := []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`)
	_, err := translateRequest(in, "inst")
	if err == nil {
		t.Fatal("expected error for unknown model")
	}
}

// TestTranslateRequestStreamForced verifies that the output always has stream:true.
func TestTranslateRequestStreamForced(t *testing.T) {
	in := []byte(`{"model":"gpt-5","stream":false,"messages":[{"role":"user","content":"hi"}]}`)
	out, err := translateRequest(in, "inst")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.Unmarshal(out, &got)
	if got["stream"] != true {
		t.Fatalf("expected stream:true, got %v", got["stream"])
	}
}

// assertHasDeveloperInput asserts that got["input"] contains at least one
// message item with role "developer".
func assertHasDeveloperInput(t *testing.T, got map[string]any) {
	t.Helper()
	input, ok := got["input"].([]any)
	if !ok {
		t.Fatal("input is not an array")
	}
	for _, item := range input {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "message" && m["role"] == "developer" {
			return
		}
	}
	t.Fatal("no developer input item found")
}

// assertHasFunctionTool asserts that got["tools"] contains at least one tool
// with type "function".
func assertHasFunctionTool(t *testing.T, got map[string]any) {
	t.Helper()
	tools, ok := got["tools"].([]any)
	if !ok {
		t.Fatal("tools is not an array")
	}
	for _, tool := range tools {
		m, ok := tool.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == "function" {
			return
		}
	}
	t.Fatal("no function tool found")
}

// assertInputContainsType asserts that the input slice contains at least one
// item with the given type field.
func assertInputContainsType(t *testing.T, input []any, itemType string) {
	t.Helper()
	for _, item := range input {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if m["type"] == itemType {
			return
		}
	}
	t.Fatalf("no input item with type %q", itemType)
}
