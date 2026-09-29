package integration

import (
	"testing"

	"github.com/tidwall/gjson"
)

// thinkingBody builds one generation whose thinking object is the given JSON,
// or carries no thinking at all when it is empty.
func thinkingBody(thinking string) string {
	field := ""
	if thinking != "" {
		field = `"thinking": ` + thinking + `,`
	}
	return `{
	"model": "anthropic-test-model",
	"max_tokens": 2048,
	` + field + `
	"messages": [{"role": "user", "content": "fixture-prompt"}]
}`
}

// TestGenerationClaimsOmittedThinkingDisplay proves an adaptive or enabled
// thinking block without a display reaches the upstream asking for omitted
// thinking. Without the claim the embedded SDK sets the display to updates on
// cloaked requests, which changes the thinking output callers receive.
//
// The stub upstream captures bodies only, not request headers, so the
// consequence on the redact-thinking beta header is not observable here; the
// body claim is the whole mechanism that suppresses the SDK's own choice.
func TestGenerationClaimsOmittedThinkingDisplay(t *testing.T) {
	for _, thinking := range []string{
		`{"type": "adaptive"}`,
		`{"type": "enabled", "budget_tokens": 1024}`,
	} {
		created := testHarness.createKey(t, "thinking-display-claimed")
		effortGeneration(t, created, thinkingBody(thinking))

		display := gjson.GetBytes(lastUpstreamBody(t), "thinking.display")
		if display.String() != "omitted" {
			t.Fatalf("thinking %s: upstream thinking.display = %q, want omitted; body=%s",
				thinking, display.String(), lastUpstreamBody(t))
		}
	}
}

// TestGenerationKeepsCallerThinkingDisplay proves a caller that named a display
// keeps it.
func TestGenerationKeepsCallerThinkingDisplay(t *testing.T) {
	created := testHarness.createKey(t, "thinking-display-caller")
	effortGeneration(t, created, thinkingBody(`{"type": "adaptive", "display": "updates"}`))

	display := gjson.GetBytes(lastUpstreamBody(t), "thinking.display")
	if display.String() != "updates" {
		t.Fatalf("upstream thinking.display = %q, want updates", display.String())
	}
}

// TestGenerationLeavesOtherThinkingModesAlone proves the claim never writes a
// display for a thinking mode that does not take one: between_tools rejects any
// sibling field with a 400, and a disabled or absent thinking has no display to
// own.
func TestGenerationLeavesOtherThinkingModesAlone(t *testing.T) {
	for name, thinking := range map[string]string{
		"between_tools": `{"type": "between_tools"}`,
		"disabled":      `{"type": "disabled"}`,
		"absent":        ``,
	} {
		t.Run(name, func(t *testing.T) {
			created := testHarness.createKey(t, "thinking-display-"+name)
			effortGeneration(t, created, thinkingBody(thinking))

			if display := gjson.GetBytes(lastUpstreamBody(t), "thinking.display"); display.Exists() {
				t.Fatalf("upstream thinking.display = %s, want none", display.Raw)
			}
		})
	}
}
