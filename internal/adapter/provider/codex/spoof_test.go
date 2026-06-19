package codex

import (
	"net/http"
	"testing"
)

// TestSpoofDecorateSetsCodexHeaders asserts decorate sets every Codex client marker the
// backend expects and propagates the per-account ChatGPT account id.
func TestSpoofDecorateSetsCodexHeaders(t *testing.T) {
	req, _ := http.NewRequest(http.MethodPost, "https://example/responses", nil)

	spoof{version: "0.40.0"}.decorate(req, "tok", "acct_123")

	for _, h := range []string{"Authorization", "User-Agent", "originator", "ChatGPT-Account-ID"} {
		if req.Header.Get(h) == "" {
			t.Fatalf("missing header %q", h)
		}
	}
	if req.Header.Get("ChatGPT-Account-ID") != "acct_123" {
		t.Fatal("account id not propagated")
	}
	if req.Header.Get("Authorization") != "Bearer tok" {
		t.Fatalf("Authorization = %q, want %q", req.Header.Get("Authorization"), "Bearer tok")
	}
	if req.Header.Get("originator") != "codex_cli_rs" {
		t.Fatalf("originator = %q, want %q", req.Header.Get("originator"), "codex_cli_rs")
	}
}
