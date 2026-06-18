package e2e

// This is the de-risk spike for the session-key -> Claude Code OAuth bootstrap. It mints an OAuth
// token set from a claude.ai session key against the REAL Anthropic endpoint, then proves the
// minted access token is accepted on /v1/messages through the provider path (which applies the
// Claude Code spoof + billing header). Gated on LLMGW_TEST_SESSION_KEY; skips when absent.
//
//	set -a; . ./.env; set +a; go test ./test/e2e -run Bootstrap -v

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/clemsix6/LLMGW/internal/adapter/provider/claudemax"
	"github.com/clemsix6/LLMGW/internal/domain"
)

// TestBootstrapRealSessionKey bootstraps OAuth tokens from a session key and sends a tiny real
// request through the provider, asserting a 200-path success.
func TestBootstrapRealSessionKey(t *testing.T) {
	sessionKey := os.Getenv("LLMGW_TEST_SESSION_KEY")
	if sessionKey == "" {
		t.Skip("LLMGW_TEST_SESSION_KEY not set; skipping real-API bootstrap spike")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	token, err := claudemax.Bootstrap(ctx, sessionKey, testClaudeCodeVersion)
	if err != nil {
		t.Fatalf("bootstrap from session key: %v", err)
	}
	assertBootstrappedToken(t, token, sessionKey)

	provider := newRealProvider(t, ctx, token)
	req := tinyRequest(t)
	result, body := sendWithRetry(t, ctx, provider, req)
	assertPlausibleReply(t, body, result)
}

// assertBootstrappedToken checks the bootstrap produced a usable, non-expired token set that
// carries the session key forward for later self-healing.
func assertBootstrappedToken(t *testing.T, token domain.Token, sessionKey string) {
	t.Helper()

	if token.AccessToken == "" || token.RefreshToken == "" {
		t.Fatalf("bootstrap returned empty tokens (access=%d refresh=%d chars)", len(token.AccessToken), len(token.RefreshToken))
	}
	if !token.ExpiresAt.After(time.Now()) {
		t.Fatalf("bootstrap returned an already-expired token: %s", token.ExpiresAt)
	}
	if token.SessionKey != sessionKey {
		t.Fatal("bootstrap did not carry the session key forward")
	}
}
