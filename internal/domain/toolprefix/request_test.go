package toolprefix

import (
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

// TestPrefixRequestToolDeclarations proves every tools[].name entry is prefixed.
func TestPrefixRequestToolDeclarations(t *testing.T) {
	payload := []byte(`{"tools":[{"name":"search_web"},{"name":"read_file"}]}`)
	got := PrefixRequest(payload)

	want := []string{"new_search_web", "new_read_file"}
	for i, w := range want {
		if name := gjson.GetBytes(got, fmt.Sprintf("tools.%d.name", i)).String(); name != w {
			t.Fatalf("tools.%d.name = %q, want %q", i, name, w)
		}
	}
}

// TestPrefixRequestSkipsToolWithoutName proves a declaration missing its name
// is skipped rather than erroring.
func TestPrefixRequestSkipsToolWithoutName(t *testing.T) {
	payload := []byte(`{"tools":[{"description":"no name here"}]}`)
	got := PrefixRequest(payload)
	if string(got) != string(payload) {
		t.Fatalf("payload without tool name changed: got %s, want %s", got, payload)
	}
}

// TestPrefixRequestToolChoice proves tool_choice.name is prefixed when its
// type is "tool".
func TestPrefixRequestToolChoice(t *testing.T) {
	payload := []byte(`{"tool_choice":{"type":"tool","name":"search_web"}}`)
	got := PrefixRequest(payload)
	if name := gjson.GetBytes(got, "tool_choice.name").String(); name != "new_search_web" {
		t.Fatalf("tool_choice.name = %q, want new_search_web", name)
	}
}

// TestPrefixRequestToolChoiceIgnoresNonToolType proves tool_choice is left
// untouched for every type other than "tool".
func TestPrefixRequestToolChoiceIgnoresNonToolType(t *testing.T) {
	for _, choiceType := range []string{"auto", "any", "none"} {
		t.Run(choiceType, func(t *testing.T) {
			payload := []byte(fmt.Sprintf(`{"tool_choice":{"type":%q}}`, choiceType))
			got := PrefixRequest(payload)
			if string(got) != string(payload) {
				t.Fatalf("payload changed: got %s, want %s", got, payload)
			}
		})
	}
}

// TestPrefixRequestHistoryToolUse proves a tool_use block replayed in
// conversation history is prefixed.
func TestPrefixRequestHistoryToolUse(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","name":"search_web","input":{}}]}]}`)
	got := PrefixRequest(payload)
	if name := gjson.GetBytes(got, "messages.0.content.0.name").String(); name != "new_search_web" {
		t.Fatalf("messages.0.content.0.name = %q, want new_search_web", name)
	}
}

// TestPrefixRequestLeavesToolResultUntouched proves a tool_result block,
// which references its call by tool_use_id and carries no name, is untouched.
func TestPrefixRequestLeavesToolResultUntouched(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}]}`)
	got := PrefixRequest(payload)
	if string(got) != string(payload) {
		t.Fatalf("tool_result block changed: got %s, want %s", got, payload)
	}
}

// TestPrefixRequestLeavesMessageTextUntouched proves free-form message text
// that happens to contain a tool name is never inspected or rewritten.
func TestPrefixRequestLeavesMessageTextUntouched(t *testing.T) {
	payload := []byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"please call search_web"}]}]}`)
	got := PrefixRequest(payload)
	if string(got) != string(payload) {
		t.Fatalf("text block changed: got %s, want %s", got, payload)
	}
}

// TestPrefixRequestAllThreeLocationsTogether proves the three locations are
// rewritten together in a single payload, and nothing else is touched.
func TestPrefixRequestAllThreeLocationsTogether(t *testing.T) {
	payload := []byte(`{
		"tools":[{"name":"search_web"}],
		"tool_choice":{"type":"tool","name":"search_web"},
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","name":"search_web","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
		]
	}`)
	got := PrefixRequest(payload)

	if v := gjson.GetBytes(got, "tools.0.name").String(); v != "new_search_web" {
		t.Fatalf("tools.0.name = %q", v)
	}
	if v := gjson.GetBytes(got, "tool_choice.name").String(); v != "new_search_web" {
		t.Fatalf("tool_choice.name = %q", v)
	}
	if v := gjson.GetBytes(got, "messages.0.content.0.name").String(); v != "new_search_web" {
		t.Fatalf("messages.0.content.0.name = %q", v)
	}
	if v := gjson.GetBytes(got, "messages.1.content.0.type").String(); v != "tool_result" {
		t.Fatalf("tool_result block type changed: %q", v)
	}
	if gjson.GetBytes(got, "messages.1.content.0.name").Exists() {
		t.Fatalf("tool_result block gained a name field")
	}
}

// TestPrefixRequestSeveralToolsAndHistoryBlocks proves the enumerate-then-
// set-concrete-paths rule: every index across a multi-element tools array
// and a multi-message, multi-block history is rewritten, none skipped and
// none corrupted by writes made to earlier indices in the same pass.
func TestPrefixRequestSeveralToolsAndHistoryBlocks(t *testing.T) {
	payload := []byte(`{
		"tools":[{"name":"alpha"},{"name":"beta"},{"name":"gamma"}],
		"messages":[
			{"role":"assistant","content":[
				{"type":"tool_use","name":"alpha","input":{}},
				{"type":"text","text":"checking"},
				{"type":"tool_use","name":"beta","input":{}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"t1","content":"ok"},
				{"type":"tool_result","tool_use_id":"t2","content":"ok"}
			]},
			{"role":"assistant","content":[
				{"type":"tool_use","name":"gamma","input":{}}
			]}
		]
	}`)
	got := PrefixRequest(payload)

	assertToolNames(t, got, []string{"new_alpha", "new_beta", "new_gamma"})
	assertHistoryNames(t, got, map[string]string{
		"messages.0.content.0.name": "new_alpha",
		"messages.0.content.2.name": "new_beta",
		"messages.2.content.0.name": "new_gamma",
	})
	if gjson.GetBytes(got, "messages.0.content.1.name").Exists() {
		t.Fatalf("text block gained a name field")
	}
	if gjson.GetBytes(got, "messages.1.content.0.name").Exists() ||
		gjson.GetBytes(got, "messages.1.content.1.name").Exists() {
		t.Fatalf("tool_result block gained a name field")
	}
}

// assertToolNames checks tools[].name against the expected sequence.
func assertToolNames(t *testing.T, payload []byte, want []string) {
	t.Helper()
	for i, w := range want {
		path := fmt.Sprintf("tools.%d.name", i)
		if v := gjson.GetBytes(payload, path).String(); v != w {
			t.Fatalf("%s = %q, want %q", path, v, w)
		}
	}
}

// assertHistoryNames checks each given path against its expected value.
func assertHistoryNames(t *testing.T, payload []byte, want map[string]string) {
	t.Helper()
	for path, w := range want {
		if v := gjson.GetBytes(payload, path).String(); v != w {
			t.Fatalf("%s = %q, want %q", path, v, w)
		}
	}
}

// TestPrefixRequestLeavesAnthropicToolsAlone proves a tool Anthropic defines
// keeps the exact name upstream requires, while a project declaration in the
// same payload is still namespaced.
func TestPrefixRequestLeavesAnthropicToolsAlone(t *testing.T) {
	payload := []byte(`{"tools":[
		{"type":"web_search_20260209","name":"web_search"},
		{"type":"bash_20250124","name":"bash"},
		{"type":"text_editor_20250728","name":"str_replace_based_edit_tool"},
		{"name":"search_web","description":"search","input_schema":{"type":"object"}}
	]}`)
	got := PrefixRequest(payload)

	assertToolNames(t, got, []string{
		"web_search",
		"bash",
		"str_replace_based_edit_tool",
		"new_search_web",
	})
}

// TestPrefixRequestPrefixesCustomTypedTool proves the explicit "custom" type,
// which the Anthropic Messages API accepts on a project's own declaration, is
// renamed exactly like the type-less shape.
func TestPrefixRequestPrefixesCustomTypedTool(t *testing.T) {
	payload := []byte(`{"tools":[
		{"type":"custom","name":"search_web","input_schema":{"type":"object"}}
	]}`)
	got := PrefixRequest(payload)

	assertToolNames(t, got, []string{"new_search_web"})
}

// TestPrefixRequestSameNameDiscriminatedByType proves the discriminating case:
// a project tool named bash and Anthropic's own bash declared in one payload
// are told apart by their type, not by their shared name.
func TestPrefixRequestSameNameDiscriminatedByType(t *testing.T) {
	payload := []byte(`{"tools":[
		{"name":"bash","description":"the project's own bash","input_schema":{"type":"object"}},
		{"type":"bash_20250124","name":"bash"}
	]}`)
	got := PrefixRequest(payload)

	assertToolNames(t, got, []string{"new_bash", "bash"})
}

// TestPrefixRequestToolChoiceFollowsDeclaration proves a forced choice is
// rewritten to match the declaration it names — prefixed for a project tool,
// left alone for an Anthropic-defined one — so the two never disagree.
func TestPrefixRequestToolChoiceFollowsDeclaration(t *testing.T) {
	for _, testCase := range exemptionCases() {
		t.Run(testCase.name, func(t *testing.T) {
			payload := []byte(fmt.Sprintf(
				`{"tools":[%s],"tool_choice":{"type":"tool","name":"bash"}}`,
				testCase.declaration,
			))
			got := PrefixRequest(payload)
			if v := gjson.GetBytes(got, "tool_choice.name").String(); v != testCase.want {
				t.Fatalf("tool_choice.name = %q, want %q", v, testCase.want)
			}
		})
	}
}

// TestPrefixRequestHistoryFollowsDeclaration proves a replayed tool_use block
// is rewritten to match the declaration it names, which is what keeps a
// request internally consistent for Anthropic.
func TestPrefixRequestHistoryFollowsDeclaration(t *testing.T) {
	for _, testCase := range exemptionCases() {
		t.Run(testCase.name, func(t *testing.T) {
			payload := []byte(fmt.Sprintf(
				`{"tools":[%s],"messages":[{"role":"assistant","content":[
					{"type":"tool_use","id":"toolu_1","name":"bash","input":{}}
				]}]}`,
				testCase.declaration,
			))
			got := PrefixRequest(payload)
			if v := gjson.GetBytes(got, "messages.0.content.0.name").String(); v != testCase.want {
				t.Fatalf("messages.0.content.0.name = %q, want %q", v, testCase.want)
			}
		})
	}
}

// exemptionCase is one declaration of a tool named bash and the name every
// other location in the payload must carry once the rewrite has run.
type exemptionCase struct {
	name        string // name identifies the kind of tool under test.
	declaration string // declaration is the tools[] entry, as raw JSON.
	want        string // want is the name expected at every referencing location.
}

// exemptionCases returns the two declarations that decide the exemption, so
// tool_choice and replayed history are exercised against the same pair.
func exemptionCases() []exemptionCase {
	return []exemptionCase{
		{
			name:        "project tool",
			declaration: `{"name":"bash","input_schema":{"type":"object"}}`,
			want:        "new_bash",
		},
		{
			name:        "anthropic tool",
			declaration: `{"type":"bash_20250124","name":"bash"}`,
			want:        "bash",
		},
	}
}

// TestPrefixRequestMalformedJSONUnchanged proves a payload that is not valid
// JSON is returned unchanged rather than rejected.
func TestPrefixRequestMalformedJSONUnchanged(t *testing.T) {
	payload := []byte(`{"tools": not valid json`)
	got := PrefixRequest(payload)
	if string(got) != string(payload) {
		t.Fatalf("malformed payload changed: got %s, want %s", got, payload)
	}
}

// TestPrefixRequestEmptyPayloadUnchanged proves nil and empty payloads pass
// through untouched.
func TestPrefixRequestEmptyPayloadUnchanged(t *testing.T) {
	if got := PrefixRequest(nil); got != nil {
		t.Fatalf("nil payload changed: got %v", got)
	}
	if got := PrefixRequest([]byte("")); len(got) != 0 {
		t.Fatalf("empty payload changed: got %v", got)
	}
}

// TestPrefixRequestAbsentFieldsSkipped proves a payload carrying none of the
// three locations passes through unchanged.
func TestPrefixRequestAbsentFieldsSkipped(t *testing.T) {
	payload := []byte(`{}`)
	got := PrefixRequest(payload)
	if string(got) != string(payload) {
		t.Fatalf("empty object payload changed: got %s, want %s", got, payload)
	}
}
