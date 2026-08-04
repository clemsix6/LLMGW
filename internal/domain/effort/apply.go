package effort

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// effortPath is the single field the injection writes, and the only one it
	// reads to decide whether the client already named an effort of its own.
	effortPath = "output_config.effort"
	// thinkingTypePath carries the client's thinking switch. Anthropic accepts
	// disabled thinking only at effort "high" or below, so injecting a higher
	// level beside it would return a 400 the client would never have received.
	thinkingTypePath = "thinking.type"
	// thinkingDisabled is the thinking.type value that suppresses the injection.
	thinkingDisabled = "disabled"
)

// Apply sets output_config.effort to level in an Anthropic message payload,
// and touches nothing else.
//
// The payload is returned unchanged when the level is empty, when the payload
// is not valid JSON, when output_config.effort is already present — whatever
// its value, since a client that named an effort has expressed one — or when
// the client disabled thinking. A failed write also yields the original: an
// injection never fails a request that would otherwise succeed.
func Apply(payload []byte, level string) []byte {
	if level == "" || !gjson.ValidBytes(payload) {
		return payload
	}
	if gjson.GetBytes(payload, effortPath).Exists() {
		return payload
	}
	if gjson.GetBytes(payload, thinkingTypePath).String() == thinkingDisabled {
		return payload
	}

	updated, err := sjson.SetBytes(payload, effortPath, level)
	if err != nil {
		return payload
	}
	return updated
}
