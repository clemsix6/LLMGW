package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/provider/claudemax"
)

// TestWriteProviderErrorAllCooling proves the handler maps a provider *AllCoolingError to a 503
// with a whole-seconds Retry-After header and the typed "all_cooling" body.
func TestWriteProviderErrorAllCooling(t *testing.T) {
	rec := httptest.NewRecorder()

	writeProviderError(rec, &claudemax.AllCoolingError{RetryAfter: 90 * time.Second})

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "90" {
		t.Fatalf("Retry-After = %q, want %q", got, "90")
	}

	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON (%v): %s", err, rec.Body.Bytes())
	}
	if body.Error.Type != "all_cooling" {
		t.Fatalf("error type = %q, want all_cooling", body.Error.Type)
	}
}

// TestRetryAfterDurationFloorsToOneSecond proves a sub-second (or non-positive) RetryAfter still
// renders a meaningful Retry-After of at least one second.
func TestRetryAfterDurationFloorsToOneSecond(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{90 * time.Second, "90"},
		{500 * time.Millisecond, "1"},
		{0, "1"},
		{-5 * time.Second, "1"},
	}

	for _, c := range cases {
		if got := retryAfterDuration(c.d); got != c.want {
			t.Errorf("retryAfterDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
