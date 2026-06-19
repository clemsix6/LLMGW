package httpserver

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeProvErr is a test implementation of domain.ProviderError used to verify that
// writeProviderError routes via the interface contract rather than concrete types.
type fakeProvErr struct {
	status int           // status is the HTTP status to return.
	typ    string        // typ is the stable error type string.
	after  time.Duration // after is the Retry-After duration.
	has    bool          // has is whether a Retry-After header should be set.
}

// Error implements the error interface.
func (e fakeProvErr) Error() string { return e.typ }

// HTTPStatus returns the HTTP status code for this error.
func (e fakeProvErr) HTTPStatus() int { return e.status }

// ErrorType returns the stable machine-readable error type.
func (e fakeProvErr) ErrorType() string { return e.typ }

// RetryAfter returns the backoff duration and whether one applies.
func (e fakeProvErr) RetryAfter() (time.Duration, bool) { return e.after, e.has }

// TestWriteProviderErrorUsesContract verifies that writeProviderError routes a ProviderError
// through the interface contract: correct HTTP status, Retry-After header, and error type.
func TestWriteProviderErrorUsesContract(t *testing.T) {
	rec := httptest.NewRecorder()
	writeProviderError(rec, fakeProvErr{status: 503, typ: "all_cooling", after: 90 * time.Second, has: true})

	if rec.Code != 503 || rec.Header().Get("Retry-After") != "90" {
		t.Fatalf("status=%d retry=%q", rec.Code, rec.Header().Get("Retry-After"))
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("failed to decode response body: %v", err)
	}
	errDetail, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("error field is missing or not an object")
	}
	errType, ok := errDetail["type"].(string)
	if !ok || errType != "all_cooling" {
		t.Fatalf("error.type=%v, want all_cooling", errType)
	}
}
