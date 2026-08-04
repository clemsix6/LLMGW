package effort

import (
	"testing"

	"github.com/tidwall/gjson"
)

// TestApplyWritesAbsentOutputConfig proves a payload carrying no
// output_config at all gains the field with the project's level.
func TestApplyWritesAbsentOutputConfig(t *testing.T) {
	payload := []byte(`{"model":"claude-opus-5","messages":[]}`)
	got := Apply(payload, "low")

	if level := gjson.GetBytes(got, "output_config.effort").String(); level != "low" {
		t.Fatalf("output_config.effort = %q, want low", level)
	}
	if model := gjson.GetBytes(got, "model").String(); model != "claude-opus-5" {
		t.Fatalf("model = %q, want claude-opus-5", model)
	}
}

// TestApplyKeepsOutputConfigSiblings proves an output_config present without
// an effort gains the field beside whatever else it already carries.
func TestApplyKeepsOutputConfigSiblings(t *testing.T) {
	payload := []byte(`{"output_config":{"verbosity":"medium"}}`)
	got := Apply(payload, "max")

	if level := gjson.GetBytes(got, "output_config.effort").String(); level != "max" {
		t.Fatalf("output_config.effort = %q, want max", level)
	}
	if verbosity := gjson.GetBytes(got, "output_config.verbosity").String(); verbosity != "medium" {
		t.Fatalf("output_config.verbosity = %q, want medium", verbosity)
	}
}

// TestApplyLeavesClientEffortAlone proves presence is what counts: an effort
// the client already named wins over the project default, whatever its value,
// so the default never becomes an override on the payloads hardest to reason
// about.
func TestApplyLeavesClientEffortAlone(t *testing.T) {
	for _, clientEffort := range []string{`"low"`, `""`, `"unrecognised"`, `null`} {
		t.Run(clientEffort, func(t *testing.T) {
			payload := []byte(`{"output_config":{"effort":` + clientEffort + `}}`)
			got := Apply(payload, "max")
			if string(got) != string(payload) {
				t.Fatalf("payload changed: got %s, want %s", got, payload)
			}
		})
	}
}

// TestApplySkipsDisabledThinking proves a request that disabled thinking is
// left alone, since a level above "high" beside it is refused upstream.
func TestApplySkipsDisabledThinking(t *testing.T) {
	payload := []byte(`{"thinking":{"type":"disabled"},"messages":[]}`)
	got := Apply(payload, "max")
	if string(got) != string(payload) {
		t.Fatalf("payload changed: got %s, want %s", got, payload)
	}
}

// TestApplyWritesBesideEnabledThinking proves only the disabled case is
// suppressed: a client thinking block that is on keeps the injection.
func TestApplyWritesBesideEnabledThinking(t *testing.T) {
	payload := []byte(`{"thinking":{"type":"enabled","budget_tokens":1024}}`)
	got := Apply(payload, "high")

	if level := gjson.GetBytes(got, "output_config.effort").String(); level != "high" {
		t.Fatalf("output_config.effort = %q, want high", level)
	}
	if budget := gjson.GetBytes(got, "thinking.budget_tokens").Int(); budget != 1024 {
		t.Fatalf("thinking.budget_tokens = %d, want 1024", budget)
	}
}

// TestApplyEmptyLevelUnchanged proves the sentinel for "no default" injects
// nothing, which is the path every unset project takes.
func TestApplyEmptyLevelUnchanged(t *testing.T) {
	payload := []byte(`{"messages":[]}`)
	got := Apply(payload, "")
	if string(got) != string(payload) {
		t.Fatalf("payload changed: got %s, want %s", got, payload)
	}
}

// TestApplyMalformedJSONUnchanged proves an unparseable payload is forwarded
// as it stands, so Anthropic returns its own specific validation error rather
// than a vaguer one from the gateway.
func TestApplyMalformedJSONUnchanged(t *testing.T) {
	payload := []byte(`{"messages": not valid json`)
	got := Apply(payload, "low")
	if string(got) != string(payload) {
		t.Fatalf("malformed payload changed: got %s, want %s", got, payload)
	}
}

// TestApplyEmptyPayloadUnchanged proves nil and empty payloads pass through
// untouched instead of becoming a synthesized object.
func TestApplyEmptyPayloadUnchanged(t *testing.T) {
	if got := Apply(nil, "low"); got != nil {
		t.Fatalf("nil payload changed: got %v", got)
	}
	if got := Apply([]byte(""), "low"); len(got) != 0 {
		t.Fatalf("empty payload changed: got %v", got)
	}
}
