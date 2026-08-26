package contextedit

import (
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	// contextManagementPath is the single field the claim writes, and the only
	// one it reads to decide whether the caller already owns context editing.
	contextManagementPath = "context_management"
	// noEdits claims the field while asking for no edit at all. Anthropic
	// accepts an empty edit list and applies nothing.
	noEdits = `{"edits":[]}`
)

// Claim marks context editing as the caller's own in an Anthropic message
// payload, and touches nothing else.
//
// Left absent, the field is filled in by the embedded SDK with a
// thinking-clearing strategy of its own, so the request matches the shape a
// native Claude Code client sends. That strategy rewrites the conversation
// prefix on every turn, which makes the prompt cache unusable: each request
// re-pays the whole context, at the cache-write rate rather than the cache-read
// one. The SDK leaves a payload that already carries the field alone, so
// claiming it is what keeps the cache reachable.
//
// The payload is returned unchanged when it is not valid JSON, when the caller
// already sent a context_management of its own — whatever it asks for, since a
// caller that expressed an edit policy owns it — or when the write fails: a
// claim never fails a request that would otherwise succeed.
func Claim(payload []byte) []byte {
	if !gjson.ValidBytes(payload) {
		return payload
	}
	if gjson.GetBytes(payload, contextManagementPath).Exists() {
		return payload
	}

	updated, err := sjson.SetRawBytes(payload, contextManagementPath, []byte(noEdits))
	if err != nil {
		return payload
	}
	return updated
}
