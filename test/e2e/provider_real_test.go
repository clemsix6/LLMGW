package e2e

// This suite hits the REAL Anthropic API through the Claude Max OAuth provider — it is the
// core validation that the OAuth refresh + Claude Code spoof still pass Anthropic's
// anti-abuse today. It is gated on LLMGW_TEST_REFRESH_TOKEN and skips when absent.
//
// Token rotation: an OAuth refresh rotates the refresh_token. After a successful call the
// rotated token is persisted to the (ephemeral) store; this test writes it back into the
// repo .env (LLMGW_TEST_REFRESH_TOKEN and LLMGW_OAUTH_REFRESH_TOKENS) so a re-run against a
// fresh DB uses the latest token. Re-source .env before each run:
//
//	set -a; . ./.env; set +a; go test ./test/e2e -run Provider -v
//
// The write-back is registered via t.Cleanup right after the provider is built, so it runs even
// if a later assertion fails or sendWithRetry exhausts its retries after a refresh already
// rotated and persisted the token — the .env always ends with the latest refresh_token.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"

	"github.com/clemsix6/LLMGW/internal/adapter/postgres"
	"github.com/clemsix6/LLMGW/internal/adapter/provider/claudemax"
	"github.com/clemsix6/LLMGW/internal/domain"
	"github.com/clemsix6/LLMGW/internal/domain/llm"
	"github.com/clemsix6/LLMGW/internal/domain/usage"
)

const (
	// testAccount is the OAuth account label the E2E seeds and the provider serves.
	testAccount = "acct1"

	// testClaudeCodeVersion is the spoofed Claude Code version used in the E2E.
	testClaudeCodeVersion = "2.1.181"

	// minContentLength is the lower bound (chars) for a plausible-length model reply (spec §11).
	minContentLength = 3

	// maxAttempts bounds retries of transient (5xx/network/timeout) upstream errors.
	maxAttempts = 4
)

// captureSink buffers the relayed response body for assertions.
type captureSink struct {
	buf bytes.Buffer // buf accumulates the bytes the provider relays.
}

// Write appends to the buffer.
func (s *captureSink) Write(p []byte) (int, error) {
	return s.buf.Write(p)
}

// Flush is a no-op: the buffer is read after Send returns.
func (s *captureSink) Flush() {}

// TestProviderRealNonStreaming sends a tiny real request through the Claude Max provider and
// asserts a 200-path success: relayed content of plausible length and metered output tokens.
func TestProviderRealNonStreaming(t *testing.T) {
	seedToken := os.Getenv("LLMGW_TEST_REFRESH_TOKEN")
	if seedToken == "" {
		t.Skip("LLMGW_TEST_REFRESH_TOKEN not set; skipping real-API provider test")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	provider := newRealProvider(t, ctx, seedToken)

	req := tinyRequest(t)
	result, body := sendWithRetry(t, ctx, provider, req)

	assertPlausibleReply(t, body, result)
}

// newRealProvider boots an ephemeral Postgres, seeds the refresh token, and builds a Claude
// Max provider bound to the test account. It registers the .env token write-back via t.Cleanup
// so the rotated refresh_token is persisted even if a later assertion fails or sendWithRetry
// exhausts its retries after a refresh already rotated the token.
func newRealProvider(t *testing.T, ctx context.Context, seedToken string) *claudemax.Provider {
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

	if err := store.SaveToken(ctx, testAccount, domain.Token{RefreshToken: seedToken}); err != nil {
		t.Fatalf("seed token: %v", err)
	}

	provider := claudemax.New(store, testAccount, testClaudeCodeVersion)

	// Write the rotated refresh token back to .env on test exit (LIFO: before store.Close), so it
	// runs even on a later assertion failure or retry exhaustion after a refresh persisted a token.
	t.Cleanup(func() { rotateEnvToken(t, ctx, store) })

	return provider
}

// tinyRequest builds a minimal non-streaming Messages request.
func tinyRequest(t *testing.T) llm.ChatRequest {
	t.Helper()

	const body = `{
		"model": "claude-sonnet-4-6",
		"max_tokens": 16,
		"messages": [{"role": "user", "content": "Reply with a single word."}]
	}`

	req, err := llm.ParseRequest([]byte(body))
	if err != nil {
		t.Fatalf("parse request: %v", err)
	}
	return req
}

// sendWithRetry calls Send, retrying transient API errors with bounded backoff. It stops
// immediately on a dead refresh token or a rate limit (those are not transient).
func sendWithRetry(t *testing.T, ctx context.Context, provider *claudemax.Provider, req llm.ChatRequest) (usage.Usage, []byte) {
	t.Helper()

	backoff := time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		sink := &captureSink{}
		attemptCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		result, err := provider.Send(attemptCtx, req, sink)
		cancel()

		if err == nil {
			return result, sink.buf.Bytes()
		}

		failFastOrRetry(t, err, attempt, &backoff)
	}

	t.Fatalf("exhausted %d attempts against the real API", maxAttempts)
	return usage.Usage{}, nil
}

// failFastOrRetry fails the test on a non-transient error (dead token, rate limit, 4xx) or
// sleeps before the next attempt on a transient one.
func failFastOrRetry(t *testing.T, err error, attempt int, backoff *time.Duration) {
	t.Helper()

	var dead *claudemax.DeadRefreshTokenError
	if errors.As(err, &dead) {
		t.Fatalf("STOP: dead refresh token (invalid_grant), re-seed required: %v", err)
	}

	var rate *claudemax.RateLimitError
	if errors.As(err, &rate) {
		t.Fatalf("upstream 429 (spoof rejected or account rate-limited): %v", err)
	}

	if !transient(err) {
		t.Fatalf("non-transient upstream error: %v", err)
	}

	if attempt == maxAttempts {
		return
	}
	time.Sleep(*backoff)
	*backoff *= 2
}

// transient reports whether err is worth retrying: a 5xx upstream or a network/timeout error.
func transient(err error) bool {
	var upstream *claudemax.UpstreamError
	if errors.As(err, &upstream) {
		return upstream.Status >= 500
	}

	var netErr net.Error
	return errors.As(err, &netErr)
}

// anthropicResponse is the slice of the Messages response the assertions inspect.
type anthropicResponse struct {
	Content []struct {
		Type string `json:"type"` // Type is the content block type (e.g. "text").
		Text string `json:"text"` // Text is the generated text of a text block.
	} `json:"content"`
	StopReason string `json:"stop_reason"` // StopReason is why generation ended.
}

// assertPlausibleReply checks the relayed body parses, carries non-empty text of a plausible
// length, and that the metered usage reports generated tokens.
func assertPlausibleReply(t *testing.T, body []byte, result usage.Usage) {
	t.Helper()

	var parsed anthropicResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("response is not valid JSON (%v): %s", err, body)
	}

	text := joinText(parsed)
	if len(text) < minContentLength {
		t.Fatalf("reply content too short (%d chars): %q", len(text), text)
	}

	if result.OutputTokens <= 0 {
		t.Fatalf("Usage.OutputTokens = %d, want > 0", result.OutputTokens)
	}

	t.Logf("real-API 200: input_tokens=%d output_tokens=%d content_len=%d stop_reason=%q reply=%q",
		result.InputTokens, result.OutputTokens, len(text), parsed.StopReason, text)
}

// joinText concatenates the text of all text content blocks.
func joinText(resp anthropicResponse) string {
	var b strings.Builder
	for _, block := range resp.Content {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

// rotateEnvToken reads the rotated refresh token the successful call persisted and writes it
// back to the repo .env so the gated suite stays re-runnable against a fresh DB.
func rotateEnvToken(t *testing.T, ctx context.Context, store *postgres.Store) {
	t.Helper()

	rotated, err := store.LoadToken(ctx, testAccount)
	if err != nil {
		t.Logf("skip token write-back: load rotated token: %v", err)
		return
	}
	if rotated.RefreshToken == "" {
		t.Log("skip token write-back: rotated refresh token is empty")
		return
	}

	writeEnvToken(t, rotated.RefreshToken)
}

// writeEnvToken rewrites the refresh-token lines of the repo .env in place.
func writeEnvToken(t *testing.T, newToken string) {
	t.Helper()

	path := envFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Logf("skip token write-back: read %s: %v", path, err)
		return
	}

	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, "LLMGW_TEST_REFRESH_TOKEN="):
			lines[i] = "LLMGW_TEST_REFRESH_TOKEN=" + newToken
		case strings.HasPrefix(line, "LLMGW_OAUTH_REFRESH_TOKENS="):
			lines[i] = "LLMGW_OAUTH_REFRESH_TOKENS=" + testAccount + "=" + newToken
		}
	}

	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o600); err != nil {
		t.Logf("skip token write-back: write %s: %v", path, err)
		return
	}
	t.Logf("rotated refresh token written back to %s", path)
}

// envFilePath returns the repo-root .env path, resolved relative to this test file so it is
// independent of the working directory.
func envFilePath() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", ".env")
}
