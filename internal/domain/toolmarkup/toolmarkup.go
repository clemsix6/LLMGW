package toolmarkup

import (
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

// snippetLimit bounds the reported evidence so a log line stays readable while
// still identifying the leak.
const snippetLimit = 80

// leakPattern matches the text-format tool-call grammar that must never appear
// inside a tool_use input value: the antml namespace, or an invoke/parameter
// tag. Word boundaries keep prose mentioning "invoke" or "parameter" clean.
var leakPattern = regexp.MustCompile(`(?i)antml|<\s*/?\s*(?:invoke|parameter)\b`)

// DetectResponse reports the first leaked tool-call markup found in a
// tool_use input string value of one non-streamed Anthropic response. The
// returned snippet starts at the leak and is bounded, safe for logging. A
// payload that is not valid JSON reports no leak: refusing it is the
// client's protocol handling to do, not this guard's.
func DetectResponse(payload []byte) (string, bool) {
	if !gjson.ValidBytes(payload) {
		return "", false
	}

	snippet := ""
	gjson.GetBytes(payload, "content").ForEach(func(_, block gjson.Result) bool {
		if block.Get("type").String() != "tool_use" {
			return true
		}
		leak, found := detectValue(block.Get("input"))
		if found {
			snippet = leak
			return false
		}
		return true
	})
	return snippet, snippet != ""
}

// detectValue walks one JSON value depth-first and reports the first string
// carrying the leak pattern.
func detectValue(value gjson.Result) (string, bool) {
	if value.Type == gjson.String {
		return detectString(value.String())
	}
	if !value.IsObject() && !value.IsArray() {
		return "", false
	}

	snippet := ""
	value.ForEach(func(_, child gjson.Result) bool {
		leak, found := detectValue(child)
		if found {
			snippet = leak
			return false
		}
		return true
	})
	return snippet, snippet != ""
}

// detectString reports a bounded single-line snippet starting at the first
// leak match inside one string value.
func detectString(text string) (string, bool) {
	location := leakPattern.FindStringIndex(text)
	if location == nil {
		return "", false
	}

	snippet := text[location[0]:]
	if len(snippet) > snippetLimit {
		snippet = snippet[:snippetLimit]
	}
	return strings.ReplaceAll(snippet, "\n", " "), true
}
