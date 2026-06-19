package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/clemsix6/LLMGW/internal/domain"
)

const (
	// headerProject carries the project name; it is required on every /v1/messages request.
	headerProject = "X-Project"

	// headerTags carries the budget bucket the call is attributed to.
	headerTags = "X-Tags"

	// statusOK and statusError are the recorded outcomes of a usage_event.
	statusOK    = "ok"
	statusError = "error"
)

// resolveProject returns the project a request is attributed to: the X-Project header when
// present, otherwise the configured default project. ok is false when neither is set, which
// the caller maps to a 400 — a request must always be attributable to a project.
func resolveProject(header, fallback string) (string, bool) {
	if header != "" {
		return header, true
	}
	if fallback != "" {
		return fallback, true
	}
	return "", false
}

// readBody reads the entire request body and returns it as bytes.
func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body)
}

// bufferedSink buffers what the provider relays so the handler can decide HTTP status and
// headers after Send returns. For non-streaming responses the provider writes the full body
// then flushes once; the handler copies the buffer to the consumer on success.
type bufferedSink struct {
	buf bytes.Buffer // buf accumulates the relayed response body.
}

// Write appends to the buffer.
func (s *bufferedSink) Write(p []byte) (int, error) {
	return s.buf.Write(p)
}

// Flush is a no-op: the buffer is read after Send returns.
func (s *bufferedSink) Flush() {}

// writeSuccess relays a buffered upstream response to the consumer as a 200.
func writeSuccess(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// writeProviderError maps a provider error to a clean HTTP status via the domain.ProviderError
// contract: the error itself carries the status, type, and optional Retry-After. Anything that
// does not implement the contract falls back to 500 "internal".
func writeProviderError(w http.ResponseWriter, err error) {
	var pe domain.ProviderError
	if errors.As(err, &pe) {
		if d, ok := pe.RetryAfter(); ok {
			w.Header().Set("Retry-After", retryAfterDuration(d))
		}
		writeError(w, pe.HTTPStatus(), pe.ErrorType(), pe.Error())
		return
	}
	writeError(w, http.StatusInternalServerError, "internal", err.Error())
}

// retryAfterDuration renders a Retry-After delay (whole seconds, at least one) for a duration.
func retryAfterDuration(d time.Duration) string {
	seconds := int(d.Seconds())
	if seconds < 1 {
		seconds = 1
	}
	return strconv.Itoa(seconds)
}

// errorBody is the gateway's JSON error envelope for non-2xx responses.
type errorBody struct {
	Error errorDetail `json:"error"` // Error holds the typed error detail.
}

// errorDetail describes a gateway error: a stable machine-readable type and a human message.
// Code and Param are nullable OpenAI/Anthropic-compatible fields included when set.
type errorDetail struct {
	Type string `json:"type"` // Type is a stable error classifier (e.g. "rate_limited").

	Message string `json:"message"` // Message is a human-readable description.

	Code *string `json:"code,omitempty"` // Code is an optional provider-specific error code.

	Param *string `json:"param,omitempty"` // Param is the request parameter that triggered the error, if any.
}

// writeError writes a typed JSON error response with the given HTTP status.
func writeError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{Error: errorDetail{Type: errType, Message: message}})
}
