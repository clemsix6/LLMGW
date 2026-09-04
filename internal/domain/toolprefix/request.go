package toolprefix

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// toolNamePrefix is prepended to every tool name a client declares, putting
// it in the MCP shape the embedded SDK forwards verbatim.
//
// The SDK renames any tool name it does not recognise as MCP-shaped, and its
// replacement is derived from a per-request value, so the tool declarations —
// which open the prompt the upstream cache is keyed on — differ on every
// request and the cache never hits. Handing the SDK names it already accepts
// is what keeps that prefix stable.
const toolNamePrefix = "mcp__llmgw__"

// mcpNamePrefix marks a name the embedded SDK already forwards verbatim. A
// client's own MCP tools arrive under names of that shape, such as
// mcp__notion__search, and carry their namespace already.
const mcpNamePrefix = "mcp__"

// maxToolNameLength is the longest name the embedded SDK accepts as
// MCP-shaped. Prefixing past it would produce a name the SDK renames anyway,
// so such a name is left as the client sent it.
const maxToolNameLength = 64

// customToolType is the only value the "type" field of a tool declaration can
// carry while still describing a tool the client itself defines. The
// Anthropic Messages API accepts it as an explicit spelling of the type-less
// shape, so it is renamed exactly like one.
const customToolType = "custom"

// PrefixRequest rewrites the tool names an Anthropic message payload carries,
// prepending toolNamePrefix at three locations: the declarations in
// tools[].name, the forced choice in tool_choice.name, and the conversation
// history replayed in messages[].content[].name for tool_use blocks.
// tool_result blocks reference their call through tool_use_id and carry no
// name, so they are left untouched, and nothing outside these three
// locations — system prompts, message text, tool arguments — is inspected.
//
// Three kinds of name are exempt, each honoured identically at all three
// locations so a request never declares one name while its history uses
// another:
//
//   - Tools Anthropic itself defines, recognised by the declaration's "type"
//     field and never by its name, because a client may legitimately declare
//     its own tool called `bash`. Upstream knows them under one exact name,
//     which a prefix would break.
//   - Names already carrying mcpNamePrefix, which the embedded SDK forwards
//     verbatim as they are.
//   - Names the prefix would push past maxToolNameLength, since the SDK would
//     then rename them anyway.
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
// A name declared by both an Anthropic-defined tool and a client tool lands in
// the set, so an ambiguous reference elsewhere in the payload resolves to the
// exempt reading. Prefixing a name Anthropic owns is the failure that has no
// recovery; leaving a client name alone merely forgoes the stable shape.
func exemptToolNames(payload []byte) map[string]bool {
	names := make(map[string]bool)
	gjson.GetBytes(payload, "tools").ForEach(func(_, tool gjson.Result) bool {
		if isClientTool(tool) {
			return true
		}
		if name := tool.Get("name"); name.Exists() {
			names[name.String()] = true
		}
		return true
	})
	return names
}

// isClientTool reports whether one tool declaration describes a tool the
// client defines itself, which is the only kind the prefix applies to.
func isClientTool(tool gjson.Result) bool {
	toolType := tool.Get("type")
	return !toolType.Exists() || toolType.String() == customToolType
}

// prefixToolDeclarations prefixes the name of every client tool in tools[],
// deciding per declaration from its own type rather than from the exempt names,
// so two declarations sharing a name are still told apart. Indices are
// enumerated once, but each declaration is read fresh from the current payload
// before its own write, since sjson cannot address a wildcard path and a stale
// gjson.Result would not reflect prior iterations' rewrites.
func prefixToolDeclarations(payload []byte) []byte {
	count := len(gjson.GetBytes(payload, "tools").Array())
	for i := 0; i < count; i++ {
		toolPath := fmt.Sprintf("tools.%d", i)
		if !isClientTool(gjson.GetBytes(payload, toolPath)) {
			continue
		}
		namePath := toolPath + ".name"
		name := gjson.GetBytes(payload, namePath)
		if !name.Exists() || !prefixable(name.String()) {
			continue
		}
		payload = setString(payload, namePath, toolNamePrefix+name.String())
	}
	return payload
}

// prefixToolChoice prefixes tool_choice.name, but only when tool_choice.type
// is "tool" — the only shape in which the field names a specific tool — and
// only when the name is neither one an Anthropic-defined declaration owns nor
// one PrefixRequest exempts.
func prefixToolChoice(payload []byte, exempt map[string]bool) []byte {
	if gjson.GetBytes(payload, "tool_choice.type").String() != "tool" {
		return payload
	}
	name := gjson.GetBytes(payload, "tool_choice.name")
	if !name.Exists() || exempt[name.String()] || !prefixable(name.String()) {
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

// prefixMessageToolUse prefixes every tool_use block in one message's content
// array whose name no Anthropic-defined declaration owns and PrefixRequest
// does not exempt, re-reading each block's type and name from the current
// payload before writing to it.
func prefixMessageToolUse(payload []byte, messageIndex int, exempt map[string]bool) []byte {
	contentPath := fmt.Sprintf("messages.%d.content", messageIndex)
	contentCount := len(gjson.GetBytes(payload, contentPath).Array())
	for c := 0; c < contentCount; c++ {
		blockPath := fmt.Sprintf("%s.%d", contentPath, c)
		if gjson.GetBytes(payload, blockPath+".type").String() != "tool_use" {
			continue
		}
		name := gjson.GetBytes(payload, blockPath+".name")
		if !name.Exists() || exempt[name.String()] || !prefixable(name.String()) {
			continue
		}
		payload = setString(payload, blockPath+".name", toolNamePrefix+name.String())
	}
	return payload
}

// prefixable reports whether one tool name is one toolNamePrefix applies to,
// which is the same question at all three rewritten locations.
func prefixable(name string) bool {
	if strings.HasPrefix(name, mcpNamePrefix) {
		return false
	}
	return len(toolNamePrefix)+len(name) <= maxToolNameLength
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
