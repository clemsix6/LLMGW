package thinkingdisplay

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestClaimOmitsDisplayForAdaptiveAndEnabled proves an adaptive or enabled
// thinking block without a display gains display "omitted" and nothing else
// changes.
func TestClaimOmitsDisplayForAdaptiveAndEnabled(t *testing.T) {
	for _, mode := range []string{"adaptive", "enabled"} {
		t.Run(mode, func(t *testing.T) {
			payload := []byte(`{"model":"m","thinking":{"type":"` + mode + `","budget_tokens":2048}}`)
			got := Claim(payload)

			if display := gjson.GetBytes(got, "thinking.display").String(); display != "omitted" {
				t.Fatalf("thinking.display = %q, want omitted", display)
			}
			if budget := gjson.GetBytes(got, "thinking.budget_tokens").Int(); budget != 2048 {
				t.Fatalf("thinking.budget_tokens = %d, want 2048", budget)
			}
		})
	}
}

// TestClaimLeavesOtherPayloadsAlone proves every payload the claim must not
// touch is returned byte for byte.
func TestClaimLeavesOtherPayloadsAlone(t *testing.T) {
	for name, payload := range map[string]string{
		"caller display": `{"thinking":{"type":"adaptive","display":"updates"}}`,
		"empty display":  `{"thinking":{"type":"enabled","display":""}}`,
		"between tools":  `{"thinking":{"type":"between_tools"}}`,
		"disabled":       `{"thinking":{"type":"disabled"}}`,
		"no thinking":    `{"model":"m"}`,
		"no type":        `{"thinking":{}}`,
		"not json":       `not json at all`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := Claim([]byte(payload)); string(got) != payload {
				t.Fatalf("payload changed: got %s, want %s", got, payload)
			}
		})
	}
}
