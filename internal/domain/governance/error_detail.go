package governance

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"
)

const (
	// MaxErrorMessageRunes bounds a stored error message.
	MaxErrorMessageRunes = 1000
	// maxErrorTypeRunes bounds a stored error type.
	maxErrorTypeRunes = 100
)

// ErrorDetail is the diagnosable part of a provider-style error response.
//
// Only the error's type and message are ever kept. Those two fields are
// written by the party that rejects the request to describe why, while the
// rest of an exchange (request payload, headers, credentials, any body that
// is not the standard error JSON) can carry client content or secrets and is
// never stored.
type ErrorDetail struct {
	Type    string // Type is the provider's error classification.
	Message string // Message is the provider's human-readable reason, bounded in length.
}

// errorEnvelope is the standard `{"error":{"type":...,"message":...}}` shape
// shared by the Anthropic and OpenAI wire formats.
type errorEnvelope struct {
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// ParseErrorDetail extracts the error type and message from a standard error
// JSON body. It returns nil for any other body, so nothing is stored for a
// body that is not the standard error JSON.
func ParseErrorDetail(body []byte) *ErrorDetail {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil
	}
	var envelope errorEnvelope
	if err := json.Unmarshal(trimmed, &envelope); err != nil || envelope.Error == nil {
		return nil
	}
	detail := ErrorDetail{
		Type:    truncateRunes(strings.TrimSpace(envelope.Error.Type), maxErrorTypeRunes),
		Message: truncateRunes(strings.TrimSpace(envelope.Error.Message), MaxErrorMessageRunes),
	}
	if detail.Type == "" && detail.Message == "" {
		return nil
	}
	return &detail
}

// IsClientErrorStatus reports whether a status is a 4xx, the only class whose
// error text is recorded.
func IsClientErrorStatus(status int) bool {
	return status >= 400 && status < 500
}

// truncateRunes cuts text to at most limit runes without splitting one.
func truncateRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	return string([]rune(text)[:limit])
}
