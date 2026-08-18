package toolmarkup

import (
	"strings"
	"testing"
)

// response wraps one tool_use input JSON into a minimal Anthropic response.
func response(input string) []byte {
	return []byte(`{
		"type": "message",
		"content": [
			{"type": "text", "text": "thinking aloud"},
			{"type": "tool_use", "id": "toolu_1", "name": "submit_post", "input": ` + input + `}
		]
	}`)
}

func TestDetectResponseFindsProductionLeakShapes(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "closing antml tag in body",
			input: `{"title": "ok", "body": "</antmlItitle=\"\"> "}`,
		},
		{
			name:  "parameter tag spliced into title",
			input: `{"title": "</antml railway> <parameter name=\"body\">Druckenmiller is"}`,
		},
		{
			name:  "mangled parameter closing",
			input: `{"body": "</antmlameter>\n<parameter name=\"title\">Ohio's Most Active"}`,
		},
		{
			name:  "invoke tag alone",
			input: `{"body": "<invoke name=\"submit_post\">"}`,
		},
		{
			name:  "leak nested under an object value",
			input: `{"meta": {"notes": ["fine", "</antml>"]}}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snippet, found := DetectResponse(response(test.input))
			if !found {
				t.Fatal("DetectResponse() found nothing")
			}
			if snippet == "" || len(snippet) > snippetLimit {
				t.Fatalf("snippet = %q, want bounded non-empty evidence", snippet)
			}
		})
	}
}

func TestDetectResponseKeepsLegitimateContentClean(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "financial prose with comparison",
			input: `{"title": "AAPL", "body": "The stock \"AAPL\" > $200 is overbought."}`,
		},
		{
			name:  "prose using the words invoke and parameter",
			input: `{"body": "Invoke the function with the parameter set to 3."}`,
		},
		{
			name:  "cite tag",
			input: `{"body": "See <cite index=\"ab\">source</cite>."}`,
		},
		{
			name:  "numbers and booleans only",
			input: `{"score": 4, "publish": true}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if snippet, found := DetectResponse(response(test.input)); found {
				t.Fatalf("DetectResponse() = %q, want no leak", snippet)
			}
		})
	}
}

func TestDetectResponseIgnoresTextBlocksAndInvalidJSON(t *testing.T) {
	textOnly := []byte(`{"content": [{"type": "text", "text": "</antml> quoted in prose"}]}`)
	if snippet, found := DetectResponse(textOnly); found {
		t.Fatalf("DetectResponse(text block) = %q, want no leak", snippet)
	}
	if snippet, found := DetectResponse([]byte("not json")); found {
		t.Fatalf("DetectResponse(invalid) = %q, want no leak", snippet)
	}
}

func TestDetectResponseBoundsAndFlattensSnippet(t *testing.T) {
	long := `{"body": "</antml railway> ` + strings.Repeat("x", 200) + `\nnext line"}`
	snippet, found := DetectResponse(response(long))
	if !found {
		t.Fatal("DetectResponse() found nothing")
	}
	if len(snippet) != snippetLimit {
		t.Fatalf("snippet length = %d, want %d", len(snippet), snippetLimit)
	}
	if strings.Contains(snippet, "\n") {
		t.Fatalf("snippet %q carries a newline", snippet)
	}
}
