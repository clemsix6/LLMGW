package llm

import (
	"encoding/json"
	"testing"
)

func TestParseRequestRejectsInvalidJSON(t *testing.T) {
	if _, err := ParseRequest([]byte("not json")); err == nil {
		t.Fatal("ParseRequest(invalid) error = nil, want an error")
	}
}

func TestModelAndStream(t *testing.T) {
	req, err := ParseRequest([]byte(`{"model":"claude-sonnet-4-6","stream":true}`))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if req.Model() != "claude-sonnet-4-6" {
		t.Errorf("Model() = %q, want %q", req.Model(), "claude-sonnet-4-6")
	}
	if !req.Stream() {
		t.Error("Stream() = false, want true")
	}
}

func TestModelAndStreamDefaults(t *testing.T) {
	req, err := ParseRequest([]byte(`{}`))
	if err != nil {
		t.Fatalf("ParseRequest: %v", err)
	}
	if req.Model() != "" {
		t.Errorf("Model() = %q, want empty", req.Model())
	}
	if req.Stream() {
		t.Error("Stream() = true, want false (absent defaults to false)")
	}
}

func TestFirstUserText(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "string content",
			body: `{"messages":[{"role":"user","content":"hey there"}]}`,
			want: "hey there",
		},
		{
			name: "first text block of block content",
			body: `{"messages":[{"role":"user","content":[{"type":"image","source":{}},{"type":"text","text":"abcdefg"},{"type":"text","text":"ignored"}]}]}`,
			want: "abcdefg",
		},
		{
			name: "skips non-user messages",
			body: `{"messages":[{"role":"assistant","content":"nope"},{"role":"user","content":"yes"}]}`,
			want: "yes",
		},
		{
			name: "no user message",
			body: `{"messages":[{"role":"assistant","content":"nope"}]}`,
			want: "",
		},
		{
			name: "no messages",
			body: `{"model":"x"}`,
			want: "",
		},
		{
			name: "user block with no text block",
			body: `{"messages":[{"role":"user","content":[{"type":"image","source":{}}]}]}`,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := ParseRequest([]byte(tc.body))
			if err != nil {
				t.Fatalf("ParseRequest: %v", err)
			}
			if got := req.FirstUserText(); got != tc.want {
				t.Errorf("FirstUserText() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestWithClaudeCodeSystemAbsent(t *testing.T) {
	req := parse(t, `{"model":"x"}`)

	system := systemBlocks(t, req.WithClaudeCodeSystem("BILLING"))

	if len(system) != 1 {
		t.Fatalf("system blocks = %d, want 1", len(system))
	}
	assertTextBlock(t, system[0], "BILLING")
}

func TestWithClaudeCodeSystemString(t *testing.T) {
	req := parse(t, `{"system":"original system"}`)

	system := systemBlocks(t, req.WithClaudeCodeSystem("BILLING"))

	if len(system) != 2 {
		t.Fatalf("system blocks = %d, want 2", len(system))
	}
	assertTextBlock(t, system[0], "BILLING")
	assertTextBlock(t, system[1], "original system")
}

func TestWithClaudeCodeSystemEmptyStringTreatedAsAbsent(t *testing.T) {
	req := parse(t, `{"system":"   "}`)

	system := systemBlocks(t, req.WithClaudeCodeSystem("BILLING"))

	if len(system) != 1 {
		t.Fatalf("system blocks = %d, want 1 (empty string dropped)", len(system))
	}
	assertTextBlock(t, system[0], "BILLING")
}

func TestWithClaudeCodeSystemArray(t *testing.T) {
	req := parse(t, `{"system":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`)

	system := systemBlocks(t, req.WithClaudeCodeSystem("BILLING"))

	if len(system) != 3 {
		t.Fatalf("system blocks = %d, want 3", len(system))
	}
	assertTextBlock(t, system[0], "BILLING")
	assertTextBlock(t, system[1], "a")
	assertTextBlock(t, system[2], "b")
}

func TestWithClaudeCodeSystemDoesNotMutateOriginal(t *testing.T) {
	req := parse(t, `{"system":"original"}`)

	_ = req.WithClaudeCodeSystem("BILLING")

	if got := req.body["system"]; got != "original" {
		t.Fatalf("original system mutated to %v, want %q", got, "original")
	}
}

func TestBytesPreservesUnknownFields(t *testing.T) {
	req := parse(t, `{"model":"x","tools":[{"name":"t"}],"max_tokens":16}`)

	var decoded map[string]any
	if err := json.Unmarshal(req.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal Bytes(): %v", err)
	}
	if _, ok := decoded["tools"]; !ok {
		t.Error("Bytes() dropped the tools field")
	}
	if decoded["model"] != "x" {
		t.Errorf("Bytes() model = %v, want x", decoded["model"])
	}
}

// parse decodes body into a ChatRequest, failing the test on error.
func parse(t *testing.T, body string) ChatRequest {
	t.Helper()
	req, err := ParseRequest([]byte(body))
	if err != nil {
		t.Fatalf("ParseRequest(%s): %v", body, err)
	}
	return req
}

// systemBlocks marshals then re-decodes the request and returns its system array.
func systemBlocks(t *testing.T, req ChatRequest) []any {
	t.Helper()

	var decoded map[string]any
	if err := json.Unmarshal(req.Bytes(), &decoded); err != nil {
		t.Fatalf("unmarshal request bytes: %v", err)
	}
	system, ok := decoded["system"].([]any)
	if !ok {
		t.Fatalf("system is not an array: %T", decoded["system"])
	}
	return system
}

// assertTextBlock asserts a system block is a text block carrying the wanted text.
func assertTextBlock(t *testing.T, block any, wantText string) {
	t.Helper()

	obj, ok := block.(map[string]any)
	if !ok {
		t.Fatalf("block is not an object: %T", block)
	}
	if obj["type"] != "text" {
		t.Errorf("block type = %v, want text", obj["type"])
	}
	if obj["text"] != wantText {
		t.Errorf("block text = %v, want %q", obj["text"], wantText)
	}
}
