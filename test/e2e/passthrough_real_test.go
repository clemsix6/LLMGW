package e2e

// This suite drives the FULL gateway (POST /v1/messages) against the REAL Anthropic API: it
// boots the gateway + an ephemeral Postgres, seeds a Claude Max refresh token, forwards a tiny
// real request, and asserts both the HTTP response and the recorded DB state. It is the proof
// that LLMGW is a working drop-in replacement for clewdr's /code path. Gated on
// LLMGW_TEST_REFRESH_TOKEN; skips when absent.
//
// Re-source .env before each run so the rotated refresh token is picked up:
//
//	set -a; . ./.env; set +a; go test ./test/e2e -run Passthrough -v
//
// The .env write-back is registered via t.Cleanup right after seeding, so the rotated token is
// persisted even if a later assertion fails or the retry budget is exhausted after a refresh.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
)

const (
	// passthroughProject is the X-Project the passthrough suite tracks usage under.
	passthroughProject = "e2e"

	// passthroughTag is the X-Tags bucket the passthrough suite attributes usage to.
	passthroughTag = "news"
)

// TestPassthroughRealNonStreaming forwards a tiny real request through the full gateway and
// asserts a 200 with plausible content, plus a project row and an attributed usage_event.
func TestPassthroughRealNonStreaming(t *testing.T) {
	seedToken := os.Getenv("LLMGW_TEST_REFRESH_TOKEN")
	if seedToken == "" {
		t.Skip("LLMGW_TEST_REFRESH_TOKEN not set; skipping real-API passthrough test")
	}
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	harness := startPassthroughHarness(t, ctx, seedToken)

	body := postMessagesWithRetry(t, ctx, harness)
	assertPlausibleMessagesBody(t, body)

	assertProjectExists(t, ctx, harness.DSN, passthroughProject)
	assertUsageRecorded(t, ctx, harness.DSN, passthroughProject, passthroughTag)
}

// startPassthroughHarness boots the gateway, seeds the Claude Max provider, and registers the
// .env token write-back so the rotated refresh token survives even on a later failure.
func startPassthroughHarness(t *testing.T, ctx context.Context, seedToken string) *Harness {
	t.Helper()

	harness, err := Start(ctx)
	if err != nil {
		t.Fatalf("start harness: %v", err)
	}
	t.Cleanup(func() { harness.Stop(context.Background()) })

	if err := harness.SeedClaudeMax(ctx, testAccount, seedToken, testClaudeCodeVersion); err != nil {
		t.Fatalf("seed claude max: %v", err)
	}

	// LIFO: this runs before harness.Stop closes the store, so the rotated token is readable.
	t.Cleanup(func() { rotateEnvToken(t, ctx, harness.store) })

	return harness
}

// postMessagesWithRetry POSTs the tiny request, retrying transient gateway 5xx with bounded
// backoff. It fails fast on a dead refresh token or a rate limit (the gateway's own 503/4xx).
func postMessagesWithRetry(t *testing.T, ctx context.Context, h *Harness) []byte {
	t.Helper()

	const body = `{
		"model": "claude-sonnet-4-6",
		"max_tokens": 16,
		"messages": [{"role": "user", "content": "Reply with a single word."}]
	}`
	headers := map[string]string{
		"Content-Type": "application/json",
		"X-Project":    passthroughProject,
		"X-Tags":       passthroughTag,
	}

	backoff := time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if respBody, done := attemptPost(t, ctx, h, []byte(body), headers, attempt, &backoff); done {
			return respBody
		}
	}

	t.Fatalf("exhausted %d attempts against the real API", maxAttempts)
	return nil
}

// attemptPost performs one POST and decides the outcome: it returns the body with done=true on
// a 200, fails the test on a fatal status, or sleeps and returns done=false on a transient one.
func attemptPost(t *testing.T, ctx context.Context, h *Harness, body []byte, headers map[string]string, attempt int, backoff *time.Duration) ([]byte, bool) {
	t.Helper()

	attemptCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	resp, err := h.Post(attemptCtx, "/v1/messages", body, headers)
	if err != nil {
		retryTransient(t, attempt, backoff, "POST /v1/messages: "+err.Error())
		return nil, false
	}

	respBody := readBody(t, resp)
	if resp.StatusCode == http.StatusOK {
		return respBody, true
	}

	classifyFailure(t, resp.StatusCode, respBody, attempt, backoff)
	return nil, false
}

// classifyFailure fails fast on the gateway's own assertions (dead token, rate limit, other
// 4xx) and retries transient 5xx errors.
func classifyFailure(t *testing.T, status int, body []byte, attempt int, backoff *time.Duration) {
	t.Helper()

	switch errorType(body) {
	case "dead_refresh_token":
		t.Fatalf("STOP: dead refresh token (invalid_grant), re-seed required: %s", body)
	case "rate_limited":
		t.Fatalf("gateway 503 (spoof rejected or account rate-limited): %s", body)
	}

	if status < 500 {
		t.Fatalf("non-retryable gateway status %d: %s", status, body)
	}
	retryTransient(t, attempt, backoff, "transient gateway status: "+http.StatusText(status))
}

// retryTransient sleeps before the next attempt, or fails the test once the budget is exhausted.
func retryTransient(t *testing.T, attempt int, backoff *time.Duration, reason string) {
	t.Helper()

	if attempt == maxAttempts {
		t.Fatalf("exhausted retries (%s)", reason)
	}
	time.Sleep(*backoff)
	*backoff *= 2
}

// assertPlausibleMessagesBody checks the relayed body parses and carries non-empty text of a
// plausible length (reusing the provider suite's anthropicResponse shape).
func assertPlausibleMessagesBody(t *testing.T, body []byte) {
	t.Helper()

	var parsed anthropicResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("response is not valid JSON (%v): %s", err, body)
	}

	text := joinText(parsed)
	if len(text) < minContentLength {
		t.Fatalf("reply content too short (%d chars): %q", len(text), text)
	}
	t.Logf("gateway 200: content_len=%d stop_reason=%q reply=%q", len(text), parsed.StopReason, text)
}

// assertProjectExists asserts the named project row was auto-created.
func assertProjectExists(t *testing.T, ctx context.Context, dsn, name string) {
	t.Helper()

	conn := connectDB(t, ctx, dsn)
	defer conn.Close(ctx)

	var exists bool
	const query = `SELECT EXISTS (SELECT 1 FROM project WHERE name = $1)`
	if err := conn.QueryRow(ctx, query, name).Scan(&exists); err != nil {
		t.Fatalf("query project %q: %v", name, err)
	}
	if !exists {
		t.Errorf("expected project row %q to exist after the request", name)
	}
}

// assertUsageRecorded asserts a successful usage_event was recorded for (project, tag) with
// output tokens (metering attribution) and a positive notional cost (the requested model
// claude-sonnet-4-6 is priced by migration 0003, so cost_usd must be > 0).
func assertUsageRecorded(t *testing.T, ctx context.Context, dsn, project, tag string) {
	t.Helper()

	conn := connectDB(t, ctx, dsn)
	defer conn.Close(ctx)

	const query = `
SELECT ue.output_tokens, ue.input_tokens, ue.cost_usd
FROM usage_event ue
JOIN project p ON p.id = ue.project_id
WHERE p.name = $1 AND ue.tag = $2 AND ue.status = 'ok'
ORDER BY ue.ts DESC
LIMIT 1`

	var outputTokens, inputTokens int64
	var costUSD float64
	if err := conn.QueryRow(ctx, query, project, tag).Scan(&outputTokens, &inputTokens, &costUSD); err != nil {
		t.Fatalf("query usage_event for (%s, %s): %v", project, tag, err)
	}
	if outputTokens <= 0 {
		t.Errorf("usage_event output_tokens = %d, want > 0", outputTokens)
	}
	if costUSD <= 0 {
		t.Errorf("usage_event cost_usd = %v, want > 0 (claude-sonnet-4-6 is priced)", costUSD)
	}
	t.Logf("usage_event recorded for (%s, %s): input=%d output=%d cost_usd=%v", project, tag, inputTokens, outputTokens, costUSD)
}

// connectDB opens a single pgx connection to the ephemeral database for assertions.
func connectDB(t *testing.T, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect to db: %v", err)
	}
	return conn
}

// readBody reads and closes a response body, failing the test on error.
func readBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	return body
}

// errorType extracts the gateway's typed error classifier from a JSON error body, or "".
func errorType(body []byte) string {
	var parsed struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return ""
	}
	return parsed.Error.Type
}
