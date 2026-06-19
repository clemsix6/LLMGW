package e2e

import (
	"encoding/base64"
	"testing"
	"time"
)

// TestAccessTokenExpiry verifies that accessTokenExpiry returns the exact time encoded in a
// well-formed JWT's exp claim, and falls back to approximately now+30 minutes for malformed input.
func TestAccessTokenExpiry(t *testing.T) {
	// Build a minimal JWT whose payload carries exp=1700000000.
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1700000000}`))
	token := "eyJhbGciOiJub25lIn0." + payload + ".sig"

	got := accessTokenExpiry(token)
	want := time.Unix(1700000000, 0)

	if !got.Equal(want) {
		t.Errorf("accessTokenExpiry(valid JWT) = %v, want %v", got, want)
	}

	// A token with no dots cannot be split into three parts: must fall back to now+30min.
	before := time.Now()
	fallback := accessTokenExpiry("not-a-jwt-at-all")
	after := time.Now()

	lo := before.Add(30 * time.Minute)
	hi := after.Add(30 * time.Minute)

	if fallback.Before(lo) || fallback.After(hi) {
		t.Errorf("accessTokenExpiry(garbage) = %v, want between %v and %v", fallback, lo, hi)
	}
}
