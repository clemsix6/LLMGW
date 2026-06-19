package e2e

// This smoke hits the REAL ChatGPT Codex subscription through the codex OAuth provider — the
// core validation that the OpenAI OAuth refresh + Codex client spoof reach a 200 on
// chatgpt.com/backend-api/codex/responses today. It is gated on real test credentials and SKIPS
// cleanly when they are absent (this environment has none), so the suite still compiles and runs.
//
// Provide credentials to exercise it (the refresh token rotates on use — re-seed after a run):
//
//	export LLMGW_CODEX_TEST_REFRESH_TOKEN=...   # the account's durable OAuth refresh token
//	export LLMGW_CODEX_TEST_ACCOUNT_ID=...      # the ChatGPT-Account-ID (acct_...)
//	go test ./test/e2e -run CodexReal -v

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/adapter/provider/codex"
	"github.com/clemsix6/LLMGW/internal/domain/llm"
)

const (
	// codexTestAccount is the account label the smoke seeds and the provider serves.
	codexTestAccount = "codex1"

	// testCodexVersion is the spoofed Codex client version used in the smoke.
	testCodexVersion = "0.40.0"
)

// TestCodexRealSmoke seeds a real Codex account and proves the provider reaches a 200 with
// non-empty relayed content through the OAuth + spoof path. It skips when credentials are absent.
func TestCodexRealSmoke(t *testing.T) {
	refreshToken, accountID := codexCreds(t)

	ctx := context.Background()
	provider := newCodexProvider(t, ctx, refreshToken, accountID)

	body := codexSendWithRetry(t, ctx, provider)

	if len(body) < minContentLength {
		t.Fatalf("relayed content too short (%d bytes): %q", len(body), body)
	}
	t.Logf("codex real 200: relayed %d bytes", len(body))
}

// codexCreds returns the seeded refresh token and account id, skipping the test when either is
// absent. A partial pair fails loudly so a half-configured run is not silently skipped.
func codexCreds(t *testing.T) (refreshToken, accountID string) {
	t.Helper()

	refreshToken = os.Getenv("LLMGW_CODEX_TEST_REFRESH_TOKEN")
	accountID = os.Getenv("LLMGW_CODEX_TEST_ACCOUNT_ID")
	if refreshToken == "" && accountID == "" {
		t.Skip("LLMGW_CODEX_TEST_REFRESH_TOKEN / LLMGW_CODEX_TEST_ACCOUNT_ID not set; skipping codex smoke")
	}
	if refreshToken == "" || accountID == "" {
		t.Fatal("both LLMGW_CODEX_TEST_REFRESH_TOKEN and LLMGW_CODEX_TEST_ACCOUNT_ID must be set")
	}
	return refreshToken, accountID
}

// newCodexProvider boots an ephemeral Postgres, seeds the Codex account, and builds the provider.
func newCodexProvider(t *testing.T, ctx context.Context, refreshToken, accountID string) *codex.Provider {
	t.Helper()

	container, dsn, err := startPostgres(ctx)
	if err != nil {
		t.Fatalf("start postgres: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	store, err := postgres.New(ctx, dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(store.Close)

	if err := store.SeedCodexAccount(ctx, codexTestAccount, refreshToken, accountID); err != nil {
		t.Fatalf("seed codex account: %v", err)
	}
	return codex.New(store, testCodexVersion)
}

// codexSendWithRetry calls Send, retrying transient (5xx/network) errors with bounded backoff. It
// stops immediately on a dead refresh token or a rate limit (those are not transient).
func codexSendWithRetry(t *testing.T, ctx context.Context, provider *codex.Provider) []byte {
	t.Helper()

	backoff := time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		sink := &captureSink{}
		attemptCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		_, err := provider.Send(attemptCtx, codexTinyRequest(t), sink)
		cancel()

		if err == nil {
			return sink.buf.Bytes()
		}
		codexFailFastOrRetry(t, err, attempt, &backoff)
	}

	t.Fatalf("exhausted %d attempts against the real codex API", maxAttempts)
	return nil
}

// codexFailFastOrRetry fails on a non-transient error (dead token, rate limit, non-5xx upstream)
// or sleeps before the next attempt on a transient one.
func codexFailFastOrRetry(t *testing.T, err error, attempt int, backoff *time.Duration) {
	t.Helper()

	var dead *codex.DeadRefreshTokenError
	if errors.As(err, &dead) {
		t.Fatalf("STOP: dead refresh token (invalid_grant), re-seed required: %v", err)
	}

	var rate *codex.RateLimitError
	if errors.As(err, &rate) {
		t.Fatalf("upstream 429 (spoof rejected or account rate-limited): %v", err)
	}

	if !codexTransient(err) {
		t.Fatalf("non-transient upstream error: %v", err)
	}
	if attempt == maxAttempts {
		return
	}
	time.Sleep(*backoff)
	*backoff *= 2
}

// codexTransient reports whether err is worth retrying: a 5xx upstream or a network error.
func codexTransient(err error) bool {
	var upstream *codex.UpstreamError
	if errors.As(err, &upstream) {
		return upstream.Status >= 500
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}

// codexTinyRequest builds a minimal request; the skeleton provider ignores its body.
func codexTinyRequest(t *testing.T) llm.Request {
	t.Helper()

	req, err := llm.ParseRequest([]byte(`{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	return req
}
