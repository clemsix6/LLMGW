package toolprefix

import (
	"fmt"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// toolNamePrefix is prepended to every tool name a flagged project declares,
// putting the project's tools in a namespace the upstream model does not
// associate with Claude Code's own tools. It is applied unconditionally: a
// name that already carries it becomes doubly prefixed, because the client's
// namespace is its own to manage.
const toolNamePrefix = "new_"

// PrefixRequest rewrites every tool name in an Anthropic message payload,
// prepending toolNamePrefix at three locations: the declarations in
// tools[].name, the forced choice in tool_choice.name, and the conversation
// history replayed in messages[].content[].name for tool_use blocks.
// tool_result blocks reference their call through tool_use_id and carry no
// name, so they are left untouched, and nothing outside these three
// locations — system prompts, message text, tool arguments — is inspected.
//
// A payload that is not valid JSON is returned unchanged. A missing field at
// any of the three locations is skipped rather than treated as an error.
func PrefixRequest(payload []byte) []byte {
	if !gjson.ValidBytes(payload) {
		return payload
	}

	payload = prefixToolDeclarations(payload)
	payload = prefixToolChoice(payload)
	payload = prefixHistoryToolUse(payload)
	return payload
}

// prefixToolDeclarations prefixes every tools[].name entry. Indices are
// enumerated once, but each name is read fresh from the current payload
// before its own write, since sjson cannot address a wildcard path and a
// stale gjson.Result would not reflect prior iterations' rewrites.
func prefixToolDeclarations(payload []byte) []byte {
	count := len(gjson.GetBytes(payload, "tools").Array())
	for i := 0; i < count; i++ {
		path := fmt.Sprintf("tools.%d.name", i)
		name := gjson.GetBytes(payload, path)
		if !name.Exists() {
			continue
		}
		payload = setString(payload, path, toolNamePrefix+name.String())
	}
	return payload
}

// prefixToolChoice prefixes tool_choice.name, but only when tool_choice.type
// is "tool" — the only shape in which the field names a specific tool.
func prefixToolChoice(payload []byte) []byte {
	if gjson.GetBytes(payload, "tool_choice.type").String() != "tool" {
		return payload
	}
	name := gjson.GetBytes(payload, "tool_choice.name")
	if !name.Exists() {
		return payload
	}
	return setString(payload, "tool_choice.name", toolNamePrefix+name.String())
}

// prefixHistoryToolUse prefixes the name of every tool_use block replayed in
// the conversation history, across every message.
func prefixHistoryToolUse(payload []byte) []byte {
	messageCount := len(gjson.GetBytes(payload, "messages").Array())
	for m := 0; m < messageCount; m++ {
		payload = prefixMessageToolUse(payload, m)
	}
	return payload
}

// prefixMessageToolUse prefixes every tool_use block in one message's
// content array, re-reading each block's type and name from the current
// payload before writing to it.
func prefixMessageToolUse(payload []byte, messageIndex int) []byte {
	contentPath := fmt.Sprintf("messages.%d.content", messageIndex)
	contentCount := len(gjson.GetBytes(payload, contentPath).Array())
	for c := 0; c < contentCount; c++ {
		blockPath := fmt.Sprintf("%s.%d", contentPath, c)
		if gjson.GetBytes(payload, blockPath+".type").String() != "tool_use" {
			continue
		}
		name := gjson.GetBytes(payload, blockPath+".name")
		if !name.Exists() {
			continue
		}
		payload = setString(payload, blockPath+".name", toolNamePrefix+name.String())
	}
	return payload
}

// setString sets a string value at path, returning the original payload
// unchanged if the write fails — a rewrite never fails a request that would
// otherwise succeed.
func setString(payload []byte, path string, value string) []byte {
	updated, err := sjson.SetBytes(payload, path, value)
	if err != nil {
		return payload
	}
	return updated
}
