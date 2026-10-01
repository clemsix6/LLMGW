package governance

import (
	"strings"
	"testing"
)

// TestParseErrorDetail proves only the error type and message are read, and
// only from the standard error JSON.
func TestParseErrorDetail(t *testing.T) {
	tests := []struct {
		name string
		body string
		want *ErrorDetail
	}{
		{
			name: "anthropic",
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"Invalid ` + "`signature`" + ` in thinking block"}}`,
			want: &ErrorDetail{Type: "invalid_request_error", Message: "Invalid `signature` in thinking block"},
		},
		{
			name: "openai with extra fields ignored",
			body: `{"error":{"message":"bad","type":"invalid_request_error","code":"x","param":"secret-param"}}`,
			want: &ErrorDetail{Type: "invalid_request_error", Message: "bad"},
		},
		{name: "type only", body: `{"error":{"type":"budget_exceeded","dimension":"cost"}}`, want: &ErrorDetail{Type: "budget_exceeded"}},
		{name: "plain text", body: `unknown provider for model x`, want: nil},
		{name: "html", body: `<html>oops sk-secret</html>`, want: nil},
		{name: "error is a string", body: `{"error":"sk-secret leaked"}`, want: nil},
		{name: "no error object", body: `{"message":"top level"}`, want: nil},
		{name: "empty error object", body: `{"error":{}}`, want: nil},
		{name: "truncated json", body: `{"error":{"type":"x","message":"y"`, want: nil},
		{name: "empty", body: ``, want: nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := ParseErrorDetail([]byte(test.body))
			if (got == nil) != (test.want == nil) || (got != nil && *got != *test.want) {
				t.Fatalf("ParseErrorDetail = %#v, want %#v", got, test.want)
			}
		})
	}
}

// TestParseErrorDetailTruncates proves the message is cut to the bound on a
// rune boundary.
func TestParseErrorDetailTruncates(t *testing.T) {
	long := strings.Repeat("é", MaxErrorMessageRunes+50)
	got := ParseErrorDetail([]byte(`{"error":{"type":"t","message":"` + long + `"}}`))
	if got == nil || len([]rune(got.Message)) != MaxErrorMessageRunes {
		t.Fatalf("message runes = %v, want %d", got, MaxErrorMessageRunes)
	}
	if got.Message != strings.Repeat("é", MaxErrorMessageRunes) {
		t.Fatal("truncation split a rune")
	}
}

// TestIsClientErrorStatus proves only 4xx statuses qualify.
func TestIsClientErrorStatus(t *testing.T) {
	for status, want := range map[int]bool{399: false, 400: true, 429: true, 499: true, 500: false, 0: false} {
		if IsClientErrorStatus(status) != want {
			t.Fatalf("IsClientErrorStatus(%d) != %v", status, want)
		}
	}
}
