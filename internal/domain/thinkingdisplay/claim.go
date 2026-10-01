package thinkingdisplay

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// thinkingTypePath carries the thinking mode the claim decides on.
	thinkingTypePath = "thinking.type"
	// thinkingDisplayPath is the single field the claim writes, and the only
	// one it reads to decide whether the caller already owns the display.
	thinkingDisplayPath = "thinking.display"
	// displayOmitted asks Anthropic to return thinking blocks with their
	// content redacted, which is what callers of the gateway have always
	// received.
	displayOmitted = "omitted"
)

// Claim marks the thinking display as the caller's own in an Anthropic message
// payload, asking for omitted thinking content, and touches nothing else.
//
// Left absent, the embedded SDK fills the field itself on cloaked requests for
// the models that support it, switching the response to streamed thinking
// updates and dropping the redaction the gateway's callers rely on. The SDK
// leaves a payload that already carries the field alone, so claiming it keeps
// the redacted behaviour.
//
// Only an adaptive or enabled thinking mode is claimed. Any other mode —
// disabled, absent, or the between-tools mode that Anthropic rejects with any
// sibling field — is returned unchanged, as is a payload that is not valid
// JSON, one that already carries a display whatever its value, or one whose
// write fails: a claim never fails a request that would otherwise succeed.
func Claim(payload []byte) []byte {
	if !gjson.ValidBytes(payload) {
		return payload
	}
	if !claimsThinking(gjson.GetBytes(payload, thinkingTypePath).String()) {
		return payload
	}
	if gjson.GetBytes(payload, thinkingDisplayPath).Exists() {
		return payload
	}

	updated, err := sjson.SetBytes(payload, thinkingDisplayPath, displayOmitted)
	if err != nil {
		return payload
	}
	return updated
}

// claimsThinking reports whether a thinking mode accepts a display field.
func claimsThinking(mode string) bool {
	return mode == "adaptive" || mode == "enabled"
}
