package contextedit

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestClaimWritesAbsentContextManagement proves a payload carrying no
// context_management gains the field with an empty edit list, which is what
// stops the embedded SDK from filling it with a strategy that rewrites the
// conversation prefix on every turn and voids the prompt cache.
func TestClaimWritesAbsentContextManagement(t *testing.T) {
	payload := []byte(`{"model":"claude-opus-5","messages":[]}`)
	got := Claim(payload)

	edits := gjson.GetBytes(got, "context_management.edits")
	if !edits.IsArray() || len(edits.Array()) != 0 {
		t.Fatalf("context_management.edits = %s, want an empty array", edits.Raw)
	}
	if model := gjson.GetBytes(got, "model").String(); model != "claude-opus-5" {
		t.Fatalf("model = %q, want claude-opus-5", model)
	}
}

// TestClaimLeavesCallerPolicyAlone proves presence is what counts: a
// context_management the caller sent wins whatever it asks for, so a caller
// that genuinely wants context editing keeps it.
func TestClaimLeavesCallerPolicyAlone(t *testing.T) {
	for _, callerPolicy := range []string{
		`{"edits":[{"type":"clear_tool_uses_20250919"}]}`,
		`{"edits":[]}`,
		`{}`,
		`null`,
	} {
		t.Run(callerPolicy, func(t *testing.T) {
			payload := []byte(`{"context_management":` + callerPolicy + `}`)
			got := Claim(payload)
			if string(got) != string(payload) {
				t.Fatalf("payload changed: got %s, want %s", got, payload)
			}
		})
	}
}

// TestClaimLeavesInvalidPayloadAlone proves a body the gateway cannot parse is
// forwarded byte for byte, so a claim never turns a request the upstream would
// have answered into one it rejects.
func TestClaimLeavesInvalidPayloadAlone(t *testing.T) {
	payload := []byte(`not json at all`)
	if got := Claim(payload); string(got) != string(payload) {
		t.Fatalf("payload changed: got %s, want %s", got, payload)
	}
}
