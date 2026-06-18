package e2e

// This suite drives the budget enforcement path through the FULL gateway against the REAL
// Anthropic API. Each test inserts budget_limit rows directly, then issues real /v1/messages
// calls and asserts the gateway admits or blocks them per the configured limits:
//
//   - calls limit (deterministic): a cap of N → the (N+1)th call is blocked with a typed 402.
//   - concurrency (the key test): a cap of N, fired by 2N concurrent calls → exactly N admitted,
//     N blocked, no overshoot (the atomic per-(project, tag) reservation guarantees this).
//   - cost crossing: a tiny cost_usd cap → the call after the recorded cost crosses it is blocked.
//   - unknown model: a cost_usd cap + an unpriced model → fail-closed 402 (no upstream call).
//
// Gated on LLMGW_TEST_REFRESH_TOKEN; TestMain shares one refreshed access token across the suite.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

// budgetRequestBody is the tiny non-streaming request the budget tests forward (priced model, so
// it accrues cost_usd > 0).
const budgetRequestBody = `{
	"model": "claude-sonnet-4-6",
	"max_tokens": 16,
	"messages": [{"role": "user", "content": "Reply with a single word."}]
}`

// TestBudgetCallsLimit proves a calls cap is enforced deterministically: with a cap of 3, the
// first three real calls succeed and the fourth is blocked with the typed 402 body.
func TestBudgetCallsLimit(t *testing.T) {
	token := requireSharedToken(t)
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	harness := startPassthroughHarness(t, ctx, token)

	const project, tag = "budget-calls", "news"
	insertBudgetLimit(t, ctx, harness.DSN, project, tag, "calls", "hour", 3, "block")

	for i := 1; i <= 3; i++ {
		body := successfulCall(t, ctx, harness, project, tag)
		assertPlausibleMessagesBody(t, body)
	}

	status, body := postMessage(t, ctx, harness, project, tag)
	if status != http.StatusPaymentRequired {
		t.Fatalf("4th call: status %d, want 402; body: %s", status, body)
	}
	assertBudgetExceeded(t, body, tag, "calls", "hour", 3)
}

// TestBudgetConcurrency is the critical concurrency test: with a calls cap of 5, ten concurrent
// real calls must yield exactly five 200s and five 402s — the atomic per-(project, tag) admission
// must let no extra request through (no overshoot).
func TestBudgetConcurrency(t *testing.T) {
	token := requireSharedToken(t)
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	harness := startPassthroughHarness(t, ctx, token)

	const project, tag = "budget-concurrency", "news"
	const cap, fired = 5, 10
	insertBudgetLimit(t, ctx, harness.DSN, project, tag, "calls", "hour", cap, "block")

	statuses, bodies := fireConcurrent(ctx, harness, project, tag, fired)
	ok, blocked, other := classifyWave(t, statuses, bodies)

	if other != 0 {
		t.Fatalf("concurrency wave had %d non-(200|402) responses (likely transient upstream); re-run. ok=%d blocked=%d", other, ok, blocked)
	}
	if blocked != fired-cap {
		t.Fatalf("blocked=%d, want %d (no overshoot allowed)", blocked, fired-cap)
	}
	if ok != cap {
		t.Fatalf("admitted(200)=%d, want exactly the cap %d", ok, cap)
	}
	t.Logf("concurrency: fired=%d cap=%d → admitted(200)=%d blocked(402)=%d", fired, cap, ok, blocked)
}

// TestBudgetCostCrossing proves a cost_usd cap blocks at crossing: a tiny hourly cap is crossed by
// one real call's recorded cost, so the next call is blocked.
func TestBudgetCostCrossing(t *testing.T) {
	token := requireSharedToken(t)
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	harness := startPassthroughHarness(t, ctx, token)

	const project, tag = "budget-cost", "news"
	const tinyCap = 1e-7 // far below one tiny call's notional cost, so it crosses after one call.
	insertBudgetLimit(t, ctx, harness.DSN, project, tag, "cost_usd", "hour", tinyCap, "block")

	body := successfulCall(t, ctx, harness, project, tag)
	assertPlausibleMessagesBody(t, body)
	assertUsageRecorded(t, ctx, harness.DSN, project, tag) // confirms a recorded cost_usd > 0 crossed the cap.

	status, blockedBody := postMessage(t, ctx, harness, project, tag)
	if status != http.StatusPaymentRequired {
		t.Fatalf("post-crossing call: status %d, want 402; body: %s", status, blockedBody)
	}
	assertBudgetExceeded(t, blockedBody, tag, "cost_usd", "hour", tinyCap)
}

// TestBudgetUnknownModel proves the unknown-model fail-closed rule: a cost_usd cap with an
// unpriced model blocks with a 402 before any upstream call, recording no usage.
func TestBudgetUnknownModel(t *testing.T) {
	token := requireSharedToken(t)
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	harness := startPassthroughHarness(t, ctx, token)

	const project, tag = "budget-unknown", "news"
	insertBudgetLimit(t, ctx, harness.DSN, project, tag, "cost_usd", "hour", 10, "block")

	status, body := postModel(t, ctx, harness, project, tag, "claude-nonexistent-model-9-9")
	if status != http.StatusPaymentRequired {
		t.Fatalf("unknown-model call: status %d, want 402 fail-closed; body: %s", status, body)
	}
	assertBudgetExceeded(t, body, tag, "cost_usd", "hour", 10)
	assertNoUsage(t, ctx, harness.DSN, project)
}

// insertBudgetLimit auto-creates the project (so the limit can reference it) and inserts one
// budget_limit row for the (project, tag).
func insertBudgetLimit(t *testing.T, ctx context.Context, dsn, project, tag, dimension, window string, maxValue float64, action string) {
	t.Helper()

	conn := connectDB(t, ctx, dsn)
	defer conn.Close(ctx)

	var projectID int64
	const ensure = `INSERT INTO project (name) VALUES ($1) ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name RETURNING id`
	if err := conn.QueryRow(ctx, ensure, project).Scan(&projectID); err != nil {
		t.Fatalf("ensure project %q: %v", project, err)
	}

	const insert = `INSERT INTO budget_limit (project_id, tag, dimension, "window", max_value, action) VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := conn.Exec(ctx, insert, projectID, tag, dimension, window, maxValue, action); err != nil {
		t.Fatalf("insert budget_limit (%s/%s): %v", dimension, window, err)
	}
}

// successfulCall issues calls for (project, tag), retrying transient upstream 5xx with bounded
// backoff, and returns the body of the first 200. It fails fast on a 402 (a call expected to
// succeed must not be blocked), a dead token, or any other non-retryable status.
func successfulCall(t *testing.T, ctx context.Context, h *Harness, project, tag string) []byte {
	t.Helper()

	backoff := time.Second
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		status, body := postMessage(t, ctx, h, project, tag)
		switch {
		case status == http.StatusOK:
			return body
		case status == http.StatusPaymentRequired:
			t.Fatalf("unexpected 402 on a call expected to succeed: %s", body)
		case status < 500:
			classifyFailure(t, status, body, attempt, &backoff) // fatals on dead token / other 4xx
		default:
			retryTransient(t, attempt, &backoff, "transient gateway status")
		}
	}
	t.Fatalf("exhausted %d attempts against the real API", maxAttempts)
	return nil
}

// postMessage issues one /v1/messages POST for (project, tag) and returns its status and body.
func postMessage(t *testing.T, ctx context.Context, h *Harness, project, tag string) (int, []byte) {
	t.Helper()
	return postBody(t, ctx, h, project, tag, budgetRequestBody)
}

// postModel issues one POST for (project, tag) with a custom model id, returning status and body.
func postModel(t *testing.T, ctx context.Context, h *Harness, project, tag, model string) (int, []byte) {
	t.Helper()

	body := `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"Reply with a single word."}]}`
	return postBody(t, ctx, h, project, tag, body)
}

// postBody POSTs a request body for (project, tag) under a bounded timeout, returning the response
// status and body.
func postBody(t *testing.T, ctx context.Context, h *Harness, project, tag, body string) (int, []byte) {
	t.Helper()

	attemptCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	resp, err := h.Post(attemptCtx, "/v1/messages", []byte(body), messagesHeaders(project, tag))
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	return resp.StatusCode, readBody(t, resp)
}

// fireConcurrent issues n concurrent /v1/messages calls for (project, tag) and returns each
// call's status and body, indexed alike. It uses no *testing.T inside goroutines (status 0 marks
// a transport error, surfaced by the caller).
func fireConcurrent(ctx context.Context, h *Harness, project, tag string, n int) ([]int, [][]byte) {
	statuses := make([]int, n)
	bodies := make([][]byte, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			statuses[i], bodies[i] = postConcurrent(ctx, h, project, tag)
		}(i)
	}
	wg.Wait()
	return statuses, bodies
}

// postConcurrent is the goroutine-safe single POST used by the concurrency wave: it returns the
// status and body, or status 0 on a transport error.
func postConcurrent(ctx context.Context, h *Harness, project, tag string) (int, []byte) {
	attemptCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	resp, err := h.Post(attemptCtx, "/v1/messages", []byte(budgetRequestBody), messagesHeaders(project, tag))
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, body
}

// classifyWave tallies a concurrency wave into admitted (200), blocked (402 budget_exceeded), and
// other (anything else, e.g. a transient upstream error that should prompt a re-run).
func classifyWave(t *testing.T, statuses []int, bodies [][]byte) (ok, blocked, other int) {
	t.Helper()

	for i, status := range statuses {
		switch {
		case status == http.StatusOK:
			ok++
		case status == http.StatusPaymentRequired && errorType(bodies[i]) == "budget_exceeded":
			blocked++
		default:
			other++
			t.Logf("unexpected wave response [%d]: status=%d body=%s", i, status, bodies[i])
		}
	}
	return ok, blocked, other
}

// messagesHeaders builds the headers for a /v1/messages request attributed to (project, tag).
func messagesHeaders(project, tag string) map[string]string {
	return map[string]string{
		"Content-Type": "application/json",
		"X-Project":    project,
		"X-Tags":       tag,
	}
}

// assertBudgetExceeded checks a 402 body is the typed budget_exceeded envelope naming the expected
// (tag, dimension, window, limit).
func assertBudgetExceeded(t *testing.T, body []byte, tag, dimension, window string, limit float64) {
	t.Helper()

	var parsed struct {
		Error struct {
			Type      string  `json:"type"`
			Project   string  `json:"project"`
			Tag       string  `json:"tag"`
			Dimension string  `json:"dimension"`
			Window    string  `json:"window"`
			Limit     float64 `json:"limit"`
			Current   float64 `json:"current"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("402 body is not valid JSON (%v): %s", err, body)
	}

	got := parsed.Error
	if got.Type != "budget_exceeded" {
		t.Errorf("error.type = %q, want budget_exceeded", got.Type)
	}
	if got.Tag != tag || got.Dimension != dimension || got.Window != window || got.Limit != limit {
		t.Errorf("402 body = {tag:%q dimension:%q window:%q limit:%v}, want {tag:%q dimension:%q window:%q limit:%v}",
			got.Tag, got.Dimension, got.Window, got.Limit, tag, dimension, window, limit)
	}
	t.Logf("budget 402: dimension=%s window=%s limit=%v current=%v", got.Dimension, got.Window, got.Limit, got.Current)
}

// assertNoUsage asserts no usage_event was recorded for the project (a blocked request never
// reaches the provider).
func assertNoUsage(t *testing.T, ctx context.Context, dsn, project string) {
	t.Helper()

	conn := connectDB(t, ctx, dsn)
	defer conn.Close(ctx)

	const query = `SELECT COUNT(*) FROM usage_event ue JOIN project p ON p.id = ue.project_id WHERE p.name = $1`
	var n int64
	if err := conn.QueryRow(ctx, query, project).Scan(&n); err != nil {
		t.Fatalf("count usage_event for %q: %v", project, err)
	}
	if n != 0 {
		t.Errorf("usage_event rows for %q = %d, want 0 (blocked pre-forward)", project, n)
	}
}
