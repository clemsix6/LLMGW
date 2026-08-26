package integration

import (
	"testing"

	"github.com/tidwall/gjson"
)

// contextEditCallerPolicyBody carries a context-editing policy of the caller's
// own, which the gateway must forward untouched.
const contextEditCallerPolicyBody = `{
	"model": "anthropic-test-model",
	"max_tokens": 16,
	"context_management": {"edits": [{"type": "clear_tool_uses_20250919"}]},
	"messages": [{"role": "user", "content": "fixture-prompt"}]
}`

// TestGenerationClaimsContextEditing proves every generation reaches the
// upstream owning its context editing, with an edit list that asks for nothing.
// Without the claim the embedded SDK fills the field with a thinking-clearing
// strategy, which rewrites the conversation prefix on each turn and makes the
// prompt cache unreachable: the whole context is then re-paid at the
// cache-write rate on every single request.
func TestGenerationClaimsContextEditing(t *testing.T) {
	created := testHarness.createKey(t, "context-edit-claimed")
	effortGeneration(t, created, effortPlainBody)

	edits := gjson.GetBytes(lastUpstreamBody(t), "context_management.edits")
	if !edits.IsArray() || len(edits.Array()) != 0 {
		t.Fatalf("upstream context_management.edits = %s, want an empty array", edits.Raw)
	}
}

// TestGenerationKeepsCallerContextEditing proves a caller that expressed its
// own edit policy keeps it. Without this, claiming the field would silently
// disable context editing for a caller that deliberately asked for it.
func TestGenerationKeepsCallerContextEditing(t *testing.T) {
	created := testHarness.createKey(t, "context-edit-caller")
	effortGeneration(t, created, contextEditCallerPolicyBody)

	editType := gjson.GetBytes(lastUpstreamBody(t), "context_management.edits.0.type")
	if editType.String() != "clear_tool_uses_20250919" {
		t.Fatalf("upstream first edit type = %q, want clear_tool_uses_20250919", editType.String())
	}
}
