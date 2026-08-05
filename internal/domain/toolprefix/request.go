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

// customToolType is the only value the "type" field of a tool declaration can
// carry while still describing a tool the project itself defines. The
// Anthropic Messages API accepts it as an explicit spelling of the type-less
// shape, so it is renamed exactly like one.
const customToolType = "custom"

// PrefixRequest rewrites the tool names a flagged project declares in an
// Anthropic message payload, prepending toolNamePrefix at three locations: the
// declarations in tools[].name, the forced choice in tool_choice.name, and the
// conversation history replayed in messages[].content[].name for tool_use
// blocks. tool_result blocks reference their call through tool_use_id and
// carry no name, so they are left untouched, and nothing outside these three
// locations — system prompts, message text, tool arguments — is inspected.
//
// Tools Anthropic itself defines are exempt: they are recognised by upstream
// under one exact name, which a prefix would break. The discriminator is the
// declaration's "type" field, never its name, because a project may legitimately
// declare its own tool called `bash`. A declaration with no "type", or whose
// type is customToolType, is the project's own and is renamed; every other type
// is Anthropic's and is left alone.
//
// The exemption is resolved from tools[] first and then honoured at all three
// locations, so a request never ends up declaring `bash` while its history
// names `new_bash`.
//
// A payload that is not valid JSON is returned unchanged. A missing field at
// any of the three locations is skipped rather than treated as an error.
func PrefixRequest(payload []byte) []byte {
	if !gjson.ValidBytes(payload) {
		return payload
	}

	exempt := exemptToolNames(payload)
	payload = prefixToolDeclarations(payload)
	payload = prefixToolChoice(payload, exempt)
	payload = prefixHistoryToolUse(payload, exempt)
	return payload
}

// exemptToolNames collects the name of every Anthropic-defined tool the
// payload declares, read from the payload as the client sent it — before any
// rewrite — since tool_choice and the replayed history name tools the way the
// client does.
//
// A name declared by both an Anthropic-defined tool and a project tool lands in
// the set, so an ambiguous reference elsewhere in the payload resolves to the
// exempt reading. Prefixing a name Anthropic owns is the failure that has no
// recovery; leaving a project name alone merely forgoes the namespace.
func exemptToolNames(payload []byte) map[string]bool {
	names := make(map[string]bool)
	gjson.GetBytes(payload, "tools").ForEach(func(_, tool gjson.Result) bool {
		if isProjectTool(tool) {
			return true
		}
		if name := tool.Get("name"); name.Exists() {
			names[name.String()] = true
		}
		return true
	})
	return names
}

// isProjectTool reports whether one tool declaration describes a tool the
// project defines itself, which is the only kind the prefix applies to.
func isProjectTool(tool gjson.Result) bool {
	toolType := tool.Get("type")
	return !toolType.Exists() || toolType.String() == customToolType
}

// prefixToolDeclarations prefixes the name of every project tool in tools[],
// deciding per declaration from its own type rather than from the exempt names,
// so two declarations sharing a name are still told apart. Indices are
// enumerated once, but each declaration is read fresh from the current payload
// before its own write, since sjson cannot address a wildcard path and a stale
// gjson.Result would not reflect prior iterations' rewrites.
func prefixToolDeclarations(payload []byte) []byte {
	count := len(gjson.GetBytes(payload, "tools").Array())
	for i := 0; i < count; i++ {
		toolPath := fmt.Sprintf("tools.%d", i)
		if !isProjectTool(gjson.GetBytes(payload, toolPath)) {
			continue
		}
		namePath := toolPath + ".name"
		name := gjson.GetBytes(payload, namePath)
		if !name.Exists() {
			continue
		}
		payload = setString(payload, namePath, toolNamePrefix+name.String())
	}
	return payload
}

// prefixToolChoice prefixes tool_choice.name, but only when tool_choice.type
// is "tool" — the only shape in which the field names a specific tool — and
// only when the name is not one an Anthropic-defined declaration owns.
func prefixToolChoice(payload []byte, exempt map[string]bool) []byte {
	if gjson.GetBytes(payload, "tool_choice.type").String() != "tool" {
		return payload
	}
	name := gjson.GetBytes(payload, "tool_choice.name")
	if !name.Exists() || exempt[name.String()] {
		return payload
	}
	return setString(payload, "tool_choice.name", toolNamePrefix+name.String())
}

// prefixHistoryToolUse prefixes the name of every tool_use block replayed in
// the conversation history, across every message.
func prefixHistoryToolUse(payload []byte, exempt map[string]bool) []byte {
	messageCount := len(gjson.GetBytes(payload, "messages").Array())
	for m := 0; m < messageCount; m++ {
		payload = prefixMessageToolUse(payload, m, exempt)
	}
	return payload
}

// prefixMessageToolUse prefixes every tool_use block in one message's
// content array whose name no Anthropic-defined declaration owns, re-reading
// each block's type and name from the current payload before writing to it.
func prefixMessageToolUse(payload []byte, messageIndex int, exempt map[string]bool) []byte {
	contentPath := fmt.Sprintf("messages.%d.content", messageIndex)
	contentCount := len(gjson.GetBytes(payload, contentPath).Array())
	for c := 0; c < contentCount; c++ {
		blockPath := fmt.Sprintf("%s.%d", contentPath, c)
		if gjson.GetBytes(payload, blockPath+".type").String() != "tool_use" {
			continue
		}
		name := gjson.GetBytes(payload, blockPath+".name")
		if !name.Exists() || exempt[name.String()] {
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
