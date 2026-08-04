package toolprefix

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
)

// StripResponse reverses PrefixRequest on a complete Anthropic response,
// removing toolNamePrefix from the name of every tool_use block in
// content[]. A payload that is not valid JSON is returned unchanged.
func StripResponse(payload []byte) []byte {
	if !gjson.ValidBytes(payload) {
		return payload
	}

	count := len(gjson.GetBytes(payload, "content").Array())
	for i := 0; i < count; i++ {
		payload = stripBlockName(payload, fmt.Sprintf("content.%d", i))
	}
	return payload
}

// StripStreamEvent reverses PrefixRequest for one SSE event, removing
// toolNamePrefix from a tool_use block's name. It receives and returns the
// raw event bytes exactly as written by the SDK — any "event:" line, the
// "data:" prefix, and the trailing delimiter are preserved untouched; only
// the JSON payload after "data:" is parsed and, if it qualifies, rewritten.
//
// Only events whose top-level type is "content_block_start" and whose
// content_block.type is "tool_use" are touched. A frame with no "data:"
// line — such as the SDK's ": keep-alive\n\n" comment — is returned
// unchanged, and so is one whose data is not valid JSON.
func StripStreamEvent(event []byte) []byte {
	jsonStart, jsonEnd, ok := dataLineBounds(event)
	if !ok {
		return event
	}

	payload := event[jsonStart:jsonEnd]
	if !gjson.ValidBytes(payload) {
		return event
	}
	if gjson.GetBytes(payload, "type").String() != "content_block_start" {
		return event
	}

	updated := stripBlockName(payload, "content_block")
	rebuilt := make([]byte, 0, len(event)-len(payload)+len(updated))
	rebuilt = append(rebuilt, event[:jsonStart]...)
	rebuilt = append(rebuilt, updated...)
	rebuilt = append(rebuilt, event[jsonEnd:]...)
	return rebuilt
}

// stripBlockName strips toolNamePrefix from the "name" field of the JSON
// object found at blockPath, but only when that object's "type" is
// "tool_use" and its name actually carries the prefix. A name without the
// prefix — a model naming a tool that was never declared, for instance — is
// returned unchanged, never truncated or rejected.
func stripBlockName(payload []byte, blockPath string) []byte {
	if gjson.GetBytes(payload, blockPath+".type").String() != "tool_use" {
		return payload
	}
	name := gjson.GetBytes(payload, blockPath+".name")
	if !name.Exists() || !strings.HasPrefix(name.String(), toolNamePrefix) {
		return payload
	}
	stripped := strings.TrimPrefix(name.String(), toolNamePrefix)
	return setString(payload, blockPath+".name", stripped)
}

// dataLineBounds locates the "data:" line within one SSE event's raw bytes
// and returns the byte offsets of its value, trimmed of the "data:" prefix
// and any leading spaces. ok is false when the event carries no such line.
func dataLineBounds(event []byte) (jsonStart int, jsonEnd int, ok bool) {
	const marker = "data:"

	lineStart := 0
	for lineStart <= len(event) {
		lineEnd := len(event)
		if newline := bytes.IndexByte(event[lineStart:], '\n'); newline >= 0 {
			lineEnd = lineStart + newline
		}

		line := event[lineStart:lineEnd]
		if bytes.HasPrefix(line, []byte(marker)) {
			start := lineStart + len(marker)
			for start < lineEnd && event[start] == ' ' {
				start++
			}
			return start, lineEnd, true
		}
		if lineEnd == len(event) {
			break
		}
		lineStart = lineEnd + 1
	}
	return 0, 0, false
}
